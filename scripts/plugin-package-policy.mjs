const PACKAGE_MANAGER_CACHE = /(?:^|\/)(?:node_modules|\.npm|\.pnpm-store|\.yarn(?:\/cache)?)(?:\/|$)/;

const CORE_PATTERNS = Object.freeze([
  /^(?:\.claude-plugin|\.codex-plugin|\.cursor-plugin)\/plugin\.json$/,
  /^(?:\.mcp\.json|README\.md|mcp\.json|plugin\.json|plugin\.meta\.json)$/,
  /^assets\/icon\.svg$/,
  /^clients\/(?:README\.md|codex\.toml|mcp\.json|vscode\.mcp\.json)$/,
  /^docs\/(?:auth\.md|llms\.txt|sdk\.md|setup\.md|templates\.md)$/,
  /^skills\/e2a\/SKILL\.md$/,
  /^skills\/e2a-doctor\/(?:SKILL\.md|references\/(?:diagnostic-matrix|guided-repairs)\.md)$/,
  /^skills\/e2a-integrate\/(?:SKILL\.md|references\/(?:integration-modes|rest-openapi|sdk-recipes|webhooks-and-tests)\.md)$/,
  /^skills\/e2a-setup\/(?:SKILL\.md|references\/(?:clients|custom-domains)\.md)$/,
  /^skills\/email-evals\/(?:SKILL\.md|email-evals\.sh|launcher\.mjs|scaffold\.mjs|setup\.mjs)$/,
  /^skills\/email-evals\/templates\/(?:README\.md|suite\.yaml|fixtures\/README\.md|results\/\.gitignore|cases\/(?:happy-path|missing-information|unsafe-request)\.yaml)$/,
]);

// The installed runtime is security-sensitive. Keep every tracked runtime
// input explicit so an added source, fixture, cache, or native file fails the
// package gate until it receives an intentional review here.
const EMAIL_EVAL_RUNTIME_FILES = new Set(`
THIRD_PARTY_NOTICES.md
cli.mjs
email-evals-runtime.bundle.mjs
lib/cli-arguments.mjs
lib/contract.mjs
lib/e2a-adapter.mjs
lib/errors.mjs
lib/grade-content.mjs
lib/grade-core.mjs
lib/mime.mjs
lib/normalize.mjs
lib/report.mjs
lib/result-contract.mjs
lib/runner.mjs
lib/safe-pattern.mjs
package-lock.json
package.json
test/bundle.test.mjs
test/cli.test.mjs
test/contract.test.mjs
test/e2a-adapter-observe.test.mjs
test/e2a-adapter-preflight.test.mjs
test/final-security.test.mjs
test/grade-content.test.mjs
test/grade-core.test.mjs
test/launcher.test.mjs
test/live-responder.mjs
test/mime.test.mjs
test/report.test.mjs
test/runner.test.mjs
test/safe-pattern.test.mjs
test/scaffold.test.mjs
test/setup.test.mjs
testdata/contracts/invalid/bad-duration/suite.yaml
testdata/contracts/invalid/bad-regex/case.yaml
testdata/contracts/invalid/bad-regex/suite.yaml
testdata/contracts/invalid/case-traversal/suite.yaml
testdata/contracts/invalid/duplicate-key/suite.yaml
testdata/contracts/invalid/escape.yaml
testdata/contracts/invalid/missing-envelope-expectation/case.yaml
testdata/contracts/invalid/missing-envelope-expectation/suite.yaml
testdata/contracts/invalid/missing-environment/case.yaml
testdata/contracts/invalid/missing-environment/suite.yaml
testdata/contracts/invalid/partial-environment/suite.yaml
testdata/contracts/invalid/recipient-outside-allowlist/case.yaml
testdata/contracts/invalid/recipient-outside-allowlist/suite.yaml
testdata/contracts/invalid/unknown-key/suite.yaml
testdata/contracts/valid/cases/happy-path.yaml
testdata/contracts/valid/cases/missing-information.yaml
testdata/contracts/valid/cases/unsafe-request.yaml
testdata/contracts/valid/suite.yaml
testdata/e2a/events-blocked.json
testdata/e2a/events-success.json
testdata/e2a/protection-safe.json
testdata/e2a/protection-wide.json
testdata/evidence/core-forward.json
testdata/evidence/core-safe-reply.json
testdata/mime/attachment.eml
testdata/mime/header-injection.eml
testdata/mime/reply.eml
testdata/reports/mixed/cases.jsonl
testdata/reports/mixed/report.md
testdata/reports/mixed/summary.json
testdata/reports/pass/cases.jsonl
testdata/reports/pass/report.md
testdata/reports/pass/summary.json
`.trim().split("\n"));

