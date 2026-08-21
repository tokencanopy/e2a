import { randomBytes } from "node:crypto";
import type { ApiClient, RawResponse } from "./client.ts";
import { isProductionTarget, loadEnv } from "./env.ts";

const RUN_ID = randomBytes(3).toString("hex");

export function runId(): string {
  return RUN_ID;
}

export function uniqueSlug(prefix = "e2etest"): string {
  return `${prefix}-${RUN_ID}-${randomBytes(3).toString("hex")}`;
}

// Environment-aware fixture-domain suffix — THE central, obvious place the
// domain suites decide which Cloudflare zone-subtree a fixture domain lives
// under: `<zone>` against production, `staging.<zone>` everywhere else. Do
// NOT mint a fixture domain as a bare `${uniqueSlug(...)}.${CF_ZONE_NAME}`.
// The staging AWS IAM policy (e2a-ops PR #318) Deny-fences every mutating
// SES action (CreateEmailIdentity, PutEmailIdentityDkimSigningAttributes,
// SetIdentityMailFromDomain, DeleteEmailIdentity, ...) to
// `identity/*.staging.trymnexa.com`, and with sender_identity enabled on
// staging EVERY registered domain touches SES: verify auto-enqueues identity
// provisioning, and delete runs a best-effort deprovision whose AccessDenied
// leaves the teardown receipt fail-closed at "pending" — so an out-of-fence
// fixture strands its DNS records (cleanup is contract-gated on a
// "confirmed" receipt) and fails the run, reading like a code bug instead of
// a naming mismatch. Driven by the SAME apiUrl-derived signal
// (isProductionTarget) that already gates the destructive-prod opt-in and
// the event-coverage-gate's target detection — not a new, independently
// settable flag.
export function fixtureDomainSuffix(apiUrl: string, zoneName: string): string {
  return isProductionTarget(apiUrl) ? zoneName : `staging.${zoneName}`;
}

export function uniqueSubject(label: string): string {
  return `[e2e-${RUN_ID}] ${label} ${Date.now()}`;
}

export function uniqueIdempotencyKey(): string {
  return `idem-${RUN_ID}-${randomBytes(6).toString("hex")}`;
}

export const SINK_EMAIL = loadEnv().sinkEmail;

// holdAllOutbound replaces the retired `hitl_enabled` flag. It sets an
// outbound review gate with policy=allowlist + action=review and an empty
// allowlist, so every recipient is unknown and every send is held for
// review (status=pending_review). The /protection sub-resource is a full
// replace (PUT), so we send the complete inbound/outbound/holds shape.
export function holdAllOutbound<T = unknown>(
  client: ApiClient,
  email: string,
): Promise<RawResponse<T>> {
  return client.put<T>(`/v1/agents/${encodeURIComponent(email)}/protection`, {
    body: {
      inbound: { gate: {}, scan: {} },
      outbound: { gate: { policy: "allowlist", action: "review", allowlist: [] }, scan: {} },
      holds: {},
    },
  });
}
