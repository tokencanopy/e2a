"""Python contract-test runner for the shared scenarios.yaml.

Runs against a live test server. Requires env vars:
  E2A_TEST_BASE_URL  — test server URL (e.g. http://localhost:8080)
  E2A_TEST_API_KEY   — valid API key for the test user
  E2A_TEST_CAPPED_API_KEY — optional; key for the contract server's secondary
    account, seeded with tiny plan caps. Scenarios asserting quota enforcement
    run as that account and skip without it (a deployed staging target has no
    capped account to offer).
  E2A_TEST_OVERCAP_API_KEY: optional; key for the contract server's third
    account, seeded already over its plan caps. The scenario proving the 402
    envelope's `current` field runs as that account and skips without it (a
    deployed staging target has no over-cap account to offer).

The runner drives the server over raw HTTP (a thin scenario interpreter, not
the ergonomic client):
- Each request step issues a raw bearer-authed httpx request to step.path
- Auth-override scenarios send their own Authorization header (by design)
- WS scenarios skipped in this sync runner (async WS tested separately)

Setup steps requiring direct store access (inject_message, verify_domain as
setup) cause the scenario to be skipped with a clear reason.
"""

from __future__ import annotations

import base64
import binascii
import json as json_mod
import os
import re
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import httpx
import pytest
import yaml

from e2a.v1 import E2AClient
from e2a.v1.generated.models import PageMessageLifecycleTransition

# NOTE: the runner drives the server over raw HTTP (a thin scenario interpreter,
# not the ergonomic client). scenario `path`s are repointed from /api/v1 to /v1
# as part of the cross-language scenarios.yaml migration (tracked separately);
# this runner is gated behind live-server env vars and not part of unit CI.

# ── Config ────────────────────────────────────────────────────────

BASE_URL = os.environ.get("E2A_TEST_BASE_URL", "")
API_KEY = os.environ.get("E2A_TEST_API_KEY", "")
CAPPED_API_KEY = os.environ.get("E2A_TEST_CAPPED_API_KEY", "")
OVERCAP_API_KEY = os.environ.get("E2A_TEST_OVERCAP_API_KEY", "")

# tests/test_contract.py -> sdks/python/tests/ -> sdks/python/ -> sdks/ -> repo root
SCENARIOS_PATH = Path(__file__).resolve().parents[3] / "tests" / "contract" / "scenarios.yaml"

# ── Helpers ───────────────────────────────────────────────────────


def load_scenarios() -> list[dict[str, Any]]:
    with open(SCENARIOS_PATH) as f:
        data = yaml.safe_load(f)
    return data["scenarios"]


_MISSING = object()


def json_path_get(obj: Any, path: str, default: Any = None) -> Any:
    """Evaluate a simple JSON path like 'agents[0].email' or 'agents.length'."""
    parts = path.split(".")
    current = obj
    for part in parts:
        if part == "length":
            return len(current) if isinstance(current, list) else default
        m = re.match(r"^(.+)\[(\d+)\]$", part)
        if m:
            name, idx = m.group(1), int(m.group(2))
            arr = current.get(name) if isinstance(current, dict) else None
            if not isinstance(arr, list) or idx >= len(arr):
                return default
            current = arr[idx]
        else:
            if not isinstance(current, dict):
                return default
            current = current.get(part, default)
            if current is default:
                return default
    return current


def step_raw_body(step: dict[str, Any]) -> bytes | None:
    """Decode a step's raw_body_base64, enforcing one body source per step.

    The raw-bytes escape hatches exist because YAML is UTF-8 by definition and
    its \\xNN escape denotes a CODEPOINT (\\xFF is U+00FF, which encodes to the
    perfectly VALID two-byte C3 BF), so no scenario scalar can carry the
    ill-formed bytes invalid_utf8_rejected has to put on the wire.
    """
    encoded = step.get("raw_body_base64")
    if encoded is None:
        return None
    if "body" in step:
        raise ValueError(
            f"step {step['id']}: body and raw_body_base64 are mutually exclusive"
        )
    if encoded == "":
        raise ValueError(
            f"step {step['id']}: raw_body_base64 is empty; omit the key to send no body"
        )
    return base64.b64decode(encoded, validate=True)


def step_raw_headers(step: dict[str, Any]) -> dict[str, bytes]:
    """Decode headers_base64 into RAW BYTES.

    httpx encodes a ``str`` header value as ASCII and raises
    UnicodeEncodeError on anything above U+007F, so a non-ASCII value can only
    be sent as ``bytes`` — which httpx passes through verbatim (verified
    against a raw socket: ``Idempotency-Key: k\\xffz``).
    """
    return {
        name: base64.b64decode(encoded, validate=True)
        for name, encoded in (step.get("headers_base64") or {}).items()
    }


def values_equal(json_val: Any, yaml_val: Any) -> bool:
    """Cross-type comparison (JSON number vs YAML int, bool, string)."""
    if isinstance(yaml_val, bool):
        return json_val is yaml_val or json_val == yaml_val
    if isinstance(yaml_val, (int, float)) and isinstance(json_val, (int, float)):
        return json_val == yaml_val
    return str(json_val) == str(yaml_val)


# ── Scenario determination ────────────────────────────────────────

STORE_ACTIONS = {"inject_message", "verify_and_retry"}

CAPPED_KEY_PLACEHOLDER = "{capped_api_key}"
OVERCAP_KEY_PLACEHOLDER = "{overcap_api_key}"


