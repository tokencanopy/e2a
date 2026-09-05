"""Unit tests for the typed error hierarchy (Slice 8c-1)."""

from e2a.v1.errors import (
    E2AAuthError,
    E2AConflictError,
    E2AConnectionError,
    E2AError,
    E2AIdempotencyError,
    E2ALimitExceededError,
    E2ANotFoundError,
    E2APermissionError,
    E2ARateLimitError,
    E2AServerError,
    E2AValidationError,
    connection_error,
    from_api_exception,
    is_retryable_status,
)
from e2a.v1.generated.exceptions import ApiException


def _exc(status, body=None, headers=None):
    e = ApiException(status=status, body=body)
    e.headers = headers
    return e


def test_status_to_class_mapping():
    cases = {
        401: E2AAuthError,
        402: E2ALimitExceededError,
        403: E2APermissionError,
        404: E2ANotFoundError,
        409: E2AConflictError,
        422: E2AValidationError,
        429: E2ARateLimitError,
        500: E2AServerError,
        503: E2AServerError,
    }
    for status, klass in cases.items():
        err = from_api_exception(_exc(status, body="{}"))
        assert isinstance(err, klass), f"{status} -> {type(err).__name__}"
        assert err.status == status


def test_retryable_flags():
    assert from_api_exception(_exc(429, body="{}")).retryable is True
    assert from_api_exception(_exc(500, body="{}")).retryable is True
    assert from_api_exception(_exc(404, body="{}")).retryable is False
    assert from_api_exception(_exc(422, body="{}")).retryable is False


def test_402_429_split():
    # The permanent GA split: 402 limit_exceeded is a QUOTA cap (NOT retryable);
    # 429 rate_limited is a request-RATE limit (retryable). Clients branch on the
    # exception type / HTTP status.
    quota = from_api_exception(
        _exc(402, body='{"error":{"code":"limit_exceeded","message":"monthly cap"}}')
    )
    assert isinstance(quota, E2ALimitExceededError)
    assert not isinstance(quota, E2ARateLimitError)
    assert quota.retryable is False

    rate = from_api_exception(
        _exc(429, body='{"error":{"code":"rate_limited","message":"slow"}}')
    )
    assert isinstance(rate, E2ARateLimitError)
    assert not isinstance(rate, E2ALimitExceededError)
    assert rate.retryable is True

    # Code-first: limit_exceeded on an unexpected status still routes to the
    # quota class (never the rate-limit class).
    odd = from_api_exception(
        _exc(400, body='{"error":{"code":"limit_exceeded","message":"cap"}}')
    )
    assert isinstance(odd, E2ALimitExceededError)
    assert odd.retryable is False


def test_idempotency_code_wins_over_status():
    in_flight = from_api_exception(
        _exc(409, body='{"error":{"code":"idempotency_in_flight","message":"wait"}}')
    )
    assert isinstance(in_flight, E2AIdempotencyError)
    assert in_flight.retryable is True

    reuse = from_api_exception(
        _exc(422, body='{"error":{"code":"idempotency_key_reuse","message":"bad"}}')
    )
    assert isinstance(reuse, E2AIdempotencyError)
    assert reuse.retryable is False


def test_envelope_fields_surfaced():
    err = from_api_exception(
        _exc(
            404,
            body='{"error":{"code":"agent_not_found","message":"no such agent","details":{"x":1}}}',
            headers={"X-Request-Id": "req_abc", "Content-Type": "application/json"},
        )
    )
    assert err.code == "agent_not_found"
    assert err.message == "no such agent"
    assert err.details == {"x": 1}
    assert err.request_id == "req_abc"


def test_request_id_falls_back_to_envelope_body():
    err = from_api_exception(
        _exc(
            404,
            body='{"error":{"code":"not_found","message":"missing","request_id":"req_body"}}',
        )
    )
    assert err.request_id == "req_body"


def test_non_envelope_message_does_not_expose_raw_response():
    secret = "private-upstream-body-" + ("x" * 1000)
    err = from_api_exception(_exc(503, body=secret))
    assert err.message == "e2a API error (503)"
    assert secret not in str(err)
    assert err.__cause__ is not None


def test_non_json_body_falls_back_to_status_bucket():
    err = from_api_exception(_exc(503, body="<html>502 bad gateway</html>"))
    assert isinstance(err, E2AServerError)
    assert err.code == "internal_error"


def test_retry_after_numeric_and_http_date():
    numeric = from_api_exception(_exc(429, body="{}", headers={"Retry-After": "12"}))
    assert numeric.retry_after_seconds == 12

    # An HTTP-date in the past clamps to >= 0.
    dated = from_api_exception(
        _exc(503, body="{}", headers={"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"})
    )
    assert dated.retry_after_seconds is not None
    assert dated.retry_after_seconds >= 0


def test_missing_envelope_does_not_crash():
    for body in (None, "", "null", "[]", '"a string"', "12"):
        err = from_api_exception(_exc(500, body=body))
        assert isinstance(err, E2AError)
        assert err.status == 500


def test_list_valued_retry_after_does_not_crash():
    # Regression: a non-string Retry-After must not raise TypeError out of the mapper.
    err = from_api_exception(_exc(503, body="{}", headers={"retry-after": ["12", "34"]}))
    assert isinstance(err, E2AServerError)
    assert err.retry_after_seconds is None


