import { render, screen } from "@testing-library/react";
import { DNSRecord } from "./DnsRecord";

// Regression guard for the Domains DNS-records overflow bug: a long, unbreakable
// value (e.g. a DKIM public key) rendered in the copyable <code> used to push
// past the card boundary because the flex child had no min-width:0 and the code
// never wrapped. The fix adds `min-w-0` + `break-all`. These tests assert the
// overflow-guard classes are present so the layout can't silently regress.
const LONG_DKIM =
  "v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAr11Mo7JbCOJrRliNihNbbmQVmxOGK3vT0kgm6Ad/bfgHguuNMn5Ku4GK6ByKdFxx2Ss7vqzPZkjg+PgHUTJpfi/zm88wU3jKfThisKeepsGoingAndGoingForAVeryLongTimeIndeed";

describe("DNSField — long value overflow guard", () => {
  it("renders the long value verbatim and keeps it copyable", () => {
    render(
      <DNSRecord
        type="TXT"
        label="Authenticate outbound mail (DKIM)"
        fields={[{ label: "Content", value: LONG_DKIM }]}
      />,
    );
    expect(screen.getByText(LONG_DKIM)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy" })).toBeInTheDocument();
  });

  it("wraps long values via break-all and a shrinkable flex child (min-w-0)", () => {
    render(
      <DNSRecord
        type="TXT"
        label="DKIM"
        fields={[{ label: "Content", value: LONG_DKIM }]}
      />,
    );
    const code = screen.getByText(LONG_DKIM);
    // The value-bearing <code> must be allowed to wrap and shrink, otherwise it
    // overflows its container on long input.
    expect(code).toHaveClass("break-all");
    expect(code).toHaveClass("min-w-0");
    expect(code).toHaveClass("flex-1");

    // Its flex parent must also be shrinkable (flexbox default min-width:auto
    // would otherwise let the code push the row wider than the card).
    expect(code.parentElement).toHaveClass("min-w-0");
  });
});
