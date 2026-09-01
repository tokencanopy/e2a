from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("guard-dot-segment-path.py")


def load_guard():
    spec = importlib.util.spec_from_file_location("guard_dot_segment_path", SCRIPT)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"unable to load {SCRIPT}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def http_ts_source() -> str:
    return (
        "function ensureAbsoluteUrl(url: string) {\n"
        '    if (url.startsWith("http://") || url.startsWith("https://")) {\n'
        "        return url;\n"
        "    }\n"
        "    return window.location.origin + url;\n"
        "}\n"
        "\n"
        "export class RequestContext {\n"
        "    public constructor(url: string, private httpMethod: HttpMethod) {\n"
        "        this.url = new URL(ensureAbsoluteUrl(url));\n"
        "    }\n"
        "\n"
        "    public setUrl(url: string) {\n"
        "        this.url = new URL(ensureAbsoluteUrl(url));\n"
        "    }\n"
        "}\n"
    )


class GuardDotSegmentPathTest(unittest.TestCase):
    def test_guards_constructor_and_set_url(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "http.ts"
            path.write_text(http_ts_source(), encoding="utf-8")

            self.assertTrue(guard.guard_file(path))
            content = path.read_text(encoding="utf-8")
            self.assertEqual(2, content.count("assertNoDotSegmentInPath(url);"))
            self.assertIn("function assertNoDotSegmentInPath(url: string): void {", content)
            # Guard call precedes the collapsing new URL() call at both sites.
            for site in content.split("this.url = new URL(ensureAbsoluteUrl(url));")[:-1]:
                self.assertTrue(site.rstrip().endswith("assertNoDotSegmentInPath(url);"))

    def test_rerun_is_idempotent(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "http.ts"
            path.write_text(http_ts_source(), encoding="utf-8")

            self.assertTrue(guard.guard_file(path))
            first_pass = path.read_text(encoding="utf-8")
            self.assertFalse(guard.guard_file(path))
            self.assertEqual(first_pass, path.read_text(encoding="utf-8"))

    def test_raises_when_call_site_count_drifts(self) -> None:
        # A future generator version that changes how many call sites collapse
        # the URL must fail loudly, not silently re-bless a partial guard.
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "http.ts"
            path.write_text(
                http_ts_source().replace(
                    "        this.url = new URL(ensureAbsoluteUrl(url));\n"
                    "    }\n"
                    "\n"
                    "    public setUrl(url: string) {\n"
                    "        this.url = new URL(ensureAbsoluteUrl(url));\n"
                    "    }\n",
                    "        this.url = new URL(ensureAbsoluteUrl(url));\n"
                    "    }\n",
                ),
                encoding="utf-8",
            )

            with self.assertRaises(RuntimeError):
                guard.guard_file(path)

    def test_raises_when_already_guarded_call_site_missing(self) -> None:
        guard = load_guard()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "http.ts"
            source = http_ts_source().replace(
                "        this.url = new URL(ensureAbsoluteUrl(url));\n"
                "    }\n"
                "\n"
                "    public setUrl(url: string) {\n",
                "        assertNoDotSegmentInPath(url);\n"
                "        this.url = new URL(ensureAbsoluteUrl(url));\n"
                "    }\n"
                "\n"
                "    public setUrl(url: string) {\n",
            )
            # Simulate a hand-tampered file: guard function present (so the
            # idempotent short-circuit fires) but the second call site is bare.
            source = "function assertNoDotSegmentInPath(url: string): void {}\n" + source
            path.write_text(source, encoding="utf-8")

            with self.assertRaises(RuntimeError):
                guard.guard_file(path)


if __name__ == "__main__":
    unittest.main()
