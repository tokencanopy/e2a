"""Continuous production conformance monitor for the *published* e2a client
surfaces: the raw HTTP API, the Python SDK, the TypeScript SDK, the CLI, and
the MCP server.

Neither existing validator touches a published client. `cmd/e2a-prober`
speaks raw HTTP against a staging deploy and `tests/e2e-prod` deliberately
uses zero-dependency `fetch`. So a broken publish is invisible — the SDKs sat
at 4.0.1 on PyPI/npm while `main` was at 5.2.0 and nothing caught it. This
service closes that gap by driving a real agent-to-agent round trip through
EACH of five interfaces a user would actually reach for:

    api          raw HTTP against the server (no SDK) — isolates a
                 server-contract break from an SDK break
    python_sdk   the published `e2a` PyPI package
    mcp          the deployed MCP streamable-HTTP server's tool call
    ts_sdk       the published `@e2a/sdk` npm package (shelled to node)
    cli          the published `@e2a/cli` npm package (shelled to node)

All five never touch workspace source (pinned in requirements.txt / package.json,
installed from PyPI/npm — see Dockerfile).

Stateless and request-driven — Cloud Run scales to zero, so nothing may live
in memory between requests except immutable, config-derived interface
strategies (no per-round-trip state). Correlation state travels in the
message subject, which now also carries which interface sent it:

    POST /tick      one round trip PER interface: send A -> B, subject
                    "probe <nonce>"; nonce embeds the interface, the send
                    time, and randomness
    POST /webhook   email.received on B -> reply (via the SAME interface the
                    nonce names), then trash the probe; email.received on A
                    -> success, then trash the reply (cleanup is best-effort
                    and decoupled from the success signal — see _cleanup)
    GET  /health    liveness

Success is a structured log line (``monitor_ok``) carrying `iface` and the
round-trip `latency_ms`; ops builds a log-based metric plus a per-interface
"no success in N minutes" alert on it. There is no in-process aggregation to
lose — everything needed to build the alert lives in one log line.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import re
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Optional, Protocol

from e2a.v1 import (
    E2AClient,
    E2ANotFoundError,
    E2AWebhookSignatureError,
    construct_event,
    is_email_received,
)

# Subject carries the correlation state: a marker, WHICH INTERFACE performed
# the send, the send time in epoch ms (so latency needs no storage),
# randomness so concurrent probes and redelivered events can never be
# confused for one another, and a keyed MAC tag binding all three together
# (see NONCE_KEY / _nonce_tag below) so the nonce can only be minted by this
# service — not just anyone who can email agent A. The iface segment is
# captured generically (`[a-z0-9_]+`) rather than hardcoded to the five known
# keys, so an unrecognized value is a *parsed-but-unknown* iface (handled
# explicitly, see handle_webhook) instead of silently falling through "not a
# probe". Older nonces (pre-multi-interface or pre-MAC, no iface/tag segment)
# simply won't match — no deployed consumer depends on parsing those today.
NONCE_RE = re.compile(r"e2asdkmon\.([a-z0-9_]+)\.(\d+)\.([0-9a-f]{16})\.([0-9a-f]{16})")
SUBJECT_PREFIX = "probe"

API_KEY = os.environ.get("E2A_API_KEY", "")
BASE_URL = os.environ.get("E2A_BASE_URL", "https://api.e2a.dev")
AGENT_A = os.environ.get("E2A_MONITOR_AGENT_A", "")
AGENT_B = os.environ.get("E2A_MONITOR_AGENT_B", "")
WEBHOOK_SECRET = os.environ.get("E2A_MONITOR_WEBHOOK_SECRET", "")
# Nonce-signing key. Optional and dedicated (E2A_MONITOR_NONCE_SECRET) so it
# CAN be rotated independently of the webhook HMAC secret, but falls back to
# the already-required WEBHOOK_SECRET so protection never silently depends on
# an unset var — no new required config. Domain separation from the webhook
# signature's own use of this same value comes from the fixed message prefix
# in _nonce_tag, not from a distinct key.
NONCE_SECRET = os.environ.get("E2A_MONITOR_NONCE_SECRET", "")
NONCE_KEY = NONCE_SECRET or WEBHOOK_SECRET
# Full streamable-HTTP MCP endpoint (e.g. https://api.e2a.dev/mcp), matching
# the prober's E2A_PROBE_MCP_URL convention. Optional: when unset, the mcp
# interface is skipped (monitor_skip) rather than failing the whole service —
# not every deployment necessarily exposes the MCP server at a fixed URL this
# monitor can reach.
MCP_URL = os.environ.get("E2A_MONITOR_MCP_URL", "")
# A reply older than this is a stale redelivery, not a fresh success — it must
# never refresh the alert's "last seen" clock.
MAX_AGE_MS = int(os.environ.get("E2A_MONITOR_MAX_AGE_MS", "900000"))
# Per-tick ceiling on the hygiene sweep, per inbox and per phase. The tick
# creates a handful of messages, so the surplus also drains backlog from
# earlier ticks without turning the monitor into a bulk-delete client.
SWEEP_BUDGET = int(os.environ.get("E2A_MONITOR_SWEEP_BUDGET", "50"))
# How old a message must be before the sweep may touch it. NOT a tuning knob:
# the sweep runs at the end of /tick, but a round trip completes LATER, on
# /webhook, so the tick's own probes are still in flight when it runs. Below
# this age a purge destroys the monitor's own probe and manufactures the
# outage it exists to detect.
#
# MAX_AGE_MS is exactly the right floor, and provably so: a message's
# created_at is >= the sent_ms its nonce carries (the row is written at or
# after the send), and a leg only counts when now - sent_ms <= MAX_AGE_MS. So
# a row older than this cutoff can no longer produce anything but
# monitor_stale, and purging it cannot cost a success. The extra minute is
# margin for clock skew between this container and the server.
SWEEP_MIN_AGE_MS = MAX_AGE_MS + 60_000
PORT = int(os.environ.get("PORT", "8080"))

# Every interface this service exercises, in the fixed order /tick fires them.
IFACES = ("api", "python_sdk", "mcp", "ts_sdk", "cli")

# The raw-HTTP interfaces (api, mcp) call urllib directly; its default
# User-Agent (Python-urllib/x.y) is blocked by Cloudflare bot rules
# (HTTP 403 / error 1010) on Cloudflare-fronted hosts such as prod
# api.e2a.dev. Present a real, identifiable UA so the monitor's traffic
# isn't treated as an anonymous scraper. The python_sdk/ts_sdk/cli
# interfaces set their own client UAs and are unaffected.
USER_AGENT = "e2a-sdk-monitor/1.0"

APP_DIR = os.path.dirname(os.path.abspath(__file__))
NODE_HELPER = os.path.join(APP_DIR, "js", "monitor-helper.mjs")
CLI_BIN = os.path.join(APP_DIR, "node_modules", "@e2a", "cli", "dist", "bin", "e2a.js")
SUBPROCESS_TIMEOUT_S = 20

_client: Optional[E2AClient] = None


def log(event: str, **fields: object) -> None:
    """One JSON line per event on stdout. Never pass a secret in here."""
    print(json.dumps({"event": event, **fields}, default=str), flush=True)


def client() -> E2AClient:
    global _client
    if _client is None:
        _client = E2AClient(api_key=API_KEY, base_url=BASE_URL)
    return _client


NONCE_TAG_PREFIX = "e2asdkmon-nonce:"


def _nonce_tag(iface: str, epoch_ms: int, rand_hex: str, key: str = NONCE_KEY) -> str:
    """First 16 hex chars of HMAC-SHA256(key, "e2asdkmon-nonce:<iface>.<epoch_ms>.<rand_hex>").
    The fixed prefix domain-separates this MAC from any other use of `key`
    (e.g. the webhook signature, which also derives from WEBHOOK_SECRET when
    no dedicated E2A_MONITOR_NONCE_SECRET is set). Never log `key` or the
    resulting `tag` alongside enough context to look like the secret itself —
    the tag is a public value that rides in the nonce, but callers should
    still avoid gratuitously echoing it."""
    msg = f"{NONCE_TAG_PREFIX}{iface}.{epoch_ms}.{rand_hex}".encode()
    return hmac.new(key.encode(), msg, hashlib.sha256).hexdigest()[:16]


def new_nonce(iface: str) -> str:
    epoch_ms = int(time.time() * 1000)
    rand_hex = secrets.token_hex(8)
    tag = _nonce_tag(iface, epoch_ms, rand_hex)
    return f"e2asdkmon.{iface}.{epoch_ms}.{rand_hex}.{tag}"


# Send-response outcome handling, uniform across every interface (api,
# python_sdk, mcp, ts_sdk via js/monitor-helper.mjs, cli via its own subprocess
# exit code). SendResultView.status (api/openapi.yaml) is an open string set;
# only `sent` and `accepted` are genuinely successful outcomes (`accepted` =
# durably queued for async submission, the normal non-held outcome).
# `pending_review` (held for human review) and `failed` (terminal failure) —
# and any status this build doesn't recognize — must NOT be logged
# success-shaped; they must fail immediately at send/reply time rather than
# only surfacing later as a monitor_stale timeout. Mirrors the CLI's
# emitSendResult (cli/src/commands/send.ts), which already got this right.
SEND_OK_STATUSES = frozenset({"sent", "accepted"})


def _check_send_status(iface: str, status: object) -> None:
    if status not in SEND_OK_STATUSES:
        raise RuntimeError(f"{iface} send/reply returned non-success status: {status!r}")


# --------------------------------------------------------------------------
# Interface strategies.
#
# Each interface implements the same three operations, uniform across all
# five so the webhook hub can dispatch on the iface name alone:
#
#   send(agent_a, agent_b, subject, body) -> None    # outbound leg
#   reply(inbox, message_id, text, idempotency_key) -> None   # inbound leg
#   delete(inbox, message_id) -> None                # cleanup (trash the
#                                                    # probe/reply message)
#
# supports_delete() reports whether the interface's published client HAS a
# delete verb at all — the published @e2a/cli has no delete command, so the
# cli interface reports False and its cleanup is a documented skip, never a
# silent substitution through another surface (which would mask the missing
# capability).
#
# None of the five ever sets a subject on reply: every wire shape (REST
# ReplyRequest, the SDKs' reply(), the CLI's `reply`, the MCP
# reply_to_message tool) derives it server-side as "Re: <original subject>",
# which still satisfies NONCE_RE.search() since the nonce is a substring.
# --------------------------------------------------------------------------


class Interface(Protocol):
    def available(self) -> bool: ...

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None: ...

    def reply(
        self, *, inbox: str, message_id: str, text: str, idempotency_key: str
    ) -> None: ...

    def supports_delete(self) -> bool: ...

    def delete(self, *, inbox: str, message_id: str) -> None: ...


class ApiStrategy:
    """Raw HTTP against the server API — no SDK. Exercises the wire contract
    directly, so a server-side regression shows up here even if every SDK
    happens to paper over it (or vice versa: an SDK-only bug does NOT show up
    here, which is exactly the point of running both)."""

    def __init__(self, base_url: str, api_key: str) -> None:
        self._base_url = base_url.rstrip("/")
        self._api_key = api_key

    def available(self) -> bool:
        return True  # core interface; required config already gates startup

    def _request(
        self,
        path: str,
        body: Optional[dict] = None,
        method: str = "POST",
        idempotency_key: Optional[str] = None,
    ) -> dict:
        url = f"{self._base_url}{path}"
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self._api_key}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        req.add_header("User-Agent", USER_AGENT)
        if idempotency_key:
            req.add_header("Idempotency-Key", idempotency_key)
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")[:300]
            raise RuntimeError(f"api HTTP {exc.code}: {detail}") from exc
        if not raw:
            return {}
        try:
            return json.loads(raw.decode("utf-8", "replace"))
        except ValueError:
            return {}

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None:
        path = f"/v1/agents/{urllib.parse.quote(agent_a, safe='')}/messages"
        result = self._request(path, {"to": [agent_b], "subject": subject, "text": body})
        _check_send_status("api", result.get("status"))

    def reply(self, *, inbox: str, message_id: str, text: str, idempotency_key: str) -> None:
        path = (
            f"/v1/agents/{urllib.parse.quote(inbox, safe='')}"
            f"/messages/{urllib.parse.quote(message_id, safe='')}/reply"
        )
        result = self._request(path, {"text": text}, idempotency_key=idempotency_key)
        _check_send_status("api", result.get("status"))

    def supports_delete(self) -> bool:
        return True

    def delete(self, *, inbox: str, message_id: str) -> None:
        path = (
            f"/v1/agents/{urllib.parse.quote(inbox, safe='')}"
            f"/messages/{urllib.parse.quote(message_id, safe='')}"
        )
        result = self._request(path, method="DELETE")
        # The receipt is DeleteMessageResult ({deleted:true, id}) — a 200 with
        # anything else is a contract drift worth failing on, same discipline
        # as _check_send_status on the send path.
        if result.get("deleted") is not True:
            raise RuntimeError(f"api delete returned an unexpected receipt: {result!r}")


class PythonSdkStrategy:
    """The published `e2a` PyPI package — today's original path."""

    def available(self) -> bool:
        return True  # core interface; required config already gates startup

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None:
        result = client().messages.send(agent_a, {"to": [agent_b], "subject": subject, "text": body})
        _check_send_status("python_sdk", getattr(result, "status", None))

    def reply(self, *, inbox: str, message_id: str, text: str, idempotency_key: str) -> None:
        result = client().messages.reply(
            inbox, message_id, {"text": text}, idempotency_key=idempotency_key
        )
        _check_send_status("python_sdk", getattr(result, "status", None))

    def supports_delete(self) -> bool:
        return True

    def delete(self, *, inbox: str, message_id: str) -> None:
        result = client().messages.delete(inbox, message_id)
        if getattr(result, "deleted", None) is not True:
            raise RuntimeError(f"python_sdk delete returned an unexpected receipt: {result!r}")


