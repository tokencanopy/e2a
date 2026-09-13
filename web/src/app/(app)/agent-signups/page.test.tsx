import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import AgentSignupsPage from "./page";

const mockFetch = jest.fn();
global.fetch = mockFetch;

const signup = {
  id: "as_1",
  inbox: "build-bot@agents.example.test",
  human_email: "owner@example.test",
  display_name: "Build Bot",
  note_to_human: "",
  harness: "codex",
  status: "pending",
  review_outbound: false,
  created_at: "2026-09-01T00:00:00Z",
  verification_expires_at: "2026-09-03T00:00:00Z",
};

function response(body: unknown) {
  return Promise.resolve({
    ok: true,
    status: 200,
    text: () => Promise.resolve(JSON.stringify(body)),
  });
}

beforeEach(() => {
  mockFetch.mockReset();
  mockFetch.mockReturnValue(response({ items: [signup], next_cursor: null }));
});

it("lets the human approve a requested agent with the existing review gate", async () => {
  render(<AgentSignupsPage />);

  expect(await screen.findByText("Build Bot")).toBeInTheDocument();
  expect(screen.getByText("build-bot@agents.example.test")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("checkbox", { name: /review outbound/i }));
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));

  await waitFor(() => {
    expect(mockFetch).toHaveBeenCalledWith(
      "/v1/agent-signup/as_1/approve",
      expect.objectContaining({ body: '{"review_outbound":true}' }),
    );
  });
});

it("rejects a request and removes it from the list", async () => {
  render(<AgentSignupsPage />);
  await screen.findByText("Build Bot");
  fireEvent.click(screen.getByRole("button", { name: "Reject" }));

  await waitFor(() => {
    expect(screen.queryByText("Build Bot")).not.toBeInTheDocument();
  });
});