def _scenario_uses_placeholder(sc: dict[str, Any], placeholder: str) -> bool:
    """True when the scenario's auth_override anywhere names `placeholder`.

    Inspects auth_override VALUES specifically. Dumping the whole scenario and
    substring-matching looks equivalent and is not: the scenario's own
    description mentions the placeholder in prose, so a blob match stays true
    even if every auth_override is switched back to the primary account —
    exactly the regression this is meant to detect. (It also avoids serializing
    arbitrary YAML scalars, which JSON cannot always encode.)
    """
    overrides = [sc.get("auth_override")]
    for key in ("steps", "cleanup", "setup"):
        for step in sc.get(key) or []:
            if isinstance(step, dict):
                overrides.append(step.get("auth_override"))
    return any(isinstance(o, str) and placeholder in o for o in overrides)


def scenario_needs_capped_account(sc: dict[str, Any]) -> bool:
    """True when the scenario authenticates as the capped-plan account.

    Those scenarios need a cap they can actually reach, which only the contract
    server's seeded secondary account provides.
    """
    return _scenario_uses_placeholder(sc, CAPPED_KEY_PLACEHOLDER)


def scenario_needs_overcap_account(sc: dict[str, Any]) -> bool:
    """True when the scenario authenticates as the over-cap account.

    Those scenarios need an account already over its plan caps, which only the
    contract server's seeded third account provides.
    """
    return _scenario_uses_placeholder(sc, OVERCAP_KEY_PLACEHOLDER)


def scenario_needs_store(sc: dict[str, Any]) -> bool:
    setup = sc.get("setup") or []
    if any("inject_message" in s or "verify_domain" in s for s in setup):
        return True
    steps = sc.get("steps") or []
    if any(s.get("action") in STORE_ACTIONS for s in steps):
        return True
    return False


# ── Runner ────────────────────────────────────────────────────────


