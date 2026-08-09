#!/usr/bin/env python3
"""Static validation for the agentify framework — run from the agentify dir.

Catches the classes of bug unit selftests can't: YAML that won't parse, the
rendered config drifting from what the workflows read, inconsistent e2a URLs,
unfilled placeholders. (The e2a_mcp_url host bug is exactly check #3.)
"""
import glob
import re
import sys
from urllib.parse import urlparse

import yaml

fail = 0


def err(msg):
    global fail
    print(f"FAIL: {msg}")
    fail = 1


WORKFLOWS = glob.glob("templates/workflows/*.yml.tmpl")
CONFIGS = ["templates/autonomous-repo.config.yml.tmpl", "examples/e2a/autonomous-repo.config.yml"]

# 1. everything parses as YAML.
for f in WORKFLOWS + CONFIGS:
    try:
        yaml.safe_load(open(f))
    except Exception as e:  # noqa: BLE001
        err(f"{f} does not parse: {e}")

cfg = yaml.safe_load(open("examples/e2a/autonomous-repo.config.yml"))


def resolve(d, dotted):
    cur = d
    for p in dotted.split("."):
        if isinstance(cur, dict) and p in cur:
            cur = cur[p]
        else:
            return (False, None)
    return (True, cur)


# 2. the rendered (example) config has no unfilled {{PLACEHOLDER}}.
if re.search(r"\{\{[A-Z]", open("examples/e2a/autonomous-repo.config.yml").read()):
    err("example config still has an unfilled {{PLACEHOLDER}}")

# 3. the e2a MCP + REST URLs must share a host (the mcp.e2a.dev vs api.e2a.dev bug).
mcp = (cfg.get("comms") or {}).get("e2a_mcp_url", "")
api = (cfg.get("comms") or {}).get("e2a_api_url", "")
if mcp and api and urlparse(mcp).hostname != urlparse(api).hostname:
    err(f"e2a_mcp_url host '{urlparse(mcp).hostname}' != e2a_api_url host '{urlparse(api).hostname}'")
for url in (mcp, api):
    if url and not urlparse(url).scheme.startswith("http"):
        err(f"e2a url is not http(s): {url}")

# 4. required config keys are present.
REQUIRED = [
    "repo", "marker", "github_app_login", "reviewer",
    "comms.support_address", "comms.e2a_mcp_url", "comms.e2a_api_url", "fix_gate.mode",
    "fix_gate.approver", "labels.feedback", "labels.agent_fix", "labels.wontfix", "labels.ops",
    "labels.status_triaged", "labels.status_awaiting_approval", "labels.status_in_progress",
    "models.triage", "models.comms", "models.fix", "verify_setup_script",
]
for path in REQUIRED:
    ok, _ = resolve(cfg, path)
    if not ok:
        err(f"required config key missing: {path}")

# 5. every config key a workflow reads with yq must exist (unless it has a // default).
KEY_RE = re.compile(r"yq -?r? *'(\.[^']+)'")
for wf in WORKFLOWS:
    for m in KEY_RE.finditer(open(wf).read()):
        expr = m.group(1)
        optional = "//" in expr
        path = expr.split("//")[0].strip().lstrip(".")
        if not path:
            continue
        ok, _ = resolve(cfg, path)
        if not ok and not optional:
            err(f"{wf}: reads config key '.{path}' that is not in the example config")

print("agentify config validation: OK" if not fail else "agentify config validation: FAILED")
sys.exit(1 if fail else 0)
