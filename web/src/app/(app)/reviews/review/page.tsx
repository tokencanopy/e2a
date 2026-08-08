"use client";

// The standalone /reviews/review route was folded into the
// consolidated queue at /reviews. This page exists only to redirect
// callers that have the old URL bookmarked or follow it from a stale
// notification. The destination preserves the ?id= query so context
// isn't lost.

import { Suspense, useEffect } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { PageShell } from "../../../components/loft/PageShell";

function ReviewRedirect() {
  const router = useRouter();
  const params = useSearchParams();
  useEffect(() => {
    const id = params.get("id");
    router.replace(
      id ? `/reviews?id=${encodeURIComponent(id)}` : "/reviews",
    );
  }, [router, params]);
  return (
    <PageShell>
      <div
        className="text-[13px] py-12 text-center"
        style={{ color: "var(--fg-muted)" }}
      >
        Redirecting…
      </div>
    </PageShell>
  );
}

export default function ReviewPage() {
  return (
    <Suspense fallback={null}>
      <ReviewRedirect />
    </Suspense>
  );
}
