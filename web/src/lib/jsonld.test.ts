import {
  ORGANIZATION_ID,
  SOFTWARE_ID,
  blogPosting,
  breadcrumbs,
  faqPage,
  howTo,
  organization,
  softwareApplication,
  website,
} from "./jsonld";

describe("structured data", () => {
  it("states publisher identity once and references it by @id", () => {
    // Duplicating the Organization node per page is how publisher metadata
    // drifts. Everything else should point at the one @id.
    expect(organization()["@id"]).toBe(ORGANIZATION_ID);
    expect(website().publisher).toEqual({ "@id": ORGANIZATION_ID });
    expect(softwareApplication("desc").publisher).toEqual({
      "@id": ORGANIZATION_ID,
    });
  });

  it("gives the product a stable @id the pricing page can attach Offers to", () => {
    // The pricing page ships from a different repo and hangs its paid-tier
    // Offer nodes off this @id. Drop it and that reference dangles, leaving
    // two competing SoftwareApplication nodes for one product.
    expect(softwareApplication("desc")["@id"]).toBe(SOFTWARE_ID);
    expect(SOFTWARE_ID).toMatch(/#software$/);
    // Resolved from SITE_URL, never a hardcoded host — a self-host overrides it.
    expect(SOFTWARE_ID.startsWith(String(softwareApplication("desc").url))).toBe(
      true,
    );
  });

  it("keeps dollar amounts off the software node", () => {
    // Only the free tier is stated here; paid amounts live on the pricing
    // page so structured data cannot drift away from billing.
    expect(softwareApplication("desc").offers).toEqual({
      "@type": "Offer",
      price: "0",
      priceCurrency: "USD",
    });
  });

  it("builds a BlogPosting with absolute URLs and an ISO publish date", () => {
    const node = blogPosting({
      title: "Send real email from a Python AI agent",
      description: "A minimal walkthrough.",
      date: "2026-04-13",
      author: "e2a",
      slug: "send-email-from-python-agent",
    });

    expect(node["@type"]).toBe("BlogPosting");
    expect(node.datePublished).toBe("2026-04-13T00:00:00.000Z");
    expect(String(node.url)).toMatch(/\/blog\/send-email-from-python-agent$/);
    expect(String(node.url)).toMatch(/^https?:\/\//);
    expect(node.mainEntityOfPage).toEqual({
      "@type": "WebPage",
      "@id": node.url,
    });
  });

  it("maps FAQ entries to Question/acceptedAnswer pairs in order", () => {
    const node = faqPage([
      { question: "Do I need an API key?", answer: "No — OAuth 2.1." },
      { question: "Is it open source?", answer: "Yes, Apache-2.0." },
    ]);

    expect(node["@type"]).toBe("FAQPage");
    expect(node.mainEntity).toEqual([
      {
        "@type": "Question",
        name: "Do I need an API key?",
        acceptedAnswer: { "@type": "Answer", text: "No — OAuth 2.1." },
      },
      {
        "@type": "Question",
        name: "Is it open source?",
        acceptedAnswer: { "@type": "Answer", text: "Yes, Apache-2.0." },
      },
    ]);
  });

  it("numbers HowTo steps and breadcrumbs from one", () => {
    const steps = howTo({
      name: "Connect",
      description: "Connect an MCP client.",
      steps: [
        { name: "Add", text: "Add the server." },
        { name: "Authorize", text: "Authorize in the browser." },
      ],
    }).step as Array<{ position: number }>;
    expect(steps.map((s) => s.position)).toEqual([1, 2]);

    const crumbs = breadcrumbs([
      { name: "e2a", path: "/" },
      { name: "MCP", path: "/mcp" },
    ]).itemListElement as Array<{ position: number; item: string }>;
    expect(crumbs.map((c) => c.position)).toEqual([1, 2]);
    expect(crumbs[1].item).toMatch(/\/mcp$/);
  });
});
