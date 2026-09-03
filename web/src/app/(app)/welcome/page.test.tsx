import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import WelcomePage from "./page";

const mockReplace = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ replace: mockReplace, push: jest.fn(), back: jest.fn() }),
  usePathname: () => "/welcome",
}));

const mockSetUser = jest.fn();
const baseUser = {
  id: "usr_1",
  email: "alice@example.test",
  name: "Alice",
  created_at: "2026-01-01T00:00:00Z",
  onboarding_survey_pending: true,
};
jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => ({ user: baseUser, loading: false, setUser: mockSetUser, signOut: jest.fn() }),
}));

const fetchMock = jest.fn();

beforeEach(() => {
  mockReplace.mockReset();
  mockSetUser.mockReset();
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

function okResponse(body: unknown) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
}
function errResponse(status: number) {
  return { ok: false, status, json: async () => ({}), text: async () => "nope" };
}

function lastPatchBody() {
  const [, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1];
  return JSON.parse((init as RequestInit).body as string);
}

describe("/welcome", () => {
  it("renders the question, all nine options, and a disabled Continue", () => {
    render(<WelcomePage />);
    expect(screen.getByRole("heading", { name: "Where did you hear about e2a?" })).toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(9);
    expect(screen.getByRole("radio", { name: "Friend or colleague" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.queryByPlaceholderText("Tell us more (optional)")).not.toBeInTheDocument();
  });

  it("submits the chosen source, pushes the response into auth, and goes to /inboxes", async () => {
    const updated = { ...baseUser, onboarding_survey_pending: false };
    fetchMock.mockResolvedValue(okResponse(updated));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "GitHub" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/auth/me");
    expect((init as RequestInit).method).toBe("PATCH");
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "github" } });
    expect(mockSetUser).toHaveBeenCalledWith(updated);
  });

  it("reveals the detail field for Other, enforces the limit, and sends it", async () => {
    fetchMock.mockResolvedValue(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "Other" }));
    const detail = screen.getByPlaceholderText("Tell us more (optional)");
    expect(detail).toHaveAttribute("maxLength", "200");
    expect(screen.getByText("0/200")).toBeInTheDocument();
    await userEvent.type(detail, "a newsletter");
    expect(screen.getByText("12/200")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "other", detail: "a newsletter" } });
  });

  it("Skip records skipped and leaves", async () => {
    fetchMock.mockResolvedValue(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "skipped" } });
  });

  it("treats 409 as done", async () => {
    fetchMock.mockResolvedValue(errResponse(409));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "Search engine" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(mockSetUser).toHaveBeenCalledWith({ ...baseUser, onboarding_survey_pending: false });
  });

  it("shows an error on a 500 and keeps the form and Skip usable", async () => {
    fetchMock.mockResolvedValueOnce(errResponse(500));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "MCP directory" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/try again or skip/i);
    expect(mockReplace).not.toHaveBeenCalled();
    expect(screen.getByRole("radio", { name: "MCP directory" })).toBeChecked();
    fetchMock.mockResolvedValueOnce(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
  });

  it("Skip still leaves when the network is down", async () => {
    fetchMock.mockRejectedValue(new Error("offline"));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(mockSetUser).toHaveBeenCalledWith({ ...baseUser, onboarding_survey_pending: false });
  });
});
