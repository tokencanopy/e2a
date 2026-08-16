import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "preserve-generated-domain-delete-signatures.py"


class PreserveGeneratedDomainDeleteSignaturesTest(unittest.TestCase):
    def test_preserves_existing_options_positions_and_appends_idempotency_key(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            ts = base / "ts"
            py = base / "py"
            (ts / "apis").mkdir(parents=True)
            (ts / "types").mkdir()
            (py / "api").mkdir(parents=True)

            (ts / "apis" / "DomainsApi.ts").write_text(
                "public async deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: Configuration): Promise<RequestContext> {}\n"
            )
            (ts / "types" / "ObservableAPI.ts").write_text(
                "public deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: ConfigurationOptions): Observable<X> {\n"
                "const p = this.requestFactory.deleteDomain(domain, confirm, idempotencyKey, _config);\n}\n"
                "public deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: ConfigurationOptions): Observable<X> {\n"
                "return this.deleteDomainWithHttpInfo(domain, confirm, idempotencyKey, _options);\n}\n"
            )
            (ts / "types" / "PromiseAPI.ts").write_text(
                "public deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: PromiseConfigurationOptions): Promise<X> {\n"
                "const result = this.api.deleteDomainWithHttpInfo(domain, confirm, idempotencyKey, observableOptions);\n}\n"
                "public deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: PromiseConfigurationOptions): Promise<X> {\n"
                "const result = this.api.deleteDomain(domain, confirm, idempotencyKey, observableOptions);\n}\n"
            )
            (ts / "types" / "ObjectParamAPI.ts").write_text(
                "return this.api.deleteDomainWithHttpInfo(param.domain, param.confirm, param.idempotencyKey,  options).toPromise();\n"
                "return this.api.deleteDomain(param.domain, param.confirm, param.idempotencyKey,  options).toPromise();\n"
            )
            (py / "api" / "domains_api.py").write_text(
                "".join(
                    f"    async def {name}(\n"
                    "        self,\n"
                    "        domain: StrictStr,\n"
                    "        confirm: StrictStr,\n"
                    "        idempotency_key: Optional[str] = None,\n"
                    "        _request_timeout: object = None,\n"
                    "        _host_index: int = 0,\n"
                    "    ) -> object:\n"
                    "        pass\n"
                    for name in (
                        "delete_domain",
                        "delete_domain_with_http_info",
                        "delete_domain_without_preload_content",
                    )
                )
            )

            subprocess.run(
                ["python3", str(SCRIPT), "typescript", str(ts)],
                check=True,
                capture_output=True,
                text=True,
            )
            python_result = subprocess.run(
                ["python3", str(SCRIPT), "python", str(py)],
                capture_output=True,
                text=True,
            )
            self.assertEqual(python_result.returncode, 0, python_result.stderr)

            request_factory = (ts / "apis" / "DomainsApi.ts").read_text()
            self.assertIn("confirm: 'DELETE', _options?: Configuration, idempotencyKey?: string", request_factory)
            observable = (ts / "types" / "ObservableAPI.ts").read_text()
            self.assertIn("confirm: 'DELETE', _options?: ConfigurationOptions, idempotencyKey?: string", observable)
            self.assertIn("deleteDomain(domain, confirm, _config, idempotencyKey)", observable)
            promise = (ts / "types" / "PromiseAPI.ts").read_text()
            self.assertIn("confirm: 'DELETE', _options?: PromiseConfigurationOptions, idempotencyKey?: string", promise)
            self.assertIn("deleteDomain(domain, confirm, observableOptions, idempotencyKey)", promise)
            object_api = (ts / "types" / "ObjectParamAPI.ts").read_text()
            self.assertIn("deleteDomain(param.domain, param.confirm, options, param.idempotencyKey)", object_api)

            python_api = (py / "api" / "domains_api.py").read_text()
            for signature in python_api.split("    async def ")[1:]:
                header = signature.split(") -> object:", 1)[0]
                self.assertLess(header.index("_request_timeout"), header.index("idempotency_key"))
                self.assertLess(header.index("_host_index"), header.index("idempotency_key"))

    def test_fails_closed_when_generator_shape_changes(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            ts = base / "ts"
            py = base / "py"
            (ts / "apis").mkdir(parents=True)
            (ts / "types").mkdir()
            (py / "api").mkdir(parents=True)
            for path in (
                ts / "apis" / "DomainsApi.ts",
                ts / "types" / "ObservableAPI.ts",
                ts / "types" / "PromiseAPI.ts",
                ts / "types" / "ObjectParamAPI.ts",
                py / "api" / "domains_api.py",
            ):
                path.write_text("unexpected generator output\n")

            result = subprocess.run(
                ["python3", str(SCRIPT), "typescript", str(ts)],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
