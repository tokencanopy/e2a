"""Live ergonomic-facade coverage for the Python SDK — the resource-coverage
gate's test half (see ``tests/resource_coverage_gate.py`` for the gate itself
and the full scope/design writeup).

``tests/test_e2e.py`` already exercises 7 methods (``info``, ``agents.create``,
``agents.delete``, ``messages.send``, ``messages.list``, ``messages.get``,
``messages.reply``) as part of its self-loopback round trip and now calls
``mark_covered`` for each. This module fills in the rest of the 64 methods
runtime introspection finds on ``AsyncE2AClient``
(``tests/resource_coverage_lib/discovery.py``), minus the two destructive/
no-happy-path entries allowlisted in the gate (``account.delete``,
``account.suppressions.delete``) and ``listen`` (allowlisted — see the gate
for why).

Every test here calls a real method against LIVE staging and asserts on the
ACTUAL RETURNED DATA, then calls ``mark_covered("resource.method")`` — never
the other way around, and never behind a bare ``pytest.skip()`` or an
``if not x: return``. A missing fixture (no starter templates, no held
review, ...) is a hard test failure, not a silent skip — that exact anti-
pattern (a happy path silently no-op'ing on an empty precondition while the
suite reports green) is the bug class this whole exercise exists to close.

Same env contract as test_e2e.py: E2A_TEST_BASE_URL / E2A_TEST_API_KEY /
E2A_TEST_AGENT_EMAIL (skips cleanly when absent). E2E_SINK_EMAIL defaults to
the SES mailbox simulator's always-succeeds address, matching the e2e-prod
harness's convention (tests/e2e-prod/harness/fixtures.ts).
"""

from __future__ import annotations

import asyncio
import base64
import os
import time

import pytest

from e2a import AsyncE2AClient

from .resource_coverage_lib.tracker import mark_covered

BASE_URL = os.environ.get("E2A_TEST_BASE_URL", "")
API_KEY = os.environ.get("E2A_TEST_API_KEY", "")
AGENT = os.environ.get("E2A_TEST_AGENT_EMAIL", "")
SINK_EMAIL = os.environ.get("E2E_SINK_EMAIL", "success@simulator.amazonses.com")

pytestmark = [
    pytest.mark.anyio,
    pytest.mark.skipif(
        not BASE_URL or not API_KEY or not AGENT,
        reason="E2A_TEST_BASE_URL, E2A_TEST_API_KEY, E2A_TEST_AGENT_EMAIL required for live e2e",
    ),
]


@pytest.fixture
def anyio_backend() -> str:
    # Pin the asyncio backend, matching test_e2e.py.
    return "asyncio"


def _domain() -> str:
    return AGENT.split("@", 1)[1]


def _slug(label: str) -> str:
    return f"py-sdk-cov-{label}-{int(time.time() * 1000):x}-{os.getpid()}"


def _bot(label: str) -> str:
    return f"{_slug(label)}@{_domain()}"


def _client() -> AsyncE2AClient:
    return AsyncE2AClient(api_key=API_KEY, base_url=BASE_URL)


async def _find_inbound(client: AsyncE2AClient, email: str, subject: str, *, attempts: int = 15, delay: float = 1.5):
    for _ in range(attempts):
        msgs = await client.messages.list(email, direction="inbound", limit=20).to_list(limit=20)
        found = next((m for m in msgs if m.subject == subject), None)
        if found:
            return found
        await asyncio.sleep(delay)
    raise AssertionError(f"no inbound message with subject {subject!r} appeared within ~{attempts * delay:.0f}s")


async def _hold_all_outbound(client: AsyncE2AClient, email: str):
    # Same shape as tests/e2e-prod/harness/fixtures.ts's holdAllOutbound(): an
    # outbound review gate with policy=allowlist + action=review and an empty
    # allowlist, so every recipient is unknown and every send is held.
    return await client.agents.replace_protection(
        email,
        {
            "inbound": {"gate": {}, "scan": {}},
            "outbound": {"gate": {"policy": "allowlist", "action": "review", "allowlist": []}, "scan": {}},
            "holds": {},
        },
    )


# ── account + api keys ──────────────────────────────────────────────


