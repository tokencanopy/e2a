import { execFile } from "node:child_process";
import { randomBytes } from "node:crypto";
import { createServer, type IncomingMessage, type Server } from "node:http";
import { loadConfig, saveConfig } from "../config.js";

/**
 * Render the URL's host for human-facing log lines. Falls back to the raw
 * input if it isn't a valid URL — better than throwing in the middle of
 * a status message.
 */
function hostLabel(apiUrl: string): string {
  try {
    return new URL(apiUrl).host;
  } catch {
    return apiUrl;
  }
}

const LOGIN_TIMEOUT_MS = 2 * 60 * 1000;

function openBrowser(url: string): void {
  if (process.platform === "win32") {
    const child = execFile("cmd", ["/c", "start", "", url], (err) => {
      if (err) {
        process.stderr.write(`Could not open browser. Visit manually: ${url}\n`);
      }
    });
    child.unref();
    return;
  }

  const cmd = process.platform === "darwin" ? "open" : "xdg-open";
  const child = execFile(cmd, [url], (err) => {
    if (err) {
      process.stderr.write(`Could not open browser. Visit manually: ${url}\n`);
    }
  });
  child.unref();
}

function renderCallbackPage(title: string, message: string): string {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${title}</title>
  <style>
    body {
      margin: 0;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: #f5f7fb;
      color: #111827;
      display: grid;
      place-items: center;
      min-height: 100vh;
      padding: 24px;
    }
    main {
      width: 100%;
      max-width: 460px;
      background: white;
      border: 1px solid #e5e7eb;
      border-radius: 16px;
      padding: 28px;
      box-shadow: 0 18px 50px rgba(15, 23, 42, 0.08);
    }
    h1 { margin: 0 0 10px; font-size: 24px; }
    p { margin: 0; color: #4b5563; line-height: 1.5; }
  </style>
</head>
<body>
  <main>
    <h1>${title}</h1>
    <p>${message}</p>
  </main>
</body>
</html>`;
}

function readRequestBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (chunk) => {
      body += chunk;
    });
    req.on("end", () => resolve(body));
    req.on("error", reject);
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve) => {
    if (!server.listening) {
      resolve();
      return;
    }
    server.close(() => resolve());
  });
}

function buildBrowserLoginURL(apiUrl: string, callbackUrl: string, cliState: string): string {
  // Keep the legacy Google door as the deployment-agnostic default. The OIDC
  // door supports the same cli_callback/cli_state handoff, but the server
  // exposes no public auth-methods endpoint the CLI can consult to discover
  // which optional door an operator configured for the static dashboard.
  const loginUrl = new URL("/api/auth/login", apiUrl);
  loginUrl.searchParams.set("cli_callback", callbackUrl);
  loginUrl.searchParams.set("cli_state", cliState);
  return loginUrl.toString();
}

type BrowserLoginResult = {
  apiKey: string;
  agentEmail: string;
};

async function waitForBrowserLogin(apiUrl: string, timeoutMs = LOGIN_TIMEOUT_MS): Promise<BrowserLoginResult> {
  const cliState = randomBytes(16).toString("hex");

  let settled = false;
  let server: Server | null = null;
  // Single timeout handle that's cleared in finish() and the outer
  // finally — earlier versions had two setTimeouts and one of them
  // wasn't cleared on success, keeping the Node event loop alive for
  // the full timeout (2 min) after a successful login. Symptom was
  // the terminal hanging after "Logged in to <host>" printed.
  let timeoutHandle: ReturnType<typeof setTimeout> | undefined;

  try {
    return await new Promise<BrowserLoginResult>((resolve, reject) => {
      const finish = async (value: BrowserLoginResult | Error, isError: boolean) => {
        if (settled) return;
        settled = true;
        if (timeoutHandle) clearTimeout(timeoutHandle);
        if (server) {
          await closeServer(server);
        }
        if (isError) reject(value);
        else resolve(value as BrowserLoginResult);
      };

      timeoutHandle = setTimeout(
        () => void finish(new Error("timed out waiting for browser login"), true),
        timeoutMs,
      );

      server = createServer(async (req, res) => {
        res.setHeader("Connection", "close");
        try {
          if (req.method !== "POST") {
            res.statusCode = 405;
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            res.end(renderCallbackPage("Return to your terminal", "Run e2a login again to finish connecting the CLI."));
            return;
          }

          const body = await readRequestBody(req);
          const params = new URLSearchParams(body);
          const returnedState = params.get("cli_state") || "";
          const apiKey = params.get("api_key") || "";
          const agentEmail = params.get("agent_email") || "";
          const error = params.get("error") || "";

          if (returnedState != cliState) {
            res.statusCode = 400;
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            res.end(renderCallbackPage("e2a login failed", "The browser login state did not match this terminal session. Return to the terminal and try again."));
            await finish(new Error("browser login state mismatch"), true);
            return;
          }

          if (error) {
            res.statusCode = 400;
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            res.end(renderCallbackPage("e2a login failed", error));
            await finish(new Error(error), true);
            return;
          }

          if (!apiKey.startsWith("e2a_")) {
            res.statusCode = 400;
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            res.end(renderCallbackPage("e2a login failed", "The browser did not return a valid API key."));
            await finish(new Error("browser login did not return a valid api key"), true);
            return;
          }

          res.statusCode = 200;
          res.setHeader("Content-Type", "text/html; charset=utf-8");
          res.end(renderCallbackPage("e2a CLI connected", "You can return to the terminal. Your config has been updated automatically."));

          await finish({ apiKey, agentEmail }, false);
        } catch (error) {
          res.statusCode = 500;
          res.setHeader("Content-Type", "text/html; charset=utf-8");
          res.end(renderCallbackPage("e2a login failed", "The CLI could not process the browser callback."));
          await finish(error instanceof Error ? error : new Error("failed to process browser callback"), true);
        }
      });

      server.on("error", async (error) => {
        await finish(error instanceof Error ? error : new Error("browser login server failed"), true);
      });

      server.listen(0, "127.0.0.1", () => {
        const address = server?.address();
        if (!address || typeof address === "string") {
          void finish(new Error("failed to start local login callback"), true);
          return;
        }

        const callbackUrl = `http://127.0.0.1:${address.port}/callback`;
        const loginUrl = buildBrowserLoginURL(apiUrl, callbackUrl, cliState);

        // Always print the URL — `open`/`xdg-open` can return success even
        // when no browser actually launches (headless box, SSH session,
        // container without a desktop), and without the URL printed the
        // user has no way to recover. Frame it as a fallback so the happy
        // path still reads cleanly.
        process.stderr.write("\n");
        process.stderr.write(`Opening ${hostLabel(apiUrl)} in your browser to finish login...\n`);
        process.stderr.write(`If it doesn't open, visit:\n  ${loginUrl}\n`);
        process.stderr.write("\n");

        openBrowser(loginUrl);
      });
    });
  } finally {
    // Defensive: finish() should already clear these on every code path,
    // but if somebody throws before we register them or after settled is
    // set, the finally block guarantees no orphan timer / open socket
    // keeps the process alive.
    if (timeoutHandle) clearTimeout(timeoutHandle);
    if (server) {
      await closeServer(server);
    }
  }
}

