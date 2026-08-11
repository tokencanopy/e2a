"""Unit tests for the namespaced AsyncE2AClient (Slice 8c-2).

Mocks httpx (the layer the generated base uses) so these exercise the full
stack: namespaced resource -> generated *Api -> bearer auth -> retry layer ->
httpx -> envelope unwrap -> typed-error mapping.
"""

import json

import httpx
import pytest
from pydantic import ValidationError
import e2a.v1 as v1

from e2a.v1._retry import RetryConfig
from e2a.v1.client import AsyncE2AClient
from e2a.v1.errors import (
    E2AConflictError,
    E2AConnectionError,
    E2AError,
    E2ANotFoundError,
    E2APermissionError,
    E2AServerError,
    E2AValidationError,
)
from e2a.v1.webhook_signature import WebhookEvent
from e2a.v1.generated.models import (
    AgentView,
    AgentSuppressionView,
    APIKeyView,
    AttachmentView,
    ConversationSummaryView,
    CreateAPIKeyResponse,
    CreateWebhookResponse,
    DomainView,
    EventView,
    ErrorBody,
    FieldError,
    MessageSummaryView,
    MessageLifecycleTransition,
    PageMessageLifecycleTransition,
    MessageView,
    ReviewView,
    StarterTemplateView,
    SuppressionView,
    TemplateSummaryView,
    TemplateView,
    WebhookDeliveryView,
    WebhookView,
    ReplyRequest,
    SendEmailRequest,
    UnsubscribeOptions,
)

BASE = "http://test.local"


def test_validation_field_location_is_required():
    with pytest.raises(ValidationError):
        FieldError(message="invalid")

    field = FieldError(location="", message="invalid")
    assert field.location == ""


def test_managed_unsubscribe_model_construction():
    request = SendEmailRequest(
        to=["recipient@example.net"],
        subject="Update",
        text="Hello",
        unsubscribe=UnsubscribeOptions(mode="managed"),
    )
    assert request.to_dict()["unsubscribe"] == {"mode": "managed"}


def test_error_body_requires_request_id_and_accepts_future_details():
    with pytest.raises(ValidationError):
        ErrorBody(code="invalid_request", message="invalid")

    body = ErrorBody(
        code="future_error_code",
        message="future failure",
        request_id="req_future",
        details={"future_field": {"nested": True}},
    )
    assert body.details == {"future_field": {"nested": True}}


def test_lifecycle_models_are_exported_from_v1():
    assert v1.MessageLifecycleTransition is MessageLifecycleTransition
    assert v1.PageMessageLifecycleTransition is PageMessageLifecycleTransition
    assert "MessageLifecycleTransition" in v1.__all__
    assert "PageMessageLifecycleTransition" in v1.__all__


from datetime import datetime, timezone  # noqa: E402

_DT = datetime(2026, 6, 18, tzinfo=timezone.utc)
# Valid values for required fields (incl. enum-constrained ones) so a fixture is
# a real, deserializable server object rather than a stub.
_REQUIRED_DEFAULTS = {
    "created_at": _DT,
    "first_message_at": _DT,
    "last_message_at": _DT,
    "next_retry_at": _DT,
    "updated_at": _DT,
    "domain": "test.dev",
    "domain_verified": True,
    "email": "x@test.dev",
    "hitl_expiration_action": "reject",
    "hitl_ttl_seconds": 0,
    "id": "id_1",
    "inbound_policy": "open",
    "name": "n",
    "direction": "inbound",
    "header_from": "a@x.com",
    "authentication": None,
    "labels": [],
    "message_id": "msg_1",
    "delivered_to": "bot@test.dev",
    "read_status": "unread",
    "subject": "s",
    "to": [],
}


def _dummy(ann):
    s = str(ann)
    if "bool" in s:
        return False
    if "int" in s:
        return 0
    if "float" in s:
        return 0.0
    if "List" in s or "list" in s:
        return []
    if "Dict" in s or "dict" in s or "Mapping" in s:
        return {}
    return ""


def _valid(model_cls, **overrides):
    """Construct a REAL model (valid by construction) and dump it to wire JSON —
    the Go server always returns full objects, so fixtures should too. Recurses
    into nested model fields (e.g. DomainView.dns_records) so deep views work."""
    from pydantic import BaseModel as _BaseModel

    kwargs = {}
    for name, field in model_cls.model_fields.items():
        if name in overrides:
            kwargs[name] = overrides[name]
        elif field.is_required():
            ann = field.annotation
            if isinstance(ann, type) and issubclass(ann, _BaseModel):
                kwargs[name] = _valid(ann)
            else:
                kwargs[name] = _REQUIRED_DEFAULTS.get(name, _dummy(field.annotation))
    inst = model_cls(**kwargs)
    return inst.model_dump(by_alias=True, mode="json")


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for v in ("E2A_API_KEY", "E2A_API_URL", "E2A_BASE_URL", "E2A_AGENT_EMAIL"):
        monkeypatch.delenv(v, raising=False)


def _client():
    # No-sleep retry config so any retry path is instant.
    async def no_sleep(_s):
        return None

    return AsyncE2AClient(
        api_key="e2a_test", base_url=BASE, _retry_config=RetryConfig(sleep=no_sleep)
    )


# ── construction ────────────────────────────────────────────────────

def test_requires_api_key():
    with pytest.raises(E2AError, match="api_key is required"):
        AsyncE2AClient(base_url=BASE)


# Base-URL resolution: base_url= > E2A_API_URL > E2A_BASE_URL (deprecated) >
# the api.e2a.dev default. Mirrors the TypeScript SDK's suite.

def test_base_url_defaults_to_api_host():
    assert AsyncE2AClient(api_key="e2a_test")._base_url == "https://api.e2a.dev"


def test_base_url_reads_e2a_api_url(monkeypatch):
    monkeypatch.setenv("E2A_API_URL", "https://api.self-host.example")
    assert AsyncE2AClient(api_key="e2a_test")._base_url == "https://api.self-host.example"


def test_base_url_honours_deprecated_e2a_base_url(monkeypatch):
    monkeypatch.setenv("E2A_BASE_URL", "https://legacy.example")
    with pytest.warns(DeprecationWarning, match="E2A_BASE_URL is deprecated"):
        client = AsyncE2AClient(api_key="e2a_test")
    assert client._base_url == "https://legacy.example"


def test_base_url_prefers_canonical_over_deprecated(monkeypatch):
    monkeypatch.setenv("E2A_API_URL", "https://canonical.example")
    monkeypatch.setenv("E2A_BASE_URL", "https://legacy.example")
    assert AsyncE2AClient(api_key="e2a_test")._base_url == "https://canonical.example"


def test_explicit_base_url_beats_env(monkeypatch):
    monkeypatch.setenv("E2A_API_URL", "https://api.self-host.example")
    assert AsyncE2AClient(api_key="e2a_test", base_url=BASE)._base_url == BASE


def test_resources_exposed():
    c = _client()
    for name in ("agents", "messages", "conversations", "domains", "events", "webhooks", "account", "reviews", "templates"):
        assert getattr(c, name) is not None
    assert c.account.suppressions is not None


@pytest.mark.parametrize(
    "timeout_ms, expected_s",
    [
        (None, 30.0),        # default
        (30_000.0, 30.0),    # explicit default
        (5_000.0, 5.0),      # custom
        (0, None),           # disabled → transport default applies
    ],
)
def test_timeout_ms_to_seconds(timeout_ms, expected_s):
    kwargs = {} if timeout_ms is None else {"timeout_ms": timeout_ms}
    c = AsyncE2AClient(api_key="e2a_test", base_url=BASE, **kwargs)
    assert c._timeout_s == expected_s


@pytest.mark.anyio
async def test_timeout_injected_into_transport(monkeypatch):
    # The real client wraps rest_client.request to inject _request_timeout when the
    # caller passes none. Patch the generated transport BEFORE construction so the
    # wrapper closes over our spy, then assert the default seconds value lands.
    from e2a.v1.generated import rest as _rest_mod

    seen = {}

    async def spy(self, method, url, headers=None, body=None, post_params=None, _request_timeout=None):
        seen["timeout"] = _request_timeout
        raise httpx.ConnectError("boom")  # short-circuit; we only inspect the arg

    monkeypatch.setattr(_rest_mod.RESTClientObject, "request", spy)
    c = AsyncE2AClient(api_key="e2a_test", base_url=BASE, timeout_ms=5_000.0)
    with pytest.raises(httpx.ConnectError):
        await c._api_client.rest_client.request("GET", "http://test.local/x")
    assert seen["timeout"] == 5.0


