"""Tests for the synchronous E2AClient — the facade over AsyncE2AClient.

The sync client bridges every call onto a background event-loop thread
(``sync_client.py``), so these tests exercise the full stack through httpx
mocks exactly like the async suite does: sync facade -> bridge ->
AsyncE2AClient resource -> generated *Api -> retry -> httpx -> typed errors.

The parity tests are the drift guard: they reflect over AsyncE2AClient's
resource tree and assert every public method/namespace is reachable on the
sync facade, so a new async resource or method can't silently be missing from
the sync surface.
"""

import asyncio
import inspect
import json
import sys
import threading
import time
from unittest.mock import MagicMock, patch

import pytest

from e2a.v1 import E2AClient
from e2a.v1._retry import RetryConfig
from e2a.v1.client import AsyncE2AClient
from e2a.v1.errors import E2AError, E2ALimitExceededError, E2ANotFoundError
from e2a.v1.pagination import Page
from e2a.v1.sync_client import SyncAutoPager, SyncStream
from e2a.v1.inbound import InboundEmail
from e2a.v1.generated.models import (
    AgentView,
    PageMessageLifecycleTransition,
    TemplateSummaryView,
)

from .test_v1_client import _valid  # the real-model wire-JSON fixture builder
from .test_v1_inbound import event_from_wire, load

BASE = "http://test.local"


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for v in ("E2A_API_KEY", "E2A_API_URL", "E2A_BASE_URL", "E2A_AGENT_EMAIL"):
        monkeypatch.delenv(v, raising=False)


def _client(**kwargs):
    # No-sleep retry config so any retry path is instant.
    async def no_sleep(_s):
        return None

    kwargs.setdefault("_retry_config", RetryConfig(sleep=no_sleep))
    return E2AClient(api_key="e2a_test", base_url=BASE, **kwargs)


# ── construction + parity ───────────────────────────────────────────


def test_requires_api_key_eagerly():
    # Constructor validation happens at construction, same as the async client
    # (the async client is built eagerly; only the loop thread is lazy).
    with pytest.raises(E2AError, match="api_key is required"):
        E2AClient(base_url=BASE)


def test_constructor_signature_matches_async():
    """The sync constructor must mirror AsyncE2AClient.__init__ exactly —
    this is the drift guard for new constructor options."""
    sync_sig = inspect.signature(E2AClient.__init__)
    async_sig = inspect.signature(AsyncE2AClient.__init__)
    assert list(sync_sig.parameters) == list(async_sig.parameters)
    for name, p in async_sig.parameters.items():
        sp = sync_sig.parameters[name]
        assert sp.kind == p.kind, name
        assert sp.default == p.default, name


def test_no_loop_thread_until_first_call():
    c = _client()
    assert c._bridge._thread is None  # lazy: construction costs no thread
    c.close()


def _walk_parity(async_obj, sync_obj, path=""):
    """Recursively assert every public async attribute is mirrored."""
    seen = []
    for name in dir(async_obj):
        if name.startswith("_") or name == "aclose":
            continue
        a = getattr(async_obj, name)
        s = getattr(sync_obj, name)  # must not raise
        full = f"{path}{name}"
        seen.append(full)
        if inspect.iscoroutinefunction(a):
            assert callable(s), full
            assert not inspect.iscoroutinefunction(s), f"{full} leaked a coroutine fn"
        elif callable(a):
            assert callable(s), full
        else:
            # A nested resource namespace — recurse into it.
            seen.extend(_walk_parity(a, s, path=f"{full}."))
    return seen


