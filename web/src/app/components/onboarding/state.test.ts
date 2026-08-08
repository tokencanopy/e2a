import {
  deriveChecklistStep,
  buildDomainProgress,
  getResumeTarget,
  getAddressType,
  isValidSlug,
  isValidDomain,
  isValidLocalPart,
  inboundCapability,
  outboundCapability,
  canReceive,
} from "./state";
import type { DomainInfo } from "./types";
import type { DashboardAgent } from "../types";

// ── Fixtures ─────────────────────────────────────────────

function makeDomain(overrides: Partial<DomainInfo> = {}): DomainInfo {
  return {
    domain: "mail.example.com",
    verified: false,
    verification_token: "e2a-verify=abc123",
    dns_records: [
      {
        type: "TXT",
        name: "mail.example.com",
        value: "e2a-verify=abc123",
        priority: null,
        purpose: "ownership",
        status: "pending",
      },
      {
        type: "MX",
        name: "mail.example.com",
        value: "mx.e2a.dev",
        priority: 10,
        purpose: "inbound_mx",
        status: "pending",
      },
    ],
    created_at: "2026-01-01T00:00:00Z",
    verified_at: null,
    ...overrides,
  };
}

function makeAgent(overrides: Partial<DashboardAgent> = {}): DashboardAgent {
  return {
    domain: "mail.example.com",
    email: "support@mail.example.com",
    name: "support",
    domain_verified: true,
    created_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

// ── deriveChecklistStep ──────────────────────────────────

// The capability readers are the single seam between the UI and the two
// independent axes. They must prefer the backend's `capabilities` object, fall
// back to the legacy verified/sending_status pair when it is absent, and pass
// unknown values through untouched (both axes are documented open sets).
describe("capability readers", () => {
  it("prefer capabilities over the legacy fields", () => {
    const d = makeDomain({
      verified: false,
      sending_status: "none",
      capabilities: { inbound: "verified", outbound: "failed" },
    });
    expect(inboundCapability(d)).toBe("verified");
    expect(outboundCapability(d)).toBe("failed");
    expect(canReceive(d)).toBe(true);
  });

  it("fall back to verified/sending_status when capabilities is absent", () => {
    const unverified = makeDomain();
    expect(inboundCapability(unverified)).toBe("pending");
    expect(outboundCapability(unverified)).toBe("none");
    expect(canReceive(unverified)).toBe(false);

    const ready = makeDomain({ verified: true, sending_status: "verified" });
    expect(inboundCapability(ready)).toBe("verified");
    expect(outboundCapability(ready)).toBe("verified");
    expect(canReceive(ready)).toBe(true);
  });

  it("pass unknown open-set values through unchanged", () => {
    const d = makeDomain({
      capabilities: {
        inbound: "provisioning" as never,
        outbound: "provisioning" as never,
      },
    });
    expect(inboundCapability(d)).toBe("provisioning");
    expect(outboundCapability(d)).toBe("provisioning");
    // Anything that is not exactly "verified" cannot receive — fail closed.
    expect(canReceive(d)).toBe(false);
  });

  it("treat the axes as independent in both directions", () => {
    const sendOnly = makeDomain({
      capabilities: { inbound: "pending", outbound: "verified" },
    });
    expect(canReceive(sendOnly)).toBe(false);
    expect(outboundCapability(sendOnly)).toBe("verified");

    const receiveOnly = makeDomain({
      capabilities: { inbound: "verified", outbound: "none" },
    });
    expect(canReceive(receiveOnly)).toBe(true);
    expect(outboundCapability(receiveOnly)).toBe("none");
  });

  // deriveChecklistStep reads the inbound axis through canReceive, so a domain
  // whose capabilities say inbound-verified advances even if the legacy boolean
  // has not caught up.
  it("drive checklist derivation off the inbound axis", () => {
    const d = makeDomain({
      verified: false,
      capabilities: { inbound: "verified", outbound: "none" },
    });
    expect(deriveChecklistStep(d, [])).toBe("domain_verified");
  });
});

describe("deriveChecklistStep", () => {
  it("returns domain_added for unverified domain with no agents", () => {
    const domain = makeDomain({ verified: false });
    expect(deriveChecklistStep(domain, [])).toBe("domain_added");
  });

  it("returns domain_added for unverified domain even with agents on other domains", () => {
    const domain = makeDomain({ verified: false });
    const agent = makeAgent({ domain: "other.com" });
    expect(deriveChecklistStep(domain, [agent])).toBe("domain_added");
  });

  it("returns domain_verified for verified domain with no agents", () => {
    const domain = makeDomain({ verified: true });
    expect(deriveChecklistStep(domain, [])).toBe("domain_verified");
  });

  it("returns agent_created for verified domain with agents", () => {
    const domain = makeDomain({ verified: true });
    const agent = makeAgent({ domain: "mail.example.com" });
    expect(deriveChecklistStep(domain, [agent])).toBe("agent_created");
  });
});

// ── buildDomainProgress ──────────────────────────────────

describe("buildDomainProgress", () => {
  it("returns empty array for no domains", () => {
    expect(buildDomainProgress([], [])).toEqual([]);
  });

  it("builds progress for multiple domains", () => {
    const d1 = makeDomain({ domain: "a.com", verified: false });
    const d2 = makeDomain({ domain: "b.com", verified: true });
    const agent = makeAgent({ domain: "b.com" });

    const progress = buildDomainProgress([d1, d2], [agent]);
    expect(progress).toHaveLength(2);
    expect(progress[0].step).toBe("domain_added");
    expect(progress[0].agentCount).toBe(0);
    expect(progress[1].step).toBe("agent_created");
    expect(progress[1].agentCount).toBe(1);
  });
});

// ── getResumeTarget ──────────────────────────────────────

describe("getResumeTarget", () => {
  it("resumes at dns for domain_added", () => {
    const progress = { domain: makeDomain(), step: "domain_added" as const, agentCount: 0 };
    expect(getResumeTarget(progress)).toBe("dns");
  });

  it("resumes at create_agent for domain_verified", () => {
    const progress = { domain: makeDomain({ verified: true }), step: "domain_verified" as const, agentCount: 0 };
    expect(getResumeTarget(progress)).toBe("create_agent");
  });

  it("returns null for agent_created — user should go to Domains/Agents, not onboarding", () => {
    const progress = { domain: makeDomain({ verified: true }), step: "agent_created" as const, agentCount: 1 };
    expect(getResumeTarget(progress)).toBeNull();
  });
});

describe("getAddressType", () => {
  it("returns shared for agents.e2a.dev", () => {
    expect(getAddressType("agents.e2a.dev")).toBe("shared");
  });
  it("returns custom for other domains", () => {
    expect(getAddressType("mail.example.com")).toBe("custom");
  });
});

// ── Validation ───────────────────────────────────────────

describe("isValidSlug", () => {
  it("accepts valid slugs (2-40 chars)", () => {
    expect(isValidSlug("my-agent")).toBe(true);
    expect(isValidSlug("ab")).toBe(true);
    expect(isValidSlug("bot123")).toBe(true);
    expect(isValidSlug("a".repeat(40))).toBe(true);
  });

  it("rejects slugs that are too short or too long", () => {
    expect(isValidSlug("")).toBe(false);
    expect(isValidSlug("a")).toBe(false);
    expect(isValidSlug("a".repeat(41))).toBe(false);
  });

  it("rejects invalid format", () => {
    expect(isValidSlug("-start")).toBe(false);
    expect(isValidSlug("end-")).toBe(false);
    expect(isValidSlug("UPPER")).toBe(false);
    expect(isValidSlug("has space")).toBe(false);
  });

  it("rejects reserved slugs", () => {
    expect(isValidSlug("admin")).toBe(false);
    expect(isValidSlug("postmaster")).toBe(false);
    expect(isValidSlug("test")).toBe(false);
    expect(isValidSlug("noreply")).toBe(false);
    expect(isValidSlug("demo")).toBe(false);
    expect(isValidSlug("api")).toBe(false);
    expect(isValidSlug("system")).toBe(false);
    expect(isValidSlug("root")).toBe(false);
  });
});

describe("isValidDomain", () => {
  it("accepts valid domains", () => {
    expect(isValidDomain("mail.example.com")).toBe(true);
    expect(isValidDomain("example.com")).toBe(true);
    expect(isValidDomain("sub.deep.example.com")).toBe(true);
  });

  it("rejects invalid domains", () => {
    expect(isValidDomain("")).toBe(false);
    expect(isValidDomain("nodot")).toBe(false);
    expect(isValidDomain("UPPER.com")).toBe(false);
    expect(isValidDomain("-start.com")).toBe(false);
  });
});

describe("isValidLocalPart", () => {
  it("accepts valid local parts", () => {
    expect(isValidLocalPart("support")).toBe(true);
    expect(isValidLocalPart("my.agent")).toBe(true);
    expect(isValidLocalPart("agent-1")).toBe(true);
  });

  it("rejects invalid local parts", () => {
    expect(isValidLocalPart("")).toBe(false);
    expect(isValidLocalPart(".start")).toBe(false);
    expect(isValidLocalPart("UPPER")).toBe(false);
  });
});

