from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("guard-optional-header-params.py")


def load_guard():
    spec = importlib.util.spec_from_file_location("guard_optional_header_params", SCRIPT)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"unable to load {SCRIPT}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


METHOD = (
    "    public async updateContact(address: string, "
    "updateContactRequest: UpdateContactRequest, ifMatch?: string, "
    "_options?: Configuration): Promise<RequestContext> {\n"
)


def api_source(header_lines: str) -> str:
    return (
        "export class ContactsApiRequestFactory extends BaseAPIRequestFactory {\n\n"
        + METHOD
        + "        // Header Params\n"
        + header_lines
        + "\n        return requestContext;\n"
        + "    }\n\n"
        + "}\n"
    )


class GuardOptionalHeaderParamsTest(unittest.TestCase):
    def test_guards_optional_header_param(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ContactsApi.ts"
            path.write_text(
                api_source(
                    '        requestContext.setHeaderParam("If-Match", '
                    'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                ),
                encoding="utf-8",
            )

            self.assertTrue(guard.guard_file(path))
            self.assertEqual(
                api_source(
                    "        if (ifMatch !== undefined) {\n"
                    '            requestContext.setHeaderParam("If-Match", '
                    'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                    "        }\n"
                ),
                path.read_text(encoding="utf-8"),
            )

    def test_leaves_idempotency_key_stub_unguarded(self) -> None:
        # retry.ts depends on the present-but-undefined Idempotency-Key stub to
        # recognise server-deduped POSTs and mint a key — never guard it.
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "MessagesApi.ts"
            source = api_source(
                '        requestContext.setHeaderParam("Idempotency-Key", '
                'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
            )
            path.write_text(source, encoding="utf-8")

            self.assertFalse(guard.guard_file(path))
            self.assertEqual(source, path.read_text(encoding="utf-8"))

    def test_leaves_required_header_param_unguarded(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "RequiredApi.ts"
            source = (
                "export class RequiredApiRequestFactory {\n"
                "    public async op(tenant: string, _options?: Configuration): "
                "Promise<RequestContext> {\n"
                '        requestContext.setHeaderParam("X-Tenant", '
                'ObjectSerializer.serialize(tenant, "string", ""));\n'
                "        return requestContext;\n"
                "    }\n"
                "}\n"
            )
            path.write_text(source, encoding="utf-8")

            self.assertFalse(guard.guard_file(path))
            self.assertEqual(source, path.read_text(encoding="utf-8"))

    def test_rerun_is_idempotent(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ContactsApi.ts"
            path.write_text(
                api_source(
                    '        requestContext.setHeaderParam("If-Match", '
                    'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                ),
                encoding="utf-8",
            )

            self.assertTrue(guard.guard_file(path))
            first_pass = path.read_text(encoding="utf-8")
            self.assertFalse(guard.guard_file(path))
            self.assertEqual(first_pass, path.read_text(encoding="utf-8"))

    def test_optional_params_are_scoped_per_method(self) -> None:
        # `ifMatch` optional in one method must not guard a same-named REQUIRED
        # param in the next method.
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "MixedApi.ts"
            unguarded = (
                "    public async second(ifMatch: string, _options?: Configuration): "
                "Promise<RequestContext> {\n"
                '        requestContext.setHeaderParam("If-Match", '
                'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                "    }\n"
            )
            path.write_text(
                api_source(
                    '        requestContext.setHeaderParam("If-Match", '
                    'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                )
                + unguarded,
                encoding="utf-8",
            )

            self.assertTrue(guard.guard_file(path))
            content = path.read_text(encoding="utf-8")
            self.assertIn(
                "        if (ifMatch !== undefined) {\n"
                '            requestContext.setHeaderParam("If-Match", ',
                content,
            )
            self.assertIn(unguarded, content)

    def test_directory_argument_expands_to_ts_files(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ContactsApi.ts"
            path.write_text(
                api_source(
                    '        requestContext.setHeaderParam("If-Match", '
                    'ObjectSerializer.serialize(ifMatch, "string", ""));\n'
                ),
                encoding="utf-8",
            )

            self.assertEqual(0, guard.main([directory]))
            self.assertIn("if (ifMatch !== undefined) {", path.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
