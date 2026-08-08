"""Offline regression net for the resource-coverage gate's DENOMINATOR.

``tests/resource_coverage_gate.py`` fails when a method runtime introspection
finds on ``AsyncE2AClient`` has no live ``mark_covered`` call — but that only
protects the facade if introspection actually SEES every resource. A discovery
regression (e.g. a heuristic change that stops descending into a resource
namespace) would shrink the denominator and let the gate pass while whole
resources went untested live. These tests pin three properties of the
discovered set:

  1. it contains the full ``contacts.*`` surface (13 methods — the surface the
     live contacts section in ``tests/test_e2e_resources.py`` exists to cover,
     so a discovery miss here demonstrably turns the gate red for the wrong
     reason or, worse, green for the wrong reason);
  2. it still contains long-standing methods (``agents.create``,
     ``messages.send``) — a canary against the walk breaking wholesale;
  3. it excludes private names and lifecycle plumbing (``aclose``), which have
     no request/response surface for a coverage gate to be accountable for.

Needs no credentials, no network, and writes no coverage shards:
``discover_client_methods`` builds a throwaway client whose constructor
performs no I/O (see its docstring), and nothing here imports the tracker.
"""

from __future__ import annotations

from .resource_coverage_lib.discovery import discover_client_methods

# The 13 contacts.* ids the live suite in test_e2e_resources.py covers. Kept
# as an explicit set (not derived from the resource class) precisely so the
# test and discovery can disagree — that disagreement is the signal.
CONTACTS_METHODS = frozenset(
    {
        "contacts.create",
        "contacts.delete",
        "contacts.delete_import",
        "contacts.delete_outreach",
        "contacts.get",
        "contacts.get_outreach",
        "contacts.get_outreach_with_etag",
        "contacts.get_with_etag",
        "contacts.import_",
        "contacts.list",
        "contacts.outreach",
        "contacts.set_outreach",
        "contacts.update",
    }
)

LONGSTANDING_METHODS = frozenset({"agents.create", "messages.send"})


def test_discovery_finds_the_full_contacts_surface():
    discovered = discover_client_methods()
    missing = CONTACTS_METHODS - discovered.keys()
    assert not missing, (
        f"discover_client_methods no longer sees {sorted(missing)} — the gate's "
        "denominator shrank, so absent live coverage would pass silently"
    )


def test_discovery_still_finds_longstanding_methods():
    discovered = discover_client_methods()
    missing = LONGSTANDING_METHODS - discovered.keys()
    assert not missing, f"discovery walk looks broken: missing {sorted(missing)}"


def test_discovery_excludes_private_and_lifecycle_names():
    discovered = discover_client_methods()
    assert "aclose" not in discovered
    for name in discovered:
        for part in name.split("."):
            assert not part.startswith("_"), f"private name leaked into the denominator: {name}"
