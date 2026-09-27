import {
  disallowedRecipients,
  isSendingRestricted,
  parseErrorEnvelope,
  parseExternalSendingNotEnabledError,
  parseRecipientList,
  offeredUnlocks,
  sendingAccessEligibilityLabel,
  sendingAccessEnabledRoute,
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
    expect(copy.headline).toBe("External sending is restricted for this account.");
    expect(copy.body).toBe(
      "Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, verify your own domain or request approval.",
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

  it("lists all three routes as a proper list when billing is enabled and all unlocks apply", () => {
    const copy = sendingAccessNoticeCopy(base, { billingEnabled: true });
    expect(copy.body).toBe(
      "Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, verify your own domain, request approval, or activate a paid base plan.",
    );
  });

  it.each([
    [["operator_approval"], true, "To email other recipients, request approval."],
    [["operator_approval", "verified_domain"], true, "To email other recipients, verify your own domain or request approval."],
    [["operator_approval", "paid_entitlement"], true, "To email other recipients, request approval or activate a paid base plan."],
    [["operator_approval", "paid_entitlement"], false, "To email other recipients, request approval."],
  ])("unlocks %j (billing %s) → %s", (unlocks, billingEnabled, sentence) => {
    const copy = sendingAccessNoticeCopy({ ...base, available_unlocks: unlocks }, { billingEnabled });
    expect(copy.body.endsWith(sentence)).toBe(true);
    expect(copy.headline).toBe("External sending is restricted for this account.");
  });
});

it("says 'below' only when the request form renders under the copy", () => {
  const approvalOnly = { ...base, available_unlocks: ["operator_approval"] };
  expect(sendingAccessNoticeCopy(approvalOnly, { billingEnabled: false, formBelow: true }).body).toMatch(
    /request approval below\.$/,
  );
  expect(sendingAccessNoticeCopy(approvalOnly, { billingEnabled: false }).body).toMatch(/request approval\.$/);
});

describe("unlock-aware grants", () => {
  it("a paid entitlement is not a grant where paid_entitlement is not an unlock", () => {
    const hosted = { ...base, paid_external_sending_entitled: true, available_unlocks: ["operator_approval"] };
    expect(isSendingRestricted(hosted)).toBe(true);
    expect(sendingAccessEligibilityLabel(hosted)).toBeNull();
    expect(sendingAccessEnabledRoute(hosted)).toBeNull();
  });

  it("an absent list (older server) keeps the paid grant", () => {
    expect(isSendingRestricted({ ...base, paid_external_sending_entitled: true })).toBe(false);
  });

  it("operator approval always lifts it and names the route", () => {
    const approved = { ...base, shared_external_approved: true, available_unlocks: ["operator_approval"] };
    expect(isSendingRestricted(approved)).toBe(false);
    expect(sendingAccessEnabledRoute(approved)).toBe("An operator approved external sending for this account.");
  });

  it("offeredUnlocks follows the list and the billing gate", () => {
    expect(offeredUnlocks({ ...base, available_unlocks: ["operator_approval"] }, { billingEnabled: true })).toEqual({
      domain: false, approval: true, paid: false,
    });
    expect(offeredUnlocks(base, { billingEnabled: false })).toEqual({ domain: true, approval: true, paid: false });
    expect(offeredUnlocks(base, { billingEnabled: true })).toEqual({ domain: true, approval: true, paid: true });
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
