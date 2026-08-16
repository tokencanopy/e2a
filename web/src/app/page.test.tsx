import { render, screen, fireEvent, within } from "@testing-library/react";
import Home from "./page";

jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...props
  }: {
    href: string;
    children: React.ReactNode;
    [key: string]: unknown;
  }) {
    return (
      <a href={href} {...props}>
        {children}
      </a>
    );
  };
});

const mockSignOut = jest.fn();
let mockAuthValue = {
  user: null as
    | { id: string; email: string; name: string; created_at: string }
    | null,
  loading: false,
  signOut: mockSignOut,
};

jest.mock("./components/AuthProvider", () => ({
  useAuth: () => mockAuthValue,
}));

beforeEach(() => {
  mockAuthValue = { user: null, loading: false, signOut: mockSignOut };
  mockSignOut.mockReset();
});

describe("Landing page", () => {
  it("renders hero heading", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", {
        level: 1,
        name: /Build the best email agents with e2a/i,
      }),
    ).toBeInTheDocument();
    // The hero leads with the product promise and makes the first use cases
    // explicit rather than asking a visitor to infer what e2a is for.
    expect(
      screen.getByText(/e2a gives every agent a real, authenticated inbox/i),
    ).toBeInTheDocument();
  });

  it("renders the e2a wordmark", () => {
    render(<Home />);
    expect(screen.getAllByText("e2a").length).toBeGreaterThan(0);
  });

  // The nav groups into Quick start / Product / Resources — a flat row of
  // every link wrapped onto two lines once the social icons joined it.
  it("shows resources dropdown items on hover", () => {
    render(<Home />);
    const resources = screen.getByText(/^Resources/).closest("div")!;
    fireEvent.mouseEnter(resources);
    expect(screen.getByText("API Reference")).toBeInTheDocument();
    expect(screen.getAllByText("Python SDK").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("TypeScript SDK").length).toBeGreaterThanOrEqual(
      2,
    );
    expect(screen.getAllByText("CLI").length).toBeGreaterThanOrEqual(2);
  });

  it("hides resources dropdown on mouse leave", () => {
    render(<Home />);
    const resources = screen.getByText(/^Resources/).closest("div")!;
    fireEvent.mouseEnter(resources);
    expect(screen.getByText("API Reference")).toBeInTheDocument();
    fireEvent.mouseLeave(resources);
    expect(screen.queryByText("API Reference")).not.toBeInTheDocument();
  });

  // Everything in Resources leaves the landing page, so all of it opens in a
  // new tab — including the internal routes, which would otherwise navigate
  // away and lose the visitor's place.
  it("opens every Resources link in a new tab", () => {
    render(<Home />);
    const resources = screen.getByText(/^Resources/).closest("div")!;
    fireEvent.mouseEnter(resources);
    // Scoped to the dropdown: the footer carries its own Blog/CLI links, and
    // an unscoped query picks those up instead (they render later in the DOM).
    for (const name of ["API Reference", "Blog", "Plugin", "CLI"]) {
      const link = within(resources as HTMLElement).getByRole("link", { name });
      expect(link).toHaveAttribute("target", "_blank");
      expect(link).toHaveAttribute("rel", expect.stringContaining("noopener"));
    }
  });

  // Regression: the menus opened on hover only, so a keyboard user could
  // reach the button but never its contents. That was a reachability
  // REGRESSION, not just a pre-existing gap — Human-in-the-loop, Use cases
  // and Blog used to be top-level links and are now inside these menus.
  it("opens a nav menu from the keyboard and closes it with Escape", async () => {
    render(<Home />);
    const button = screen.getByRole("button", { name: /^Product/ });

    // Nothing is open to start with.
    expect(screen.queryByRole("link", { name: "Use cases" })).not.toBeInTheDocument();

    // Tabbing to the button reveals the menu...
    fireEvent.focus(button);
    expect(screen.getByRole("link", { name: "Use cases" })).toBeInTheDocument();

    // ...and Escape dismisses it without touching a mouse.
    fireEvent.keyDown(button, { key: "Escape" });
    expect(screen.queryByRole("link", { name: "Use cases" })).not.toBeInTheDocument();
  });

  it("toggles a nav menu on click, for touch and pointer users", () => {
    render(<Home />);
    const button = screen.getByRole("button", { name: /^Resources/ });
    fireEvent.click(button);
    expect(screen.getByText("API Reference")).toBeInTheDocument();
    fireEvent.click(button);
    expect(screen.queryByText("API Reference")).not.toBeInTheDocument();
  });

  it("groups the on-page sections under Product", () => {
    render(<Home />);
    const product = screen.getByText(/^Product/).closest("div")!;
    fireEvent.mouseEnter(product);
    expect(
      screen.getByRole("link", { name: "Human-in-the-loop" }),
    ).toHaveAttribute("href", "#hitl");
    expect(screen.getByRole("link", { name: "Use cases" })).toHaveAttribute(
      "href",
      "/use-cases",
    );
  });

  it("links Token Canopy's channels from the header", () => {
    render(<Home />);
    for (const [name, href] of [
      ["Token Canopy on X", "https://x.com/TokenCanopy"],
      ["Token Canopy on LinkedIn", "https://www.linkedin.com/company/tokencanopy/"],
      ["Token Canopy on Discord", "https://discord.gg/EQTK2REXPb"],
    ] as const) {
      expect(screen.getByRole("link", { name })).toHaveAttribute("href", href);
    }
  });

  // Onboarding is agent-native: the first thing the page asks you to do is
  // install the plugin into a coding agent, not install an SDK. The SDK/CLI
  // exist, but as the "wiring e2a into your own service" escape hatch.
  it("leads onboarding with the plugin, not the SDK", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", {
        name: /Install the plugin\. Your agent has an inbox\./i,
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/claude plugin install e2a@e2a/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Any MCP client" }),
    ).toBeInTheDocument();
  });

  // The SDK path exists, but as the step AFTER onboarding — for people
  // already running an agent framework, not as the way in.
  it("offers the SDK path for existing agent frameworks, after the quick start", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", { name: /Already have an agent framework\?/i }),
    ).toBeInTheDocument();
    expect(screen.getByText(/LangChain, Google ADK/)).toBeInTheDocument();
  });

  it("shows the use cases section", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", { name: /What you can build/i }),
    ).toBeInTheDocument();
    expect(screen.getByText("Support and intake")).toBeInTheDocument();
    expect(screen.getAllByText("Procurement").length).toBeGreaterThan(0);
  });

  it("shows the human-in-the-loop section", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", { name: /Approve.*before.*hits send/i }),
    ).toBeInTheDocument();
  });

  it("shows CTA section with Get started link", () => {
    render(<Home />);
    const getStartedLinks = screen.getAllByText(/Get started free/i);
    expect(getStartedLinks.length).toBeGreaterThan(0);
  });

  // Regression: the CTA button rows were `inline-flex`, so in the hero the
  // "What is email infrastructure…" explainer link (itself inline) flowed
  // onto the SAME line and sat jammed against "Get started free" instead of
  // stacking above it. jsdom doesn't compute layout, so the guard is
  // structural: every CTA row must be a block-level flex container.
  it("renders the CTA button rows as block-level flex containers", () => {
    render(<Home />);
    const rows = screen
      .getAllByRole("link", { name: /Get started free/ })
      .map((link) => link.parentElement!);
    expect(rows).toHaveLength(2); // hero + closing CTA
    for (const row of rows) {
      const classes = row.className.split(/\s+/);
      expect(classes).toContain("flex");
      expect(classes).not.toContain("inline-flex");
    }
    // The hero explainer link the row collided with is still present above.
    expect(
      screen.getByRole("link", {
        name: /What is email infrastructure for AI agents\?/,
      }),
    ).toBeInTheDocument();
  });
});

