import { appendUniqueByAddress, encodeAddressSegment } from "./suppressionPath";

describe("encodeAddressSegment", () => {
  // The reason this guard exists: encodeURIComponent leaves "." untouched, so
  // an all-dots value survives escaping and the URL parser then resolves it as
  // a relative path segment, retargeting the request at the PARENT resource.
  it.each(["..", ".", "...", "  ..  ", "", "   "])(
    "rejects %p, which would collapse the path onto the parent resource",
    (address) => {
      expect(() => encodeAddressSegment(address)).toThrow(/not a valid address/i);
    },
  );

  // The guard must stay surgical: dots are legal and common inside real
  // addresses, so a broader rule would break ordinary recipients.
  it.each([
    "first.last@example.com",
    "a.b.c@sub.example.co.uk",
    "..leading@example.com",
    "trailing..@example.com",
  ])("accepts %p — dots inside a real address are untouched", (address) => {
    expect(() => encodeAddressSegment(address)).not.toThrow();
  });

  it("percent-encodes the separators that would otherwise escape the segment", () => {
    expect(encodeAddressSegment("a@b.com")).toBe("a%40b.com");
    expect(encodeAddressSegment("we/ird?x=1#f@b.com")).toBe("we%2Fird%3Fx%3D1%23f%40b.com");
    // One segment: no unencoded slash can introduce a new path level.
    expect(encodeAddressSegment("a/b@c.com")).not.toContain("/");
  });

  it("trims surrounding whitespace rather than encoding it into the path", () => {
    expect(encodeAddressSegment("  a@b.com  ")).toBe("a%40b.com");
  });
});

describe("appendUniqueByAddress", () => {
  const row = (address: string) => ({ address, source: "bounce" });

  it("appends only rows whose address is not already displayed", () => {
    const current = [row("a@x.net"), row("b@x.net")];
    const merged = appendUniqueByAddress(current, [row("b@x.net"), row("c@x.net")]);
    expect(merged.map((r) => r.address)).toEqual(["a@x.net", "b@x.net", "c@x.net"]);
  });

  it("keeps the already-rendered row rather than the incoming duplicate", () => {
    const current = [{ address: "a@x.net", source: "bounce" }];
    const merged = appendUniqueByAddress(current, [{ address: "a@x.net", source: "manual" }]);
    expect(merged).toHaveLength(1);
    expect(merged[0].source).toBe("bounce");
  });

  it("handles an empty page and an empty starting list", () => {
    expect(appendUniqueByAddress([], [row("a@x.net")])).toHaveLength(1);
    expect(appendUniqueByAddress([row("a@x.net")], [])).toHaveLength(1);
  });
});
