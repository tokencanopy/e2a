// Complements page.test.tsx (rendering, export link, danger zone) with the
// profile name-edit flow: edit → validate → PATCH → setUser, plus the
// error and cancel branches.

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import SettingsPage from "./page";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

const setUser = jest.fn();
let mockAuth: {
  user: { id: string; email: string; name: string; created_at: string } | null;
  loading: boolean;
  setUser: jest.Mock;
};

jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => mockAuth,
}));

const mockFetch = jest.fn();
global.fetch = mockFetch as unknown as typeof fetch;

beforeEach(() => {
  mockAuth = {
    user: {
      id: "usr_abc123",
      email: "alice@example.com",
      name: "Alice",
      created_at: "2026-04-01T10:00:00Z",
    },
    loading: false,
    setUser,
  };
  setUser.mockClear();
  mockFetch.mockReset();
});

async function openEditor() {
  render(<SettingsPage />);
  await userEvent.click(screen.getByRole("button", { name: "Edit" }));
  return screen.getByDisplayValue("Alice");
}

describe("Settings — profile name editing", () => {
  it("opens the editor prefilled with the current name", async () => {
    const input = await openEditor();
    expect(input).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("disables Save for a blank or untrimmed name", async () => {
    const input = await openEditor();

    await userEvent.clear(input);
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

    await userEvent.type(input, " Alice");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

    await userEvent.clear(input);
    await userEvent.type(input, "Alice Cooper");
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("PATCHes the new name and hands the updated user to the auth context", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ id: "usr_abc123", name: "Alice Cooper" }),
    });
    const input = await openEditor();

    await userEvent.clear(input);
    await userEvent.type(input, "Alice Cooper");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(setUser).toHaveBeenCalledWith({ id: "usr_abc123", name: "Alice Cooper" }),
    );
    expect(mockFetch).toHaveBeenCalledWith("/api/auth/me", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ name: "Alice Cooper" }),
    });
    // Editing mode closes after a successful save.
    expect(screen.queryByDisplayValue("Alice Cooper")).not.toBeInTheDocument();
  });

  it("shows the server error and stays in editing mode on failure", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 400,
      text: () => Promise.resolve("name too long"),
    });
    const input = await openEditor();

    await userEvent.clear(input);
    await userEvent.type(input, "Alice Cooper");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("name too long")).toBeInTheDocument();
    expect(screen.getByDisplayValue("Alice Cooper")).toBeInTheDocument();
    expect(setUser).not.toHaveBeenCalled();
  });

  it("shows a network-error message when the request rejects", async () => {
    mockFetch.mockRejectedValue(new Error("offline"));
    const input = await openEditor();

    await userEvent.clear(input);
    await userEvent.type(input, "Alice Cooper");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Network error")).toBeInTheDocument();
  });

  it("cancelling exits editing and restores the original draft", async () => {
    const input = await openEditor();
    await userEvent.clear(input);
    await userEvent.type(input, "Changed");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByDisplayValue("Changed")).not.toBeInTheDocument();

    // Reopening starts from the saved name, not the abandoned draft.
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByDisplayValue("Alice")).toBeInTheDocument();
  });
});
