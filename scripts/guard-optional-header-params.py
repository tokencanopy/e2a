#!/usr/bin/env python3
"""Guard optional header params in the generated TypeScript request factories.

OpenAPI Generator's `typescript` generator (v7.16.0) emits

    requestContext.setHeaderParam("X", ObjectSerializer.serialize(param, ...));

unconditionally for every header parameter, including OPTIONAL ones.
`ObjectSerializer.serialize(undefined, ...)` returns undefined, the
RequestContext stores it, and the fetch layer's `Headers.set` coerces it to the
literal string "undefined" on the wire. A server then receives e.g.
`If-Match: undefined` and correctly treats the request as conditional, so an
unkeyed `contacts.update` / `contacts.setOutreach` always fails with 412
precondition_failed. The Python generator already guards these sites
(`if if_match is not None: ...`); this step normalizes the TypeScript output to
match:

    if (param !== undefined) {
        requestContext.setHeaderParam("X", ObjectSerializer.serialize(param, ...));
    }

Exception: "Idempotency-Key" is deliberately left UNGUARDED. The retry layer
(sdks/typescript/src/v1/retry.ts) relies on the presence of the generated
present-but-undefined stub to (a) recognise server-deduped POSTs as retry-safe
and (b) mint a key once before the first attempt. Guarding it would silently
disable retry + key minting on send/reply/forward/approve/create-webhook/
create-api-key. The stub never reaches the wire through the supported client:
ensureIdempotencyKey overwrites it with a minted key first.

Usage: guard-optional-header-params.py FILE_OR_DIR [FILE_OR_DIR ...]
Directories are expanded to their immediate *.ts files. Idempotent: already
guarded emissions are left alone.
"""

from __future__ import annotations

from pathlib import Path
import re
import sys


UNGUARDED_HEADERS = {"idempotency-key"}

SIGNATURE = re.compile(
    r"^\s*public async \w+\((?P<params>[^)]*)\): Promise<RequestContext> \{\s*$"
)
OPTIONAL_PARAM = re.compile(r"(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\?:")
HEADER_PARAM = re.compile(
    r"^(?P<indent>[ \t]*)requestContext\.setHeaderParam\("
    r"\"(?P<header>[^\"]+)\", ObjectSerializer\.serialize\("
    r"(?P<param>[A-Za-z_$][A-Za-z0-9_$]*), .*\);[ \t]*(?P<newline>\r?\n)?$"
)


def guard_file(path: Path) -> bool:
    source = path.read_text(encoding="utf-8")
    lines = source.splitlines(keepends=True)
    output: list[str] = []
    changed = False
    optional_params: set[str] = set()

    for line in lines:
        signature = SIGNATURE.match(line)
        if signature is not None:
            optional_params = {
                match.group("name")
                for match in OPTIONAL_PARAM.finditer(signature.group("params"))
            }
            output.append(line)
            continue

        header = HEADER_PARAM.match(line)
        if (
            header is None
            or header.group("header").lower() in UNGUARDED_HEADERS
            or header.group("param") not in optional_params
        ):
            output.append(line)
            continue

        guard = f"if ({header.group('param')} !== undefined) {{"
        if output and output[-1].strip() == guard:
            output.append(line)  # already guarded (idempotent re-run)
            continue

        indent = header.group("indent")
        newline = header.group("newline") or "\n"
        output.append(f"{indent}{guard}{newline}")
        output.append(f"{indent}    {line.lstrip()}")
        output.append(f"{indent}}}{newline}")
        changed = True

    if changed:
        path.write_text("".join(output), encoding="utf-8")
    return changed


def expand(argument: str) -> list[Path]:
    path = Path(argument)
    if path.is_dir():
        return sorted(path.glob("*.ts"))
    return [path]


def main(argv: list[str]) -> int:
    if not argv:
        print(
            "usage: guard-optional-header-params.py FILE_OR_DIR [FILE_OR_DIR ...]",
            file=sys.stderr,
        )
        return 2

    changed = sum(guard_file(path) for argument in argv for path in expand(argument))
    print(f"guard-optional-header-params: guarded {changed} file(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
