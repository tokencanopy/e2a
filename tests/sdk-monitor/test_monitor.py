"""Offline checks for the multi-interface conformance monitor. No pytest, no
network, no real subprocess to npm/node:

    E2A_API_KEY=x E2A_MONITOR_AGENT_A=a@x E2A_MONITOR_AGENT_B=b@x \
    E2A_MONITOR_WEBHOOK_SECRET=whsec_test python test_monitor.py

Covers the parts that must not regress silently: signature accept/reject,
fail-closed on an unset secret, non-inbound event types, the nonce <-> iface
encoding round trip (including the HMAC tag), nonce-MAC authentication (a
tampered iface/timestamp/tag is rejected, and a pre-MAC 3-part nonce doesn't
even match), the stale-reply guard (both legs), the interface-strategy
dispatch, the unknown-iface safety net, the /tick aggregate + status-code
semantics (all-ok / partial / total-outage / an optional interface skipped),
uniform non-success send-status handling across every offline-exercisable
interface, the cleanup (message-deletion) behavior on both legs (trash via
the nonce's own interface AFTER the leg's success marker, a cleanup failure
never revoking monitor_ok/monitor_replied, and a delete-less interface being
a documented monitor_skip rather than an error), the wire shape of the
api/python_sdk/ts_sdk/cli/mcp strategies for send/reply AND delete
(argv/URL/headers/tool-arguments, receipt validation, and that secrets never
land in argv), the minimal subprocess environment, and McpStrategy's
JSON-RPC response parsing (SSE framing, a JSON-RPC error, a tool-level
isError, a leading notification/mismatched-id frame that must be skipped
rather than mistaken for our response, and the malformed-response guard).
Every interface's actual send/reply/delete I/O is stubbed with fakes
injected into monitor.STRATEGIES, or with urllib.request.urlopen /
subprocess.run monkeypatched to capture/replay a call — no real network
call, no real subprocess to node/npm.
"""

import calendar
import hashlib
import hmac
import io
import json
import os
import sys
import time

os.environ.setdefault("E2A_API_KEY", "e2a_acct_test")
os.environ.setdefault("E2A_BASE_URL", "https://api.e2a.dev")
os.environ.setdefault("E2A_MONITOR_AGENT_A", "mon-a@agents.e2a.dev")
os.environ.setdefault("E2A_MONITOR_AGENT_B", "mon-b@agents.e2a.dev")
os.environ.setdefault("E2A_MONITOR_WEBHOOK_SECRET", "whsec_testsecret")

import monitor  # noqa: E402

SECRET = monitor.WEBHOOK_SECRET
failures = []


def check(name, got, want):
    if got != want:
        failures.append(f"{name}: got {got!r}, want {want!r}")
    print(("PASS " if got == want else "FAIL ") + name)


def sign(body: bytes, secret: str = SECRET, ts: int | None = None) -> str:
    ts = ts if ts is not None else int(time.time())
    mac = hmac.new(secret.encode(), f"{ts}".encode() + b"." + body, hashlib.sha256)
    return f"t={ts},v1={mac.hexdigest()}"


def envelope(event_type: str, data: dict) -> bytes:
    return json.dumps(
        {
            "id": "evt_test_1",
            "type": event_type,
            "schema_version": "1",
            "created_at": "2026-07-21T00:00:00Z",
            "data": data,
        }
    ).encode()


def make_nonce(iface: str, epoch_ms: int, rand_hex: str = "0123456789abcdef") -> str:
    """Build a correctly-tagged nonce for an arbitrary timestamp — the tests
    need to control staleness/freshness directly, so this mirrors
    monitor.new_nonce() but takes epoch_ms as an argument instead of reading
    the clock."""
    tag = monitor._nonce_tag(iface, epoch_ms, rand_hex)
    return f"e2asdkmon.{iface}.{epoch_ms}.{rand_hex}.{tag}"


def header(headers: dict, name: str):
    """Case-insensitive header lookup — urllib.request.Request.add_header
    stores keys via str.capitalize() (e.g. "Idempotency-Key" ->
    "Idempotency-key"), not as-passed."""
    for k, v in headers.items():
        if k.lower() == name.lower():
            return v
    return None


# ---------------------------------------------------------------------------
# Nonce <-> iface encoding round trip, including the HMAC tag (FIX 1).
# ---------------------------------------------------------------------------

for _iface in monitor.IFACES:
    _nonce = monitor.new_nonce(_iface)
    _match = monitor.NONCE_RE.search(f"probe {_nonce}")
    check(f"nonce round-trips iface={_iface}", _match is not None, True)
    if _match:
        check(f"nonce iface group matches ({_iface})", _match.group(1), _iface)
        check(f"nonce timestamp group is numeric ({_iface})", _match.group(2).isdigit(), True)
        check(f"nonce full match is the nonce itself ({_iface})", _match.group(0), _nonce)
        _expected_tag = monitor._nonce_tag(_iface, int(_match.group(2)), _match.group(3))
        check(f"nonce tag matches HMAC({_iface})", _match.group(4), _expected_tag)

# A nonce naming an iface this build doesn't recognize still matches the
# generic regex (handled explicitly downstream — see the unknown-iface test
# below) rather than silently failing to parse. Shape only here — no mac
# correctness claim (that's tested against handle_webhook further down).
_unknown_nonce = f"e2asdkmon.some_future_iface.{int(time.time() * 1000)}.0123456789abcdef.abcdef0123456789"
_unknown_match = monitor.NONCE_RE.search(f"probe {_unknown_nonce}")
check("unrecognized-but-shaped iface still parses", _unknown_match is not None, True)
check("unrecognized iface group captured", _unknown_match.group(1) if _unknown_match else None, "some_future_iface")

# Old-style pre-MAC nonce (3 parts, no tag segment) simply does not match the
# new 4-group NONCE_RE at all.
_old_style_nonce = f"e2asdkmon.python_sdk.{int(time.time() * 1000)}.0123456789abcdef"
check("old-style 3-part nonce does not match the new regex", monitor.NONCE_RE.search(f"probe {_old_style_nonce}"), None)


# ---------------------------------------------------------------------------
# Webhook signature verification: fail-closed, replay, non-inbound ignore.
# ---------------------------------------------------------------------------

body = envelope("email.sent", {"message_id": "msg_1"})

status, _ = monitor.handle_webhook(body, sign(body))
check("valid signature accepted (non-inbound ignored)", status, 200)

status, _ = monitor.handle_webhook(body, sign(body, secret="whsec_wrong"))
check("bad signature rejected", status, 401)

status, _ = monitor.handle_webhook(body, "")
check("missing signature header rejected", status, 401)

status, _ = monitor.handle_webhook(body, sign(body, ts=int(time.time()) - 3600))
check("replayed timestamp rejected", status, 401)

saved, monitor.WEBHOOK_SECRET = monitor.WEBHOOK_SECRET, ""
status, _ = monitor.handle_webhook(body, sign(body, secret=saved))
check("unset secret fails closed", status, 401)
monitor.WEBHOOK_SECRET = saved