// The landing page is the site's main answer surface. These assertions guard
// the two properties that make it one: the FAQ answers are actually in the
// HTML (not only in the markup), and the markup parses as a valid FAQPage.
describe("FAQ and structured data", () => {
  function ldNodes(container: HTMLElement): Record<string, unknown>[] {
    return Array.from(
      container.querySelectorAll('script[type="application/ld+json"]'),
    ).map((s) => JSON.parse(s.innerHTML.replace(/\\u003c/g, "<")));
  }

  it("renders the FAQ answers as visible prose", () => {
    render(<Home />);
    expect(
      screen.getByRole("heading", { name: /Questions people ask/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", {
        name: "Can an AI agent have its own email address?",
      }),
    ).toBeInTheDocument();
    // Google only honors FAQ markup whose answers are on the page, and an
    // answer engine quotes the prose as readily as the JSON-LD.
    expect(
      screen.getByText(/The inbox belongs to the agent rather than to a human/),
    ).toBeInTheDocument();
  });

  it("emits a valid FAQPage whose answers match the visible ones", () => {
    const { container } = render(<Home />);
    const faq = ldNodes(container).find((n) => n["@type"] === "FAQPage");
    expect(faq).toBeDefined();

    const questions = faq!.mainEntity as {
      "@type": string;
      name: string;
      acceptedAnswer: { "@type": string; text: string };
    }[];
    expect(questions.length).toBeGreaterThanOrEqual(6);
    for (const q of questions) {
      expect(q["@type"]).toBe("Question");
      expect(q.acceptedAnswer["@type"]).toBe("Answer");
      // Self-contained means the answer names the product: a quoted fragment
      // has to still attribute, with none of this page attached to it.
      expect(q.acceptedAnswer.text).toMatch(/e2a/);
      // Same strings in both copies, so a quote can't diverge from the page.
      expect(screen.getByText(q.acceptedAnswer.text)).toBeInTheDocument();
      expect(
        screen.getByRole("heading", { name: q.name }),
      ).toBeInTheDocument();
    }
  });

  it("links the FAQ from the Product menu", () => {
    render(<Home />);
    const product = screen.getByText(/^Product/).closest("div")!;
    fireEvent.mouseEnter(product);
    expect(screen.getByRole("link", { name: "FAQ" })).toHaveAttribute(
      "href",
      "#faq",
    );
  });
});

