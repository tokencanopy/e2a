// Structured-data builders for the marketing surface.
//
// Two audiences read this output and they want different things. Search engines
// use it for rich results (breadcrumbs, FAQ accordions, article bylines).
// Answer engines use it as a pre-parsed summary of the page — a question/answer
// pair in FAQPage schema is the single most quotable unit we can publish,
// because it needs no extraction step.
//
// Every builder returns a plain object. Render it with <JsonLd data={...} />.
// Keep the shapes minimal: a field we cannot keep true is worse than an absent
// one, since stale structured data is a manual-action risk on the SEO side and
// a wrong-answer risk on the AEO side.

import { SITE_NAME, SITE_URL } from "./site";

/** Loose JSON-LD node. Values are whatever schema.org allows at that key. */
export type JsonLdNode = Record<string, unknown>;

/** Absolute URL for a site-relative path. Schema.org wants absolute URLs. */
function abs(path: string): string {
  return path.startsWith("http") ? path : `${SITE_URL}${path}`;
}

/**
 * The publisher behind every page. Referenced by @id from other nodes so the
 * organization is described once rather than duplicated per page.
 */
export const ORGANIZATION_ID = `${SITE_URL}/#organization`;

export function organization(): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "Organization",
    "@id": ORGANIZATION_ID,
    name: SITE_NAME,
    url: SITE_URL,
    logo: abs("/logo-wordmark.svg"),
    description:
      "e2a is the open-source email API for applications and AI agents. It sends transactional email from any product, gives agents real two-way inboxes, and returns structured SPF, DKIM, and DMARC evidence for inbound mail. That evidence applies to the From domain, not a person, mailbox, or content.",
    sameAs: [
      "https://github.com/tokencanopy/e2a",
      "https://www.npmjs.com/package/@e2a/sdk",
      "https://pypi.org/project/e2a/",
    ],
    parentOrganization: {
      "@type": "Organization",
      name: "Token Canopy",
      url: "https://tokencanopy.com",
    },
  };
}

export function website(): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "WebSite",
    "@id": `${SITE_URL}/#website`,
    name: SITE_NAME,
    url: SITE_URL,
    publisher: { "@id": ORGANIZATION_ID },
    inLanguage: "en-US",
  };
}

/**
 * The product node's stable @id, for the same reason ORGANIZATION_ID exists:
 * the pricing page is served from the hosted deployment's own repo, and it
 * attaches its paid-tier Offer nodes to this @id. Without one, that reference
 * dangles and the two pages describe two competing SoftwareApplications
 * instead of one.
 */
export const SOFTWARE_ID = `${SITE_URL}/#software`;
export const FREE_OFFER_ID = `${SITE_URL}/#offer-free`;

export function softwareApplication(description: string): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "SoftwareApplication",
    "@id": SOFTWARE_ID,
    name: SITE_NAME,
    description,
    url: SITE_URL,
    applicationCategory: "DeveloperApplication",
    operatingSystem: "Any",
    publisher: { "@id": ORGANIZATION_ID },
    // The free tier is real and requires no card, so a zero-price Offer is
    // accurate here. Dollar amounts are deliberately NOT restated in this
    // file: paid tiers belong to the pricing page, which is not part of this
    // app (see PRICING_PATH in ./site) and describes them against SOFTWARE_ID.
    // Duplicating them here is how structured data drifts out of sync with
    // billing — and a deployment that overrides SITE_URL may price differently
    // or not sell at all.
    offers: {
      "@type": "Offer",
      "@id": FREE_OFFER_ID,
      price: "0",
      priceCurrency: "USD",
    },
  };
}

export type BlogPostingInput = {
  title: string;
  description: string;
  /** ISO date string (YYYY-MM-DD). */
  date: string;
  author: string;
  slug: string;
};

export function blogPosting(post: BlogPostingInput): JsonLdNode {
  const published = new Date(`${post.date}T00:00:00Z`).toISOString();
  const url = abs(`/blog/${post.slug}`);
  return {
    "@context": "https://schema.org",
    "@type": "BlogPosting",
    headline: post.title,
    description: post.description,
    datePublished: published,
    // We do not track per-post revisions, so modified mirrors published rather
    // than claiming a freshness we cannot substantiate.
    dateModified: published,
    author: { "@type": "Organization", name: post.author, url: SITE_URL },
    publisher: { "@id": ORGANIZATION_ID },
    url,
    mainEntityOfPage: { "@type": "WebPage", "@id": url },
    image: abs("/og-image.png"),
    inLanguage: "en-US",
  };
}

export type FaqEntry = { question: string; answer: string };

/**
 * FAQPage schema. The `answer` strings should be self-contained prose — an
 * answer engine may quote one in isolation, with no surrounding page context.
 */
export function faqPage(entries: readonly FaqEntry[]): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "FAQPage",
    mainEntity: entries.map((e) => ({
      "@type": "Question",
      name: e.question,
      acceptedAnswer: { "@type": "Answer", text: e.answer },
    })),
  };
}

export type HowToStepInput = { name: string; text: string };

export function howTo(input: {
  name: string;
  description: string;
  steps: readonly HowToStepInput[];
}): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "HowTo",
    name: input.name,
    description: input.description,
    step: input.steps.map((s, i) => ({
      "@type": "HowToStep",
      position: i + 1,
      name: s.name,
      text: s.text,
    })),
  };
}

export type Crumb = { name: string; path: string };

export function breadcrumbs(crumbs: readonly Crumb[]): JsonLdNode {
  return {
    "@context": "https://schema.org",
    "@type": "BreadcrumbList",
    itemListElement: crumbs.map((c, i) => ({
      "@type": "ListItem",
      position: i + 1,
      name: c.name,
      item: abs(c.path),
    })),
  };
}
