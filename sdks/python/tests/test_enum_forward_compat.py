"""Forward-compatibility: generated models must NOT raise on unknown enum values.

A new server-side enum value (a new event ``type``, ``delivery_status``,
``inbound_policy``, …) is an additive, non-breaking change. OpenAPI Generator
emits ``*_validate_enum`` pydantic validators that raise ``ValueError`` on any
value outside a hard-coded set — which would turn that additive change into a
crash for every deployed client. ``scripts/strip-enum-validators.py`` (run by
generate-oag.sh) removes them; these tests lock that in so a regeneration can't
silently reintroduce the crash. Mirrors the TypeScript SDK's passthrough.
"""

from __future__ import annotations

import glob
import os

import pytest
from pydantic import ValidationError

from e2a.v1.generated.models.event_view import EventView
from e2a.v1.generated.models.message_lifecycle_transition import (
    MessageLifecycleTransition,
)


CANONICAL_LIFECYCLE_VALUES = {
    "direction": ["inbound", "outbound"],
    "stage": [
        "accepted",
        "authentication",
        "review",
        "suppression",
        "queued",
        "submission",
        "delivery",
        "complaint",
    ],
    "outcome": [
        "accepted",
        "passed",
        "failed",
        "indeterminate",
        "pending",
        "approved",
        "rejected",
        "blocked",
        "applied",
        "enqueued",
        "deferred",
        "delivered",
        "bounced",
        "reported",
    ],
    "reason_code": [
        "acceptance.inbound_smtp",
        "acceptance.outbound_api",
        "acceptance.local_loopback",
        "authentication.dmarc_pass",
        "authentication.dmarc_fail",
        "authentication.dmarc_none",
        "authentication.dmarc_temporary_error",
        "authentication.dmarc_permanent_error",
        "review.hold_created",
        "review.approved",
        "review.rejected",
        "review.expired_approved",
        "review.expired_rejected",
        "suppression.recipient_blocked",
        "suppression.hard_bounce_applied",
        "suppression.complaint_applied",
        "queue.inbound_processing",
        "queue.outbound_submission",
        "submission.upstream_accepted",
        "submission.local_loopback_accepted",
        "submission.temporary_failure",
        "submission.provider_rejected",
        "submission.local_retries_exhausted",
        "submission.cancelled",
        "submission.policy_budget_expired",
        "submission.sending_setup_expired",
        "delivery.recipient_server_accepted",
        "delivery.temporary_delay",
        "delivery.permanent_bounce",
        "delivery.transient_bounce",
        "delivery.undetermined_bounce",
        "complaint.recipient_reported",
    ],
}


def lifecycle_transition(**overrides: str) -> MessageLifecycleTransition:
    values = {
        "correlation_ids": {},
        "direction": "outbound",
        "evidence": {},
        "id": "mlt_1",
        "message_id": "msg_1",
        "occurred_at": "2026-07-22T00:00:00Z",
        "outcome": "accepted",
        "reason_code": "acceptance.outbound_api",
        "reconstructed": False,
        "retryable": False,
        "stage": "accepted",
    }
    values.update(overrides)
    return MessageLifecycleTransition.model_validate(values)


def test_unknown_event_type_parses() -> None:
    e = EventView.from_dict(
        {
            "id": "evt_1",
            "type": "email.some_future_type",  # not in the known catalog
            "status": "delivered",
            "schema_version": "1",
            "created_at": "2026-06-18T00:00:00Z",
            "agent_email": "a@x.com",
            "data": {},
        }
    )
    assert e is not None
    assert e.type == "email.some_future_type"


def test_unknown_event_status_parses() -> None:
    e = EventView.from_dict(
        {
            "id": "evt_1",
            "type": "email.received",
            "status": "future_status",
            "schema_version": "1",
            "created_at": "2026-06-18T00:00:00Z",
            "agent_email": "a@x.com",
            "data": {},
        }
    )
    assert e is not None
    assert e.status == "future_status"


@pytest.mark.parametrize(
    ("field", "value"),
    [
        (field, value)
        for field, values in CANONICAL_LIFECYCLE_VALUES.items()
        for value in values
    ],
)
def test_every_canonical_lifecycle_value_parses(field: str, value: str) -> None:
    transition = lifecycle_transition(**{field: value})
    assert getattr(transition, field) == value


@pytest.mark.parametrize("field", CANONICAL_LIFECYCLE_VALUES)
def test_unknown_lifecycle_values_are_rejected(field: str) -> None:
    with pytest.raises(ValidationError, match=field):
        lifecycle_transition(**{field: "future_unknown_value"})


def test_only_closed_lifecycle_enum_validators_remain_in_generated_models() -> None:
    root = os.path.join(
        os.path.dirname(__file__), "..", "src", "e2a", "v1", "generated", "models"
    )
    offenders = [
        os.path.basename(p)
        for p in glob.glob(os.path.join(root, "*.py"))
        if "_validate_enum" in open(p, encoding="utf-8").read()
    ]
    assert offenders == ["message_lifecycle_transition.py"]
