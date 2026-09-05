"""Typed error hierarchy for the e2a Python SDK (Slice 8c).

Mirrors the TypeScript SDK's error model. The /v1 surface returns a uniform
envelope ``{"error": {"code", "message", "request_id", "details"}}``
(internal/httpapi/errors.go). We map it to typed exceptions so a caller can
branch with ``except E2ANotFoundError`` and read ``.code`` / ``.status`` /
``.request_id`` / ``.retryable`` without parsing bodies.

Class selection is genuinely code-first: a known, stable ``error.code`` (see
``_CODE_MAP`` and the ``*_not_found`` / ``*_taken`` / ``invalid_*`` naming
families from the published catalog in docs/api.md "Error codes") maps to
its typed class *regardless of the HTTP status it arrives on*, so a code carried
on an unexpected status no longer degrades to the bare base error. Unknown or
empty codes fall back to the HTTP status bucket, which preserves every
status→class outcome (401→auth, 403→permission, 404→not-found, 409→conflict,
422→validation, 429→rate-limit, 5xx/408→server). The two idempotency codes are
the most specific and win over both.
"""

from __future__ import annotations

import json
from email.utils import parsedate_to_datetime
from typing import Any, Mapping, Optional

from pydantic import ValidationError as _PydanticValidationError

from .generated.exceptions import ApiException

__all__ = [
    "E2AError",
    "E2AAuthError",
    "E2APermissionError",
    "E2ANotFoundError",
    "E2AConflictError",
    "E2AValidationError",
    "E2AIdempotencyError",
    "E2ALimitExceededError",
    "E2ARateLimitError",
    "E2AServerError",
    "E2AConnectionError",
    "E2AConnectionReplacedError",
    "E2AWebhookSignatureError",
    "is_retryable_status",
    "from_api_exception",
    "from_validation_error",
    "connection_error",
    "IDEMPOTENCY_IN_FLIGHT_CODE",
    "error_code_from_api_exception",
]


class E2AError(Exception):
    """Base class for every error the SDK raises."""

    def __init__(
        self,
        *,
        code: str,
        message: str,
        status: int,
        retryable: bool,
        request_id: Optional[str] = None,
        details: Any = None,
        retry_after_seconds: Optional[float] = None,
    ) -> None:
        super().__init__(message)
        #: Stable machine code from the envelope (e.g. ``"domain_not_verified"``).
        self.code = code
        self.message = message
        #: HTTP status; ``0`` for a connection-level failure with no response.
        self.status = status
        #: ``X-Request-Id`` echoed by the server — quote it in support requests.
        self.request_id = request_id
        #: Structured field-level detail (envelope ``error.details``).
        self.details = details
        #: True when retrying could plausibly succeed (429 / 5xx / connection).
        self.retryable = retryable
        #: Seconds from a ``Retry-After`` header, when present.
        self.retry_after_seconds = retry_after_seconds


class E2AAuthError(E2AError):
    """401 — missing/invalid credentials."""


class E2APermissionError(E2AError):
    """403 — authenticated but not allowed (also: existence-hiding on get)."""


class E2ANotFoundError(E2AError):
    """404 — no such resource."""


class E2AConflictError(E2AError):
    """409 — state conflict (already exists, etc.)."""


class E2AValidationError(E2AError):
    """422 — input validation failure.

    Also raised for CLIENT-SIDE (pre-flight) validation failures — e.g. an
    argument violating a generated ``@validate_call`` constraint like
    ``limit<=100`` — with ``status=0`` and ``request_id=None`` because no HTTP
    request was sent (see :func:`from_validation_error`)."""


class E2AIdempotencyError(E2AError):
    """idempotency_in_flight / idempotency_key_reuse."""


class E2ALimitExceededError(E2AError):
    """402 — a per-account resource QUOTA (stock/flow cap) was hit
    (``code == "limit_exceeded"``). Distinct from :class:`E2ARateLimitError`
    (429, a request-RATE/throughput limit): a 402 is NOT retryable — a retry
    alone will not clear it; surface a quota/upgrade path. ``details.resource``
    keys the cap to ``usage.<resource>`` / ``limits.max_<resource>``. This is the
    permanent GA split — branch on the exception type (equivalently, the HTTP
    status): 402 → quota, 429 → back off and retry."""


class E2ARateLimitError(E2AError):
    """429 — a request-RATE / throughput limit was hit
    (``code == "rate_limited"``). Transient and retryable — wait
    ``retry_after_seconds`` and retry. Distinct from
    :class:`E2ALimitExceededError` (402, a QUOTA cap)."""


