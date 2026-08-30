import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import GetStartedPage from "./page";

// ── Mocks ────────────────────────────────────────────────

const mockUseSearchParams = jest.fn();

// Track navigations so individual tests can assert on them. The push /
// replace handlers also reflect the new URL into the search-params
// mock so subsequent re-renders see the right ?step, matching real
// router behavior — without this, clicking "Shared" would push to a
// mock spy and the next render would still see the empty search
// params (step=choose).
const navigationHistory: Array<{ type: "push" | "replace" | "back"; url?: string }> = [];

function reflectUrlIntoSearchParams(url: string) {
  const q = url.includes("?") ? url.slice(url.indexOf("?") + 1) : "";
  const usp = new URLSearchParams(q);
  const params: Record<string, string> = {};
  usp.forEach((v, k) => {
    params[k] = v;
  });
  mockUseSearchParams.mockReturnValue({
    get: (key: string) => params[key] ?? null,
  });
}

const mockRouterPush = jest.fn((url: string) => {
  navigationHistory.push({ type: "push", url });
  reflectUrlIntoSearchParams(url);
});
const mockRouterReplace = jest.fn((url: string) => {
  navigationHistory.push({ type: "replace", url });
  reflectUrlIntoSearchParams(url);
});
const mockRouterBack = jest.fn(() => {
  navigationHistory.push({ type: "back" });
});

jest.mock("next/navigation", () => ({
  useSearchParams: () => mockUseSearchParams(),
  useRouter: () => ({
    push: mockRouterPush,
    replace: mockRouterReplace,
    back: mockRouterBack,
  }),
}));

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...props }: { href: string; children: React.ReactNode; [key: string]: unknown }) {
    return <a href={href} {...props}>{children}</a>;
  };
});

const mockFetch = jest.fn();
global.fetch = mockFetch;

Object.assign(navigator, {
  clipboard: { writeText: jest.fn() },
});

function setSearchParams(params: Record<string, string | undefined> = {}) {
  mockUseSearchParams.mockReturnValue({
    get: (key: string) => params[key] ?? null,
  });
}

// Fresh users land on the top-level method choice (SetupMethodChoice:
// "With an agent" / "Set up in the web UI"). The shared/custom address
// chooser now lives one level deeper, behind "Set up in the web UI".
// Tests that exercise the address chooser navigate through it first.
async function chooseWebUI() {
  await waitFor(() => expect(screen.getByText("With an agent")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Set up in the web UI"));
  await waitFor(() => expect(screen.getByText("Shared e2a domain")).toBeInTheDocument());
}


const verifiedDomain = {
  domain: "verified.example.com",
  verified: true,
  verification_token: "e2a-verify=abc123",
  dns_records: [
    {
      type: "TXT",
      name: "verified.example.com",
      value: "e2a-verify=abc123",
      priority: null,
      purpose: "ownership",
      status: "verified",
    },
    {
      type: "MX",
      name: "verified.example.com",
      value: "mx.e2a.dev",
      priority: 10,
      purpose: "inbound_mx",
      status: "verified",
    },
    {
      type: "MX",
      name: "*.verified.example.com",
      value: "mx.e2a.dev",
      priority: 10,
      purpose: "inbound_mx_wildcard",
      status: "verified",
    },
  ],
  created_at: "2026-01-01T00:00:00Z",
  verified_at: "2026-01-15T00:00:00Z",
};

/** Mock both listDomains and listAgents returning empty (fresh user). */
function mockFreshUser() {
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/domains") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [] }) });
    }
    if (url === "/api/inboxes") {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
  });
}

beforeEach(() => {
  mockFetch.mockReset();
  mockRouterPush.mockClear();
  mockRouterReplace.mockClear();
  mockRouterBack.mockClear();
  navigationHistory.length = 0;
  setSearchParams();
});

function mockAgentCreation(email: string) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/domains") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [] }) });
    }
    if (url === "/api/inboxes" && !init?.method) {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
    }
    if (url === "/v1/agents" && init?.method === "POST") {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ id: "ag_test", domain: "agents.e2a.dev", email }),
      });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
  });
}