# ---------------------------------------------------------------------------
# Fakes for the interface-strategy dispatch + stale-guard + success-path
# tests. Every strategy's I/O is stubbed — no network, no subprocess.
# ---------------------------------------------------------------------------


class FakeStrategy:
    def __init__(self):
        self.send_calls = []
        self.reply_calls = []
        self.delete_calls = []

    def available(self):
        return True

    def send(self, **kwargs):
        self.send_calls.append(kwargs)

    def reply(self, **kwargs):
        self.reply_calls.append(kwargs)

    def supports_delete(self):
        return True

    def delete(self, **kwargs):
        self.delete_calls.append(kwargs)


class FakeEmail:
    id = "msg_stale"
    inbox = monitor.AGENT_A
    subject = ""  # set per-test below


class FakeInbound:
    def from_event(self, event):
        return FakeEmail()


class FakeClient:
    inbound = FakeInbound()


monitor._client = FakeClient()
emitted = []
monitor.log = lambda event, **f: emitted.append((event, f))

fake_strategies = {iface: FakeStrategy() for iface in monitor.IFACES}
monitor.STRATEGIES = fake_strategies

inbound_body = envelope("email.received", {"message_id": "msg_stale", "delivered_to": monitor.AGENT_A})


def emitted_events():
    return [e for e, _ in emitted]


# Stale guard on the reply leg (inbox == A): a reply whose nonce timestamp is
# older than MAX_AGE_MS must not emit monitor_ok.
old_ms = int(time.time() * 1000) - monitor.MAX_AGE_MS - 60_000
FakeEmail.subject = f"Re: probe {make_nonce('python_sdk', old_ms)}"
emitted.clear()
status, _ = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("stale reply returns 200", status, 200)
check("stale reply does not emit monitor_ok", "monitor_ok" in emitted_events(), False)
check("stale reply emits monitor_stale", "monitor_stale" in emitted_events(), True)

# Fresh reply on the same path emits the success marker with iface + latency,
# and does NOT dispatch to any strategy (inbox == A is the terminal leg).
FakeEmail.subject = f"Re: probe {make_nonce('python_sdk', int(time.time() * 1000) - 4200)}"
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("fresh reply emits monitor_ok", "monitor_ok" in emitted_events(), True)
check("fresh reply reports latency", payload["latency_ms"] >= 4200, True)
ok_fields = next(f for e, f in emitted if e == "monitor_ok")
check("fresh reply's monitor_ok carries iface", ok_fields.get("iface"), "python_sdk")
check("fresh reply's monitor_ok carries latency_ms", ok_fields.get("latency_ms", 0) >= 4200, True)

# A subject with no probe nonce is ignored, not counted.
FakeEmail.subject = "hello from a real human"
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("non-probe subject ignored", payload.get("ignored"), "not a probe")

# Hydration is where a leg can die before it is even classified, and the two
# failure kinds need opposite answers.
_saved_inbound = FakeClient.inbound


class RaisingInbound:
    def __init__(self, exc):
        self._exc = exc

    def from_event(self, event):
        raise self._exc


# not_found is terminal — the message is gone (purged by a sweep, deleted by an
# operator) and no redelivery brings it back. 5xx here asks e2a to retry a
# delivery that can only 404 again, turning one lost probe into an unbounded
# retry loop against the very server under test.
FakeClient.inbound = RaisingInbound(
    monitor.E2ANotFoundError(code="not_found", message="message not found", status=404, retryable=False)
)
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("hydrate not_found does not ask for a retry", status, 200)
check("hydrate not_found reports the drop", payload.get("dropped"), True)
check("hydrate not_found logs monitor_dropped", "monitor_dropped" in emitted_events(), True)
check("hydrate not_found claims no success", "monitor_ok" in emitted_events(), False)

# Everything else may genuinely be transient (timeout, upstream 5xx). Dropping
# those would lose a leg that would have succeeded on redelivery and
# manufacture a false silence alert, so they keep asking for the retry.
FakeClient.inbound = RaisingInbound(RuntimeError("connection reset"))
emitted.clear()
status, _ = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("hydrate transient failure still retries", status, 500)
check("hydrate transient failure logs monitor_error", "monitor_error" in emitted_events(), True)

FakeClient.inbound = _saved_inbound


# ---------------------------------------------------------------------------
# Interface-strategy dispatch: the outbound leg (inbox == B) replies via the
# EXACT strategy named in the nonce, and no other strategy is touched.
# ---------------------------------------------------------------------------


class FakeEmailB:
    id = "msg_fresh_b"
    inbox = monitor.AGENT_B
    subject = ""


class FakeInboundB:
    def from_event(self, event):
        return FakeEmailB()


class FakeClientB:
    inbound = FakeInboundB()


monitor._client = FakeClientB()

fresh_ms = int(time.time() * 1000) - 100
FakeEmailB.subject = f"probe {make_nonce('ts_sdk', fresh_ms)}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
inbound_body_b = envelope("email.received", {"message_id": "msg_fresh_b", "delivered_to": monitor.AGENT_B})
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("outbound leg (fresh) returns 200", status, 200)
check("outbound leg dispatches to the nonce's own iface (ts_sdk)", len(fake_strategies["ts_sdk"].reply_calls), 1)
check("outbound leg does not touch other ifaces", sum(len(s.reply_calls) for k, s in fake_strategies.items() if k != "ts_sdk"), 0)
replied_kwargs = fake_strategies["ts_sdk"].reply_calls[0]
check("dispatched reply targets inbox B", replied_kwargs.get("inbox"), monitor.AGENT_B)
check("dispatched reply carries the message id", replied_kwargs.get("message_id"), "msg_fresh_b")
check("dispatched reply carries an idempotency key scoped to the event", replied_kwargs.get("idempotency_key"), "sdkmon:evt_test_1")
check("monitor_replied logs the dispatched iface", any(e == "monitor_replied" and f.get("iface") == "ts_sdk" for e, f in emitted), True)

# Stale guard on the outbound leg (inbox == B) must ALSO block dispatch —
# never calls the strategy's reply().
FakeEmailB.subject = f"probe {make_nonce('cli', old_ms)}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, _ = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("stale outbound leg does not dispatch a reply", len(fake_strategies["cli"].reply_calls), 0)
check("stale outbound leg emits monitor_stale", "monitor_stale" in emitted_events(), True)


# ---------------------------------------------------------------------------
# Nonce MAC authentication (FIX 1): only a nonce whose tag actually matches
# _nonce_tag(iface, sent_ms, rand_hex) may act on either leg. Anyone able to
# email agent A can pick an iface/timestamp/random hex freely, but without
# NONCE_KEY they cannot produce a matching tag — the forged nonce must be
# silently ignored: no reply dispatched, no monitor_ok, no crash.
# ---------------------------------------------------------------------------

_valid_epoch = fresh_ms
_valid_rand = "abcdef0123456789"
_valid_tag = monitor._nonce_tag("ts_sdk", _valid_epoch, _valid_rand)

