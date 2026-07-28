export function parseCsv(input: string): string[][] {
  const text = input.replace(/^\uFEFF/, "");
  const rows: string[][] = [];
  let row: string[] = [];
  let field = "";
  let quoted = false;
  for (let index = 0; index < text.length; index++) {
    const char = text[index];
    if (quoted) {
      if (char === '"') {
        if (text[index + 1] === '"') {
          field += '"';
          index++;
        } else {
          quoted = false;
        }
      } else {
        field += char;
      }
    } else if (char === '"' && field === "") {
      quoted = true;
    } else if (char === ",") {
      row.push(field);
      field = "";
    } else if (char === "\n") {
      row.push(field);
      rows.push(row);
      row = [];
      field = "";
    } else if (char !== "\r") {
      field += char;
    }
  }
  if (quoted) throw new Error("CSV has an unterminated quoted field");
  if (field !== "" || row.length > 0) {
    row.push(field);
    rows.push(row);
  }
  return rows.filter((values) => values.some((value) => value.trim() !== ""));
}

export type ImportPreviewRow = {
  address: string;
  display_name?: string;
  metadata?: Record<string, string>;
};

export function mapCsvRows(
  table: string[][],
  emailColumn: string,
  nameColumn?: string,
): ImportPreviewRow[] {
  if (table.length < 2) throw new Error("CSV must include a header and at least one row");
  const headers = table[0].map((header) => header.trim());
  const emailIndex = headers.indexOf(emailColumn);
  if (emailIndex < 0) throw new Error(`No “${emailColumn}” column found`);
  const nameIndex = nameColumn ? headers.indexOf(nameColumn) : -1;
  if (nameColumn && nameIndex < 0) throw new Error(`No “${nameColumn}” column found`);
  const rows = table.slice(1).map((values) => {
    const row: ImportPreviewRow = { address: (values[emailIndex] ?? "").trim() };
    if (nameIndex >= 0) row.display_name = values[nameIndex] ?? "";
    const metadata: Record<string, string> = {};
    headers.forEach((header, index) => {
      if (header && index !== emailIndex && index !== nameIndex) {
        metadata[header] = values[index] ?? "";
      }
    });
    if (Object.keys(metadata).length > 0) row.metadata = metadata;
    return row;
  });
  if (rows.length > 1000) throw new Error("Imports are limited to 1,000 rows");
  return rows;
}
