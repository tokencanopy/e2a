import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import { chmod, copyFile, lstat, mkdir, mkdtemp, readdir, readFile, realpath, rm, stat, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

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

async function installTrackedCorePlugin(destination) {
  const listing = spawnSync("git", ["ls-files", "-z", "--stage", "--", "plugins/e2a"], {
    encoding: "utf8",
  });
  assert.equal(listing.status, 0, listing.stderr);
  const entries = listing.stdout.split("\0").filter(Boolean);
  assert.ok(entries.length > 0, "tracked plugin file list is non-empty");

  for (const entry of entries) {
    const separator = entry.indexOf("\t");
    assert.notEqual(separator, -1, `malformed git index entry: ${entry}`);
    const [mode] = entry.slice(0, separator).split(" ");
    assert.match(mode, /^100(?:644|755)$/, `unsupported tracked plugin mode: ${mode}`);
    const source = entry.slice(separator + 1);
    const relative = source.slice("plugins/e2a/".length);
    assert.ok(relative && !relative.startsWith("../"), `unsafe tracked plugin path: ${source}`);
    const target = path.join(destination, relative);
    await mkdir(path.dirname(target), { recursive: true });
    await copyFile(source, target);
    await chmod(target, mode === "100755" ? 0o755 : 0o644);
  }
}

async function listenLoopback(server) {
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return `http://127.0.0.1:${address.port}`;
}

async function closeServer(server) {
  if (!server.listening) return;
  await new Promise((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
}
/**
 * Compact terminal question-punctuation set: Unicode Other Punctuation (Po)
 * characters whose names identify standalone question marks or question/
 * exclamation combinations, plus question-specific presentation and ornament
 * forms used as punctuation.
 * Math relations, tag characters, and enclosed symbols stay excluded.
 */
const questionCodePoints = new Set([
  0x003f, // QUESTION MARK
  0x00bf, // INVERTED QUESTION MARK
  0x037e, // GREEK QUESTION MARK
  0x055e, // ARMENIAN QUESTION MARK
  0x061f, // ARABIC QUESTION MARK
  0x1367, // ETHIOPIC QUESTION MARK
  0x1945, // LIMBU QUESTION MARK
  0x203d, // INTERROBANG
  0x2047, // DOUBLE QUESTION MARK
  0x2048, // QUESTION EXCLAMATION MARK
  0x2049, // EXCLAMATION QUESTION MARK
  0x2753, // BLACK QUESTION MARK ORNAMENT
  0x2754, // WHITE QUESTION MARK ORNAMENT
  0x2cfa, // COPTIC OLD NUBIAN DIRECT QUESTION MARK
  0x2cfb, // COPTIC OLD NUBIAN INDIRECT QUESTION MARK
  0x2e18, // INVERTED INTERROBANG
  0x2e2e, // REVERSED QUESTION MARK
  0xa60f, // VAI QUESTION MARK
  0xa6f7, // BAMUM QUESTION MARK
  0xfe16, // PRESENTATION FORM FOR VERTICAL QUESTION MARK
  0xfe56, // SMALL QUESTION MARK
  0xff1f, // FULLWIDTH QUESTION MARK
  0x11143, // CHAKMA QUESTION MARK
  0x1e95f, // ADLAM INITIAL QUESTION MARK
  0x1f679, // HEAVY INTERROBANG ORNAMENT
  0x1f67a, // SANS-SERIF INTERROBANG ORNAMENT
  0x1f67b, // HEAVY SANS-SERIF INTERROBANG ORNAMENT
]);
const documentedQuestionPunctuation = [
  ["U+003F QUESTION MARK", "\u{003F}", "&#63;", "&#x3F;"],
  ["U+00BF INVERTED QUESTION MARK", "\u{00BF}", "&#191;", "&#xBF;"],
  ["U+037E GREEK QUESTION MARK", "\u{037E}", "&#894;", "&#x37E;"],
  ["U+055E ARMENIAN QUESTION MARK", "\u{055E}", "&#1374;", "&#x55E;"],
  ["U+061F ARABIC QUESTION MARK", "\u{061F}", "&#1567;", "&#x61F;"],
  ["U+1367 ETHIOPIC QUESTION MARK", "\u{1367}", "&#4967;", "&#x1367;"],
  ["U+1945 LIMBU QUESTION MARK", "\u{1945}", "&#6469;", "&#x1945;"],
  ["U+203D INTERROBANG", "\u{203D}", "&#8253;", "&#x203D;"],
  ["U+2047 DOUBLE QUESTION MARK", "\u{2047}", "&#8263;", "&#x2047;"],
  ["U+2048 QUESTION EXCLAMATION MARK", "\u{2048}", "&#8264;", "&#x2048;"],
  ["U+2049 EXCLAMATION QUESTION MARK", "\u{2049}", "&#8265;", "&#x2049;"],
  ["U+2753 BLACK QUESTION MARK ORNAMENT", "\u{2753}", "&#10067;", "&#x2753;"],
  ["U+2754 WHITE QUESTION MARK ORNAMENT", "\u{2754}", "&#10068;", "&#x2754;"],
  ["U+2CFA COPTIC OLD NUBIAN DIRECT QUESTION MARK", "\u{2CFA}", "&#11514;", "&#x2CFA;"],
  ["U+2CFB COPTIC OLD NUBIAN INDIRECT QUESTION MARK", "\u{2CFB}", "&#11515;", "&#x2CFB;"],
  ["U+2E18 INVERTED INTERROBANG", "\u{2E18}", "&#11800;", "&#x2E18;"],
  ["U+2E2E REVERSED QUESTION MARK", "\u{2E2E}", "&#11822;", "&#x2E2E;"],
  ["U+A60F VAI QUESTION MARK", "\u{A60F}", "&#42511;", "&#xA60F;"],
  ["U+A6F7 BAMUM QUESTION MARK", "\u{A6F7}", "&#42743;", "&#xA6F7;"],
  ["U+FE16 PRESENTATION FORM FOR VERTICAL QUESTION MARK", "\u{FE16}", "&#65046;", "&#xFE16;"],
  ["U+FE56 SMALL QUESTION MARK", "\u{FE56}", "&#65110;", "&#xFE56;"],
  ["U+FF1F FULLWIDTH QUESTION MARK", "\u{FF1F}", "&#65311;", "&#xFF1F;"],
  ["U+11143 CHAKMA QUESTION MARK", "\u{11143}", "&#69955;", "&#x11143;"],
  ["U+1E95F ADLAM INITIAL QUESTION MARK", "\u{1E95F}", "&#125279;", "&#x1E95F;"],
  ["U+1F679 HEAVY INTERROBANG ORNAMENT", "\u{1F679}", "&#128633;", "&#x1F679;"],
  ["U+1F67A SANS-SERIF INTERROBANG ORNAMENT", "\u{1F67A}", "&#128634;", "&#x1F67A;"],
  ["U+1F67B HEAVY SANS-SERIF INTERROBANG ORNAMENT", "\u{1F67B}", "&#128635;", "&#x1F67B;"],
];

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

    if (source.startsWith("&quest;", index)) {
      count += 1;
      index += 7;
      continue;
    }
    if (source.startsWith("&iquest;", index)) {
      count += 1;
      index += 8;
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
  const before = source.slice(0, start);
  const after = source.slice(end + authoringPromptsEnd.length);
  assert.equal(countQuestionPunctuation(before), 0, "question exists before canonical authoring block");
  assert.equal(countQuestionPunctuation(after), 0, "question exists after canonical authoring block");

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
  for (const [name, raw, decimal, hex] of documentedQuestionPunctuation) {
    for (const [form, punctuation] of [["raw", raw], ["decimal", decimal], ["hex", hex]]) {
      assert.throws(
        () => parseAuthoringPrompts(`${source}\n${punctuation}`),
        undefined,
        `${name} ${form} outside block`,
      );
      const secondQuestion = source.replace(
        "What is the synthetic use case?",
        `First question? Second question${punctuation}`,
      );
      assert.throws(
        () => parseAuthoringPrompts(secondQuestion),
        undefined,
        `${name} ${form} as second question`,
      );

      const soleQuestion = source.replace(
        "What is the synthetic use case?",
        `What is the synthetic use case${punctuation}`,
      );
      assert.doesNotThrow(() => parseAuthoringPrompts(soleQuestion), `${name} ${form} as sole question`);
    }
  }

  for (const [name, punctuation] of [
    ["leading-zero decimal reference", "&#000063;"],
    ["upper-case leading-zero hex reference", "&#X00003F;"],
  ]) assert.throws(() => parseAuthoringPrompts(`${source}\n${punctuation}`), undefined, name);
});

test("authoring prompt contract counts only exact supported named question references", async () => {
  const source = await readFile(skillFile, "utf8");
  for (const entity of ["&quest;", "&iquest;"]) {
    assert.throws(() => parseAuthoringPrompts(`${source}\n${entity}`), undefined, `${entity} outside block`);
    const secondQuestion = source.replace(
      "What is the synthetic use case?",
      `First question? Second question${entity}`,
    );
    assert.throws(() => parseAuthoringPrompts(secondQuestion), undefined, `${entity} as second question`);
    const soleQuestion = source.replace("What is the synthetic use case?", `What is the synthetic use case${entity}`);
    assert.doesNotThrow(() => parseAuthoringPrompts(soleQuestion), `${entity} as sole question`);
  }

  for (const entity of ["&QuEsT;", "&Iquest;", "&IQUEST;"]) {
    assert.doesNotThrow(() => parseAuthoringPrompts(`${source}\n${entity}`), `${entity} is case-sensitive`);
    const noQuestion = source.replace("What is the synthetic use case?", `What is the synthetic use case${entity}`);
    assert.throws(() => parseAuthoringPrompts(noQuestion), undefined, `${entity} is not a question`);
    const rawQuestion = source.replace("What is the synthetic use case?", `What is the synthetic use case${entity}?`);
    assert.doesNotThrow(() => parseAuthoringPrompts(rawQuestion), `${entity} does not mask raw punctuation`);
  }
});

test("authoring prompt contract scans pre-block and post-block text independently", async () => {
  const source = await readFile(skillFile, "utf8");
  for (const [name, before, after] of [
    ["split named reference", "&que", "st;"],
    ["split numeric reference", "&#", "63;"],
  ]) {
    const splitEntity = source
      .replace(authoringPromptsStart, `${before}${authoringPromptsStart}`)
      .replace(authoringPromptsEnd, `${authoringPromptsEnd}${after}`);
    assert.doesNotThrow(() => parseAuthoringPrompts(splitEntity), name);
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

  const excludedNonterminalSymbols = [
    ["U+2A7B LESS-THAN WITH QUESTION MARK ABOVE", "\u{2A7B}"],
    ["U+2A7C GREATER-THAN WITH QUESTION MARK ABOVE", "\u{2A7C}"],
    ["U+1FBC4 NEGATIVE SQUARED QUESTION MARK", "\u{1FBC4}"],
    ["U+E003F TAG QUESTION MARK", "\u{E003F}"],
    ["generic emoji", "\u{1F600}"],
    ["general terminal punctuation", "!"],
  ];
  for (const [name, symbol] of excludedNonterminalSymbols) {
    assert.doesNotThrow(() => parseAuthoringPrompts(`${source}\n${symbol}`), `${name} outside block`);
    const noQuestion = source.replace("What is the synthetic use case?", `What is the synthetic use case${symbol}`);
    assert.throws(() => parseAuthoringPrompts(noQuestion), undefined, `${name} does not satisfy the question contract`);
  }

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

  assert.match(source, /\$EMAIL_EVALS_LAUNCHER" scaffold --root <suite-root> --name <suite-name> --target-env <[^>]+> --actor-env <[^>]+>/);
  assert.match(source, /credential name is fixed outside suite authority/i);
  assert.match(source, /never copies or executes\s+JavaScript or dependencies\s+beneath the suite root/i);
  assert.match(source, /--trusted-origin <origin>/);
  assert.match(source, /\$EMAIL_EVALS_LAUNCHER" setup --root <suite-root>/);
  assert.match(source, /\$EMAIL_EVALS_LAUNCHER" validate --suite <suite-root>\/suite\.yaml/);
  assert.match(source, /show the complete alias-only dry-run plan[\s\S]*protection failures/i);
  assert.match(source, /`approvalDigest`/);
  assert.match(source, /\$EMAIL_EVALS_LAUNCHER" run --suite <suite-root>\/suite\.yaml --approval-digest <approvalDigest-from-validate>/);
  assert.match(source, /request fresh approval/i);
  assert.match(source, /ask for explicit user approval immediately before.*`?run`?/is);
  assert.match(source, /sends real email between the dedicated agents/i);
  const scaffoldFlow = source.match(/## Scaffold, edit, and validate\n([\s\S]*?)(?=\n## )/)?.[1] ?? "";
  assertBefore(scaffoldFlow, "$EMAIL_EVALS_LAUNCHER\" scaffold", "$EMAIL_EVALS_LAUNCHER\" setup");
  assertBefore(scaffoldFlow, "$EMAIL_EVALS_LAUNCHER\" setup", "$EMAIL_EVALS_LAUNCHER\" validate");
  assertBefore(scaffoldFlow, "$EMAIL_EVALS_LAUNCHER\" validate", "dry-run plan");
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

test("email-evals resolves its launcher from the loaded skill directory", async () => {
  const skill = await readFile(skillFile, "utf8");
  assert.doesNotMatch(skill, /plugins\/e2a\/skills\/email-evals\/email-evals\.sh/);
  assert.doesNotMatch(skill, /(?:CLAUDE|CODEX|CURSOR)_PLUGIN_ROOT/);
  assert.match(skill, /absolute directory containing this loaded `SKILL\.md`/);
  assert.match(skill, /EMAIL_EVALS_LAUNCHER/);
});

test("email-evals launches from a clean-room installed plugin", async () => {
  const cleanRoom = await mkdtemp(path.join(await realpath(tmpdir()), "email-evals-clean-room-"));
  const installRoot = path.join(cleanRoom, "installed-plugin");
  const unrelatedProject = path.join(cleanRoom, "unrelated-project");
  const installedPlugin = path.join(installRoot, "e2a");
  const skillResource = path.join(installedPlugin, "skills", "email-evals", "SKILL.md");
  const suiteRoot = path.join(unrelatedProject, "evals", "email");
  const environment = {
    PATH: process.env.PATH ?? "",
    TMPDIR: await realpath(tmpdir()),
    E2A_EVAL_API_KEY: "synthetic-key.test",
    E2A_EVAL_TARGET: "target@eval.test",
    E2A_EVAL_ACTOR: "actor@eval.test",
  };
  const requests = [];
  const server = createServer((request, response) => {
    const parsed = new URL(request.url ?? "/", "http://127.0.0.1");
    requests.push({
      method: request.method,
      pathname: parsed.pathname,
      authorization: request.headers.authorization,
    });
    const match = parsed.pathname.match(/^\/v1\/agents\/([^/]+)(\/protection)?$/);
    const email = match ? decodeURIComponent(match[1]) : null;
    const knownAgent = email === environment.E2A_EVAL_ACTOR || email === environment.E2A_EVAL_TARGET;
    if (request.method !== "GET" || !match || !knownAgent
      || request.headers.authorization !== `Bearer ${environment.E2A_EVAL_API_KEY}`) {
      response.writeHead(404, { "content-type": "application/json" });
      response.end(JSON.stringify({ code: "not_found", message: "synthetic route not found" }));
      return;
    }

    response.writeHead(200, { "content-type": "application/json" });
    if (match[2]) {
      const peer = email === environment.E2A_EVAL_ACTOR
        ? environment.E2A_EVAL_TARGET : environment.E2A_EVAL_ACTOR;
      response.end(JSON.stringify({
        holds: {},
        inbound: { gate: { policy: "open" }, scan: {} },
        outbound: {
          gate: { policy: "allowlist", action: "block", allowlist: [peer] },
          scan: {},
        },
      }));
      return;
    }
    response.end(JSON.stringify({
      created_at: "2026-01-01T00:00:00.000Z",
      domain: "eval.test",
      domain_verified: true,
      email,
      name: email === environment.E2A_EVAL_ACTOR ? "Synthetic Actor" : "Synthetic Target",
      registered_domain: "eval.test",
    }));
  });

  try {
    const trustedOrigin = await listenLoopback(server);
    await installTrackedCorePlugin(installedPlugin);
    await mkdir(unrelatedProject, { recursive: true });
    const loadedSkill = await readFile(skillResource, "utf8");
    assert.match(loadedSkill, /EMAIL_EVALS_LAUNCHER/);
    const launcher = path.join(path.dirname(skillResource), "email-evals.sh");
    assert.notEqual((await stat(launcher)).mode & 0o111, 0, "tracked launcher stays executable");
    await assert.rejects(
      lstat(path.join(installedPlugin, "skills", "email-evals", "runtime", "node_modules")),
      (error) => error?.code === "ENOENT",
    );

    const invoke = (args) => execFileAsync(launcher, args, {
      cwd: unrelatedProject,
      env: environment,
      encoding: "utf8",
    });
    const help = await invoke(["--help"]);
    assert.match(help.stdout, /email-evals validate/);

    const scaffold = await invoke([
      "scaffold", "--root", suiteRoot, "--name", "fictional-support-smoke",
      "--target-env", "E2A_EVAL_TARGET", "--actor-env", "E2A_EVAL_ACTOR",
    ]);
    assert.match(scaffold.stdout, /^created README\.md$/m);
    assert.match(scaffold.stdout, /^created suite\.yaml$/m);

    const setup = await invoke(["setup", "--root", suiteRoot]);
    assert.match(setup.stdout, /Prepared trusted email eval runtime/);

    const suiteFile = path.join(suiteRoot, "suite.yaml");
    const suite = await readFile(suiteFile, "utf8");
    const loopbackSuite = suite.replace(
      "  adapter: e2a\n",
      `  adapter: e2a\n  base_url: ${trustedOrigin}\n`,
    );
    assert.notEqual(loopbackSuite, suite, "scaffolded suite receives the loopback base URL");
    await writeFile(suiteFile, loopbackSuite);

    const validate = await invoke([
      "validate", "--suite", suiteFile, "--trusted-origin", trustedOrigin, "--json",
    ]);
    const output = JSON.parse(validate.stdout);
    assert.equal(output.command, "validate");
    assert.equal(output.plan.networkSends, false);
    assert.deepEqual(output.plan.recipientAliases, ["actor", "target"]);
    assert.doesNotMatch(JSON.stringify(output.plan), /@|actor@eval\.test|target@eval\.test/);
    assert.deepEqual(
      requests,
      [
        "/v1/agents/actor%40eval.test",
        "/v1/agents/target%40eval.test",
        "/v1/agents/actor%40eval.test/protection",
        "/v1/agents/target%40eval.test/protection",
      ].map((pathname) => ({
        method: "GET", pathname, authorization: `Bearer ${environment.E2A_EVAL_API_KEY}`,
      })),
    );
  } finally {
    await closeServer(server);
    await rm(cleanRoom, { recursive: true, force: true });
  }
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
    assert.equal(manifest.version ?? manifest.metadata?.version, "0.9.2", file);
  }

  const claude = JSON.parse(await readFile(manifestFiles[0], "utf8"));
  const codex = JSON.parse(await readFile(manifestFiles[1], "utf8"));
  const cursor = JSON.parse(await readFile(manifestFiles[2], "utf8"));
  assert.equal(Object.hasOwn(claude, "skills"), false);
  assert.equal(codex.skills, "./skills/");
  assert.equal(Object.hasOwn(cursor, "skills"), false);
});