# Tampered iface: a real tag, but minted for "ts_sdk" and replayed under a
# different iface name in the subject.
FakeEmailB.subject = f"probe e2asdkmon.cli.{_valid_epoch}.{_valid_rand}.{_valid_tag}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("tampered-iface nonce returns 200", status, 200)
check("tampered-iface nonce is ignored as a bad nonce mac", payload.get("ignored"), "bad nonce mac")
check("tampered-iface nonce dispatches no reply", sum(len(s.reply_calls) for s in fake_strategies.values()), 0)
check("tampered-iface nonce logs stage=nonce_auth", any(e == "monitor_error" and f.get("stage") == "nonce_auth" for e, f in emitted), True)
check("tampered-iface nonce never emits monitor_ok", "monitor_ok" in emitted_events(), False)
check("tampered-iface nonce never emits monitor_replied", "monitor_replied" in emitted_events(), False)

# Tampered timestamp: a real tag for _valid_epoch, replayed under a forged
# (fresher) epoch in the subject — the forger can't produce a tag for a
# timestamp they didn't request one for, so this also defeats trying to
# "refresh" a captured nonce's apparent age.
_forged_epoch = _valid_epoch + 1
FakeEmailB.subject = f"probe e2asdkmon.ts_sdk.{_forged_epoch}.{_valid_rand}.{_valid_tag}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("tampered-timestamp nonce is ignored as a bad nonce mac", payload.get("ignored"), "bad nonce mac")
check("tampered-timestamp nonce dispatches no reply", sum(len(s.reply_calls) for s in fake_strategies.values()), 0)

# Tampered tag: correct iface/epoch/rand, but the tag itself is flipped.
_bad_tag = ("0" if _valid_tag[0] != "0" else "1") + _valid_tag[1:]
FakeEmailB.subject = f"probe e2asdkmon.ts_sdk.{_valid_epoch}.{_valid_rand}.{_bad_tag}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("tampered-tag nonce is ignored as a bad nonce mac", payload.get("ignored"), "bad nonce mac")
check("tampered-tag nonce dispatches no reply", sum(len(s.reply_calls) for s in fake_strategies.values()), 0)

# Old-style 3-part nonce (pre-MAC, no tag segment): NONCE_RE simply doesn't
# match it, so it's "not a probe", not "bad nonce mac" — there is no partial
# parse to reject.
FakeEmailB.subject = f"probe e2asdkmon.ts_sdk.{_valid_epoch}.{_valid_rand}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("old-style 3-part nonce is not treated as a probe", payload.get("ignored"), "not a probe")
check("old-style 3-part nonce dispatches no reply", sum(len(s.reply_calls) for s in fake_strategies.values()), 0)

# The positive counterpart, right next to the tamper tests above: a
# correctly-tagged fresh nonce on the outbound leg is accepted end-to-end and
# dispatches exactly one reply via the named interface.
FakeEmailB.subject = f"probe e2asdkmon.ts_sdk.{_valid_epoch}.{_valid_rand}.{_valid_tag}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("valid nonce accepted end-to-end (200)", status, 200)
check("valid nonce dispatches exactly one reply via its iface", len(fake_strategies["ts_sdk"].reply_calls), 1)
check("valid nonce does not touch other ifaces", sum(len(s.reply_calls) for k, s in fake_strategies.items() if k != "ts_sdk"), 0)


# ---------------------------------------------------------------------------
# Unknown iface: parses AND authenticates (a real tag for that iface name),
# but isn't in STRATEGIES — handled safely, no crash, no success/stale claim,
# no dispatch.
# ---------------------------------------------------------------------------

_unknown_iface_tag = monitor._nonce_tag("some_future_iface", fresh_ms, "0123456789abcdef")
FakeEmailB.subject = f"probe e2asdkmon.some_future_iface.{fresh_ms}.0123456789abcdef.{_unknown_iface_tag}"
for s in fake_strategies.values():
    s.reply_calls.clear()
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("unknown iface handled without crashing (200)", status, 200)
check("unknown iface does not dispatch any strategy", sum(len(s.reply_calls) for s in fake_strategies.values()), 0)
check("unknown iface logs a distinct error stage", any(e == "monitor_error" and f.get("stage") == "unknown_iface" for e, f in emitted), True)
check("unknown iface never emits monitor_ok", "monitor_ok" in emitted_events(), False)


# ---------------------------------------------------------------------------
# Cleanup (message deletion): each leg trashes its message via the SAME
# interface, AFTER the leg's success marker — a hygiene/coverage step that is
# deliberately decoupled from the round-trip success signal.
# ---------------------------------------------------------------------------


class CleanupStrategy(FakeStrategy):
    def __init__(self, fail_delete=False, delete_supported=True):
        super().__init__()
        self.fail_delete = fail_delete
        self._delete_supported = delete_supported

    def supports_delete(self):
        return self._delete_supported

    def delete(self, **kwargs):
        super().delete(**kwargs)
        if self.fail_delete:
            raise RuntimeError("delete boom")


# B leg: after the reply is dispatched, the probe in B's inbox is trashed via
# the nonce's own interface, logged as monitor_deleted, after monitor_replied.
cleanup_ts = CleanupStrategy()
monitor._client = FakeClientB()
monitor.STRATEGIES = {iface: FakeStrategy() for iface in monitor.IFACES}
monitor.STRATEGIES["ts_sdk"] = cleanup_ts
FakeEmailB.subject = f"probe {make_nonce('ts_sdk', int(time.time() * 1000) - 100)}"
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("cleanup (B leg) returns 200", status, 200)
check("cleanup (B leg) still dispatches the reply first", len(cleanup_ts.reply_calls), 1)
check("cleanup (B leg) deletes via the nonce's own iface", len(cleanup_ts.delete_calls), 1)
_del_kwargs = cleanup_ts.delete_calls[0]
check(
    "cleanup (B leg) deletes the probe from B's inbox",
    (_del_kwargs.get("inbox"), _del_kwargs.get("message_id")),
    (monitor.AGENT_B, "msg_fresh_b"),
)
check(
    "cleanup (B leg) does not delete via other ifaces",
    sum(len(s.delete_calls) for k, s in monitor.STRATEGIES.items() if k != "ts_sdk"),
    0,
)
check("cleanup (B leg) logs monitor_deleted", any(e == "monitor_deleted" and f.get("iface") == "ts_sdk" for e, f in emitted), True)
check(
    "cleanup (B leg) runs after monitor_replied",
    [e for e, _ in emitted].index("monitor_replied") < [e for e, _ in emitted].index("monitor_deleted"),
    True,
)

# A leg: after monitor_ok, the reply in A's inbox is trashed the same way.
cleanup_py = CleanupStrategy()
monitor._client = FakeClient()
monitor.STRATEGIES = {iface: FakeStrategy() for iface in monitor.IFACES}
monitor.STRATEGIES["python_sdk"] = cleanup_py
FakeEmail.subject = f"Re: probe {make_nonce('python_sdk', int(time.time() * 1000) - 3000)}"
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("cleanup (A leg) returns 200", status, 200)
check("cleanup (A leg) still emits monitor_ok", "monitor_ok" in emitted_events(), True)
check("cleanup (A leg) deletes via the nonce's own iface", len(cleanup_py.delete_calls), 1)
check(
    "cleanup (A leg) deletes the reply from A's inbox",
    (cleanup_py.delete_calls[0].get("inbox"), cleanup_py.delete_calls[0].get("message_id")),
    (monitor.AGENT_A, FakeEmail.id),
)
check(
    "cleanup (A leg) runs after monitor_ok",
    [e for e, _ in emitted].index("monitor_ok") < [e for e, _ in emitted].index("monitor_deleted"),
    True,
)

