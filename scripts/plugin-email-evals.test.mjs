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
const authoringPromptFields = [
  "use-case",
  "existing-target-runtime",
  "dedicated-actor-environment-name",
  "dedicated-target-environment-name",
  "expected-action",
  "exact-allowed-recipients",
  "sender",
  "reply-to",
  "thread",
  "subject",
  "required-facts",
  "forbidden-patterns",
  "attachments",
  "timeout",
  "lifecycle",
];
const authoringPromptsStart = "<!-- email-evals:authoring-prompts:start -->";
const authoringPromptsEnd = "<!-- email-evals:authoring-prompts:end -->";
const fieldMarker = (field) => `<!-- email-evals:field=${field} -->`;
const questionCodePoints = new Set([0x3f, 0x37e, 0x61f, 0x2e2e, 0xfe56, 0xff1f]);

/** Count literal and HTML-encoded question punctuation in one bounded linear scan. */
function countQuestionPunctuation(source) {
  let count = 0;
  for (let index = 0; index < source.length;) {
    const codePoint = source.codePointAt(index);
    if (codePoint !== 0x26) {
      if (questionCodePoints.has(codePoint)) count += 1;
      index += codePoint > 0xffff ? 2 : 1;
      continue;
    }

    if (source.slice(index, index + 7).toLowerCase() === "&quest;") {
      count += 1;
      index += 7;
      continue;
    }
    if (source.charCodeAt(index + 1) !== 0x23) {
      index += 1;
      continue;
    }

    let cursor = index + 2;
    let radix = 10;
    if (source[cursor] === "x" || source[cursor] === "X") {
      radix = 16;
      cursor += 1;
    }
    let digits = 0;
    let value = 0;
    let overflow = false;
    while (cursor < source.length) {
      const code = source.charCodeAt(cursor);
      let digit = -1;
      if (code >= 0x30 && code <= 0x39) digit = code - 0x30;
      else if (radix === 16 && code >= 0x41 && code <= 0x46) digit = code - 0x41 + 10;
      else if (radix === 16 && code >= 0x61 && code <= 0x66) digit = code - 0x61 + 10;
      if (digit < 0 || digit >= radix) break;

      digits += 1;
      if (!overflow) {
        if (value > Math.floor((0x10ffff - digit) / radix)) overflow = true;
        else value = value * radix + digit;
      }
      cursor += 1;
    }

    const validScalar = !overflow && !(value >= 0xd800 && value <= 0xdfff);
    if (digits > 0 && validScalar && questionCodePoints.has(value)) count += 1;
    index = cursor + (source[cursor] === ";" ? 1 : 0);
  }
  return count;
}

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
  assert.equal(countQuestionPunctuation(outside), 0, "question exists outside canonical authoring block");

  const section = source.slice(start + authoringPromptsStart.length, end).trim();
  const prompts = section.split("\n").map((line) => {
    const item = line.match(/^(\d+)\. (<!-- email-evals:field=([a-z0-9-]+) -->) \*\*([^*]+)\*\* — (.+)$/);
    assert.ok(item, `invalid authoring prompt item: ${line}`);
    return {
      number: Number(item[1]), marker: item[2], field: item[3], label: item[4], question: item[5], line,
    };
  });
  assert.equal(prompts.length, authoringPromptFields.length);
  assert.deepEqual(prompts.map(({ field }) => field), authoringPromptFields);
  for (const [index, { number, marker, field, line }] of prompts.entries()) {
    assert.equal(number, index + 1);
    assert.equal(marker, fieldMarker(field));
    assert.equal([...line.matchAll(/<!-- email-evals:field=[a-z0-9-]+ -->/g)].length, 1, `${field} has one marker`);
    assert.equal(countQuestionPunctuation(line), 1, `${field} has one question`);
    assert.doesNotMatch(line, /email-evals:authoring-prompts:(?:start|end)/, `${field} has no nested block`);
  }
  const markers = [...source.matchAll(/<!-- email-evals:field=([a-z0-9-]+) -->/g)];
  assert.equal(markers.length, authoringPromptFields.length, "field markers appear exactly once globally");
  assert.deepEqual(markers.map((match) => match[1]), authoringPromptFields);
  for (const [index, match] of markers.entries()) {
    assert.ok(match.index > start && match.index < end, `${authoringPromptFields[index]} marker is inside canonical block`);
  }
  return prompts;
}

