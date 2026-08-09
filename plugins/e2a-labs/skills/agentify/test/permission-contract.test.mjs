import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const workflow = (name) => readFile(
  new URL(`../templates/workflows/${name}`, import.meta.url),
  "utf8",
);

const toolSet = (source, flag, nextFlag) => {
  const match = source.match(new RegExp(
    `${flag}\\s+([\\s\\S]*?)\\s+${nextFlag}\\b`,
  ));
  assert.ok(match, `${flag} must be followed by ${nextFlag}`);
  const tools = [...match[1].matchAll(/"([^"]+)"/g)].map((entry) => entry[1]);
  const unparsed = match[1].replaceAll(/"[^"]+"/g, "").replaceAll(/[\s\\]/g, "");
  assert.equal(unparsed, "", `${flag} must contain only quoted tool names`);
  return tools.sort();
};

const expected = {
  "feedback-triage.yml.tmpl": {
    allowed: [
      "Bash(scripts/ticket_card.sh:*)",
      "Bash(gh issue:*)",
      "Bash(gh pr list:*)",
      "Bash(gh pr view:*)",
      "Read",
      "mcp__e2a__list_messages",
      "mcp__e2a__get_message",
      "mcp__e2a__get_conversation",
      "mcp__e2a__list_conversations",
      "mcp__e2a__get_attachment",
      "mcp__e2a__whoami",
    ].sort(),
    disallowed: [
      "Bash(gh auth:*)",
      "Bash(gh api:*)",
      "Bash(gh secret:*)",
      "Bash(gh gist:*)",
      "Bash(gh pr merge:*)",
      "mcp__e2a__send_message",
      "mcp__e2a__reply_to_message",
      "mcp__e2a__forward_message",
    ].sort(),
  },
  "feedback-comms.yml.tmpl": {
    allowed: [
      "Bash(scripts/ticket_card.sh:*)",
      "Bash(scripts/comms_send.sh:*)",
      "Bash(gh issue:*)",
      "Read",
      "mcp__e2a__list_messages",
      "mcp__e2a__get_message",
      "mcp__e2a__whoami",
    ].sort(),
    disallowed: [
      "Bash(gh auth:*)",
      "Bash(gh api:*)",
      "Bash(gh secret:*)",
      "Bash(gh gist:*)",
      "mcp__e2a__send_message",
      "mcp__e2a__reply_to_message",
      "mcp__e2a__forward_message",
    ].sort(),
  },
};

test("triage and comms use deny-by-default tool permissions", async () => {
  for (const name of ["feedback-triage.yml.tmpl", "feedback-comms.yml.tmpl"]) {
    const source = await workflow(name);
    assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
    assert.match(source, /--permission-mode\s+dontAsk/);
    assert.match(source, /--tools\s+"Bash,Read"/);
    assert.match(source, /--strict-mcp-config/);
    assert.doesNotMatch(source, /--allowedTools[\s\\]+(?:"[^"]+"\s+)*"Bash"(?:\s|\\)/);
    assert.doesNotMatch(source, /"Bash\((?:curl|wget|env|printenv):\*\)"/);
    assert.deepEqual(
      toolSet(source, "--allowedTools", "--disallowedTools"),
      expected[name].allowed,
      `${name} --allowedTools changed`,
    );
    assert.deepEqual(
      toolSet(source, "--disallowedTools", "--max-turns"),
      expected[name].disallowed,
      `${name} --disallowedTools changed`,
    );
  }
});

test("the fix lane exposes only its documented built-ins", async () => {
  const source = await workflow("feedback-fix.yml.tmpl");
  assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
  assert.match(source, /--permission-mode\s+dontAsk/);
  assert.match(source, /--tools\s+"Bash,Edit,Write,Read,Glob,Grep"/);
  assert.match(source, /--strict-mcp-config/);
});
