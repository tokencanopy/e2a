import { METRIC_HELP } from "./metricDefinitions";

describe("metric help", () => {
  it("names loopback-adjusted external delivery denominators", () => {
    expect(METRIC_HELP.accepted).toMatch(/accepted minus loopback/i);
    expect(METRIC_HELP.submitted).toMatch(
      /upstream provider or the local loopback path/i,
    );
    expect(METRIC_HELP.submitted).toMatch(/submitted minus loopback/i);
    expect(METRIC_HELP.deliveredRate).toMatch(
      /delivered.*accepted minus loopback/i,
    );
    expect(METRIC_HELP.bounceRate).toMatch(/submitted minus loopback/i);
    expect(METRIC_HELP.complaintRate).toMatch(/complaints divided by delivered/i);
    expect(METRIC_HELP.suppressionBlockRate).toMatch(
      /accepted minus loopback/i,
    );
  });
});