def test_parity_every_async_method_reachable_sync():
    c = _client()
    try:
        seen = set(_walk_parity(c._async_client, c))
        # Sanity: the walk actually covered the resource tree, including
        # nested namespaces and both method shapes (async + pager-returning).
        expected = {
            "agents", "messages", "conversations", "domains", "events",
            "webhooks", "inbound", "account", "reviews", "scheduled", "templates", "info", "listen",
            "agents.get", "agents.list", "agents.create", "agents.delete", "agents.restore",
            "messages.delete", "messages.restore",
            "messages.get_lifecycle",
            "messages.send", "messages.reply", "webhooks.fetch_message",
            "inbound.from_event",
            "account.suppressions", "account.suppressions.list",
            "account.api_keys", "account.api_keys.create",
            "templates.list_starters", "reviews.approve", "scheduled.list",
        }
        missing = expected - seen
        assert not missing, f"parity walk did not cover: {sorted(missing)}"
    finally:
        c.close()


def test_dir_lists_mirrored_surface():
    c = _client()
    try:
        top = dir(c)
        for name in ("agents", "messages", "inbound", "templates", "info", "listen", "close"):
            assert name in top
        assert "aclose" not in top
        assert "list" in dir(c.agents) and "get" in dir(c.agents)
        assert "suppressions" in dir(c.account)
    finally:
        c.close()


def test_aclose_not_exposed_points_to_close():
    c = _client()
    try:
        with pytest.raises(AttributeError, match="use close\\(\\)"):
            c.aclose
    finally:
        c.close()


def test_inbound_facade_is_blocking_and_matches_shared_vector(httpx_mock):
    vector = load("full.json")
    httpx_mock.add_response(json=vector["message"])
    httpx_mock.add_response(json={"message_id": "msg_reply", "status": "pending_review"})
    httpx_mock.add_response(
        json={
            "index": 0,
            "size_bytes": 12345,
            "download_url": "https://download.example/invoice",
            "expires_at": "2026-07-01T11:00:00Z",
        }
    )

    with _client() as c:
        email = c.inbound.from_event(event_from_wire(vector["event"]))
        assert isinstance(email, InboundEmail)
        assert not inspect.iscoroutinefunction(email.reply)
        assert email.to_dict() == vector["expected"]
        result = email.reply({"text": "Got it"}, idempotency_key="reply:evt")
        attachment = email.attachments[0].get(inline=True)

    assert result.message_id == "msg_reply"
    assert result.status == "pending_review"
    assert attachment.index == 0


# ── CRUD round-trips over httpx mocks ───────────────────────────────


def test_get_agent_sends_bearer_and_encodes_address(httpx_mock):
    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    with _client() as c:
        agent = c.agents.get("bot@test.dev")
    assert isinstance(agent, AgentView)
    assert agent.email == "bot@test.dev"
    req = httpx_mock.get_requests()[-1]
    assert req.headers["Authorization"] == "Bearer e2a_test"
    assert "/v1/agents/bot%40test.dev" in str(req.url)


def test_agents_list_sync_iteration_threads_cursor(httpx_mock):
    # `for agent in client.agents.list()` walks the cursor exactly like the
    # async pager (same implementation, bridged per item).
    httpx_mock.add_response(
        json={"items": [_valid(AgentView, id="ag_1", email="a@test.dev")], "next_cursor": "cur_2"}
    )
    httpx_mock.add_response(
        json={"items": [_valid(AgentView, id="ag_2", email="b@test.dev")], "next_cursor": None}
    )
    with _client() as c:
        emails = [a.email for a in c.agents.list()]
    assert emails == ["a@test.dev", "b@test.dev"]
    reqs = httpx_mock.get_requests()
    assert len(reqs) == 2
    assert "cursor=cur_2" in str(reqs[1].url)


