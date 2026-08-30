import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AgentPromptCard, AGENT_PROMPTS } from "./AgentPromptCard";

// jsdom doesn't provide navigator.clipboard. The Copy-prompt button calls
// writeText, so we install a jest mock once at module level (same pattern
// as the settings page tests).
const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });
beforeEach(() => writeText.mockClear());

test("renders the blurb and prompt verbatim", () => {
  render(<AgentPromptCard {...AGENT_PROMPTS.templates} />);
  expect(
    screen.getByRole("heading", { name: "Set up with a coding agent" }),
  ).toBeInTheDocument();
  expect(screen.getByText(AGENT_PROMPTS.templates.blurb)).toBeInTheDocument();
  expect(screen.getByText(AGENT_PROMPTS.templates.prompt)).toBeInTheDocument();
});

test("uses concise page-specific MCP prompts", () => {
  expect(AGENT_PROMPTS.inboxes.prompt).toBe(
    "Help me set up an e2a inbox using https://api.e2a.dev/mcp",
  );
  expect(AGENT_PROMPTS.domains.prompt).toBe(
    "Help me connect a custom domain to e2a using https://api.e2a.dev/mcp",
  );
  expect(AGENT_PROMPTS.templates.prompt).toBe(
    "Help me set up e2a email templates using https://api.e2a.dev/mcp",
  );
  expect(AGENT_PROMPTS.contacts.blurb).toMatch(/while it is running/i);
  expect(AGENT_PROMPTS.contacts.blurb).not.toMatch(/headlessly/i);
});

test("shows the optional outbound-review instruction for inbox setup", () => {
  render(<AgentPromptCard {...AGENT_PROMPTS.inboxes} />);

  const notice = screen.getByRole("note", {
    name: "Want every outbound email reviewed first?",
  });
  expect(notice).toHaveTextContent(
    "Configure this inbox so every outbound email requires human review.",
  );
});

// DNS records land at the registrar, not in e2a — an agent driving domain
// setup over MCP stalls there unless it also holds a provider MCP. The
// recommendation belongs next to the prompt the user is about to paste.
test("recommends the Cloudflare MCP on the domain setup card", () => {
  render(<AgentPromptCard {...AGENT_PROMPTS.domains} />);

  const notice = screen.getByRole("note", {
    name: "Domain managed by Cloudflare?",
  });
  expect(notice).toHaveTextContent("create the DNS records for you");
  expect(
    screen.getByRole("link", { name: "https://github.com/cloudflare/mcp" }),
  ).toHaveAttribute("href", "https://github.com/cloudflare/mcp");
});

test("shows no notice on setup cards that do not define one", () => {
  render(<AgentPromptCard {...AGENT_PROMPTS.templates} />);
  expect(screen.queryByRole("note")).not.toBeInTheDocument();
});

test("copy button writes the card's prompt to the clipboard and flips its label", async () => {
  render(<AgentPromptCard {...AGENT_PROMPTS.domains} />);
  fireEvent.click(screen.getByRole("button", { name: "Copy prompt" }));
  await waitFor(() =>
    expect(writeText).toHaveBeenCalledWith(AGENT_PROMPTS.domains.prompt),
  );
  await waitFor(() =>
    expect(
      screen.getByRole("button", { name: "Copy prompt" }),
    ).toHaveTextContent("Copied"),
  );
});
