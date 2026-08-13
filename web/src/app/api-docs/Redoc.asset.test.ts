import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

const publicDirectory = join(process.cwd(), "public");
const redocDirectory = join(publicDirectory, "vendor", "redoc");

function pageSources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return pageSources(path);
    return /\.(?:html|jsx|mdx|tsx)$/.test(entry.name) &&
      !/\.(?:test|spec)\.[jt]sx$/.test(entry.name)
      ? [path]
      : [];
  });
}

const ALLOWED_EXECUTABLE_SOURCES = new Set([
  "/vendor/redoc/redoc-v2.5.0.standalone.js",
  "{UMAMI_TRACKER_PATH}",
]);

function scriptStartTags(contents: string): string[] {
  const tags: string[] = [];
  const startPattern = /<script\b/gi;
  let start: RegExpExecArray | null;

  while ((start = startPattern.exec(contents)) !== null) {
    let braceDepth = 0;
    let quote = "";
    let escaped = false;

    for (let index = startPattern.lastIndex; index < contents.length; index += 1) {
      const character = contents[index];
      if (quote) {
        if (escaped) escaped = false;
        else if (character === "\\") escaped = true;
        else if (character === quote) quote = "";
        continue;
      }
      if (character === '"' || character === "'" || character === "`") {
        quote = character;
      } else if (character === "{") {
        braceDepth += 1;
      } else if (character === "}" && braceDepth > 0) {
        braceDepth -= 1;
      } else if (character === ">" && braceDepth === 0) {
        tags.push(contents.slice(start.index, index + 1));
        startPattern.lastIndex = index + 1;
        break;
      }
    }
  }

  return tags;
}

function scriptAttributes(tag: string): Array<[string, string]> {
  const attributes: Array<[string, string]> = [];
  let index = tag.search(/\bscript\b/i) + "script".length;

  while (index < tag.length) {
    while (/\s/.test(tag[index] ?? "")) index += 1;
    if (tag[index] === ">" || tag[index] === "/") break;

    const nameStart = index;
    while (index < tag.length && !/[\s=/>]/.test(tag[index])) index += 1;
    const name = tag.slice(nameStart, index).toLowerCase();
    if (!name) break;

    while (/\s/.test(tag[index] ?? "")) index += 1;
    let value = "";
    if (tag[index] === "=") {
      index += 1;
      while (/\s/.test(tag[index] ?? "")) index += 1;
      const valueStart = index;
      const opener = tag[index];

      if (opener === '"' || opener === "'" || opener === "`") {
        index += 1;
        let escaped = false;
        while (index < tag.length) {
          const character = tag[index];
          if (escaped) escaped = false;
          else if (character === "\\") escaped = true;
          else if (character === opener) {
            index += 1;
            break;
          }
          index += 1;
        }
        value = tag.slice(valueStart + 1, index - 1);
      } else if (opener === "{") {
        let depth = 0;
        let quote = "";
        let escaped = false;
        while (index < tag.length) {
          const character = tag[index];
          if (quote) {
            if (escaped) escaped = false;
            else if (character === "\\") escaped = true;
            else if (character === quote) quote = "";
          } else if (character === '"' || character === "'" || character === "`") {
            quote = character;
          } else if (character === "{") {
            depth += 1;
          } else if (character === "}") {
            depth -= 1;
            if (depth === 0) {
              index += 1;
              break;
            }
          }
          index += 1;
        }
        value = tag.slice(valueStart, index);
      } else {
        while (index < tag.length && !/[\s>]/.test(tag[index])) index += 1;
        value = tag.slice(valueStart, index);
      }
    }

    attributes.push([name, value]);
  }

  return attributes;
}

