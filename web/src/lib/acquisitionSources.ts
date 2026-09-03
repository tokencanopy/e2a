// Answer set for the onboarding survey ("Where did you hear about e2a?").
// Values mirror internal/identity.AcquisitionSources and the CHECK in
// migration 108 exactly; labels are display-only and never stored.
// "skipped" is a valid value the page sends from the Skip action but is
// never offered as a choice.
export type AcquisitionSource =
  | "search"
  | "ai_assistant"
  | "github"
  | "x_twitter"
  | "hn_reddit"
  | "content"
  | "mcp_directory"
  | "word_of_mouth"
  | "other"
  | "skipped";

export const ACQUISITION_SOURCES: ReadonlyArray<{ value: AcquisitionSource; label: string }> = [
  { value: "search", label: "Search engine" },
  { value: "ai_assistant", label: "ChatGPT / Claude / another AI assistant" },
  { value: "github", label: "GitHub" },
  { value: "x_twitter", label: "X / Twitter" },
  { value: "hn_reddit", label: "Hacker News / Reddit" },
  { value: "content", label: "YouTube, podcast, or blog" },
  { value: "mcp_directory", label: "MCP directory" },
  { value: "word_of_mouth", label: "Friend or colleague" },
  { value: "other", label: "Other" },
];

// Server-enforced ceiling for the free-text detail (code points, trimmed).
export const ACQUISITION_DETAIL_MAX = 200;
