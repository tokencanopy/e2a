import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";

const tool = path.resolve(import.meta.dirname, "..", "job-tool.mjs");

function runTool(command, { socketPath, token, input = "" }) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [tool, command], {
      env: {
        PATH: process.env.PATH,
        AUTOPILOT_JOB_SOCKET: socketPath,
        AUTOPILOT_JOB_TOKEN: token,
      },
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
    child.stdin.end(input);
  });
}

test("job tool exposes only the five job-scoped gateway operations", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-job-tool-"));
  const socketPath = path.join(root, "gateway.sock");
  const received = [];
  const server = createServer((request, response) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk) => (body += chunk));
    request.on("end", () => {
      received.push({
        method: request.method,
        route: request.url,
        authorization: request.headers.authorization,
        body: body ? JSON.parse(body) : null,
      });
      response.writeHead(200, { "content-type": "application/json" });
      response.end('{"ok":true}\n');
    });
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
  t.after(() => new Promise((resolve) => server.close(resolve)));

  const token = "synthetic_job_capability";
  const cases = [
    ["current-message", "", "GET", "/v1/current-message", null],
    ["current-thread", "", "GET", "/v1/current-thread", null],
    ["reply", "A helpful reply.\n", "POST", "/v1/reply", { text: "A helpful reply." }],
    ["escalate", "Billing decision.\n", "POST", "/v1/escalate", { reason: "Billing decision." }],
    ["complete", "Escalated.\n", "POST", "/v1/complete", { summary: "Escalated." }],
  ];
  for (const [command, input] of cases) {
    const result = await runTool(command, { socketPath, token, input });
    assert.equal(result.code, 0, result.stderr);
    assert.deepEqual(JSON.parse(result.stdout), { ok: true });
  }

  assert.deepEqual(
    received,
    cases.map(([, , method, route, body]) => ({
      method,
      route,
      authorization: `Bearer ${token}`,
      body,
    })),
  );

  const denied = await runTool("delete", { socketPath, token });
  assert.equal(denied.code, 2);
  assert.match(denied.stderr, /unknown command/i);
  assert.equal(received.length, 5);
});

test("job tool requires a capability without printing its value", async () => {
  const result = await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [tool, "current-message"], {
      env: {
        PATH: process.env.PATH,
        AUTOPILOT_JOB_SOCKET: "/tmp/synthetic.sock",
        AUTOPILOT_JOB_TOKEN: "",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stderr = "";
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stderr }));
  });

  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /AUTOPILOT_JOB_TOKEN is required/);
  assert.doesNotMatch(result.stderr, /synthetic_job_capability/);
});
