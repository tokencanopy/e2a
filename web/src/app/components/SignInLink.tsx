"use client";

import { SIGN_IN_URL, SITE_URL, LEGACY_SIGN_IN_URL } from "../../lib/site";

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
}: {
  className?: string;
  style?: React.CSSProperties;
  children: React.ReactNode;
}) {
  const handleClick = (e: React.MouseEvent) => {
    if (isInAppBrowser()) {
      e.preventDefault();
      // Try to open in system browser on iOS/Android. Build via the URL
      // constructor so an operator-supplied absolute SIGN_IN_URL (or one
      // missing its leading slash) can't produce a concatenated dead link.
      const url = new URL(SIGN_IN_URL, window.location.origin).href;
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
      href={SIGN_IN_URL}
      className={className}
      style={style}
      onClick={handleClick}
    >
      {children}
    </a>
  );
}