# Fixed id shared by every send/reply JSON-RPC request this service makes.
# Fixing it lets the parser positively identify OUR response frame instead of
# just grabbing the first jsonrpc-shaped thing in the stream: if the MCP
# server ever emits a leading notification (no id) before the real
# tools/call response, or a stream somehow interleaves an unrelated
# response, a bare "first dict with jsonrpc in it" match would false-fail (or
# worse, false-succeed on) a healthy round trip.
MCP_REQUEST_ID = 1


def _is_matching_jsonrpc_response(env: object, request_id: int) -> bool:
    """True only for the frame that is actually THIS request's response:
    right id, and carrying a `result` or `error` (a notification has neither
    and must be skipped, not mistaken for the answer)."""
    return (
        isinstance(env, dict)
        and env.get("id") == request_id
        and ("result" in env or "error" in env)
    )


def _parse_jsonrpc_envelope(raw: bytes, content_type: str, request_id: int = MCP_REQUEST_ID) -> dict:
    """Decode a JSON-RPC message from an MCP streamable-HTTP response,
    accepting either a bare JSON body or an SSE stream (the deployed MCP
    server runs stateless with `enableJsonResponse` unset, so it answers with
    SSE — see mcp/src/http-server.ts). Mirrors
    internal/selftest/scenarios.go's parseJSONRPCEnvelope so both validators
    agree on the wire shape. Skips any frame that isn't THIS request's
    response (wrong id, or a notification with neither result nor error) —
    see _is_matching_jsonrpc_response."""
    if "text/event-stream" in content_type:
        body = raw.decode("utf-8", "replace").replace("\r\n", "\n")
        for event in body.split("\n\n"):
            data_lines = [
                line[len("data:") :].lstrip(" ")
                for line in event.split("\n")
                if line.startswith("data:")
            ]
            if not data_lines:
                continue
            try:
                env = json.loads("\n".join(data_lines))
            except ValueError:
                continue
            if _is_matching_jsonrpc_response(env, request_id):
                return env
        raise RuntimeError("no matching JSON-RPC response in SSE stream")
    env = json.loads(raw.decode("utf-8", "replace"))
    if not _is_matching_jsonrpc_response(env, request_id):
        raise RuntimeError(f"JSON-RPC response id mismatch or missing result/error: {env}")
    return env


