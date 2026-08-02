import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";

import {
  buildRuntimeInvocation,
  buildRuntimePrompt,
  runRuntimeInvocation,
  sanitizeRuntimeEnvironment,
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
      sandbox: "native",
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

test("OpenClaw adapter uses the documented embedded headless command", () => {
  const invocation = buildRuntimeInvocation(policy("openclaw"), context, {
    environment: { PATH: "/usr/bin", HOME: "/home/operator" },
  });

  assert.deepEqual(invocation.args, [
    "agent",
    "exec",
    "--message-file",
    "-",
    "--cwd",
    "/srv/autopilot/support",
    "--no-auth-env-only",
    "--timeout",
    "120",
    "--json",
  ]);
  assert.equal(invocation.stdin, invocation.prompt);
});

test("Hermes adapter uses one-shot safe mode without yolo", () => {
  const invocation = buildRuntimeInvocation(policy("hermes"), context, {
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