function mockAgentCreationFailure(errorMessage: string) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/domains") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [] }) });
    }
    if (url === "/api/inboxes" && !init?.method) {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
    }
    if (url === "/v1/agents" && init?.method === "POST") {
      return Promise.resolve({
        ok: false,
        status: 400,
        text: () => Promise.resolve(errorMessage),
      });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
  });
}

// ── Address type choice ──────────────────────────────────

describe("Address type choice", () => {
  it("renders Shared e2a domain and Custom domain options for fresh user", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await chooseWebUI();
    expect(screen.getByText("Custom domain")).toBeInTheDocument();
  });

  it("shows a neutral chooser on plain /get-started with no state", async () => {
    mockFreshUser();
    render(<GetStartedPage />);

    await chooseWebUI();
    expect(screen.getByRole("button", { name: /Shared e2a domain/i })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(screen.getByRole("button", { name: /Custom domain/i })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("does NOT show Cloud Agents or Local Agents labels", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await chooseWebUI();
    expect(screen.queryByText("Cloud Agents")).not.toBeInTheDocument();
    expect(screen.queryByText("Local Agents")).not.toBeInTheDocument();
  });

  it("shows shared form when Shared e2a domain is clicked", async () => {
    mockFreshUser();
    render(<GetStartedPage />);

    await chooseWebUI();
    fireEvent.click(screen.getByText("Shared e2a domain"));

    await waitFor(() => {
      expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    });
    expect(screen.getByPlaceholderText("my-agent")).toBeInTheDocument();
  });

  it("shows custom domain checklist when Custom domain is clicked", async () => {
    mockFreshUser();
    render(<GetStartedPage />);

    await chooseWebUI();
    fireEvent.click(screen.getByText("Custom domain"));

    await waitFor(() => {
      expect(screen.getByText("Choose a domain")).toBeInTheDocument();
    });
  });

  it("shows plugin and MCP setup for Claude Code by default", async () => {
    mockFreshUser();
    render(<GetStartedPage />);

    await waitFor(() => {
      expect(screen.getByText("With an agent")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText("With an agent"));

    await waitFor(() => expect(screen.getByText("Claude Code setup")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Claude Code" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(
      screen.getByText(/claude plugin marketplace add tokencanopy\/e2a/),
    ).toBeInTheDocument();
    expect(screen.getByText(/claude plugin install e2a@e2a/)).toBeInTheDocument();
    expect(
      screen.getByText(
        /claude mcp add --transport http --scope user e2a https:\/\/api\.e2a\.dev\/mcp/,
      ),
    ).toBeInTheDocument();
  });

  it("switches between Codex and generic MCP instructions", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await waitFor(() => expect(screen.getByText("With an agent")).toBeInTheDocument());
    fireEvent.click(screen.getByText("With an agent"));
    await waitFor(() => expect(screen.getByText("Claude Code setup")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Codex" }));
    expect(screen.getByText("Codex setup")).toBeInTheDocument();
    expect(
      screen.getByText(/codex plugin marketplace add tokencanopy\/e2a/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/codex mcp add e2a --url https:\/\/api\.e2a\.dev\/mcp/),
    ).toBeInTheDocument();
    expect(screen.getByText(/codex mcp login e2a/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Other agents" }));
    expect(screen.getByText("Connect any MCP client")).toBeInTheDocument();
    expect(screen.getByText(/"url": "https:\/\/api\.e2a\.dev\/mcp"/)).toBeInTheDocument();
  });

  it("copies the Claude Code plugin install commands", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await waitFor(() => expect(screen.getByText("With an agent")).toBeInTheDocument());
    fireEvent.click(screen.getByText("With an agent"));
    await waitFor(() => expect(screen.getByText("Copy plugin commands")).toBeInTheDocument());

    fireEvent.click(screen.getByText("Copy plugin commands"));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(
      expect.stringContaining("claude plugin marketplace add tokencanopy/e2a"),
    );
  });
});

// ── Shared local flow ────────────────────────────────────

describe("Shared local flow", () => {
  it("creates a local agent and shows local success state", async () => {
    mockAgentCreation("my-bot@agents.e2a.dev");
    render(<GetStartedPage />);

    // Wait for bootstrap then choose web UI -> shared
    await chooseWebUI();
    fireEvent.click(screen.getByText("Shared e2a domain"));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("my-agent")).toBeInTheDocument();
    });

    // Enter slug
    await userEvent.type(screen.getByPlaceholderText("my-agent"), "my-bot");

    // Submit
    fireEvent.click(screen.getByText("Create inbox"));

    // Verify API call
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/v1/agents",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ email: "my-bot@agents.e2a.dev" }),
        }),
      );
    });

    // Should show success state
    await waitFor(() => {
      expect(screen.getByText("Inbox created!")).toBeInTheDocument();
    });
    expect(screen.getByText("Connect your application or agent")).toBeInTheDocument();
    expect(screen.getByText("Install the e2a skill")).toBeInTheDocument();
  });
});

