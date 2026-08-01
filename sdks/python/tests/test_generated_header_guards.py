"""Static audit: generated api modules must None-guard header params.

The Python analogue of the TS SDK's static audit in
sdks/typescript/test/v1/optional-header-params.test.ts, pinning the same bug
class from the other side. Background: OpenAPI Generator's `typescript`
generator emitted setHeaderParam(...) unconditionally for optional header
params, so an omitted etag reached the wire as the literal string
"If-Match: undefined" and every unkeyed contacts.update / contacts.setOutreach
failed with 412 precondition_failed — found only by the first live staging
conformance run (e2a-ops release-pipeline run 30612956986), because no offline
gate looked at the emission. The TS pipeline now post-processes the generated
code (scripts/guard-optional-header-params.py) and audits it in unit CI.

The Python generator currently does the right thing on its own: every
param-sourced header assignment it emits is guarded —

    if if_match is not None:
        _header_params['If-Match'] = if_match

— but until this file, NOTHING pinned that. A generator upgrade or template
change that dropped the guard would regenerate green (the freshness gate only
checks the committed code matches the generator output — it re-blesses
whatever the generator does) and ship `If-Match: None` on the wire, failing
only at the staging gate again. This audit makes that a per-PR unit-CI
failure instead.

Rule enforced: in every generated api module, a header assignment whose
right-hand side is a plain method parameter

    _header_params['X'] = some_param

must be immediately preceded by its own None guard (`if some_param is not
None:`). Generator-internal assignments (Accept / Content-Type negotiation)
assign from underscore-prefixed locals or method calls and are out of scope.

The audit is deliberately stricter than "optional params only": the Python
generator guards EVERY param-sourced header (including ones a spec marks
required), so requiring the guard universally can't false-fail today's tree
and needs no optionality inference from the public-method signatures. If a
future header param legitimately must be emitted unguarded, add it to
UNGUARDED_HEADER_ALLOWLIST with the reason (the TS pipeline has exactly one
such exception, Idempotency-Key, whose retry layer needs the stub; the Python
retry layer does not use that mechanism, so the list starts empty).
"""

from __future__ import annotations

import re
from pathlib import Path

GENERATED_API_DIR = (
    Path(__file__).resolve().parent.parent / "src" / "e2a" / "v1" / "generated" / "api"
)

# Header names (lowercased) allowed to be assigned from a param WITHOUT a
# None guard, mapped to the reason. Every entry is a decision — an unguarded
# optional header goes on the wire rendered from None when omitted.
UNGUARDED_HEADER_ALLOWLIST: dict[str, str] = {}

# A param-sourced header assignment: RHS is a single plain identifier that
# does not start with an underscore (generator-internal locals like
# _content_type / _default_content_type are underscore-prefixed, and
# negotiation assignments like select_header_accept(...) are calls, so
# neither shape matches).
HEADER_ASSIGNMENT = re.compile(
    r"^(?P<indent>[ \t]*)_header_params\[(?P<quote>['\"])(?P<header>[^'\"]+)(?P=quote)\]"
    r"\s*=\s*(?P<param>[A-Za-z][A-Za-z0-9_]*)\s*(?:#.*)?$"
)


def none_guard(param: str) -> re.Pattern[str]:
    return re.compile(rf"^[ \t]*if\s+{re.escape(param)}\s+is\s+not\s+None\s*:\s*$")


def audit_lines(lines: list[str]) -> tuple[list[tuple[int, str, str]], int]:
    """Return ([(line_no, header, param) offenders], guarded_site_count)."""
    offenders: list[tuple[int, str, str]] = []
    guarded = 0
    for i, line in enumerate(lines):
        m = HEADER_ASSIGNMENT.match(line)
        if m is None:
            continue
        if m.group("header").lower() in UNGUARDED_HEADER_ALLOWLIST:
            continue
        if i > 0 and none_guard(m.group("param")).match(lines[i - 1]):
            guarded += 1
            continue
        offenders.append((i + 1, m.group("header"), m.group("param")))
    return offenders, guarded


def test_generated_api_files_none_guard_every_param_sourced_header() -> None:
    api_files = sorted(GENERATED_API_DIR.glob("*_api.py"))
    assert api_files, f"no generated api modules found under {GENERATED_API_DIR}"

    offenders: list[str] = []
    guarded_total = 0
    for path in api_files:
        file_offenders, guarded = audit_lines(
            path.read_text(encoding="utf-8").splitlines()
        )
        guarded_total += guarded
        offenders.extend(
            f"{path.name}:{line_no} assigns header {header!r} from param "
            f"{param!r} without an `if {param} is not None:` guard"
            for line_no, header, param in file_offenders
        )

    assert not offenders, (
        "generated Python api modules set param-sourced headers unconditionally "
        "(an omitted optional param would reach the wire rendered from None, the "
        "same bug class as the TS SDK's literal 'If-Match: undefined' — see this "
        "file's docstring). Fix the generation pipeline (never hand-edit "
        "generated code) or, if the emission is deliberate, add the header to "
        "UNGUARDED_HEADER_ALLOWLIST with the reason:\n  " + "\n  ".join(offenders)
    )

    # Non-vacuity: today's tree carries guarded param-sourced header sites
    # (contacts If-Match, messages Idempotency-Key). If a generator upgrade
    # changes the emission shape so the audit no longer recognises ANY site,
    # this must fail loudly rather than pass with an empty denominator.
    assert guarded_total >= 2, (
        f"audit recognised only {guarded_total} guarded header site(s) — the "
        "generator's emission shape has drifted from what HEADER_ASSIGNMENT "
        "matches; update the audit so it still sees the real header sites"
    )


def test_audit_discrimination() -> None:
    """Pin the matcher's own behaviour on the shapes it must catch and skip."""
    unguarded = [
        "        _header_params['If-Match'] = if_match",
        '        _header_params["X-Custom"] = custom',
    ]
    for line in unguarded:
        offenders, _ = audit_lines(["        # process the header parameters", line])
        assert offenders, f"audit missed an unguarded header assignment: {line!r}"

    guarded = [
        "        if if_match is not None:",
        "            _header_params['If-Match'] = if_match",
    ]
    offenders, guarded_count = audit_lines(guarded)
    assert not offenders and guarded_count == 1

    # Guard for a DIFFERENT param must not satisfy the assignment's guard.
    mismatched = [
        "        if other_param is not None:",
        "            _header_params['If-Match'] = if_match",
    ]
    offenders, _ = audit_lines(mismatched)
    assert offenders, "audit accepted a None guard for the wrong param"

    out_of_scope = [
        # Generator-internal negotiation: call RHS and underscore locals.
        "            _header_params['Accept'] = self.api_client.select_header_accept(",
        "            _header_params['Content-Type'] = _content_type",
        "                _header_params['Content-Type'] = _default_content_type",
    ]
    for line in out_of_scope:
        offenders, _ = audit_lines([line])
        assert not offenders, f"audit false-positived on generator plumbing: {line!r}"