class Runner:
    def __init__(self, base_url: str, api_key: str, scenario: dict[str, Any]):
        self.base_url = base_url
        self.api_key = api_key
        self.scenario = scenario
        # One stable instant per scenario: request bodies and later
        # expectations must resolve to the exact same value, while never aging
        # into the past.
        future = (datetime.now(timezone.utc) + timedelta(minutes=5)).replace(
            microsecond=0
        )
        self.vars: dict[str, str] = {
            "future_rfc3339": future.isoformat().replace("+00:00", "Z"),
            "scenario_token": uuid.uuid4().hex[:12],
        }
        # Bound only when supplied: scenarios needing it skip otherwise, so an
        # empty bearer token can never reach the wire as a confusing 401.
        if CAPPED_API_KEY:
            self.vars["capped_api_key"] = CAPPED_API_KEY
        if OVERCAP_API_KEY:
            self.vars["overcap_api_key"] = OVERCAP_API_KEY
        self._http = httpx.Client(base_url=base_url, timeout=30)

    def close(self):
        self._http.close()

    def _raw(
        self,
        method: str,
        path: str,
        body: Any = None,
        *,
        content: bytes | None = None,
        headers: dict[str, Any] | None = None,
    ) -> httpx.Response:
        """Issue one request.

        `headers`, when given, REPLACES the default bearer auth (that is how
        auth-override steps send their own credential, or none at all).
        `content` sends bytes verbatim — no JSON encoder, no transcoding — and
        is mutually exclusive with `body`.
        """
        hdrs: dict[str, Any] = (
            dict(headers) if headers is not None else {"Authorization": f"Bearer {self.api_key}"}
        )
        if body is not None or content is not None:
            hdrs.setdefault("Content-Type", "application/json")
        if content is not None:
            return self._http.request(method, path, headers=hdrs, content=content)
        return self._http.request(method, path, headers=hdrs, json=body)

    def resolve(self, s: str) -> str:
        s = s.replace("{base_url}", self.base_url)
        s = s.replace("{api_key}", self.api_key)
        for k, v in self.vars.items():
            s = s.replace(f"{{{k}}}", v)
        return s

    def resolve_value(self, v: Any) -> Any:
        if isinstance(v, str):
            return self.resolve(v)
        if isinstance(v, list):
            return [self.resolve_value(item) for item in v]
        if isinstance(v, dict):
            return {k: self.resolve_value(val) for k, val in v.items()}
        return v

    def auth_override(self, step: dict[str, Any]) -> str | None:
        return step.get("auth_override") or self.scenario.get("auth_override")

    def has_auth_override(self, step: dict[str, Any]) -> bool:
        return self.auth_override(step) is not None

    def execute_setup(self) -> bool:
        """Returns True if scenario should be skipped (needs store access)."""
        setup = self.scenario.get("setup") or []
        for s in setup:
            if "inject_message" in s or "verify_domain" in s:
                return True

            if "register_domain" in s:
                domain = self.resolve(s["register_domain"])
                resp = self._raw("POST", "/v1/domains", {"domain": domain})
                if resp.status_code >= 400 and resp.status_code != 409:
                    resp.raise_for_status()
            elif "register_agent" in s:
                agent = s["register_agent"]
                email = self.resolve(agent["email"])
                resp = self._raw("POST", "/v1/agents", {"email": email})
                if resp.status_code >= 400 and resp.status_code != 409:
                    resp.raise_for_status()
                self.vars["agent_email"] = email
            else:
                raise ValueError(f"unknown setup step: {s!r}")

        return False

    def execute_steps(self):
        for step in self.scenario.get("steps", []):
            action = step["action"]
            if action == "request":
                self._exec_request(step)
            elif action == "inject_message":
                pytest.skip(f"step {step['id']}: inject_message not supported in Python runner")
            elif action in ("ws_connect", "ws_reconnect", "ws_read"):
                pytest.skip(f"step {step['id']}: WS actions require async runner")
            elif action == "verify_and_retry":
                pytest.skip(f"step {step['id']}: verify_and_retry not supported in Python runner")
            else:
                raise ValueError(f"step {step['id']}: unknown action {action}")

    def cleanup(self):
        errors: list[BaseException] = []
        for step in self.scenario.get("cleanup") or []:
            try:
                if step.get("action") != "request":
                    raise ValueError(
                        f"cleanup step {step['id']}: only request actions are supported"
                    )
                self._exec_request(step)
            except BaseException as exc:
                errors.append(exc)
        if errors:
            raise AssertionError(
                f"contract scenario cleanup failed ({len(errors)} error(s)): {errors[0]}"
            ) from errors[0]

    def _exec_request(self, step: dict[str, Any]):
        path = self.resolve(step["path"])
        body = self.resolve_value(step["body"]) if "body" in step else None
        content = step_raw_body(step)
        raw_headers = step_raw_headers(step)
        text_headers = {
            name: self.resolve(value)
            for name, value in (step.get("headers") or {}).items()
        }
        text_names = {name.lower() for name in text_headers}
        for name in raw_headers:
            if name.lower() in text_names:
                raise ValueError(
                    f"step {step['id']}: header {name} is declared in both headers and headers_base64"
                )
        ex = step.get("expect") or {}

        headers: dict[str, Any] | None = None
        if self.has_auth_override(step):
            # Auth-override scenarios bypass SDK auth by design.
            override = self.auth_override(step)
            headers = {} if override == "none" else {"Authorization": self.resolve(override)}
        if text_headers or raw_headers:
            if headers is None:
                headers = {"Authorization": f"Bearer {self.api_key}"}
            headers.update(text_headers)
            headers.update(raw_headers)

        resp = self._raw(step["method"], path, body, content=content, headers=headers)
        status = resp.status_code
        raw_body = resp.text
        data = None

        if "status" in ex:
            assert status == ex["status"], f"step {step['id']}: expected {ex['status']}, got {status}"

        # Raw-text assertion. Runs on the undecoded response and BEFORE the
        # JSON assertions, so it also applies to steps making no other body
        # claim. body_excludes is a top-level KEY check and cannot see a nested
        # leak (e.g. error.details.fields[0].value); this one is deliberately
        # not JSON-aware.
        for needle in ex.get("response_excludes", []):
            resolved = self.resolve(needle)
            assert resolved not in raw_body, (
                f"step {step['id']}: response contains {resolved!r} anywhere in "
                f"its body, which it must not: {raw_body}"
            )

        has_capture = bool(step.get("capture"))
        if not any(
            k in ex for k in ("body_contains", "body_excludes", "body_match", "body_array_contains")
        ) and not has_capture:
            return

        if data is None:
            data = json_mod.loads(raw_body)

        for key in ex.get("body_contains", []):
            resolved = self.resolve(key)
            assert resolved in data, f"step {step['id']}: body_contains {resolved}"

        for key in ex.get("body_excludes", []):
            resolved = self.resolve(key)
            assert resolved not in data, f"step {step['id']}: body_excludes {resolved}"

        if "body_match" in ex:
            for json_path, expected in ex["body_match"].items():
                resolved_path = self.resolve(json_path)
                # _MISSING, not None: json_path_get's default is returned for an
                # ABSENT path, so defaulting to None made an expected YAML
                # `null` pass on a field that simply was not there — while the
                # Go runner (jsonPathGet's `found` flag) and the TypeScript one
                # (undefined vs null) both failed it. A sentinel that equals
                # nothing keeps "absent" and "present and null" distinct, and
                # keeps all three runners answering the same way.
                actual = json_path_get(data, resolved_path, _MISSING)
                assert actual is not _MISSING, (
                    f"step {step['id']}: body_match {resolved_path} not found in response: {raw_body}"
                )
                resolved_expected = self.resolve_value(expected)
                assert values_equal(actual, resolved_expected), (
                    f"step {step['id']}: body_match {resolved_path} = {actual!r}, want {resolved_expected!r}"
                )

        for json_path, expected_fields in ex.get("body_array_contains", {}).items():
            resolved_path = self.resolve(json_path)
            items = json_path_get(data, resolved_path, _MISSING)
            assert isinstance(items, list), (
                f"step {step['id']}: body_array_contains {resolved_path} is not an array"
            )
            resolved_fields = self.resolve_value(expected_fields)
            assert any(
                isinstance(item, dict)
                and all(
                    values_equal(json_path_get(item, field, _MISSING), expected)
                    for field, expected in resolved_fields.items()
                )
                for item in items
            ), f"step {step['id']}: body_array_contains {resolved_path} has no matching item"

        for name, src_path in (step.get("capture") or {}).items():
            resolved_path = self.resolve(src_path)
            value = json_path_get(data, resolved_path, _MISSING)
            assert value is not _MISSING, (
                f"step {step['id']}: capture path {resolved_path} not found in response"
            )
            if value is None:
                self.vars[name] = "null"
            elif isinstance(value, bool):
                self.vars[name] = str(value).lower()
            else:
                self.vars[name] = str(value)


# ── Test entry point ──────────────────────────────────────────────


