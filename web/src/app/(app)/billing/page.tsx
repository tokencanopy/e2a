"use client";

import { useEffect, useRef, useState } from "react";
import useSWR from "swr";
import { billingPolling } from "../../../lib/livePolling";
import { limitsKey } from "../../../lib/swrKeys";
import { PageShell } from "../../components/loft/PageShell";

// LimitsInfo matches the LimitsView shape returned by GET /v1/account.
// Kept inline rather than imported from a generated client because the
// OSS SDK doesn't expose this endpoint yet (it's a dashboard-only
// surface — SDK consumers would call /agents and /messages directly).
type LimitsInfo = {
  plan_code: string;
  limits: {
    max_agents: number;
    max_domains: number;
    max_messages_month: number;
    max_storage_bytes: number;
  };
  usage: {
    agents: number;
    domains: number;
    messages_month: number;
    storage_bytes: number;
  };
  upgrade_url: string;
};

// PlanInfo mirrors the response of GET /api/billing/plan on the hosted
// billing sidecar. The sidecar's plans package is the single source of
// truth for what each tier includes and what it costs; we render the
// comparison straight from it so the dashboard can never drift from the
// caps the webhook actually provisions. Stripe price IDs stay
// server-side — the client only ever sends a plan `code`.
type PlanEntry = {
  code: string;
  display_name: string;
  monthly_price_cents: number;
  max_agents: number;
  max_domains: number;
  max_messages_month: number;
  // int64 on the Go side; safe as a JS number — the largest tier cap
  // (~100 GiB ≈ 1.07e11) is far below Number.MAX_SAFE_INTEGER (2^53).
  max_storage_bytes: number;
};

type CurrentState = {
  code: string;
  status: string;
  current_period_end?: string;
  has_stripe_customer: boolean;
};

type PlanInfo = {
  catalog: PlanEntry[];
  current: CurrentState;
};

// BILLING_API is the base URL of the external limits provisioner (the
// hosted billing sidecar). When empty the dashboard renders the usage
// surface without the plan comparison or Upgrade / Manage Billing
// affordances — appropriate for self-host deployments that don't run a
// paid tier. Set at build time via NEXT_PUBLIC_BILLING_API; populated in
// the prod web image.
const BILLING_API = (process.env.NEXT_PUBLIC_BILLING_API ?? "").replace(/\/$/, "");

async function fetchLimits(): Promise<LimitsInfo> {
  const res = await fetch("/v1/account", { credentials: "include" });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`limits: HTTP ${res.status}${body ? ` — ${body}` : ""}`);
  }
  return res.json();
}

async function fetchPlan(url: string): Promise<PlanInfo> {
  const res = await fetch(url, { credentials: "include" });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`plan: HTTP ${res.status}${body ? ` — ${body}` : ""}`);
  }
  return res.json();
}

// formatBytes renders storage in the unit that makes the number
// human-legible — KB / MB / GB. Matches the formatting Resend and
// AgentMail use in their dashboards so users carrying mental models
// from those tools aren't surprised.
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function formatNumber(n: number): string {
  return n.toLocaleString();
}

// formatPrice turns the catalog's integer cents into the dashboard's
// price label. $0 reads "Free"; whole-dollar amounts drop the trailing
// ".00" so "$20/mo" not "$20.00/mo".
function formatPrice(cents: number): string {
  if (cents <= 0) return "Free";
  const dollars = cents / 100;
  const amount = Number.isInteger(dollars) ? `$${dollars}` : `$${dollars.toFixed(2)}`;
  return `${amount}/mo`;
}

// QUOTA_DIMS is the data-driven list of caps we show per tier. Each
// dimension formats its own cell from a PlanEntry, so adding a future
// cap (webhooks, seats, …) is one entry here plus the matching field on
// PlanEntry — no per-tier markup to touch.
const QUOTA_DIMS: { label: string; format: (p: PlanEntry) => string }[] = [
  { label: "Inboxes", format: (p) => formatNumber(p.max_agents) },
  { label: "Domains", format: (p) => formatNumber(p.max_domains) },
  { label: "Messages / mo", format: (p) => formatNumber(p.max_messages_month) },
  { label: "Storage", format: (p) => formatBytes(p.max_storage_bytes) },
];

