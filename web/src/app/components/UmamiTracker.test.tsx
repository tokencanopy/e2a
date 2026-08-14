import { act, render, waitFor } from "@testing-library/react";
import { UmamiTracker } from "./UmamiTracker";

let mockPathname = "/";
let mockWebsiteId = "website_test";

jest.mock("next/navigation", () => ({
  usePathname: () => mockPathname,
}));

jest.mock("../../lib/site", () => ({
  get UMAMI_WEBSITE_ID() {
    return mockWebsiteId;
  },
}));

jest.mock("next/script", () => {
  const React = jest.requireActual<typeof import("react")>("react");

  return function MockScript(props: React.ComponentProps<"script"> & {
    onReady?: () => void;
    strategy?: string;
  }) {
    const { onReady } = props;
    const scriptProps = { ...props };
    delete scriptProps.onReady;
    delete scriptProps.strategy;
    React.useEffect(() => {
      onReady?.();
    }, [onReady]);
    return <script {...scriptProps} />;
  };
});

type UmamiPayload = Record<string, unknown> & {
  referrer?: string;
  url?: string;
};

type TrackArgument = (payload: UmamiPayload) => UmamiPayload;

const track = jest.fn<void, [TrackArgument]>();

beforeEach(() => {
  mockPathname = "/";
  mockWebsiteId = "website_test";
  window.history.replaceState({}, "", "/");
  Object.defineProperty(window, "umami", {
    configurable: true,
    value: { track },
    writable: true,
  });
  track.mockReset();
});

afterEach(() => {
  delete (window as Window & { umami?: unknown }).umami;
  delete (window as Window & { tcUmamiBeforeSend?: unknown }).tcUmamiBeforeSend;
});

describe("UmamiTracker", () => {
  it("does not load or track when analytics is not configured", async () => {
    mockWebsiteId = "";
    render(<UmamiTracker />);

    await act(async () => {});
    expect(
      document.querySelector(
        'script[src="/vendor/umami/umami-v3.2.0.1ad1145d.js"]',
      ),
    ).toBeNull();
    expect(track).not.toHaveBeenCalled();
  });

  it("loads an inert tracker and manually records one public pageview without query data", async () => {
    window.history.replaceState({}, "", "/?campaign=sensitive");
    render(<UmamiTracker />);

    const script = document.querySelector("#tc-umami-tracker");
    expect(script).toHaveAttribute(
      "src",
      "/vendor/umami/umami-v3.2.0.1ad1145d.js",
    );
    expect(script).toHaveAttribute(
      "data-host-url",
      "https://umami.tokencanopy.com",
    );
    expect(
      document.querySelector('script[src^="https://umami.tokencanopy.com"]'),
    ).toBeNull();
    expect(script).toHaveAttribute("data-auto-track", "false");
    expect(script).toHaveAttribute("data-exclude-search", "true");
    expect(script).toHaveAttribute("data-exclude-hash", "true");
    expect(script).toHaveAttribute("data-before-send", "tcUmamiBeforeSend");

    await waitFor(() => expect(track).toHaveBeenCalledTimes(1));
    const payload = track.mock.calls[0][0]({
      referrer: "https://search.example/?q=private",
      url: "http://localhost/?campaign=sensitive",
    });
    expect(payload.url).toBe("http://localhost/");
    expect(payload.referrer).toBe("");
  });

  it.each([
    "/",
    "/api-docs",
    "/blog",
    "/blog/build-ecommerce-agent-with-email",
    "/blog/build-procurement-agent-with-email",
    "/blog/build-recruiting-agent-with-email",
    "/blog/build-sales-agent-with-email",
    "/blog/build-voice-follow-up-agent-with-email",
    "/blog/email-agent-with-google-adk",
    "/blog/email-for-openclaw-agents",
    "/blog/human-in-the-loop-for-agent-email",
    "/blog/send-email-from-python-agent",
    "/compare/e2a-vs-agentmail",
    "/docs",
    "/docs/python",
    "/email-api-for-ai-agents",
    "/mcp",
    "/python-sdk",
    "/use-cases",
    "/use-cases/ai-receptionist",
    "/use-cases/ecommerce-agent",
    "/use-cases/procurement-agent",
    "/use-cases/recruiting-agent",
    "/use-cases/sales-agent",
    "/use-cases/scheduling-agent",
    "/use-cases/support-agent",
    "/use-cases/voice-agent",
  ])("tracks the known public route %s", async (pathname) => {
    mockPathname = pathname;
    window.history.replaceState({}, "", pathname);
    render(<UmamiTracker />);

    await waitFor(() => expect(track).toHaveBeenCalledTimes(1));
  });

  it.each([
    "/inboxes/messages/view",
    "/get-started",
    "/oauth/consent",
    "/future-authenticated-route",
    "/blog/future-private-route",
  ])("fails closed and does not load or track %s", async (pathname) => {
    mockPathname = pathname;
    window.history.replaceState({}, "", `${pathname}?id=resource_private`);
    render(<UmamiTracker />);

    await act(async () => {});
    expect(
      document.querySelector(
        'script[src="/vendor/umami/umami-v3.2.0.1ad1145d.js"]',
      ),
    ).toBeNull();
    expect(track).not.toHaveBeenCalled();
  });

  it("does not track a client-side transition from marketing into the dashboard", async () => {
    const { rerender } = render(<UmamiTracker />);
    await waitFor(() => expect(track).toHaveBeenCalledTimes(1));

    mockPathname = "/inboxes/messages/view";
    window.history.pushState(
      {},
      "",
      "/inboxes/messages/view?email=agent%40example.com&id=msg_private",
    );
    rerender(<UmamiTracker />);

    await act(async () => {});
    expect(track).toHaveBeenCalledTimes(1);
  });

  it("starts tracking only after a private-to-public transition and stops again on private routes", async () => {
    mockPathname = "/oauth/consent";
    window.history.replaceState({}, "", "/oauth/consent?state=private");
    const { rerender } = render(<UmamiTracker />);

    await act(async () => {});
    expect(track).not.toHaveBeenCalled();

    mockPathname = "/docs";
    window.history.pushState({}, "", "/docs?source=private");
    rerender(<UmamiTracker />);
    await waitFor(() => expect(track).toHaveBeenCalledTimes(1));
    expect(track.mock.calls[0][0]({}).referrer).toBe("");

    mockPathname = "/inboxes";
    window.history.pushState({}, "", "/inboxes?id=private");
    rerender(<UmamiTracker />);

    await act(async () => {});
    expect(track).toHaveBeenCalledTimes(1);
  });

  it("drops any attempted private payload and strips public query strings", async () => {
    render(<UmamiTracker />);
    await waitFor(() => expect(window.tcUmamiBeforeSend).toBeDefined());

    expect(
      window.tcUmamiBeforeSend?.("event", {
        url: "http://localhost/oauth/consent?state=secret",
      }),
    ).toBeUndefined();
    expect(
      window.tcUmamiBeforeSend?.("event", {
        referrer: "https://search.example/?q=private",
        url: "http://localhost/docs?resource=private#fragment",
      }),
    ).toMatchObject({
      referrer: "https://search.example/",
      url: "http://localhost/docs",
    });
    expect(
      window.tcUmamiBeforeSend?.("event", {
        url: "http://user:secret@localhost/docs?resource=private",
      }),
    ).toMatchObject({ url: "http://localhost/docs" });
  });
});