def test_send_mints_idempotency_key(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_1", "status": "sent"})
    with _client() as c:
        result = c.messages.send("bot@test.dev", {"to": ["a@x.com"], "subject": "Hi", "text": "yo"})
    assert result.message_id == "msg_1"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "POST"
    assert "/v1/agents/bot%40test.dev/messages" in str(req.url)
    assert req.headers.get("Idempotency-Key")


def test_send_uses_caller_idempotency_key(httpx_mock):
    httpx_mock.add_response(json={"message_id": "msg_2", "status": "sent"})
    with _client() as c:
        c.messages.send(
            "bot@test.dev",
            {"to": ["a@x.com"], "subject": "Hi", "text": "yo"},
            idempotency_key="caller-key-123",
        )
    assert httpx_mock.get_requests()[-1].headers["Idempotency-Key"] == "caller-key-123"


def test_messages_delete_blocks_and_returns_receipt(httpx_mock):
    # The sync facade mirrors the async coroutine: it blocks and hands back the
    # typed receipt, and the keyword-only `permanent` flag survives the bridge.
    httpx_mock.add_response(json={"deleted": True, "id": "msg_1"})
    with _client() as c:
        res = c.messages.delete("bot@test.dev", "msg_1", permanent=True)
    assert res.deleted is True
    assert res.id == "msg_1"
    req = httpx_mock.get_requests()[-1]
    assert req.method == "DELETE"
    assert "/v1/agents/bot%40test.dev/messages/msg_1" in str(req.url)
    assert req.url.params["permanent"] == "true"
    assert "confirm=DELETE" in str(req.url)


def test_messages_get_lifecycle_sync_facade_forwards_identical_arguments(httpx_mock):
    httpx_mock.add_response(
        json={
            "items": [
                {
                    "id": "mlt_recon_1",
                    "message_id": "msg_1",
                    "direction": "inbound",
                    "stage": "accepted",
                    "outcome": "accepted",
                    "reason_code": "acceptance.inbound_smtp",
                    "retryable": False,
                    "evidence": {"source": "message"},
                    "correlation_ids": {},
                    "occurred_at": "2026-07-22T00:00:00Z",
                    "reconstructed": True,
                }
            ],
            "next_cursor": None,
        }
    )

    with _client() as c:
        page = c.messages.get_lifecycle(
            "bot@test.dev", "msg_1", cursor="cur_1", limit=1
        )

    assert isinstance(page, PageMessageLifecycleTransition)
    assert page.items[0].reconstructed is True
    request = httpx_mock.get_requests()[-1]
    assert "/v1/agents/bot%40test.dev/messages/msg_1/lifecycle" in str(request.url)
    assert request.url.params["cursor"] == "cur_1"
    assert request.url.params["limit"] == "1"


def test_templates_list_multi_page_walk_and_page_resume(httpx_mock):
    p1 = {"items": [_valid(TemplateSummaryView, id="tmpl_1", name="t1")], "next_cursor": "cur_2"}
    p2 = {"items": [_valid(TemplateSummaryView, id="tmpl_2", name="t2")], "next_cursor": None}
    # Full sync-iteration walk…
    httpx_mock.add_response(json=p1)
    httpx_mock.add_response(json=p2)
    # …then a manual page()/resume walk.
    httpx_mock.add_response(json=p1)
    httpx_mock.add_response(json=p2)
    with _client() as c:
        ids = [t.id for t in c.templates.list()]
        assert ids == ["tmpl_1", "tmpl_2"]

        pager = c.templates.list()
        page1 = pager.page()
        assert isinstance(page1, Page)
        assert [t.id for t in page1.items] == ["tmpl_1"]
        assert page1.next_cursor == "cur_2"
        page2 = pager.page(page1.next_cursor)  # checkpoint/resume
        assert [t.id for t in page2.items] == ["tmpl_2"]
        assert page2.next_cursor is None
    reqs = httpx_mock.get_requests()
    assert "cursor=cur_2" in str(reqs[1].url)
    assert "cursor" not in str(reqs[2].url)
    assert "cursor=cur_2" in str(reqs[3].url)


def test_pager_to_list_and_for_each(httpx_mock):
    p1 = {"items": [_valid(AgentView, id="ag_1", email="a@test.dev")], "next_cursor": "c2"}
    p2 = {"items": [_valid(AgentView, id="ag_2", email="b@test.dev")], "next_cursor": None}
    for p in (p1, p2, p1):  # to_list walks both pages; for_each stops on page 1
        httpx_mock.add_response(json=p)
    with _client() as c:
        got = c.agents.list().to_list(limit=10)
        assert [a.email for a in got] == ["a@test.dev", "b@test.dev"]

        collected = []

        def take_one(agent):
            collected.append(agent.email)
            return False  # stop early — page 2 must never be fetched

        c.agents.list().for_each(take_one)
        assert collected == ["a@test.dev"]
    assert len(httpx_mock.get_requests()) == 3


def test_pager_early_break_closes_async_generator(httpx_mock):
    httpx_mock.add_response(
        json={"items": [_valid(AgentView, id="ag_1", email="a@test.dev")], "next_cursor": "c2"}
    )
    with _client() as c:
        for a in c.agents.list():
            break  # abandon mid-page: must not hang or leak, no 2nd request
    assert len(httpx_mock.get_requests()) == 1


# ── typed error transparency ────────────────────────────────────────


def test_limit_exceeded_propagates_typed(httpx_mock):
    # The 402 quota error must arrive as E2ALimitExceededError itself — not a
    # concurrent.futures wrapper — so sync except-clauses match identically.
    httpx_mock.add_response(
        status_code=402,
        json={"error": {"code": "limit_exceeded", "message": "quota exhausted"}},
    )
    with _client() as c:
        with pytest.raises(E2ALimitExceededError) as excinfo:
            c.agents.get("bot@test.dev")
    err = excinfo.value
    assert err.code == "limit_exceeded"
    assert err.status == 402
    assert err.retryable is False
    assert "quota exhausted" in str(err)


def test_not_found_propagates_typed(httpx_mock):
    httpx_mock.add_response(
        status_code=404,
        json={"error": {"code": "message_not_found", "message": "nope"}},
    )
    with _client() as c:
        with pytest.raises(E2ANotFoundError):
            c.messages.get("bot@test.dev", "msg_missing")


# ── the async-context guard ─────────────────────────────────────────


def test_sync_call_inside_running_loop_raises():
    c = _client()
    try:
        async def main():
            with pytest.raises(RuntimeError, match="use AsyncE2AClient"):
                c.agents.get("bot@test.dev")
            # Iterating a pager from async code must trip the same guard.
            pager = c.agents.list()
            with pytest.raises(RuntimeError, match="use AsyncE2AClient"):
                next(iter(pager))

        asyncio.run(main())
    finally:
        c.close()


def test_guard_does_not_fire_outside_loop(httpx_mock):
    # Same call is fine in plain sync code.
    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    with _client() as c:
        assert c.agents.get("bot@test.dev").email == "bot@test.dev"


# ── lifecycle ───────────────────────────────────────────────────────


def test_close_is_idempotent_and_stops_loop_thread(httpx_mock):
    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    c = _client()
    c.agents.get("bot@test.dev")  # starts the loop thread
    thread = c._bridge._thread
    assert thread is not None and thread.is_alive()
    c.close()
    assert not thread.is_alive()
    c.close()  # double-close is a no-op
    c.close()


def test_context_manager_closes():
    with _client() as c:
        assert c._bridge.closed is False
    assert c._bridge.closed is True


def test_use_after_close_raises_cleanly():
    c = _client()
    c.close()
    with pytest.raises(E2AError, match="closed") as excinfo:
        c.agents.get("bot@test.dev")
    assert excinfo.value.code == "client_closed"


def test_close_without_ever_starting_loop():
    c = _client()
    c.close()  # loop thread never started — must not hang or raise
    assert c._bridge._thread is None


# ── WS bridge (listen) ──────────────────────────────────────────────


class _FakeWS:
    """Minimal websockets-connection stand-in (mirrors test_v1_websocket)."""

    def __init__(self, messages):
        self._messages = list(messages)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        pass

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self._messages:
            raise StopAsyncIteration
        return self._messages.pop(0)


def test_listen_returns_sync_stream_and_yields_events():
    payload = json.dumps({
        "type": "email.received",
        "id": "evt_abc",
        "schema_version": "1",
        "created_at": "2026-04-27T10:00:00Z",
        "data": {
            "message_id": "msg_123",
            "from": "alice@example.com",
            "delivered_to": "bot@agents.e2a.dev",
            "subject": "Hi",
            "received_at": "2026-04-27T10:00:00Z",
        },
    })
    mock_module = MagicMock()
    mock_module.connect = MagicMock(return_value=_FakeWS([payload]))
    with patch.dict(sys.modules, {"websockets": mock_module}):
        with _client() as c:
            stream = c.listen("bot@agents.e2a.dev")
            assert isinstance(stream, SyncStream)
            got = []
            for notif in stream:
                got.append(notif)
                break  # the real stream reconnects forever; one item is the test
    assert got[0].type == "email.received"
    assert got[0].id == "evt_abc"
    assert got[0].data["message_id"] == "msg_123"
    assert got[0].data["from"] == "alice@example.com"


def test_listen_stream_ends_cleanly_when_source_ends():
    # A bounded async source ends the sync iteration with StopIteration.
    async def bounded():
        from e2a.v1.websocket import WSEvent

        for i in range(2):
            yield WSEvent.from_payload({
                "type": "email.received",
                "id": f"evt_{i}",
                "schema_version": "1",
                "created_at": "2026-04-27T10:00:00Z",
                "data": {"message_id": f"msg_{i}", "delivered_to": "bot@x.dev"},
            })

    c = _client()
    try:
        c._async_client.listen = lambda email: bounded()  # instance-attr shadow
        ids = [e.data["message_id"] for e in c.listen("bot@x.dev")]
        assert ids == ["msg_0", "msg_1"]
    finally:
        c.close()


def test_close_unblocks_pending_listen_iteration():
    """close() from another thread must wake a blocked `for notif in listen()`
    and end it cleanly (no exception, no hang)."""

    async def forever():
        await asyncio.Event().wait()  # blocks until cancelled
        yield None  # pragma: no cover - never reached (makes this an async gen)

    c = _client()
    c._async_client.listen = lambda email: forever()

    result = {}

    def consume():
        try:
            for _ in c.listen("bot@x.dev"):
                pass  # pragma: no cover - nothing is ever yielded
            result["outcome"] = "clean-stop"
        except BaseException as e:  # noqa: BLE001
            result["outcome"] = repr(e)

    t = threading.Thread(target=consume, daemon=True)
    t.start()
    # Wait until the consumer is actually blocked on the bridge.
    deadline = time.time() + 5
    while c._bridge._thread is None and time.time() < deadline:
        time.sleep(0.01)
    time.sleep(0.1)

    c.close()
    t.join(timeout=5)
    assert not t.is_alive(), "close() failed to unblock the pending iteration"
    assert result["outcome"] == "clean-stop"


# ── bridge internals ────────────────────────────────────────────────


def test_pager_is_sync_autopager_type(httpx_mock):
    with _client() as c:
        pager = c.agents.list()
        assert isinstance(pager, SyncAutoPager)
        # Sync pagers are not async-iterable; async pagers are not sync-iterable.
        assert not hasattr(pager, "__aiter__")


def test_resource_proxy_caches_wrappers():
    with _client() as c:
        assert c.agents is c.agents
        assert c.agents.get is c.agents.get
        assert c.account.suppressions is c.account.suppressions


def test_finalizer_cleans_up_unclosed_client(httpx_mock):
    # An unclosed, dropped client must not leave the loop thread running:
    # the weakref.finalize fallback stops it at GC (and would at exit).
    import gc

    httpx_mock.add_response(json=_valid(AgentView, id="ag_1", email="bot@test.dev"))
    c = _client()
    c.agents.get("bot@test.dev")
    bridge = c._bridge
    thread = bridge._thread
    assert thread.is_alive()
    del c
    gc.collect()
    thread.join(timeout=5)
    assert not thread.is_alive()
    assert bridge.closed
