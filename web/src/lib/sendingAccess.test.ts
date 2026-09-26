import {
  disallowedRecipients,
  isSendingRestricted,
  parseErrorEnvelope,
  parseExternalSendingNotEnabledError,
  parseRecipientList,
  sendingAccessEligibilityLabel,
  sendingAccessNoticeCopy,
  type SendingAccessStatus,
} from "./sendingAccess";

const base: SendingAccessStatus = {
  enforcement_applies: true,
  shared_external_approved: false,
  paid_external_sending_entitled: false,
  owner_recipient_verified: true,
};

describe("isSendingRestricted", () => {
  it("is false for a missing status (disabled deployment)", () => {
    expect(isSendingRestricted(undefined)).toBe(false);
    expect(isSendingRestricted(null)).toBe(false);
  });

  it("is false when enforcement doesn't apply (shadow mode / out of cohort)", () => {
    expect(isSendingRestricted({ ...base, enforcement_applies: false })).toBe(false);
  });

  it("is true when enforcement applies and neither grant is held", () => {
    expect(isSendingRestricted(base)).toBe(true);
  });

  it("is false once operator-approved", () => {
    expect(isSendingRestricted({ ...base, shared_external_approved: true })).toBe(false);
  });

  it("is false once a paid base plan entitles it", () => {
    expect(isSendingRestricted({ ...base, paid_external_sending_entitled: true })).toBe(false);
  });
});

describe("sendingAccessEligibilityLabel", () => {
  it("is null when enforcement doesn't apply", () => {
    expect(sendingAccessEligibilityLabel({ ...base, enforcement_applies: false })).toBeNull();
  });

  it("is null while still restricted", () => {
    expect(sendingAccessEligibilityLabel(base)).toBeNull();
  });

  it("names the operator approval grant", () => {
    expect(sendingAccessEligibilityLabel({ ...base, shared_external_approved: true })).toBe(
      "Operator-approved external sending",
    );
  });

  it("names the paid-plan grant", () => {
    expect(sendingAccessEligibilityLabel({ ...base, paid_external_sending_entitled: true })).toBe(
      "Paid plan: external sending enabled",
    );
  });

  it("prefers the paid-plan label when both grants are held", () => {
    expect(
      sendingAccessEligibilityLabel({
        ...base,
        shared_external_approved: true,
        paid_external_sending_entitled: true,
      }),
    ).toBe("Paid plan: external sending enabled");
  });
});

describe("sendingAccessNoticeCopy", () => {
  it("offers the verified-email destination when owner proof exists", () => {
    const copy = sendingAccessNoticeCopy(base, { billingEnabled: false });
    expect(copy.headline).toBe("Your inbox is ready. External sending is restricted.");
    expect(copy.body).toBe(
      "Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, send from your own verified domain or request approval.",
    );
  });

  it("omits the verified-email destination without owner proof", () => {
    const copy = sendingAccessNoticeCopy(
      { ...base, owner_recipient_verified: false },
      { billingEnabled: false },
    );
    expect(copy.body).not.toMatch(/verified account email/);
    expect(copy.body).toMatch(/testing/);
  });

  it("appends the paid-plan option only when the billing gate is enabled", () => {
    const copy = sendingAccessNoticeCopy(base, { billingEnabled: true });
    expect(copy.body).toBe(
      "Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, send from your own verified domain or request approval, activate a paid base plan.",
    );
  });
});

describe("parseRecipientList", () => {
  it("splits, trims, and drops empty entries", () => {
    expect(parseRecipientList(" a@x.example ,, b@y.example,")).toEqual(["a@x.example", "b@y.example"]);
  });

  it("returns an empty array for blank input", () => {
    expect(parseRecipientList("   ")).toEqual([]);
  });
});

describe("disallowedRecipients", () => {
  it("allows same-account agent addresses, case-insensitively", () => {
    expect(
      disallowedRecipients(["Agent@Acme.dev"], {
        accountAgentEmails: ["agent@acme.dev"],
        ownerVerified: false,
      }),
    ).toEqual([]);
  });

  it("flags addresses that are neither an agent nor the verified owner", () => {
    expect(
      disallowedRecipients(["customer@bigco.example"], {
        accountAgentEmails: ["agent@acme.dev"],
        ownerEmail: "owner@acme.dev",
        ownerVerified: true,
      }),
    ).toEqual(["customer@bigco.example"]);
  });

  it("allows the owner's email only when verified", () => {
    const opts = { accountAgentEmails: [], ownerEmail: "owner@acme.dev" };
    expect(disallowedRecipients(["owner@acme.dev"], { ...opts, ownerVerified: true })).toEqual([]);
    expect(disallowedRecipients(["owner@acme.dev"], { ...opts, ownerVerified: false })).toEqual([
      "owner@acme.dev",
    ]);
  });

  it("extracts the address out of a display-name token", () => {
    expect(
      disallowedRecipients(["Big Co <customer@bigco.example>"], {
        accountAgentEmails: [],
        ownerVerified: false,
      }),
    ).toEqual(["customer@bigco.example"]);
  });

  it("deduplicates and preserves first-seen order", () => {
    expect(
      disallowedRecipients(["b@x.example", "a@x.example", "b@x.example"], {
        accountAgentEmails: [],
        ownerVerified: false,
      }),
    ).toEqual(["b@x.example", "a@x.example"]);
  });

  it("ignores blank tokens", () => {
    expect(
      disallowedRecipients([" ", ""], { accountAgentEmails: [], ownerVerified: false }),
    ).toEqual([]);
  });
});

describe("parseErrorEnvelope", () => {
  it("extracts code and message from a JSON error body", () => {
    expect(
      parseErrorEnvelope(JSON.stringify({ error: { code: "rate_limited", message: "slow down" } })),
    ).toEqual({ code: "rate_limited", message: "slow down" });
  });

  it("falls back to the raw text when the body isn't JSON", () => {
    expect(parseErrorEnvelope("boom")).toEqual({ code: "", message: "boom" });
  });
});

describe("parseExternalSendingNotEnabledError", () => {
  it("parses allowed_recipients and recovery_url out of the details", () => {
    const raw = JSON.stringify({
      error: {
        code: "external_sending_not_enabled",
        message: "This account may not send to one or more recipients.",
        details: {
          allowed_recipients: ["verified_owner_email", "same_account_agents"],
          recovery_url: "https://e2a.test/sending-access",
        },
      },
    });
    expect(parseExternalSendingNotEnabledError(raw)).toEqual({
      message: "This account may not send to one or more recipients.",
      allowedRecipients: ["verified_owner_email", "same_account_agents"],
      recoveryUrl: "https://e2a.test/sending-access",
    });
  });

  it("returns null for a different error code", () => {
    const raw = JSON.stringify({ error: { code: "forbidden", message: "nope" } });
    expect(parseExternalSendingNotEnabledError(raw)).toBeNull();
  });

  it("returns null for a non-JSON body", () => {
    expect(parseExternalSendingNotEnabledError("plain text")).toBeNull();
  });

  it("defaults gracefully when details are missing", () => {
    const raw = JSON.stringify({ error: { code: "external_sending_not_enabled" } });
    expect(parseExternalSendingNotEnabledError(raw)).toEqual({
      message: "External sending is not enabled for this account.",
      allowedRecipients: [],
      recoveryUrl: undefined,
    });
  });
});
