import assert from "node:assert/strict";
import { accessSync, chmodSync, constants, mkdirSync, mkdtempSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import {
  buildMacosSandboxProfile,
  buildRuntimeInvocation,
  buildRuntimePrompt,
  resolveOsSandbox,
  runRuntimeInvocation,
  sanitizeRuntimeEnvironment,
  wrapRuntimeInvocation,
} from "../runtime.mjs";

const context = {
  jobId: "job_example",
  messageId: "msg_example",
  socketPath: "/tmp/autopilot/job.sock",
  token: "job_capability_example",
  helperPath: "/opt/e2a/autopilot/job-tool.mjs",
  timeoutMs: 120_000,
};

function policy(adapter) {
  return {
    task: {
      profile: "customer-support",
      objective: "Resolve routine support requests.",
      instructions: "Use approved documentation and escalate billing requests.",
    },
    runtime: {
      adapter,
      command: `/usr/local/bin/${adapter}`,
      workdir: "/srv/autopilot/support",
      sandbox: "custom",
    },
  };
}

test("runtime prompt exposes only job-scoped mail operations", () => {
  const prompt = buildRuntimePrompt(policy("codex"), context);

  assert.match(prompt, /current-message/);
  assert.match(prompt, /current-thread/);
  assert.match(prompt, /reply/);
  assert.match(prompt, /escalate/);
  assert.match(prompt, /complete/);
  assert.match(prompt, /msg_example/);
  assert.doesNotMatch(prompt, /messages list|delete|trash|API key/i);
});

test("sanitizeRuntimeEnvironment does not inherit e2a or unrelated credentials", () => {
  const env = sanitizeRuntimeEnvironment(
    {
      PATH: "/usr/bin:/bin",
      HOME: "/home/operator",
      LANG: "en_US.UTF-8",
      E2A_API_KEY: "must-not-pass",
      E2A_AUTOPILOT_FORWARD_TOKEN: "must-not-pass",
      AWS_SECRET_ACCESS_KEY: "must-not-pass",
      GOOGLE_APPLICATION_CREDENTIALS: "/secret.json",
      GITHUB_TOKEN: "must-not-pass",
      ANTHROPIC_API_KEY: "must-not-pass",
    },
    context,
  );

  assert.deepEqual(env, {
    PATH: "/usr/bin:/bin",
    HOME: "/home/operator",
    LANG: "en_US.UTF-8",
    AUTOPILOT_JOB_ID: "job_example",
    AUTOPILOT_JOB_SOCKET: "/tmp/autopilot/job.sock",
    AUTOPILOT_JOB_TOKEN: "job_capability_example",
  });
});

test("Claude adapter is one-shot, non-persistent, and ignores MCP inheritance", () => {
  const invocation = buildRuntimeInvocation(policy("claude"), context, {
    environment: { PATH: "/usr/bin", HOME: "/home/operator" },
  });

  assert.equal(invocation.command, "/usr/local/bin/claude");
  assert.deepEqual(invocation.args.slice(0, 2), ["-p", invocation.prompt]);
  assert.ok(invocation.args.includes("--no-session-persistence"));
  assert.ok(invocation.args.includes("--strict-mcp-config"));
  assert.ok(invocation.args.includes("--safe-mode"));
  assert.equal(invocation.stdin, null);
});

test("Codex adapter uses an ephemeral workspace sandbox and ignores user config", () => {
  const invocation = buildRuntimeInvocation(policy("codex"), context, {
    environment: { PATH: "/usr/bin", HOME: "/home/operator" },
  });

  assert.deepEqual(invocation.args, [
    "exec",
    "--ephemeral",
    "--ignore-user-config",
    "--strict-config",
    "--sandbox",
    "workspace-write",
    "--cd",
    "/srv/autopilot/support",
    "-",
  ]);
  assert.equal(invocation.stdin, invocation.prompt);
});

test("OpenClaw adapter is unavailable until its invocation flags are verified", () => {
  assert.throws(
    () =>
      buildRuntimeInvocation(policy("openclaw"), context, {
        environment: { PATH: "/usr/bin", HOME: "/home/operator" },
      }),
    /OpenClaw adapter is unavailable: its invocation flags are unverified/,
  );
});

test("Hermes adapter uses one-shot safe mode without yolo", () => {
  const hermesPolicy = policy("hermes");
  hermesPolicy.runtime.sandbox = "custom";
  const invocation = buildRuntimeInvocation(hermesPolicy, context, {
    environment: { PATH: "/usr/bin", HOME: "/home/operator" },
  });

  assert.deepEqual(invocation.args, [
    "--oneshot",
    invocation.prompt,
    "--safe-mode",
    "--no-restore-cwd",
  ]);
  assert.ok(!invocation.args.includes("--yolo"));
});

test("custom adapter receives the prompt on stdin and requires the custom policy acknowledgement", () => {
  const customPolicy = policy("custom");
  customPolicy.runtime.command = "/opt/acme/agent";
  customPolicy.runtime.sandbox = "custom";
  const invocation = buildRuntimeInvocation(customPolicy, context, {
    environment: { PATH: "/usr/bin", HOME: "/home/operator" },
  });

  assert.equal(invocation.command, "/opt/acme/agent");
  assert.deepEqual(invocation.args, []);
  assert.equal(invocation.stdin, invocation.prompt);
  assert.equal(invocation.cwd, path.resolve("/srv/autopilot/support"));
});

test("runRuntimeInvocation supplies only the sanitized environment and stdin", async () => {
  const invocation = {
    command: process.execPath,
    args: [
      "-e",
      "let s=''; process.stdin.on('data',c=>s+=c); process.stdin.on('end',()=>process.stdout.write(JSON.stringify({stdin:s,job:process.env.AUTOPILOT_JOB_ID,e2a:process.env.E2A_API_KEY})))",
    ],
    cwd: "/tmp",
    env: sanitizeRuntimeEnvironment(
      { PATH: process.env.PATH, HOME: process.env.HOME, E2A_API_KEY: "do-not-pass" },
      context,
    ),
    stdin: "job prompt",
    timeoutMs: 2_000,
  };

  const result = await runRuntimeInvocation(invocation);

  assert.equal(result.code, 0);
  assert.equal(result.timedOut, false);
  assert.deepEqual(JSON.parse(result.stdout), {
    stdin: "job prompt",
    job: "job_example",
  });
});

test("runRuntimeInvocation bounds captured output", async () => {
  const result = await runRuntimeInvocation(
    {
      command: process.execPath,
      args: ["-e", "process.stdout.write('x'.repeat(100000))"],
      cwd: "/tmp",
      env: { PATH: process.env.PATH },
      stdin: null,
      timeoutMs: 2_000,
    },
    { maxOutputBytes: 1_024 },
  );

  assert.equal(result.code, 0);
  assert.equal(Buffer.byteLength(result.stdout), 1_024);
  assert.equal(result.stdoutTruncated, true);
});

test("runRuntimeInvocation terminates a timed-out process group", async () => {
  const result = await runRuntimeInvocation(
    {
      command: process.execPath,
      args: ["-e", "setInterval(() => {}, 1000)"],
      cwd: "/tmp",
      env: { PATH: process.env.PATH },
      stdin: null,
      timeoutMs: 30,
    },
    { terminationGraceMs: 20 },
  );

  assert.equal(result.timedOut, true);
  assert.notEqual(result.code, 0);
});

test("macOS sandbox profile denies reads with canonicalized subpaths", () => {
  const profile = buildMacosSandboxProfile({
    denyPaths: [
      { path: "/opt/e2a-autopilot/state", subpath: true },
      { path: "/opt/e2a-autopilot/secrets.json", subpath: false },
    ],
  });

  assert.match(profile, /^\(version 1\)\n\(allow default\)\n/);
  assert.match(profile, /\(deny file-read\* \(subpath "\/opt\/e2a-autopilot\/state"\)\)/);
  assert.match(profile, /\(deny file-read\* \(literal "\/opt\/e2a-autopilot\/secrets\.json"\)\)/);
});

test("resolveOsSandbox falls back to unwrapped when no sandbox tool exists", () => {
  const sandbox = resolveOsSandbox({
    denyPaths: [{ path: "/synthetic/state", subpath: true }],
    maskPath: "/synthetic",
    profilePath: "/synthetic-profile.sb",
    platform: "darwin",
    findExecutable: () => null,
  });

  assert.equal(sandbox, null);
  const invocation = {
    command: "/usr/local/bin/claude",
    args: ["-p", "prompt"],
    cwd: "/srv/autopilot/support",
    env: {},
    stdin: null,
    timeoutMs: 1_000,
  };
  assert.equal(wrapRuntimeInvocation(invocation, sandbox), invocation);
});

test("Linux sandbox masks the install root and re-binds the runtime bundle read-only", () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-bwrap-"));
  const installRoot = path.join(root, "install");
  const bundle = path.join(installRoot, "runtime");
  mkdirSync(bundle, { recursive: true });

  const sandbox = resolveOsSandbox({
    denyPaths: [],
    maskPath: installRoot,
    allowPaths: [bundle, path.join(root, "nonexistent-bundle")],
    platform: "linux",
    findExecutable: (name) => (name === "bwrap" ? "/usr/bin/bwrap" : null),
  });

  assert.equal(sandbox.tool, "bwrap");
  assert.deepEqual(sandbox.args.slice(0, 4), ["--dev-bind", "/", "/", "--tmpfs"]);
  assert.equal(sandbox.args[4], installRoot);
  const joined = sandbox.args.join(" ");
  assert.match(joined, new RegExp(`--ro-bind ${bundle} ${bundle}`.replaceAll("/", "\\/")));
  assert.doesNotMatch(joined, /nonexistent-bundle/);
  assert.equal(sandbox.args.at(-1), "--");

  const wrapped = wrapRuntimeInvocation(
    { command: "/usr/local/bin/codex", args: ["exec", "-"], cwd: "/srv", env: {}, stdin: null, timeoutMs: 1_000 },
    sandbox,
  );
  assert.equal(wrapped.command, "/usr/bin/bwrap");
  assert.deepEqual(wrapped.args.slice(-3), ["/usr/local/bin/codex", "exec", "-"]);
});