requires_contract_server = pytest.mark.skipif(
    not BASE_URL or not API_KEY,
    reason="E2A_TEST_BASE_URL and E2A_TEST_API_KEY required for contract tests",
)


def test_generated_message_lifecycle_page_parses_canonical_contract():
    page = PageMessageLifecycleTransition.from_dict(
        {
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
            "next_cursor": None,
        }
    )

    assert page is not None
    assert page.items[0].recipient is None
    assert page.items[0].evidence["nested"] == {"future": True}
    assert page.items[0].correlation_ids["future_id"] == "future_1"
    assert page.items[1].recipient is None
    assert page.items[1].reconstructed is True
    assert page.items[1].reason_code == "delivery.recipient_server_accepted"


def _scenario_ids():
    if not SCENARIOS_PATH.exists():
        return []
    return [sc["name"] for sc in load_scenarios()]


def _scenario_by_name(name: str) -> dict[str, Any]:
    for sc in load_scenarios():
        if sc["name"] == name:
            return sc
    raise ValueError(f"scenario {name!r} not found")


def run_runner(runner: Runner) -> None:
    primary: BaseException | None = None
    traceback = None
    try:
        skipped = runner.execute_setup()
        if not skipped:
            runner.execute_steps()
    except BaseException as exc:
        primary = exc
        traceback = exc.__traceback__
    try:
        runner.cleanup()
    except BaseException:
        if primary is None:
            raise
    if primary is not None:
        raise primary.with_traceback(traceback)


def test_runner_captures_response_values_for_later_paths(monkeypatch):
    scenario = {
        "name": "capture",
        "description": "capture parity",
        "steps": [
            {
                "id": "create",
                "action": "request",
                "method": "POST",
                "path": "/messages",
                "expect": {"status": 202},
                "capture": {"message_id": "message_id"},
            },
            {
                "id": "read",
                "action": "request",
                "method": "GET",
                "path": "/messages/{message_id}/lifecycle",
                "expect": {"status": 200},
            },
        ],
    }
    paths: list[str] = []
    responses = iter(
        [
            httpx.Response(202, json={"message_id": "msg_captured"}),
            httpx.Response(200, json={"items": []}),
        ]
    )
    runner = Runner("https://contract.test", "key", scenario)

    def fake_raw(method: str, path: str, body: Any = None, **kwargs: Any) -> httpx.Response:
        del method, body, kwargs
        paths.append(path)
        return next(responses)

    monkeypatch.setattr(runner, "_raw", fake_raw)
    try:
        runner.execute_steps()
    finally:
        runner.close()

    assert paths == ["/messages", "/messages/msg_captured/lifecycle"]


def test_runner_resolves_future_rfc3339_once_per_scenario():
    before = datetime.now(timezone.utc)
    runner = Runner(
        "https://contract.test",
        "key",
        {
            "name": "future_timestamp",
            "description": "dynamic timestamp parity",
            "steps": [],
        },
    )
    try:
        first = runner.resolve("{future_rfc3339}")
        second = runner.resolve("{future_rfc3339}")
    finally:
        runner.close()

    parsed = datetime.fromisoformat(first.replace("Z", "+00:00"))
    assert second == first
    assert before + timedelta(minutes=4) <= parsed
    assert parsed <= datetime.now(timezone.utc) + timedelta(minutes=6)
    assert first.endswith("Z")
    assert parsed.microsecond == 0


def test_runner_assigns_unique_lowercase_hex_token_per_scenario():
    first = Runner(
        "https://contract.test",
        "key",
        {"name": "first", "description": "unique dynamic placeholder", "steps": []},
    )
    second = Runner(
        "https://contract.test",
        "key",
        {"name": "second", "description": "unique dynamic placeholder", "steps": []},
    )
    try:
        first_token = first.resolve("{scenario_token}")
        assert re.fullmatch(r"[0-9a-f]{12}", first_token)
        assert second.resolve("{scenario_token}") != first_token
    finally:
        first.close()
        second.close()


def test_runner_substitutes_stable_per_run_token_into_text_headers(monkeypatch):
    scenario = {
        "name": "dynamic_header",
        "description": "text headers support placeholders",
        "steps": [
            {
                "id": "delete",
                "action": "request",
                "method": "DELETE",
                "path": "/v1/domains/dynamic.test?confirm=DELETE",
                "headers": {"Idempotency-Key": "domain-delete-{scenario_token}"},
                "expect": {"status": 200},
            }
        ],
    }
    runner = Runner("https://contract.test", "key", scenario)
    received = ""

    def fake_raw(method: str, path: str, body: Any = None, **kwargs: Any) -> httpx.Response:
        nonlocal received
        del method, path, body
        received = kwargs["headers"]["Idempotency-Key"]
        return httpx.Response(200, json={})

    monkeypatch.setattr(runner, "_raw", fake_raw)
    try:
        runner.execute_steps()
    finally:
        runner.close()

    assert re.fullmatch(r"domain-delete-[0-9a-f]{12}", received)


def test_domain_crud_is_runnable_without_store_access():
    scenario = _scenario_by_name("domain_crud")
    assert scenario["setup"] == [{"register_domain": "domain-crud.test.dev"}]
    assert not scenario_needs_store(scenario)


