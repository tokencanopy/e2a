import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  getFlag,
  getFlags,
  hasFlag,
  parseArgs,
  getPositionals,
  hasBareFlag,
  checkFlags,
  getConversationId,
  USAGE,
} from "../bin/e2a.js";

describe("hasBareFlag", () => {
  it("finds a flag in normal position", () => {
    expect(hasBareFlag(["--json", "--help"], "--help")).toBe(true);
    expect(hasBareFlag(["--help"], "--help")).toBe(true);
  });

  it("does NOT match the flag when it is the value of a value-taking flag", () => {
    // `e2a send --body "--help"` must not hijack the command into help+exit 0.
    expect(hasBareFlag(["--body", "--help"], "--help")).toBe(false);
    expect(hasBareFlag(["--subject", "--version"], "--version")).toBe(false);
  });

  it("matches when preceded by a boolean flag", () => {
    expect(hasBareFlag(["--to", "a@b.c", "--json", "--help"], "--help")).toBe(true);
  });
});

describe("getPositionals", () => {
  it("extracts positionals, skipping value flags and their values", () => {
    expect(getPositionals(["msg_1", "--body", "hi", "--json"])).toEqual(["msg_1"]);
    expect(getPositionals(["--body", "hi", "msg_1"])).toEqual(["msg_1"]);
  });

  it("returns empty when only flags are present", () => {
    expect(getPositionals(["--body", "hi", "--json"])).toEqual([]);
  });

  it("rejects extra or missing operands when an exact count is required", () => {
    const mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    const mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
    try {
      expect(() => getPositionals(["key_a", "key_b"], 1, "usage: delete <id>")).toThrow(
        "process.exit",
      );
      expect(() => getPositionals([], 1, "usage: delete <id>")).toThrow("process.exit");
      expect(() => getPositionals(["unexpected"], 0, "usage: list")).toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      expect(mockStderr).toHaveBeenCalledWith("usage: delete <id>\n");
    } finally {
      mockExit.mockRestore();
      mockStderr.mockRestore();
    }
  });
});

describe("parseArgs", () => {
  it("extracts command and remaining args", () => {
    const result = parseArgs(["node", "e2a", "inbox", "--unread"]);
    expect(result).toEqual({ command: "inbox", args: ["--unread"] });
  });

  it("returns empty command when no args", () => {
    const result = parseArgs(["node", "e2a"]);
    expect(result).toEqual({ command: "", args: [] });
  });
});

describe("getFlag", () => {
  it("returns the value after the flag", () => {
    expect(getFlag(["--to", "a@b.com", "--subject", "Hi"], "--to")).toBe(
      "a@b.com",
    );
    expect(getFlag(["--to", "a@b.com", "--subject", "Hi"], "--subject")).toBe(
      "Hi",
    );
  });

  it("returns undefined when flag is missing", () => {
    expect(getFlag(["--to", "a@b.com"], "--subject")).toBeUndefined();
  });

  it("returns undefined when next token is another flag", () => {
    // Fix #14: --subject has no value, --body follows immediately
    expect(
      getFlag(["--subject", "--body", "hello"], "--subject"),
    ).toBeUndefined();
  });

  it("returns undefined when flag is last token", () => {
    expect(getFlag(["--to"], "--to")).toBeUndefined();
  });

  it("does not consume flags as values in a realistic send scenario", () => {
    const args = ["--to", "a@b.com", "--subject", "--body", "hello"];
    // --subject should be undefined (next token is --body)
    expect(getFlag(args, "--subject")).toBeUndefined();
    // --body should still get "hello"
    expect(getFlag(args, "--body")).toBe("hello");
    // --to should still work
    expect(getFlag(args, "--to")).toBe("a@b.com");
  });

  it("extracts a messages list filter expression verbatim", () => {
    expect(
      getFlag(["messages", "list", "--filter", "label:urgent"], "--filter"),
    ).toBe("label:urgent");
  });
});

describe("getFlags", () => {
  it("collects repeated flag values (used by --label, --to, --cc, etc.)", () => {
    expect(
      getFlags(["--label", "urgent", "--label", "follow-up"], "--label"),
    ).toEqual(["urgent", "follow-up"]);
  });

  it("returns an empty array when the flag is absent", () => {
    expect(getFlags(["--unread"], "--label")).toEqual([]);
  });

  it("returns a single-entry array for a single occurrence", () => {
    expect(getFlags(["--label", "urgent"], "--label")).toEqual(["urgent"]);
  });
});

describe("hasFlag", () => {
  it("returns true when flag is present", () => {
    expect(hasFlag(["--unread", "--limit", "5"], "--unread")).toBe(true);
  });

  it("returns false when flag is absent", () => {
    expect(hasFlag(["--unread"], "--read")).toBe(false);
  });
});

describe("--limit validation", () => {
  it("parseInt returns NaN for non-numeric input", () => {
    // This documents the behavior that the fix addresses.
    // The actual validation is in main() which calls process.exit,
    // so we test the underlying behavior here.
    const val = parseInt("abc", 10);
    expect(Number.isFinite(val)).toBe(false);
  });

  it("parseInt returns negative for negative input", () => {
    const val = parseInt("-5", 10);
    expect(val).toBe(-5);
    expect(val < 1).toBe(true);
  });
});