async def test_account_and_api_keys():
    async with _client() as client:
        account = await client.account.get()
        assert account.scope == "account"
        assert account.usage.agents >= 0
        mark_covered("account.get")

        export = await client.account.export()
        assert export.schema_version
        assert isinstance(export.suppressions, list)
        mark_covered("account.export")

        # Account-level suppressions have no create operation (only bounces /
        # complaints populate them — see resource_coverage_gate.py's ALLOWLIST
        # for .delete), so .list is exercised for its shape, not its contents.
        page = await client.account.suppressions.list().to_list(limit=5)
        assert isinstance(page, list)
        mark_covered("account.suppressions.list")

        key_name = f"py-sdk-cov-key-{int(time.time() * 1000):x}"
        created = await client.account.api_keys.create({"name": key_name})
        assert created.key.startswith("e2a_acct_")
        assert created.name == key_name
        mark_covered("account.api_keys.create")
        try:
            keys = await client.account.api_keys.list(limit=50).to_list(limit=50)
            assert any(k.id == created.id for k in keys)
            mark_covered("account.api_keys.list")
        finally:
            deleted = await client.account.api_keys.delete(created.id)
            assert deleted.deleted is True
            assert deleted.id == created.id
            mark_covered("account.api_keys.delete")


# ── agents lifecycle + agent-scoped suppressions ────────────────────


async def test_agents_lifecycle_and_suppressions():
    async with _client() as client:
        email = _bot("agents")
        created = await client.agents.create({"email": email, "name": "cov agents"})
        assert created.email == email
        mark_covered("agents.create")
        try:
            got = await client.agents.get(email)
            assert got.email == email
            mark_covered("agents.get")

            updated = await client.agents.update(email, {"name": "cov agents updated"})
            assert updated.name == "cov agents updated"
            mark_covered("agents.update")

            agents = await client.agents.list(limit=100).to_list(limit=100)
            assert any(a.email == email for a in agents)
            mark_covered("agents.list")

            protection = await client.agents.get_protection(email)
            assert protection.outbound.gate.policy == "open"  # default, before replace below
            mark_covered("agents.get_protection")

            replaced = await _hold_all_outbound(client, email)
            assert replaced.outbound.gate.policy == "allowlist"
            assert replaced.outbound.gate.action == "review"
            mark_covered("agents.replace_protection")

            tested = await client.agents.test(email)
            assert tested.message_id
            assert tested.status in ("accepted", "sent", "pending_review")
            mark_covered("agents.test")

            suppress_addr = f"suppressed-{int(time.time() * 1000):x}@example.com"
            suppression = await client.agents.create_suppression(
                email, {"address": suppress_addr, "reason": "py-sdk coverage gate"}
            )
            assert suppression.address == suppress_addr
            mark_covered("agents.create_suppression")

            suppressions = await client.agents.list_suppressions(email).to_list(limit=20)
            assert any(s.address == suppress_addr for s in suppressions)
            mark_covered("agents.list_suppressions")

            deleted_suppression = await client.agents.delete_suppression(email, suppress_addr)
            assert deleted_suppression.deleted is True
            mark_covered("agents.delete_suppression")

            deleted_agent = await client.agents.delete(email)
            assert deleted_agent.deleted is True
            assert deleted_agent.email == email
            mark_covered("agents.delete")

            restored = await client.agents.restore(email)
            assert restored.email == email
            assert restored.deleted_at is None
            mark_covered("agents.restore")
        finally:
            await client.agents.delete(email)


# ── messages extended surface + conversations + the inbound facade ──


async def test_messages_extended_surface_conversations_and_inbound_facade():
    async with _client() as client:
        email = _bot("msg")
        await client.agents.create({"email": email, "name": "cov messages"})
        try:
            subject = f"py-sdk-cov-msg {int(time.time() * 1000)}"
            attachment_bytes = b"hello from the python sdk coverage gate"
            sent = await client.messages.send(
                email,
                {
                    "to": [email],
                    "subject": subject,
                    "text": "cov msg body",
                    "attachments": [
                        {
                            "filename": "note.txt",
                            "content_type": "text/plain",
                            "data": base64.b64encode(attachment_bytes).decode(),
                        }
                    ],
                },
            )
            assert sent.status in ("sent", "accepted")

            found = await _find_inbound(client, email, subject)
            assert found.subject == subject

            full = await client.messages.get(email, found.id)
            assert full.attachments and full.attachments[0].filename == "note.txt"

            attachment = await client.messages.get_attachment(email, found.id, 0, inline=True)
            assert attachment.filename == "note.txt"
            assert base64.b64decode(attachment.data) == attachment_bytes
            mark_covered("messages.get_attachment")

            lifecycle = await client.messages.get_lifecycle(email, found.id)
            assert lifecycle.items, "expected at least one recorded lifecycle transition"
            assert lifecycle.items[0].message_id == found.id
            mark_covered("messages.get_lifecycle")

            labeled = await client.messages.update_labels(email, found.id, {"add_labels": ["cov-test"]})
            assert "cov-test" in labeled.labels
            assert labeled.message_id == found.id
            mark_covered("messages.update_labels")

            forwarded = await client.messages.forward(
                email, found.id, {"to": [SINK_EMAIL], "text": "forwarded by the py-sdk coverage gate"}
            )
            assert forwarded.message_id
            assert forwarded.status in ("sent", "accepted", "pending_review")
            mark_covered("messages.forward")

            deleted = await client.messages.delete(email, found.id)
            assert deleted.deleted is True
            assert deleted.id == found.id
            mark_covered("messages.delete")

            restored = await client.messages.restore(email, found.id)
            assert restored.id == found.id
            assert restored.deleted_at is None
            mark_covered("messages.restore")

            conversations = await client.conversations.list(email).to_list(limit=20)
            assert any(c.id == found.conversation_id for c in conversations)
            mark_covered("conversations.list")

            detail = await client.conversations.get(email, found.conversation_id)
            assert detail.id == found.conversation_id
            assert detail.message_count >= 1
            mark_covered("conversations.get")

            class _Envelope:
                """Minimal structural stand-in for the webhook/WS event
                envelope (matches inbound.InboundEvent / client.EventLike)."""

                id = "evt_cov_placeholder"
                type = "email.received"
                schema_version = "1"
                created_at = "2026-01-01T00:00:00Z"
                data = {"message_id": found.id, "delivered_to": email}

            inbound_email = await client.inbound.from_event(_Envelope())
            assert inbound_email.id == found.id
            assert inbound_email.subject == subject
            mark_covered("inbound.from_event")

            fetched = await client.webhooks.fetch_message(_Envelope())
            assert fetched.id == found.id
            mark_covered("webhooks.fetch_message")
        finally:
            await client.agents.delete(email)


