jest.mock("@e2a/ui/styles.css", () => ({}), { virtual: true });
jest.mock("./globals.css", () => ({}), { virtual: true });
jest.mock("next/font/local", () => ({
  __esModule: true,
  default: () => ({ variable: "mock-font" }),
}));

import { metadata } from "./layout";

describe("root metadata", () => {
  it("uses the canonical open-source email API positioning", () => {
    expect(metadata.title).toMatchObject({
      default: "e2a | The open-source email API for applications and AI agents",
    });
    expect(metadata.description).toBe(
      "Send transactional email from any product, give agents real two-way inboxes, and keep people in control. Use e2a as a hosted service or run the Apache-2.0 stack yourself.",
    );
    expect(metadata.openGraph).toMatchObject({
      title: "e2a | The open-source email API for applications and AI agents",
      description:
        "Send transactional email from any product, give agents real two-way inboxes, and keep people in control. Use e2a as a hosted service or run the Apache-2.0 stack yourself.",
    });
  });
});
