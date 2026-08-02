import path from "node:path";
import { spawn } from "node:child_process";

const SAFE_ENVIRONMENT_KEYS = [
  "PATH",
  "HOME",
  "LANG",
  "LC_ALL",
  "LC_CTYPE",
  "TERM",
  "TMPDIR",
  "SSL_CERT_FILE",
  "SSL_CERT_DIR",
];

function required(value, name) {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`${name} is required.`);
  }
  return value.trim();
}

export function sanitizeRuntimeEnvironment(environment, context) {
  const result = {};
  for (const name of SAFE_ENVIRONMENT_KEYS) {
    if (typeof environment?.[name] === "string" && environment[name]) {
      result[name] = environment[name];
    }
  }
  result.AUTOPILOT_JOB_ID = required(context.jobId, "job ID");
  result.AUTOPILOT_JOB_SOCKET = required(context.socketPath, "job socket");
  result.AUTOPILOT_JOB_TOKEN = required(context.token, "job capability");
  return result;
}

function helperCommand(context, command) {
  return `${JSON.stringify(process.execPath)} ${JSON.stringify(
    required(context.helperPath, "job helper path"),
  )} ${command}`;
}

export function buildRuntimePrompt(policy, context) {
  const task = policy?.task || {};
  const objective = required(task.objective, "task objective");
  const instructions = required(task.instructions, "task instructions");
  const messageId = required(context.messageId, "message ID");

  return [
    "You are handling one email job under a least-privilege local gateway.",
    `Job: ${required(context.jobId, "job ID")}`,
    `Current message: ${messageId}`,
    "",
    `Objective: ${objective}`,
    "Task policy:",
    instructions,
    "",
    "Use only these job-scoped mail commands:",
    `- Read current message: ${helperCommand(context, "current-message")}`,
    `- Read current thread: ${helperCommand(context, "current-thread")}`,
    `- Submit an in-thread reply: pipe the reply text to ${helperCommand(context, "reply")}`,
    `- Escalate: pipe a reason to ${helperCommand(context, "escalate")}`,
    `- Finish: pipe a concise summary to ${helperCommand(context, "complete")}`,
    "",
    "Recipients are chosen by the gateway, and owner CC is inserted by policy.",
    "A pending_review reply is a successful handoff to a human; do not resubmit it.",
    "Do not attempt to access any other mailbox message or any mailbox administration operation.",
    "If the request exceeds the task policy, escalate without performing it.",
    "Always call complete after replying or escalating, then exit.",
  ].join("\n");
}

export function buildRuntimeInvocation(
  policy,
  context,
  { environment = process.env } = {},
) {
  const runtime = policy?.runtime || {};
  const adapter = required(runtime.adapter, "runtime adapter").toLowerCase();
  const command = required(runtime.command, "runtime command");
  if (!path.isAbsolute(command)) throw new Error("Runtime command must be an absolute path.");
  const cwd = path.resolve(required(runtime.workdir, "runtime workspace"));
  const prompt = buildRuntimePrompt(policy, context);
  const env = sanitizeRuntimeEnvironment(environment, context);
  const timeoutSeconds = Math.max(1, Math.ceil((context.timeoutMs || 600_000) / 1_000));

  let args;
  let stdin = null;
  switch (adapter) {
    case "claude":
      args = [
        "-p",
        prompt,
        "--no-session-persistence",
        "--safe-mode",
        "--strict-mcp-config",
        "--mcp-config",
        "{}",
        "--permission-mode",
        "dontAsk",
        "--tools",
        "Bash,Read,Grep,Glob",
      ];
      break;
    case "codex":
      args = [
        "exec",
        "--ephemeral",
        "--ignore-user-config",
        "--strict-config",
        "--sandbox",
        "workspace-write",
        "--cd",
        cwd,
        "-",
      ];
      stdin = prompt;
      break;
    case "openclaw":
      args = [
        "agent",
        "exec",
        "--message-file",
        "-",
        "--cwd",
        cwd,
        "--no-auth-env-only",
        "--timeout",
        String(timeoutSeconds),
        "--json",
      ];
      stdin = prompt;
      break;
    case "hermes":
      args = ["--oneshot", prompt, "--safe-mode", "--no-restore-cwd"];
      break;
    case "custom":
      args = [];
      stdin = prompt;
      break;
    default:
      throw new Error(`Unsupported runtime adapter: ${adapter}.`);
  }

  return {
    adapter,
    command,
    args,
    cwd,
    env,
    stdin,
    prompt,
    timeoutMs: context.timeoutMs || 600_000,
  };
}

export function runRuntimeInvocation(
  invocation,
  { maxOutputBytes = 64 * 1024, terminationGraceMs = 2_000 } = {},
) {
  return new Promise((resolve) => {
    const detached = process.platform !== "win32";
    const child = spawn(invocation.command, invocation.args, {
      cwd: invocation.cwd,
      env: invocation.env,
      stdio: ["pipe", "pipe", "pipe"],
      detached,
      windowsHide: true,
    });

    let stdoutBytes = 0;
    let stderrBytes = 0;
    const stdoutChunks = [];
    const stderrChunks = [];
    let stdoutTruncated = false;
    let stderrTruncated = false;
    let timedOut = false;
    let spawnError = null;
    let killTimer;

    function capture(chunk, chunks, currentBytes, setTruncated) {
      const available = Math.max(0, maxOutputBytes - currentBytes);
      if (chunk.length > available) setTruncated();
      if (available > 0) chunks.push(chunk.subarray(0, available));
      return currentBytes + Math.min(chunk.length, available);
    }

    child.stdout.on("data", (chunk) => {
      stdoutBytes = capture(
        chunk,
        stdoutChunks,
        stdoutBytes,
        () => (stdoutTruncated = true),
      );
    });
    child.stderr.on("data", (chunk) => {
      stderrBytes = capture(
        chunk,
        stderrChunks,
        stderrBytes,
        () => (stderrTruncated = true),
      );
    });

    function terminate(signal) {
      try {
        if (detached && child.pid) process.kill(-child.pid, signal);
        else child.kill(signal);
      } catch (error) {
        if (error?.code !== "ESRCH") spawnError ||= error;
      }
    }

    const timeout = setTimeout(() => {
      timedOut = true;
      terminate("SIGTERM");
      killTimer = setTimeout(() => terminate("SIGKILL"), terminationGraceMs);
    }, invocation.timeoutMs);

    child.on("error", (error) => {
      spawnError = error;
    });
    child.on("close", (code, signal) => {
      clearTimeout(timeout);
      if (killTimer) clearTimeout(killTimer);
      resolve({
        code: code ?? -1,
        signal,
        timedOut,
        stdout: Buffer.concat(stdoutChunks).toString("utf8"),
        stderr: Buffer.concat(stderrChunks).toString("utf8"),
        stdoutTruncated,
        stderrTruncated,
        ...(spawnError
          ? { error: { code: spawnError.code || "spawn_error", message: "Runtime process failed." } }
          : {}),
      });
    });

    child.stdin.on("error", () => {});
    child.stdin.end(invocation.stdin ?? "");
  });
}
