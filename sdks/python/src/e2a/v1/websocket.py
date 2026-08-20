"""Notification-only WebSocket stream for e2a /v1.

Connects to ``/v1/agents/{address}/ws`` and yields :class:`WSEvent` objects —
each frame is the SAME versioned event envelope a webhook delivery carries
(``{type, id, schema_version, created_at, data}``). Today the relay emits
``email.received`` events (``data`` is the :class:`~e2a.v1.webhook_signature.EmailReceivedData`
shape); tolerate unknown ``type`` values — future WS event kinds parse into
the same envelope. The protocol is server-to-client only; the client never
sends application frames.

The event carries metadata (message_id, sender, subject, …), never the body.
Fetch the full message via REST when you want it::

    async for event in client.listen("bot@agents.e2a.dev"):
        if event.type != "email.received":
            continue  # forward-compat: skip unknown event kinds
        data = event.data
        email = await client.messages.get(data["delivered_to"], data["message_id"])

Requires ``websockets`` (``pip install e2a[ws]``).
"""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass
from typing import Any, AsyncIterator, Generic, Optional, TypeVar
from urllib.parse import quote, urlparse, urlunparse

from .errors import (
    E2AAuthError,
    E2AConnectionReplacedError,
    E2AError,
    E2ANotFoundError,
    E2APermissionError,
)
from ._event_lifecycle import coerce_v1_stable_lifecycle
from .webhook_signature import WebhookEvent

__all__ = ["WSEvent", "WSStream", "WS_CLOSE_REPLACED"]

#: e2a application close code: a NEWER connection for this agent superseded
#: this one (the server holds one connection per agent). Terminal — do not
#: reconnect. See docs/api.md "Connection lifecycle & close codes".
WS_CLOSE_REPLACED = 4000

logger = logging.getLogger("e2a.v1.websocket")


def _handshake_status(exc: BaseException) -> Optional[int]:
    """Return the HTTP status of a rejected WebSocket handshake, if any.

    The ``websockets`` library rejects a bad handshake with ``InvalidStatus``
    (modern; status on ``exc.response.status_code``) or the deprecated
    ``InvalidStatusCode`` (status on ``exc.status_code``). We probe both so this
    works across library versions without importing version-specific symbols.
    """
    resp = getattr(exc, "response", None)
    status = getattr(resp, "status_code", None)
    if isinstance(status, int):
        return status
    status = getattr(exc, "status_code", None)  # deprecated InvalidStatusCode
    if isinstance(status, int):
        return status
    return None


def _fatal_error_for_status(status: int, exc: BaseException) -> Optional[E2AError]:
    """Map a fatal (non-retryable) handshake status to a typed E2AError.

    Auth/permission and other 4xx handshake rejections are caller/credential
    bugs that will never succeed on retry, so we surface them instead of
    looping. 5xx and everything else stays transient.
    """
    if status == 401:
        return E2AAuthError(
            code="unauthorized",
            message=f"WebSocket handshake rejected: HTTP {status}",
            status=status,
            retryable=False,
        )
    if status == 403:
        return E2APermissionError(
            code="forbidden",
            message=f"WebSocket handshake rejected: HTTP {status}",
            status=status,
            retryable=False,
        )
    if status == 404:
        # The agent doesn't exist OR isn't yours — the server collapses the
        # cross-tenant case into not_found so the handshake can't be used to
        # enumerate which agent addresses exist across accounts.
        return E2ANotFoundError(
            code="not_found",
            message=f"WebSocket handshake rejected: HTTP {status}",
            status=status,
            retryable=False,
        )
    if 400 <= status < 500:
        return E2AError(
            code="ws_handshake_rejected",
            message=f"WebSocket handshake rejected: HTTP {status}",
            status=status,
            retryable=False,
        )
    return None


def _close_code_and_reason(exc: BaseException) -> tuple[Optional[int], str]:
    """Return the (code, reason) of a server-sent WebSocket close, if any.

    The ``websockets`` library raises ``ConnectionClosed`` with the received
    close frame on ``exc.rcvd`` (modern; ``.rcvd.code`` / ``.rcvd.reason``) or
    the deprecated ``.code`` / ``.reason`` attributes. Probe both so this works
    across library versions without importing version-specific symbols.
    """
    rcvd = getattr(exc, "rcvd", None)
    code = getattr(rcvd, "code", None)
    if isinstance(code, int):
        reason = getattr(rcvd, "reason", "")
        return code, reason if isinstance(reason, str) else ""
    code = getattr(exc, "code", None)  # deprecated attribute shape
    if isinstance(code, int):
        reason = getattr(exc, "reason", "")
        return code, reason if isinstance(reason, str) else ""
    return None, ""


