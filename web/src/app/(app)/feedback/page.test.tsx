import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import FeedbackPage from "./page";

let mockAuth: {
  user: { id: string; email: string; name: string; created_at: string } | null;
  loading: boolean;
  signOut: jest.Mock;
  setUser: jest.Mock;
};

jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => mockAuth,
}));

const mockFetch = jest.fn();
global.fetch = mockFetch;

beforeEach(() => {
  mockFetch.mockReset();
  mockAuth = {
    user: {
      id: "usr_abc123",
      email: "alice@example.com",
      name: "Alice",
      created_at: "2026-04-01T10:00:00Z",
    },
    loading: false,
    signOut: jest.fn(),
    setUser: jest.fn(),
  };
});

describe("Feedback page", () => {
  it("renders form with all elements", () => {
    render(<FeedbackPage />);
    expect(screen.getByText("Send us feedback")).toBeInTheDocument();
    expect(screen.queryByPlaceholderText("you@example.com")).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText("What's on your mind?")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Bug report" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Feature request" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "General" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Submit feedback" })).toBeDisabled();
  });

  it("submits with the signed-in user's email, no email field shown", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => ({ status: "ok" }) });
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "Great product!");
    await user.click(screen.getByRole("button", { name: "Submit feedback" }));
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "/api/feedback",
        expect.objectContaining({
          body: JSON.stringify({ email: "alice@example.com", category: "general", message: "Great product!" }),
        }),
      );
    });
  });

  it("enables submit when message is entered", async () => {
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "Great product!");
    expect(screen.getByRole("button", { name: "Submit feedback" })).toBeEnabled();
  });

  it("submits feedback successfully and shows thank you", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => ({ status: "ok" }) });
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "Love it!");
    await user.click(screen.getByRole("button", { name: "Submit feedback" }));
    await waitFor(() => {
      expect(screen.getByText("Thanks for your feedback")).toBeInTheDocument();
    });
  });

  it("shows error message on failed submission", async () => {
    mockFetch.mockResolvedValueOnce({ ok: false, status: 500, text: async () => "server error" });
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "Test error");
    await user.click(screen.getByRole("button", { name: "Submit feedback" }));
    await waitFor(() => {
      expect(screen.getByText("Something went wrong. Please try again or email us directly.")).toBeInTheDocument();
    });
  });

  it("shows rate-limited message on 429 response", async () => {
    mockFetch.mockResolvedValueOnce({ ok: false, status: 429, text: async () => "rate limited" });
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "Too many");
    await user.click(screen.getByRole("button", { name: "Submit feedback" }));
    await waitFor(() => {
      expect(screen.getByText("Too many submissions. Please wait a minute before trying again.")).toBeInTheDocument();
    });
  });

  it("allows submitting more feedback after success", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => ({ status: "ok" }) });
    const user = userEvent.setup();
    render(<FeedbackPage />);
    await user.type(screen.getByPlaceholderText("What's on your mind?"), "First feedback");
    await user.click(screen.getByRole("button", { name: "Submit feedback" }));
    await waitFor(() => {
      expect(screen.getByText("Thanks for your feedback")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Submit more feedback"));
    await waitFor(() => {
      expect(screen.getByText("Send us feedback")).toBeInTheDocument();
    });
  });

  it("selects different category", async () => {
    const user = userEvent.setup();
    render(<FeedbackPage />);
    const bugBtn = screen.getByRole("button", { name: "Bug report" });
    const generalBtn = screen.getByRole("button", { name: "General" });
    // Default category is "general"
    expect(generalBtn).toHaveAttribute("aria-pressed", "true");
    expect(bugBtn).toHaveAttribute("aria-pressed", "false");
    // After clicking bug, it becomes the pressed/selected one
    await user.click(bugBtn);
    expect(bugBtn).toHaveAttribute("aria-pressed", "true");
    expect(generalBtn).toHaveAttribute("aria-pressed", "false");
  });
});
