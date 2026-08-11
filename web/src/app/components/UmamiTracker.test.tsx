import { act, render, waitFor } from "@testing-library/react";
import { UmamiTracker } from "./UmamiTracker";

let mockPathname = "/";

jest.mock("next/navigation", () => ({
  usePathname: () => mockPathname,
}));

jest.mock("../../lib/site", () => ({
  UMAMI_WEBSITE_ID: "website_test",
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
  it("loads an inert tracker and manually records one public pageview without query data", async () => {
    window.history.replaceState({}, "", "/?campaign=sensitive");
    render(<UmamiTracker />);

    const script = document.querySelector(
      'script[src="https://umami.tokencanopy.com/script.js"]',
    );
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
    "/api-docs",
    "/blog/send-email-from-python-agent",
    "/compare/e2a-vs-agentmail",
    "/docs/python",
    "/email-api-for-ai-agents",
    "/mcp",
    "/python-sdk",
    "/use-cases/customer-support",
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
  ])("fails closed and does not load or track %s", async (pathname) => {
    mockPathname = pathname;
    window.history.replaceState({}, "", `${pathname}?id=resource_private`);
    render(<UmamiTracker />);

    await act(async () => {});
    expect(
      document.querySelector(
        'script[src="https://umami.tokencanopy.com/script.js"]',
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
  });
});