def _fatal_error_for_close(code: int, reason: str) -> Optional[E2AError]:
    """Map a terminal (no-reconnect) server close CODE to a typed E2AError.

    Implements the documented close-code contract (docs/api.md "Connection
    lifecycle & close codes"; mirrors the TS SDK's ``fatalErrorForClose``):

    - ``4000 "replaced"`` → :class:`E2AConnectionReplacedError` — a newer
      connection for this agent took over; reconnecting would steal the socket
      back and loop.
    - ``1008`` → :class:`E2APermissionError` — genuine policy rejection;
      retrying the same connection cannot succeed.
    - other ``4001–4999`` — reserved e2a application codes; unknown ones are
      fatal by contract (forward-compat).

    Everything else (1001 shutting_down / ping_timeout, 1006 abnormal, 1011
    internal error, …) is transient → ``None`` → backoff reconnect.
    """
    suffix = f'WebSocket closed by server: code={code} reason="{reason}"'
    if code == WS_CLOSE_REPLACED:
        return E2AConnectionReplacedError(
            code="ws_replaced",
            message=(
                "a newer connection for this agent superseded this one; not "
                f"reconnecting (one connection per agent) — {suffix}"
            ),
            status=0,
            retryable=False,
        )
    if code == 1008:
        return E2APermissionError(
            code="ws_policy_violation",
            message=f"connection rejected by server policy; not reconnecting — {suffix}",
            status=0,
            retryable=False,
        )
    if 4000 <= code <= 4999:
        return E2AError(
            code="ws_closed",
            message=f"terminal application close; not reconnecting — {suffix}",
            status=0,
            retryable=False,
        )
    return None


def _close_disposition(code: int, reason: str) -> str:
    """Classify the GA-frozen peer-close contract for reconnect decisions."""
    del reason  # Stable reason tokens are diagnostic; clients branch on code.
    if code == 1000:
        return "normal"
    if code == WS_CLOSE_REPLACED:
        return "replaced"
    if code == 1008 or 4000 <= code <= 4999:
        return "terminal"
    return "transient"


WSDataT = TypeVar("WSDataT")


@dataclass(frozen=True)
class WSEvent(WebhookEvent[WSDataT], Generic[WSDataT]):
    """One WebSocket frame: the versioned event envelope (same shape as a
    webhook delivery / ``GET /v1/events/{id}``).

    ``data`` is the per-event payload dict. For ``email.received`` it matches
    :class:`~e2a.v1.webhook_signature.EmailReceivedData`; unknown/beta types
    stay generic dicts (tolerate them — forward-compat). ``id`` is stable
    across channels for the same event, so a consumer receiving both the
    webhook and the WS frame can dedup on it.
    """

    @classmethod
    def from_payload(cls, payload: dict) -> "WSEvent[Any]":
        if not (
            isinstance(payload.get("type"), str)
            and isinstance(payload.get("id"), str)
            and isinstance(payload.get("schema_version"), str)
            and isinstance(payload.get("created_at"), str)
            and isinstance(payload.get("data"), dict)
        ):
            raise ValueError("WebSocket event is missing required envelope fields")
        return cls(
            type=payload["type"],
            id=payload["id"],
            schema_version=payload["schema_version"],
            created_at=payload["created_at"],
            data=coerce_v1_stable_lifecycle(
                payload["schema_version"], payload["type"], payload["data"]
            ),
            raw=payload,
        )


def _build_ws_url(base_url: str, agent_email: str) -> str:
    """Build the versioned WebSocket URL from the HTTP base URL.

    Auth is the ``Authorization: Bearer`` handshake header (see ``_ws_connect``),
    so the API key never appears in the URL — nothing for access logs / proxy
    traces to leak.
    """
    # No dot-segment guard here (unlike client.py's _assert_not_dot_segment):
    # this path has no sibling route for ".." or "." to collapse onto (there
    # is no "/v1/ws"), so a collapsed value fails closed with a 404/connection
    # error instead of retargeting a live resource. Flagged in review
    # (e2a#792 PR #909) and left as a one-line comment, not a fix, for that
    # reason -- noted out of scope here rather than left unexplained.
    parsed = urlparse(base_url)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    path = f"/v1/agents/{quote(agent_email, safe='')}/ws"
    return urlunparse((scheme, parsed.netloc, path, "", "", ""))


