import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";

const skillFile = "plugins/e2a/skills/email-evals/SKILL.md";
const templateDirectory = "plugins/e2a/skills/email-evals/templates";
const manifestFiles = [
  "plugins/e2a/.claude-plugin/plugin.json",
  "plugins/e2a/.codex-plugin/plugin.json",
  "plugins/e2a/.cursor-plugin/plugin.json",
  ".claude-plugin/marketplace.json",
  ".cursor-plugin/marketplace.json",
];
const authoringPromptLabels = [
  "use case",
  "existing target runtime",
  "dedicated actor environment name",
  "dedicated target environment name",
  "expected action",
  "exact allowed recipients",
  "sender",
  "Reply-To",
  "thread",
  "subject",
  "required facts",
  "forbidden patterns",
  "attachments",
  "timeout",
  "lifecycle",
];
const authoringPromptsStart = "<!-- email-evals:authoring-prompts:start -->";
const authoringPromptsEnd = "<!-- email-evals:authoring-prompts:end -->";
const authoringPromptTokens = [
  ["use case"],
  ["target runtime"],
  ["actor environment"],
  ["target environment"],
  ["action"],
  ["recipient"],
  ["sender"],
  ["Reply-To"],
  ["thread"],
  ["subject"],
  ["fact"],
  ["pattern"],
  ["attachment"],
  ["timeout"],
  ["lifecycle"],
];

async function emailEvalFiles(directory = templateDirectory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const nested = await Promise.all(entries.map(async (entry) => {
    const file = path.join(directory, entry.name);
    return entry.isDirectory() ? emailEvalFiles(file) : [file];
  }));
  return [skillFile, ...nested.flat()];
}