export async function login(): Promise<void> {
  const config = loadConfig();

  // Preflight: hit the deployment's public info endpoint before opening
  // the browser. Two wins —
  //   1. Fast-fail when the server is unreachable instead of opening a
  //      browser tab that loads nothing and timing out 2 minutes later.
  //   2. Captures shared_domain in the same request so we don't need a
  //      separate fetch after login completes.
  // `/v1/info` is unauthenticated and login runs before we have a key, so we
  // probe it with a raw fetch rather than the SDK client (which requires an
  // apiKey to construct). Distinguish a connection failure (refused, DNS,
  // timeout) — which should abort — from an HTTP-level error (server up, /info
  // absent on an older deployment), which should let login continue.
  let discoveredSharedDomain: string | undefined;
  try {
    const resp = await fetch(`${config.api_url.replace(/\/+$/, "")}/v1/info`);
    if (resp.ok) {
      const info = (await resp.json()) as { shared_domain?: string };
      if (info.shared_domain) {
        discoveredSharedDomain = info.shared_domain;
      }
    }
    // A non-ok response means the server is up but /info is unavailable
    // (older deployment) — continue with login without shared_domain.
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    throw new Error(
      `could not reach ${config.api_url}: ${detail}. ` +
        `Check the server is running and E2A_URL is correct.`,
    );
  }

  const { apiKey, agentEmail } = await waitForBrowserLogin(config.api_url);

  // agent_email is deliberately NOT written here. The key is account-scoped —
  // it spans every inbox on the account — so there is no such thing as "the"
  // sending identity to infer. The server's handoff offers one (its first
  // agent, arbitrarily ordered), and persisting that silently picked the
  // From: address for every later `e2a send`. Commands that need an inbox now
  // resolve it explicitly (--agent / E2A_AGENT_EMAIL / a deliberately-set
  // agent_email) or fail with EXIT.USAGE via requireAgentEmail.
  //
  // Omitting the key rather than clearing it matters: saveConfig preserves
  // config keys absent from the update, so an agent_email the user set on
  // purpose survives re-login. Only the guess is gone.
  saveConfig({
    api_key: apiKey,
    key_scope: "account",
    ...(discoveredSharedDomain ? { shared_domain: discoveredSharedDomain } : {}),
  });

  process.stdout.write(`Logged in to ${hostLabel(config.api_url)}.\n`);
  if (discoveredSharedDomain) {
    process.stdout.write(`  Shared domain: ${discoveredSharedDomain}\n`);
  }
  process.stdout.write("Config saved to ~/.e2a/config.json\n");

  if (config.agent_email) {
    process.stdout.write(`Default inbox: ${config.agent_email}\n`);
  } else if (agentEmail) {
    process.stdout.write(
      "No default inbox set. Pass --agent <email> per command, or set one:\n" +
        "  e2a agents list\n" +
        "  e2a config set agent_email <email>\n",
    );
  } else {
    process.stderr.write(
      `No agents found yet. Run: e2a agents create <name>@<shared-domain> — or visit ${config.api_url.replace(/\/+$/, "")}/get-started\n`,
    );
  }
}