// ── Shared form validation ───────────────────────────────

describe("Shared form validation", () => {
  it("shows inline error for invalid slug", async () => {
    mockFreshUser();
    render(<GetStartedPage />);

    await chooseWebUI();
    fireEvent.click(screen.getByText("Shared e2a domain"));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("my-agent")).toBeInTheDocument();
    });

    await userEvent.type(screen.getByPlaceholderText("my-agent"), "a");
    fireEvent.click(screen.getByText("Create inbox"));

    await waitFor(() => {
      expect(screen.getByText(/Slug must be 2/)).toBeInTheDocument();
    });

    expect(mockFetch).not.toHaveBeenCalledWith("/v1/agents", expect.anything());
  });
});

// ── Slug/server error handling ───────────────────────────

describe("Slug/server error handling", () => {
  it("shows server error inline and keeps the slug field populated", async () => {
    mockAgentCreationFailure('slug "my-bot" is already taken');
    render(<GetStartedPage />);

    await chooseWebUI();
    fireEvent.click(screen.getByText("Shared e2a domain"));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("my-agent")).toBeInTheDocument();
    });

    await userEvent.type(screen.getByPlaceholderText("my-agent"), "my-bot");
    fireEvent.click(screen.getByText("Create inbox"));

    await waitFor(() => {
      expect(screen.getByText('slug "my-bot" is already taken')).toBeInTheDocument();
    });

    // Slug field should still have the value
    expect(screen.getByPlaceholderText("my-agent")).toHaveValue("my-bot");
    // Should still be on the form
    expect(screen.getByText("Create inbox")).toBeInTheDocument();
  });
});

// ── Query param support ──────────────────────────────────

describe("Query param support", () => {
  it("?mode=shared renders the shared flow immediately", () => {
    setSearchParams({ mode: "shared" });
    render(<GetStartedPage />);

    // Should skip the address choice and show the shared form
    expect(screen.queryByText("Give your agent an email address")).not.toBeInTheDocument();
    expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("my-agent")).toBeInTheDocument();
  });

  it("?domain= resumes at agent creation for a verified domain", async () => {
    setSearchParams({ domain: "verified.example.com" });
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [verifiedDomain] }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await waitFor(() => {
      expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    });
    expect(screen.getByText("@verified.example.com")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("support")).toBeInTheDocument();
  });

  it("?domain= falls back to choose when domain is not owned", async () => {
    setSearchParams({ domain: "missing.example.com" });
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [] }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await waitFor(() => {
      expect(
        screen.getByText("Domain missing.example.com not found in your account"),
      ).toBeInTheDocument();
    });
    // Should fall back to the top-level method choice (router.replace to
    // /get-started → step=choose), not the register step.
    expect(screen.getByText("With an agent")).toBeInTheDocument();
    expect(screen.getByText("Set up in the web UI")).toBeInTheDocument();
  });

  it("?domain= for unverified domain resumes at DNS+verify step", async () => {
    const unverifiedDomainFixture = {
      ...verifiedDomain,
      domain: "unverified.example.com",
      verified: false,
      verified_at: null,
    };
    setSearchParams({ domain: "unverified.example.com" });
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [unverifiedDomainFixture] }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    // DNS and verify shown together for unverified domains
    await waitFor(() => {
      expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
    });
    expect(screen.getByText("Route email to e2a")).toBeInTheDocument();
    expect(screen.getByText("Route email for all subdomains")).toBeInTheDocument();
    expect(screen.queryByText("inbound_mx_wildcard")).not.toBeInTheDocument();
    expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
  });
});

