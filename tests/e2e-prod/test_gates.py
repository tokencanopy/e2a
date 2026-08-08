"""Unit tests for the coverage gates, run as SUBPROCESSES against
synthetic shard directories in a temp dir.

Nothing is imported from the gates themselves: each test drives the real CLI
contract (`python3 <gate>.py --reports … --openapi … --target-dir …`) so the
tests keep passing only while the gates keep behaving the way the suites and
CI rely on — fail closed on a coverage gap (exit 1), pass on full coverage
(exit 0). Hermetic and offline: the only repo file read is api/openapi.yaml
(the drift-gated SSOT the gates themselves consume).

Run: python3 -m unittest test_gates -v   (from tests/e2e-prod/)
"""
from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
OPENAPI = (HERE / ".." / ".." / "api" / "openapi.yaml").resolve()

# The 11 contacts/outreach operationIds suite 30 was added to cover.
CONTACTS_OPS = {
    "createContact",
    "listContacts",
    "getContact",
    "updateContact",
    "deleteContact",
    "importContacts",
    "deleteImportBatch",
    "listEngagements",
    "getEngagement",
    "upsertEngagement",
    "deleteEngagement",
}

# Mirrors of the gates' own allowlist tiers for a NON-production target (the
# synthetic target shard below is staging). The gates fail loudly if these
# drift out of sync with the real constants — which is the point: the tests
# pin the documented tiers, they don't weaken them.
COVERAGE_GATE_ALWAYS_ALLOWLIST = {"deleteAccount"}
COVERAGE_GATE_STAGING_ONLY_ALLOWLIST = {"deleteSuppression"}

EVENT_GATE_ALWAYS_ALLOWLIST = {"domain.sending_failed"}
EVENT_GATE_STAGING_ONLY_ALLOWLIST = {
    "email.delivered",
    "email.bounced",
    "email.complained",
    "domain.suppression_added",
    "agent.suppression_added",
    "domain.sending_verified",
}

# mcp_coverage_gate.py refuses to run when its allowlist names a tool the
# shard does not advertise, so every synthetic advertisement must include them.
MCP_GATE_ALLOWLISTED_TOOLS = {
    "send_email",
    "approve_pending_message",
    "reject_pending_message",
    "delete_suppression",
}

# Synthetic target shard: staging, so the staging-only allowlist tiers apply.
STAGING_TARGET = {"apiUrl": "https://api-staging.e2a.dev", "isProd": False}


def load_ops() -> list[tuple[str, str, str]]:
    """(METHOD, path template, operationId) for every operation in the spec."""
    with open(OPENAPI) as f:
        spec = yaml.safe_load(f)
    ops = []
    for path, item in (spec.get("paths") or {}).items():
        for method, op in (item or {}).items():
            if isinstance(op, dict) and "operationId" in op:
                ops.append((method.upper(), path, op["operationId"]))
    return ops


def concrete_path(template: str) -> str:
    """Substitute every {param} segment with a concrete value — match_op only
    needs a non-empty segment in the right position."""
    return "/".join("x" if seg.startswith("{") and seg.endswith("}") else seg for seg in template.split("/"))


def load_event_types() -> set[str]:
    with open(OPENAPI) as f:
        spec = yaml.safe_load(f)
    return set(spec["components"]["schemas"]["CreateWebhookRequest"]["properties"]["events"]["items"]["enum"])


def run_gate(script: str, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(HERE / script), *args],
        capture_output=True,
        text=True,
    )


def write_shard(directory: Path, name: str, payload: object) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    with open(directory / name, "w") as f:
        json.dump(payload, f)


class CoverageGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        root = Path(self._tmp.name)
        self.reports = root / "coverage"
        self.target = root / "target"
        self.reports.mkdir()
        write_shard(self.target, "1.json", STAGING_TARGET)
        allowlisted = COVERAGE_GATE_ALWAYS_ALLOWLIST | COVERAGE_GATE_STAGING_ONLY_ALLOWLIST
        # The full "METHOD /concrete/path" set a perfect run would record,
        # minus the documented allowlist tiers, paired with the operationId
        # each pair covers (so tests can subtract by operationId).
        self.full = sorted(
            (f"{method} {concrete_path(path)}", opid)
            for method, path, opid in load_ops()
            if opid not in allowlisted
        )
        self.full_pairs = [pair for pair, _ in self.full]
        self.args = (
            "--openapi", str(OPENAPI),
            "--reports", str(self.reports),
            "--target-dir", str(self.target),
        )

    def test_missing_contacts_ops_fail_closed(self) -> None:
        covered = [pair for pair, opid in self.full if opid not in CONTACTS_OPS]
        write_shard(self.reports, "1.json", covered)
        proc = run_gate("coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        for opid in CONTACTS_OPS:
            self.assertIn(opid, proc.stdout, f"gate output must name uncovered {opid}")

    def test_full_coverage_passes(self) -> None:
        write_shard(self.reports, "1.json", self.full_pairs)
        proc = run_gate("coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)


class EventCoverageGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        root = Path(self._tmp.name)
        self.reports = root / "event-coverage"
        self.target = root / "target"
        self.reports.mkdir()
        write_shard(self.target, "1.json", STAGING_TARGET)
        allowlisted = EVENT_GATE_ALWAYS_ALLOWLIST | EVENT_GATE_STAGING_ONLY_ALLOWLIST
        self.required = load_event_types() - allowlisted
        self.assertIn("contact.due", self.required, "contact.due must be a REQUIRED type on a staging target")
        self.args = (
            "--openapi", str(OPENAPI),
            "--reports", str(self.reports),
            "--target-dir", str(self.target),
        )

    def test_missing_contact_due_fails_closed(self) -> None:
        write_shard(self.reports, "1.json", sorted(self.required - {"contact.due"}))
        proc = run_gate("event_coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        self.assertIn("contact.due", proc.stdout)

    def test_all_required_types_verified_passes(self) -> None:
        write_shard(self.reports, "1.json", sorted(self.required))
        proc = run_gate("event_coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)


def _has_jsonschema() -> bool:
    try:
        import jsonschema  # noqa: F401

        return True
    except ImportError:
        return False


@unittest.skipUnless(_has_jsonschema(), "response_schema_gate needs jsonschema (pip install jsonschema)")
class ResponseSchemaGateTest(unittest.TestCase):
    """Drives response_schema_gate.py with synthetic samples against the real
    spec. The violating cases exist to prove the gate can actually FAIL — a
    validator that never rejects is worse than none."""

    # getAccount has no documented 401, so a 401 sample validates against the
    # op's `default` → ErrorEnvelope (error.code/message/request_id required).
    VALID_ERROR = {"error": {"code": "unauthorized", "message": "no", "request_id": "req_1"}}

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.reports = Path(self._tmp.name) / "response-samples"
        self.args = ("--openapi", str(OPENAPI), "--reports", str(self.reports))

    def sample(self, status: int, body: object, kind: str = "json") -> dict:
        s = {"method": "GET", "path": "/v1/account", "status": status, "contentType": "application/json", "kind": kind}
        if kind == "json":
            s["body"] = body
        return s

    def test_conformant_samples_pass(self) -> None:
        write_shard(self.reports, "1.json", [self.sample(401, self.VALID_ERROR)])
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertIn("GATE: PASS", proc.stdout)

    def test_missing_required_field_fails_closed(self) -> None:
        # error.request_id is required by ErrorBody — dropping it must fail.
        body = {"error": {"code": "unauthorized", "message": "no"}}
        write_shard(self.reports, "1.json", [self.sample(401, body)])
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        self.assertIn("getAccount", proc.stdout)
        self.assertIn("request_id", proc.stdout)

    def test_wrong_type_fails_closed(self) -> None:
        body = {"error": {"code": 401, "message": "no", "request_id": "req_1"}}  # code must be a string
        write_shard(self.reports, "1.json", [self.sample(401, body)])
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        self.assertIn("getAccount", proc.stdout)

    def test_extra_fields_are_not_violations(self) -> None:
        # Responses are deliberately open (additionalProperties: true).
        body = {"error": {**self.VALID_ERROR["error"], "hint": "extra"}, "extra_top": 1}
        write_shard(self.reports, "1.json", [self.sample(401, body)])
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)

    def test_non_json_body_fails_closed(self) -> None:
        s = self.sample(401, None, kind="nonjson")
        s["rawPrefix"] = "<html>upstream error page</html>"
        write_shard(self.reports, "1.json", [s])
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        self.assertIn("non-JSON", proc.stdout)

    def test_no_shards_is_not_a_pass(self) -> None:
        proc = run_gate("response_schema_gate.py", *self.args)
        self.assertEqual(proc.returncode, 2, proc.stdout + proc.stderr)


class McpCoverageGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        root = Path(self._tmp.name)
        self.reports = root / "mcp-coverage"
        self.reports.mkdir()
        self.advertised = sorted(MCP_GATE_ALLOWLISTED_TOOLS | {"whoami", "create_contact"})
        self.args = ("--reports", str(self.reports))

    def test_uncovered_tool_fails_closed(self) -> None:
        write_shard(self.reports, "1.json", {"advertised": self.advertised, "covered": ["whoami"]})
        proc = run_gate("mcp_coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 1, proc.stdout + proc.stderr)
        self.assertIn("create_contact", proc.stdout + proc.stderr)

    def test_all_advertised_covered_passes(self) -> None:
        write_shard(
            self.reports,
            "1.json",
            {"advertised": self.advertised, "covered": ["whoami", "create_contact"]},
        )
        proc = run_gate("mcp_coverage_gate.py", *self.args)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)


if __name__ == "__main__":
    unittest.main()
