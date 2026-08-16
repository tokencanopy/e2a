#!/usr/bin/env python3
"""Keep new delete-domain headers from displacing existing SDK options.

OpenAPI Generator inserts optional operation parameters before its transport
options. Adding Idempotency-Key would therefore reinterpret existing third
positional arguments as the header in both generated clients. This narrow,
fail-closed post-processor appends the new parameter after the pre-existing
options while leaving the wire serialization generated from OpenAPI intact.
"""

from pathlib import Path
import sys


def replace_exact(path: Path, old: str, new: str) -> None:
    text = path.read_text()
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{path}: found {count} copies, want 1: {old!r}")
    path.write_text(text.replace(old, new))


def normalize_typescript(root: Path) -> None:
    replace_exact(
        root / "apis" / "DomainsApi.ts",
        "deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: Configuration)",
        "deleteDomain(domain: string, confirm: 'DELETE', _options?: Configuration, idempotencyKey?: string)",
    )

    observable = root / "types" / "ObservableAPI.ts"
    replace_exact(
        observable,
        "deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: ConfigurationOptions)",
        "deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', _options?: ConfigurationOptions, idempotencyKey?: string)",
    )
    replace_exact(
        observable,
        "this.requestFactory.deleteDomain(domain, confirm, idempotencyKey, _config)",
        "this.requestFactory.deleteDomain(domain, confirm, _config, idempotencyKey)",
    )
    replace_exact(
        observable,
        "deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: ConfigurationOptions)",
        "deleteDomain(domain: string, confirm: 'DELETE', _options?: ConfigurationOptions, idempotencyKey?: string)",
    )
    replace_exact(
        observable,
        "this.deleteDomainWithHttpInfo(domain, confirm, idempotencyKey, _options)",
        "this.deleteDomainWithHttpInfo(domain, confirm, _options, idempotencyKey)",
    )

    promise = root / "types" / "PromiseAPI.ts"
    replace_exact(
        promise,
        "deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: PromiseConfigurationOptions)",
        "deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', _options?: PromiseConfigurationOptions, idempotencyKey?: string)",
    )
    replace_exact(
        promise,
        "this.api.deleteDomainWithHttpInfo(domain, confirm, idempotencyKey, observableOptions)",
        "this.api.deleteDomainWithHttpInfo(domain, confirm, observableOptions, idempotencyKey)",
    )
    replace_exact(
        promise,
        "deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: PromiseConfigurationOptions)",
        "deleteDomain(domain: string, confirm: 'DELETE', _options?: PromiseConfigurationOptions, idempotencyKey?: string)",
    )
    replace_exact(
        promise,
        "this.api.deleteDomain(domain, confirm, idempotencyKey, observableOptions)",
        "this.api.deleteDomain(domain, confirm, observableOptions, idempotencyKey)",
    )

    object_api = root / "types" / "ObjectParamAPI.ts"
    replace_exact(
        object_api,
        "this.api.deleteDomainWithHttpInfo(param.domain, param.confirm, param.idempotencyKey,  options)",
        "this.api.deleteDomainWithHttpInfo(param.domain, param.confirm, options, param.idempotencyKey)",
    )
    replace_exact(
        object_api,
        "this.api.deleteDomain(param.domain, param.confirm, param.idempotencyKey,  options)",
        "this.api.deleteDomain(param.domain, param.confirm, options, param.idempotencyKey)",
    )


def normalize_python(root: Path) -> None:
    path = root / "api" / "domains_api.py"
    lines = path.read_text().splitlines(keepends=True)
    for name in (
        "delete_domain",
        "delete_domain_with_http_info",
        "delete_domain_without_preload_content",
    ):
        marker = f"    async def {name}(\n"
        starts = [index for index, line in enumerate(lines) if line == marker]
        if len(starts) != 1:
            raise RuntimeError(f"{path}: found {len(starts)} {name} signatures, want 1")
        start = starts[0]
        try:
            end = next(
                index
                for index in range(start + 1, len(lines))
                if lines[index].startswith("    ) -> ")
            )
        except StopIteration as exc:
            raise RuntimeError(f"{path}: unterminated {name} signature") from exc
        idem_lines = [
            index
            for index in range(start + 1, end)
            if lines[index].lstrip().startswith("idempotency_key:")
        ]
        if len(idem_lines) != 1:
            raise RuntimeError(f"{path}: found {len(idem_lines)} idempotency_key params in {name}, want 1")
        idem_line = lines.pop(idem_lines[0])
        lines.insert(end - 1, idem_line)
    path.write_text("".join(lines))


def main() -> None:
    if len(sys.argv) != 3 or sys.argv[1] not in {"typescript", "python"}:
        raise SystemExit(
            "usage: preserve-generated-domain-delete-signatures.py {typescript|python} ROOT"
        )
    if sys.argv[1] == "typescript":
        normalize_typescript(Path(sys.argv[2]))
    else:
        normalize_python(Path(sys.argv[2]))
    print(f"preserve-generated-domain-delete-signatures: normalized {sys.argv[1]}")


if __name__ == "__main__":
    main()