class E2AServerError(E2AError):
    """5xx / 408, or a successful response that violates the API schema."""


class E2AConnectionError(E2AError):
    """No HTTP response (DNS, refused, reset, timeout)."""


class E2AConnectionReplacedError(E2AError):
    """WebSocket close code 4000 (``"replaced"``): a NEWER connection for the
    same agent superseded this one — the server holds one connection per agent.

    Terminal by contract: the SDK does NOT auto-reconnect, because reconnecting
    would steal the socket back from the replacement and loop. If the takeover
    was yours, it already succeeded on the other side; run one listener per
    agent."""


class E2AWebhookSignatureError(E2AError):
    """Local webhook signature verification failure."""


def is_retryable_status(status: int) -> bool:
    """Status codes where a retry could plausibly help (excludes connection —
    that path is handled separately, since there's no status to inspect)."""
    return status == 408 or status == 429 or (500 <= status <= 599)


# The two idempotency-conflict codes from internal/httpapi/idempotency.go.
# in_flight is safe to retry (same key, server dedupes); key_reuse is a caller
# bug (same key, different body) and must not be retried.
#
# Exposed (not underscore-prefixed) because the retry layer (_retry.py) needs
# to recognize this exact code on a bare 409 *before* status-based retry
# resolution even applies — 409 alone is never retryable (is_retryable_status
# excludes it, precisely so idempotency_key_reuse is never retried), so
# idempotency_in_flight is the one carve-out that must be matched by code.
IDEMPOTENCY_IN_FLIGHT_CODE = "idempotency_in_flight"
_IDEMPOTENCY_RETRYABLE = IDEMPOTENCY_IN_FLIGHT_CODE
_IDEMPOTENCY_CODES = {_IDEMPOTENCY_RETRYABLE, "idempotency_key_reuse"}

# Fallback code synthesized when the envelope omits one. invalid_request is the
# single canonical validation code the server emits for BOTH 400 (malformed) and
# 422 (semantically invalid), so both statuses map to it here.
_DEFAULT_CODE = {
    400: "invalid_request",
    401: "unauthorized",
    402: "limit_exceeded",
    403: "forbidden",
    404: "not_found",
    409: "conflict",
    422: "invalid_request",
    429: "rate_limited",
}

# Code-first map: a known, stable server `code` selects the typed class
# regardless of the HTTP status it arrives on, so a code carried on an
# unexpected status no longer degrades to the bare E2AError. Seeded from the
# codes the /v1 server emits (internal/httpapi/errors.go defaultCodeForStatus
# + the NewError(...) call sites in internal/httpapi/*.go). Unknown codes fall
# through to the status bucket below, preserving every status→class outcome.
_CODE_MAP: "dict[str, tuple[type[E2AError], bool]]" = {
    # 401 family
    "unauthorized": (E2AAuthError, False),
    # 403 family
    "forbidden": (E2APermissionError, False),
    "blocked_by_policy": (E2APermissionError, False),
    "sending_paused": (E2APermissionError, False),
    # 404/410 family — also covers *_not_found via the suffix check in _resolve.
    "not_found": (E2ANotFoundError, False),
    "gone": (E2ANotFoundError, False),
    # 409 family — also covers the *_taken family (agent_taken / domain_taken /
    # alias_taken) and *_exists via the suffix checks in _resolve.
    "conflict": (E2AConflictError, False),
    "message_not_pending": (E2AConflictError, False),
    "webhook_cooldown": (E2AConflictError, False),
    "webhook_disabled": (E2AConflictError, False),
    # 4xx validation / bad-request family. invalid_request is the single
    # canonical code the server emits for both 400 and 422 (its invalid_*
    # prefix refinements resolve via the prefix check in _resolve); bad_request
    # / unprocessable_entity are retained only to tolerate legacy/mixed
    # responses.
    "domain_not_verified": (E2AValidationError, False),
    "recipient_suppressed": (E2AValidationError, False),
    "invalid_request": (E2AValidationError, False),
    "bad_request": (E2AValidationError, False),
    "unprocessable_entity": (E2AValidationError, False),
    # 402 — QUOTA cap (stock/flow). NOT retryable: distinct from the 429
    # request-RATE limit below. This is the permanent GA 402/429 split.
    # The 400 fixed per-account count caps (template_limit_reached /
    # webhook_limit_reached) join the same family: a retry never clears them.
    "limit_exceeded": (E2ALimitExceededError, False),
    "template_limit_reached": (E2ALimitExceededError, False),
    "webhook_limit_reached": (E2ALimitExceededError, False),
    # 429 — request-RATE / throughput limit. Retryable (back off Retry-After).
    "rate_limited": (E2ARateLimitError, True),
    # 501 — feature not available on this deployment. Overrides the 5xx status
    # bucket's retryable=True: retrying a not-implemented feature never helps.
    "not_implemented": (E2AServerError, False),
    "events_log_disabled": (E2AServerError, False),
}