# ── reviews: the account-level human-review queue ───────────────────


async def test_reviews_list_get_approve_reject():
    async with _client() as client:
        approve_email = _bot("rev-a")
        reject_email = _bot("rev-r")
        await client.agents.create({"email": approve_email, "name": "cov review approve"})
        await client.agents.create({"email": reject_email, "name": "cov review reject"})
        try:
            for email in (approve_email, reject_email):
                await _hold_all_outbound(client, email)

            approve_subject = f"py-sdk-cov-rev-approve {int(time.time() * 1000)}"
            reject_subject = f"py-sdk-cov-rev-reject {int(time.time() * 1000)}"
            approve_send = await client.messages.send(
                approve_email, {"to": [SINK_EMAIL], "subject": approve_subject, "text": "held for approve"}
            )
            reject_send = await client.messages.send(
                reject_email, {"to": [SINK_EMAIL], "subject": reject_subject, "text": "held for reject"}
            )
            assert approve_send.status == "pending_review"
            assert reject_send.status == "pending_review"

            reviews = await client.reviews.list(limit=100).to_list(limit=100)
            ids = {r.id for r in reviews}
            assert approve_send.message_id in ids
            assert reject_send.message_id in ids
            mark_covered("reviews.list")

            got = await client.reviews.get(approve_send.message_id)
            assert got.id == approve_send.message_id
            mark_covered("reviews.get")

            approved = await client.reviews.approve(approve_send.message_id)
            assert approved.message_id == approve_send.message_id
            assert approved.status in ("sent", "accepted")
            mark_covered("reviews.approve")

            rejected = await client.reviews.reject(reject_send.message_id, {"reason": "py-sdk coverage gate reject"})
            assert rejected.message_id == reject_send.message_id
            assert rejected.status == "review_rejected"
            mark_covered("reviews.reject")
        finally:
            await client.agents.delete(approve_email)
            await client.agents.delete(reject_email)


# ── templates: CRUD + validate + the starter catalog ────────────────


async def test_templates_full_crud_and_starters():
    async with _client() as client:
        starters = await client.templates.list_starters(limit=10).to_list(limit=10)
        assert starters, "expected at least one starter template in the deployment's catalog"
        mark_covered("templates.list_starters")

        starter_detail = await client.templates.get_starter(starters[0].alias)
        assert starter_detail.alias == starters[0].alias
        assert starter_detail.subject
        mark_covered("templates.get_starter")

        name = f"py-sdk-cov-tmpl-{int(time.time() * 1000):x}"
        created = await client.templates.create({"name": name, "subject": "Hi {{name}}", "text": "Hello {{name}}"})
        assert created.name == name
        assert created.id.startswith("tmpl_")
        mark_covered("templates.create")
        try:
            got = await client.templates.get(created.id)
            assert got.id == created.id
            assert got.subject == "Hi {{name}}"
            mark_covered("templates.get")

            listed = await client.templates.list(limit=100).to_list(limit=100)
            assert any(t.id == created.id for t in listed)
            mark_covered("templates.list")

            validated = await client.templates.validate(
                {"subject": "Hi {{name}}", "text": "Hello {{name}}", "test_data": {"name": "Ada"}}
            )
            assert validated.valid is True
            assert validated.rendered is not None
            assert validated.rendered.subject == "Hi Ada"
            mark_covered("templates.validate")

            updated = await client.templates.update(created.id, {"subject": "Hi again {{name}}"})
            assert updated.subject == "Hi again {{name}}"
            mark_covered("templates.update")
        finally:
            deleted = await client.templates.delete(created.id)
            assert deleted.deleted is True
            assert deleted.id == created.id
            mark_covered("templates.delete")