function executableSourceViolations(contents: string): string[] {
  const violations: string[] = [];
  const scriptTags = scriptStartTags(contents);

  for (const tag of scriptTags) {
    const parsedAttributes = scriptAttributes(tag);
    if (parsedAttributes.some(([name]) => name.startsWith("{..."))) {
      violations.push("script element with spread attributes");
      continue;
    }

    const sources = parsedAttributes
      .filter(([name]) => name === "src")
      .map(([, value]) => value);
    if (sources.length > 1) {
      violations.push("script element with duplicate src attributes");
      continue;
    }

    const source = sources[0];
    if (source !== undefined && !ALLOWED_EXECUTABLE_SOURCES.has(source)) {
      violations.push(`unapproved script source: ${source}`);
    }
  }

  if (/\b(?:document\.)?createElement\(\s*["'`]script["'`]\s*\)/i.test(contents)) {
    violations.push("runtime-created script element");
  }

  return violations;
}

describe("vendored Redoc runtime", () => {
  it("pins the reviewed bundle bytes with its upstream MIT license", () => {
    const manifest = JSON.parse(
      readFileSync(join(redocDirectory, "manifest.json"), "utf8"),
    ) as {
      bytes: number;
      license: string;
      name: string;
      notices: string;
      noticesBytes: number;
      noticesSha256: string;
      sha256: string;
      source: string;
      upstream: string;
      upstreamCommit: string;
      version: string;
    };
    const bundle = readFileSync(
      join(redocDirectory, "redoc-v2.5.0.standalone.js"),
    );
    const license = readFileSync(join(redocDirectory, "LICENSE.txt"));
    const notices = readFileSync(
      join(redocDirectory, "redoc.standalone.js.LICENSE.txt"),
    );

    expect(manifest).toMatchObject({
      bytes: 910994,
      license: "MIT",
      name: "Redoc standalone",
      notices: "redoc.standalone.js.LICENSE.txt",
      noticesBytes: 2729,
      noticesSha256:
        "1b18b986225f8a85fa7fcaf191f0118bb297ba8c6f027b669e4b79828c9c17ed",
      sha256: "0ec05be285ac885a330289b02f470e1bdbd2b6b3223a9fa213f24bf805a851d1",
      source: "https://cdn.redoc.ly/redoc/v2.5.0/bundles/redoc.standalone.js",
      upstream: "https://github.com/Redocly/redoc/releases/tag/v2.5.0",
      upstreamCommit: "00bc6edfc42c9cec9e453a2af4a8f5cef5e033ca",
      version: "2.5.0",
    });
    expect(bundle).toHaveLength(910994);
    expect(createHash("sha256").update(bundle).digest("hex")).toBe(
      "0ec05be285ac885a330289b02f470e1bdbd2b6b3223a9fa213f24bf805a851d1",
    );
    expect(license).toHaveLength(1091);
    expect(createHash("sha256").update(license).digest("hex")).toBe(
      "d3026d549cf68ab7355bcfa85877bf8f845b3334a7efbfdc63936432fb34ff0e",
    );
    expect(license.toString("utf8").startsWith("The MIT License (MIT)")).toBe(
      true,
    );
    expect(license.toString("utf8")).toContain(
      "Copyright (c) 2015-present, Rebilly, Inc.",
    );
    expect(notices).toHaveLength(2729);
    expect(createHash("sha256").update(notices).digest("hex")).toBe(
      "1b18b986225f8a85fa7fcaf191f0118bb297ba8c6f027b669e4b79828c9c17ed",
    );
    expect(notices.toString("utf8")).toContain("@license DOMPurify 3.2.4");
    expect(notices.toString("utf8")).toContain("@license React");
  });

  it("loads executable JavaScript only from the same origin", () => {
    const scalar = readFileSync(join(publicDirectory, "scalar.html"), "utf8");

    expect(scalar).toContain(
      '<script src="/vendor/redoc/redoc-v2.5.0.standalone.js"></script>',
    );
    expect(scalar).not.toMatch(/<script\b[^>]*\bsrc=["']https?:\/\//i);

    const sourceViolations = [
      ...pageSources(publicDirectory),
      ...pageSources(join(process.cwd(), "src", "app")),
    ].flatMap((path) => executableSourceViolations(readFileSync(path, "utf8")));
    expect(sourceViolations).toEqual([]);
  });

  it("rejects alternate remote and dynamically-created script forms", () => {
    const unsafeSources = [
      '<script src="https://cdn.example.test/a.js">',
      "<script src='//cdn.example.test/a.js'>",
      "<script\n defer\n src=https://cdn.example.test/a.js>",
      '<script data-note=">" src="https://cdn.example.test/a.js">',
      '<Script src={REMOTE_TRACKER_URL} />',
      '<Script onReady={() => ready()} src={REMOTE_TRACKER_URL} />',
      '<Script src={REMOTE_TRACKER_URL} onReady={() => { const src="/vendor/redoc/redoc-v2.5.0.standalone.js"; ready(); }} />',
      '<Script {...remoteScriptProps} />',
      'document.createElement("script")',
    ];

    for (const source of unsafeSources) {
      expect(executableSourceViolations(source)).not.toEqual([]);
    }
  });
});
