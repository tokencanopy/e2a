"""Runtime introspection of AsyncE2AClient's public ergonomic surface.

This is the Python-SDK analogue of ``tests/e2e-prod/harness/mcp-coverage.ts``'s
``recordToolList`` (denominator = whatever the live MCP server advertises) and
``scripts/check-sdk-operation-coverage.py``'s TS-side AST walk: the count of
"things this gate is accountable for" comes from asking the real object graph
what it exposes, NOT from grepping ``client.py`` for ``async def`` / ``def``.

Why a grep would go quietly wrong here specifically:

  - Nested resource namespaces. ``client.account`` is one ``AccountResource``,
    but it also carries ``.suppressions`` (a ``SuppressionsResource``) and
    ``.api_keys`` (an ``APIKeysResource``) assigned dynamically in
    ``AccountResource.__init__`` — a regex over method definitions in
    ``client.py`` has no reliable way to know those five methods
    (``suppressions.list/delete``, ``api_keys.list/create/delete``) are
    reachable as ``client.account.suppressions.*`` / ``client.account.api_keys.*``
    without re-deriving the exact attribute-assignment graph the source encodes
    procedurally. A one-level-deep walk of the ten top-level resource names
    (the natural first cut) undercounts by exactly this nesting: 57 methods
    instead of the true 62. This is the same class of miss that made a TS SDK
    regex report 32 methods against runtime's 59, and an MCP tool grep find 58
    tools against the server's real 60 — the grep sees the resources it already
    knows to look for and silently drops what it doesn't.
  - Sync/async parity is achieved by ``sync_client.py`` wrapping the async tree
    at RUNTIME (``__getattr__`` + ``_wrap_attr``), not by hand-written sync
    methods a grep could find at all.

So this module walks the live object graph instead: ``dir()``/``getattr()`` on
a real ``AsyncE2AClient`` instance, recursing into nested resource namespaces
with the identical "is this a resource, not a data model" heuristic
``sync_client.py`` already uses for its own drift guard
(``_is_resource`` there) — reusing that heuristic here (rather than inventing a
second one) means this gate and the sync mirror can never quietly disagree
about what counts as a resource.
"""

from __future__ import annotations

from typing import Any, Callable, Dict, Optional

from e2a import AsyncE2AClient

#: Public attributes that are lifecycle plumbing, not ergonomic API surface —
#: excluded from the denominator for the same reason sync_client.py's
#: ``_EXCLUDED_ATTRS`` keeps ``aclose`` off the sync facade: it has no request
#: body to marshal and no response to shape, so there is no "argument
#: marshalling / response shaping" bug for a coverage gate to catch here.
_SKIP_NAMES = frozenset({"aclose"})


def _is_resource(value: Any) -> bool:
    """A nested resource namespace (e.g. ``account.suppressions``): a
    non-callable, non-primitive object defined in the ``e2a`` package that
    isn't a pydantic data model. Mirrors ``sync_client.py``'s ``_is_resource``
    exactly — see the module docstring for why sharing the heuristic matters.
    """
    if callable(value) or isinstance(value, (str, bytes, int, float, bool, type(None))):
        return False
    cls = type(value)
    if not cls.__module__.startswith("e2a."):
        return False
    return not hasattr(cls, "model_fields")  # pydantic models are data, not namespaces


def discover_methods(obj: Any, prefix: str = "") -> Dict[str, Callable[..., Any]]:
    """Recursively enumerate every public callable reachable from ``obj``.

    Returns a ``{"agents.create": <bound method>, ...}`` map (dotted path ->
    the live bound callable), built purely from runtime introspection
    (``dir()``/``getattr()`` on the actual instance — the ``inspect`` module's
    own recommended way to enumerate an object's members; equivalent in spirit
    to ``vars(type(obj))`` but also picks up instance attributes assigned
    dynamically in ``__init__``, which nested resources like
    ``account.suppressions`` are).
    """
    found: Dict[str, Callable[..., Any]] = {}
    for name in dir(obj):
        if name.startswith("_") or name in _SKIP_NAMES:
            continue
        value = getattr(obj, name)
        full = f"{prefix}{name}"
        if callable(value):
            found[full] = value
        elif _is_resource(value):
            found.update(discover_methods(value, prefix=f"{full}."))
    return found


def discover_client_methods(client: Optional[AsyncE2AClient] = None) -> Dict[str, Callable[..., Any]]:
    """Discover the full ergonomic method surface of ``AsyncE2AClient``.

    Builds a throwaway client when none is given — construction performs no
    I/O (see ``client.py``: the constructor only builds the httpx transport
    and resource wrappers), so this is safe to call at gate/collection time
    without live credentials.
    """
    c = client or AsyncE2AClient(api_key="introspection-only", base_url="http://introspection.invalid")
    return discover_methods(c)


def assert_no_reflection_bugs() -> None:  # pragma: no cover - debugging helper
    """Fail fast if ``discover_client_methods`` returns something absurd (e.g.
    zero methods, meaning the heuristic broke against a refactor)."""
    methods = discover_client_methods()
    if len(methods) < 10:
        raise AssertionError(
            f"discover_client_methods found only {len(methods)} methods — "
            "the resource-walk heuristic likely broke; fix before trusting the gate"
        )