class McpStrategy:
    """The deployed MCP server's `send_message` / `reply_to_message` tools
    over streamable HTTP (mcp/src/tools/messages.ts). Stateless: the server
    skips all session/initialize gating when built with
    `sessionIdGenerator: undefined` (mcp/src/http-server.ts), so a bare
    `tools/call` dispatches with no prior `initialize` — a hand-rolled single
    JSON-RPC request suffices without a full client library."""

    def __init__(self, url: str, api_key: str) -> None:
        self._url = url
        self._api_key = api_key

    def available(self) -> bool:
        return bool(self._url)

    def _call(self, name: str, arguments: dict) -> dict:
        if not self._url:
            raise RuntimeError("E2A_MONITOR_MCP_URL not configured")
        payload = json.dumps(
            {
                "jsonrpc": "2.0",
                "id": MCP_REQUEST_ID,
                "method": "tools/call",
                "params": {"name": name, "arguments": arguments},
            }
        ).encode()
        req = urllib.request.Request(self._url, data=payload, method="POST")
        req.add_header("Authorization", f"Bearer {self._api_key}")
        req.add_header("Content-Type", "application/json")
        req.add_header("User-Agent", USER_AGENT)
        # Streamable-HTTP requires the client to accept both framings.
        req.add_header("Accept", "application/json, text/event-stream")
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read()
                content_type = resp.headers.get("Content-Type", "")
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")[:300]
            raise RuntimeError(f"mcp HTTP {exc.code}: {detail}") from exc
        # _parse_jsonrpc_envelope already guarantees the returned frame
        # matches our request id and carries a result or error — a malformed/
        # incomplete/mismatched-id response raises there, before we get here.
        env = _parse_jsonrpc_envelope(raw, content_type)
        if env.get("error"):
            raise RuntimeError(f"mcp JSON-RPC error: {env['error']}")
        result = env.get("result") or {}
        if result.get("isError"):
            text = ""
            for block in result.get("content") or []:
                if isinstance(block, dict) and block.get("type") == "text":
                    text = block.get("text", "")
                    break
            raise RuntimeError(f"mcp tool error: {text}")
        return result

    @staticmethod
    def _result_json(result: dict) -> Optional[dict]:
        """The tool's success text block is a JSON-stringified view
        (mcp/src/tools/util.ts's runTool + toMcpOutput) — a SendResultView on
        send/reply, a DeleteMessageResult on delete. Pull the parsed payload
        out so callers can check `status` / `deleted` and treat a held/failed
        or unreceipted outcome as a failure here too, not just on the
        api/python_sdk/ts_sdk interfaces."""
        for block in result.get("content") or []:
            if isinstance(block, dict) and block.get("type") == "text":
                try:
                    payload = json.loads(block.get("text", ""))
                except ValueError:
                    return None
                if isinstance(payload, dict):
                    return payload
        return None

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None:
        result = self._call(
            "send_message",
            {"to": [agent_b], "subject": subject, "text": body, "email": agent_a},
        )
        _check_send_status("mcp", (self._result_json(result) or {}).get("status"))

    def reply(self, *, inbox: str, message_id: str, text: str, idempotency_key: str) -> None:
        result = self._call(
            "reply_to_message",
            {
                "message_id": message_id,
                "text": text,
                "email": inbox,
                "idempotency_key": idempotency_key,
            },
        )
        _check_send_status("mcp", (self._result_json(result) or {}).get("status"))

    def supports_delete(self) -> bool:
        return True

    def delete(self, *, inbox: str, message_id: str) -> None:
        result = self._call(
            "delete_message",
            {"message_id": message_id, "email": inbox, "confirm": True},
        )
        if (self._result_json(result) or {}).get("deleted") is not True:
            raise RuntimeError("mcp delete_message returned an unexpected receipt")


