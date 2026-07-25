"use client";

import { Suspense, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "next/navigation";
import { useAuth } from "../../components/AuthProvider";
import { SignInLink } from "../../components/SignInLink";
import { LEGACY_SIGN_IN_URL, SIGN_IN_LABEL } from "../../../lib/site";
import type { DashboardAgent } from "../../components/types";

// Required OAuth params the consent screen needs. If any is missing
// we refuse to render the form — the request didn't come from a real
// /oauth2/authorize and pushing forward would either tamper-fail
// at the backend or, worse, submit half-baked params that fosite
// silently defaults.
const REQUIRED_PARAMS = [
  "response_type",
  "client_id",
  "redirect_uri",
  "code_challenge",
  "code_challenge_method",
] as const;

type ClientMeta = {
  client_id: string;
  client_name: string;
  redirect_uris: string[];
  scopes: string[];
  client_id_issued_at: number;
  // True iff every registered redirect_uri is loopback — i.e. the consent
  // screen may offer account (workspace-admin) scope. The backend re-checks
  // at grant time, so this only drives whether the Account option is
  // selectable in the UI.
  account_eligible: boolean;
};

// Slug rules mirror validateSlug in internal/agent/api.go (length
// 2–40, alphanumeric or hyphen, must start AND end with an
// alphanumeric). Keep the regex in sync — the backend re-validates
// so a drift here only affects UX, not security. Earlier versions
// of this regex made the second char class optional, which allowed
// 1-char slugs that the backend rejected with "slug must be 2–40
// characters" and no inline UI feedback. The tail is now mandatory.
const SLUG_PATTERN = /^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$/;

export default function ConsentPage() {
  // useSearchParams forces dynamic rendering; the Suspense boundary
  // is required for static export builds to succeed.
  return (
    <Suspense fallback={<ConsentShell><p className="text-muted">Loading…</p></ConsentShell>}>
      <ConsentInner />
    </Suspense>
  );
}

function ConsentInner() {
  const { user, loading: authLoading } = useAuth();
  const search = useSearchParams();

  // Snapshot the params synchronously — useSearchParams returns a
  // ReadonlyURLSearchParams that we want to pass through to the form
  // verbatim, plus pluck the few fields we render.
  const params = useMemo(() => {
    const out: Record<string, string> = {};
    if (search) {
      search.forEach((v, k) => {
        out[k] = v;
      });
    }
    return out;
  }, [search]);

  const missing = REQUIRED_PARAMS.filter((k) => !params[k]);
  const clientID = params["client_id"] ?? "";

  const [client, setClient] = useState<ClientMeta | null>(null);
  const [clientError, setClientError] = useState<string | null>(null);
  const [agents, setAgents] = useState<DashboardAgent[] | null>(null);
  const [agentsError, setAgentsError] = useState<string | null>(null);

  // Pull client metadata so we can render the friendly name in the
  // prompt. Skipped when there's no client_id (malformed request) or
  // the user isn't logged in yet — both branches render their own
  // error/loading UI before we get here.
  useEffect(() => {
    if (!clientID || missing.length > 0) return;
    let cancelled = false;
    // Stale-state note: a previous error/success may already be in
    // state when this effect re-fires (e.g. clientID changed). We do
    // NOT reset clientError/client here because the eslint rule
    // `react-hooks/set-state-in-effect` flags synchronous setState in
    // an effect body; the .then/.catch below set the final value, and
    // the cancelled flag prevents a stale resolution from clobbering
    // a fresh one. Worst case (rare): user sees the previous error
    // for the brief window before the new fetch settles — acceptable.
    fetch(`/oauth2/clients/${encodeURIComponent(clientID)}`, {
      credentials: "include",
    })
      .then(async (r) => {
        if (cancelled) return;
        if (r.status === 404) {
          setClientError("This client is not registered with e2a.");
          return;
        }
        if (!r.ok) {
          setClientError(`Could not look up client (HTTP ${r.status}).`);
          return;
        }
        const data: ClientMeta = await r.json();
        if (!cancelled) {
          setClientError(null);
          setClient(data);
        }
      })
      .catch((e) => {
        if (!cancelled) setClientError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [clientID, missing.length]);

  useEffect(() => {
    if (!user || missing.length > 0) return;
    let cancelled = false;
    fetch("/v1/agents", { credentials: "include" })
      .then(async (r) => {
        if (cancelled) return;
        if (!r.ok) {
          setAgentsError(`Could not list your inboxes (HTTP ${r.status}).`);
          return;
        }
        const data: { items?: DashboardAgent[] | null } = await r.json();
        if (!cancelled) setAgents(data.items ?? []);
      })
      .catch((e) => {
        if (!cancelled) setAgentsError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [user, missing.length]);

  // Malformed request → friendly explanation. Don't try to recover.
  if (missing.length > 0) {
    return (
      <ConsentShell>
        <h1 className="text-xl font-semibold mb-3">Invalid authorization request</h1>
        <p className="text-muted text-sm mb-2">
          The request is missing required parameters:
        </p>
        <ul className="list-disc list-inside text-sm mb-4 text-muted">
          {missing.map((p) => (
            <li key={p}><code>{p}</code></li>
          ))}
        </ul>
        <p className="text-muted text-sm">
          This screen is reached from{" "}
          <code>/oauth2/authorize</code>. If you arrived here directly,
          start the flow from your MCP client.
        </p>
      </ConsentShell>
    );
  }

  if (authLoading) {
    return <ConsentShell><p className="text-muted">Loading…</p></ConsentShell>;
  }

  // Not signed in → bounce through login carrying return_to so
  // the user lands back on /oauth2/authorize (which then re-renders
  // this page with a session). We construct return_to from the same
  // params so the round-trip preserves everything.
  if (!user) {
    const qs = new URLSearchParams(params).toString();
    const returnTo = `/oauth2/authorize?${qs}`;
    // return_to is honored by the legacy Google door only — the OIDC door
    // (/api/auth/oidc/login) ignores it and always lands on /dashboard, so
    // this link stays on the legacy door until the OIDC flow learns
    // return_to. The legacy door remains registered on every deployment.
    const loginURL = `${LEGACY_SIGN_IN_URL}?return_to=${encodeURIComponent(returnTo)}`;
    return (
      <ConsentShell>
        <h1 className="text-xl font-semibold mb-3">Sign in to continue</h1>
        <p className="text-muted text-sm mb-6">
          Sign in to authorize this application.
        </p>
        <SignInLink className="inline-block px-4 py-2 bg-accent text-white rounded-md text-sm font-medium hover:bg-accent-light transition">
          {SIGN_IN_LABEL}
        </SignInLink>
        {/* Fallback: SignInLink hits the configured sign-in door with no
            return_to. Provide an alternate link that carries return_to for
            the common case. */}
        <p className="mt-4">
          <a className="text-sm text-accent underline" href={loginURL}>
            Sign in and return to this authorization
          </a>
        </p>
      </ConsentShell>
    );
  }

  if (clientError) {
    return (
      <ConsentShell>
        <h1 className="text-xl font-semibold mb-3">Unknown client</h1>
        <p className="text-muted text-sm mb-4">{clientError}</p>
        <p className="text-muted text-sm">
          The client must register via{" "}
          <code>/oauth2/register</code> before requesting authorization.
        </p>
      </ConsentShell>
    );
  }

  // Loading client OR agents — both are required for the form.
  if (!client || agents === null) {
    return <ConsentShell><p className="text-muted">Loading…</p></ConsentShell>;
  }

  if (agentsError) {
    return (
      <ConsentShell>
        <h1 className="text-xl font-semibold mb-3">Could not load your inboxes</h1>
        <p className="text-muted text-sm">{agentsError}</p>
      </ConsentShell>
    );
  }

  return (
    <ConsentForm
      params={params}
      client={client}
      agents={agents}
      userEmail={user.email}
    />
  );
}

function ConsentForm({
  params,
  client,
  agents,
  userEmail,
}: {
  params: Record<string, string>;
  client: ClientMeta;
  agents: DashboardAgent[];
  userEmail: string;
}) {
  // Default the choice: pick the first existing agent if any, else
  // fall back to creating a fresh one. The radio that's checked here
  // is the value submitted unless the user changes it.
  const hasAgents = agents.length > 0;
  const [agentChoice, setAgentChoice] = useState<string>(
    hasAgents ? `existing:${agents[0].email}` : "create_new",
  );

  // Default slug derived from the client name. Matches
  // defaultAgentSlug in the backend; backend will replace with its
  // own randomized suffix if we leave this blank.
  const defaultSlug = useMemo(() => deriveDefaultSlug(client.client_name), [client.client_name]);
  const [slug, setSlug] = useState<string>(defaultSlug);

  // Scope the user is granting. Default to "account" (full workspace admin)
  // whenever this client is account-eligible, else the least-privilege
  // "agent". The backend re-validates account eligibility server-side, so
  // this is UX only.
  const accountEligible = client.account_eligible;
  const [scopeChoice, setScopeChoice] = useState<"agent" | "account">(
    accountEligible ? "account" : "agent",
  );
  const pickingAccount = scopeChoice === "account";

  const creating = agentChoice === "create_new";
  const slugValid = !creating || SLUG_PATTERN.test(slug);
  // Verify the inbound redirect_uri matches one of the values the
  // client registered. fosite re-validates this server-side so a
  // mismatch isn't an exploit, but the UI was happy to render
  // "Redirect: https://attacker.example" with no warning and let
  // the user click Authorize. The backend would then 400 with a
  // generic OAuth error, leaving the user confused. Bail loudly
  // upfront instead.
  const inboundRedirect = params["redirect_uri"] ?? "";
  const redirectMismatch =
    inboundRedirect !== "" &&
    client.redirect_uris.length > 0 &&
    !client.redirect_uris.includes(inboundRedirect);
  // Account scope binds to no inbox, so the slug is irrelevant then.
  const submitDisabled = redirectMismatch || (!pickingAccount && !slugValid);

  // The hidden inputs re-emit every OAuth param we received. We use
  // params (the readback of useSearchParams) rather than re-deriving
  // the names so forward-compat fields (e.g. resource per RFC 8707)
  // pass through cleanly.
  return (
    <ConsentShell>
      <h1 className="text-xl font-semibold mb-1">Authorize {client.client_name}</h1>
      <p className="text-muted text-sm mb-6">
        Signed in as <span className="font-mono">{userEmail}</span>.
      </p>

      <p className="mb-4 text-sm">
        <strong>{client.client_name}</strong> is asking to send and receive
        email on your behalf via e2a.
      </p>

      {redirectMismatch && (
        <div
          role="alert"
          className="mb-4 p-3 text-sm border rounded"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            borderColor: "var(--danger-bg)",
          }}
        >
          <strong>Redirect URI mismatch.</strong> The redirect URL this
          request is using (<span className="font-mono">{inboundRedirect}</span>)
          is not on the client&apos;s registered list. The server would
          reject the authorization anyway — refusing in the UI to avoid
          phishing-by-confused-redirect.
        </div>
      )}

      <form method="POST" action="/oauth2/consent" className="space-y-4">
        {Object.entries(params).map(([k, v]) => (
          <input key={k} type="hidden" name={k} value={v} />
        ))}

        <fieldset className="border border-border rounded-md p-4">
          <legend className="text-sm font-medium px-2">Access level</legend>

          <label className="flex items-start gap-2 py-1 cursor-pointer">
            <input
              type="radio"
              name="scope_choice"
              value="agent"
              checked={!pickingAccount}
              onChange={() => setScopeChoice("agent")}
              className="mt-1"
            />
            <span className="text-sm">
              <span className="font-medium">Agent — act as one inbox</span>
              <span className="block text-xs text-muted">
                Send &amp; receive email as the inbox you pick below. Cannot
                manage other inboxes, domains, webhooks, or API keys.
              </span>
            </span>
          </label>

          <label
            className={`flex items-start gap-2 py-1 ${
              accountEligible ? "cursor-pointer" : "opacity-60 cursor-not-allowed"
            }`}
          >
            <input
              type="radio"
              name="scope_choice"
              value="account"
              checked={pickingAccount}
              disabled={!accountEligible}
              onChange={() => setScopeChoice("account")}
              className="mt-1"
            />
            <span className="text-sm">
              <span
                className="font-medium"
                style={pickingAccount ? { color: "var(--danger-strong)" } : undefined}
              >
                Account — full workspace admin ⚠
              </span>
              <span className="block text-xs text-muted">
                Everything Agent can do, plus: create/delete inboxes, manage
                domains &amp; webhooks, mint API keys, resolve reviews, and
                account settings. Grant only to clients you fully trust.
              </span>
              {!accountEligible && (
                <span className="block text-xs mt-0.5" style={{ color: "var(--danger-strong)" }}>
                  Unavailable: this client isn&apos;t registered for account
                  scope.
                </span>
              )}
            </span>
          </label>
        </fieldset>

        {pickingAccount && (
          <div
            role="alert"
            className="p-3 text-xs border rounded"
            style={{
              background: "var(--danger-bg)",
              color: "var(--danger-strong)",
              borderColor: "var(--danger-bg)",
            }}
          >
            <strong>{client.client_name}</strong> will be able to administer your
            entire e2a workspace. Only continue if you started this from a tool
            you run and trust.
          </div>
        )}

        {!pickingAccount && (
        <fieldset className="border border-border rounded-md p-4">
          <legend className="text-sm font-medium px-2">Choose an inbox</legend>

          {agents.map((a) => (
            <label key={a.email} className="flex items-center gap-2 py-1 cursor-pointer">
              <input
                type="radio"
                name="agent_choice"
                value={`existing:${a.email}`}
                checked={agentChoice === `existing:${a.email}`}
                onChange={() => setAgentChoice(`existing:${a.email}`)}
              />
              <span className="font-mono text-sm">{a.email}</span>
            </label>
          ))}

          <label className="flex items-center gap-2 py-1 cursor-pointer">
            <input
              type="radio"
              name="agent_choice"
              value="create_new"
              checked={creating}
              onChange={() => setAgentChoice("create_new")}
            />
            <span className="text-sm">Create a new inbox</span>
          </label>

          {creating && (
            <div className="ml-6 mt-2">
              <input
                type="text"
                name="new_agent_slug"
                value={slug}
                onChange={(e) => setSlug(e.target.value.toLowerCase())}
                aria-label="New inbox slug"
                aria-invalid={!slugValid}
                className={`w-full text-sm font-mono px-2 py-1 border rounded ${
                  slugValid ? "border-border" : "border-red-500"
                }`}
                spellCheck={false}
                autoComplete="off"
              />
              {!slugValid && (
                <p className="text-xs text-red-500 mt-1">
                  Slug must be 2–40 lowercase letters, digits, or hyphens, and start and end with a letter or digit.
                </p>
              )}
            </div>
          )}
        </fieldset>
        )}

        <div className="text-xs text-muted">
          <strong>Scope:</strong>{" "}
          <span className="font-mono">{pickingAccount ? "account" : "agent"}</span>
          <br />
          <strong>Redirect:</strong>{" "}
          <span className="font-mono break-all">{params["redirect_uri"]}</span>
        </div>

        <div className="flex gap-3 justify-end pt-2">
          <button
            type="submit"
            name="action"
            value="deny"
            className="px-4 py-2 rounded-md text-sm border border-border bg-transparent hover:bg-muted/10 transition"
          >
            Deny
          </button>
          <button
            type="submit"
            name="action"
            value="allow"
            disabled={submitDisabled}
            className="px-4 py-2 rounded-md text-sm font-medium bg-accent text-white hover:bg-accent-light transition disabled:opacity-50 disabled:cursor-not-allowed"
          >
            Allow
          </button>
        </div>
      </form>
    </ConsentShell>
  );
}

function ConsentShell({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex-1 flex items-center justify-center px-4 py-12">
      <div className="w-full max-w-md p-8 border border-border rounded-lg bg-card">
        {children}
      </div>
    </main>
  );
}

// deriveDefaultSlug mirrors the backend's slugifyClientID +
// random-suffix approach, but uses the client_name (more human-
// readable) and a stable suffix derived from client_id so two
// reloads of the same /authorize request render the same default.
// Backend will randomize when we submit a blank slug, so this is
// purely for the placeholder shown to the user.
function deriveDefaultSlug(clientName: string): string {
  const base = clientName
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 30);
  const cleanBase = base || "agent";
  // Short suffix from Math.random so the default isn't the same every
  // time the user reloads (avoiding "I picked 'foo' once and now my
  // form keeps offering 'foo' which is taken").
  const suffix = Math.floor(Math.random() * 0xffffff)
    .toString(16)
    .padStart(6, "0");
  return `${cleanBase}-${suffix}`.slice(0, 40);
}