// ── Custom-domain flow: new domain ───────────────────────

const unverifiedDomain = {
  domain: "mail.newco.com",
  verified: false,
  verification_token: "e2a-verify=new123",
  dns_records: [
    {
      type: "TXT",
      name: "mail.newco.com",
      value: "e2a-verify=new123",
      priority: null,
      purpose: "ownership",
      status: "pending",
    },
    {
      type: "MX",
      name: "mail.newco.com",
      value: "mx.e2a.dev",
      priority: 10,
      purpose: "inbound_mx",
      status: "pending",
    },
  ],
  created_at: "2026-03-01T00:00:00Z",
  verified_at: null,
};

function mockCustomDomainFlow() {
  let domainVerified = false;
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    // listAgents — empty for bootstrap
    if (url === "/api/inboxes" && !init?.method) {
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
    }
    // listDomains — initially empty
    if (url === "/v1/domains" && (!init?.method || init.method === "GET")) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ items: [] }),
      });
    }
    // registerDomain
    if (url === "/v1/domains" && init?.method === "POST") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(unverifiedDomain),
      });
    }
    // verifyDomain
    if (url.includes("/verify") && init?.method === "POST") {
      domainVerified = true;
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ domain: "mail.newco.com", verified: true }),
      });
    }
    // createAgent
    if (url === "/v1/agents" && init?.method === "POST") {
      const body = JSON.parse(init.body as string);
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({
          id: "ag_custom",
          domain: "mail.newco.com",
          email: body.email,
        }),
      });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
  });
  return { isDomainVerified: () => domainVerified };
}

describe("Custom-domain flow: new domain -> DNS -> verify -> local agent", () => {
  it("completes the full custom local flow", async () => {
    mockCustomDomainFlow();
    render(<GetStartedPage />);

    // Wait for bootstrap then choose web UI -> custom domain
    await chooseWebUI();
    fireEvent.click(screen.getByText("Custom domain"));

    // Wait for domain selector to load
    await waitFor(() => {
      expect(screen.getByText("Choose a domain")).toBeInTheDocument();
    });

    // Register new domain
    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.newco.com");
    fireEvent.click(screen.getByText("Register domain"));

    // Should show DNS records and verify together
    await waitFor(() => {
      expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
    });
    expect(screen.getByText("Route email to e2a")).toBeInTheDocument();
    expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();

    // Verify domain
    fireEvent.click(screen.getByText("Verify domain"));

    // Should show agent creation
    await waitFor(() => {
      expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    });

    // Enter local part and submit
    await userEvent.type(screen.getByPlaceholderText("support"), "hello");
    fireEvent.click(screen.getByText("Create inbox"));

    // Should show success
    await waitFor(() => {
      expect(screen.getByText("Inbox created!")).toBeInTheDocument();
    });
    expect(screen.getByText("Connect your application or agent")).toBeInTheDocument();
  });
});

