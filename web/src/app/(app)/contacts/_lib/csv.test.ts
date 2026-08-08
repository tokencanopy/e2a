import { getCsvMetadataColumns, mapCsvRows, parseCsv } from "./csv";

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

  it("reports metadata columns with first-row samples", () => {
    const table = parseCsv(
      "email,name,company,role,notes\npartner@example.com,A. Partner,Example Capital,GP,met at demo day\n",
    );

    expect(getCsvMetadataColumns(table, "email", "name")).toEqual([
      { name: "company", sampleValue: "Example Capital" },
      { name: "role", sampleValue: "GP" },
      { name: "notes", sampleValue: "met at demo day" },
    ]);
  });

  it("updates metadata reporting when the selected name column changes", () => {
    const table = parseCsv("email,name,company\npartner@example.com,A. Partner,Example Capital\n");

    expect(getCsvMetadataColumns(table, "email", "name")).toEqual([
      { name: "company", sampleValue: "Example Capital" },
    ]);
    expect(getCsvMetadataColumns(table, "email", "company")).toEqual([
      { name: "name", sampleValue: "A. Partner" },
    ]);
    expect(mapCsvRows(table, "email", "company")).toEqual([{
      address: "partner@example.com",
      display_name: "Example Capital",
      metadata: { name: "A. Partner" },
    }]);
  });

  it("reports no metadata columns and skips empty headers", () => {
    const onlyIdentity = parseCsv("email,name\npartner@example.com,A. Partner\n");
    const withEmptyHeader = parseCsv("email,name, ,role\npartner@example.com,A. Partner,ignored,GP\n");

    expect(getCsvMetadataColumns(onlyIdentity, "email", "name")).toEqual([]);
    expect(getCsvMetadataColumns(withEmptyHeader, "email", "name")).toEqual([
      { name: "role", sampleValue: "GP" },
    ]);
    expect(mapCsvRows(withEmptyHeader, "email", "name")).toEqual([{
      address: "partner@example.com",
      display_name: "A. Partner",
      metadata: { role: "GP" },
    }]);
  });

  it("keeps mapping and reporting aligned when the selected email header is empty", () => {
    const table = parseCsv(",alias,company\n1,A. Partner,Example Capital\n");

    expect(getCsvMetadataColumns(table, "")).toEqual([
      { name: "alias", sampleValue: "A. Partner" },
      { name: "company", sampleValue: "Example Capital" },
    ]);
    expect(mapCsvRows(table, "")).toEqual([{
      address: "1",
      metadata: { alias: "A. Partner", company: "Example Capital" },
    }]);
  });
});