# ── domains: register/get/list/verify/delete ─────────────────────────


async def test_domains_register_get_list_verify_delete():
    async with _client() as client:
        # .example.com is RFC 2606-reserved — safe to register-then-never-
        # verify without colliding with real DNS (same convention as
        # tests/e2e-prod/suites/10-domains.test.ts's fakeDomain()).
        domain = f"py-sdk-cov-{int(time.time() * 1000):x}.example.com"
        created = await client.domains.create({"domain": domain})
        assert created.domain == domain
        assert created.verified is False
        mark_covered("domains.create")
        try:
            got = await client.domains.get(domain)
            assert got.domain == domain
            mark_covered("domains.get")

            listed = await client.domains.list(limit=100).to_list(limit=100)
            assert any(d.domain == domain for d in listed)
            mark_covered("domains.list")

            verified = await client.domains.verify(domain)
            assert verified.domain == domain
            # No real DNS was ever published for this throwaway domain, so the
            # honest assertion is "still unverified" — asserting True here
            # would be the exact "assert on a result you didn't actually
            # produce" anti-pattern the task calls out.
            assert verified.verified is False
            mark_covered("domains.verify")
        finally:
            deleted = await client.domains.delete(domain)
            assert deleted.deleted is True
            assert deleted.domain == domain
            mark_covered("domains.delete")


# ── webhooks (full CRUD + test + deliveries) and events ──────────────


async def test_webhooks_and_events_full_surface():
    async with _client() as client:
        email = _bot("evt")
        await client.agents.create({"email": email, "name": "cov events"})
        hook = await client.webhooks.create(
            {
                "url": "https://example.com/py-sdk-coverage-webhook",
                "events": ["email.sent"],
                "description": "py-sdk resource coverage gate",
            }
        )
        assert hook.id.startswith("wh_")
        assert hook.signing_secret.startswith("whsec_")
        mark_covered("webhooks.create")
        try:
            got = await client.webhooks.get(hook.id)
            assert got.id == hook.id
            mark_covered("webhooks.get")

            listed = await client.webhooks.list(limit=100).to_list(limit=100)
            assert any(w.id == hook.id for w in listed)
            mark_covered("webhooks.list")

            updated = await client.webhooks.update(hook.id, {"description": "patched by the coverage gate"})
            assert updated.description == "patched by the coverage gate"
            mark_covered("webhooks.update")

            rotated = await client.webhooks.rotate_secret(hook.id)
            assert rotated.signing_secret.startswith("whsec_")
            assert rotated.signing_secret != hook.signing_secret
            mark_covered("webhooks.rotate_secret")

            tested = await client.webhooks.test(hook.id, {"type": "email.sent", "data": {}})
            assert tested.delivery_id
            mark_covered("webhooks.test")

            # webhooks.deliveries: the test() call above enqueues a real
            # delivery attempt — give it a moment to land, then assert it's
            # actually visible (not just that the call didn't raise).
            deliveries = []
            for _ in range(10):
                deliveries = await client.webhooks.deliveries(hook.id, limit=20).to_list(limit=20)
                if deliveries:
                    break
                await asyncio.sleep(1.0)
            assert deliveries, "expected at least one delivery to be visible after webhooks.test()"
            assert any(d.id == tested.delivery_id for d in deliveries)
            mark_covered("webhooks.deliveries")

            # events.list/get/redeliver need a REAL event (not the synthetic
            # payload webhooks.test() sent) — trigger one with a real send.
            subject = f"py-sdk-cov-evt {int(time.time() * 1000)}"
            sent = await client.messages.send(email, {"to": [SINK_EMAIL], "subject": subject, "text": "for events coverage"})

            event = None
            for _ in range(20):
                events = await client.events.list(type="email.sent", agent_email=email, limit=20).to_list(limit=20)
                event = next((e for e in events if e.message_id == sent.message_id), None)
                if event:
                    break
                await asyncio.sleep(1.5)
            assert event is not None, "expected an email.sent event for the just-sent message within ~30s"
            assert event.message_id == sent.message_id
            mark_covered("events.list")

            fetched_event = await client.events.get(event.id)
            assert fetched_event.id == event.id
            mark_covered("events.get")

            redelivered = await client.events.redeliver(event.id, {"webhook_id": hook.id})
            assert redelivered.event_id == event.id
            assert redelivered.status
            mark_covered("events.redeliver")
        finally:
            deleted_hook = await client.webhooks.delete(hook.id)
            assert deleted_hook.deleted is True
            mark_covered("webhooks.delete")
            await client.agents.delete(email)