# The full parent environment is never handed to a child: it would carry
# E2A_MONITOR_WEBHOOK_SECRET (and anything else configured on this Cloud Run
# service), which neither the ts_sdk helper nor the CLI needs — a dump-env-
# to-stderr bug in either child could then leak it straight into a logged
# monitor_error. Only PATH + HOME are passed through (verified sufficient to
# run node: `env -i PATH="$PATH" HOME="$HOME" node -e '...'` works), merged
# with the caller's explicit overrides (E2A_API_KEY + the base-url var).
_MINIMAL_ENV_PASSTHROUGH = ("PATH", "HOME")


def _minimal_env(env_overrides: dict) -> dict:
    env = {name: os.environ[name] for name in _MINIMAL_ENV_PASSTHROUGH if name in os.environ}
    env.update(env_overrides)
    return env


def _run_subprocess(argv: list[str], env_overrides: dict) -> str:
    env = _minimal_env(env_overrides)
    try:
        proc = subprocess.run(
            argv,
            env=env,
            capture_output=True,
            text=True,
            timeout=SUBPROCESS_TIMEOUT_S,
        )
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError(f"{argv[0]} timed out after {SUBPROCESS_TIMEOUT_S}s") from exc
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()[:300]
        raise RuntimeError(f"{argv[0]} exited {proc.returncode}: {detail}")
    return proc.stdout