test("runtime child cannot read files outside the workspace", async (t) => {
  if (process.platform !== "darwin" || !findSandboxExec()) {
    t.skip("sandbox-exec is not available on this machine");
    return;
  }

  const root = mkdtempSync(path.join(tmpdir(), "autopilot-sandbox-"));
  const installRoot = path.join(root, "install");
  const workdir = path.join(root, "workspace");
  mkdirSync(installRoot, { recursive: true });
  mkdirSync(workdir, { recursive: true });
  const credential = path.join(installRoot, "secrets.json");
  writeFileSync(credential, '{"apiKey":"synthetic-credential"}\n', { mode: 0o600 });
  chmodSync(credential, 0o600);
  writeFileSync(path.join(workdir, "notes.txt"), "workspace file\n");

  const profilePath = path.join(root, "job.sb");
  const sandbox = resolveOsSandbox({
    denyPaths: [
      { path: installRoot, subpath: true },
      { path: credential, subpath: false },
    ],
    profilePath,
    platform: "darwin",
  });
  assert.equal(sandbox?.tool, "sandbox-exec");
  assert.equal(statSync(profilePath).mode & 0o777, 0o600);

  const wrap = (command, args) =>
    wrapRuntimeInvocation(
      { command, args, cwd: workdir, env: { PATH: process.env.PATH }, stdin: null, timeoutMs: 5_000 },
      sandbox,
    );

  const denied = await runRuntimeInvocation(wrap("/bin/cat", [credential]));
  assert.notEqual(denied.code, 0, "sandboxed child must not read the credential file");
  assert.doesNotMatch(denied.stdout, /synthetic-credential/);
  assert.match(denied.stderr, /Operation not permitted/i);

  const allowed = await runRuntimeInvocation(wrap("/bin/cat", [path.join(workdir, "notes.txt")]));
  assert.equal(allowed.code, 0, allowed.stderr);
  assert.match(allowed.stdout, /workspace file/);
});

function findSandboxExec() {
  try {
    accessSync("/usr/bin/sandbox-exec", constants.X_OK);
    return true;
  } catch {
    return false;
  }
}
