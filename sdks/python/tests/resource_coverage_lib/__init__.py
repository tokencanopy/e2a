"""Ergonomic-facade coverage tooling for the Python SDK.

Named `resource_coverage_lib` (not `coverage`) deliberately — `coverage` is
pytest-cov's own dependency's top-level module name, and this package's
directory is added to `sys.path` by `tests/resource_coverage_gate.py`, which
would shadow the real one for the rest of the process.

See ``tests/resource_coverage_gate.py`` for the gate's usage/design docstring
(the MCP/`/v1` analogue: ``tests/e2e-prod/mcp_coverage_gate.py`` +
``tests/e2e-prod/coverage_gate.py`` in the repo root).
"""
