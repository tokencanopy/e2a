import robots from "./robots";

describe("robots", () => {
  it("blocks every authenticated app route family from crawlers", () => {
    const rules = robots().rules;
    const rule = Array.isArray(rules) ? rules[0] : rules;

    expect(rule).toMatchObject({ userAgent: "*", allow: "/" });
    expect(rule.disallow).toEqual([
      "/api/",
      "/api-keys",
      "/billing",
      "/contacts",
      "/dashboard",
      "/domains",
      "/feedback",
      "/get-started",
      "/inboxes",
      "/metrics",
      "/reviews",
      "/settings",
      "/templates",
      "/trash",
      "/webhook-secrets",
      "/webhooks",
    ]);
  });
});