class WSStream:
    """Async-iterable event stream returned by ``client.listen(address)``.

    Iterate it directly — each item is a :class:`WSEvent` envelope::

        async for event in client.listen("bot@agents.e2a.dev"):
            ...

    Reconnects with exponential backoff (1s → ``max_backoff``) by default —
    but only on TRANSIENT closes (network drops, server restart/shutdown, ping
    timeout, internal error). Terminal close codes — 4000 ``"replaced"`` (a
    newer connection for this agent took over), 1008 (policy rejection), and
    other 4xxx application codes — never reconnect: iteration raises a typed
    error (:class:`~e2a.v1.errors.E2AConnectionReplacedError` for 4000).
    """

    def __init__(
        self,
        *,
        api_key: str,
        agent_email: str,
        base_url: str,
        reconnect: bool = True,
        max_backoff: float = 30.0,
    ) -> None:
        self._url = _build_ws_url(base_url, agent_email)
        self._api_key = api_key
        self._email = agent_email
        self._reconnect = reconnect
        self._max_backoff = max_backoff

    def __aiter__(self) -> AsyncIterator[WSEvent]:
        return self._stream()

    async def _stream(self) -> AsyncIterator[WSEvent]:
        try:
            import websockets  # noqa: F401
        except ImportError:  # pragma: no cover - optional dep
            raise ImportError(
                "The 'websockets' package is required for listen(). "
                "Install it with: pip install e2a[ws]"
            )

        backoff = 1.0
        while True:
            try:
                async for notif in _connect_and_stream(self._url, self._api_key, self._email):
                    yield notif
                    backoff = 1.0  # reset on a successful message
            except E2AError:
                # Already-typed fatal errors (e.g. raised below) propagate.
                raise
            except Exception as exc:  # noqa: BLE001
                # A terminal server close (4000 "replaced", 1008 policy, other
                # 4xxx) will never succeed on retry — and for "replaced",
                # retrying would steal the socket back from our own
                # replacement. Surface the typed error and stop.
                close_code, close_reason = _close_code_and_reason(exc)
                if close_code is not None:
                    if _close_disposition(close_code, close_reason) == "normal":
                        return
                    fatal = _fatal_error_for_close(close_code, close_reason)
                    if fatal is not None:
                        fatal.__cause__ = exc
                        raise fatal
                # A fatal handshake rejection (auth/permission/4xx) will never
                # succeed on retry — surface it and stop instead of looping.
                status = _handshake_status(exc)
                if status is not None:
                    fatal = _fatal_error_for_status(status, exc)
                    if fatal is not None:
                        fatal.__cause__ = exc
                        raise fatal
                # Transient/network failure — log and reconnect.
                logger.warning("WebSocket disconnected: %s", exc)

            if not self._reconnect:
                return
            logger.info("Reconnecting in %.1fs...", backoff)
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, self._max_backoff)


def _ws_connect(ws_url: str, api_key: str):
    """Open the WS handshake with the API key in the Authorization header.

    Uses ``additional_headers`` (websockets >= 14, the dependency floor). Note
    websockets validates connect kwargs lazily — at ``__aenter__``, not at this
    call — so version handling must be by capability, not a try/except here.
    """
    import websockets

    headers = {"Authorization": f"Bearer {api_key}"}
    return websockets.connect(ws_url, additional_headers=headers)


async def _connect_and_stream(
    ws_url: str, api_key: str, agent_email: str
) -> AsyncIterator[WSEvent]:
    """Connect once and yield event envelopes until disconnect. Sends no frames."""
    async with _ws_connect(ws_url, api_key) as ws:
        logger.info("Connected to WebSocket for %s", agent_email)
        async for raw in ws:
            try:
                payload = json.loads(raw)
                # Every frame is the versioned event envelope; a frame without
                # a string `type` is not one. Unknown type VALUES are yielded
                # (forward-compat) — consumers branch on event.type.
                if not isinstance(payload, dict) or not isinstance(payload.get("type"), str) or not payload["type"]:
                    logger.warning("WS frame is not an event envelope (missing `type`): %s", raw)
                    continue
                yield WSEvent.from_payload(payload)
            except Exception:  # noqa: BLE001
                logger.exception("Error processing WS event")
                continue
