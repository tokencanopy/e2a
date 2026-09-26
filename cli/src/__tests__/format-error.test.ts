import { describe, it, expect } from "vitest";
import { E2AError } from "@e2a/sdk/v1";
import { formatError } from "../bin/e2a.js";

// external_sending_not_enabled is the one error code send/reply/forward need a
// richer top-level rendering for: the bare `Error: <message> [<code>]` line
// leaves a caller guessing what "not enabled" means and what to do next. This
// pins that rendering directly, without going through a live send/reply call.
//
// `error.details` on the wire is snake_case (allowed_recipients/recovery_url)
// and is NEVER renamed to the generated ExternalSendingNotEnabledDetails
// model's camelCase fields (see bin/e2a.ts's ExternalSendingNotEnabledDetailsWire
// comment) — these fixtures use the real wire shape on purpose.
describe("formatError", () => {
  it("renders external_sending_not_enabled with allowed destinations and the recovery URL", () => {
    const err = new E2AError({
      code: "external_sending_not_enabled",
      message: "the account may not send to one or more of the recipients",
      status: 403,
      retryable: false,
      details: {
        allowed_recipients: ["verified_owner_email", "same_account_agents"],
        recovery_url: "https://e2a.dev/dashboard/sending-access",
      },
    });

    const out = formatError(err);
    expect(out).toContain(
      "Error: the account may not send to one or more of the recipients [external_sending_not_enabled]",
    );
    expect(out).toContain("allowed destinations: verified_owner_email, same_account_agents");
    expect(out).toContain("recover at: https://e2a.dev/dashboard/sending-access");
    expect(out).toContain("e2a sending-access request");
    expect(out.toLowerCase()).toContain("do not retry");
  });

  it("falls back to a generic allowed-destinations line when details are absent", () => {
    const err = new E2AError({
      code: "external_sending_not_enabled",
      message: "refused",
      status: 403,
      retryable: false,
    });
    const out = formatError(err);
    expect(out).toContain(
      "allowed destinations: your verified account email and agent inboxes in this account",
    );
    expect(out).not.toContain("recover at:");
  });

  it("does not add the rich rendering for unrelated error codes", () => {
    const err = new E2AError({
      code: "not_found",
      message: "no such message",
      status: 404,
      retryable: false,
    });
    expect(formatError(err)).toBe("Error: no such message [not_found]\n");
  });

  it("handles a non-E2AError throw without crashing", () => {
    expect(formatError(new Error("boom"))).toBe("Error: boom\n");
  });
});