// Stripe Checkout returns the user to CHECKOUT_SUCCESS_URL
// (/billing?status=success) as a full page load. That load races the
// asynchronous checkout.session.completed webhook that provisions the new
// limits, and the redirect normally wins — so the first read still reports
// the old plan. Re-read on a short interval until the sidecar says the
// subscription is active, then stop.
//
// "active" is a sound completion signal here because Checkout is only
// reachable from a non-subscribed state; existing subscribers change plans
// through the Stripe portal, which returns to /billing with no marker.
const RECONCILE_INTERVAL_MS = 2000;
const RECONCILE_TIMEOUT_MS = 20000;

// idle    — ordinary visit, background polling only.
// pending — just came back from Checkout, waiting on the webhook.
// timeout — waited RECONCILE_TIMEOUT_MS and it still hasn't landed.
type ReconcileState = "idle" | "pending" | "timeout";

// pctTone picks the bar color based on how close to the cap the user
// is. <70% neutral, 70-90% warning, >=90% danger. Tones reuse existing
// CSS vars so the dashboard's theme tokens own the actual colors.
function pctTone(pct: number): "neutral" | "warn" | "danger" {
  if (pct >= 90) return "danger";
  if (pct >= 70) return "warn";
  return "neutral";
}

type UsageRowProps = {
  label: string;
  current: string;
  limit: string;
  pct: number;
};

function UsageRow({ label, current, limit, pct }: UsageRowProps) {
  const tone = pctTone(pct);
  const barColor =
    tone === "danger"
      ? "var(--accent-danger, #ef4444)"
      : tone === "warn"
      ? "var(--accent-warn, #f59e0b)"
      : "var(--accent)";
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between text-sm">
        <span className="text-foreground font-medium">{label}</span>
        <span className="text-muted">
          {current} <span className="text-muted/70">/ {limit}</span>
        </span>
      </div>
      <div className="h-1.5 rounded-full bg-background overflow-hidden">
        <div
          className="h-full rounded-full transition-[width] duration-300"
          style={{
            width: `${Math.min(100, Math.max(0, pct))}%`,
            background: barColor,
          }}
          aria-hidden
        />
      </div>
    </div>
  );
}

// PlanCTA describes the action button on one tier card. `null` means the
// tier shows no button (e.g. the Free tier for a brand-new user — it's
// already their plan, nothing to buy).
type PlanCTA = {
  label: string;
  onClick: () => void;
  disabled: boolean;
};

function PlanCard({
  tier,
  isCurrent,
  cta,
}: {
  tier: PlanEntry;
  isCurrent: boolean;
  cta: PlanCTA | null;
}) {
  return (
    <div
      className="rounded-lg border p-4 flex flex-col gap-3"
      style={{
        // The current tier is marked by the accent border and the "Current"
        // badge. It deliberately shares --bg-elev with the other tiers: the
        // surface scale has no step above --bg-elev that keeps its direction
        // in both themes (--bg-panel is lighter than --bg-elev in light mode
        // but darker in dark mode), so a background distinction here would
        // read as "raised" on one theme and "recessed" on the other.
        //
        // This previously read `isCurrent ? "var(--surface)" : "var(--bg-elev)"`.
        // --surface is defined nowhere, so with no fallback the declaration was
        // invalid at computed-value time and background-color fell back to its
        // initial value, transparent — the current tier was the ONLY card with
        // no surface at all, inverting the hierarchy it was meant to signal.
        borderColor: isCurrent ? "var(--accent)" : "var(--border)",
        background: "var(--bg-elev)",
      }}
      aria-current={isCurrent ? "true" : undefined}
    >
      <div>
        <div className="flex items-center justify-between gap-2">
          <span className="text-sm font-semibold text-foreground">
            {tier.display_name}
          </span>
          {isCurrent && (
            <span
              className="text-[10px] uppercase tracking-wide rounded px-1.5 py-0.5"
              style={{ background: "var(--accent)", color: "white" }}
            >
              Current
            </span>
          )}
        </div>
        <div className="text-xs text-muted mt-0.5">
          {formatPrice(tier.monthly_price_cents)}
        </div>
      </div>

      <dl className="text-xs text-muted space-y-1">
        {QUOTA_DIMS.map((d) => (
          <div key={d.label} className="flex items-baseline justify-between gap-2">
            <dt>{d.label}</dt>
            <dd className="text-foreground font-medium">{d.format(tier)}</dd>
          </div>
        ))}
      </dl>

      {cta && (
        <button
          type="button"
          disabled={cta.disabled}
          onClick={cta.onClick}
          className="mt-auto px-3 py-1.5 rounded-md text-sm font-medium bg-accent text-white hover:bg-accent/90 transition disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {cta.label}
        </button>
      )}
    </div>
  );
}

