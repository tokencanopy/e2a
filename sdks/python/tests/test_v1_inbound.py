import json
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock

import pytest

from e2a.v1.errors import E2AValidationError
from e2a.v1.client import AsyncE2AClient
from e2a.v1.generated.models import (
    AttachmentView,
    MessageSummaryView,
    MessageView,
    SendResultView,
)
from e2a.v1.inbound import AsyncInboundResource
from e2a.v1.webhook_signature import WebhookEvent


FIXTURES = Path(__file__).resolve().parents[2] / "testdata" / "inbound-email"


def load(name: str) -> Any:
    return json.loads((FIXTURES / name).read_text())


def event_from_wire(value: dict[str, Any]) -> WebhookEvent:
    return WebhookEvent(
        type=value["type"],
        id=value["id"],
        schema_version=value["schema_version"],
        created_at=value["created_at"],
        data=value["data"],
        raw=value,
    )


def operations(message: MessageView) -> Any:
    pending = SendResultView(message_id="msg_reply", status="pending_review")
    attachment = AttachmentView(
        index=0,
        size_bytes=1,
        download_url="https://download.example/one",
        expires_at="2026-07-01T11:00:00Z",
    )

    class Operations:
        get = AsyncMock(return_value=message)
        get_attachment = AsyncMock(return_value=attachment)
        reply = AsyncMock(return_value=pending)
        forward = AsyncMock(return_value=pending)

    return Operations(), pending, attachment


def test_async_client_exposes_inbound_resource() -> None:
    client = AsyncE2AClient(api_key="e2a_test", base_url="http://test.local")
    assert isinstance(client.inbound, AsyncInboundResource)


def test_message_models_deserialize_optional_beta_thread_ids() -> None:
    detail_wire = dict(load("minimal.json")["message"])
    detail_wire["thread_id"] = "thr_0123456789abcdef0123456789abcdef"
    detail = MessageView.from_dict(detail_wire)
    summary = MessageSummaryView.from_dict(
        {
            "id": "msg_summary",
            "direction": "inbound",
            "header_from": "alice@example.com",
            "envelope_from": "alice@example.com",
            "verified_domain": "example.com",
            "to": ["bot@example.com"],
            "delivered_to": "bot@example.com",
            "subject": "hello",
            "read_status": "unread",
            "labels": [],
            "created_at": "2026-07-01T10:00:00Z",
            "thread_id": "thr_fedcba9876543210fedcba9876543210",
        }
    )
    legacy = MessageView.from_dict(load("minimal.json")["message"])

    assert detail is not None
    assert summary is not None
    assert legacy is not None
    assert detail.thread_id == "thr_0123456789abcdef0123456789abcdef"
    assert summary.thread_id == "thr_fedcba9876543210fedcba9876543210"
    assert legacy.thread_id is None
    assert not MessageView.model_fields["thread_id"].is_required()
    assert not MessageSummaryView.model_fields["thread_id"].is_required()


@pytest.mark.anyio
@pytest.mark.parametrize("name", ["full.json", "minimal.json", "adversarial.json"])
async def test_shared_conformance_vectors(name: str) -> None:
    vector = load(name)
    message = MessageView.from_dict(vector["message"])
    assert message is not None
    ops, _, _ = operations(message)

    event = event_from_wire(vector["event"])
    email = await AsyncInboundResource(ops).from_event(event)

    assert email.to_dict() == vector["expected"]
    assert email.event is event
    ops.get.assert_awaited_once_with(
        vector["event"]["data"]["delivered_to"],
        vector["event"]["data"]["message_id"],
    )
    assert "raw_message" not in repr(email)
    assert "download_url" not in repr(email)


@pytest.mark.anyio
async def test_invalid_vectors_fail_before_transport() -> None:
    base = load("full.json")
    message = MessageView.from_dict(base["message"])
    assert message is not None
    ops, _, _ = operations(message)
    inbound = AsyncInboundResource(ops)

    for item in load("invalid.json"):
        raw = json.loads(json.dumps(base["event"]))
        raw.update(item.get("patch", {}))
        for key, value in item.get("data_patch", {}).items():
            if value == "__delete__":
                raw["data"].pop(key, None)
            else:
                raw["data"][key] = value
        event = event_from_wire(raw)

        with pytest.raises(E2AValidationError) as exc:
            await inbound.from_event(event)
        assert exc.value.code == "invalid_email_received_event", item["name"]
        assert exc.value.status == 0
        assert not exc.value.retryable

    ops.get.assert_not_awaited()


@pytest.mark.anyio
async def test_bound_operations_preserve_results_and_keys() -> None:
    vector = load("full.json")
    message = MessageView.from_dict(vector["message"])
    assert message is not None
    ops, pending, attachment = operations(message)
    email = await AsyncInboundResource(ops).from_event(event_from_wire(vector["event"]))

    assert await email.reply({"text": "Got it"}, idempotency_key="reply:evt") is pending
    ops.reply.assert_awaited_once_with(
        vector["expected"]["inbox"],
        vector["expected"]["id"],
        {"text": "Got it"},
        idempotency_key="reply:evt",
    )

    assert await email.forward({"to": ["ops@example.com"], "text": "FYI"}) is pending
    ops.forward.assert_awaited_once_with(
        vector["expected"]["inbox"],
        vector["expected"]["id"],
        {"to": ["ops@example.com"], "text": "FYI"},
        idempotency_key=None,
    )

    assert await email.attachments[0].get(inline=True) is attachment
    ops.get_attachment.assert_awaited_once_with(
        vector["expected"]["inbox"], vector["expected"]["id"], 0, inline=True
    )
