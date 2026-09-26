// Full-document navigation, isolated so tests can mock it (jsdom does not
// implement window.location.assign).
//
// Use this instead of router.push when the destination must re-read the
// session from scratch: AuthProvider fetches /api/auth/me once per document,
// so a client-side transition after the session cookie changed (restore,
// sign-out) would keep rendering the stale signed-out state.
export function hardNavigate(url: string): void {
  window.location.assign(url);
}