function assertBefore(source, earlier, later) {
  assert.ok(source.indexOf(earlier) >= 0, `missing ${earlier}`);
  assert.ok(source.indexOf(later) >= 0, `missing ${later}`);
  assert.ok(source.indexOf(earlier) < source.indexOf(later), `${earlier} must precede ${later}`);
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function authoringPromptItem(source) {
  return /^\s*(?:\d+\.|[-*])\s+(?:\*\*[^*\n]+\*\*\s+—\s+)?(?:Ask:\s*)?.*\?/m.test(source);
}

/** Parse and validate the sole canonical authoring prompt block without I/O. */
function parseAuthoringPrompts(source) {
  const startCount = source.split(authoringPromptsStart).length - 1;
  const endCount = source.split(authoringPromptsEnd).length - 1;
  assert.equal(startCount, 1, "authoring prompts require exactly one marked block start");
  assert.equal(endCount, 1, "authoring prompts require exactly one marked block end");

  const start = source.indexOf(authoringPromptsStart);
  const end = source.indexOf(authoringPromptsEnd);
  assert.ok(start < end, "authoring prompt markers must be in order");
  const outside = `${source.slice(0, start)}${source.slice(end + authoringPromptsEnd.length)}`;
  assert.equal(authoringPromptItem(outside), false, "authoring question item exists outside canonical block");

  const section = source.slice(start + authoringPromptsStart.length, end).trim();
  const prompts = section.split("\n").map((line) => {
    const item = line.match(/^(\d+)\. \*\*([^*]+)\*\* — (.+)$/);
    assert.ok(item, `invalid authoring prompt item: ${line}`);
    return { number: Number(item[1]), label: item[2], prompt: item[3] };
  });
  assert.equal(prompts.length, authoringPromptLabels.length);
  assert.deepEqual(prompts.map(({ label }) => label), authoringPromptLabels);
  for (const [index, { number, label, prompt }] of prompts.entries()) {
    assert.equal(number, index + 1);
    assert.match(prompt, new RegExp(escapeRegExp(label), "i"));
    assert.doesNotMatch(prompt, /\*\*/, `${label} must contain one label`);
    assert.equal([...prompt.matchAll(/\?/g)].length, 1, `${label} must be one question`);
    for (const token of authoringPromptTokens.flatMap((tokens, tokenIndex) => tokenIndex === index ? [] : tokens)) {
      assert.doesNotMatch(prompt, new RegExp(`\\b${escapeRegExp(token)}\\b`, "i"), `${label} mentions ${token}`);
    }
  }
  return prompts;
}

test("authoring prompt contract rejects noncanonical blocks and bundled mutations", async () => {
  const source = await readFile(skillFile, "utf8");
  const senderPrompt = "1. **sender** — Ask: “What is the sender?”";
  const mutations = [
    ["duplicate marker pair", `${source}\n${authoringPromptsStart}\n${authoringPromptsEnd}`],
    ["second bundled marked block", `${source}\n${authoringPromptsStart}\n${senderPrompt}\n${authoringPromptsEnd}`],
    ["question before canonical block", source.replace(authoringPromptsStart, `${senderPrompt}\n${authoringPromptsStart}`)],
    ["question after canonical block", source.replace(authoringPromptsEnd, `${authoringPromptsEnd}\n${senderPrompt}`)],
    ["duplicate label", source.replace("2. **existing target runtime**", "2. **use case**")],
    ["reordered label", source.replace(
      "1. **use case** — Ask: “What is the synthetic use case?”\n2. **existing target runtime** — Ask: “Does the existing target runtime already run?”",
      "1. **existing target runtime** — Ask: “Does the existing target runtime already run?”\n2. **use case** — Ask: “What is the synthetic use case?”",
    )],
    ["missing label", source.replace("15. **lifecycle** — Ask: “What lifecycle outcome is required?”\n", "")],
    ["prompt mentions another field", source.replace("What is the synthetic use case?", "What use case should the sender handle?")],
  ];

  for (const [name, mutated] of mutations) {
    assert.throws(() => parseAuthoringPrompts(mutated), undefined, name);
  }
});

test("email-evals skill preserves the safe authoring and run sequence", async () => {
  const source = await readFile(skillFile, "utf8");

  assert.match(source, /^---\nname: email-evals\ndescription: .+\n---/m);
  assert.match(source, /Ask one logical question at a time/i);
  assert.match(source, /do not dump a questionnaire/i);
  assert.match(source, /(?:one prompt|one logical question)[^.]*wait[^.]*answer[^.]*advance/i);
  assert.match(source, /only when an earlier answer proves.*irrelevant/i);
  const prompts = parseAuthoringPrompts(source);
  assert.equal(prompts.length, authoringPromptLabels.length);
  assert.deepEqual(prompts.map(({ label }) => label), authoringPromptLabels);

  assert.match(source, /refuse.*(?:customer|production-derived).*?(?:message|identifier|domain|fixture)/is);
  assert.match(source, /synthetic replacement/i);
  assert.match(source, /does not build or start the target agent runtime/i);
  assert.match(source, /never.*change.*protection/is);
  assert.match(source, /same dedicated account/i);
  assert.match(source, /account-scoped API key/i);
  assert.match(source, /actor.*allowlist\/block\s*\[target\]/i);
  assert.match(source, /target.*allowlist\/block\s*\[actor, probes\.\.\.\]/i);

  assert.match(source, /email-evals\.sh scaffold --root <suite-root> --name <suite-name> --target-env <[^>]+> --actor-env <[^>]+> --api-key-env <[^>]+>/);
  assert.match(source, /email-evals\.sh setup --root <suite-root>/);
  assert.match(source, /email-evals\.sh validate --suite <suite-root>\/suite\.yaml/);
  assert.match(source, /show the complete alias-only dry-run plan and protection failures/i);
  assert.match(source, /ask for explicit user approval immediately before.*`?run`?/is);
  assert.match(source, /sends real email between the dedicated agents/i);
  const scaffoldFlow = source.match(/## Scaffold, edit, and validate\n([\s\S]*?)(?=\n## )/)?.[1] ?? "";
  assertBefore(scaffoldFlow, "email-evals.sh scaffold", "email-evals.sh setup");
  assertBefore(scaffoldFlow, "email-evals.sh setup", "email-evals.sh validate");
  assertBefore(scaffoldFlow, "email-evals.sh validate", "dry-run plan");
  assertBefore(source, "## Scaffold, edit, and validate", "## Request approval immediately before sending");
  assertBefore(source, "dry-run plan", "explicit user approval");

  assert.match(source, /read .*report\.md/i);
  assert.match(source, /without hiding errors/i);
  assert.match(source, /smallest case\/agent change/i);
  assert.match(source, /only assertions changed/i);
  assert.match(source, /regrade.*no sends/is);
  for (const limitation of [
    "no semantic judge", "no deep HTML equivalence", "no scheduled-send proof",
    "no full review/bounce/complaint matrix",
  ]) assert.match(source, new RegExp(limitation, "i"));
});

test("templates and skill contain only synthetic email identities", async () => {
  const emailPattern = /[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@([a-z0-9.-]+)/gi;
  const allowedDomains = /(?:^|\.)(?:example\.com|localhost|test|invalid)$/i;

  for (const file of await emailEvalFiles()) {
    const source = await readFile(file, "utf8");
    for (const match of source.matchAll(emailPattern)) {
      assert.match(match[1], allowedDomains, `${file} contains a non-synthetic email identity`);
    }
  }
});

test("all plugin manifests release email-evals together without changing discovery conventions", async () => {
  for (const file of manifestFiles) {
    const manifest = JSON.parse(await readFile(file, "utf8"));
    assert.equal(manifest.version ?? manifest.metadata?.version, "0.7.0", file);
  }

  const claude = JSON.parse(await readFile(manifestFiles[0], "utf8"));
  const codex = JSON.parse(await readFile(manifestFiles[1], "utf8"));
  const cursor = JSON.parse(await readFile(manifestFiles[2], "utf8"));
  assert.equal(Object.hasOwn(claude, "skills"), false);
  assert.equal(codex.skills, "./skills/");
  assert.equal(Object.hasOwn(cursor, "skills"), false);
});