@pytest.mark.anyio
async def test_request_timeout_surfaces_as_connection_error(httpx_mock):
    # A transport timeout (httpx.TimeoutException family) must surface as a typed,
    # retryable connection error — same path as any connection-level failure.
    httpx_mock.add_exception(httpx.ReadTimeout("read timed out"))

    async def no_sleep(_s):
        return None

    c = AsyncE2AClient(
        api_key="e2a_test",
        base_url=BASE,
        timeout_ms=50.0,
        _retry_config=RetryConfig(max_retries=0, sleep=no_sleep),
    )
    async with c:
        with pytest.raises(E2AConnectionError):
            await c.agents.get("bot@test.dev")


# ── auth + agents ───────────────────────────────────────────────────

@pytest.mark.anyio
async def test_get_agent_sends_bearer_and_encodes_address(httpx_mock):
    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    async with _client() as c:
        agent = await c.agents.get("bot@test.dev")
    assert agent.email == "bot@test.dev"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/v1/agents/" in str(req.url)
    assert "bot%40test.dev" in str(req.url)
    assert req.headers["authorization"] == "Bearer e2a_test"


@pytest.mark.anyio
async def test_get_attachment_hits_endpoint_and_maps_view(httpx_mock):
    httpx_mock.add_response(
        json=_valid(
            AttachmentView,
            index=0,
            filename="report.pdf",
            content_type="application/pdf",
            size_bytes=14,
            download_url="https://api.test/d?token=tok",
            expires_at="2026-06-21T10:15:00Z",
        )
    )
    async with _client() as c:
        att = await c.messages.get_attachment("bot@test.dev", "msg_1", 0, inline=True)
    assert att.download_url == "https://api.test/d?token=tok"
    assert att.size_bytes == 14
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/messages/msg_1/attachments/0" in str(req.url)
    assert "inline=true" in str(req.url)


@pytest.mark.anyio
async def test_webhooks_fetch_message_resolves_keys(httpx_mock):
    # email.received is metadata-only; fetch_message resolves (delivered_to,
    # message_id) into the full-message GET. A held outbound draft has no
    # canonical MIME until approval, so the generated model must accept the
    # required raw_message field with a null value.
    httpx_mock.add_response(json=_valid(MessageView, id="msg_9", subject="Hi", raw_message=None))
    event = WebhookEvent(
        type="email.received",
        data={"message_id": "msg_9", "delivered_to": "bot@test.dev"},
        id="evt_1",
        schema_version="1",
        created_at="2026-06-21T10:15:00Z",
    )
    async with _client() as c:
        msg = await c.webhooks.fetch_message(event)
    assert msg.id == "msg_9"
    assert msg.raw_message is None
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/messages/msg_9" in str(req.url)
    assert "bot%40test.dev" in str(req.url)


@pytest.mark.anyio
async def test_webhooks_fetch_message_rejects_bad_event():
    async with _client() as c:
        with pytest.raises(ValueError, match="email.received"):
            await c.webhooks.fetch_message(
                WebhookEvent(
                    type="email.bounced", id="evt_1", schema_version="1",
                    created_at="2026-06-21T10:15:00Z",
                    data={"message_id": "m", "delivered_to": "r"},
                )
            )
        with pytest.raises(ValueError, match="delivered_to"):
            await c.webhooks.fetch_message(
                WebhookEvent(
                    type="email.received", id="evt_1", schema_version="1",
                    created_at="2026-06-21T10:15:00Z", data={"message_id": "m"},
                )
            )


@pytest.mark.anyio
async def test_send_error_surfaces_machine_code(httpx_mock):
    # send/reply/forward now declare default: ErrorEnvelope (the custom Responses
    # map had suppressed Huma's auto default). The SDK must surface the machine
    # `code` for the send-family, not a generic/raw error. Guards §6a #4.
    httpx_mock.add_response(
        status_code=403,
        json={"error": {"code": "sending_not_verified", "message": "domain not verified", "request_id": "req_1"}},
    )
    async with _client() as c:
        with pytest.raises(E2APermissionError) as ei:
            await c.messages.send("bot@test.dev", {"to": ["a@x.com"], "subject": "Hi", "text": "Hello"})
    assert ei.value.code == "sending_not_verified"
    assert ei.value.status == 403


@pytest.mark.anyio
async def test_create_agent_posts_body(httpx_mock):
    httpx_mock.add_response(status_code=201, json=_valid(AgentView, email="new@test.dev"))
    async with _client() as c:
        res = await c.agents.create({"email": "new@test.dev"})
    assert res.email == "new@test.dev"
    assert not hasattr(res, "id")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert str(req.url).endswith("/v1/agents")


@pytest.mark.anyio
async def test_agent_suppression_management_methods(httpx_mock):
    row = _valid(
        AgentSuppressionView,
        agent_email="sender@example.com",
        address="recipient@example.net",
        source="manual",
    )
    httpx_mock.add_response(json={"items": [row], "next_cursor": None})
    httpx_mock.add_response(json=row)
    httpx_mock.add_response(json={"deleted": True, "address": "recipient@example.net"})

    async with _client() as c:
        listed = await c.agents.list_suppressions("sender@example.com").to_list(limit=10)
        created = await c.agents.create_suppression(
            "sender@example.com", {"address": "recipient@example.net"}
        )
        deleted = await c.agents.delete_suppression(
            "sender@example.com", "recipient@example.net"
        )

    assert listed[0].address == "recipient@example.net"
    assert created.agent_email == "sender@example.com"
    assert deleted.deleted is True
    requests = httpx_mock.get_requests()
    assert requests[0].method == "GET"
    assert requests[1].method == "POST"
    assert requests[2].method == "DELETE"
    assert "confirm=DELETE" in str(requests[2].url)


@pytest.mark.anyio
async def test_agents_delete_sends_confirm_and_returns_receipt(httpx_mock):
    httpx_mock.add_response(
        json={"deleted": True, "email": "bot@test.dev", "messages_deleted": 12}
    )
    async with _client() as c:
        res = await c.agents.delete("bot@test.dev")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "/v1/agents/bot%40test.dev" in str(req.url)
    assert "confirm=DELETE" in str(req.url)
    # 200 + typed deletion receipt (uniform delete contract).
    assert res.deleted is True
    assert res.email == "bot@test.dev"
    assert res.messages_deleted == 12


@pytest.mark.anyio
async def test_agents_list_autopager(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(AgentView, id="ag_1", email="bot@test.dev")], "next_cursor": None}
    )
    async with _client() as c:
        items = await c.agents.list().to_list(limit=10)
    assert [a.email for a in items] == ["bot@test.dev"]


@pytest.mark.anyio
async def test_agents_list_deleted_lists_trash(httpx_mock):
    httpx_mock.add_response(json={"items": [], "next_cursor": None})
    async with _client() as c:
        await c.agents.list(deleted=True).to_list(limit=10)
    assert httpx_mock.get_requests()[-1].url.params["deleted"] == "true"


@pytest.mark.anyio
async def test_agents_restore(httpx_mock):
    httpx_mock.add_response(json=_valid(AgentView, email="bot@test.dev"))
    async with _client() as c:
        restored = await c.agents.restore("bot@test.dev")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/agents/bot%40test.dev/restore" in str(req.url)
    assert restored.email == "bot@test.dev"


# ── messages: idempotency + pagination ──────────────────────────────

