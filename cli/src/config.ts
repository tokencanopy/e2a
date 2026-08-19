import { readFileSync, writeFileSync, mkdirSync, chmodSync } from "node:fs";
import { join } from "node:path";
import { homedir } from "node:os";
import { EXIT } from "./exit.js";

export interface Config {
  api_key: string;
  api_url: string;
  agent_email: string;
  /**
   * Shared mail domain the deployment uses for slug-based agent addresses
   * (e.g. "agents.example.com"), auto-discovered from `GET /v1/info` on
   * `e2a login` and cached here. Self-hosters override it with
   * E2A_SHARED_DOMAIN. Empty ("") when unknown — deliberately NOT defaulted
   * to the hosted product's "agents.e2a.dev": a caller authenticating by env
   * only (E2A_API_KEY + E2A_URL, never `e2a login` — CI, containers, the
   * tether harness) would otherwise silently expand bare inbox names onto
   * the operator's domain instead of a self-hosted one. Use
   * {@link resolveSharedDomain} to get a value, which discovers it live
   * from `/v1/info` when this is empty.
   */
  shared_domain: string;
  /**
   * Scope of the stored api_key ("account" or "agent"), recorded by `e2a
   * login` after the browser handoff. Lets commands that need workspace-admin
   * scope fail with a precise message instead of a server 403. Absent for keys
   * saved by older CLIs or set out-of-band.
   */
  key_scope?: string;
}

const CONFIG_DIR = join(homedir(), ".e2a");
const CONFIG_PATH = join(CONFIG_DIR, "config.json");
const DEFAULT_URL = "https://e2a.dev";
const DEFAULT_SHARED_DOMAIN = "agents.e2a.dev";

export function loadConfig(): Config {
  const config: Config = {
    api_key: "",
    api_url: DEFAULT_URL,
    agent_email: "",
    // No default: see the field doc on Config.shared_domain. Resolved live
    // via resolveSharedDomain() where it's actually needed.
    shared_domain: "",
  };

  // Read from file
  try {
    const raw = readFileSync(CONFIG_PATH, "utf-8");
    const file = JSON.parse(raw);
    if (file.api_key) config.api_key = file.api_key;
    if (file.api_url) config.api_url = file.api_url;
    if (file.agent_email) config.agent_email = file.agent_email;
    if (file.shared_domain) config.shared_domain = file.shared_domain;
    if (file.key_scope) config.key_scope = file.key_scope;
  } catch {
    // No config file yet
  }

  // Env vars override file. E2A_AGENT_EMAIL selects the inbox — the exact name
  // the tether harness trains users to export, so ignoring it is a silent trap
  // for the CLI's primary scripting consumer.
  //
  // E2A_BASE_URL is deliberately NOT read here. api_url is the deployment root
  // (login opens a browser against it and points at /get-started, both served
  // by the web front, which proxies /v1 through), whereas E2A_BASE_URL/
  // E2A_API_URL name the API host alone. Accepting it as an alias meant that
  // exporting E2A_BASE_URL=https://api.e2a.dev to configure an SDK silently
  // repointed the CLI at the API host and broke `e2a login`.
  if (process.env.E2A_API_KEY) config.api_key = process.env.E2A_API_KEY;
  if (process.env.E2A_URL) config.api_url = process.env.E2A_URL;
  if (process.env.E2A_AGENT_EMAIL) config.agent_email = process.env.E2A_AGENT_EMAIL;
  if (process.env.E2A_SHARED_DOMAIN) config.shared_domain = process.env.E2A_SHARED_DOMAIN;

  // Someone who set E2A_BASE_URL expecting it to steer the CLI (it used to)
  // would otherwise be ignored silently. Warn whenever it's set without the
  // canonical E2A_URL — the CLI then resolves to either its default OR a host
  // stored by `e2a login`, and neither is what E2A_BASE_URL asked for. It used
  // to override the stored config, so a user with stored host A and legacy
  // env host B now silently gets A. Show the host actually in use so that
  // mismatch is visible rather than hidden.
  if (!process.env.E2A_URL && process.env.E2A_BASE_URL) {
    process.stderr.write(
      `e2a: E2A_BASE_URL is set but the CLI does not read it — that name configures the SDKs.\n` +
        `     The CLI uses E2A_URL for the deployment root and is talking to ${config.api_url}.\n` +
        `     Set E2A_URL to point it at a self-hosted deployment.\n`,
    );
  }

  return config;
}