class TsSdkStrategy:
    """The published `@e2a/sdk` npm package, driven from a small node helper
    (js/monitor-helper.mjs) — this Python service shells out rather than
    embedding a JS runtime. Never workspace source: the helper imports
    `@e2a/sdk/v1` resolved from node_modules, populated at image build time by
    `npm install @e2a/sdk@5.3.0` (see Dockerfile), same discipline as
    requirements.txt for the Python SDK.

    Send-status handling lives in the helper itself, not here: it now checks
    the same SEND_OK_STATUSES set (mirroring the CLI's emitSendResult) and
    exits non-zero on a held/failed/unrecognized status, so `_run_subprocess`
    raises uniformly with every other interface."""

    def __init__(self, base_url: str, api_key: str) -> None:
        # E2A_API_URL is the canonical name the TS SDK reads (client.ts);
        # E2A_BASE_URL is accepted too but emits a one-shot deprecation
        # warning on stderr, which would pollute the structured log stream.
        self._env = {"E2A_API_KEY": api_key, "E2A_API_URL": base_url}

    def available(self) -> bool:
        return True  # core interface; baked into the image at build time

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None:
        _run_subprocess(["node", NODE_HELPER, "send", agent_a, agent_b, subject, body], self._env)

    def reply(self, *, inbox: str, message_id: str, text: str, idempotency_key: str) -> None:
        _run_subprocess(
            ["node", NODE_HELPER, "reply", inbox, message_id, text, idempotency_key],
            self._env,
        )

    def supports_delete(self) -> bool:
        return True

    def delete(self, *, inbox: str, message_id: str) -> None:
        _run_subprocess(["node", NODE_HELPER, "delete", inbox, message_id], self._env)


class CliStrategy:
    """The published `@e2a/cli` npm package's `e2a` bin, invoked directly
    (`node .../dist/bin/e2a.js ...`) rather than relying on a PATH-installed
    symlink — more robust across base images. `e2a reply <id> --body ...`
    supports true in-thread reply (confirmed: cli/src/commands/send.ts calls
    `client.messages.reply`, whose wire ReplyRequest has no subject field, so
    the server derives "Re: <original subject>" the same as every other
    interface) — no send-only fallback needed."""

    def __init__(self, base_url: str, api_key: str) -> None:
        # The CLI reads E2A_URL (not E2A_BASE_URL/E2A_API_URL) for its API
        # host — see cli/src/config.ts. It also serves /mcp and /v1/* on the
        # same api.e2a.dev host per the Caddy allowlist, so pointing E2A_URL
        # straight at BASE_URL (rather than the dashboard root) works and
        # skips an extra proxy hop.
        self._env = {"E2A_API_KEY": api_key, "E2A_URL": base_url}

    def available(self) -> bool:
        return True  # core interface; baked into the image at build time

    def send(self, *, agent_a: str, agent_b: str, subject: str, body: str) -> None:
        _run_subprocess(
            ["node", CLI_BIN, "send", "--to", agent_b, "--subject", subject, "--body", body,
             "--agent", agent_a, "--json"],
            self._env,
        )

    def reply(self, *, inbox: str, message_id: str, text: str, idempotency_key: str) -> None:
        _run_subprocess(
            ["node", CLI_BIN, "reply", message_id, "--body", text, "--agent", inbox,
             "--idempotency-key", idempotency_key, "--json"],
            self._env,
        )

    def supports_delete(self) -> bool:
        # The published @e2a/cli (cli/src/commands/) has no delete command.
        # Cleanup for this interface is a documented skip (monitor_skip,
        # stage=cleanup) — never a silent substitution through another
        # interface's surface, which would mask the missing capability.
        return False

    def delete(self, *, inbox: str, message_id: str) -> None:
        raise RuntimeError("cli interface has no delete command (supports_delete is False)")


def _build_strategies() -> dict:
    return {
        "api": ApiStrategy(BASE_URL, API_KEY),
        "python_sdk": PythonSdkStrategy(),
        "mcp": McpStrategy(MCP_URL, API_KEY),
        "ts_sdk": TsSdkStrategy(BASE_URL, API_KEY),
        "cli": CliStrategy(BASE_URL, API_KEY),
    }


