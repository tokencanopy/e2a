import { beforeEach, describe, expect, it, vi } from "vitest";

const mockSignupAgent = vi.fn();
const mockVerify = vi.fn();

vi.mock("@e2a/sdk/v1", () => ({
  signupAgent: mockSignupAgent,
  E2AClient: vi.fn(function () {
    return { agentSignup: { verify: mockVerify } };
  }),
}));

vi.mock("../config.js", () => ({
  loadConfig: vi.fn(() => ({ api_url: "https://e2a.example.test" })),
}));

describe("signup commands", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.spyOn(process.stdout, "write").mockImplementation(() => true);
  });

  it("creates a provisional identity without loading an API key", async () => {
    mockSignupAgent.mockResolvedValue({
      inbox: "build-bot@agents.example.test",
      apiKey: "e2a_agt_signup",
      status: "pending",
      humanEmail: "owner@example.test",
      verificationExpiresAt: new Date("2026-09-03T00:00:00Z"),
    });
    const { signupCreate } = await import("../commands/signup.js");

    await signupCreate({
      humanEmail: "owner@example.test",
      displayName: "Build Bot",
      noteToHuman: "Created for the build queue.",
      harness: "codex",
      json: true,
    });

    expect(mockSignupAgent).toHaveBeenCalledWith(
      {
        humanEmail: "owner@example.test",
        displayName: "Build Bot",
        noteToHuman: "Created for the build queue.",
        harness: "codex",
      },
      { baseUrl: "https://e2a.example.test" },
    );
    expect(process.stdout.write).toHaveBeenCalledWith(
      expect.stringContaining('"apiKey":"e2a_agt_signup"'),
    );
  });

  it("verifies with the returned agent key and optional review gate", async () => {
    mockVerify.mockResolvedValue({ status: "verified", reviewOutbound: true });
    const { signupVerify } = await import("../commands/signup.js");

    await signupVerify({
      apiKey: "e2a_agt_signup",
      code: "123456",
      reviewOutbound: true,
      json: true,
    });

    const { E2AClient } = await import("@e2a/sdk/v1");
    expect(E2AClient).toHaveBeenCalledWith({
      apiKey: "e2a_agt_signup",
      baseUrl: "https://e2a.example.test",
    });
    expect(mockVerify).toHaveBeenCalledWith({ code: "123456", reviewOutbound: true });
  });
});