# A cleanup failure is decoupled from the success signal: 200, monitor_ok
# stands, stage=cleanup error logged, no monitor_deleted.
failing_cleanup = CleanupStrategy(fail_delete=True)
monitor.STRATEGIES = {iface: FakeStrategy() for iface in monitor.IFACES}
monitor.STRATEGIES["python_sdk"] = failing_cleanup
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body, sign(inbound_body))
check("cleanup failure (A leg) still returns 200", status, 200)
check("cleanup failure (A leg) still emits monitor_ok", "monitor_ok" in emitted_events(), True)
check(
    "cleanup failure (A leg) logs stage=cleanup",
    any(e == "monitor_error" and f.get("stage") == "cleanup" and f.get("iface") == "python_sdk" for e, f in emitted),
    True,
)
check("cleanup failure (A leg) does not log monitor_deleted", "monitor_deleted" in emitted_events(), False)

# An interface without a delete verb (cli): cleanup is a documented skip, not
# an error — the reply is still dispatched and no delete is attempted.
no_delete_cleanup = CleanupStrategy(delete_supported=False)
monitor._client = FakeClientB()
monitor.STRATEGIES = {iface: FakeStrategy() for iface in monitor.IFACES}
monitor.STRATEGIES["cli"] = no_delete_cleanup
FakeEmailB.subject = f"probe {make_nonce('cli', int(time.time() * 1000) - 100)}"
emitted.clear()
status, payload = monitor.handle_webhook(inbound_body_b, sign(inbound_body_b))
check("cleanup-unsupported iface still replies and returns 200", status, 200)
check("cleanup-unsupported iface dispatches the reply", len(no_delete_cleanup.reply_calls), 1)
check("cleanup-unsupported iface does not attempt a delete", no_delete_cleanup.delete_calls, [])
check(
    "cleanup-unsupported iface logs monitor_skip stage=cleanup",
    any(e == "monitor_skip" and f.get("stage") == "cleanup" and f.get("iface") == "cli" for e, f in emitted),
    True,
)
check(
    "cleanup-unsupported iface does not log a cleanup error",
    any(e == "monitor_error" and f.get("stage") == "cleanup" for e, f in emitted),
    False,
)


# ---------------------------------------------------------------------------
# /tick aggregate + status-code semantics (FIX 2): the aggregate is computed
# over non-skipped ("considered") interfaces only, and the HTTP status
# distinguishes all-ok / partial / total outage for Cloud Scheduler.
# ---------------------------------------------------------------------------


class SendOnlyStrategy(FakeStrategy):
    def __init__(self, should_fail=False, is_available=True):
        super().__init__()
        self.should_fail = should_fail
        self._available = is_available

    def available(self):
        return self._available

    def send(self, **kwargs):
        super().send(**kwargs)
        if self.should_fail:
            raise RuntimeError("boom")


def tick_strategies(fail=(), unavailable=()):
    return {
        iface: SendOnlyStrategy(should_fail=iface in fail, is_available=iface not in unavailable)
        for iface in monitor.IFACES
    }


# handle_tick ends with a hygiene sweep that talks to the API directly rather
# than through a strategy, so the strategy fakes above do not cover it. Stub it
# out for the aggregate/status-code checks — an offline suite must make no real
# network call, and the sweep has its own tests below.
_saved_sweep_inbox = monitor._sweep_inbox
_swept_inboxes = []


def _fake_sweep_inbox(inbox):
    _swept_inboxes.append(inbox)
    return (0, 0)


monitor._sweep_inbox = _fake_sweep_inbox

# All interfaces succeed -> 200, ok=True.
monitor.STRATEGIES = tick_strategies()
emitted.clear()
status, payload = monitor.handle_tick()
check("tick all-ok returns 200", status, 200)
check("tick all-ok reports ok=True", payload["ok"], True)

# `mcp` skipped (unconfigured — a supported normal deployment), every other
# interface succeeds -> still 200, ok=True: a skip must not permanently pin
# the aggregate to False.
mcp_skipped_strategies = tick_strategies(unavailable={"mcp"})
monitor.STRATEGIES = mcp_skipped_strategies
emitted.clear()
status, payload = monitor.handle_tick()
check("tick mcp-skipped-others-ok returns 200", status, 200)
check("tick mcp-skipped-others-ok reports ok=True", payload["ok"], True)
check("tick mcp-skipped-others-ok marks mcp as skipped", payload["results"]["mcp"]["skipped"], True)
check("tick mcp-skipped-others-ok does not call send on the skipped iface", mcp_skipped_strategies["mcp"].send_calls, [])
check("tick mcp-skipped-others-ok logs monitor_skip for mcp", any(e == "monitor_skip" and f.get("iface") == "mcp" for e, f in emitted), True)

# Partial failure: mcp's send fails, cli is unavailable (skipped), the rest
# succeed -> 207, ok=False.
partial_strategies = tick_strategies(fail={"mcp"}, unavailable={"cli"})
monitor.STRATEGIES = partial_strategies
emitted.clear()
status, payload = monitor.handle_tick()
check("tick partial-failure returns 207", status, 207)
check("tick partial-failure reports ok=False", payload["ok"], False)
check("tick partial-failure attempts every non-skipped iface", partial_strategies["api"].send_calls != [], True)
check("tick partial-failure reports the failing iface as not ok", payload["results"]["mcp"]["ok"], False)
check("tick partial-failure reports a succeeding iface as ok", payload["results"]["python_sdk"]["ok"], True)
check("tick partial-failure does not call send on the unavailable iface", partial_strategies["cli"].send_calls, [])
check("tick partial-failure marks the unavailable iface as skipped", payload["results"]["cli"]["skipped"], True)
check("tick partial-failure logs monitor_skip for the unavailable iface", any(e == "monitor_skip" and f.get("iface") == "cli" for e, f in emitted), True)
check("tick partial-failure logs monitor_error for the failing iface", any(e == "monitor_error" and f.get("iface") == "mcp" and f.get("stage") == "send" for e, f in emitted), True)

# Total outage: every CONSIDERED interface fails (mcp is skipped, so it's not
# considered and can't rescue the aggregate) -> 500, ok=False.
all_fail_strategies = tick_strategies(fail=set(monitor.IFACES) - {"mcp"}, unavailable={"mcp"})
monitor.STRATEGIES = all_fail_strategies
emitted.clear()
status, payload = monitor.handle_tick()
check("tick all-fail (considered) returns 500", status, 500)
check("tick all-fail reports ok=False", payload["ok"], False)
check("tick all-fail still marks the skipped iface as skipped, not failed", payload["results"]["mcp"]["skipped"], True)

