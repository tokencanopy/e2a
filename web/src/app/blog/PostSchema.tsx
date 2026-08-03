import { JsonLd } from "../components/JsonLd";
import { blogPosting, breadcrumbs } from "../../lib/jsonld";
import type { Post } from "./posts";

/**
 * Per-post structured data. Without this a post inherits only the root
 * SoftwareApplication node, so search and answer engines see a product page
 * where an article is — no byline, no publish date, no article body signal.
 */
export function PostSchema({ post }: { post: Post }) {
  return (
    <JsonLd
      data={[
        blogPosting(post),
        breadcrumbs([
          { name: "e2a", path: "/" },
          { name: "Blog", path: "/blog" },
          { name: post.title, path: `/blog/${post.slug}` },
        ]),
      ]}
    />
  );
}