# Built once at import time from static config — not per-request state.
# Correlation state (which round trip is in flight) still lives entirely in
# the message subject, never here.
STRATEGIES: dict = _build_strategies()


def _cleanup(strategy: Interface, iface: str, nonce: str, *, inbox: str, message_id: str) -> None:
    """Best-effort trash of one round-trip message via the SAME interface
    that carried the leg — each interface's delete path is exercised every
    tick, and probe/reply residue doesn't accumulate in the two inboxes
    forever (trash purges after the retention window; the *sent* copies in
    the opposite folders are out of scope — the service is stateless and the
    send-side ids would have to ride in the nonce).

    Cleanup is deliberately decoupled from the success signal: the leg's
    monitor_replied/monitor_ok line is logged BEFORE this runs, so a delete
    failure is a monitor_error(stage=cleanup), never a 5xx — a webhook
    redelivery must not fan out duplicate replies or double-count a success
    for what is only a hygiene failure. An interface without a delete verb
    (cli) is a documented skip, not an error."""
    if not strategy.supports_delete():
        log("monitor_skip", stage="cleanup", iface=iface, detail="interface has no delete verb")
        return
    try:
        strategy.delete(inbox=inbox, message_id=message_id)
    except Exception as exc:  # noqa: BLE001 - cleanup failure is a signal, not a crash
        log("monitor_error", stage="cleanup", iface=iface, nonce=nonce, error=type(exc).__name__, detail=str(exc))
        return
    log("monitor_deleted", iface=iface, nonce=nonce, inbox=inbox, message_id=message_id)


def _sweep_api(path: str, method: str = "GET") -> dict:
    """One raw-API call for the hygiene sweep.

    Deliberately NOT routed through an Interface strategy: the per-leg
    _cleanup already exercises each interface's delete verb, which is the
    coverage that matters, and the cli interface has no delete verb at all.
    The sweep is housekeeping, so it takes the shortest reliable path."""
    req = urllib.request.Request(f"{BASE_URL.rstrip('/')}{path}", method=method)
    req.add_header("Authorization", f"Bearer {API_KEY}")
    req.add_header("User-Agent", USER_AGENT)
    with urllib.request.urlopen(req, timeout=15) as resp:
        raw = resp.read()
    return json.loads(raw.decode("utf-8", "replace")) if raw else {}


def _sweep_ids(inbox: str, *, deleted: bool) -> list:
    """Up to SWEEP_BUDGET message ids from one inbox, oldest first and none
    of them younger than SWEEP_MIN_AGE_MS.

    direction=all and read_status=all are both load-bearing: the endpoint
    defaults to inbound-only, and for inbound to unread-only, so the defaults
    would walk straight past every outbound copy — which is precisely the
    residue this sweep exists to clear (the per-leg _cleanup trashes the
    RECEIVED copy; the sent copy in the opposite folder was never in scope).

    So are the other two. `until` is the correctness boundary: the listing is
    newest-first by default, so an unscoped page is the tick's own in-flight
    probes — the sweep would eat the round trip it just started and the
    interface would go silent (see SWEEP_MIN_AGE_MS). `sort=asc` then aims
    the budget at the oldest residue, which is what the sweep is FOR; on its
    own it would only postpone the collision until the backlog drained below
    one page, so both are required, not alternatives."""
    cutoff = time.strftime(
        "%Y-%m-%dT%H:%M:%SZ",
        time.gmtime((time.time() * 1000 - SWEEP_MIN_AGE_MS) / 1000),
    )
    q = (
        f"direction=all&read_status=all&sort=asc&limit={SWEEP_BUDGET}"
        f"&until={cutoff}"
        f"{'&deleted=true' if deleted else ''}"
    )
    page = _sweep_api(f"/v1/agents/{urllib.parse.quote(inbox, safe='')}/messages?{q}")
    return [it["id"] for it in page.get("items", []) if it.get("id")]


def _sweep_inbox(inbox: str) -> tuple:
    """Trash this inbox's live messages, then purge its trash.

    The order is a requirement, not a preference: "delete forever" only accepts
    a message ALREADY in the trash (409 not_in_trash otherwise), so a live
    message has to be trashed first. Trashing is idempotent, so re-trashing a
    row an earlier pass moved is harmless.

    Best-effort, exactly like _cleanup and for the same reason: this runs after
    the tick's sends are logged, so a failure here is hygiene debt and must
    never change the tick's status code. Cloud Scheduler reads that code as the
    health of the client surfaces, not of our housekeeping."""
    trashed = purged = 0
    try:
        for mid in _sweep_ids(inbox, deleted=False):
            try:
                _sweep_api(
                    f"/v1/agents/{urllib.parse.quote(inbox, safe='')}"
                    f"/messages/{urllib.parse.quote(mid, safe='')}",
                    method="DELETE",
                )
                trashed += 1
            except Exception:  # noqa: BLE001,S112 - held/in-flight retries next tick
                continue
        for mid in _sweep_ids(inbox, deleted=True):
            try:
                _sweep_api(
                    f"/v1/agents/{urllib.parse.quote(inbox, safe='')}"
                    f"/messages/{urllib.parse.quote(mid, safe='')}"
                    "?permanent=true&confirm=DELETE",
                    method="DELETE",
                )
                purged += 1
            except Exception:  # noqa: BLE001,S112 - retries next tick
                continue
    except Exception as exc:  # noqa: BLE001 - a listing failure ends this pass only
        log("monitor_error", stage="sweep", inbox=inbox,
            error=type(exc).__name__, detail=str(exc))
    return trashed, purged


