import type { SendingAccessRequestView, SendingAccessView } from "@e2a/sdk/v1";
import { E2AError, E2ANotFoundError } from "@e2a/sdk/v1";
import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

// External sending access (beta): a platform control on the shared sending
// identity. `status` reads GET /v1/account's additive `sending_access` object
// plus the account's latest request (if any); `request` files a new one via
// POST /v1/account/sending-access/request. Filing never grants access by
// itself — an operator's approval does (or, where the deployment's
// `available_unlocks` lists them, a verified domain or a paid plan).

export interface SendingAccessStatusOptions {
  json?: boolean;
}

export interface SendingAccessRequestOptions {
  useCase?: string;
  recipients?: string;
  volume?: string;
  json?: boolean;
}

export const SENDING_ACCESS_REQUEST_USAGE =
  "usage: e2a sending-access request --use-case <text> --recipients <text> --volume <n> [--json]";

/** The deployment's unlock set. A server that predates `available_unlocks`
 *  omits it and accepts all three, so absence reads as all three. */
export function availableUnlocks(access: SendingAccessView): string[] {
  return access.availableUnlocks ?? ["operator_approval", "verified_domain", "paid_entitlement"];
}

/** True when the paid entitlement actually lifts the restriction here. */
function paidUnlocks(access: SendingAccessView): boolean {
  return access.paidExternalSendingEntitled && availableUnlocks(access).includes("paid_entitlement");
}

/** True exactly when the account is restricted to the narrow allowed-recipient
 *  set right now — enforcement applies and no account-level grant in effect
 *  (operator approval, or a paid plan where the deployment accepts one).
 *  Mirrors the server's own decision at send time. */
export function isRestricted(access: SendingAccessView): boolean {
  return access.enforcementApplies && !access.sharedExternalApproved && !paidUnlocks(access);
}

const REQUEST_HINT =
  "e2a sending-access request --use-case <text> --recipients <text> --volume <n>";

/** The one-line restricted summary shared by `whoami` and `status`: what the
 *  account can still reach, and only the recovery routes the deployment
 *  honors. */
export function restrictedLine(access: SendingAccessView): string {
  const unlocks = availableUnlocks(access);
  const routes = [`request approval with: ${REQUEST_HINT}`];
  if (unlocks.includes("verified_domain")) routes.push("or send from your own verified domain");
  if (unlocks.includes("paid_entitlement")) routes.push("or choose a paid plan");
  return (
    "External sending: restricted (send to your verified account email and agent inboxes in " +
    `this account; ${routes.join(" ")})`
  );
}

function describeAccess(access: SendingAccessView | undefined): string {
  if (!access) {
    // Additive and omitted when unavailable (self-host, or a deployment that
    // doesn't run this control) — say so rather than printing nothing.
    return "External sending: not reported by this deployment.";
  }
  if (!access.enforcementApplies) {
    return "External sending: unrestricted (this control does not apply to this account).";
  }
  if (access.sharedExternalApproved) {
    return "External sending: allowed (approved by an operator).";
  }
  if (paidUnlocks(access)) {
    return "External sending: allowed (paid plan entitlement).";
  }
  return restrictedLine(access);
}

function describeRequest(req: SendingAccessRequestView): string {
  return `latest request: ${req.id}\tstate=${req.state}\tfiled=${new Date(req.createdAt).toISOString()}`;
}

export async function sendingAccessStatus(opts: SendingAccessStatusOptions): Promise<void> {
  const client = createClient();
  const account = await client.account.get();
  let latest: SendingAccessRequestView | undefined;
  try {
    latest = await client.account.getSendingAccessRequest();
  } catch (err) {
    // 404 not_found means the account has never filed one, and 501
    // not_implemented means the deployment does not enable external sending
    // access at all — neither is an error for a status readout.
    const disabled = err instanceof E2AError && err.code === "not_implemented";
    if (!(err instanceof E2ANotFoundError) && !disabled) throw err;
  }

  if (opts.json) {
    // Raw objects, not a hand-picked subset — a caller scripting against this
    // should see exactly what the API returns.
    process.stdout.write(
      JSON.stringify({ sendingAccess: account.sendingAccess ?? null, latestRequest: latest ?? null }) +
        "\n",
    );
    return;
  }

  process.stdout.write(describeAccess(account.sendingAccess) + "\n");
  if (account.sendingAccess) {
    process.stdout.write(`available unlocks: ${availableUnlocks(account.sendingAccess).join(", ")}\n`);
  }
  if (latest) process.stdout.write(describeRequest(latest) + "\n");
}

export async function sendingAccessRequest(opts: SendingAccessRequestOptions): Promise<void> {
  if (!opts.useCase || !opts.recipients || !opts.volume) fail(EXIT.USAGE, SENDING_ACCESS_REQUEST_USAGE);
  const volume = Number(opts.volume);
  if (!Number.isInteger(volume) || volume < 1 || volume > 1_000_000) {
    fail(EXIT.USAGE, "--volume must be an integer from 1 to 1000000");
  }

  const client = createClient();
  const result = await client.account.requestSendingAccess({
    useCase: opts.useCase,
    recipients: opts.recipients,
    expectedDailyVolume: volume,
  });

  if (opts.json) {
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }
  process.stdout.write(`${result.id}\t${result.state}\n`);
  process.stderr.write(
    result.state === "pending"
      ? "Filed (or already pending) — an operator will review it and the account owner will get an email with the decision. Do not re-file. Check back with: e2a sending-access status\n"
      : `Note: the account's latest request is already ${result.state}; this filing may be a new appeal.\n`,
  );
}
