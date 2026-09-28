import { ACCOUNT_READ_ONLY_MESSAGE, readOnlyMessageFromBody } from "./readOnly";

describe("readOnlyMessageFromBody", () => {
  it("maps the account_read_only envelope to the dashboard copy", () => {
    const body = JSON.stringify({
      error: { code: "account_read_only", message: "sending is paused for this account pending an abuse review" },
    });
    expect(readOnlyMessageFromBody(body)).toBe(ACCOUNT_READ_ONLY_MESSAGE);
  });

  it("ignores other codes, non-JSON and empty bodies", () => {
    expect(readOnlyMessageFromBody(JSON.stringify({ error: { code: "forbidden" } }))).toBeNull();
    expect(readOnlyMessageFromBody("not authenticated")).toBeNull();
    expect(readOnlyMessageFromBody("")).toBeNull();
  });
});
