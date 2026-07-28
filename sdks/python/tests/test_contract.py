"""Python contract-test runner for the shared scenarios.yaml.

Runs against a live test server. Requires env vars:
  E2A_TEST_BASE_URL  — test server URL (e.g. http://localhost:8080)
  E2A_TEST_API_KEY   — valid API key for the test user

The runner drives the server over raw HTTP (a thin scenario interpreter, not
the ergonomic client):
- Each request step issues a raw bearer-authed httpx request to step.path
- Auth-override scenarios send their own Authorization header (by design)
- WS scenarios skipped in this sync runner (async WS tested separately)

Setup steps requiring direct store access (inject_message, verify_domain as
setup) cause the scenario to be skipped with a clear reason.
"""

from __future__ import annotations

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


def values_equal(json_val: Any, yaml_val: Any) -> bool:
    """Cross-type comparison (JSON number vs YAML int, bool, string)."""
    if isinstance(yaml_val, bool):
        return json_val is yaml_val or json_val == yaml_val
    if isinstance(yaml_val, (int, float)) and isinstance(json_val, (int, float)):
        return json_val == yaml_val
    return str(json_val) == str(yaml_val)


# ── Scenario determination ────────────────────────────────────────

STORE_ACTIONS = {"inject_message", "verify_and_retry"}


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
            "future_rfc3339": future.isoformat().replace("+00:00", "Z")
        }
        self._http = httpx.Client(base_url=base_url, timeout=30)

    def close(self):
        self._http.close()

    def _raw(self, method: str, path: str, body: Any = None) -> httpx.Response:
        headers = {"Authorization": f"Bearer {self.api_key}"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        return self._http.request(method, path, headers=headers, json=body)

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

            if "register_agent" in s:
                agent = s["register_agent"]
                email = self.resolve(agent["email"])
                resp = self._raw("POST", "/v1/agents", {"email": email})
                if resp.status_code >= 400 and resp.status_code != 409:
                    resp.raise_for_status()
                self.vars["agent_email"] = email

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
        ex = step.get("expect") or {}

        if self.has_auth_override(step):
            # Auth-override scenarios bypass SDK auth by design
            override = self.auth_override(step)
            headers: dict[str, str] = {}
            if override != "none":
                headers["Authorization"] = self.resolve(override)
            if body is not None:
                headers["Content-Type"] = "application/json"

            resp = self._http.request(
                step["method"], path,
                headers=headers,
                json=body if body is not None else None,
            )
            status = resp.status_code
            data = None
            raw_body = resp.text
        else:
            # Default auth — raw bearer request.
            resp = self._raw(step["method"], path, body)
            status = resp.status_code
            raw_body = resp.text
            data = None

        if "status" in ex:
            assert status == ex["status"], f"step {step['id']}: expected {ex['status']}, got {status}"

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
                actual = json_path_get(data, resolved_path)
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

    def fake_raw(method: str, path: str, body: Any = None) -> httpx.Response:
        del method, body
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

    def fake_raw(method: str, path: str, body: Any = None) -> httpx.Response:
        del method, body
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
        lambda method, path, body=None: httpx.Response(500, text="cleanup"),
    )
    try:
        with pytest.raises(AssertionError, match="contract scenario cleanup failed"):
            run_runner(runner)
    finally:
        runner.close()


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
    assert [step["id"] for step in scenario["cleanup"]] == [
        "delete_scheduled_agent_permanently"
    ]


@pytest.fixture(params=_scenario_ids() if SCENARIOS_PATH.exists() else [])
def scenario(request):
    return _scenario_by_name(request.param)


@requires_contract_server
def test_contract_scenario(scenario):
    if scenario_needs_store(scenario):
        pytest.skip(f"scenario {scenario['name']}: requires store access (inject_message/verify_domain)")

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
