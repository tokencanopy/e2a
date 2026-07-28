import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import OutreachPage from "./page";

jest.mock("next/navigation", () => ({
  useSearchParams: () => new URLSearchParams("email=raise%40example.com"),
}));

const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

it("renders real activity, scheduling, and suppression state", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({
      items: [{
        address: "partner@fund.vc",
        agent_email: "raise@example.com",
        stage: "follow-up",
        next_action_at: "2026-07-28T17:00:00Z",
        replied: false,
        suppressed: true,
        suppression_reason: "asked to stop",
        outbound_count: 2,
        inbound_count: 0,
        contact: { display_name: "A. Partner", metadata: {} },
      }],
    }),
  });
  render(<OutreachPage />);

  expect((await screen.findAllByText("A. Partner")).length).toBeGreaterThan(0);
  expect(screen.getAllByText(/Suppressed · asked to stop/).length).toBeGreaterThan(0);
  expect(screen.getAllByText("2 sent · 0 received").length).toBeGreaterThan(0);
  expect(screen.getAllByText("No reply").length).toBeGreaterThan(0);
});

it("edits outreach in a form and guards the write with If-Match", async () => {
  const outreach = {
    address: "partner@fund.vc",
    agent_email: "raise@example.com",
    stage: "follow-up",
    next_action_at: "2026-07-28T17:00:00Z",
    replied: false,
    suppressed: false,
    outbound_count: 2,
    inbound_count: 0,
    contact: { display_name: "A. Partner", metadata: {} },
  };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [outreach] }) })
    .mockResolvedValueOnce({
      ok: true,
      headers: new Headers({ ETag: '"outreach-v1"' }),
      json: async () => outreach,
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ ...outreach, stage: "touch3" }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [{ ...outreach, stage: "touch3" }] }) });
  render(<OutreachPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Edit partner@fund.vc" }));
  const stage = await screen.findByRole("textbox", { name: "Stage" });
  await userEvent.clear(stage);
  await userEvent.type(stage, "touch3");
  await userEvent.click(screen.getByRole("button", { name: "Save outreach" }));

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
  const [, put] = fetchMock.mock.calls[2] as [string, RequestInit];
  expect(put.method).toBe("PUT");
  expect((put.headers as Record<string, string>)["If-Match"]).toBe('"outreach-v1"');
});

it("keeps outreach editing fail-closed when the fresh version cannot be loaded", async () => {
  const outreach = {
    address: "partner@fund.vc",
    agent_email: "raise@example.com",
    stage: "stale-list-stage",
    next_action_at: "2026-07-28T17:00:00Z",
    replied: false,
    suppressed: false,
    outbound_count: 2,
    inbound_count: 0,
    contact: { display_name: "A. Partner", metadata: {} },
  };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [outreach] }) })
    .mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: async () => ({ error: { message: "Could not load the latest outreach state" } }),
    });
  render(<OutreachPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Edit partner@fund.vc" }));

  expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the latest outreach state");
  expect(screen.getByRole("textbox", { name: "Stage" })).toBeDisabled();
  expect(screen.getByLabelText("Next action")).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save outreach" })).toBeDisabled();
  expect(fetchMock).toHaveBeenCalledTimes(2);
});