const LABS_PATTERNS = Object.freeze([
  /^(?:\.claude-plugin|\.codex-plugin)\/plugin\.json$/,
  /^(?:README\.md|plugin\.json|plugin\.meta\.json)$/,
  /^assets\/icon\.svg$/,
  /^skills\/agentify\/(?:SKILL\.md|agentify-render\.sh|safe-paths\.sh)$/,
  /^skills\/agentify\/examples\/e2a\/(?:agentify-fix-verify-setup\.sh|autonomous-repo\.config\.yml)$/,
  /^skills\/agentify\/references\/(?:adapters|security-invariants|setup-checklist)\.md$/,
  /^skills\/agentify\/templates\/(?:autonomous-repo\.config\.yml\.tmpl|addons\/README\.md)$/,
  /^skills\/agentify\/templates\/addons\/submit-feedback-mcp\/(?:manifest\.yml|setup\.md|files\/(?:bridge\.mjs|bridge\.test\.mjs|package\.json|server\.mjs))$/,
  /^skills\/agentify\/templates\/runtime-skill\/(?:SKILL\.md|comms\.md|fix\.md|state-machine\.md|ticket-card\.md|triage\.md)$/,
  /^skills\/agentify\/templates\/runtime-skill\/templates\/(?:approval-request|resolved-closed|shipped|triage-ack)\.md$/,
  /^skills\/agentify\/templates\/scripts\/(?:comms_send|released_markers|ticket_card)\.sh$/,
  /^skills\/agentify\/templates\/workflows\/feedback-(?:comms|fix|released|triage)\.yml\.tmpl$/,
  /^skills\/agentify\/test\/(?:permission-contract\.test\.mjs|render-config\.test\.py|run\.sh|validate\.py)$/,
  /^skills\/agentify\/test\/fixtures\/(?:README\.md|run-fixtures\.sh)$/,
  /^skills\/agentify\/test\/fixtures\/harness\/(?:\.gitignore|assert-selftest\.sh|mock-comms_send\.sh|mock-gh|mock-mcp\.mjs|mock-ticket_card\.sh|package\.json|runner\.sh)$/,
  /^skills\/agentify\/test\/fixtures\/harness\/prompts\/triage\.txt$/,
  /^skills\/agentify\/test\/fixtures\/triage\/(?:injection|new-feedback|reply-skip)\/(?:assert\.sh|findbycomms\.txt|messages\.json)$/,
  /^skills\/autopilot\/(?:SKILL\.md|autopilot\.mjs|autopilot\.sh|config\.mjs|daemon\.mjs|gateway\.mjs|installer\.mjs|interview\.mjs|job-tool\.mjs|lock\.mjs|mail-client\.mjs|operator\.mjs|policy\.mjs|runner\.mjs|runtime\.mjs|service\.mjs|setup\.mjs|spool\.mjs|supervisor\.mjs)$/,
  /^skills\/autopilot\/test\/(?:autopilot-cli|clean-room|config|daemon|gateway|installer|interview|job-tool|lock|mail-client|operator|policy|runtime|service|setup|spool|supervisor)\.test\.mjs$/,
  /^skills\/tether\/(?:SKILL\.md|install\.sh|lib\.sh|tether\.env\.example|tether\.sh|hooks\/tether-notify\.sh)$/,
]);

function isAllowedRelativePath(plugin, relative) {
  if (PACKAGE_MANAGER_CACHE.test(relative)) return false;
  if (plugin === "e2a") {
    const runtimePrefix = "skills/email-evals/runtime/";
    if (relative.startsWith(runtimePrefix)) {
      return EMAIL_EVAL_RUNTIME_FILES.has(relative.slice(runtimePrefix.length));
    }
    return CORE_PATTERNS.some((pattern) => pattern.test(relative));
  }
  if (plugin === "e2a-labs") {
    return LABS_PATTERNS.some((pattern) => pattern.test(relative));
  }
  return false;
}

export function unexpectedPluginPackagePaths(plugin, files) {
  const prefix = `plugins/${plugin}/`;
  return files.filter((file) => (
    typeof file !== "string"
    || !file.startsWith(prefix)
    || !isAllowedRelativePath(plugin, file.slice(prefix.length))
  ));
}