describe("Navigation auth state", () => {
  it("shows Sign in link when not authenticated", () => {
    render(<Home />);
    expect(screen.getByText("Sign in")).toBeInTheDocument();
    expect(screen.queryByText("Go to Dashboard")).not.toBeInTheDocument();
  });

  it("shows loading indicator while checking auth", () => {
    mockAuthValue = { user: null, loading: true, signOut: mockSignOut };
    render(<Home />);
    expect(screen.getByText("...")).toBeInTheDocument();
    expect(screen.queryByText("Sign in")).not.toBeInTheDocument();
  });

  it("shows 'Go to Dashboard' link when authenticated", () => {
    mockAuthValue = {
      user: {
        id: "u1",
        email: "dev@example.com",
        name: "Dev",
        created_at: "2026-01-01T00:00:00Z",
      },
      loading: false,
      signOut: mockSignOut,
    };
    render(<Home />);
    expect(screen.getByText("Go to Dashboard")).toBeInTheDocument();
    expect(screen.queryByText("Sign in")).not.toBeInTheDocument();
  });

  it("links the GitHub repo from the header", () => {
    render(<Home />);
    const githubLink = screen.getByRole("link", {
      name: /view source on github/i,
    });
    expect(githubLink).toHaveAttribute(
      "href",
      "https://github.com/tokencanopy/e2a",
    );
  });
});

describe("Footer", () => {
  it("renders footer links", () => {
    render(<Home />);
    expect(screen.getAllByText("API Docs").length).toBeGreaterThan(0);
    // Links the plugin directory, not one SKILL.md inside it — the plugin is
    // what onboarding installs, and it serves Claude Code, Codex and Cursor.
    expect(screen.getAllByText("Plugin").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Feedback").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Apache 2.0").length).toBeGreaterThan(0);
  });

  it("links the GitHub repo from the footer", () => {
    render(<Home />);
    const githubLink = screen
      .getAllByRole("link")
      .find(
        (l) =>
          l.textContent === "GitHub" &&
          l.getAttribute("href") === "https://github.com/tokencanopy/e2a",
      );
    expect(githubLink).toBeInTheDocument();
  });

  it("renders SDK and CLI footer links to package registries", () => {
    render(<Home />);
    const footerLinks = screen.getAllByRole("link");
    const pypiLink = footerLinks.find(
      (l) =>
        l.textContent === "Python SDK" &&
        l.getAttribute("href")?.includes("pypi.org"),
    );
    const tsLink = footerLinks.find(
      (l) =>
        l.textContent === "TypeScript SDK" &&
        l.getAttribute("href")?.includes("npmjs.com"),
    );
    const cliLink = footerLinks.find(
      (l) =>
        l.textContent === "CLI" &&
        l.getAttribute("href")?.includes("npmjs.com"),
    );
    expect(pypiLink).toBeInTheDocument();
    expect(tsLink).toBeInTheDocument();
    expect(cliLink).toBeInTheDocument();
  });
});
