import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { SuccessPanel } from "./SuccessPanel";
import type { AgentData } from "../../../components/types";

const agent: AgentData = { domain: "agents.e2a.dev", email: "my-agent@agents.e2a.dev" };

const mockFetch = jest.fn();
global.fetch = mockFetch;

// jsdom has no clipboard API; the CodeBlock copy button writes to it.
const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });

beforeEach(() => {
  mockFetch.mockReset();
  writeText.mockClear();
  // The panel console.errors on send failure — keep the test output clean.
  jest.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  (console.error as jest.Mock).mockRestore?.();
});

describe("SuccessPanel", () => {
  it("announces the created inbox with its email address", () => {
    render(<SuccessPanel agent={agent} />);
    expect(screen.getByText("Inbox created!")).toBeInTheDocument();
    expect(screen.getByText("my-agent@agents.e2a.dev")).toBeInTheDocument();
  });

  it("sends a test email to the new inbox and shows the delivered state", async () => {
    mockFetch.mockResolvedValue({ ok: true });
    render(<SuccessPanel agent={agent} />);

    await userEvent.click(
      screen.getByRole("button", { name: /Send a test email to my-agent@agents\.e2a\.dev/ }),
    );

    expect(
      await screen.findByText(/Test email is on its way to my-agent@agents\.e2a\.dev/),
    ).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledWith(
      "/v1/agents/my-agent%40agents.e2a.dev/test",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    expect(screen.getByText(/Connect your agent below to receive it/)).toBeInTheDocument();
  });

  it("surfaces the server error and returns to the idle state on failure", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 502,
      text: () => Promise.resolve("SMTP upstream unavailable"),
    });
    render(<SuccessPanel agent={agent} />);

    await userEvent.click(screen.getByRole("button", { name: /Send a test email/ }));

    expect(await screen.findByText("SMTP upstream unavailable")).toBeInTheDocument();
    // The send button is offered again after a failure.
    expect(screen.getByRole("button", { name: /Send a test email/ })).toBeInTheDocument();
  });

  it("shows a network-error message when the request rejects", async () => {
    mockFetch.mockRejectedValue(new Error("offline"));
    render(<SuccessPanel agent={agent} />);

    await userEvent.click(screen.getByRole("button", { name: /Send a test email/ }));

    expect(
      await screen.findByText("Network error — check your connection and try again."),
    ).toBeInTheDocument();
  });

  it("copies the skill-install commands to the clipboard", async () => {
    render(<SuccessPanel agent={agent} />);
    await userEvent.click(screen.getByRole("button", { name: "Copy" }));

    expect(writeText).toHaveBeenCalledWith(
      expect.stringContaining("mkdir -p .claude/skills/e2a"),
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Copied!" })).toBeInTheDocument(),
    );
  });

  it("links out to the inboxes list", () => {
    render(<SuccessPanel agent={agent} />);
    expect(screen.getByRole("link", { name: "Go to Inboxes" })).toHaveAttribute("href", "/inboxes");
  });
});