def test_execute_setup_registers_domain_then_agent(monkeypatch):
    scenario = {
        "name": "setup_domain_and_agent",
        "description": "known setup keys still dispatch",
        "setup": [
            {"register_domain": "setup.test.dev"},
            {"register_agent": {"email": "agent@setup.test.dev"}},
        ],
        "steps": [],
    }
    runner = Runner("https://contract.test", "key", scenario)
    calls: list[tuple[str, str]] = []

    def fake_raw(method: str, path: str, body: Any = None, **kwargs: Any) -> httpx.Response:
        del body, kwargs
        calls.append((method, path))
        return httpx.Response(200, json={})

    monkeypatch.setattr(runner, "_raw", fake_raw)
    try:
        skipped = runner.execute_setup()
    finally:
        runner.close()

    assert skipped is False
    assert calls == [("POST", "/v1/domains"), ("POST", "/v1/agents")]
    assert runner.vars["agent_email"] == "agent@setup.test.dev"


def test_execute_setup_rejects_unrecognized_key():
    scenario = {
        "name": "setup_typo",
        "description": "a typo'd setup key must not silently no-op",
        "setup": [{"regsiter_domain": "typo.test.dev"}],
        "steps": [],
    }
    runner = Runner("https://contract.test", "key", scenario)
    try:
        with pytest.raises(ValueError, match="unknown setup step"):
            runner.execute_setup()
    finally:
        runner.close()


def test_runner_cleanup_preserves_primary_failure_and_runs_every_request(monkeypatch):
    scenario = {
        "name": "failure_cleanup",
        "description": "cleanup survives primary and cleanup failures",
        "steps": [
            {
                "id": "fail",
                "action": "request",
                "method": "GET",
                "path": "/fail",
                "expect": {"status": 200},
            }
        ],
        "cleanup": [
            {
                "id": "cleanup_agent",
                "action": "request",
                "method": "DELETE",
                "path": "/cleanup-agent",
                "expect": {"status": 200},
            },
            {
                "id": "cleanup_domain",
                "action": "request",
                "method": "DELETE",
                "path": "/cleanup-domain",
                "expect": {"status": 200},
            },
        ],
    }
    paths: list[str] = []
    responses = iter(
        [
            httpx.Response(500, text="primary"),
            httpx.Response(500, text="cleanup"),
            httpx.Response(200),
        ]
    )
    runner = Runner("https://contract.test", "key", scenario)

    def fake_raw(method: str, path: str, body: Any = None, **kwargs: Any) -> httpx.Response:
        del method, body, kwargs
        paths.append(path)
        return next(responses)

    monkeypatch.setattr(runner, "_raw", fake_raw)
    try:
        with pytest.raises(AssertionError, match="step fail"):
            run_runner(runner)
    finally:
        runner.close()

    assert paths == ["/fail", "/cleanup-agent", "/cleanup-domain"]


def test_runner_surfaces_cleanup_failure_without_primary_failure(monkeypatch):
    scenario = {
        "name": "cleanup_failure",
        "description": "cleanup failure is visible",
        "steps": [],
        "cleanup": [
            {
                "id": "cleanup",
                "action": "request",
                "method": "DELETE",
                "path": "/cleanup",
                "expect": {"status": 200},
            }
        ],
    }
    runner = Runner("https://contract.test", "key", scenario)
    monkeypatch.setattr(
        runner,
        "_raw",
        lambda method, path, body=None, **kwargs: httpx.Response(500, text="cleanup"),
    )
    try:
        with pytest.raises(AssertionError, match="contract scenario cleanup failed"):
            run_runner(runner)
    finally:
        runner.close()


# ── Raw-bytes escape hatches (raw_body_base64 / headers_base64) ───
#
# Runner-level regressions, mirrored in tests/contract/runner_rawbytes_test.go
# and sdks/typescript/test/v1/contract.test.ts.

RAW_INVALID_BODY = b'{"address":"a\xffb@example.com"}'
RAW_INVALID_HEADER = b"k\xffz"


def _is_valid_utf8(data: bytes) -> bool:
    try:
        data.decode("utf-8")
    except UnicodeDecodeError:
        return False
    return True


def test_runner_sends_raw_bytes_to_the_wire_verbatim():
    """A real socket, not a stub: the question is whether httpx preserves bytes
    >= 0x80, which only the wire can answer. It does — for HEADERS only when
    the value is ``bytes``; a ``str`` raises UnicodeEncodeError('ascii')."""
    import socketserver
    import threading
    from http.server import BaseHTTPRequestHandler

    captured: dict[str, Any] = {}

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler API
            length = int(self.headers.get("Content-Length", "0"))
            captured["body"] = self.rfile.read(length)
            # http.client parses header values as latin1, so encoding back
            # recovers the exact received bytes.
            captured["header"] = self.headers.get("Idempotency-Key", "").encode("latin-1")
            captured["content_type"] = self.headers.get("Content-Type", "")
            payload = b'{"error":{"code":"invalid_request"}}'
            self.send_response(400)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *args):  # silence the default stderr logging
            pass

    server = socketserver.TCPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        scenario = {
            "name": "raw_bytes",
            "description": "raw bytes reach the wire unmodified",
            "steps": [
                {
                    "id": "raw",
                    "action": "request",
                    "method": "POST",
                    "path": "/v1/contacts",
                    "raw_body_base64": base64.b64encode(RAW_INVALID_BODY).decode(),
                    "headers_base64": {
                        "Idempotency-Key": base64.b64encode(RAW_INVALID_HEADER).decode()
                    },
                    "expect": {
                        "status": 400,
                        "body_match": {"error.code": "invalid_request"},
                    },
                }
            ],
        }
        host, port = server.server_address[0], server.server_address[1]
        runner = Runner(f"http://{host}:{port}", "key", scenario)
        try:
            run_runner(runner)
        finally:
            runner.close()
    finally:
        server.shutdown()
        server.server_close()

    assert captured["body"] == RAW_INVALID_BODY
    assert captured["header"] == RAW_INVALID_HEADER
    assert captured["content_type"] == "application/json"
    # Non-vacuity: if the transport silently "fixed" the bytes, the test would
    # prove nothing about the rule it exists to cover.
    assert not _is_valid_utf8(captured["body"])
    assert not _is_valid_utf8(captured["header"])


