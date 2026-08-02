#!/usr/bin/env node

import http from "node:http";

const MAX_INPUT_BYTES = 1024 * 1024;

function requiredEnvironment(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required.`);
  return value;
}

async function readInput() {
  let size = 0;
  const chunks = [];
  for await (const chunk of process.stdin) {
    size += chunk.length;
    if (size > MAX_INPUT_BYTES) throw new Error("Job tool input is too large.");
    chunks.push(chunk);
  }
  const value = Buffer.concat(chunks).toString("utf8").trim();
  if (!value) throw new Error("This job operation requires non-empty text on stdin.");
  return value;
}

function request(socketPath, token, { method, route, body }) {
  return new Promise((resolve, reject) => {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    const operation = http.request(
      {
        socketPath,
        path: route,
        method,
        headers: {
          authorization: `Bearer ${token}`,
          accept: "application/json",
          ...(payload
            ? {
                "content-type": "application/json",
                "content-length": Buffer.byteLength(payload),
              }
            : {}),
        },
      },
      (response) => {
        let size = 0;
        const chunks = [];
        response.on("data", (chunk) => {
          size += chunk.length;
          if (size > MAX_INPUT_BYTES) {
            response.destroy(new Error("Gateway response is too large."));
            return;
          }
          chunks.push(chunk);
        });
        response.on("error", reject);
        response.on("end", () => {
          let value;
          try {
            value = JSON.parse(Buffer.concat(chunks).toString("utf8"));
          } catch {
            reject(new Error(`Job gateway returned invalid JSON (status ${response.statusCode}).`));
            return;
          }
          if (response.statusCode < 200 || response.statusCode >= 300) {
            reject(new Error(`Job gateway rejected the operation (status ${response.statusCode}).`));
            return;
          }
          resolve(value);
        });
      },
    );
    operation.on("error", reject);
    if (payload) operation.write(payload);
    operation.end();
  });
}

async function operationFor(command) {
  switch (command) {
    case "current-message":
      return { method: "GET", route: "/v1/current-message" };
    case "current-thread":
      return { method: "GET", route: "/v1/current-thread" };
    case "reply":
      return { method: "POST", route: "/v1/reply", body: { text: await readInput() } };
    case "escalate":
      return { method: "POST", route: "/v1/escalate", body: { reason: await readInput() } };
    case "complete":
      return { method: "POST", route: "/v1/complete", body: { summary: await readInput() } };
    default: {
      const error = new Error(`Unknown command: ${command || "(missing)"}.`);
      error.exitCode = 2;
      throw error;
    }
  }
}

async function main() {
  const operation = await operationFor(process.argv[2]);
  const socketPath = requiredEnvironment("AUTOPILOT_JOB_SOCKET");
  const token = requiredEnvironment("AUTOPILOT_JOB_TOKEN");
  const result = await request(socketPath, token, operation);
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

main().catch((error) => {
  process.stderr.write(`autopilot job tool: ${error.message}\n`);
  process.exitCode = error.exitCode || 1;
});