# The sweep runs on every tick, including a total outage: residue accumulates
# regardless of whether the client surfaces are healthy, and a stuck sweep
# would be exactly the wrong response to an already-degraded stack.
check("tick sweeps both monitor inboxes", sorted(set(_swept_inboxes)), sorted({monitor.AGENT_A, monitor.AGENT_B}))
monitor._sweep_inbox = _saved_sweep_inbox


# ---------------------------------------------------------------------------
# FIX 3: uniform non-"sent"/"accepted" send-status handling. A held
# (pending_review), failed, or unrecognized status must raise immediately
# for every offline-exercisable interface — never logged success-shaped.
# ---------------------------------------------------------------------------

import urllib.request as _urllib_request  # noqa: E402
import urllib.error as _urllib_error  # noqa: E402


class _FakeHeaders:
    def __init__(self, content_type):
        self._content_type = content_type

    def get(self, name, default=None):
        return self._content_type if name == "Content-Type" else default


class _FakeResponse:
    def __init__(self, raw: bytes, content_type: str):
        self._raw = raw
        self.headers = _FakeHeaders(content_type)

    def read(self):
        return self._raw

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _stub_urlopen(sse_body: str):
    def _fake_urlopen(req, timeout=None):
        return _FakeResponse(sse_body.encode(), "text/event-stream")

    return _fake_urlopen


def _stub_urlopen_json(response_body: dict):
    def _fake_urlopen(req, timeout=None):
        return _FakeResponse(json.dumps(response_body).encode(), "application/json")

    return _fake_urlopen


_saved_urlopen = _urllib_request.urlopen

# --- api ---
_api = monitor.ApiStrategy("https://api.example", "e2a_acct_test")
for _status in ("pending_review", "failed", "some_unrecognized_status"):
    _urllib_request.urlopen = _stub_urlopen_json({"status": _status, "message_id": "msg_x"})
    try:
        _api.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
        _api_raised = False
    except RuntimeError:
        _api_raised = True
    check(f"api strategy raises on send status={_status!r}", _api_raised, True)

_urllib_request.urlopen = _stub_urlopen_json({"status": "accepted", "message_id": "msg_x"})
try:
    _api.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _api_ok = True
except Exception:
    _api_ok = False
check("api strategy does not raise on status=accepted", _api_ok, True)
_urllib_request.urlopen = _saved_urlopen


# --- python_sdk ---
class _FakeSendResult:
    def __init__(self, status):
        self.status = status
        self.message_id = "msg_x"


class _FakeMessagesResource:
    def __init__(self, status):
        self._status = status

    def send(self, *a, **k):
        return _FakeSendResult(self._status)

    def reply(self, *a, **k):
        return _FakeSendResult(self._status)


class _FakePySdkClient:
    def __init__(self, status):
        self.messages = _FakeMessagesResource(status)


_py = monitor.PythonSdkStrategy()
_saved_client = monitor._client
for _status in ("pending_review", "failed", None):
    monitor._client = _FakePySdkClient(_status)
    try:
        _py.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
        _py_raised = False
    except RuntimeError:
        _py_raised = True
    check(f"python_sdk strategy raises on send status={_status!r}", _py_raised, True)

monitor._client = _FakePySdkClient("accepted")
try:
    _py.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _py_ok = True
except Exception:
    _py_ok = False
check("python_sdk strategy does not raise on status=accepted", _py_ok, True)
monitor._client = _saved_client


# python_sdk delete: client.messages.delete(inbox, message_id) with the
# DeleteMessageResult receipt validated.
class _FakeDeleteReceipt:
    def __init__(self, deleted):
        self.deleted = deleted


class _FakeDeleteMessages:
    def __init__(self, deleted):
        self._deleted = deleted
        self.delete_calls = []

    def delete(self, *a, **k):
        self.delete_calls.append((a, k))
        return _FakeDeleteReceipt(self._deleted)


class _FakePySdkDeleteClient:
    def __init__(self, deleted):
        self.messages = _FakeDeleteMessages(deleted)


_saved_client = monitor._client
monitor._client = _FakePySdkDeleteClient(True)
try:
    _py.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
    _py_del_ok = True
except Exception:
    _py_del_ok = False
check("python_sdk strategy accepts a deleted:true receipt", _py_del_ok, True)
check(
    "python_sdk delete calls client.messages.delete(inbox, message_id)",
    monitor._client.messages.delete_calls,
    [(("mon-b@agents.e2a.dev", "msg_abc"), {})],
)

monitor._client = _FakePySdkDeleteClient(False)
try:
    _py.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
    _py_del_raised = False
except RuntimeError:
    _py_del_raised = True
check("python_sdk strategy raises on a non-deleted receipt", _py_del_raised, True)
monitor._client = _saved_client


# --- mcp ---
def _mcp_sse_for_status(status):
    text = json.dumps({"status": status, "message_id": "msg_x"})
    env = {"jsonrpc": "2.0", "id": 1, "result": {"content": [{"type": "text", "text": text}]}}
    return f"data: {json.dumps(env)}\n\n"


_mcp = monitor.McpStrategy("https://mcp.example/mcp", "e2a_acct_test")

for _status in ("pending_review", "failed"):
    _urllib_request.urlopen = _stub_urlopen(_mcp_sse_for_status(_status))
    try:
        _mcp.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
        _mcp_status_raised = False
    except RuntimeError:
        _mcp_status_raised = True
    check(f"mcp strategy raises on send status={_status!r}", _mcp_status_raised, True)

_urllib_request.urlopen = _stub_urlopen(_mcp_sse_for_status("accepted"))
try:
    _mcp.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_status_ok = True
except Exception:
    _mcp_status_ok = False
check("mcp strategy does not raise on status=accepted", _mcp_status_ok, True)
_urllib_request.urlopen = _saved_urlopen


# --- ts_sdk (via the node helper's exit code, mirroring the CLI) ---
def _fake_subprocess_run_factory(returncode: int, stdout: str = "", stderr: str = ""):
    def _fake_run(argv, env=None, capture_output=None, text=None, timeout=None):
        class _Result:
            pass

        r = _Result()
        r.returncode = returncode
        r.stdout = stdout
        r.stderr = stderr
        return r

    return _fake_run


_saved_subprocess_run = monitor.subprocess.run
_ts = monitor.TsSdkStrategy("https://api.e2a.dev", "e2a_acct_secret")

monitor.subprocess.run = _fake_subprocess_run_factory(
    1, stdout=json.dumps({"status": "pending_review", "message_id": "msg_x"}) + "\n",
    stderr='non-success send status: "pending_review"\n',
)
try:
    _ts.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _ts_raised = False
except RuntimeError:
    _ts_raised = True
check("ts_sdk strategy raises when the node helper exits non-zero (held/failed)", _ts_raised, True)

monitor.subprocess.run = _fake_subprocess_run_factory(
    0, stdout=json.dumps({"status": "accepted", "message_id": "msg_x"}) + "\n"
)
try:
    _ts.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _ts_ok = True
except Exception:
    _ts_ok = False
check("ts_sdk strategy does not raise when the node helper exits 0 (accepted)", _ts_ok, True)

