import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { E2ANotFoundError } from "@e2a/sdk/v1";

const mockAccountGet = vi.fn();
const mockGetRequest = vi.fn();
const mockRequestAccess = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    account: {
      get: mockAccountGet,
      getSendingAccessRequest: mockGetRequest,
      requestSendingAccess: mockRequestAccess,
    },
  })),
}));

function makeAccount(sendingAccess?: Record<string, unknown>) {
  return {
    user: { id: "usr_1", email: "owner@example.com" },
    scope: "account",
    planCode: "free",
    limits: { maxAgents: 5, maxDomains: 1, maxMessagesMonth: 1000, maxStorageBytes: 0 },
    usage: { agents: 2, domains: 0, messagesMonth: 17, storageBytes: 0 },
    ...(sendingAccess ? { sendingAccess } : {}),
  };
}

const RESTRICTED = {
  enforcementApplies: true,
  sharedExternalApproved: false,
  paidExternalSendingEntitled: false,
  ownerRecipientVerified: true,
};

describe("sending-access commands", () => {
  let stdout: ReturnType<typeof vi.spyOn>;
  let stderr: ReturnType<typeof vi.spyOn>;
  let exit: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    stdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    stderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    exit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  describe("sendingAccessStatus", () => {
    it("shows the restricted line and the latest request when both are present", async () => {
      mockAccountGet.mockResolvedValue(makeAccount(RESTRICTED));
      mockGetRequest.mockResolvedValue({
        id: "sar_1",
        state: "pending",
        useCase: "billing",
        recipients: "customers",
        expectedDailyVolume: 10,
        createdAt: new Date("2026-01-01T00:00:00Z"),
      });
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});

      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("External sending: restricted");
      expect(out).toContain("e2a sending-access request");
      expect(out).toContain("latest request: sar_1");
      expect(out).toContain("state=pending");
    });

    it("tolerates 404 not_found (never filed) and omits the latest-request line", async () => {
      mockAccountGet.mockResolvedValue(makeAccount(RESTRICTED));
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});

      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("External sending: restricted");
      expect(out).not.toContain("latest request:");
    });

    it("rethrows a non-404 error from getSendingAccessRequest", async () => {
      mockAccountGet.mockResolvedValue(makeAccount(RESTRICTED));
      mockGetRequest.mockRejectedValue(new Error("boom"));
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await expect(sendingAccessStatus({})).rejects.toThrow("boom");
    });

    it("reports unrestricted when enforcement does not apply", async () => {
      mockAccountGet.mockResolvedValue(
        makeAccount({ ...RESTRICTED, enforcementApplies: false }),
      );
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});
      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("External sending: unrestricted");
    });

    it("reports the paid-entitlement grant", async () => {
      mockAccountGet.mockResolvedValue(
        makeAccount({ ...RESTRICTED, paidExternalSendingEntitled: true }),
      );
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});
      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("allowed (paid plan entitlement)");
    });

    it("reports the operator-approved grant", async () => {
      mockAccountGet.mockResolvedValue(
        makeAccount({ ...RESTRICTED, sharedExternalApproved: true }),
      );
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});
      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("allowed (approved by an operator)");
    });

    it("says sending_access is not reported when the field is omitted", async () => {
      mockAccountGet.mockResolvedValue(makeAccount());
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({});
      const out = stdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");
      expect(out).toContain("not reported by this deployment");
    });

    it("--json emits the raw sendingAccess and latestRequest objects, null when absent", async () => {
      mockAccountGet.mockResolvedValue(makeAccount(RESTRICTED));
      mockGetRequest.mockRejectedValue(
        new E2ANotFoundError({ code: "not_found", message: "none", status: 404, retryable: false }),
      );
      const { sendingAccessStatus } = await import("../commands/sending-access.js");
      await sendingAccessStatus({ json: true });
      const payload = JSON.parse(String(stdout.mock.calls[0][0]));
      expect(payload.sendingAccess).toEqual(RESTRICTED);
      expect(payload.latestRequest).toBeNull();
    });
  });

  describe("sendingAccessRequest", () => {
    it("requires --use-case, --recipients, and --volume", async () => {
      const { sendingAccessRequest } = await import("../commands/sending-access.js");
      await expect(
        sendingAccessRequest({ recipients: "customers", volume: "10" }),
      ).rejects.toThrow("process.exit");
      expect(exit).toHaveBeenCalledWith(2);
      expect(mockRequestAccess).not.toHaveBeenCalled();
    });

    it("rejects a non-positive or out-of-range --volume before calling the API", async () => {
      const { sendingAccessRequest } = await import("../commands/sending-access.js");
      await expect(
        sendingAccessRequest({ useCase: "x", recipients: "y", volume: "0" }),
      ).rejects.toThrow("process.exit");
      expect(exit).toHaveBeenCalledWith(2);

      await expect(
        sendingAccessRequest({ useCase: "x", recipients: "y", volume: "not-a-number" }),
      ).rejects.toThrow("process.exit");

      await expect(
        sendingAccessRequest({ useCase: "x", recipients: "y", volume: "1000001" }),
      ).rejects.toThrow("process.exit");
      expect(mockRequestAccess).not.toHaveBeenCalled();
    });

    it("files the request with the parsed volume and prints id + state", async () => {
      mockRequestAccess.mockResolvedValue({
        id: "sar_2",
        state: "pending",
        useCase: "billing reminders",
        recipients: "our customers",
        expectedDailyVolume: 500,
        createdAt: new Date("2026-01-01T00:00:00Z"),
      });
      const { sendingAccessRequest } = await import("../commands/sending-access.js");
      await sendingAccessRequest({
        useCase: "billing reminders",
        recipients: "our customers",
        volume: "500",
      });

      expect(mockRequestAccess).toHaveBeenCalledWith({
        useCase: "billing reminders",
        recipients: "our customers",
        expectedDailyVolume: 500,
      });
      expect(stdout).toHaveBeenCalledWith("sar_2\tpending\n");
      expect(stderr.mock.calls.map((c: unknown[]) => String(c[0])).join("")).toContain("e2a sending-access status");
    });

    it("--json prints the raw filed/replayed request", async () => {
      const result = {
        id: "sar_3",
        state: "pending",
        useCase: "x",
        recipients: "y",
        expectedDailyVolume: 5,
        createdAt: new Date("2026-01-01T00:00:00Z"),
      };
      mockRequestAccess.mockResolvedValue(result);
      const { sendingAccessRequest } = await import("../commands/sending-access.js");
      await sendingAccessRequest({ useCase: "x", recipients: "y", volume: "5", json: true });
      expect(stdout).toHaveBeenCalledWith(JSON.stringify(result) + "\n");
    });
  });
});
