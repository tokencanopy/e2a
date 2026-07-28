import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import WebhooksPage from "./page";

// jsdom doesn't provide navigator.clipboard. The signing-secret Copy
// button calls writeText, so we install a jest mock once at module
// level so individual tests can assert on it.
const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });
beforeEach(() => writeText.mockClear());

// Mock next/link → plain anchors (no router in jsdom).
jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...rest
  }: {
    href: string;
    children: React.ReactNode;
    [k: string]: unknown;
  }) {
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  };
});

// --- Fetch mock helpers (lifted verbatim from the old settings test
//     file — the same helpers covered the signing-secrets section
//     before it moved here). ---

type FetchInit = { method?: string; body?: BodyInit };
type MockResponse = {
  ok: boolean;
  status: number;
  text: () => Promise<string>;
  json: () => Promise<unknown>;
};

function makeFetchMock(
  routes: Record<
    string,
    (init?: FetchInit) => MockResponse | Promise<MockResponse>
  >,
) {
  return jest.fn(async (url: string, init?: FetchInit) => {
    const method = init?.method ?? "GET";
    const key = `${method} ${url}`;
    const handler = routes[key] ?? routes[url];
    if (!handler) {
      throw new Error(`unmocked ${key}`);
    }
    return handler(init);
  });
}

function jsonResp(body: unknown, status = 200): MockResponse {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => text,
    json: async () => body,
  };
}

function errResp(text: string, status: number): MockResponse {
  return { ok: false, status, text: async () => text, json: async () => ({}) };
}