monitor.subprocess.run = _saved_subprocess_run


# ---------------------------------------------------------------------------
# FIX 4: minimal subprocess environment — the full parent environment
# (including secrets this child never needs, like the webhook secret) must
# never be handed to a node/CLI subprocess.
# ---------------------------------------------------------------------------

_captured_env_calls = []


def _capturing_subprocess_run(argv, env=None, capture_output=None, text=None, timeout=None):
    _captured_env_calls.append({"argv": list(argv), "env": dict(env or {})})

    class _Result:
        returncode = 0
        stdout = json.dumps({"status": "accepted", "message_id": "msg_x"}) + "\n"
        stderr = ""

    return _Result()


os.environ["E2A_MONITOR_TEST_SHOULD_NOT_LEAK"] = "super-secret-value"
monitor.subprocess.run = _capturing_subprocess_run
_captured_env_calls.clear()
_ts.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
_env = _captured_env_calls[-1]["env"]
check("subprocess env does not leak an arbitrary parent env var", "E2A_MONITOR_TEST_SHOULD_NOT_LEAK" in _env, False)
check("subprocess env does not leak the webhook secret", "E2A_MONITOR_WEBHOOK_SECRET" in _env, False)
check("subprocess env still carries PATH", "PATH" in _env, True)
check("subprocess env carries the explicit override (E2A_API_KEY)", _env.get("E2A_API_KEY"), "e2a_acct_secret")
del os.environ["E2A_MONITOR_TEST_SHOULD_NOT_LEAK"]
monitor.subprocess.run = _saved_subprocess_run


# ---------------------------------------------------------------------------
# FIX 5: wire-shape assertions for api / ts_sdk / cli — a flag/route/header
# typo must fail a test, not ship silently.
# ---------------------------------------------------------------------------


class _CapturedRequest:
    def __init__(self, req):
        self.full_url = req.full_url
        self.method = req.get_method()
        self.headers = {k: v for k, v in req.header_items()}
        self.data = req.data


_captured_requests = []


def _capturing_urlopen(response_body: dict):
    def _fake_urlopen(req, timeout=None):
        _captured_requests.append(_CapturedRequest(req))
        return _FakeResponse(json.dumps(response_body).encode(), "application/json")

    return _fake_urlopen


# ApiStrategy: POST /v1/agents/{email}/messages, Authorization: Bearer, JSON
# body shape (api/openapi.yaml SendEmailRequest: to/subject/text).
_urllib_request.urlopen = _capturing_urlopen({"status": "accepted", "message_id": "msg_x"})
_api_wire = monitor.ApiStrategy("https://api.e2a.dev", "e2a_acct_secret")
_captured_requests.clear()
_api_wire.send(agent_a="mon-a@agents.e2a.dev", agent_b="mon-b@agents.e2a.dev", subject="probe nonce123", body="nonce123")
_req = _captured_requests[-1]
check("ApiStrategy.send method is POST", _req.method, "POST")
check(
    "ApiStrategy.send URL is /v1/agents/{email}/messages",
    _req.full_url,
    "https://api.e2a.dev/v1/agents/mon-a%40agents.e2a.dev/messages",
)
check("ApiStrategy.send sends Authorization: Bearer <key>", header(_req.headers, "Authorization"), "Bearer e2a_acct_secret")
check(
    "ApiStrategy.send body shape matches SendEmailRequest(to, subject, text)",
    json.loads(_req.data.decode()),
    {"to": ["mon-b@agents.e2a.dev"], "subject": "probe nonce123", "text": "nonce123"},
)
check("ApiStrategy.send: the API key never appears in the URL", "e2a_acct_secret" in _req.full_url, False)

# ApiStrategy.reply: POST .../messages/{id}/reply, Idempotency-Key header, and
# ReplyRequest's wire shape (text only — no subject field).
_captured_requests.clear()
_api_wire.reply(inbox="mon-a@agents.e2a.dev", message_id="msg_abc", text="nonce123", idempotency_key="sdkmon:evt_1")
_req = _captured_requests[-1]
check(
    "ApiStrategy.reply URL is .../messages/{id}/reply",
    _req.full_url,
    "https://api.e2a.dev/v1/agents/mon-a%40agents.e2a.dev/messages/msg_abc/reply",
)
check("ApiStrategy.reply sends Idempotency-Key", header(_req.headers, "Idempotency-Key"), "sdkmon:evt_1")
check("ApiStrategy.reply body shape is text-only (ReplyRequest has no subject)", json.loads(_req.data.decode()), {"text": "nonce123"})

# ApiStrategy.delete: DELETE /v1/agents/{email}/messages/{id}, no body, and
# the DeleteMessageResult receipt ({deleted:true, id}) is validated.
_urllib_request.urlopen = _capturing_urlopen({"deleted": True, "id": "msg_abc"})
_captured_requests.clear()
_api_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
_req = _captured_requests[-1]
check("ApiStrategy.delete method is DELETE", _req.method, "DELETE")
check(
    "ApiStrategy.delete URL is .../messages/{id}",
    _req.full_url,
    "https://api.e2a.dev/v1/agents/mon-b%40agents.e2a.dev/messages/msg_abc",
)
check("ApiStrategy.delete sends no body", _req.data, None)
check("ApiStrategy.delete sends Authorization: Bearer <key>", header(_req.headers, "Authorization"), "Bearer e2a_acct_secret")

_urllib_request.urlopen = _capturing_urlopen({"deleted": False, "id": "msg_abc"})
try:
    _api_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
    _api_del_raised = False
except RuntimeError:
    _api_del_raised = True
check("ApiStrategy.delete raises on a non-deleted receipt", _api_del_raised, True)

_urllib_request.urlopen = _saved_urlopen


# TsSdkStrategy / CliStrategy: capture subprocess.run's argv + env.
_captured_calls = []


def _capturing_subprocess_run_for_wire(argv, env=None, capture_output=None, text=None, timeout=None):
    _captured_calls.append({"argv": list(argv), "env": dict(env or {})})

    class _Result:
        returncode = 0
        stdout = json.dumps({"status": "accepted", "message_id": "msg_x"}) + "\n"
        stderr = ""

    return _Result()


monitor.subprocess.run = _capturing_subprocess_run_for_wire

_ts_wire = monitor.TsSdkStrategy("https://api.e2a.dev", "e2a_acct_secret")
_captured_calls.clear()
_ts_wire.send(agent_a="mon-a@agents.e2a.dev", agent_b="mon-b@agents.e2a.dev", subject="probe x", body="x")
_call = _captured_calls[-1]
check("TsSdkStrategy.send invokes node with the helper path", _call["argv"][:2], ["node", monitor.NODE_HELPER])
check(
    "TsSdkStrategy.send passes send + positional args",
    _call["argv"][2:],
    ["send", "mon-a@agents.e2a.dev", "mon-b@agents.e2a.dev", "probe x", "x"],
)
check("TsSdkStrategy child env carries E2A_API_URL", _call["env"].get("E2A_API_URL"), "https://api.e2a.dev")
check("TsSdkStrategy child env carries E2A_API_KEY", _call["env"].get("E2A_API_KEY"), "e2a_acct_secret")
check("TsSdkStrategy secret never appears in argv", any("e2a_acct_secret" in a for a in _call["argv"]), False)