def test_runner_rejects_step_with_both_body_and_raw_body_base64():
    with pytest.raises(ValueError, match="mutually exclusive"):
        step_raw_body(
            {
                "id": "both",
                "body": {"address": "a@example.com"},
                "raw_body_base64": base64.b64encode(b"{}").decode(),
            }
        )


@pytest.mark.parametrize(
    "label,value",
    [
        ("non-alphabet characters", "not base64!!"),
        ("missing padding", "eyJhIjoxfQ"),
        ("embedded whitespace", "eyJhIjox\nfQ=="),
        ("present but empty", ""),
    ],
)
def test_runner_rejects_bad_raw_body_base64(label, value):
    """Parity contract with the Go and TypeScript runners.

    A fixture that is not exactly-canonical base64, or is present but empty,
    must fail LOUDLY in all three languages. The failure this guards against is
    worse than a wrong test: Node's ``Buffer.from(x, "base64")`` drops
    non-alphabet characters instead of erroring, so a typo'd fixture used to
    send DIFFERENT BYTES in the TypeScript runner while these two reported a
    clean error — one scenario quietly testing something else.
    """
    with pytest.raises((ValueError, binascii.Error)):
        step_raw_body({"id": "bad", "raw_body_base64": value})


def test_runner_treats_absent_raw_body_base64_as_no_body():
    assert step_raw_body({"id": "none"}) is None


def test_runner_rejects_bad_headers_base64():
    with pytest.raises((ValueError, binascii.Error)):
        step_raw_headers({"id": "bad", "headers_base64": {"Idempotency-Key": "not base64!!"}})


# ── response_excludes ─────────────────────────────────────────────
#
# A RAW-TEXT check over the whole response, not the top-level key check
# body_excludes performs. The distinction is the entire reason the key exists:
# the leak it must catch — a server echoing the offending input back inside an
# error envelope — sits at a nested path where body_excludes is vacuously
# satisfied.

_ECHO_LEAK = json_mod.dumps(
    {
        "error": {
            "code": "invalid_request",
            "details": {"fields": [{"location": "body", "value": "a\ufffdb@example.com"}]},
        }
    }
)


def _run_echo_step(monkeypatch, expect: dict[str, Any]) -> None:
    scenario = {
        "name": "leak",
        "description": "echo check",
        "steps": [
            {
                "id": "leak",
                "action": "request",
                "method": "GET",
                "path": "/v1/contacts",
                "expect": expect,
            }
        ],
    }
    runner = Runner("https://contract.test", "key", scenario)
    monkeypatch.setattr(
        runner,
        "_raw",
        lambda method, path, body=None, **kwargs: httpx.Response(400, text=_ECHO_LEAK),
    )
    try:
        run_runner(runner)
    finally:
        runner.close()


def test_body_excludes_passes_on_a_nested_leak(monkeypatch):
    """The weakness being fixed: body_excludes is satisfied by any error
    envelope, because an envelope has no top-level `address` key either way."""
    _run_echo_step(monkeypatch, {"status": 400, "body_excludes": ["address"]})


def test_response_excludes_catches_a_nested_leak(monkeypatch):
    with pytest.raises(AssertionError, match="anywhere in"):
        _run_echo_step(monkeypatch, {"status": 400, "response_excludes": ["example.com"]})


def test_response_excludes_passes_when_absent_with_no_other_body_claim(monkeypatch):
    _run_echo_step(monkeypatch, {"status": 400, "response_excludes": ["not-in-the-response"]})


# ── body_match: absent vs present-and-null ────────────────────────


def test_body_match_null_does_not_pass_on_an_absent_field(monkeypatch):
    """Regression for a Python-only vacuity: json_path_get's default was None,
    so an expected YAML `null` was satisfied by a field that simply was not
    there. Go (jsonPathGet's `found` flag) and TypeScript (undefined vs null)
    both failed that case, so a scenario asserting `null` was a real assertion
    in two runners and a no-op in the third."""
    scenario = {
        "name": "null_match",
        "description": "absent is not null",
        "steps": [
            {
                "id": "probe",
                "action": "request",
                "method": "GET",
                "path": "/v1/contacts",
                "expect": {"status": 200, "body_match": {"missing_field": None}},
            }
        ],
    }
    runner = Runner("https://contract.test", "key", scenario)
    monkeypatch.setattr(
        runner,
        "_raw",
        lambda method, path, body=None, **kwargs: httpx.Response(200, text='{"present":1}'),
    )
    try:
        with pytest.raises(AssertionError, match="not found in response"):
            run_runner(runner)
    finally:
        runner.close()