test("authoring prompt contract rejects noncanonical blocks and field mutations", async () => {
  const source = await readFile(skillFile, "utf8");
  const senderPrompt = `1. ${fieldMarker("sender")} **sender** — Ask: “What is the sender?”`;
  const reordered = source
    .replace(fieldMarker("use-case"), "__FIELD_SWAP__")
    .replace(fieldMarker("existing-target-runtime"), fieldMarker("use-case"))
    .replace("__FIELD_SWAP__", fieldMarker("existing-target-runtime"));
  const mutations = [
    ["duplicate marker pair", `${source}\n${authoringPromptsStart}\n${authoringPromptsEnd}`],
    ["second bundled marked block", `${source}\n${authoringPromptsStart}\n${senderPrompt}\n${authoringPromptsEnd}`],
    ["duplicate field marker", source.replace(fieldMarker("existing-target-runtime"), fieldMarker("use-case"))],
    ["reordered field marker", reordered],
    ["missing field marker", source.replace(`${fieldMarker("lifecycle")} `, "")],
    ["second field marker", source.replace(fieldMarker("use-case"), `${fieldMarker("use-case")} ${fieldMarker("sender")}`)],
    ["multiple questions", source.replace("What is the synthetic use case?", "What is the synthetic use case??")],
    ["question in label", source.replace("**use case**", "**use case&#63;**")],
  ];

  for (const [name, mutated] of mutations) {
    assert.throws(() => parseAuthoringPrompts(mutated), undefined, name);
  }
});

test("authoring prompt contract rejects hidden questions and permits question wording changes", async () => {
  const source = await readFile(skillFile, "utf8");
  for (const [name, hidden] of [
    ["plus item", "+ Hidden authoring question?"],
    ["HTML item", "<li>Hidden authoring question?</li>"],
    ["comment", "<!-- Hidden authoring question? -->"],
    ["fenced code", "```text\nHidden authoring question?\n```"],
    ["plain text", "Hidden authoring question?"],
  ]) {
    assert.throws(() => parseAuthoringPrompts(`${source}\n${hidden}`), undefined, name);
  }
  const rewrites = [
    source.replace("What is the synthetic use case?", "Which fictional scenario should the agent handle?"),
    source.replace("What is the sender?", "Which mailbox originates this evaluation?"),
  ];
  for (const rewritten of rewrites) assert.doesNotThrow(() => parseAuthoringPrompts(rewritten));
});

test("authoring prompt contract counts encoded and Unicode question punctuation", async () => {
  const source = await readFile(skillFile, "utf8");
  const questionSpellings = [
    ["decimal reference", "&#63;"],
    ["hex reference", "&#x3f;"],
    ["named reference", "&quest;"],
    ["full-width question mark", "？"],
    ["leading-zero decimal reference", "&#000063;"],
    ["upper-case leading-zero hex reference", "&#X00003F;"],
    ["mixed-case named reference", "&QuEsT;"],
    ["small question mark", "﹖"],
    ["Arabic question mark", "؟"],
    ["Greek question mark", ";"],
  ];

  for (const [name, punctuation] of questionSpellings) {
    assert.throws(() => parseAuthoringPrompts(`${source}\n${punctuation}`), undefined, `${name} outside block`);
    const secondQuestion = source.replace(
      "What is the synthetic use case?",
      `First question? Second question${punctuation}`,
    );
    assert.throws(() => parseAuthoringPrompts(secondQuestion), undefined, `${name} as second question`);

    const soleQuestion = source.replace("What is the synthetic use case?", `What is the synthetic use case${punctuation}`);
    assert.doesNotThrow(() => parseAuthoringPrompts(soleQuestion), `${name} as sole question`);
  }
});

test("authoring prompt question counting handles malformed and entity-heavy input safely", async () => {
  const source = await readFile(skillFile, "utf8");
  const plausibleMalformedQuestions = [
    "&#63",
    "&#x3f",
    "&#00063suffix;",
    "&#X0003Fsuffix;",
    `&#${"0".repeat(4096)}63;`,
    `&#x${"0".repeat(4096)}3F;`,
  ];
  for (const entity of plausibleMalformedQuestions) {
    assert.throws(() => parseAuthoringPrompts(`${source}\n${entity}`), undefined, entity);
  }

  const unrelatedEntities = [
    "&amp;",
    "&#65;",
    "&#x1F600;",
    "&#x110000;",
    "&#55296;",
    "&#xD800;",
    `&#${"9".repeat(4096)};`,
  ];
  assert.doesNotThrow(() => parseAuthoringPrompts(`${source}\nProse with ${unrelatedEntities.join(" ")}`));

  const entityHeavyProse = `${source}\n${"&amp; &#65; &#x41; &#x110000; &#xD800; ".repeat(10_000)}`;
  assert.doesNotThrow(() => parseAuthoringPrompts(entityHeavyProse));
});

test("email-evals skill preserves the safe authoring and run sequence", async () => {
  const source = await readFile(skillFile, "utf8");

  assert.match(source, /^---\nname: email-evals\ndescription: .+\n---/m);
  assert.match(source, /Ask one logical question at a time/i);
  assert.match(source, /do not dump a questionnaire/i);
  assert.match(source, /(?:one prompt|one logical question)[^.]*wait[^.]*answer[^.]*advance/i);
  assert.match(source, /only when an earlier answer proves.*irrelevant/i);
  const prompts = parseAuthoringPrompts(source);
  assert.equal(prompts.length, authoringPromptFields.length);
  assert.deepEqual(prompts.map(({ field }) => field), authoringPromptFields);

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