describe("Custom-domain flow: existing verified domain -> create agent directly", () => {
  it("skips DNS/verify for already-verified domains", async () => {
    setSearchParams({ domain: "verified.example.com" });
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [verifiedDomain] }),
        });
      }
      if (url === "/v1/agents" && init?.method === "POST") {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({
            id: "ag_v",
            domain: "verified.example.com",
            email: "bot@verified.example.com",
          }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    // Should jump straight to agent creation
    await waitFor(() => {
      expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    });

    // Create agent
    await userEvent.type(screen.getByPlaceholderText("support"), "bot");
    fireEvent.click(screen.getByText("Create inbox"));

    await waitFor(() => {
      expect(screen.getByText("Inbox created!")).toBeInTheDocument();
    });
  });
});

describe("Custom-domain flow: existing unverified domain -> resume verify", () => {
  it("resumes at DNS+verify for unverified domain via query param", async () => {
    const unverified = {
      ...verifiedDomain,
      domain: "pending.example.com",
      verified: false,
      verified_at: null,
    };
    setSearchParams({ domain: "pending.example.com" });
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [unverified] }),
        });
      }
      if (url.includes("/verify") && init?.method === "POST") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ domain: "pending.example.com", verified: true }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    // DNS and verify shown together
    await waitFor(() => {
      expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
      expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
    });

    // Verify
    fireEvent.click(screen.getByText("Verify domain"));

    // Should advance to agent creation
    await waitFor(() => {
      expect(screen.getByText("Create your inbox")).toBeInTheDocument();
    });
  });
});

describe("Custom-domain flow: verification retry", () => {
  it("shows error and allows retry on verification failure", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/inboxes" && !init?.method) {
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
      }
      if (url === "/v1/domains" && (!init?.method || init.method === "GET")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [] }),
        });
      }
      if (url === "/v1/domains" && init?.method === "POST") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(unverifiedDomain),
        });
      }
      if (url.includes("/verify") && init?.method === "POST") {
        return Promise.resolve({
          ok: false,
          status: 400,
          text: () => Promise.resolve("TXT record not found"),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    // Wait for bootstrap, then web UI -> custom -> register -> DNS+verify shown together
    await chooseWebUI();
    fireEvent.click(screen.getByText("Custom domain"));
    await waitFor(() => { expect(screen.getByText("Choose a domain")).toBeInTheDocument(); });
    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.newco.com");
    fireEvent.click(screen.getByText("Register domain"));
    await waitFor(() => {
      expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
      expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
    });

    // Attempt verify — should fail
    fireEvent.click(screen.getByText("Verify domain"));

    await waitFor(() => {
      expect(screen.getByText("TXT record not found")).toBeInTheDocument();
    });

    // Should still be on verify step with retry available
    expect(screen.getByText("Verify domain")).toBeInTheDocument();
    expect(screen.getAllByText(/DNS changes can take a few minutes/).length).toBeGreaterThan(0);
  });
});

// ── Smart re-entry / resume ─────────────────────────────

describe("Plain /get-started always shows address choice", () => {
  it("shows address choice even when user has existing agents", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/domains") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [] }) });
      }
      if (url === "/api/inboxes" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({
            agents: [{
              id: "ag_1", domain: "agents.e2a.dev", email: "bot@agents.e2a.dev",
              name: "bot", webhook_url: "", agent_mode: "local",
              domain_verified: true, public: false, created_at: "2026-01-01T00:00:00Z",
            }],
          }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await chooseWebUI();
    expect(screen.getByText("Custom domain")).toBeInTheDocument();
  });

  it("shows address choice even with unverified domains", async () => {
    const unverified = {
      ...verifiedDomain,
      domain: "pending.co",
      verified: false,
      verified_at: null,
    };
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/domains") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [unverified] }) });
      }
      if (url === "/api/inboxes" && !init?.method) {
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await chooseWebUI();
    expect(screen.getByText("Custom domain")).toBeInTheDocument();
  });

  it("shows address choice even with verified domain and no agents", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/domains") {
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ items: [verifiedDomain] }) });
      }
      if (url === "/api/inboxes" && !init?.method) {
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ agents: [] }) });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await chooseWebUI();
    expect(screen.getByText("Custom domain")).toBeInTheDocument();
  });
});

