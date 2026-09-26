import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import SettingsPage from "./page";

// jsdom doesn't provide navigator.clipboard. The signing-secret Copy
// button calls writeText, so we install a jest mock once at module
// level so individual tests can assert on it.
const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });
beforeEach(() => writeText.mockClear());

// Mock next/link to plain anchors so we don't need a router in jsdom.
jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

const signOut = jest.fn();
let mockAuth: {
  user: { id: string; email: string; name: string; created_at: string } | null;
  loading: boolean;
  signOut: jest.Mock;
};

jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => mockAuth,
}));

beforeEach(() => {
  mockAuth = {
    user: {
      id: "usr_abc123",
      email: "alice@example.com",
      name: "Alice",
      created_at: "2026-04-01T10:00:00Z",
    },
    loading: false,
    signOut,
  };
  // Generic fetch stub. The Settings page no longer fetches on mount
  // (the Usage section was removed); the delete-account test overrides
  // this with its own deferred mock.
  global.fetch = jest.fn(async () => ({
    ok: true,
    status: 200,
    json: async () => ({}),
    text: async () => "",
  })) as unknown as typeof fetch;
});

describe("Settings — Profile section", () => {
  it("shows the user's name, email, ID, and member-since date", () => {
    render(<SettingsPage />);
    expect(screen.getByText("Alice")).toBeInTheDocument();
    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
    expect(screen.getByText("usr_abc123")).toBeInTheDocument();
    // The exact format depends on locale but the year should always render.
    expect(screen.getByText(/2026/)).toBeInTheDocument();
  });

  it("renders nothing when user is null (defensive)", () => {
    mockAuth.user = null;
    const { container } = render(<SettingsPage />);
    expect(container.firstChild).toBeNull();
  });
});

describe("Settings — Export section", () => {
  it("links the Download export button at the API endpoint", () => {
    render(<SettingsPage />);
    const link = screen.getByRole("link", { name: /download export/i });
    expect(link).toHaveAttribute("href", "/v1/account/export");
  });
});


const mockHardNavigate = jest.fn();
jest.mock("../../../lib/navigation", () => ({
  hardNavigate: (url: string) => mockHardNavigate(url),
}));
beforeEach(() => mockHardNavigate.mockClear());

function openDeleteFlow() {
  fireEvent.click(screen.getByRole("button", { name: /delete account/i }));
}

describe("Settings — Danger zone (delete account)", () => {
  it("describes the trash, not an immediate irreversible deletion", () => {
    render(<SettingsPage />);
    expect(screen.getByText(/moves your account to the trash/i)).toBeInTheDocument();
    expect(screen.getByText(/Restoring doesn.t bring them back/i)).toBeInTheDocument();
    expect(screen.getByText(/Custom domains are unverified/i)).toBeInTheDocument();
    expect(screen.getByText(/may then be held for a while/i)).toBeInTheDocument();
    expect(screen.queryByText(/irreversible/i)).not.toBeInTheDocument();
  });

  it("hides the confirm input until the user opens the flow", () => {
    render(<SettingsPage />);
    expect(screen.queryByPlaceholderText("DELETE")).not.toBeInTheDocument();
    openDeleteFlow();
    expect(screen.getByPlaceholderText("DELETE")).toBeInTheDocument();
    // Trash is the default choice.
    expect(screen.getByRole("radio", { name: /move to trash/i })).toBeChecked();
    expect(screen.getByRole("radio", { name: /erase permanently now/i })).not.toBeChecked();
  });

  it("disables the final delete button until confirmation matches", () => {
    render(<SettingsPage />);
    openDeleteFlow();
    const finalBtn = screen.getByRole("button", { name: /delete my account/i });
    expect(finalBtn).toBeDisabled();

    const input = screen.getByPlaceholderText("DELETE") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "delete" } }); // wrong case
    expect(finalBtn).toBeDisabled();

    fireEvent.change(input, { target: { value: "DELETE" } });
    expect(finalBtn).toBeEnabled();
  });

  it("moves the account to the trash by default (no permanent flag) and redirects home", async () => {
    const fetchMock = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => '{"deleted":true,"mode":"trash"}',
      json: async () => ({ deleted: true, mode: "trash" }),
    }));
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => expect(mockHardNavigate).toHaveBeenCalledWith("/?account_deleted=1"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/account?confirm=DELETE",
      expect.objectContaining({ method: "DELETE", credentials: "include" }),
    );
  });

  it("disables the buttons while the delete is in flight", async () => {
    global.fetch = jest.fn(() => new Promise(() => {})) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    expect(await screen.findByRole("button", { name: /deleting…/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
  });

  it("erases permanently only after the separate acknowledgement, sending permanent=true", async () => {
    const fetchMock = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => '{"deleted":true,"mode":"permanent"}',
      json: async () => ({ deleted: true, mode: "permanent" }),
    }));
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });

    const eraseBtn = screen.getByRole("button", { name: /erase my account permanently/i });
    // Typed DELETE alone is not enough for the permanent path.
    expect(eraseBtn).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    expect(eraseBtn).toBeEnabled();
    fireEvent.click(eraseBtn);

    await waitFor(() => expect(mockHardNavigate).toHaveBeenCalledWith("/?account_deleted=1"));
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/account?confirm=DELETE&permanent=true",
      expect.objectContaining({ method: "DELETE", credentials: "include" }),
    );
  });

  it("switching back to trash clears the permanent acknowledgement", () => {
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    fireEvent.click(screen.getByRole("radio", { name: /move to trash/i }));
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    expect(screen.getByRole("checkbox", { name: /can.t be recovered/i })).not.toBeChecked();
  });

  it("shows an error message when the server rejects the delete", async () => {
    global.fetch = jest.fn(async () => ({ ok: false, status: 400, text: async () => "nope" })) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent(/Deleting didn.t finish:\s*nope/);
    });
    expect(mockHardNavigate).not.toHaveBeenCalled();
  });

  it("shows the error envelope's message, not the raw JSON", async () => {
    global.fetch = jest.fn(async () => ({
      ok: false,
      status: 409,
      text: async () =>
        JSON.stringify({
          error: { code: "send_in_progress", message: "a send is in progress; retry shortly", request_id: "req_test" },
        }),
    })) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("a send is in progress; retry shortly");
    });
    expect(screen.getByRole("alert")).not.toHaveTextContent("request_id");
  });
});
