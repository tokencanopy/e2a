"use client";

// Backward compatibility for bookmarks and previously delivered links to the
// retired standalone message-focus UI. Pending links converge on the canonical
// account-wide Review row; ordinary message links fall back to the owning
// threaded inbox, where message bodies, details, and lifecycle render inline.

import { Suspense, useEffect } from "react";
import { useRouter, useSearchParams } from "next/navigation";

function LegacyMessageFocusRedirectContent() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";
  const id = searchParams.get("id") ?? "";
  const pending = searchParams.get("pending") === "1";

  useEffect(() => {
    if (pending && id) {
      router.replace(`/reviews?id=${encodeURIComponent(id)}`);
      return;
    }
    if (email) {
      router.replace(`/inboxes/messages?email=${encodeURIComponent(email)}`);
      return;
    }
    router.replace("/inboxes");
  }, [email, id, pending, router]);

  return null;
}

export default function LegacyMessageFocusRedirect() {
  return (
    <Suspense fallback={null}>
      <LegacyMessageFocusRedirectContent />
    </Suspense>
  );
}
