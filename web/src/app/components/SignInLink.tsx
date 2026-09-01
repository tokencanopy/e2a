"use client";

import { useEffect, useState } from "react";
import {
  SIGN_IN_URL,
  SITE_URL,
  LEGACY_SIGN_IN_URL,
  signInURLWithReturnTo,
} from "../../lib/site";

/**
 * Detects in-app browsers (WebViews) that Google OAuth blocks.
 * Google rejects OAuth in WebViews with error 403: disallowed_useragent.
 */
function isInAppBrowser(): boolean {
  if (typeof navigator === "undefined") return false;
  const ua = navigator.userAgent || "";
  // Common in-app browser indicators
  return /FBAN|FBAV|Instagram|Line\/|Twitter|Snapchat|WeChat|MicroMessenger|LinkedIn/i.test(ua);
}

export function SignInLink({
  className,
  style,
  children,
  href = SIGN_IN_URL,
  preserveCurrentPaths,
}: {
  className?: string;
  style?: React.CSSProperties;
  children: React.ReactNode;
  href?: string;
  preserveCurrentPaths?: readonly string[];
}) {
  const [resolvedHref, setResolvedHref] = useState(href);

  useEffect(() => {
    if (!preserveCurrentPaths?.includes(window.location.pathname)) {
      setResolvedHref(href);
      return;
    }
    setResolvedHref(
      signInURLWithReturnTo(
        window.location.pathname + window.location.search + window.location.hash,
        href,
      ),
    );
  }, [href, preserveCurrentPaths]);

  const handleClick = (e: React.MouseEvent) => {
    if (isInAppBrowser()) {
      e.preventDefault();
      // Try to open in system browser on iOS/Android. Build via the URL
      // constructor so an absolute href (or one missing its leading slash)
      // can't produce a concatenated dead link.
      const url = new URL(resolvedHref, window.location.origin).href;
      // iOS: window.open in in-app browsers sometimes opens Safari
      window.open(url, "_blank");
      // Also show a message in case the open didn't work. The WebView block
      // is Google's, so only name Google when the legacy door is configured.
      const door =
        SIGN_IN_URL === LEGACY_SIGN_IN_URL ? "Google Sign-In" : "Sign in";
      alert(
        `${door} doesn't work in this browser. Please open ${SITE_URL} in Safari or Chrome to sign in.`
      );
    }
  };

  return (
    <a
      href={resolvedHref}
      className={className}
      style={style}
      onClick={handleClick}
    >
      {children}
    </a>
  );
}