export function saveConfig(updates: Partial<Config>): void {
  const current = loadConfig();
  const merged = { ...current, ...updates };

  // Read existing file to preserve fields we don't manage
  let existing: Record<string, string> = {};
  try {
    existing = JSON.parse(readFileSync(CONFIG_PATH, "utf-8"));
  } catch {
    // No existing file
  }

  // Don't write env-only values back to file, but preserve unrelated fields.
  const fileConfig: Record<string, string> = { ...existing };

  if ("api_key" in updates) {
    if (updates.api_key) fileConfig.api_key = updates.api_key;
    else delete fileConfig.api_key;
  } else if (!process.env.E2A_API_KEY && merged.api_key) {
    fileConfig.api_key = merged.api_key;
  }

  if ("api_url" in updates) {
    if (updates.api_url && updates.api_url !== DEFAULT_URL) {
      fileConfig.api_url = updates.api_url;
    } else {
      delete fileConfig.api_url;
    }
  } else if (!process.env.E2A_URL && merged.api_url !== DEFAULT_URL) {
    fileConfig.api_url = merged.api_url;
  } else if (!process.env.E2A_URL) {
    delete fileConfig.api_url;
  }

  if ("agent_email" in updates) {
    if (updates.agent_email) fileConfig.agent_email = updates.agent_email;
    else delete fileConfig.agent_email;
  } else if (!process.env.E2A_AGENT_EMAIL && merged.agent_email) {
    fileConfig.agent_email = merged.agent_email;
  } else if (!process.env.E2A_AGENT_EMAIL) {
    delete fileConfig.agent_email;
  }

  // key_scope has no env override and no default: persist when set, drop when
  // cleared. Login rewrites it whenever api_key changes, so it can't go stale
  // through the login path (a hand-edited api_key is on the user).
  if ("key_scope" in updates) {
    if (updates.key_scope) fileConfig.key_scope = updates.key_scope;
    else delete fileConfig.key_scope;
  }

  // Mirror the api_url policy: persist non-default values even while an env
  // override shadows them, so removing the override reveals the requested
  // configuration instead of an older value (or the hosted default).
  if ("shared_domain" in updates) {
    if (
      updates.shared_domain &&
      updates.shared_domain !== DEFAULT_SHARED_DOMAIN
    ) {
      fileConfig.shared_domain = updates.shared_domain;
    } else {
      delete fileConfig.shared_domain;
    }
  } else if (!process.env.E2A_SHARED_DOMAIN && merged.shared_domain !== DEFAULT_SHARED_DOMAIN) {
    fileConfig.shared_domain = merged.shared_domain;
  } else if (!process.env.E2A_SHARED_DOMAIN) {
    delete fileConfig.shared_domain;
  }

  mkdirSync(CONFIG_DIR, { recursive: true });
  writeFileSync(CONFIG_PATH, JSON.stringify(fileConfig, null, 2) + "\n", {
    mode: 0o600,
  });
  // `mode` only applies when writeFileSync creates the file. Tighten an
  // existing config too: it contains plaintext API credentials and may have
  // been copied or manually created with a permissive umask.
  chmodSync(CONFIG_PATH, 0o600);
}

export function requireApiKey(config: Config): string {
  if (!config.api_key) {
    // Missing credentials are an auth failure per the documented exit-code
    // contract (4) — never 1, which scripts treat as retryable-transient.
    // Name BOTH acquisition paths: `login` needs a browser on this machine;
    // headless boxes use the environment variable.
    process.stderr.write(
      "Not authenticated. Run: e2a login (browser), or set E2A_API_KEY\n",
    );
    process.exit(EXIT.AUTH);
  }
  return config.api_key;
}

/**
 * Resolves the deployment's shared mail domain for bare-name → full-address
 * expansion (`e2a agents create mybot` → `mybot@<shared_domain>`).
 *
 * config.shared_domain is populated by `e2a login`'s discovery request or
 * E2A_SHARED_DOMAIN, but a caller authenticating purely by env
 * (E2A_API_KEY + E2A_URL: CI, containers, the tether harness) never runs
 * `e2a login`, so it's often still empty at the point of use. `GET
 * /v1/info` is unauthenticated and reachable on every deployment
 * (hosted or self-hosted) at that same point, so we discover it live
 * instead of guessing the hosted product's "agents.e2a.dev" — a
 * self-hosted deployment's real shared domain is almost certainly
 * something else, and guessing wrong would silently create/reference an
 * address on the wrong domain.
 *
 * Returns "" — never a guess — when nothing resolves a value, so callers
 * can fail closed with a clear message instead of expanding onto the
 * wrong domain.
 */
export async function resolveSharedDomain(config: Config): Promise<string> {
  if (config.shared_domain) return config.shared_domain;
  try {
    const resp = await fetch(`${config.api_url.replace(/\/+$/, "")}/v1/info`);
    if (resp.ok) {
      const info = (await resp.json()) as { shared_domain?: string };
      if (info.shared_domain) return info.shared_domain;
    }
    // Non-ok response: server up but /v1/info unavailable (older
    // deployment) or genuinely has no shared domain configured. Either
    // way, fall through to "" rather than guessing.
  } catch {
    // Unreachable deployment: the caller's next real API call (agents
    // create / keys create) will surface this same failure with a clearer,
    // non-discovery-specific error. No need to duplicate it here.
  }
  return "";
}

/**
 * Expands a bare inbox slug (no "@") to a full address on the deployment's
 * shared domain, discovering that domain via {@link resolveSharedDomain}
 * when it isn't already known. A full address (contains "@") passes
 * through unchanged with no network call.
 *
 * Exits USAGE (2) — the same fail-closed shape as {@link requireApiKey} —
 * when the name is bare and no shared domain can be found, rather than
 * silently building an address on the operator's domain.
 */
export async function expandBareAddress(nameOrEmail: string, config: Config): Promise<string> {
  if (nameOrEmail.includes("@")) return nameOrEmail;
  const domain = await resolveSharedDomain(config);
  if (!domain) {
    process.stderr.write(
      `e2a: "${nameOrEmail}" has no @ and this deployment has no shared domain — pass a full address (name@yourdomain.com).\n`,
    );
    process.exit(EXIT.USAGE);
  }
  return `${nameOrEmail}@${domain}`;
}
