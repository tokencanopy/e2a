"use client";

// Backward compatibility for bookmarks and previously delivered links to the
// retired standalone message-focus UI. Pending links converge on the canonical
// account-wide Review row; ordinary message links fall back to the owning
// threaded inbox, where message bodies, details, and lifecycle render inline.

import { Suspense, useEffect } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { getMessageDetailWire } from "../../../../../components/onboarding/api";
import { encodeThreadFragment } from "../../../../../components/messages/threading";

function LegacyMessageFocusRedirectContent() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";
  const id = searchParams.get("id") ?? "";
  const pending = searchParams.get("pending") === "1";

  useEffect(() => {
    let cancelled = false;
    const inboxURL = email
      ? `/inboxes/messages?email=${encodeURIComponent(email)}`
      : "/inboxes";

    if (pending && id) {
      router.replace(`/reviews?id=${encodeURIComponent(id)}`);
      return () => {
        cancelled = true;
      };
    }
    if (!email) {
      router.replace("/inboxes");
      return () => {
        cancelled = true;
      };
    }
    if (!id) {
      router.replace(inboxURL);
      return () => {
        cancelled = true;
      };
    }

    void getMessageDetailWire(email, id)
      .then((message) => {
        if (cancelled) return;
        const key =
          message.thread_id != null
            ? `thr:${message.thread_id}`
            : message.conversation_id
              ? `conv:${message.conversation_id}`
              : `orphan:${message.id}`;
        router.replace(`${inboxURL}#${encodeThreadFragment(key)}`);
      })
      .catch(() => {
        if (!cancelled) router.replace(inboxURL);
      });

    return () => {
      cancelled = true;
    };
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
