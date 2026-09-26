import type { SendingAccessRequestView, SendingAccessView } from "@e2a/sdk/v1";
import { E2AError, E2ANotFoundError } from "@e2a/sdk/v1";
import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

// External sending access (beta): a platform control on the shared sending
// identity. `status` reads GET /v1/account's additive `sending_access` object
// plus the account's latest request (if any); `request` files a new one via
// POST /v1/account/sending-access/request. Filing never grants access by
// itself — only support (or a paid plan) does that.

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

/** True exactly when the account is restricted to the narrow allowed-recipient
 *  set right now — enforcement applies and neither grant (operator approval or
 *  a paid plan) is in effect. Mirrors the server's own decision at send time. */
function isRestricted(access: SendingAccessView): boolean {
  return access.enforcementApplies && !access.sharedExternalApproved && !access.paidExternalSendingEntitled;
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
  if (access.paidExternalSendingEntitled) {
    return "External sending: allowed (paid plan entitlement).";
  }
  if (access.sharedExternalApproved) {
    return "External sending: allowed (approved by an operator).";
  }
  return (
    "External sending: restricted (send to your verified account email and agent inboxes in " +
    "this account; request approval with: e2a sending-access request --use-case <text> " +
    "--recipients <text> --volume <n>)"
  );
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
      ? "Filed (or already pending) — support will review it. Check back with: e2a sending-access status\n"
      : `Note: the account's latest request is already ${result.state}; this filing may be a new appeal.\n`,
  );
}