def _resolve(status: int, code: str) -> "tuple[type[E2AError], bool]":
    # 1. Idempotency codes are the most specific — they win over everything.
    if code in _IDEMPOTENCY_CODES:
        return E2AIdempotencyError, (code == _IDEMPOTENCY_RETRYABLE)
    # 2. Code-first: a known stable code selects the class regardless of status.
    if code:
        if code in _CODE_MAP:
            return _CODE_MAP[code]
        # Naming families from the published error.code catalog (docs/api.md
        # "Error codes"): *_not_found = a missing (sub)resource, *_taken = the
        # identifier is already claimed, invalid_* = a validation refinement of
        # invalid_request. (*_exists is tolerated for forward compatibility.)
        if code.endswith("_not_found"):
            return E2ANotFoundError, False
        if code.endswith("_taken") or code.endswith("_exists"):
            return E2AConflictError, False
        if code.startswith("invalid_"):
            return E2AValidationError, False
    # 3. Fall back to the HTTP status bucket for unknown/empty codes.
    by_status: "dict[int, tuple[type[E2AError], bool]]" = {
        # Every 400 is a client/validation error — maps the remaining 400 codes
        # (too_many_recipients, reserved_domain, domain_has_agents, …) to the
        # validation family instead of degrading to the bare base error.
        400: (E2AValidationError, False),
        401: (E2AAuthError, False),
        402: (E2ALimitExceededError, False),
        403: (E2APermissionError, False),
        404: (E2ANotFoundError, False),
        409: (E2AConflictError, False),
        422: (E2AValidationError, False),
        429: (E2ARateLimitError, True),
    }
    if status in by_status:
        return by_status[status]
    if is_retryable_status(status):
        return E2AServerError, True
    return E2AError, False


def _header_get(headers: Optional[Mapping[str, str]], name: str) -> Optional[str]:
    if not headers:
        return None
    lower = name.lower()
    for k, v in headers.items():
        if k.lower() == lower:
            return v
    return None


def _parse_retry_after(headers: Optional[Mapping[str, str]]) -> Optional[float]:
    v = _header_get(headers, "retry-after")
    if not v or not isinstance(v, str):
        return None
    try:
        secs = float(v)
        if secs >= 0:
            return secs
    except (TypeError, ValueError):
        pass
    # RFC 9110 §10.2.3 also allows an HTTP-date (common behind CDNs).
    try:
        dt = parsedate_to_datetime(v)
    except (TypeError, ValueError):
        return None
    if dt is None:
        return None
    import time

    return max(0.0, dt.timestamp() - time.time())


def _retry_after_from_details(details: Any) -> Optional[float]:
    """Read ``details.retry_after_seconds`` (a number) when present.

    Used as a fallback when the ``Retry-After`` header is absent — the send-path
    429 carries its retry hint in the body, not the header.
    """
    if not isinstance(details, Mapping):
        return None
    v = details.get("retry_after_seconds")
    if isinstance(v, bool):  # bool is a subclass of int — reject it.
        return None
    if isinstance(v, (int, float)):
        return max(0.0, float(v))
    return None


def to_e2a_error(
    *,
    status: int,
    code: str = "",
    message: Optional[str] = None,
    request_id: Optional[str] = None,
    details: Any = None,
    headers: Optional[Mapping[str, str]] = None,
    cause: Optional[BaseException] = None,
) -> E2AError:
    """Build a typed error from status + the parsed envelope fields."""
    resolved_code = code or _DEFAULT_CODE.get(status) or (
        "internal_error" if status >= 500 else "error"
    )
    klass, retryable = _resolve(status, code)
    # Prefer the Retry-After header; else fall back to the error body's
    # details.retry_after_seconds. The send-path 429 carries its hint in the
    # body (not the header) because of a Huma constraint, so we honor both.
    retry_after = _parse_retry_after(headers)
    if retry_after is None:
        retry_after = _retry_after_from_details(details)
    err = klass(
        code=resolved_code,
        message=message or f"e2a API error ({status})",
        status=status,
        request_id=request_id,
        details=details,
        retryable=retryable,
        retry_after_seconds=retry_after,
    )
    if cause is not None:
        err.__cause__ = cause
    return err