def test_body_match_null_still_passes_on_a_present_null(monkeypatch):
    """Control for the test above: the sentinel must separate ABSENT from
    present-and-null, not reject null outright."""
    scenario = {
        "name": "null_match_ok",
        "description": "present null matches null",
        "steps": [
            {
                "id": "probe",
                "action": "request",
                "method": "GET",
                "path": "/v1/contacts",
                "expect": {"status": 200, "body_match": {"nullable": None}},
            }
        ],
    }
    runner = Runner("https://contract.test", "key", scenario)
    monkeypatch.setattr(
        runner,
        "_raw",
        lambda method, path, body=None, **kwargs: httpx.Response(200, text='{"nullable":null}'),
    )
    try:
        run_runner(runner)
    finally:
        runner.close()


def test_httpx_refuses_non_ascii_str_header_so_raw_bytes_are_required():
    """Pins the reason headers_base64 decodes to BYTES rather than a str: httpx
    ASCII-encodes str header values, so the str form cannot express this at
    all. If httpx ever relaxes that, this test tells us the constraint moved."""
    with pytest.raises(UnicodeEncodeError):
        httpx.Headers({"Idempotency-Key": RAW_INVALID_HEADER.decode("latin-1")})
    assert httpx.Headers({"Idempotency-Key": RAW_INVALID_HEADER}).raw[0][1] == RAW_INVALID_HEADER


def test_invalid_utf8_scenario_carries_genuinely_ill_formed_bytes():
    scenario = _scenario_by_name("invalid_utf8_rejected")
    steps = {step["id"]: step for step in scenario["steps"]}

    body = step_raw_body(steps["body_rejected"])
    assert body, "body_rejected must declare raw_body_base64"
    assert not _is_valid_utf8(body), "body_rejected payload must be ill-formed UTF-8"
    # The offending-bytes echo check must be the whole-response form; a
    # top-level body_excludes would be vacuous on an error envelope.
    assert steps["body_rejected"]["expect"].get("response_excludes"), (
        "body_rejected must declare response_excludes"
    )

    header = step_raw_headers(steps["header_rejected"])["Idempotency-Key"]
    assert not _is_valid_utf8(header), "header_rejected value must be ill-formed UTF-8"

    # Self-cleaning by construction: every step is a rejection (400) or a
    # not-created probe (404), so nothing exists to delete.
    assert "cleanup" not in scenario
    assert {step["expect"]["status"] for step in scenario["steps"]} == {400, 404}


def test_managed_unsubscribe_scenario_is_self_cleaning_and_lifecycle_observable():
    scenario = _scenario_by_name("agent_suppression_and_managed_unsubscribe")
    steps = {step["id"]: step for step in scenario["steps"]}

    assert steps["managed_unsubscribe_send_held"]["capture"] == {
        "managed_message_id": "message_id"
    }
    lifecycle = steps["get_managed_message_lifecycle"]
    assert "{managed_message_id}/lifecycle" in lifecycle["path"]
    assert lifecycle["expect"]["body_array_contains"] == {
        "items": {
            "message_id": "{managed_message_id}",
            "direction": "outbound",
            "stage": "review",
            "outcome": "pending",
            "reason_code": "review.hold_created",
            "retryable": False,
            "reconstructed": False,
        }
    }
    assert "delete_agent_permanently" not in {step["id"] for step in scenario["steps"]}
    assert [step["id"] for step in scenario["cleanup"]] == [
        "delete_agent_permanently",
        "delete_domain",
    ]


def test_scheduled_send_scenario_is_self_cleaning_and_projection_complete():
    scenario = _scenario_by_name("scheduled_send_fields")
    steps = {step["id"]: step for step in scenario["steps"]}

    assert scenario["setup"][0]["register_agent"]["email"] == (
        "scheduled-contract-{scenario_token}@agents.e2a.dev"
    )
    assert steps["schedule_send"]["body"]["send_at"] == "{future_rfc3339}"
    assert steps["schedule_send"]["expect"]["body_match"] == {
        "status": "scheduled",
        "scheduled_at": "{future_rfc3339}",
    }
    assert steps["schedule_send"]["capture"] == {
        "scheduled_message_id": "message_id"
    }
    assert steps["get_scheduled_message"]["expect"]["body_match"] == {
        "id": "{scheduled_message_id}",
        "delivery_status": "accepted",
        "scheduled_at": "{future_rfc3339}",
    }
    assert steps["list_scheduled_message"]["expect"]["body_array_contains"] == {
        "items": {
            "id": "{scheduled_message_id}",
            "delivery_status": "accepted",
            "scheduled_at": "{future_rfc3339}",
        }
    }
    assert steps["trash_scheduled_message"]["expect"]["body_match"] == {
        "deleted": True,
        "id": "{scheduled_message_id}",
    }
    assert steps["restore_before_scheduled_at"]["expect"]["body_match"] == {
        "id": "{scheduled_message_id}",
        "delivery_status": "accepted",
        "scheduled_at": "{future_rfc3339}",
    }
    assert [step["id"] for step in scenario["cleanup"]] == [
        "delete_scheduled_agent_permanently"
    ]