// ── URL-driven step navigation ───────────────────────────────
//
// The onboarding flow lives at /get-started?step=… so the browser
// back button moves between steps. These tests pin that contract.

describe("URL-driven step navigation", () => {
  it("clicking 'Shared e2a domain' pushes ?step=shared_form", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await chooseWebUI();

    fireEvent.click(screen.getByText("Shared e2a domain"));

    await waitFor(() => {
      expect(mockRouterPush).toHaveBeenCalledWith(
        "/get-started?step=shared_form",
      );
    });
  });

  it("clicking 'Custom domain' pushes ?step=custom_checklist", async () => {
    mockFreshUser();
    render(<GetStartedPage />);
    await chooseWebUI();

    fireEvent.click(screen.getByText("Custom domain"));

    await waitFor(() => {
      expect(mockRouterPush).toHaveBeenCalledWith(
        "/get-started?step=custom_checklist",
      );
    });
  });

  it("?step=shared_form renders the form directly (no choose step in the way)", () => {
    setSearchParams({ step: "shared_form" });
    mockFreshUser();
    render(<GetStartedPage />);
    expect(screen.getByText("Create your inbox")).toBeInTheDocument();
  });

  it("?step=custom_checklist renders the checklist directly", async () => {
    setSearchParams({ step: "custom_checklist" });
    mockFreshUser();
    render(<GetStartedPage />);
    // The checklist progress strip is the unambiguous signal that
    // we landed at the custom flow rather than the chooser.
    await waitFor(() => {
      expect(screen.getByText("Domain selected")).toBeInTheDocument();
    });
  });

  it("Back button on shared form navigates back via the router", async () => {
    setSearchParams({ step: "shared_form" });
    mockFreshUser();
    render(<GetStartedPage />);
    await screen.findByText("Create your inbox");

    fireEvent.click(screen.getByRole("button", { name: /back/i }));

    // Either router.back() (when there's history) or router.push to
    // /get-started (fresh tab). Both count as "going back to choose".
    await waitFor(() => {
      expect(
        mockRouterBack.mock.calls.length +
          mockRouterPush.mock.calls.filter((c) => c[0] === "/get-started")
            .length,
      ).toBeGreaterThan(0);
    });
  });

  it("Back button on custom checklist navigates back via the router", async () => {
    setSearchParams({ step: "custom_checklist" });
    mockFreshUser();
    render(<GetStartedPage />);

    // The checklist mounts asynchronously while it lists domains
    const backBtn = await screen.findByRole("button", { name: /back/i });
    fireEvent.click(backBtn);

    await waitFor(() => {
      expect(
        mockRouterBack.mock.calls.length +
          mockRouterPush.mock.calls.filter((c) => c[0] === "/get-started")
            .length,
      ).toBeGreaterThan(0);
    });
  });

  it("?mode=shared (legacy) gets translated to ?step=shared_form via replace", async () => {
    setSearchParams({ mode: "shared" });
    mockFreshUser();
    render(<GetStartedPage />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith(
        "/get-started?step=shared_form",
      );
    });
  });

  it("?domain=… (legacy) gets translated to ?step=custom_checklist via replace", async () => {
    setSearchParams({ domain: "verified.example.com" });
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/domains") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ items: [verifiedDomain] }),
        });
      }
      return Promise.resolve({ ok: false, text: () => Promise.resolve("not found") });
    });

    render(<GetStartedPage />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith(
        "/get-started?step=custom_checklist",
      );
    });
  });

  it("?step=success without local agent state falls back to choose", () => {
    setSearchParams({ step: "success" });
    mockFreshUser();
    render(<GetStartedPage />);
    // No SuccessPanel — the top-level method choice instead, because
    // there's no agent in local state (typical on refresh / direct URL
    // hit). The success fallback renders SetupMethodChoice.
    expect(screen.getByText("With an agent")).toBeInTheDocument();
    expect(screen.getByText("Set up in the web UI")).toBeInTheDocument();
  });
});