def from_api_exception(exc: ApiException) -> E2AError:
    """Map a generated ``ApiException`` (raised by the generated ``*Api`` classes on a
    non-2xx response) to a typed :class:`E2AError`."""
    headers = _normalize_headers(getattr(exc, "headers", None))
    request_id = _header_get(headers, "x-request-id")
    status = int(getattr(exc, "status", 0) or 0)

    code = ""
    # Generated ApiException.__str__ includes raw headers and body. Do not leak
    # an HTML proxy page or upstream response through the ergonomic public
    # message when the canonical envelope is absent; retain exc as __cause__.
    message = f"e2a API error ({status})"
    details: Any = None
    env = _parse_envelope(getattr(exc, "data", None), getattr(exc, "body", None))
    if isinstance(env, dict):
        error = env.get("error")
        if isinstance(error, dict):
            code = error.get("code") or ""
            message = error.get("message") or message
            details = error.get("details")
            if request_id is None:
                body_request_id = error.get("request_id")
                if isinstance(body_request_id, str) and body_request_id:
                    request_id = body_request_id

    return to_e2a_error(
        status=status,
        code=code,
        message=message,
        request_id=request_id,
        details=details,
        headers=headers,
        cause=exc,
    )


def from_validation_error(exc: _PydanticValidationError) -> E2AValidationError:
    """Map a CLIENT-SIDE pydantic ``ValidationError`` to a typed
    :class:`E2AValidationError`.

    The generated ``*Api`` methods are ``@validate_call``-decorated with the
    OpenAPI parameter constraints (e.g. ``limit`` ``Field(le=100, ge=1)``), so
    an out-of-range argument raises *before any HTTP request is sent*. Without
    this mapping the raw ``pydantic_core.ValidationError`` (a ``ValueError``,
    not an :class:`E2AError`) would leak to callers and break the documented
    "catch ``E2AError``" contract. ``status=0`` / ``request_id=None`` signal
    the pre-flight nature (no round-trip — same convention as
    :func:`connection_error`); ``details`` preserves pydantic's structured
    per-field errors.
    """
    try:
        details: Any = exc.errors(include_url=False)
    except Exception:  # pragma: no cover - defensive
        details = None
    err = E2AValidationError(
        code="invalid_request",
        message=f"client-side validation failed: {exc}",
        status=0,
        retryable=False,
        details=details,
    )
    err.__cause__ = exc
    return err


def error_code_from_api_exception(exc: ApiException) -> str:
    """Extract the envelope's ``error.code`` from a generated ``ApiException``
    without building a full typed error.

    Shared by :func:`from_api_exception` and the retry layer's 409
    ``idempotency_in_flight`` gate (see ``_retry.py``), so both read the
    ``{"error": {"code": ...}}`` shape the same way instead of hand-rolling it
    twice.
    """
    env = _parse_envelope(getattr(exc, "data", None), getattr(exc, "body", None))
    if isinstance(env, dict):
        error = env.get("error")
        if isinstance(error, dict):
            return error.get("code") or ""
    return ""


def _normalize_headers(headers: Any) -> Optional[Mapping[str, str]]:
    if headers is None:
        return None
    if isinstance(headers, Mapping):
        return headers
    # httpx.Headers / list of pairs both iterate as items().
    try:
        return dict(headers.items())
    except AttributeError:
        try:
            return dict(headers)
        except (TypeError, ValueError):
            return None


def _parse_envelope(data: Any, body: Any) -> Any:
    # The generated layer may hand us a deserialized model, a dict, or a raw
    # string body. Prefer a dict-shaped value; fall back to JSON-parsing body.
    if isinstance(data, dict):
        return data
    if hasattr(data, "to_dict"):
        try:
            return data.to_dict()
        except Exception:  # pragma: no cover - defensive
            pass
    if isinstance(body, (str, bytes)):
        try:
            return json.loads(body)
        except (ValueError, TypeError):
            return None
    return None


def connection_error(message: str, cause: Optional[BaseException] = None) -> E2AConnectionError:
    """A connection-level failure with no HTTP response."""
    err = E2AConnectionError(
        code="connection_error",
        message=message,
        status=0,
        retryable=True,
    )
    if cause is not None:
        err.__cause__ = cause
    return err
