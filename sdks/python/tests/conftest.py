import os

import pytest

from .resource_coverage_lib.discovery import assert_no_reflection_bugs
from .resource_coverage_lib.tracker import clear_reports

_LIVE = bool(
    os.environ.get("E2A_TEST_BASE_URL")
    and os.environ.get("E2A_TEST_API_KEY")
    and os.environ.get("E2A_TEST_AGENT_EMAIL")
)

# Cheap (no I/O, no credentials needed) sanity check that the resource-walk
# heuristic in discovery.py hasn't silently broken — runs on every collection,
# live or offline, so a refactor that breaks it fails fast instead of the gate
# quietly reporting a shrunken denominator.
assert_no_reflection_bugs()


@pytest.fixture(scope="session", autouse=True)
def _reset_resource_coverage_reports():
    """Wipe stale resource-coverage shards once per live session.

    A shard left over from a previous partial/aborted run could otherwise mask
    a method THIS run failed to cover — the gate would see an old "covered"
    entry and pass vacuously. Only runs when live creds are present: an
    offline-only `pytest tests/` run never writes shards in the first place, so
    there's nothing to reset (and nothing to protect against).
    """
    if _LIVE:
        clear_reports()
    yield