@pytest.mark.anyio
async def test_send_mints_idempotency_key(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.send("bot@test.dev", {"to": ["a@x.com"], "subject": "Hi", "text": "yo"})
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/agents/bot%40test.dev/messages" in str(req.url)
    assert req.headers.get("Idempotency-Key")


@pytest.mark.anyio
async def test_send_serializes_send_at_and_parses_scheduled_result(httpx_mock):
    send_at = datetime(2026, 8, 1, 16, 0, tzinfo=timezone.utc)
    httpx_mock.add_response(
        status_code=202,
        json={
            "message_id": "msg_scheduled",
            "status": "scheduled",
            "scheduled_at": send_at.isoformat(),
        },
    )
    async with _client() as c:
        result = await c.messages.send(
            "bot@test.dev",
            {
                "to": ["a@x.com"],
                "subject": "Scheduled update",
                "text": "Hello later",
                "send_at": send_at,
            },
        )

    wire_send_at = json.loads(httpx_mock.get_requests()[-1].content)["send_at"]
    assert datetime.fromisoformat(wire_send_at.replace("Z", "+00:00")) == send_at
    assert result.status == "scheduled"
    assert result.scheduled_at == send_at


@pytest.mark.anyio
async def test_send_reply_to_string_serializes_scalar(httpx_mock):
    # The historical single-address form: a bare str reply_to on a dict body is
    # wrapped into the generated oneOf and reaches the wire as a scalar string,
    # verbatim (display name preserved).
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            {
                "to": ["a@x.com"],
                "subject": "Hi",
                "text": "yo",
                "reply_to": "Support <support@acme.com>",
            },
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == "Support <support@acme.com>"


@pytest.mark.anyio
async def test_send_reply_to_list_serializes_array(httpx_mock):
    # The new address-list form: a list reply_to reaches the wire as a JSON array
    # so the server can direct replies to several destinations.
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            {
                "to": ["a@x.com"],
                "subject": "Hi",
                "text": "yo",
                "reply_to": ["support@acme.com", "owner@acme.com"],
            },
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == ["support@acme.com", "owner@acme.com"]


@pytest.mark.anyio
async def test_reply_reply_to_string_serializes_scalar(httpx_mock):
    # The historical single-address form works on reply too: a bare str reply_to
    # is wrapped into the generated oneOf and reaches the wire verbatim.
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.reply(
            "bot@test.dev",
            "msg_1",
            {"text": "thanks", "reply_to": "Support <support@acme.com>"},
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == "Support <support@acme.com>"


@pytest.mark.anyio
async def test_reply_reply_to_list_serializes_array(httpx_mock):
    # The new address-list form works on reply too: a list reply_to reaches the
    # wire as a JSON array so replies can be directed to several destinations.
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.reply(
            "bot@test.dev",
            "msg_1",
            {"text": "thanks", "reply_to": ["support@acme.com", "owner@acme.com"]},
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == ["support@acme.com", "owner@acme.com"]


@pytest.mark.anyio
async def test_forward_reply_to_string_serializes_scalar(httpx_mock):
    # The historical single-address form works on forward too.
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.forward(
            "bot@test.dev",
            "msg_1",
            {"to": ["a@x.com"], "text": "fyi", "reply_to": "Support <support@acme.com>"},
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == "Support <support@acme.com>"


@pytest.mark.anyio
async def test_forward_reply_to_list_serializes_array(httpx_mock):
    # The new address-list form works on forward too: a list reply_to reaches
    # the wire as a JSON array so replies can be directed to several destinations.
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    async with _client() as c:
        await c.messages.forward(
            "bot@test.dev",
            "msg_1",
            {"to": ["a@x.com"], "text": "fyi", "reply_to": ["support@acme.com", "owner@acme.com"]},
        )
    wire = json.loads(httpx_mock.get_requests()[-1].content)["reply_to"]
    assert wire == ["support@acme.com", "owner@acme.com"]


@pytest.mark.anyio
async def test_reply_serializes_send_at_and_parses_scheduled_result(httpx_mock):
    send_at = datetime(2026, 8, 1, 16, 0, tzinfo=timezone.utc)
    httpx_mock.add_response(
        status_code=202,
        json={
            "message_id": "msg_scheduled_reply",
            "status": "scheduled",
            "scheduled_at": send_at.isoformat(),
        },
    )
    async with _client() as c:
        result = await c.messages.reply(
            "bot@test.dev",
            "msg_1",
            {"text": "Scheduled reply", "send_at": send_at},
        )

    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/reply" in str(req.url)
    wire_send_at = json.loads(req.content)["send_at"]
    assert datetime.fromisoformat(wire_send_at.replace("Z", "+00:00")) == send_at
    assert result.status == "scheduled"
    assert result.scheduled_at == send_at


@pytest.mark.anyio
async def test_forward_serializes_send_at_and_parses_scheduled_result(httpx_mock):
    send_at = datetime(2026, 8, 1, 16, 0, tzinfo=timezone.utc)
    httpx_mock.add_response(
        status_code=202,
        json={
            "message_id": "msg_scheduled_forward",
            "status": "scheduled",
            "scheduled_at": send_at.isoformat(),
        },
    )
    async with _client() as c:
        result = await c.messages.forward(
            "bot@test.dev",
            "msg_1",
            {"to": ["a@x.com"], "text": "Scheduled forward", "send_at": send_at},
        )

    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/forward" in str(req.url)
    wire_send_at = json.loads(req.content)["send_at"]
    assert datetime.fromisoformat(wire_send_at.replace("Z", "+00:00")) == send_at
    assert result.status == "scheduled"
    assert result.scheduled_at == send_at


@pytest.mark.anyio
async def test_reviews_approve_hits_reviews_path_no_email(httpx_mock):
    # The review queue is account-scoped + id-addressed: approve(id) must hit
    # /v1/reviews/{id}/approve, NOT the deprecated /v1/agents/{email}/... path.
    httpx_mock.add_response(json={"message_id": "msg_r1", "status": "sent"})
    async with _client() as c:
        await c.reviews.approve("msg_r1")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/reviews/msg_r1/approve" in str(req.url)
    assert "/agents/" not in str(req.url)
    assert req.headers.get("Idempotency-Key")


@pytest.mark.anyio
async def test_reviews_reject_hits_reviews_path(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_r2", "status": "rejected"})
    async with _client() as c:
        await c.reviews.reject("msg_r2", {"reason": "spam"})
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/reviews/msg_r2/reject" in str(req.url)


@pytest.mark.anyio
async def test_reviews_list_reads_reviews_endpoint(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(ReviewView, id="msg_r1")], "next_cursor": None}
    )
    async with _client() as c:
        items = await c.reviews.list().to_list(limit=50)
    assert [r.id for r in items] == ["msg_r1"]
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/v1/reviews" in str(req.url)


@pytest.mark.anyio
async def test_send_uses_caller_idempotency_key(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_2", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            {"to": ["a@x.com"], "subject": "Hi", "text": "yo"},
            idempotency_key="caller-key-123",
        )
    assert httpx_mock.get_requests()[-1].headers["Idempotency-Key"] == "caller-key-123"


@pytest.mark.anyio
async def test_send_threads_managed_unsubscribe(httpx_mock):
    # Beta: the unsubscribe kwarg lands on the generated request model and
    # serializes onto the wire (parity with the TS SDK's
    # ManagedUnsubscribeOptions).
    httpx_mock.add_response(json={"message_id": "msg_u1", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            {"to": ["a@x.com"], "subject": "Hi", "text": "yo"},
            unsubscribe={"mode": "managed"},
        )
    req = httpx_mock.get_requests()[-1]
    assert json.loads(req.content)["unsubscribe"] == {"mode": "managed"}


@pytest.mark.anyio
async def test_send_unsubscribe_kwarg_accepts_model_and_overrides_body(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_u2", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            SendEmailRequest(
                to=["a@x.com"],
                subject="Hi",
                text="yo",
                unsubscribe=UnsubscribeOptions(mode="managed"),
            ),
            unsubscribe=UnsubscribeOptions(mode="managed"),
        )
    assert json.loads(httpx_mock.get_requests()[-1].content)["unsubscribe"] == {
        "mode": "managed"
    }


@pytest.mark.anyio
async def test_send_unsubscribe_kwarg_does_not_mutate_caller_model(httpx_mock):
    # Regression: _coerce returns the caller's own model when body is already a
    # SendEmailRequest — the kwarg must land on a copy, not on the caller's
    # object, or reusing one model across sends leaks unsubscribe into later
    # sends (and managed unsubscribe requires exactly one recipient
    # server-side, so the leaked second send can fail).
    httpx_mock.add_response(json={"message_id": "msg_u6", "status": "sent"})
    request = SendEmailRequest(to=["a@x.com"], subject="Hi", text="yo")
    async with _client() as c:
        await c.messages.send("bot@test.dev", request, unsubscribe={"mode": "managed"})
    assert json.loads(httpx_mock.get_requests()[-1].content)["unsubscribe"] == {
        "mode": "managed"
    }
    assert request.unsubscribe is None


@pytest.mark.anyio
async def test_send_without_unsubscribe_omits_field(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_u3", "status": "sent"})
    async with _client() as c:
        await c.messages.send("bot@test.dev", {"to": ["a@x.com"], "subject": "Hi", "text": "yo"})
    assert "unsubscribe" not in json.loads(httpx_mock.get_requests()[-1].content)


@pytest.mark.anyio
async def test_reply_threads_managed_unsubscribe(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_u4", "status": "sent"})
    async with _client() as c:
        await c.messages.reply(
            "bot@test.dev",
            "msg_1",
            {"text": "yo"},
            unsubscribe={"mode": "managed"},
        )
    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/reply" in str(req.url)
    assert json.loads(req.content)["unsubscribe"] == {"mode": "managed"}


@pytest.mark.anyio
async def test_forward_threads_managed_unsubscribe(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_u5", "status": "sent"})
    async with _client() as c:
        await c.messages.forward(
            "bot@test.dev",
            "msg_1",
            {"to": ["a@x.com"], "text": "yo"},
            unsubscribe={"mode": "managed"},
        )
    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/forward" in str(req.url)
    assert json.loads(req.content)["unsubscribe"] == {"mode": "managed"}


@pytest.mark.anyio
async def test_reply_threads_quote_history(httpx_mock):
    # Beta: the quote_history kwarg lands on the generated request
    # model and serializes onto the wire.
    httpx_mock.add_response(json={"message_id": "msg_q1", "status": "sent"})
    async with _client() as c:
        await c.messages.reply(
            "bot@test.dev",
            "msg_1",
            {"text": "yo"},
            quote_history=True,
        )
    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/reply" in str(req.url)
    assert json.loads(req.content)["quote_history"] is True


@pytest.mark.anyio
async def test_reply_quote_history_kwarg_does_not_mutate_caller_model(httpx_mock):
    # Regression: _coerce returns the caller's own model when body is already a
    # ReplyRequest — the kwarg must land on a copy, not on the caller's object,
    # or reusing one model across replies leaks quote_history into later
    # replies.
    httpx_mock.add_response(json={"message_id": "msg_q2", "status": "sent"})
    request = ReplyRequest(text="yo")
    async with _client() as c:
        await c.messages.reply("bot@test.dev", "msg_1", request, quote_history=True)
    assert json.loads(httpx_mock.get_requests()[-1].content)["quote_history"] is True
    assert request.quote_history is None


@pytest.mark.anyio
async def test_reply_without_quote_history_omits_field(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_q3", "status": "sent"})
    async with _client() as c:
        await c.messages.reply("bot@test.dev", "msg_1", {"text": "yo"})
    assert "quote_history" not in json.loads(httpx_mock.get_requests()[-1].content)


@pytest.mark.anyio
async def test_send_wait_sent_passes_query_param(httpx_mock):
    # wait="sent" is the bounded-wait opt-in (parity with the TS SDK's
    # SendOptions.wait): it must reach the wire as ?wait=sent.
    httpx_mock.add_response(json={"message_id": "msg_w1", "status": "sent"})
    async with _client() as c:
        await c.messages.send(
            "bot@test.dev",
            {"to": ["a@x.com"], "subject": "Hi", "text": "yo"},
            wait="sent",
        )
    assert httpx_mock.get_requests()[-1].url.params["wait"] == "sent"


@pytest.mark.anyio
async def test_reply_wait_sent_passes_query_param(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_w2", "status": "sent"})
    async with _client() as c:
        await c.messages.reply("bot@test.dev", "msg_1", {"text": "yo"}, wait="sent")
    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/reply" in str(req.url)
    assert req.url.params["wait"] == "sent"


@pytest.mark.anyio
async def test_forward_wait_sent_passes_query_param(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_w3", "status": "sent"})
    async with _client() as c:
        await c.messages.forward(
            "bot@test.dev", "msg_1", {"to": ["a@x.com"], "text": "yo"}, wait="sent"
        )
    req = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/forward" in str(req.url)
    assert req.url.params["wait"] == "sent"


@pytest.mark.anyio
async def test_send_without_wait_omits_query_param(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_w4", "status": "sent"})
    async with _client() as c:
        await c.messages.send("bot@test.dev", {"to": ["a@x.com"], "subject": "Hi", "text": "yo"})
    assert "wait" not in httpx_mock.get_requests()[-1].url.params


@pytest.mark.anyio
async def test_create_api_key_retries_with_one_idempotency_key(httpx_mock):
    httpx_mock.add_response(
        status_code=503,
        json={"error": {"code": "internal_error", "message": "down", "request_id": "req_1"}},
    )
    httpx_mock.add_response(
        status_code=201,
        json=_valid(
            CreateAPIKeyResponse,
            id="apk_1",
            key="e2a_account_secret",
            key_prefix="e2a_acct_abcd",
            scope="account",
        ),
    )

    async with _client() as c:
        created = await c.account.api_keys.create({"name": "ci"})

    assert created.id == "apk_1"
    requests = httpx_mock.get_requests()
    assert len(requests) == 2
    keys = [request.headers.get("Idempotency-Key") for request in requests]
    assert keys[0]
    assert keys[1] == keys[0]


@pytest.mark.anyio
async def test_create_api_key_uses_caller_idempotency_key(httpx_mock):
    httpx_mock.add_response(
        status_code=201,
        json=_valid(
            CreateAPIKeyResponse,
            id="apk_1",
            key="e2a_account_secret",
            key_prefix="e2a_acct_abcd",
            scope="account",
        ),
    )

    async with _client() as c:
        await c.account.api_keys.create({"name": "ci"}, idempotency_key="create-key-123")

    assert httpx_mock.get_requests()[-1].headers["Idempotency-Key"] == "create-key-123"


@pytest.mark.anyio
async def test_create_webhook_retry_reuses_minted_idempotency_key(httpx_mock):
    # webhooks.create mints a one-time signing secret, so a blind retry would
    # register a SECOND subscription with a second secret. The keyed write
    # makes the retry replay: the transport retry re-sends the SAME minted
    # Idempotency-Key and the server dedupes.
    httpx_mock.add_response(
        status_code=503,
        json={"error": {"code": "internal_error", "message": "down", "request_id": "req_1"}},
    )
    httpx_mock.add_response(
        status_code=201,
        json=_valid(
            CreateWebhookResponse,
            id="wh_1",
            url="https://x.com/h",
            events=["email.received"],
            signing_secret="whsec_x",
        ),
    )

    async with _client() as c:
        created = await c.webhooks.create({"url": "https://x.com/h", "events": ["email.received"]})

    assert created.id == "wh_1"
    assert created.signing_secret == "whsec_x"
    requests = httpx_mock.get_requests()
    assert len(requests) == 2
    keys = [request.headers.get("Idempotency-Key") for request in requests]
    assert keys[0]
    assert keys[1] == keys[0]


@pytest.mark.anyio
async def test_create_webhook_uses_caller_idempotency_key(httpx_mock):
    httpx_mock.add_response(
        status_code=201,
        json=_valid(
            CreateWebhookResponse,
            id="wh_1",
            url="https://x.com/h",
            events=["email.received"],
            signing_secret="whsec_x",
        ),
    )

    async with _client() as c:
        await c.webhooks.create(
            {"url": "https://x.com/h", "events": ["email.received"]},
            idempotency_key="wh-key-123",
        )

    assert httpx_mock.get_requests()[-1].headers["Idempotency-Key"] == "wh-key-123"


@pytest.mark.anyio
async def test_messages_list_threads_cursor(httpx_mock):
    httpx_mock.add_response(json={"items": [_valid(MessageSummaryView, id="msg_1")], "next_cursor": "cur_2"})
    httpx_mock.add_response(json={"items": [_valid(MessageSummaryView, id="msg_2")], "next_cursor": None})
    async with _client() as c:
        items = await c.messages.list("bot@test.dev").to_list(limit=50)
    assert [m.id for m in items] == ["msg_1", "msg_2"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


@pytest.mark.anyio
async def test_messages_list_forwards_filter(httpx_mock):
    httpx_mock.add_response(json={"items": [], "next_cursor": None})
    async with _client() as c:
        await c.messages.list(
            "bot@test.dev", deleted=True, filter="label:urgent"
        ).to_list(limit=10)
    request = httpx_mock.get_requests()[-1]
    assert request.url.params["filter"] == "label:urgent"
    assert "q" not in request.url.params
    assert request.url.params["deleted"] == "true"


@pytest.mark.anyio
async def test_messages_get_lifecycle_uses_read_path_and_parses_contract(httpx_mock):
    httpx_mock.add_response(
        json={
            "items": [
                {
                    "id": "mlt_1",
                    "message_id": "msg_1",
                    "direction": "outbound",
                    "recipient": None,
                    "stage": "accepted",
                    "outcome": "accepted",
                    "reason_code": "acceptance.outbound_api",
                    "retryable": False,
                    "evidence": {"source": "api", "nested": {"future": True}},
                    "correlation_ids": {"request_id": "req_1", "future_id": "future_1"},
                    "occurred_at": "2026-07-22T00:00:00Z",
                    "reconstructed": False,
                },
                {
                    "id": "mlt_recon_2",
                    "message_id": "msg_1",
                    "direction": "outbound",
                    "stage": "delivery",
                    "outcome": "delivered",
                    "reason_code": "delivery.recipient_server_accepted",
                    "retryable": False,
                    "evidence": {},
                    "correlation_ids": {},
                    "occurred_at": "2026-07-22T01:00:00Z",
                    "reconstructed": True,
                },
            ],
            "next_cursor": "cur_2",
        }
    )

    async with _client() as c:
        page = await c.messages.get_lifecycle(
            "bot@test.dev", "msg_1", cursor="cur_1", limit=2
        )

    assert isinstance(page, PageMessageLifecycleTransition)
    assert page.next_cursor == "cur_2"
    assert page.items[0].recipient is None
    assert page.items[0].evidence["nested"] == {"future": True}
    assert page.items[0].correlation_ids["future_id"] == "future_1"
    assert page.items[1].recipient is None
    assert page.items[1].reconstructed is True
    request = httpx_mock.get_requests()[-1]
    assert request.method == "GET"
    assert "/v1/agents/bot%40test.dev/messages/msg_1/lifecycle" in str(request.url)
    assert request.url.params["cursor"] == "cur_1"
    assert request.url.params["limit"] == "2"


@pytest.mark.anyio
async def test_malformed_success_response_is_typed_server_error(httpx_mock):
    httpx_mock.add_response(
        status_code=200,
        headers={"X-Request-Id": "req_bad_response"},
        json={},
    )

    async with _client() as c:
        with pytest.raises(E2AServerError) as ei:
            await c.messages.list("bot@test.dev").page()

    err = ei.value
    assert not isinstance(err, E2AValidationError)
    assert err.code == "invalid_response"
    assert err.status == 200
    assert err.request_id == "req_bad_response"
    assert err.retryable is False
    assert "items" in str(err.details)
    assert isinstance(err.__cause__, ValidationError)


@pytest.mark.anyio
async def test_messages_list_deleted_lists_trash(httpx_mock):
    httpx_mock.add_response(json={"items": [], "next_cursor": None})
    async with _client() as c:
        await c.messages.list("bot@test.dev", deleted=True).to_list(limit=10)
    assert httpx_mock.get_requests()[-1].url.params["deleted"] == "true"


@pytest.mark.anyio
async def test_messages_delete_soft_by_default(httpx_mock):
    httpx_mock.add_response(json={"deleted": True, "id": "msg_1"})
    async with _client() as c:
        res = await c.messages.delete("bot@test.dev", "msg_1")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "/v1/agents/bot%40test.dev/messages/msg_1" in str(req.url)
    # The soft delete is reversible, so `permanent` is omitted entirely (wire
    # parity with the TS SDK); only the permanent path is gated on it.
    assert "permanent" not in req.url.params
    # 200 + typed deletion receipt (uniform delete contract).
    assert res.deleted is True
    assert res.id == "msg_1"


@pytest.mark.anyio
async def test_messages_delete_permanent_sends_confirm(httpx_mock):
    httpx_mock.add_response(json={"deleted": True, "id": "msg_1"})
    async with _client() as c:
        res = await c.messages.delete("bot@test.dev", "msg_1", permanent=True)
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert req.url.params["permanent"] == "true"
    # The typed call is the confirmation; the SDK supplies the raw-API guard.
    assert "confirm=DELETE" in str(req.url)
    assert res.deleted is True


@pytest.mark.anyio
async def test_messages_restore(httpx_mock):
    httpx_mock.add_response(json=_valid(MessageView, id="msg_1"))
    async with _client() as c:
        restored = await c.messages.restore("bot@test.dev", "msg_1")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/agents/bot%40test.dev/messages/msg_1/restore" in str(req.url)
    assert restored.id == "msg_1"


# ── pagination: cursor-walking endpoints ────────────────────────────
# messages/conversations/events/suppressions take a `cursor` query param; the
# AutoPager must replay next_cursor until the server returns null, threading the
# cursor on each follow-up request. (messages is covered above.)


@pytest.mark.anyio
async def test_conversations_list_threads_cursor(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(ConversationSummaryView, id="conv_1")], "next_cursor": "cur_2"}
    )
    httpx_mock.add_response(
        json={"items": [_valid(ConversationSummaryView, id="conv_2")], "next_cursor": None}
    )
    async with _client() as c:
        items = await c.conversations.list("bot@test.dev").to_list(limit=50)
    assert [cv.id for cv in items] == ["conv_1", "conv_2"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


@pytest.mark.anyio
async def test_events_list_threads_cursor(httpx_mock):
    httpx_mock.add_response(json={"items": [_valid(EventView, id="evt_1")], "next_cursor": "cur_2"})
    httpx_mock.add_response(json={"items": [_valid(EventView, id="evt_2")], "next_cursor": None})
    async with _client() as c:
        items = await c.events.list().to_list(limit=50)
    assert [e.id for e in items] == ["evt_1", "evt_2"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


@pytest.mark.anyio
async def test_suppressions_list_threads_cursor(httpx_mock):
    httpx_mock.add_response(json={"items": [_valid(SuppressionView, address="a@x.com")], "next_cursor": "cur_2"})
    httpx_mock.add_response(json={"items": [_valid(SuppressionView, address="b@x.com")], "next_cursor": None})
    async with _client() as c:
        items = await c.account.suppressions.list().to_list(limit=50)
    assert [s.address for s in items] == ["a@x.com", "b@x.com"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


@pytest.mark.anyio
async def test_suppressions_list_forwards_limit(httpx_mock):
    # The optional page-size param must reach the wire on every page (parity
    # with account.api_keys.list / agents.list_suppressions and the TS SDK's
    # account.suppressions.list({ limit })).
    httpx_mock.add_response(json={"items": [_valid(SuppressionView, address="a@x.com")], "next_cursor": "cur_2"})
    httpx_mock.add_response(json={"items": [_valid(SuppressionView, address="b@x.com")], "next_cursor": None})
    async with _client() as c:
        items = await c.account.suppressions.list(limit=1).to_list(limit=50)
    assert [s.address for s in items] == ["a@x.com", "b@x.com"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "limit=1" in str(reqs[0].url)
    assert "cursor=cur_2" in str(reqs[1].url) and "limit=1" in str(reqs[1].url)


@pytest.mark.anyio
async def test_suppressions_list_omits_limit_when_unset(httpx_mock):
    # The other half of the contract: leaving the page size unset must send NO
    # limit at all, so the server default applies. A wrapper that forwarded the
    # default as `limit=None` would serialize an empty `limit=` and change the
    # request for every existing caller.
    httpx_mock.add_response(json={"items": [_valid(SuppressionView, address="a@x.com")], "next_cursor": None})
    async with _client() as c:
        await c.account.suppressions.list().to_list(limit=5)
    url = str(httpx_mock.get_requests()[-1].url)
    assert "limit=" not in url, f"no limit must be sent when unset: {url}"


# ── pagination: keyset-cursor list endpoints ────────────────────────
# agents/domains/webhooks/deliveries/api-keys/templates/starters are all
# keyset-paginated — the AutoPager must thread next_cursor to completion,
# exactly like messages/events/suppressions. This locks in the
# consistent-pagination contract (no more silent single-page cap).


@pytest.mark.anyio
@pytest.mark.parametrize(
    "page1, page2, lister, key",
    [
        (
            {"items": [_valid(AgentView, id="ag_1", email="a@test.dev")], "next_cursor": "cur_2"},
            {"items": [_valid(AgentView, id="ag_2", email="b@test.dev")], "next_cursor": None},
            lambda c: c.agents.list(),
            lambda r: r.id,
        ),
        (
            {"items": [_valid(DomainView, domain="a.dev")], "next_cursor": "cur_2"},
            {"items": [_valid(DomainView, domain="b.dev")], "next_cursor": None},
            lambda c: c.domains.list(),
            lambda r: r.domain,
        ),
        (
            {"items": [_valid(WebhookView, id="wh_1")], "next_cursor": "cur_2"},
            {"items": [_valid(WebhookView, id="wh_2")], "next_cursor": None},
            lambda c: c.webhooks.list(),
            lambda r: r.id,
        ),
        (
            {"items": [_valid(WebhookDeliveryView, id="del_1")], "next_cursor": "cur_2"},
            {"items": [_valid(WebhookDeliveryView, id="del_2")], "next_cursor": None},
            lambda c: c.webhooks.deliveries("wh_1"),
            lambda r: r.id,
        ),
        (
            {"items": [_valid(APIKeyView, id="key_1")], "next_cursor": "cur_2"},
            {"items": [_valid(APIKeyView, id="key_2")], "next_cursor": None},
            lambda c: c.account.api_keys.list(),
            lambda r: r.id,
        ),
        (
            {"items": [_valid(TemplateSummaryView, id="tmpl_1")], "next_cursor": "cur_2"},
            {"items": [_valid(TemplateSummaryView, id="tmpl_2")], "next_cursor": None},
            lambda c: c.templates.list(),
            lambda r: r.id,
        ),
        (
            {"items": [_valid(StarterTemplateView, alias="welcome")], "next_cursor": "cur_2"},
            {"items": [_valid(StarterTemplateView, alias="digest")], "next_cursor": None},
            lambda c: c.templates.list_starters(),
            lambda r: r.alias,
        ),
    ],
    ids=["agents", "domains", "webhooks", "deliveries", "api_keys", "templates", "starters"],
)
async def test_keyset_lists_thread_cursor(httpx_mock, page1, page2, lister, key):
    httpx_mock.add_response(json=page1)
    httpx_mock.add_response(json=page2)
    async with _client() as c:
        items = await lister(c).to_list(limit=100)
    assert len(items) == 2
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


# ── pagination: multi-page walk + manual page() resume ──────────────
# End-to-end guard on the two properties auto-pagination must hold: a full
# multi-page walk yields every item exactly once (no loss, no dupes), and the
# manual `page(cursor)` primitive lets a caller checkpoint on next_cursor and
# resume mid-listing (parity with the TS SDK's `pager.page()`).


@pytest.mark.anyio
async def test_templates_list_walks_all_pages_no_loss_no_dupes(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(TemplateSummaryView, id="tmpl_1"), _valid(TemplateSummaryView, id="tmpl_2")], "next_cursor": "cur_2"}
    )
    httpx_mock.add_response(
        json={"items": [_valid(TemplateSummaryView, id="tmpl_3")], "next_cursor": "cur_3"}
    )
    httpx_mock.add_response(
        json={"items": [_valid(TemplateSummaryView, id="tmpl_4")], "next_cursor": None}
    )
    async with _client() as c:
        items = await c.templates.list(limit=2).to_list(limit=100)
    ids = [t.id for t in items]
    assert ids == ["tmpl_1", "tmpl_2", "tmpl_3", "tmpl_4"]  # ordered, no loss
    assert len(set(ids)) == len(ids)  # no dupes
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 3
    assert "cursor" not in str(reqs[0].url)
    assert "cursor=cur_2" in str(reqs[1].url) and "limit=2" in str(reqs[1].url)
    assert "cursor=cur_3" in str(reqs[2].url)


@pytest.mark.anyio
async def test_manual_page_resumes_from_cursor(httpx_mock):
    # A queue-driven consumer checkpoints next_cursor and resumes later — the
    # second fetch must carry the stored cursor, and the last page must
    # normalize its next_cursor to None.
    httpx_mock.add_response(
        json={"items": [_valid(AgentView, email="a@test.dev")], "next_cursor": "cur_2"}
    )
    async with _client() as c:
        p1 = await c.agents.list(limit=1).page()
    assert [a.email for a in p1.items] == ["a@test.dev"]
    assert p1.next_cursor == "cur_2"  # checkpoint this

    httpx_mock.add_response(
        json={"items": [_valid(AgentView, email="b@test.dev")], "next_cursor": ""}
    )
    async with _client() as c:
        p2 = await c.agents.list(limit=1).page(p1.next_cursor)  # resume
    assert [a.email for a in p2.items] == ["b@test.dev"]
    assert p2.next_cursor is None  # ""/null both normalize to None = done
    assert "cursor=cur_2" in str(httpx_mock.get_requests()[-1].url)


# ── templates (beta) ────────────────────────────────────────────────
# Full parity with the TS SDK's TemplatesResource: the stored-template CRUD +
# validate dry-run + the read-only starter catalog. Python models are snake_case
# on both sides (no camel↔snake mapping), so wire keys equal field names.


@pytest.mark.anyio
async def test_templates_list_reads_templates_endpoint(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(TemplateSummaryView, id="tmpl_1", name="Welcome")], "next_cursor": None}
    )
    async with _client() as c:
        items = await c.templates.list().to_list(limit=50)
    assert [t.id for t in items] == ["tmpl_1"]
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert str(req.url).endswith("/v1/templates")


@pytest.mark.anyio
async def test_templates_get_maps_wire_fields(httpx_mock):
    httpx_mock.add_response(
        json=_valid(
            TemplateView,
            id="tmpl_1",
            name="Welcome",
            subject="Welcome, {{name}}!",
            text="Hi {{name}}",
            html="<p>Hi {{name}}</p>",
            from_starter_alias="welcome",
            from_starter_version="1",
        )
    )
    async with _client() as c:
        tmpl = await c.templates.get("tmpl_1")
    assert tmpl.html == "<p>Hi {{name}}</p>"
    assert tmpl.from_starter_alias == "welcome"
    assert tmpl.from_starter_version == "1"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/v1/templates/tmpl_1" in str(req.url)


@pytest.mark.anyio
async def test_templates_create_posts_from_starter_body(httpx_mock):
    # Exactly the caller's fields reach the wire — no fabricated subject/body keys
    # that would trip the server's from_starter exclusivity.
    httpx_mock.add_response(
        status_code=201,
        json=_valid(TemplateView, id="tmpl_new", name="Approvals", subject="s", text="b"),
    )
    async with _client() as c:
        res = await c.templates.create({"from_starter": "approval-request", "alias": "my-approvals"})
    assert res.id == "tmpl_new"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert str(req.url).endswith("/v1/templates")
    import json as _json

    assert _json.loads(req.content) == {"from_starter": "approval-request", "alias": "my-approvals"}


@pytest.mark.anyio
async def test_templates_update_patches_and_keeps_explicit_html_clear(httpx_mock):
    httpx_mock.add_response(
        json=_valid(TemplateView, id="tmpl_1", name="Welcome", subject="New {{x}}", text="b")
    )
    async with _client() as c:
        await c.templates.update("tmpl_1", {"subject": "New {{x}}", "html": ""})
    req = httpx_mock.get_requests()[-1]
    assert req.method == "PATCH"
    assert "/v1/templates/tmpl_1" in str(req.url)
    import json as _json

    # An explicit html:"" is a deliberate clear — it must survive to the wire.
    assert _json.loads(req.content) == {"subject": "New {{x}}", "html": ""}


@pytest.mark.anyio
async def test_templates_delete_issues_delete(httpx_mock):
    httpx_mock.add_response(json={"deleted": True, "id": "tmpl_1"})
    async with _client() as c:
        res = await c.templates.delete("tmpl_1")
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "/v1/templates/tmpl_1" in str(req.url)
    # 200 + typed deletion object (uniform delete contract).
    assert res.deleted is True
    assert res.id == "tmpl_1"


@pytest.mark.anyio
async def test_templates_validate_posts_and_maps_response(httpx_mock):
    httpx_mock.add_response(
        json={
            "valid": True,
            "errors": [],
            "rendered": {"subject": "Welcome, Ada!", "text": "Hi Ada", "html": "<p>Hi Ada</p>"},
            "suggested_data": {"user": {"name": "example"}},
        }
    )
    async with _client() as c:
        res = await c.templates.validate(
            {"subject": "Welcome, {{user.name}}!", "text": "Hi {{user.name}}", "test_data": {"user": {"name": "Ada"}}}
        )
    assert res.valid is True
    assert res.rendered is not None and res.rendered.html == "<p>Hi Ada</p>"
    assert res.suggested_data == {"user": {"name": "example"}}
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/templates/validate" in str(req.url)
    import json as _json

    assert _json.loads(req.content) == {
        "subject": "Welcome, {{user.name}}!",
        "text": "Hi {{user.name}}",
        "test_data": {"user": {"name": "Ada"}},
    }


@pytest.mark.anyio
async def test_templates_get_starter_reads_starter_catalog(httpx_mock):
    httpx_mock.add_response(
        json={
            "alias": "approval-request",
            "name": "Approval request",
            "description": "Ask a human to approve an action.",
            "version": "1",
            "subject": "Approval needed: {{action}}",
            "text": "Approve: {{approve_url}}",
            "html": '<a href="{{approve_url}}">Approve</a>',
            "variables": [
                {"name": "approve_url", "required": True, "raw": False, "description": "d", "example": "https://x/approve"}
            ],
        }
    )
    async with _client() as c:
        starter = await c.templates.get_starter("approval-request")
    assert "{{approve_url}}" in starter.html
    assert starter.variables[0].name == "approve_url"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "GET"
    assert "/v1/starter-templates/approval-request" in str(req.url)


@pytest.mark.anyio
async def test_templates_create_maps_parse_failure_to_validation_error(httpx_mock):
    # A template-part parse failure (400 invalid_template) must surface as a typed,
    # non-retryable E2AValidationError carrying the machine code.
    httpx_mock.add_response(
        status_code=400,
        json={"error": {"code": "invalid_template", "message": "template part body failed to parse"}},
    )
    async with _client() as c:
        with pytest.raises(E2AValidationError) as ei:
            await c.templates.create({"name": "x", "subject": "s", "text": "{{#bad}}"})
    assert ei.value.code == "invalid_template"
    assert ei.value.retryable is False


@pytest.mark.anyio
async def test_templates_round_trip_create_update_delete(httpx_mock):
    # A create→update→delete round-trip against the mocked generated layer,
    # asserting each hits the right method+path.
    httpx_mock.add_response(status_code=201, json=_valid(TemplateView, id="tmpl_rt", name="RT", subject="s", text="b"))
    httpx_mock.add_response(json=_valid(TemplateView, id="tmpl_rt", name="RT", subject="s2", text="b"))
    httpx_mock.add_response(json={"deleted": True, "id": "tmpl_rt"})
    async with _client() as c:
        created = await c.templates.create({"name": "RT", "subject": "s", "text": "b"})
        assert created.id == "tmpl_rt"
        updated = await c.templates.update("tmpl_rt", {"subject": "s2"})
        assert updated.subject == "s2"
        deleted = await c.templates.delete("tmpl_rt")
        assert deleted.deleted is True and deleted.id == "tmpl_rt"
    methods = [(r.method, str(r.url)) for r in httpx_mock.get_requests()]
    assert methods[0][0] == "POST" and methods[0][1].endswith("/v1/templates")
    assert methods[1][0] == "PATCH" and "/v1/templates/tmpl_rt" in methods[1][1]
    assert methods[2][0] == "DELETE" and "/v1/templates/tmpl_rt" in methods[2][1]


def test_templates_view_types_exported_from_v1():
    # Parity with the TS SDK, which re-exports TemplateView et al. from its public
    # index. In Python these ride the `from generated.models import *` wildcard —
    # the same mechanism as AgentView/DomainView — so they must import from e2a.v1.
    from e2a.v1 import (  # noqa: F401
        CreateTemplateRequest,
        StarterTemplateDetailView,
        StarterTemplateView,
        TemplateSummaryView,
        TemplateView,
        UpdateTemplateRequest,
        ValidateTemplateRequest,
        ValidateTemplateResponse,
    )

    assert TemplateView is not None
    assert ValidateTemplateResponse is not None


# ── error mapping ───────────────────────────────────────────────────

@pytest.mark.anyio
async def test_404_maps_to_not_found(httpx_mock):
    httpx_mock.add_response(
        status_code=404, json={"error": {"code": "agent_not_found", "message": "no such agent"}}
    )
    async with _client() as c:
        with pytest.raises(E2ANotFoundError) as ei:
            await c.agents.get("ghost@test.dev")
    assert ei.value.code == "agent_not_found"
    assert ei.value.status == 404


@pytest.mark.anyio
async def test_409_maps_to_conflict(httpx_mock):
    httpx_mock.add_response(
        status_code=409, json={"error": {"code": "domain_exists", "message": "dup"}}
    )
    async with _client() as c:
        with pytest.raises(E2AConflictError):
            await c.domains.create({"domain": "dup.dev"})


@pytest.mark.anyio
async def test_422_maps_to_validation(httpx_mock):
    httpx_mock.add_response(
        status_code=422, json={"error": {"code": "invalid_request", "message": "bad"}}
    )
    async with _client() as c:
        with pytest.raises(E2AValidationError):
            # A coercible body so the request reaches the server, which 422s.
            await c.agents.create({"email": "x@test.dev"})


@pytest.mark.anyio
async def test_retries_500_then_succeeds(httpx_mock):
    httpx_mock.add_response(status_code=503, json={"error": {"code": "x", "message": "down"}})
    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    async with _client() as c:
        agent = await c.agents.get("bot@test.dev")  # GET is retryable
    assert agent.email == "bot@test.dev"
    assert len(httpx_mock.get_requests()) == 2


# ── listen() ────────────────────────────────────────────────────────

def test_listen_requires_email():
    c = _client()
    with pytest.raises(E2AError, match="email is required"):
        c.listen("")


@pytest.mark.anyio
async def test_invalid_request_body_raises_typed_validation_error():
    # Regression: a bad body must raise E2AValidationError, not a raw pydantic
    # ValidationError (the SDK contract is "everything is an E2AError").
    async with _client() as c:
        with pytest.raises(E2AValidationError):
            await c.agents.create([1, 2, 3])


# --- protection read-modify-write (request/response schema-split regression) ---

_PROTECTION_JSON = {
    "holds": {"ttl_seconds": 3600, "on_expiry": "approve"},
    "inbound": {
        "gate": {"policy": "allowlist", "allowlist": ["partner@acme.com"], "action": "review"},
        "scan": {"sensitivity": "high"},
    },
    "outbound": {
        "gate": {"policy": "domain", "allowlist": ["acme.com"], "action": "block"},
        "scan": {"sensitivity": "off"},
    },
}


@pytest.mark.anyio
async def test_protection_read_modify_write_accepts_view(httpx_mock):
    """get_protection() returns a ProtectionConfigView; feeding it (mutated)
    straight back into replace_protection() must coerce to the Request twin —
    the natural RMW loop broke when the request/response schemas split."""
    httpx_mock.add_response(json=_PROTECTION_JSON)  # GET
    httpx_mock.add_response(json=_PROTECTION_JSON)  # PUT echo
    async with _client() as c:
        cfg = await c.agents.get_protection("bot@test.dev")
        cfg.holds.on_expiry = "reject"
        out = await c.agents.replace_protection("bot@test.dev", cfg)
    assert out.holds.ttl_seconds == 3600
    put = httpx_mock.get_requests()[-1]
    assert put.method == "PUT"
    body = json.loads(put.content)
    assert body["holds"]["on_expiry"] == "reject"  # the mutation survived coercion
    assert body["inbound"]["gate"]["allowlist"] == ["partner@acme.com"]


# ── Contacts (beta) ──────────────────────────────────────────────────────────
# Contacts are account-level identity. The address is the resource key, so these
# pin that it is URL-encoded on the wire and that the SDK supplies the
# ?confirm=DELETE guard the raw API requires.


@pytest.mark.anyio
async def test_contacts_get_url_encodes_the_address(httpx_mock):
    httpx_mock.add_response(
        json={
            "address": "partner@fund.vc",
            "display_name": "A. Partner",
            "metadata": {"fund": "Example Capital"},
            "source": "import",
            "import_batch_id": "imp_1",
            "created_at": "2026-07-01T00:00:00Z",
            "updated_at": "2026-07-01T00:00:00Z",
        }
    )
    async with _client() as c:
        contact = await c.contacts.get("partner@fund.vc")
    assert contact.display_name == "A. Partner"
    assert contact.import_batch_id == "imp_1"
    req = httpx_mock.get_requests()[-1]
    # The @ must not reach the path raw — this is the encoded-routing contract.
    assert "/v1/contacts/partner%40fund.vc" in str(req.url)


@pytest.mark.anyio
async def test_contacts_create_posts_body_and_returns_canonical_address(httpx_mock):
    httpx_mock.add_response(
        status_code=201,
        json={
            "address": "partner@fund.vc",
            "display_name": "A. Partner",
            "metadata": {},
            "source": "manual",
            "created_at": "2026-07-01T00:00:00Z",
            "updated_at": "2026-07-01T00:00:00Z",
        },
    )
    async with _client() as c:
        contact = await c.contacts.create(
            {"address": "A. Partner <Partner@Fund.VC>", "display_name": "A. Partner"},
            idempotency_key="contact:partner",
        )
    assert contact.address == "partner@fund.vc"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert req.headers["Idempotency-Key"] == "contact:partner"
    assert json.loads(req.content)["address"] == "A. Partner <Partner@Fund.VC>"


@pytest.mark.anyio
async def test_contacts_update_sends_only_given_fields(httpx_mock):
    httpx_mock.add_response(
        json={
            "address": "partner@fund.vc",
            "display_name": "Renamed",
            "metadata": {"fund": "Example Capital"},
            "source": "import",
            "created_at": "2026-07-01T00:00:00Z",
            "updated_at": "2026-07-02T00:00:00Z",
        }
    )
    async with _client() as c:
        contact = await c.contacts.update("partner@fund.vc", {"display_name": "Renamed"})
    # Metadata survives a name-only patch — omitting the key is what tells the
    # server to leave the stored value alone.
    assert contact.metadata == {"fund": "Example Capital"}
    req = httpx_mock.get_requests()[-1]
    assert req.method == "PATCH"
    assert json.loads(req.content) == {"display_name": "Renamed"}


@pytest.mark.anyio
async def test_contacts_delete_supplies_confirm_guard(httpx_mock):
    httpx_mock.add_response(json={"deleted": True, "address": "partner@fund.vc"})
    async with _client() as c:
        result = await c.contacts.delete("partner@fund.vc")
    assert result.deleted is True
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "confirm=DELETE" in str(req.url)


@pytest.mark.anyio
async def test_contacts_import_returns_per_row_results(httpx_mock):
    httpx_mock.add_response(
        json={
            "batch_id": "imp_9",
            "created": 1,
            "updated": 0,
            "skipped": 0,
            "failed": 1,
            "results": [
                {"index": 0, "address": "ok@fund.vc", "status": "created", "suppressed": True},
                {"index": 1, "status": "failed", "code": "invalid_recipient", "message": "bad"},
            ],
        }
    )
    async with _client() as c:
        result = await c.contacts.import_(
            {"contacts": [{"address": "ok@fund.vc"}, {"address": "nope"}]}
        )
    assert result.batch_id == "imp_9"
    # A suppressed row is still reported as created — marked, never dropped.
    assert result.results[0].status == "created"
    assert result.results[0].suppressed is True
    assert result.results[1].code == "invalid_recipient"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/contacts/import" in str(req.url)
    assert req.headers["Idempotency-Key"]


@pytest.mark.anyio
async def test_contacts_import_preserves_caller_idempotency_key(httpx_mock):
    httpx_mock.add_response(
        json={
            "batch_id": "imp_replay",
            "created": 0,
            "updated": 0,
            "skipped": 1,
            "failed": 0,
            "results": [{"index": 0, "address": "ok@fund.vc", "status": "skipped"}],
        }
    )
    async with _client() as c:
        await c.contacts.import_(
            {"contacts": [{"address": "ok@fund.vc"}]},
            idempotency_key="contacts:upload:sha256",
        )
    assert httpx_mock.get_requests()[-1].headers["Idempotency-Key"] == "contacts:upload:sha256"


@pytest.mark.anyio
async def test_contacts_delete_import_reverses_a_batch(httpx_mock):
    httpx_mock.add_response(
        json={
            "deleted": True,
            "batch_id": "imp_9",
            "contacts_deleted": 2,
            "contacts_retained": 0,
            "engagements_deleted": 1,
        }
    )
    async with _client() as c:
        result = await c.contacts.delete_import("imp_9")
    assert result.contacts_deleted == 2
    assert result.engagements_deleted == 1
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "/v1/contacts/imports/imp_9" in str(req.url)
    assert "confirm=DELETE" in str(req.url)


@pytest.mark.anyio
async def test_contacts_list_auto_pages(httpx_mock):
    httpx_mock.add_response(
        json={
            "items": [
                {
                    "address": "a@x.vc", "display_name": "", "metadata": {},
                    "source": "manual",
                    "created_at": "2026-07-01T00:00:00Z",
                    "updated_at": "2026-07-01T00:00:00Z",
                }
            ],
            "next_cursor": "cur_2",
        }
    )
    httpx_mock.add_response(
        json={
            "items": [
                {
                    "address": "b@x.vc", "display_name": "", "metadata": {},
                    "source": "manual",
                    "created_at": "2026-07-01T00:00:00Z",
                    "updated_at": "2026-07-01T00:00:00Z",
                }
            ],
            "next_cursor": None,
        }
    )
    async with _client() as c:
        items = await c.contacts.list().to_list(limit=50)
    assert [i.address for i in items] == ["a@x.vc", "b@x.vc"]
    assert "cursor=cur_2" in str(httpx_mock.get_requests()[-1].url)


@pytest.mark.anyio
async def test_contacts_list_exposes_creation_time_filters(httpx_mock):
    httpx_mock.add_response(json={"items": [], "next_cursor": None})
    async with _client() as c:
        await c.contacts.list(
            created_after=datetime(2026, 7, 1, tzinfo=timezone.utc),
            created_before=datetime(2026, 7, 31, tzinfo=timezone.utc),
        ).to_list(limit=50)
    url = str(httpx_mock.get_requests()[-1].url)
    assert "created_after=2026-07-01T00%3A00%3A00" in url
    assert "created_before=2026-07-31T00%3A00%3A00" in url


@pytest.mark.anyio
async def test_contacts_exposes_etag_and_sends_if_match(httpx_mock):
    contact = {
        "address": "partner@fund.vc", "display_name": "A. Partner",
        "metadata": {}, "source": "manual",
        "created_at": "2026-07-01T00:00:00Z",
        "updated_at": "2026-07-01T00:00:00Z",
    }
    httpx_mock.add_response(json=contact, headers={"ETag": '"contact-v1"'})
    httpx_mock.add_response(json={**contact, "display_name": "Renamed"})
    async with _client() as c:
        item, etag = await c.contacts.get_with_etag("partner@fund.vc")
        assert item.address == "partner@fund.vc"
        assert etag == '"contact-v1"'
        await c.contacts.update(
            "partner@fund.vc", {"display_name": "Renamed"}, if_match=etag,
        )
    assert httpx_mock.get_requests()[-1].headers["If-Match"] == '"contact-v1"'


@pytest.mark.anyio
async def test_outreach_exposes_etag_and_sends_if_match(httpx_mock):
    engagement = {
        "agent_email": "raise@example.com", "address": "partner@fund.vc",
        "stage": "touch1", "metadata": {}, "replied": False,
        "suppressed": False, "outbound_count": 0, "inbound_count": 0,
        "contact": {"address": "partner@fund.vc", "display_name": "", "metadata": {}},
        "created_at": "2026-07-01T00:00:00Z",
        "updated_at": "2026-07-01T00:00:00Z",
    }
    httpx_mock.add_response(json=engagement, headers={"ETag": '"outreach-v1"'})
    httpx_mock.add_response(json={**engagement, "stage": "touch2"})
    async with _client() as c:
        item, etag = await c.contacts.get_outreach_with_etag(
            "raise@example.com", "partner@fund.vc",
        )
        assert item.stage == "touch1"
        await c.contacts.set_outreach(
            "raise@example.com", "partner@fund.vc",
            {"stage": "touch2"}, if_match=etag,
        )
    assert httpx_mock.get_requests()[-1].headers["If-Match"] == '"outreach-v1"'