describe("Webhooks page", () => {
  // WebhookView from GET /v1/webhooks. GET never returns signing_secret —
  // it's only present on create / rotate responses.
  const webhook = {
    id: "wh_default0001",
    url: "https://app.example.com/inbox",
    description: "",
    events: ["email.received"],
    enabled: true,
    created_at: "2026-04-15T10:00:00Z",
  };

  it("lists existing webhooks (url, events, status) without exposing a secret", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("email.received")).toBeInTheDocument();
    // The Status column reports health, not the raw enabled flag: an enabled
    // endpoint that has never delivered is not a healthy one. This fixture
    // has no last_delivered_at.
    expect(screen.getByText("never delivered")).toBeInTheDocument();
    // No secret column / value in the list view.
    expect(document.body.innerHTML).not.toContain("signing_secret");
  });

  // e2a switching an endpoint off is louder than the user doing it, and it is
  // the state most likely to be silently losing events.
  it("calls out an auto-disabled subscription", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () =>
        jsonResp({
          items: [
            {
              ...webhook,
              enabled: false,
              auto_disabled_at: "2026-07-20T00:00:00Z",
            },
          ],
        }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(/auto-disabled/i)).toBeInTheDocument();
    });
  });

  it("reports a subscription that has never delivered", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(/never delivered/i)).toBeInTheDocument();
    });
  });

  // Health is derived from fields already on the list response. If a future
  // change starts probing per row, this catches it: N webhooks must still
  // cost one request.
  it("derives health without issuing a request per row", async () => {
    const fetchMock = makeFetchMock({
      "/v1/webhooks": () =>
        jsonResp({
          items: [
            { ...webhook, id: "wh_a", url: "https://a.test/h" },
            { ...webhook, id: "wh_b", url: "https://b.test/h" },
            { ...webhook, id: "wh_c", url: "https://c.test/h" },
          ],
        }),
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText("https://c.test/h")).toBeInTheDocument();
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // A bare URL doesn't read as a control. The row needs a signposted action
  // for the per-endpoint view, not just a clickable identifier.
  it("offers an explicit action into the per-webhook view", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    const action = screen.getByRole("link", { name: /deliveries/i });
    expect(action).toHaveAttribute(
      "href",
      "/webhooks/detail?id=wh_default0001",
    );
  });

  it("links each row to that webhook's detail page", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText(webhook.url).closest("a")).toHaveAttribute(
      "href",
      "/webhooks/detail?id=wh_default0001",
    );
  });

  it("shows the coding-agent prompt card with the scoping nudge", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(
      screen.getByRole("region", { name: "Set up with a coding agent" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/register the subscription/i),
    ).toBeInTheDocument();
    // The scoping nudge is the whole point of putting a notice here.
    expect(
      screen.getByText(/Only want events from certain inboxes\?/i),
    ).toBeInTheDocument();
  });

  // Scope column. An unscoped subscription receives every agent's events;
  // before this column existed, that was indistinguishable in the UI from a
  // narrowly-scoped one — which is exactly how an account-wide webhook went
  // unnoticed in production.
  it("renders an unscoped webhook as 'all agents' rather than blank", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [{ ...webhook, filters: {} }] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("all agents")).toBeInTheDocument();
  });

  it("treats a missing filters object as unscoped", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () => jsonResp({ items: [webhook] }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("all agents")).toBeInTheDocument();
  });

  it("lists the agents a scoped webhook is filtered to", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () =>
        jsonResp({
          items: [
            {
              ...webhook,
              filters: { agent_emails: ["agent@inbox.example.com"] },
            },
          ],
        }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("agent@inbox.example.com")).toBeInTheDocument();
    expect(screen.queryByText("all agents")).not.toBeInTheDocument();
  });

  it("summarizes conversation and label filters alongside agents", async () => {
    global.fetch = makeFetchMock({
      "/v1/webhooks": () =>
        jsonResp({
          items: [
            {
              ...webhook,
              filters: {
                agent_emails: ["a@example.com"],
                conversation_ids: ["conv_1", "conv_2"],
                labels: ["urgent"],
              },
            },
          ],
        }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(
      screen.getByText("a@example.com · 2 conversations · labels: urgent"),
    ).toBeInTheDocument();
  });

  it("creates a webhook, reveals the signing secret once, and hides it on dismiss", async () => {
    const created = {
      id: "wh_new0002",
      url: "https://app.example.com/hook2",
      description: "",
      events: ["email.received"],
      enabled: true,
      created_at: "2026-04-27T16:10:00Z",
      signing_secret: "whsec_" + "deadbeef".repeat(8),
    };
    let listCallCount = 0;
    global.fetch = makeFetchMock({
      "GET /v1/webhooks": () => {
        listCallCount++;
        if (listCallCount === 1) return jsonResp({ items: [webhook] });
        return jsonResp({
          items: [webhook, { ...created, signing_secret: undefined }],
        });
      },
      "POST /v1/webhooks": () => jsonResp(created, 201),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() =>
      expect(screen.getByText(webhook.url)).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    fireEvent.change(screen.getByPlaceholderText(/your-app\.com/i), {
      target: { value: created.url },
    });
    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => {
      expect(
        screen.getByText(/copy this signing secret now/i),
      ).toBeInTheDocument();
    });
    const plaintextInput = screen.getByLabelText(
      /plaintext signing secret/i,
    ) as HTMLInputElement;
    expect(plaintextInput.value).toBe(created.signing_secret);

    fireEvent.click(screen.getByRole("button", { name: /^dismiss$/i }));
    expect(
      screen.queryByText(/copy this signing secret now/i),
    ).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(created.signing_secret);
  });

  it("copies the revealed secret to the clipboard and flips the Copy label", async () => {
    const created = {
      id: "wh_new0003",
      url: "https://app.example.com/hook3",
      events: ["email.received"],
      enabled: true,
      created_at: "2026-04-27T16:10:00Z",
      signing_secret: "whsec_copyme",
    };
    global.fetch = makeFetchMock({
      "GET /v1/webhooks": () => jsonResp({ items: [webhook] }),
      "POST /v1/webhooks": () => jsonResp(created, 201),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() =>
      expect(screen.getByText(webhook.url)).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    fireEvent.change(screen.getByPlaceholderText(/your-app\.com/i), {
      target: { value: created.url },
    });
    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() =>
      expect(
        screen.getByLabelText(/plaintext signing secret/i),
      ).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: /^copy$/i }));
    expect(writeText).toHaveBeenCalledWith(created.signing_secret);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /^copied$/i }),
      ).toBeInTheDocument();
    });
  });

  it("shows an inline error when create fails", async () => {
    global.fetch = makeFetchMock({
      "GET /v1/webhooks": () => jsonResp({ items: [webhook] }),
      "POST /v1/webhooks": () => errResp("invalid url", 400),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() =>
      expect(screen.getByText(webhook.url)).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    fireEvent.change(screen.getByPlaceholderText(/your-app\.com/i), {
      target: { value: "https://bad" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => {
      expect(screen.getByText(/invalid url/i)).toBeInTheDocument();
    });
  });

  it("rotates the secret and reveals the new one", async () => {
    global.fetch = makeFetchMock({
      "GET /v1/webhooks": () => jsonResp({ items: [webhook] }),
      [`POST /v1/webhooks/${encodeURIComponent(webhook.id)}/rotate-secret`]: () =>
        jsonResp({
          signing_secret: "whsec_rotated",
          previous_secret_expires_at: "2026-05-01T00:00:00Z",
        }),
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() =>
      expect(screen.getByText(webhook.url)).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole("button", { name: /rotate secret/i }));

    await waitFor(() => {
      expect(
        screen.getByText(/copy the new signing secret now/i),
      ).toBeInTheDocument();
    });
    const plaintextInput = screen.getByLabelText(
      /plaintext signing secret/i,
    ) as HTMLInputElement;
    expect(plaintextInput.value).toBe("whsec_rotated");
  });

  it("DELETEs the right id when confirming a row delete", async () => {
    const second = {
      ...webhook,
      id: "wh_other",
      url: "https://app.example.com/other",
    };
    const deletedIDs: string[] = [];
    global.fetch = makeFetchMock({
      "GET /v1/webhooks": () => jsonResp({ items: [webhook, second] }),
      [`DELETE /v1/webhooks/${encodeURIComponent("wh_other")}?confirm=DELETE`]: () => {
        deletedIDs.push("wh_other");
        // Uniform delete contract: 200 + {deleted:true, id}.
        return {
          ok: true,
          status: 200,
          text: async () => JSON.stringify({ deleted: true, id: "wh_other" }),
          json: async () => ({ deleted: true, id: "wh_other" }),
        };
      },
    }) as unknown as typeof fetch;

    render(<WebhooksPage />);
    await waitFor(() =>
      expect(screen.getByText(second.url)).toBeInTheDocument(),
    );

    const delButtons = screen.getAllByRole("button", { name: /^delete$/i });
    expect(delButtons).toHaveLength(2);
    fireEvent.click(delButtons[1]);
    fireEvent.click(screen.getByRole("button", { name: /^confirm$/i }));

    await waitFor(() => {
      expect(deletedIDs).toEqual(["wh_other"]);
    });
  });
});
