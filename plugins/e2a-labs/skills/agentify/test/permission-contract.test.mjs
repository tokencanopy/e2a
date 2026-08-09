import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const workflow = (name) => readFile(
  new URL(`../templates/workflows/${name}`, import.meta.url),
  "utf8",
);

test("triage and comms use deny-by-default tool permissions", async () => {
  for (const name of ["feedback-triage.yml.tmpl", "feedback-comms.yml.tmpl"]) {
    const source = await workflow(name);
    assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
    assert.match(source, /--permission-mode\s+dontAsk/);
    assert.match(source, /--tools\s+"Bash,Read"/);
    assert.match(source, /--strict-mcp-config/);
    assert.doesNotMatch(source, /--allowedTools[\s\\]+(?:"[^"]+"\s+)*"Bash"(?:\s|\\)/);
    assert.doesNotMatch(source, /"Bash\((?:curl|wget|env|printenv):\*\)"/);
  }
});

test("the fix lane exposes only its documented built-ins", async () => {
  const source = await workflow("feedback-fix.yml.tmpl");
  assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
  assert.match(source, /--permission-mode\s+dontAsk/);
  assert.match(source, /--tools\s+"Bash,Edit,Write,Read,Glob,Grep"/);
  assert.match(source, /--strict-mcp-config/);
});