def handle_tick() -> tuple[int, dict]:
    """Fire one outbound leg PER interface: agent A -> agent B, nonce
    (encoding the interface) in subject and body. A per-interface send
    failure is reported in the summary and logged, but never aborts the
    others."""
    results: dict = {}
    for iface in IFACES:
        strategy = STRATEGIES[iface]
        if not strategy.available():
            log("monitor_skip", iface=iface, stage="send", detail="interface not configured")
            results[iface] = {"ok": False, "skipped": True}
            continue
        nonce = new_nonce(iface)
        try:
            strategy.send(
                agent_a=AGENT_A,
                agent_b=AGENT_B,
                subject=f"{SUBJECT_PREFIX} {nonce}",
                body=nonce,
            )
        except Exception as exc:  # noqa: BLE001 - any interface failure is a monitor signal
            log("monitor_error", stage="send", iface=iface, nonce=nonce, error=type(exc).__name__, detail=str(exc))
            results[iface] = {"ok": False, "nonce": nonce}
            continue
        log("monitor_tick", iface=iface, nonce=nonce)
        results[iface] = {"ok": True, "nonce": nonce}

    # Aggregate over CONSIDERED interfaces only — one that was skipped this
    # tick (currently only `mcp` absent E2A_MONITOR_MCP_URL, a supported
    # normal deployment) must not permanently pin `ok` to False even though
    # every configured interface succeeded.
    considered = [r for r in results.values() if not r.get("skipped")]
    ok = bool(considered) and all(r.get("ok") for r in considered)
    if not considered:
        # Nothing configured to attempt this tick — vacuously nothing failed.
        status_code = 200
    elif ok:
        status_code = 200
    elif any(r.get("ok") for r in considered):
        # Partial failure: some interfaces are broken, but not all — distinct
        # from a total outage so an alert/dashboard can tell them apart.
        status_code = 207
    else:
        # Total outage across every considered interface: Cloud Scheduler
        # needs a non-2xx so a send-side blackout doesn't read as healthy.
        status_code = 500

    # Hygiene, after the verdict is fixed so it cannot colour the tick. Without
    # it the two monitor inboxes keep every message the monitor ever sent —
    # 47k outbound copies each on staging — until unrelated deletes on that
    # instance start timing out.
    for inbox in (AGENT_A, AGENT_B):
        if not inbox:
            continue
        swept, gone = _sweep_inbox(inbox)
        if swept or gone:
            log("monitor_sweep", inbox=inbox, trashed=swept, purged=gone)

    return status_code, {"ok": ok, "results": results}


