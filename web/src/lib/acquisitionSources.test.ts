import { ACQUISITION_SOURCES, ACQUISITION_DETAIL_MAX } from "./acquisitionSources";

describe("acquisitionSources", () => {
  it("lists the nine visible options in server enum order, without skipped", () => {
    expect(ACQUISITION_SOURCES.map((o) => o.value)).toEqual([
      "search",
      "ai_assistant",
      "github",
      "x_twitter",
      "hn_reddit",
      "content",
      "mcp_directory",
      "word_of_mouth",
      "other",
    ]);
  });

  it("uses the agreed labels", () => {
    expect(ACQUISITION_SOURCES.map((o) => o.label)).toEqual([
      "Search engine",
      "ChatGPT / Claude / another AI assistant",
      "GitHub",
      "X / Twitter",
      "Hacker News / Reddit",
      "YouTube, podcast, or blog",
      "MCP directory",
      "Friend or colleague",
      "Other",
    ]);
  });

  it("caps detail at 200 characters", () => {
    expect(ACQUISITION_DETAIL_MAX).toBe(200);
  });
});