_captured_calls.clear()
_ts_wire.reply(inbox="mon-a@agents.e2a.dev", message_id="msg_abc", text="x", idempotency_key="sdkmon:evt_1")
_call = _captured_calls[-1]
check(
    "TsSdkStrategy.reply passes reply + positional args",
    _call["argv"][2:],
    ["reply", "mon-a@agents.e2a.dev", "msg_abc", "x", "sdkmon:evt_1"],
)

_captured_calls.clear()
_ts_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
_call = _captured_calls[-1]
check(
    "TsSdkStrategy.delete passes delete + positional args",
    _call["argv"][2:],
    ["delete", "mon-b@agents.e2a.dev", "msg_abc"],
)
check("TsSdkStrategy.delete secret never appears in argv", any("e2a_acct_secret" in a for a in _call["argv"]), False)

_cli_wire = monitor.CliStrategy("https://api.e2a.dev", "e2a_acct_secret")
_captured_calls.clear()
_cli_wire.send(agent_a="mon-a@agents.e2a.dev", agent_b="mon-b@agents.e2a.dev", subject="probe x", body="x")
_call = _captured_calls[-1]
check("CliStrategy.send invokes CLI_BIN via node", _call["argv"][:2], ["node", monitor.CLI_BIN])
check(
    "CliStrategy.send argv matches cli/src/bin/e2a.ts's send flags",
    _call["argv"][2:],
    [
        "send", "--to", "mon-b@agents.e2a.dev", "--subject", "probe x", "--body", "x",
        "--agent", "mon-a@agents.e2a.dev", "--json",
    ],
)
check("CliStrategy child env carries E2A_URL", _call["env"].get("E2A_URL"), "https://api.e2a.dev")
check("CliStrategy secret never appears in argv", any("e2a_acct_secret" in a for a in _call["argv"]), False)

_captured_calls.clear()
_cli_wire.reply(inbox="mon-a@agents.e2a.dev", message_id="msg_abc", text="x", idempotency_key="sdkmon:evt_1")
_call = _captured_calls[-1]
check(
    "CliStrategy.reply argv matches cli/src/bin/e2a.ts's reply flags",
    _call["argv"][2:],
    ["reply", "msg_abc", "--body", "x", "--agent", "mon-a@agents.e2a.dev", "--idempotency-key", "sdkmon:evt_1", "--json"],
)

# The published CLI has no delete command: CliStrategy must report no delete
# support (so cleanup is a documented skip) and its delete() must never be
# silently wired to some other verb.
check("CliStrategy reports no delete support", _cli_wire.supports_delete(), False)
try:
    _cli_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
    _cli_del_raised = False
except RuntimeError:
    _cli_del_raised = True
check("CliStrategy.delete raises (no delete verb in the published CLI)", _cli_del_raised, True)

monitor.subprocess.run = _saved_subprocess_run


# ---------------------------------------------------------------------------
# McpStrategy response parsing (FIX 6 additions at the end): SSE framing,
# JSON-RPC error, tool isError, a leading notification/mismatched-id frame
# that must be skipped (not mistaken for our response), a stream where no
# frame matches our request id, and the malformed-response guard (neither
# result nor error present must NOT be silently treated as success — found
# in adversarial review of the original change).
# ---------------------------------------------------------------------------

_mcp2 = monitor.McpStrategy("https://mcp.example/mcp", "e2a_acct_test")

_urllib_request.urlopen = _stub_urlopen(_mcp_sse_for_status("accepted"))
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_send_ok = True
except Exception:
    _mcp_send_ok = False
check("mcp strategy parses a well-formed SSE result", _mcp_send_ok, True)

_urllib_request.urlopen = _stub_urlopen(
    'data: {"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}\n\n'
)
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_error_raised = False
except RuntimeError:
    _mcp_error_raised = True
check("mcp strategy raises on a JSON-RPC error envelope", _mcp_error_raised, True)

_urllib_request.urlopen = _stub_urlopen(
    'data: {"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"e2a error [invalid_request]: bad"}]}}\n\n'
)
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_tool_error_raised = False
except RuntimeError:
    _mcp_tool_error_raised = True
check("mcp strategy raises on a tool-level isError result", _mcp_tool_error_raised, True)

# The malformed case: neither "result" nor "error" — must raise, not silently
# succeed (this was the adversarial-review finding this test guards).
_urllib_request.urlopen = _stub_urlopen('data: {"jsonrpc":"2.0","id":1}\n\n')
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_malformed_raised = False
except RuntimeError:
    _mcp_malformed_raised = True
check("mcp strategy raises on a response with neither result nor error", _mcp_malformed_raised, True)

# A leading notification (no id) plus an unrelated mismatched-id response,
# both BEFORE the real id:1 result — must be skipped, not mistaken for our
# response and not falsely tripped by the malformed-response guard.
_leading_notification = '{"jsonrpc":"2.0","method":"notifications/progress","params":{}}'
_mismatched_id_frame = '{"jsonrpc":"2.0","id":99,"result":{"content":[]}}'
_real_result_frame = json.dumps(
    {
        "jsonrpc": "2.0",
        "id": 1,
        "result": {"content": [{"type": "text", "text": json.dumps({"status": "accepted", "message_id": "msg_x"})}]},
    }
)
_sse_with_leading_frames = (
    f"data: {_leading_notification}\n\n"
    f"data: {_mismatched_id_frame}\n\n"
    f"data: {_real_result_frame}\n\n"
)
_urllib_request.urlopen = _stub_urlopen(_sse_with_leading_frames)
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_leading_frames_ok = True
except Exception:
    _mcp_leading_frames_ok = False
check("mcp strategy skips a leading notification/mismatched-id frame and finds the real id:1 result", _mcp_leading_frames_ok, True)

# Only a mismatched-id frame present anywhere in the stream — no frame ever
# matches our request id, so this must raise, not silently succeed.
_urllib_request.urlopen = _stub_urlopen(f"data: {_mismatched_id_frame}\n\n")
try:
    _mcp2.send(agent_a="a@x", agent_b="b@x", subject="probe x", body="x")
    _mcp_only_mismatched_raised = False
except RuntimeError:
    _mcp_only_mismatched_raised = True
check("mcp strategy raises when no frame matches our request id", _mcp_only_mismatched_raised, True)

_urllib_request.urlopen = _saved_urlopen


# McpStrategy.delete: the delete_message tool call's wire shape (name +
# message_id/email/confirm:true arguments) and receipt validation.
def _mcp_sse_for_payload(payload: dict):
    env = {"jsonrpc": "2.0", "id": 1, "result": {"content": [{"type": "text", "text": json.dumps(payload)}]}}
    return f"data: {json.dumps(env)}\n\n"


_captured_mcp_requests = []


def _capturing_urlopen_sse(sse_body: str):
    def _fake_urlopen(req, timeout=None):
        _captured_mcp_requests.append(_CapturedRequest(req))
        return _FakeResponse(sse_body.encode(), "text/event-stream")

    return _fake_urlopen