def test_limits_scenario_shape():
    """Always-on guard for the quota scenario.

    The live quota run skips without a capped key (a deployed target has no
    capped account), so this shape test is what keeps that skip from decaying
    into zero coverage. It fails if the scenario is deleted, stops using the
    capped account, or loses either direction of enforcement.
    """
    scenario = _scenario_by_name("account_limits_enforced")
    assert scenario_needs_capped_account(scenario)

    steps = {step["id"]: step for step in scenario["steps"]}

    # Refused AT the cap, with the machine-readable envelope an SDK needs to
    # say WHICH quota stopped the caller — including the upgrade affordance,
    # which is omitempty and so vanishes silently if the server drops it.
    refused = steps["second_domain_hits_the_cap"]["expect"]
    assert refused["status"] == 402
    assert refused["body_match"]["error.code"] == "limit_exceeded"
    assert refused["body_match"]["error.details.resource"] == "domains"
    assert refused["body_match"]["error.details.limit"] == 1
    assert refused["body_match"]["error.details.current"] == 1
    assert refused["body_match"]["error.details.plan_code"] == "contract_capped"
    assert (
        refused["body_match"]["error.details.upgrade_url"]
        == "https://e2a.dev/upgrade"
    )

    # A second resource with a DIFFERENT cap: no single hardcoded number can
    # satisfy both refusals.
    agent_refused = steps["third_agent_hits_the_cap"]["expect"]
    assert agent_refused["status"] == 402
    assert agent_refused["body_match"]["error.details.resource"] == "agents"
    assert agent_refused["body_match"]["error.details.limit"] == 2
    assert agent_refused["body_match"]["error.details.current"] == 2

    # ...and allowed when it should be. Without these, a server that 402'd
    # unconditionally would satisfy every assertion above.
    assert steps["reregister_owned_domain_at_cap_is_allowed"]["expect"]["status"] == 201
    assert (
        steps["agent_create_succeeds_again_after_freeing_a_slot"]["expect"]["status"]
        == 201
    )

    # Cleanup must remove EVERY agent the scenario can create, including the
    # one the happy path deletes mid-scenario: steps stop at the first failure,
    # so a failure in between would otherwise strand it and put the next run at
    # its cap on an early step.
    assert [step["id"] for step in scenario["cleanup"]] == [
        "delete_agent_1",
        "delete_agent_2",
        "delete_agent_3",
        "delete_domain",
    ]


@pytest.fixture(params=_scenario_ids() if SCENARIOS_PATH.exists() else [])
def scenario(request):
    return _scenario_by_name(request.param)


@requires_contract_server
def test_contract_scenario(scenario):
    if scenario_needs_store(scenario):
        pytest.skip(f"scenario {scenario['name']}: requires store access (inject_message/verify_domain)")
    # No capped account on this target (e.g. a deployed server) → no reachable
    # cap. The Go runner always has one, and test_limits_scenario_shape below
    # runs everywhere, so this skip cannot silently become zero coverage.
    if scenario_needs_capped_account(scenario) and not CAPPED_API_KEY:
        pytest.skip(f"scenario {scenario['name']}: needs E2A_TEST_CAPPED_API_KEY")
    if scenario_needs_overcap_account(scenario) and not OVERCAP_API_KEY:
        pytest.skip(f"scenario {scenario['name']}: needs E2A_TEST_OVERCAP_API_KEY")

    runner = Runner(BASE_URL, API_KEY, scenario)
    try:
        run_runner(runner)
    finally:
        runner.close()


# ── High-level sync client contract tests ─────────────────────────
#
# The scenario runner above drives the server over raw HTTP by design, so the
# ergonomic client's wrapper-only features (wait="sent", unsubscribe kwarg)
# need their own live-server coverage here.
#
# Contract-server send topology (cmd/e2a-contract-server): the real River
# enqueuer is wired but its outbound worker is not started, so external sends
# can prove accepted/scheduled queue contracts without submitting real mail.
# The deterministic terminal path is the self-send LOOPBACK, which delivers
# synchronously — wait="sent" on it observes status="sent" immediately rather
# than polling to the server-side ceiling.


def _slug(prefix: str) -> str:
    """Shared-domain slug satisfying the server's ^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$
    rule (2-40 chars, no underscores)."""
    return f"{prefix}-{uuid.uuid4().hex[:8]}"


@requires_contract_server
def test_client_send_wait_sent_returns_terminal_loopback_result():
    with E2AClient(API_KEY, base_url=BASE_URL) as client:
        email = f"{_slug('sdkc-wait')}@agents.e2a.dev"
        client.agents.create({"email": email})
        try:
            res = client.messages.send(
                email,
                {"to": [email], "subject": "wait contract", "text": "self-send loopback"},
                wait="sent",
            )
            assert res.status == "sent"
            assert res.message_id.startswith("msg_")
            assert res.method == "loopback"
        finally:
            # The Python wrapper exposes only the soft (trash) delete; the
            # contract DB is truncated on server start, so trash is enough.
            client.agents.delete(email)


@requires_contract_server
def test_client_send_managed_unsubscribe_is_accepted_and_held():
    # Managed unsubscribe requires exactly one non-self recipient, and the
    # contract server cannot deliver externally (no outbound worker) — so the
    # deterministic accepted shape is the review hold, mirroring the
    # managed_unsubscribe_send_held step of the Go/TS scenario runner.
    with E2AClient(API_KEY, base_url=BASE_URL) as client:
        email = f"{_slug('sdkc-unsub')}@agents.e2a.dev"
        client.agents.create({"email": email})
        try:
            client.agents.replace_protection(
                email,
                {
                    "inbound": {"gate": {}, "scan": {}},
                    "outbound": {
                        "gate": {"policy": "allowlist", "action": "review", "allowlist": []},
                        "scan": {},
                    },
                    "holds": {},
                },
            )
            res = client.messages.send(
                email,
                {
                    "to": ["alice@example.com"],
                    "subject": "managed unsubscribe contract",
                    "text": "held for review",
                },
                unsubscribe={"mode": "managed"},
            )
            assert res.status == "pending_review"
            assert res.message_id.startswith("msg_")
        finally:
            client.agents.delete(email)
