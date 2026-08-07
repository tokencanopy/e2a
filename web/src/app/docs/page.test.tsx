import { render, screen } from "@testing-library/react";
import DocsPage from "./page";

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

function ldNodes(container: HTMLElement): Record<string, unknown>[] {
  return Array.from(
    container.querySelectorAll('script[type="application/ld+json"]'),
  ).map((s) => JSON.parse(s.innerHTML.replace(/\\u003c/g, "<")));
}

describe("Docs page", () => {
  // This route used to be a JS redirect whose entire payload to a crawler was
  // "Loading API docs...". The point of the page is now that it answers
  // something without running any JavaScript.
  it("renders statically, with no redirect", () => {
    render(<DocsPage />);
    expect(screen.queryByText(/Loading API docs/i)).not.toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: /e2a developer docs/i }),
    ).toBeInTheDocument();
  });

  it("routes onward to the full API reference", () => {
    render(<DocsPage />);
    // Nobody arriving here for the endpoint reference should be stranded.
    const toReference = screen
      .getAllByRole("link")
      .filter((l) => l.getAttribute("href") === "/api-docs");
    expect(toReference.length).toBeGreaterThan(0);
  });

  it("lists the machine-readable documents an agent should fetch", () => {
    render(<DocsPage />);
    for (const [name, href] of [
      ["openapi.yaml", "https://e2a.dev/v1/openapi.yaml"],
      ["setup.md", "https://e2a.dev/setup.md"],
      ["auth.md", "https://e2a.dev/auth.md"],
      ["sdk.md", "https://e2a.dev/sdk.md"],
      ["llms.txt", "https://e2a.dev/llms.txt"],
    ] as const) {
      expect(screen.getByRole("link", { name })).toHaveAttribute("href", href);
    }
  });

  it("emits a valid FAQPage whose answers match the visible ones", () => {
    const { container } = render(<DocsPage />);
    const faq = ldNodes(container).find((n) => n["@type"] === "FAQPage");
    expect(faq).toBeDefined();

    const questions = faq!.mainEntity as {
      "@type": string;
      name: string;
      acceptedAnswer: { "@type": string; text: string };
    }[];
    expect(questions.length).toBeGreaterThanOrEqual(4);
    for (const q of questions) {
      expect(q["@type"]).toBe("Question");
      expect(q.acceptedAnswer["@type"]).toBe("Answer");
      // A quoted answer has to attribute on its own, with no page around it.
      expect(q.acceptedAnswer.text).toMatch(/e2a/);
      expect(screen.getByText(q.acceptedAnswer.text)).toBeInTheDocument();
      expect(screen.getByRole("heading", { name: q.name })).toBeInTheDocument();
    }
  });

  it("emits breadcrumbs rooted at the site home", () => {
    const { container } = render(<DocsPage />);
    const crumbs = ldNodes(container).find(
      (n) => n["@type"] === "BreadcrumbList",
    );
    expect(crumbs).toBeDefined();
    const items = crumbs!.itemListElement as { position: number; name: string }[];
    expect(items.map((i) => i.position)).toEqual([1, 2]);
    expect(items.map((i) => i.name)).toEqual(["e2a", "Docs"]);
  });

  // /mcp already owns the "how do I connect an MCP client" questions. Repeating
  // them here would split the signal across two pages instead of ranking one.
  it("does not duplicate the MCP page's questions", () => {
    const { container } = render(<DocsPage />);
    const faq = ldNodes(container).find((n) => n["@type"] === "FAQPage");
    const names = (faq!.mainEntity as { name: string }[]).map((q) => q.name);
    expect(names).not.toContain("How do I give my AI agent an email address?");
    expect(names).not.toContain("Which MCP clients does e2a work with?");
  });
});