_mcp_wire = monitor.McpStrategy("https://mcp.example/mcp", "e2a_acct_test")
_urllib_request.urlopen = _capturing_urlopen_sse(_mcp_sse_for_payload({"deleted": True, "id": "msg_abc"}))
_captured_mcp_requests.clear()
_mcp_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
_mcp_payload = json.loads(_captured_mcp_requests[-1].data.decode())
check("McpStrategy.delete calls the delete_message tool", _mcp_payload["params"]["name"], "delete_message")
check(
    "McpStrategy.delete passes message_id + email + confirm:true",
    _mcp_payload["params"]["arguments"],
    {"message_id": "msg_abc", "email": "mon-b@agents.e2a.dev", "confirm": True},
)

_urllib_request.urlopen = _stub_urlopen(_mcp_sse_for_payload({"unexpected": "receipt"}))
try:
    _mcp_wire.delete(inbox="mon-b@agents.e2a.dev", message_id="msg_abc")
    _mcp_del_raised = False
except RuntimeError:
    _mcp_del_raised = True
check("McpStrategy.delete raises on a non-deleted receipt", _mcp_del_raised, True)

_urllib_request.urlopen = _saved_urlopen

check("mcp strategy reports unavailable when no URL is configured", monitor.McpStrategy("", "k").available(), False)
check("mcp strategy reports available when a URL is configured", monitor.McpStrategy("https://x/mcp", "k").available(), True)


# --------------------------------------------------------------------------
# Hygiene sweep. The sweep is the only part of the monitor that deletes
# messages the current tick did not create, so the ordering (trash BEFORE
# purge), the query parameters (the endpoint defaults would skip exactly the
# residue it exists to clear), and the best-effort contract all matter.
# --------------------------------------------------------------------------


def _sweep_urlopen(live_ids, trashed_ids, calls, fail_on=None):
    """Serve two id pages and record every request. fail_on(url, method) may
    raise to simulate a 409/held message."""

    def _fake_urlopen(req, timeout=None):
        url = req.full_url
        method = req.get_method()
        calls.append((method, url))
        if fail_on is not None:
            fail_on(url, method)
        if method == "GET":
            ids = trashed_ids if "deleted=true" in url else live_ids
            body = {"items": [{"id": i} for i in ids], "next_cursor": None}
        else:
            body = {"deleted": True, "id": "x"}
        return _FakeResponse(json.dumps(body).encode(), "application/json")

    return _fake_urlopen


_sweep_calls = []
_urllib_request.urlopen = _sweep_urlopen(["m_live1", "m_live2"], ["m_tr1"], _sweep_calls)
_trashed, _purged = monitor._sweep_inbox("a@x.test")
_urllib_request.urlopen = _saved_urlopen

check("sweep trashes every live message", _trashed, 2)
check("sweep purges every trashed message", _purged, 1)

_sweep_deletes = [(m, u) for m, u in _sweep_calls if m == "DELETE"]
check("sweep issues one DELETE per message", len(_sweep_deletes), 3)
# A purge before its trash would 409 not_in_trash against the real server.
check(
    "sweep trashes first (no permanent flag on the live deletes)",
    [("permanent=true" in u) for _, u in _sweep_deletes],
    [False, False, True],
)
check(
    "sweep purge carries the confirm token",
    "confirm=DELETE" in _sweep_deletes[-1][1],
    True,
)

_sweep_lists = [u for m, u in _sweep_calls if m == "GET"]
check("sweep lists live then trash", len(_sweep_lists), 2)
# Defaults are inbound-only and, for inbound, unread-only: without both
# overrides every outbound copy (47k per inbox on staging) stays live forever.
check(
    "sweep lists both directions and all read states",
    all("direction=all" in u and "read_status=all" in u for u in _sweep_lists),
    True,
)
check("sweep bounds each page by the budget", all(f"limit={monitor.SWEEP_BUDGET}" in u for u in _sweep_lists), True)
check("sweep's first list is live messages", "deleted=true" not in _sweep_lists[0], True)
check("sweep's second list is the trash", "deleted=true" in _sweep_lists[1], True)

# The regression this pair exists for: the sweep runs at the end of /tick, but
# a round trip finishes LATER, on /webhook. An unscoped page is newest-first,
# so it is the tick's own in-flight probes — the monitor purges the round trip
# it just started, the leg 404s on hydrate or reply, and the interface goes
# silent. That is a manufactured outage, not a detected one.
check(
    "sweep asks for the oldest residue first, not the newest",
    all("sort=asc" in u for u in _sweep_lists),
    True,
)


def _excludes_live_traffic(url):
    """Whether one sweep listing is scoped past everything a round trip in
    flight could still legitimately use. An unscoped listing is reported as
    False rather than raising, so a regression reads as a failed check."""
    if "until=" not in url:
        return False
    value = url.split("until=")[1].split("&")[0]
    until_ms = calendar.timegm(time.strptime(value, "%Y-%m-%dT%H:%M:%SZ")) * 1000
    return time.time() * 1000 - until_ms >= monitor.MAX_AGE_MS


check("sweep scopes every list by an until cutoff", all("until=" in u for u in _sweep_lists), True)
# sort=asc alone only postpones the collision until the backlog drains below
# one page; the cutoff is what makes an in-flight probe unreachable at all.
check(
    "sweep's cutoff excludes everything a live round trip could still use",
    all(_excludes_live_traffic(u) for u in _sweep_lists),
    True,
)


# A delete that fails (message held for review, or provider submission still in
# flight) is skipped and retried next tick, never counted as done.
def _fail_every_delete(url, method):
    if method == "DELETE":
        raise _urllib_error.HTTPError(url, 409, "conflict", {}, io.BytesIO(b'{"error":{"code":"message_held"}}'))


_held_calls = []
_urllib_request.urlopen = _sweep_urlopen(["m_held"], [], _held_calls, fail_on=_fail_every_delete)
_trashed_held, _purged_held = monitor._sweep_inbox("a@x.test")
_urllib_request.urlopen = _saved_urlopen
check("sweep does not count a failed delete", (_trashed_held, _purged_held), (0, 0))


# Listing failure ends the pass without raising: hygiene must never propagate
# into the tick's status code, which Cloud Scheduler reads as stack health.
def _fail_every_call(url, method):
    raise _urllib_error.HTTPError(url, 500, "boom", {}, io.BytesIO(b"{}"))


_urllib_request.urlopen = _sweep_urlopen([], [], [], fail_on=_fail_every_call)
emitted.clear()
_trashed_err, _purged_err = monitor._sweep_inbox("a@x.test")
_urllib_request.urlopen = _saved_urlopen
check("sweep returns zero counts when listing fails", (_trashed_err, _purged_err), (0, 0))
check(
    "sweep logs a monitor_error with stage=sweep",
    any(e == "monitor_error" and f.get("stage") == "sweep" for e, f in emitted),
    True,
)


print()
if failures:
    print(f"{len(failures)} failure(s):")
    for f in failures:
        print("  " + f)
    sys.exit(1)
print("all checks passed")
