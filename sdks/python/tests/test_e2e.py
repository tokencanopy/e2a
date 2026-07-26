"""Live ergonomic e2e for the Python SDK against a RUNNING server (staging).

Exercises the real hand-written ergonomic surface (``client.messages.*`` /
``client.agents.*`` / ``client.info``), so a green run attests the published
Python SDK actually works against a live deployment — the parity signal the
raw-HTTP contract runner (``test_contract.py``) can't give.

Gated on staging creds; skips cleanly when absent, so it stays inert in the
default test run. Env is aligned with the contract runner + the TS live test
(``E2A_TEST_*`` naming). The agent email is required because Python's message
methods take the inbox as their first positional argument:

    E2A_TEST_BASE_URL     e.g. https://api-staging.e2a.dev (or a local tunnel)
    E2A_TEST_API_KEY      an API key for the target account
    E2A_TEST_AGENT_EMAIL  a shared-domain inbox on that account (self-send target)

Run:
    E2A_TEST_BASE_URL=… E2A_TEST_API_KEY=… E2A_TEST_AGENT_EMAIL=… \\
        uv run pytest tests/test_e2e.py
"""

import asyncio
import os
import time

import pytest

from e2a import AsyncE2AClient, E2ANotFoundError
from e2a.v1 import ReplyRequest, SendEmailRequest

from .resource_coverage_lib.tracker import mark_covered

BASE_URL = os.environ.get("E2A_TEST_BASE_URL", "")
API_KEY = os.environ.get("E2A_TEST_API_KEY", "")
AGENT = os.environ.get("E2A_TEST_AGENT_EMAIL", "")

pytestmark = [
    pytest.mark.anyio,
    pytest.mark.skipif(
        not BASE_URL or not API_KEY or not AGENT,
        reason="E2A_TEST_BASE_URL, E2A_TEST_API_KEY, E2A_TEST_AGENT_EMAIL required for live e2e",
    ),
]


@pytest.fixture
def anyio_backend() -> str:
    # Pin the asyncio backend so these don't also try to run under trio.
    return "asyncio"


async def test_info_reports_deployment() -> None:
    async with AsyncE2AClient(api_key=API_KEY, base_url=BASE_URL) as client:
        info = await client.info()
        assert isinstance(info.version, str) and info.version
        mark_covered("info")


async def test_send_find_get_reply_self_loopback() -> None:
    # A FRESH shared-domain agent (no protection) so the self-send delivers
    # immediately and loops back — the seeded conformance inbox may hold outbound
    # for review, which would never land in the inbox.
    domain = AGENT.split("@", 1)[1]
    bot = f"py-sdk-live-{int(time.time() * 1000):x}@{domain}"
    async with AsyncE2AClient(api_key=API_KEY, base_url=BASE_URL) as client:
        created = await client.agents.create({"email": bot, "name": "py-sdk live e2e"})
        assert created.email == bot
        mark_covered("agents.create")
        try:
            subject = f"py-sdk-live {int(time.time() * 1000)}"
            sent = await client.messages.send(
                bot,
                SendEmailRequest(
                    to=[bot],
                    subject=subject,
                    text="Hello from the Python SDK live e2e",
                ),
            )
            assert sent.message_id
            assert sent.status in ("sent", "accepted")
            mark_covered("messages.send")

            # Self-send loopback lands an INBOUND copy; filter to inbound so the
            # just-sent outbound copy (same subject) can't match.
            found = None
            for _ in range(12):
                msgs = await client.messages.list(bot, direction="inbound", limit=20).to_list(limit=20)
                found = next((m for m in msgs if m.subject == subject), None)
                if found:
                    break
                await asyncio.sleep(1.5)
            assert found is not None, f"an inbound message with subject {subject!r} must appear within ~18s"
            mark_covered("messages.list")

            full = await client.messages.get(bot, found.id)
            assert full.id == found.id
            assert full.subject == subject
            # The delivered body is under `parsed` (inbound-extracted MIME), not
            # `body` (the held-outbound draft field, null for inbound by design).
            assert full.parsed is not None and "Hello from the Python SDK live e2e" in full.parsed.text
            mark_covered("messages.get")

            reply = await client.messages.reply(
                bot,
                found.id,
                ReplyRequest(text="Reply from the Python SDK live e2e"),
            )
            assert reply.message_id
            # Fresh unprotected inbox → the reply sends immediately (same as send).
            assert reply.status in ("sent", "accepted")
            mark_covered("messages.reply")
        finally:
            deleted = await client.agents.delete(bot)
            assert deleted.deleted is True
            mark_covered("agents.delete")


async def test_list_bounded_page() -> None:
    async with AsyncE2AClient(api_key=API_KEY, base_url=BASE_URL) as client:
        msgs = await client.messages.list(AGENT, limit=2).to_list(limit=2)
        assert len(msgs) <= 2
        for m in msgs:
            assert m.id
            assert m.delivered_to


async def test_get_nonexistent_raises_not_found() -> None:
    async with AsyncE2AClient(api_key=API_KEY, base_url=BASE_URL) as client:
        with pytest.raises(E2ANotFoundError):
            await client.messages.get(AGENT, f"msg_nonexistent_{int(time.time())}")