def handle_webhook(raw_body: bytes, signature: str) -> tuple[int, dict]:
    """Verify, hydrate, then act on which leg of the round trip this is.

    Verification and hydration ALWAYS go through the published Python SDK
    (this hub is the service's own infra, not the interface under test — the
    interface named in the nonce is exercised on send and on reply, not on
    receiving the webhook)."""
    # Fail closed: an unset secret means the deployment is misconfigured, and
    # accepting unverified deliveries would make the whole signal forgeable.
    if not WEBHOOK_SECRET:
        log("monitor_error", stage="config", detail="webhook secret not configured")
        return 401, {"error": "unauthorized"}
    try:
        event = construct_event(raw_body, signature, WEBHOOK_SECRET)
    except E2AWebhookSignatureError as exc:
        log("monitor_error", stage="signature", code=getattr(exc, "code", None))
        return 401, {"error": "unauthorized"}

    # An account-scoped subscription fans out email.sent, bounces, domain
    # events — everything that is not an inbound delivery is not our business.
    if not is_email_received(event):
        return 200, {"ok": True, "ignored": event.type}

    try:
        email = client().inbound.from_event(event)
    except E2ANotFoundError as exc:
        # Terminal, not transient: the message the event names is gone, and no
        # number of redeliveries brings it back. Answering 5xx here would ask
        # e2a to retry a delivery that can only 404 again — one purged probe
        # becomes an unbounded redelivery loop, which is how this was found.
        # Drop the leg with a 200 and let the iface fall silent: the
        # absent-metric alert is the honest signal for a lost round trip.
        log("monitor_dropped", stage="hydrate", event_id=event.id,
            error=type(exc).__name__, detail=str(exc))
        return 200, {"ok": False, "stage": "hydrate", "dropped": True}
    except Exception as exc:  # noqa: BLE001
        log("monitor_error", stage="hydrate", event_id=event.id, error=type(exc).__name__, detail=str(exc))
        # 5xx so e2a retries: a transient hydration failure shouldn't silently
        # drop a leg and manufacture a false alert.
        return 500, {"ok": False, "stage": "hydrate"}

    match = NONCE_RE.search(email.subject or "")
    if not match:
        return 200, {"ok": True, "ignored": "not a probe"}
    nonce, iface, sent_ms, rand_hex, received_tag = (
        match.group(0),
        match.group(1),
        int(match.group(2)),
        match.group(3),
        match.group(4),
    )

    # Authenticate the nonce BEFORE acting on either leg: anyone who can email
    # agent A can pick an iface/timestamp/16-hex suffix, but only this service
    # (holder of NONCE_KEY) can produce the matching tag. This also
    # authenticates sent_ms itself, so a forger can't mint a fresh-looking
    # nonce to defeat the staleness guard below. Never log the key or the
    # tag alongside anything that could reconstruct it.
    expected_tag = _nonce_tag(iface, sent_ms, rand_hex)
    if not hmac.compare_digest(expected_tag, received_tag):
        log("monitor_error", stage="nonce_auth", iface=iface)
        return 200, {"ok": True, "ignored": "bad nonce mac"}

    age_ms = int(time.time() * 1000) - sent_ms

    strategy = STRATEGIES.get(iface)
    if strategy is None:
        # A nonce that matches the shape but names an iface we don't
        # recognize (deploy skew, a hand-crafted probe, a future interface
        # this build predates) — handled safely: log and ignore, never crash
        # and never claim a success or a stale-guard outcome for a strategy
        # we don't have.
        log("monitor_error", stage="unknown_iface", nonce=nonce, iface=iface)
        return 200, {"ok": True, "ignored": "unknown iface"}

    # Which leg this is comes from the recipient, not from a "Re:" prefix:
    # mailers rewrite subjects, but the delivered-to agent is authoritative.
    inbox = (email.inbox or "").lower()

    if inbox == AGENT_B.lower():
        if age_ms > MAX_AGE_MS:
            log("monitor_stale", leg="outbound", iface=iface, nonce=nonce, age_ms=age_ms)
            return 200, {"ok": True, "stale": True}
        try:
            # idempotency_key keyed on the event so a webhook redelivery can't
            # fan out into duplicate replies.
            strategy.reply(
                inbox=AGENT_B,
                message_id=email.id,
                text=nonce,
                idempotency_key=f"sdkmon:{event.id}",
            )
        except Exception as exc:  # noqa: BLE001
            log("monitor_error", stage="reply", iface=iface, nonce=nonce, error=type(exc).__name__, detail=str(exc))
            return 500, {"ok": False, "stage": "reply"}
        log("monitor_replied", iface=iface, nonce=nonce, age_ms=age_ms)
        # Reply succeeded — trash the probe sitting in B's inbox.
        _cleanup(strategy, iface, nonce, inbox=AGENT_B, message_id=email.id)
        return 200, {"ok": True, "leg": "outbound"}

    if inbox == AGENT_A.lower():
        if age_ms > MAX_AGE_MS:
            # Deliberately not monitor_ok: a stale reply must not reset the
            # freshness alert.
            log("monitor_stale", leg="reply", iface=iface, nonce=nonce, age_ms=age_ms)
            return 200, {"ok": True, "stale": True}
        log("monitor_ok", iface=iface, nonce=nonce, latency_ms=age_ms, message_id=email.id)
        # Success is logged — trash the reply sitting in A's inbox.
        _cleanup(strategy, iface, nonce, inbox=AGENT_A, message_id=email.id)
        return 200, {"ok": True, "leg": "reply", "iface": iface, "latency_ms": age_ms}

    return 200, {"ok": True, "ignored": "unknown inbox"}


class Handler(BaseHTTPRequestHandler):
    server_version = "e2a-sdk-monitor"

    def _respond(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        if self.path.split("?")[0] == "/health":
            self._respond(200, {"status": "ok"})
        else:
            self._respond(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        path = self.path.split("?")[0]
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if path == "/tick":
            self._respond(*handle_tick())
        elif path == "/webhook":
            # Must be the RAW bytes — re-serialized JSON won't match the HMAC.
            self._respond(*handle_webhook(raw, self.headers.get("X-E2A-Signature") or ""))
        else:
            self._respond(404, {"error": "not found"})

    def log_message(self, fmt: str, *args: object) -> None:
        """Silence the default access log — it would interleave non-JSON lines
        into the structured stream the alert parses."""


def main() -> None:
    missing = [
        name
        for name, value in (
            ("E2A_API_KEY", API_KEY),
            ("E2A_MONITOR_AGENT_A", AGENT_A),
            ("E2A_MONITOR_AGENT_B", AGENT_B),
            ("E2A_MONITOR_WEBHOOK_SECRET", WEBHOOK_SECRET),
        )
        if not value
    ]
    if missing:
        # Names only — never the values.
        log("monitor_error", stage="config", detail=f"missing env: {', '.join(missing)}")
        sys.exit(1)
    log(
        "monitor_start",
        port=PORT,
        base_url=BASE_URL,
        agent_a=AGENT_A,
        agent_b=AGENT_B,
        ifaces=list(IFACES),
        mcp_configured=bool(MCP_URL),
    )
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
