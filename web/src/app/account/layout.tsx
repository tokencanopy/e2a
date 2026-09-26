import type { Metadata } from "next";

// /account/* holds the restore interstitial and the sign-in refusal page.
// Both are reached only through a sign-in redirect and render per-session
// state, so keep them out of search indexes (the Umami tracker's public-path
// allowlist already leaves them untracked).
export const metadata: Metadata = {
  title: "Account",
  robots: {
    index: false,
    follow: false,
  },
};

export default function AccountLayout({ children }: { children: React.ReactNode }) {
  return children;
}
