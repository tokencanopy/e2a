import { render, screen, fireEvent, act } from "@testing-library/react";
import { ThemeProvider, useTheme } from "./ThemeProvider";

// jsdom doesn't implement window.matchMedia; ThemeProvider needs it for
// the "system" theme. Install a controllable mock that records change
// listeners so tests can simulate the OS flipping light/dark.
type ChangeHandler = (e: { matches: boolean }) => void;
function mockMatchMedia(initialMatches: boolean) {
  const listeners = new Set<ChangeHandler>();
  const mq = {
    matches: initialMatches,
    media: "(prefers-color-scheme: dark)",
    addEventListener: (_: string, cb: ChangeHandler) => listeners.add(cb),
    removeEventListener: (_: string, cb: ChangeHandler) => listeners.delete(cb),
    setMatches(next: boolean) {
      mq.matches = next;
      listeners.forEach((cb) => cb({ matches: next }));
    },
  };
  window.matchMedia = jest.fn(() => mq) as unknown as typeof window.matchMedia;
  return mq;
}

function Probe() {
  const { theme, setTheme } = useTheme();
  return (
    <div>
      <span data-testid="theme">{theme}</span>
      <button onClick={() => setTheme("light")}>set light</button>
      <button onClick={() => setTheme("dark")}>set dark</button>
      <button onClick={() => setTheme("system")}>set system</button>
    </div>
  );
}

const rootClassList = () => document.documentElement.classList;

beforeEach(() => {
  localStorage.clear();
  rootClassList().remove("dark");
});
afterEach(() => {
  localStorage.clear();
  rootClassList().remove("dark");
});

describe("ThemeProvider — initial theme from localStorage", () => {
  it("defaults to 'system' when nothing is stored", () => {
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("system");
  });

  it("reads a stored 'dark' theme and applies the dark class to <html>", () => {
    localStorage.setItem("theme", "dark");
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("dark");
    expect(rootClassList().contains("dark")).toBe(true);
  });

  it("reads a stored 'light' theme and leaves the dark class off", () => {
    localStorage.setItem("theme", "light");
    mockMatchMedia(true); // OS prefers dark, but explicit light wins
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(rootClassList().contains("dark")).toBe(false);
  });

  it("falls back to 'system' for an unrecognized stored value", () => {
    localStorage.setItem("theme", "solarized");
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("system");
  });
});

describe("ThemeProvider — system theme follows the OS preference", () => {
  it("applies the dark class when the OS prefers dark", () => {
    mockMatchMedia(true);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(rootClassList().contains("dark")).toBe(true);
  });

  it("reacts to the OS preference changing while mounted", () => {
    const mq = mockMatchMedia(true);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(rootClassList().contains("dark")).toBe(true);

    act(() => mq.setMatches(false));
    expect(rootClassList().contains("dark")).toBe(false);
  });
});

describe("ThemeProvider — setTheme", () => {
  it("persists to localStorage, notifies subscribers, and applies the class", () => {
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    fireEvent.click(screen.getByText("set dark"));

    expect(localStorage.getItem("theme")).toBe("dark");
    expect(screen.getByTestId("theme")).toHaveTextContent("dark");
    expect(rootClassList().contains("dark")).toBe(true);
  });

  it("switching back to 'system' re-applies the OS preference", () => {
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    fireEvent.click(screen.getByText("set dark"));
    expect(rootClassList().contains("dark")).toBe(true);

    fireEvent.click(screen.getByText("set system"));
    expect(localStorage.getItem("theme")).toBe("system");
    expect(rootClassList().contains("dark")).toBe(false);
  });
});

describe("ThemeProvider — cross-tab synchronization", () => {
  it("picks up a theme written by another tab via the storage event", () => {
    mockMatchMedia(false);
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("theme")).toHaveTextContent("system");

    act(() => {
      localStorage.setItem("theme", "light");
      window.dispatchEvent(new Event("storage"));
    });

    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(rootClassList().contains("dark")).toBe(false);
  });
});
