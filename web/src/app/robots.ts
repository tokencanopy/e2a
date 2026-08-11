import type { MetadataRoute } from "next";
import { SITE_URL } from "../lib/site";

// Required for static export (output: "export")
export const dynamic = "force-static";

export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      {
        userAgent: "*",
        allow: "/",
        disallow: [
          "/api/",
          "/api-keys",
          "/billing",
          "/contacts",
          "/dashboard",
          "/domains",
          "/feedback",
          "/get-started",
          "/inboxes",
          "/metrics",
          "/reviews",
          "/settings",
          "/templates",
          "/trash",
          "/webhook-secrets",
          "/webhooks",
        ],
      },
    ],
    sitemap: `${SITE_URL}/sitemap.xml`,
    host: SITE_URL,
  };
}
