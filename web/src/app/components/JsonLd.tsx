import type { JsonLdNode } from "../../lib/jsonld";

/**
 * Renders one or more JSON-LD nodes as <script type="application/ld+json">.
 *
 * The `<` escape guards against a string field (a post title, an FAQ answer)
 * closing the script tag early — JSON.stringify does not escape it, and the
 * browser's HTML parser ends the script at the first literal `</script`.
 */
export function JsonLd({ data }: { data: JsonLdNode | JsonLdNode[] }) {
  const nodes = Array.isArray(data) ? data : [data];
  return (
    <>
      {nodes.map((node, i) => (
        <script
          key={i}
          type="application/ld+json"
          dangerouslySetInnerHTML={{
            __html: JSON.stringify(node).replace(/</g, "\\u003c"),
          }}
        />
      ))}
    </>
  );
}
