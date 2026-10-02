import type {
  ProtectionConfigView,
  ProtectionConfigRequest,
  ProtectionDirectionView,
  ProtectionOutboundView,
} from "@e2a/sdk/v1";
import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

export interface ProtectionGetOptions {
  json?: boolean;
}

export interface ProtectionSetOptions {
  outboundReview?: string;
  inboundReview?: string;
  suppressNotifications?: string;
  json?: boolean;
}

const GET_USAGE = "usage: e2a protection get <agent-email> [--json]";
const SET_USAGE =
  "usage: e2a protection set <agent-email> [--outbound-review on|off] [--inbound-review on|off] [--suppress-notifications on|off] [--json]";

function summarize(config: ProtectionConfigView): string {
  const dir = (d: ProtectionDirectionView) =>
    `gate=${d.gate.policy ?? "open"}/${d.gate.action ?? "flag"} scan=${d.scan.sensitivity ?? "off"}`;
  return (
    `outbound: ${dir(config.outbound)} require_review=${config.outbound.requireReview ? "on" : "off"}\n` +
    `inbound:  ${dir(config.inbound)}\n` +
    `holds:    ttl=${config.holds.ttlSeconds ?? 604800}s on_expiry=${config.holds.onExpiry ?? "reject"} notifications=${config.holds.suppressNotifications ? "suppressed" : "enabled"}\n`
  );
}

export async function protectionGet(
  email: string | undefined,
  opts: ProtectionGetOptions,
): Promise<void> {
  if (!email) fail(EXIT.USAGE, GET_USAGE);

  const client = createClient();
  const config = await client.agents.getProtection(email);
  process.stdout.write(opts.json ? JSON.stringify(config) + "\n" : summarize(config));
}

/**
 * Flip one direction's review posture, touching ONLY the requested knobs.
 * "off" = gate non-matches are flagged-through and the content scan is
 * disabled (a scan can hold too, so review-off must silence both).
 * "on" = gate non-matches are held for review, and a disabled scan is
 * re-enabled at medium — under the default gate policy "open" every sender
 * matches (the gate action never fires), so an off→on round-trip that only
 * touched the gate would LOOK on while holding nothing. A scan already
 * tuned to low/high is left alone.
 */
function applyReview(direction: ProtectionDirectionView, mode: "on" | "off"): void {
  // Casts because the generated models type these as TS enums; the literals
  // are the enums' wire values (flag/review, off/medium).
  direction.gate.action = (mode === "on" ? "review" : "flag") as typeof direction.gate.action;
  if (mode === "off") {
    direction.scan.sensitivity = "off" as typeof direction.scan.sensitivity;
  } else if (!direction.scan.sensitivity || (direction.scan.sensitivity as string) === "off") {
    direction.scan.sensitivity = "medium" as typeof direction.scan.sensitivity;
  }
}

/**
 * Outbound review also flips require_review (#989). Without it, "hold for
 * review" only fires on recipients that fail the gate, so under the default
 * "open" policy the action never runs and the switch would look on while
 * holding nothing; the scan below was the old workaround for that. With
 * require_review the gate holds every send outright.
 */
function applyOutboundReview(direction: ProtectionOutboundView, mode: "on" | "off"): void {
  applyReview(direction, mode);
  direction.requireReview = mode === "on";
}

export async function protectionSet(
  email: string | undefined,
  opts: ProtectionSetOptions,
): Promise<void> {
  if (!email) fail(EXIT.USAGE, SET_USAGE);
  for (const v of [opts.outboundReview, opts.inboundReview, opts.suppressNotifications]) {
    if (v !== undefined && v !== "on" && v !== "off") fail(EXIT.USAGE, SET_USAGE);
  }
  if (
    opts.outboundReview === undefined &&
    opts.inboundReview === undefined &&
    opts.suppressNotifications === undefined
  ) {
    fail(EXIT.USAGE, SET_USAGE);
  }

  const client = createClient();
  // GET first, and never PUT unless it succeeded: /protection is full-replace,
  // so writing a from-scratch doc after a failed read would reset every knob
  // we weren't asked to touch (the clobber bug in the bash prototype of this
  // flow). A thrown GET propagates and the PUT below is never reached.
  const config = await client.agents.getProtection(email);

  if (opts.outboundReview) applyOutboundReview(config.outbound, opts.outboundReview as "on" | "off");
  if (opts.inboundReview) applyReview(config.inbound, opts.inboundReview as "on" | "off");
  if (opts.suppressNotifications !== undefined) {
    config.holds.suppressNotifications = opts.suppressNotifications === "on";
  }

  // ProtectionConfigRequest is the View's field-for-field request twin (the
  // spec splits them only so responses can be additive-open while requests
  // stay strict), but the generated enum types are nominal, so the
  // shape-identical view needs an explicit cast to PUT back.
  const updated = await client.agents.replaceProtection(
    email,
    config as unknown as ProtectionConfigRequest,
  );
  process.stdout.write(opts.json ? JSON.stringify(updated) + "\n" : summarize(updated));
}