// checkFlags and getConversationId call process.exit on a usage error, so
// these describe blocks mock it the same way the command-level test files
// do (e.g. send.test.ts): make it throw so the invalid-input tests can
// assert via .toThrow, and restore it afterward so a passing suite doesn't
// pick up a stray exit code.
describe("checkFlags (FIX 1: single-dash flag typos)", () => {
  let mockStderr: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    mockStderr.mockRestore();
    mockExit.mockRestore();
  });

  it("rejects a single-dash long-flag typo instead of silently dropping it", () => {
    // Verified bug: `e2a messages list -limit 5 --agent x@y.com` used to
    // pass checkFlags silently (both -limit and the 5 were ignored) and
    // dump the whole mailbox at exit 0.
    expect(() => checkFlags(["-limit", "5", "--agent", "x@y.com"], ["--agent"])).toThrow(
      "process.exit",
    );
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("unknown flag: -limit"));
  });

  it("rejects other single-dash typos (-json, -once, -text)", () => {
    expect(() => checkFlags(["-json"], ["--json"])).toThrow("process.exit");
    expect(() => checkFlags(["-once"], ["--once"])).toThrow("process.exit");
    expect(() => checkFlags(["-text"], ["--text"])).toThrow("process.exit");
  });

  it("does NOT flag a dash-leading VALUE of an allowed non-boolean flag", () => {
    // CRITICAL: --subject and --body both take a value, so -weird and
    // -alsoweird are consumed as those values (via the loop's i++) and must
    // never reach the single-dash validation as tokens of their own.
    expect(() =>
      checkFlags(
        ["--subject", "-weird", "--body", "-alsoweird"],
        ["--subject", "--body"],
      ),
    ).not.toThrow();
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("still accepts a bare positional (no leading dash) alongside allowed flags", () => {
    expect(() => checkFlags(["msg_abc123", "--json"], ["--json"])).not.toThrow();
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("still rejects unknown --long-flags and --flag=value (pre-existing behavior)", () => {
    expect(() => checkFlags(["--bogus"], ["--json"])).toThrow("process.exit");
    expect(() => checkFlags(["--json=true"], ["--json"])).toThrow("process.exit");
  });

  it("rejects the retired legacy flag while accepting --filter", () => {
    const retiredFilterFlag = `--${"q"}`;
    expect(() => checkFlags([retiredFilterFlag, "label:urgent"], ["--filter"])).toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    mockExit.mockClear();

    expect(() => checkFlags(["--filter", "label:urgent"], ["--filter"])).not.toThrow();
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("rejects the removed login --with-key flag", () => {
    expect(() => checkFlags(["--with-key"], [])).toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("unknown flag: --with-key"));
  });
});

describe("-h / -v as subcommand flags (FIX 2)", () => {
  // main() reuses hasBareFlag(args, "-h") / hasBareFlag(args, "-v") — the
  // same flag-position-aware helper already used for --help/--version — so
  // `e2a whoami -h` short-circuits to help before whoami's API call, and
  // `e2a send -h --to …` short-circuits before any send is attempted.
  it("finds -h/-v in normal flag position on a subcommand", () => {
    expect(hasBareFlag(["-h"], "-h")).toBe(true);
    expect(hasBareFlag(["--json", "-h"], "-h")).toBe(true);
    expect(hasBareFlag(["-v"], "-v")).toBe(true);
    expect(hasBareFlag(["--to", "a@b.c", "-h"], "-h")).toBe(true);
  });

  it("does NOT match -h/-v when they are the VALUE of a value-taking flag", () => {
    expect(hasBareFlag(["--subject", "-h"], "-h")).toBe(false);
    expect(hasBareFlag(["--body", "-v"], "-v")).toBe(false);
  });
});

describe("scheduled send help", () => {
  it("labels --send-at as beta", () => {
    expect(USAGE).toMatch(/--send-at[\s\S]*beta[\s\S]*may change before stable/i);
  });
});

describe("getConversationId (FIX 3: --conversation-id / --conversation precedence)", () => {
  let mockStderr: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    mockStderr.mockRestore();
    mockExit.mockRestore();
  });

  it("returns the value when only one alias is given", () => {
    expect(getConversationId(["--conversation-id", "conv_1"])).toBe("conv_1");
    expect(getConversationId(["--conversation", "conv_1"])).toBe("conv_1");
  });

  it("returns undefined when neither alias is given", () => {
    expect(getConversationId(["--agent", "x@y.com"])).toBeUndefined();
  });

  it("accepts both aliases when they agree", () => {
    expect(
      getConversationId(["--conversation-id", "conv_1", "--conversation", "conv_1"]),
    ).toBe("conv_1");
  });

  it("errors instead of silently picking a winner when both aliases disagree", () => {
    // Verified bug: send resolved --conversation-id ?? --conversation while
    // messages list / listen resolved --conversation ?? --conversation-id —
    // opposite winners for the identical pair of flags. Precedence is now
    // shared across all three call sites via this one function, and a
    // conflicting pair is a usage error rather than a coin flip.
    expect(() =>
      getConversationId(["--conversation-id", "conv_A", "--conversation", "conv_B"]),
    ).toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockStderr).toHaveBeenCalledWith(
      expect.stringContaining("--conversation-id and --conversation are aliases"),
    );
  });
});
