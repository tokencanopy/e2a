import { render, screen, waitFor, within, fireEvent } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { ProtectionEditor } from "./ProtectionEditor";
import { setProtection } from "../../../components/onboarding/api";
import type { ProtectionConfig } from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  setProtection: jest.fn(),
}));
const mockSetProtection = setProtection as jest.MockedFunction<typeof setProtection>;

const EMAIL = "agent@example.com";

const baseConfig: ProtectionConfig = {
  inbound: {
    gate: { policy: "allowlist", allowlist: ["alice@acme.com", "bob@acme.com"], action: "review" },
    scan: { sensitivity: "medium" },
  },
  outbound: {
    gate: { policy: "open", action: "flag" },
    scan: { sensitivity: "off" },
  },
  holds: { ttl_seconds: 3600, on_expiry: "approve" },
};

function renderEditor(config: ProtectionConfig = baseConfig, onSaved = jest.fn()) {
  const utils = render(<ProtectionEditor email={EMAIL} config={config} onSaved={onSaved} />);
  return { ...utils, onSaved };
}

function gateGroup(title: string) {
  return within(screen.getByRole("group", { name: `${title} trust gate` }));
}

beforeEach(() => {
  mockSetProtection.mockReset();
});

describe("ProtectionEditor — initialization from config", () => {
  it("renders the header title, beta chip, and subtitle", () => {
    renderEditor();
    expect(screen.getByText("Protection")).toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Control who may send to and from this inbox, how aggressively content is scanned, and what happens to messages held for review.",
      ),
    ).toBeInTheDocument();
  });

  it("selects the configured policy, action, and sensitivity per direction", () => {
    renderEditor();
    expect(gateGroup("Inbound").getByRole("button", { name: "Addresses" }))
      .toHaveAttribute("aria-pressed", "true");
    expect(within(screen.getByRole("group", { name: "Inbound non-match action" }))
      .getByRole("button", { name: "Hold for review" }))
      .toHaveAttribute("aria-pressed", "true");
    expect(within(screen.getByRole("group", { name: "Inbound scan sensitivity" }))
      .getByRole("button", { name: "Medium" }))
      .toHaveAttribute("aria-pressed", "true");
    expect(gateGroup("Outbound").getByRole("button", { name: "Open (anyone)" }))
      .toHaveAttribute("aria-pressed", "true");
  });

  it("prefills the allowlist textarea, one entry per line", () => {
    renderEditor();
    expect(screen.getByLabelText("Inbound allowlist")).toHaveValue(
      "alice@acme.com\nbob@acme.com",
    );
  });

  it("hides the allowlist textarea for an open policy", () => {
    renderEditor();
    expect(screen.queryByLabelText("Outbound allowlist")).not.toBeInTheDocument();
  });

  it("initializes the holds section from config", () => {
    renderEditor();
    expect(screen.getByRole("button", { name: "1 hour" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByLabelText("Approval window in seconds")).toHaveValue(3600);
    expect(screen.getByRole("radio", { name: /Auto-approve/ })).toBeChecked();
  });

  it("defaults holds to 7 days / auto-reject when the config omits them", () => {
    renderEditor({ ...baseConfig, holds: {} });
    expect(screen.getByRole("button", { name: "7 days" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("radio", { name: /Auto-reject/ })).toBeChecked();
  });
});

describe("ProtectionEditor — editing", () => {
  it("reveals the allowlist textarea with a domain-specific label for the domain policy", async () => {
    renderEditor();
    await userEvent.click(gateGroup("Outbound").getByRole("button", { name: "Domains" }));
    expect(screen.getByLabelText("Outbound allowlist")).toBeInTheDocument();
    expect(screen.getByText("Allowed domains (one per line)")).toBeInTheDocument();
  });

  it("marks the custom TTL chip active when the value matches no preset", async () => {
    renderEditor();
    const input = screen.getByLabelText("Approval window in seconds");
    await userEvent.clear(input);
    await userEvent.type(input, "7200");
    // No preset button is pressed for 7200 seconds.
    expect(screen.getByRole("button", { name: "1 hour" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "1 day" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "7 days" })).toHaveAttribute("aria-pressed", "false");
  });
});

describe("ProtectionEditor — validation", () => {
  it.each([
    ["0", "zero"],
    ["604801", "above the 7-day maximum"],
  ])("rejects a TTL of %s seconds (%s) without calling the API", async (value) => {
    const { container } = renderEditor();
    const input = screen.getByLabelText("Approval window in seconds");
    await userEvent.clear(input);
    await userEvent.type(input, value);
    // fireEvent.submit bypasses the native min/max interactive validation
    // (which jsdom enforces on button click) to exercise the component's
    // own handleSave guard directly.
    fireEvent.submit(container.querySelector("form")!);

    expect(
      await screen.findByText("Approval window must be between 1 and 604800 seconds (7 days)."),
    ).toBeInTheDocument();
    expect(mockSetProtection).not.toHaveBeenCalled();
  });
});

describe("ProtectionEditor — save", () => {
  it("disables the Save button when there are no edits", () => {
    renderEditor();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("does not call the API if submitted when there are no edits", () => {
    const { container } = renderEditor();
    fireEvent.submit(container.querySelector("form")!);
    expect(mockSetProtection).not.toHaveBeenCalled();
  });

  it("enables the Save button when an edit is made, and disables it if reverted", async () => {
    renderEditor();
    const saveButton = screen.getByRole("button", { name: "Save" });
    expect(saveButton).toBeDisabled();

    // Make an edit (TTL preset)
    await userEvent.click(screen.getByRole("button", { name: "1 day" }));
    expect(saveButton).toBeEnabled();

    // Revert back to original preset (1 hour)
    await userEvent.click(screen.getByRole("button", { name: "1 hour" }));
    expect(saveButton).toBeDisabled();
  });

  it("PUTs the wholesale replace with the edited drafts", async () => {
    mockSetProtection.mockResolvedValue(undefined);
    const { onSaved } = renderEditor();

    await userEvent.click(
      within(screen.getByRole("group", { name: "Inbound non-match action" }))
        .getByRole("button", { name: "Block" }),
    );
    await userEvent.click(screen.getByRole("button", { name: "1 day" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockSetProtection).toHaveBeenCalledTimes(1));
    expect(mockSetProtection).toHaveBeenCalledWith(EMAIL, {
      inbound: {
        gate: { policy: "allowlist", allowlist: ["alice@acme.com", "bob@acme.com"], action: "block" },
        scan: { sensitivity: "medium" },
      },
      outbound: {
        // An open policy always sends an empty allowlist, even though the
        // field is not editable in that state.
        gate: { policy: "open", allowlist: [], action: "flag" },
        scan: { sensitivity: "off" },
      },
      holds: { ttl_seconds: 86400, on_expiry: "approve" },
    });
    expect(await screen.findByText("Saved ✓")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("trims allowlist entries and drops blank lines on save", async () => {
    mockSetProtection.mockResolvedValue(undefined);
    renderEditor();

    const textarea = screen.getByLabelText("Inbound allowlist");
    await userEvent.clear(textarea);
    await userEvent.type(textarea, "  alice@acme.com  {enter}{enter}bob@acme.com{enter}   ");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockSetProtection).toHaveBeenCalledTimes(1));
    const [, payload] = mockSetProtection.mock.calls[0];
    expect(payload.inbound.gate.allowlist).toEqual(["alice@acme.com", "bob@acme.com"]);
  });

  it("clears the Saved ✓ indicator when the user edits after a save", async () => {
    mockSetProtection.mockResolvedValue(undefined);
    renderEditor();

    await userEvent.click(screen.getByRole("button", { name: "1 day" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved ✓")).toBeInTheDocument();

    await userEvent.click(
      within(screen.getByRole("group", { name: "Inbound scan sensitivity" }))
        .getByRole("button", { name: "High" }),
    );
    expect(screen.queryByText("Saved ✓")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("shows the server error when the PUT fails", async () => {
    mockSetProtection.mockRejectedValue(new Error("ttl_seconds out of range"));
    renderEditor();

    await userEvent.click(screen.getByRole("button", { name: "1 day" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("ttl_seconds out of range")).toBeInTheDocument();
    expect(screen.queryByText("Saved ✓")).not.toBeInTheDocument();
  });
});