export default function BillingPage() {
  // Both reads poll: usage counters move as the account sends and
  // receives, and the plan changes out-of-band when Stripe's webhook
  // provisions an upgrade. Focus revalidation alone left the page frozen
  // for anyone who simply kept it open.
  const { data, error, isLoading, mutate } = useSWR<LimitsInfo>(
    limitsKey,
    fetchLimits,
    billingPolling,
  );

  // The plan catalog comes from the sidecar's source of truth. Gated on
  // BILLING_API so self-host builds (no sidecar) skip the fetch entirely
  // — SWR treats a null key as "don't fetch".
  const {
    data: planData,
    error: planError,
    mutate: mutatePlan,
  } = useSWR<PlanInfo>(
    BILLING_API ? `${BILLING_API}/api/billing/plan` : null,
    fetchPlan,
    billingPolling,
  );

  // Track which specific button is in flight so it can show "Opening…"
  // while the others disable. Tier CTAs are keyed `tier-<code>`; the
  // banner's Manage-billing button is "manage".
  const [actionPending, setActionPending] = useState<string | null>(null);

  // Both Upgrade and Manage Billing POST to the sidecar and follow the
  // returned `url`. POST (not GET) because the OSS session cookie is
  // SameSite=Lax — Lax permits top-level GET navigations from third
  // parties, which would make GET endpoints CSRF-able (a malicious
  // page could create real Stripe Checkout sessions for the victim).
  // POSTs from a third-party origin are blocked by Lax, so the dashboard
  // owns the call and the cross-origin attack surface is gone.
  //
  // `body` carries the plan selector for /api/billing/checkout (sidecar
  // defaults to Pro when absent). /api/billing/portal ignores it.
  async function postBilling(endpoint: string, kind: string, body?: unknown) {
    setActionPending(kind);
    try {
      const res = await fetch(endpoint, {
        method: "POST",
        credentials: "include",
        headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
      if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error(`HTTP ${res.status}${text ? `: ${text}` : ""}`);
      }
      const json = (await res.json()) as { url?: string };
      if (!json.url) {
        throw new Error("billing endpoint returned no url");
      }
      window.location.href = json.url;
    } catch (err) {
      // Best-effort recovery: surface the error to the user, clear
      // the pending state, and let them retry. We don't reset SWR
      // because the underlying limits data is unaffected by a
      // failed checkout/portal session.
      setActionPending(null);
      alert(`Could not open billing: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  // When the user navigates away (e.g. to Stripe Checkout) and hits
  // Back, the browser may restore the page from bfcache with any
  // in-flight fetch abandoned. SWR's isLoading state then sticks at
  // true and the user sees a permanent "Loading…". Force a revalidate
  // on every pageshow with persisted=true so the page never gets
  // stuck after a Back navigation.
  useEffect(() => {
    const onShow = (e: PageTransitionEvent) => {
      if (e.persisted) {
        void mutate();
        void mutatePlan();
      }
    };
    window.addEventListener("pageshow", onShow);
    return () => window.removeEventListener("pageshow", onShow);
  }, [mutate, mutatePlan]);

  const [reconcile, setReconcile] = useState<ReconcileState>("idle");

  // Consume the ?status marker Stripe redirected us back with. Read from
  // window.location rather than useSearchParams() because this route is
  // part of a static export — useSearchParams() would force the whole
  // page under a Suspense boundary to prerender. The marker is stripped
  // immediately: it has been used, and leaving it in the URL would
  // restart the reconcile loop on every remount or Back navigation.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const status = params.get("status");
    if (!status) return;
    params.delete("status");
    const qs = params.toString();
    window.history.replaceState(
      {},
      "",
      `${window.location.pathname}${qs ? `?${qs}` : ""}`,
    );
    // `cancel` needs no reconcile — nothing was provisioned. Clearing the
    // marker above is the whole handling.
    //
    // The BILLING_API guard matters because reconciliation resolves on the
    // sidecar's plan read: with no sidecar that read never happens, so a
    // stray marker would park the page on a notice that can never clear.
    if (status === "success" && BILLING_API) setReconcile("pending");
  }, []);

  // Poll both reads until the webhook lands or the window elapses. This
  // is deliberately separate from the background `billingPolling`
  // cadence: 30s is fine for idle drift, far too slow for someone
  // staring at the page they just paid on.
  // The deadline lives in a ref, not a local, so it is set ONCE when the
  // reconcile window opens and survives any re-run of this effect. A local
  // would be recomputed on every re-run, and this effect depends on `mutate`
  // and `mutatePlan` — SWR's bound mutators, which are referentially stable
  // in v2 but are not ours to guarantee. If a future SWR ever returned a
  // fresh identity per render, a recomputed deadline would push the timeout
  // permanently out of reach and this would poll the billing sidecar
  // forever. The ref makes termination depend only on wall-clock time.
  const reconcileDeadlineRef = useRef<number | null>(null);

  useEffect(() => {
    if (reconcile !== "pending") {
      // Leaving the window (resolved, timed out, or unmounted) clears the
      // deadline so a subsequent reconcile starts a fresh one.
      reconcileDeadlineRef.current = null;
      return;
    }
    reconcileDeadlineRef.current ??= Date.now() + RECONCILE_TIMEOUT_MS;
    const id = setInterval(() => {
      if (Date.now() >= (reconcileDeadlineRef.current ?? 0)) {
        setReconcile("timeout");
        return;
      }
      void mutate();
      void mutatePlan();
    }, RECONCILE_INTERVAL_MS);
    return () => clearInterval(id);
  }, [reconcile, mutate, mutatePlan]);

  // Webhook landed — stop polling and let the page render normally.
  useEffect(() => {
    if (reconcile === "pending" && planData?.current.status === "active") {
      setReconcile("idle");
    }
  }, [reconcile, planData]);

  // Compute usage percentages once data is loaded. Guard zero limits
  // (treat as 0% rather than NaN/Infinity) so a misconfigured row with
  // max_*=0 doesn't paint a full red bar.
  const pct = (used: number, cap: number) => (cap > 0 ? (used / cap) * 100 : 0);

  // Current tier: prefer the sidecar's billing truth, fall back to the
  // OSS-enforced plan_code so the banner still labels correctly when the
  // catalog fetch hasn't landed (or isn't present on self-host).
  const currentCode = planData?.current.code ?? data?.plan_code ?? "";

  // hasSub: the user has an active Stripe subscription. upgrade_url is
  // only populated by the OSS server when a subscription exists, and it
  // *is* the portal POST target — so it doubles as the "has subscription"
  // signal and the endpoint for plan changes / cancellation.
  const hasSub = !!data?.upgrade_url;

  function ctaFor(tier: PlanEntry): PlanCTA | null {
    // Fail safe: if we couldn't determine the user's current plan (both
    // the sidecar and OSS plan code missing/empty), offer no plan-change
    // actions rather than risk mislabeling — better to show no button
    // than to send someone to Checkout for a plan they may already hold.
    if (!currentCode) {
      return null;
    }
    // The current tier is marked with a "Current" badge and offers no
    // action button — there's nothing to do on the plan you're already on.
    if (tier.code === currentCode) {
      return null;
    }
    if (hasSub) {
      // Existing subscribers change or cancel their plan through the
      // Stripe Billing Portal (it owns proration). Both "switch up/down"
      // and "downgrade to Free" route there.
      const label =
        tier.monthly_price_cents <= 0 ? "Downgrade" : `Switch to ${tier.display_name}`;
      const key = `tier-${tier.code}`;
      return {
        label: actionPending === key ? "Opening…" : label,
        onClick: () => postBilling(data!.upgrade_url, key),
        disabled: actionPending !== null,
      };
    }
    // No subscription yet. The Free tier is already their plan (handled
    // above as current), so only paid tiers get an Upgrade CTA → Checkout.
    if (tier.monthly_price_cents <= 0) {
      return null;
    }
    const key = `tier-${tier.code}`;
    return {
      label: actionPending === key ? "Opening…" : `Upgrade to ${tier.display_name}`,
      onClick: () =>
        postBilling(`${BILLING_API}/api/billing/checkout`, key, { plan: tier.code }),
      disabled: actionPending !== null,
    };
  }

  // Human label for the current plan in the banner. "default" is the
  // operator-configured self-host plan (not a catalog tier); otherwise
  // prefer the catalog's display name, falling back to the raw code.
  const currentPlanLabel =
    data?.plan_code === "default"
      ? "Default (operator-configured)"
      : planData?.catalog.find((p) => p.code === currentCode)?.display_name ??
        data?.plan_code ??
        "";

  return (
    <PageShell
      eyebrow="Plan & usage"
      title="Billing"
      subtitle="Your current resource caps and month-to-date usage."
    >
      {error && (
        <div
          className="rounded-lg border px-4 py-3 text-sm mb-6"
          style={{
            borderColor: "var(--border)",
            background: "var(--bg-elev)",
            color: "var(--fg-muted)",
          }}
        >
          Couldn&apos;t load your limits. {String(error.message ?? error)}
        </div>
      )}

      {/* Post-checkout reconciliation. Rendered outside the `data` gate so
          it shows even if the usage read is slow or failing — the user
          just paid, and silence is the worst response. A late webhook
          that lands after we gave up still clears the notice, because the
          background poll keeps running. */}
      {reconcile === "pending" && (
        <div
          className="rounded-lg border px-4 py-3 text-sm mb-6"
          style={{
            borderColor: "var(--border)",
            background: "var(--bg-elev)",
            color: "var(--fg-muted)",
          }}
          role="status"
        >
          Finalizing your upgrade… your new plan will appear here in a moment.
        </div>
      )}
      {reconcile === "timeout" && planData?.current.status !== "active" && (
        <div
          className="rounded-lg border px-4 py-3 text-sm mb-6"
          style={{
            borderColor: "var(--border)",
            background: "var(--bg-elev)",
            color: "var(--fg-muted)",
          }}
          role="status"
        >
          Your payment went through, but the new plan hasn&apos;t appeared yet.
          It usually lands within a minute — this page keeps checking, or use
          Refresh below.
        </div>
      )}

      {isLoading && !data && (
        <div className="text-sm text-muted">Loading…</div>
      )}

      {data && (
        <div className="space-y-6">
          {/* Plan summary card */}
          <section
            className="rounded-xl border p-5"
            style={{
              borderColor: "var(--border)",
              background: "var(--bg-panel)",
            }}
          >
            <div className="flex flex-col md:flex-row md:items-center md:justify-between gap-4">
              <div>
                <div className="text-xs uppercase tracking-wide text-muted">
                  Current plan
                </div>
                <div className="text-lg font-semibold text-foreground mt-1">
                  {currentPlanLabel}
                </div>
              </div>
              {BILLING_API && data.upgrade_url && (
                // upgrade_url present → user has an active Stripe
                // subscription. Clicking POSTs to the sidecar, which
                // returns a fresh Stripe Billing Portal URL. From the
                // Portal, users switch plans (Pro ↔ Scale) and cancel.
                <div className="flex items-center gap-2 flex-wrap justify-end">
                  <button
                    type="button"
                    disabled={actionPending !== null}
                    onClick={() => postBilling(data.upgrade_url, "manage")}
                    className="px-3 py-1.5 rounded-md text-sm border hover:bg-background transition disabled:opacity-50 disabled:cursor-not-allowed"
                    style={{ borderColor: "var(--border)", color: "var(--fg)" }}
                  >
                    {actionPending === "manage" ? "Opening…" : "Manage billing"}
                  </button>
                </div>
              )}
            </div>
          </section>

          {/* Plan comparison: every tier and its quota, sourced from the
              sidecar catalog (the SSOT). Hidden on self-host (no
              BILLING_API). The current tier is highlighted; each tier
              carries the right CTA for the user's subscription state. */}
          {BILLING_API && (
            <section
              className="rounded-xl border p-5 space-y-4"
              style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
            >
              <div>
                <div className="text-xs uppercase tracking-wide text-muted">
                  Plans
                </div>
                <div className="text-sm text-muted mt-1">
                  Compare what each tier includes.
                </div>
              </div>

              {planError && (
                <div className="text-sm text-muted">
                  Couldn&apos;t load plans.{" "}
                  <button
                    onClick={() => mutatePlan()}
                    className="underline hover:text-foreground transition"
                  >
                    Retry
                  </button>
                </div>
              )}

              {!planError && !planData && (
                <div className="text-sm text-muted">Loading plans…</div>
              )}

              {planData && (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {planData.catalog.map((tier) => (
                    <PlanCard
                      key={tier.code}
                      tier={tier}
                      isCurrent={tier.code === currentCode}
                      cta={ctaFor(tier)}
                    />
                  ))}
                </div>
              )}
            </section>
          )}

          {/* Usage card */}
          <section
            className="rounded-xl border p-5 space-y-5"
            style={{
              borderColor: "var(--border)",
              background: "var(--bg-panel)",
            }}
          >
            <div>
              <div className="text-xs uppercase tracking-wide text-muted">
                This billing period
              </div>
              <div className="text-sm text-muted mt-1">
                Counters reset at the start of each calendar month (UTC).
              </div>
            </div>

            <UsageRow
              label="Inboxes"
              current={formatNumber(data.usage.agents)}
              limit={formatNumber(data.limits.max_agents)}
              pct={pct(data.usage.agents, data.limits.max_agents)}
            />
            <UsageRow
              label="Domains"
              current={formatNumber(data.usage.domains)}
              limit={formatNumber(data.limits.max_domains)}
              pct={pct(data.usage.domains, data.limits.max_domains)}
            />
            <UsageRow
              label="Messages this month"
              current={formatNumber(data.usage.messages_month)}
              limit={formatNumber(data.limits.max_messages_month)}
              pct={pct(data.usage.messages_month, data.limits.max_messages_month)}
            />
            <UsageRow
              label="Storage"
              current={formatBytes(data.usage.storage_bytes)}
              limit={formatBytes(data.limits.max_storage_bytes)}
              pct={pct(data.usage.storage_bytes, data.limits.max_storage_bytes)}
            />

            <div className="pt-2">
              <button
                onClick={() => {
                  // Both reads, not just usage. The plan label, the
                  // "Current" badge and every tier CTA come from the
                  // sidecar catalog — refreshing usage alone made the
                  // button look broken to anyone who had just upgraded.
                  void mutate();
                  void mutatePlan();
                }}
                className="text-xs text-muted hover:text-foreground transition"
              >
                Refresh
              </button>
            </div>
          </section>
        </div>
      )}
    </PageShell>
  );
}
