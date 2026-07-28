import { mapCsvRows, parseCsv } from "./csv";

describe("contacts CSV parsing", () => {
  it("handles BOM, quoted commas, escaped quotes, and newlines", () => {
    expect(parseCsv('\uFEFFemail,name,notes\r\n"a@x.com","Doe, Jane","said ""yes"""\r\n'))
      .toEqual([
        ["email", "name", "notes"],
        ["a@x.com", "Doe, Jane", 'said "yes"'],
      ]);
  });

  it("maps selected columns and preserves the rest as metadata", () => {
    expect(mapCsvRows(
      parseCsv("Email,Full name,Fund\npartner@fund.vc,A. Partner,Example Capital\n"),
      "Email",
      "Full name",
    )).toEqual([{
      address: "partner@fund.vc",
      display_name: "A. Partner",
      metadata: { Fund: "Example Capital" },
    }]);
  });
});