def test_known_code_maps_by_code_on_unexpected_status():
    # `forbidden` arriving on a 200 (an impossible-but-defensive case) must map
    # to E2APermissionError by code, NOT degrade to the base E2AError.
    err = from_api_exception(
        _exc(200, body='{"error":{"code":"forbidden","message":"nope"}}')
    )
    assert isinstance(err, E2APermissionError)

    # `not_found` on a 400 maps by code to E2ANotFoundError.
    nf = from_api_exception(
        _exc(400, body='{"error":{"code":"agent_not_found","message":"x"}}')
    )
    assert isinstance(nf, E2ANotFoundError)

    # `domain_not_verified` (server emits it on both 400 and 403) → validation.
    dnv = from_api_exception(
        _exc(403, body='{"error":{"code":"domain_not_verified","message":"x"}}')
    )
    assert isinstance(dnv, E2AValidationError)

    # `rate_limited` on a non-429 status stays a rate-limit error (retryable).
    rl = from_api_exception(
        _exc(400, body='{"error":{"code":"rate_limited","message":"slow"}}')
    )
    assert isinstance(rl, E2ARateLimitError)
    assert rl.retryable is True


def test_code_family_patterns():
    # The *_taken conflict family (agent_taken / domain_taken / alias_taken)
    # maps code-first regardless of status.
    for code in ("agent_taken", "domain_taken", "alias_taken"):
        err = from_api_exception(
            _exc(200, body='{"error":{"code":"%s","message":"x"}}' % code)
        )
        assert isinstance(err, E2AConflictError)
        assert err.retryable is False

    # invalid_* refinements of invalid_request are validation errors even when
    # the individual code is not in the table.
    for code in ("invalid_slug", "invalid_filter", "invalid_expires_at"):
        err = from_api_exception(
            _exc(200, body='{"error":{"code":"%s","message":"x"}}' % code)
        )
        assert isinstance(err, E2AValidationError)


def test_catalog_family_overrides():
    # 501 not_implemented / events_log_disabled: server family but NOT
    # retryable — while a plain 501 with an unknown code stays retryable.
    for code in ("not_implemented", "events_log_disabled"):
        err = from_api_exception(
            _exc(501, body='{"error":{"code":"%s","message":"x"}}' % code)
        )
        assert isinstance(err, E2AServerError)
        assert err.retryable is False
    unknown = from_api_exception(
        _exc(501, body='{"error":{"code":"totally_unknown_code","message":"x"}}')
    )
    assert unknown.retryable is True

    # 400 fixed per-account caps map to the quota family, not validation.
    for code in ("template_limit_reached", "webhook_limit_reached"):
        err = from_api_exception(
            _exc(400, body='{"error":{"code":"%s","message":"x"}}' % code)
        )
        assert isinstance(err, E2ALimitExceededError)
        assert err.retryable is False

    # Outbound policy + review-hold + suppression + retention codes.
    assert isinstance(
        from_api_exception(
            _exc(403, body='{"error":{"code":"blocked_by_policy","message":"x"}}')
        ),
        E2APermissionError,
    )
    paused = from_api_exception(
        _exc(403, body='{"error":{"code":"sending_paused","message":"x"}}')
    )
    assert isinstance(paused, E2APermissionError)
    assert paused.retryable is False
    assert isinstance(
        from_api_exception(
            _exc(409, body='{"error":{"code":"message_not_pending","message":"x"}}')
        ),
        E2AConflictError,
    )
    assert isinstance(
        from_api_exception(
            _exc(422, body='{"error":{"code":"recipient_suppressed","message":"x"}}')
        ),
        E2AValidationError,
    )
    assert isinstance(
        from_api_exception(_exc(410, body='{"error":{"code":"gone","message":"x"}}')),
        E2ANotFoundError,
    )


def test_unknown_code_falls_back_to_status_bucket():
    # An unrecognized code must not short-circuit the status bucket.
    err = from_api_exception(
        _exc(404, body='{"error":{"code":"totally_made_up","message":"x"}}')
    )
    assert isinstance(err, E2ANotFoundError)

    # Unknown code on a plain 422 → validation via status.
    val = from_api_exception(
        _exc(422, body='{"error":{"code":"some_new_thing","message":"x"}}')
    )
    assert isinstance(val, E2AValidationError)


def test_retry_after_from_details_body_when_header_absent():
    err = from_api_exception(
        _exc(
            429,
            body='{"error":{"code":"rate_limited","details":{"retry_after_seconds":30}}}',
        )
    )
    assert isinstance(err, E2ARateLimitError)
    assert err.retry_after_seconds == 30


def test_retry_after_header_wins_over_details():
    err = from_api_exception(
        _exc(
            429,
            body='{"error":{"code":"rate_limited","details":{"retry_after_seconds":30}}}',
            headers={"Retry-After": "5"},
        )
    )
    assert err.retry_after_seconds == 5


def test_connection_error_helper():
    err = connection_error("refused", cause=OSError("ECONNREFUSED"))
    assert isinstance(err, E2AConnectionError)
    assert err.status == 0
    assert err.retryable is True
    assert err.code == "connection_error"


def test_is_retryable_status():
    assert is_retryable_status(429) is True
    assert is_retryable_status(408) is True
    assert is_retryable_status(500) is True
    assert is_retryable_status(599) is True
    assert is_retryable_status(404) is False
    assert is_retryable_status(200) is False
