// Address → URL path segment.
//
// `encodeURIComponent` does NOT encode ".", so an address of "." or ".." (or
// any all-dots value) survives encoding and is then removed by the URL parser
// as a relative path segment BEFORE the request is sent. A DELETE aimed at
// .../suppressions/.. collapses onto the parent resource — the agent or the
// account — which are themselves real DELETE endpoints taking the very same
// ?confirm=DELETE token. The server rejects such addresses on create, and chi
// does not redirect the collapsed path today, so this is a latent hazard
// rather than a live one; encode it out at the boundary regardless instead of
// depending on either.
export function encodeAddressSegment(address: string): string {
  const trimmed = address.trim();
  if (!trimmed || /^\.+$/.test(trimmed)) {
    throw new Error(`"${address}" is not a valid address to act on`);
  }
  return encodeURIComponent(trimmed);
}

// Merge a freshly fetched page into the rows already displayed, keeping one
// entry per address. A server that re-emits a row across pages would otherwise
// produce duplicate React keys — and each duplicate carries its own Remove
// button.
export function appendUniqueByAddress<T extends { address: string }>(
  current: T[],
  incoming: T[],
): T[] {
  const seen = new Set(current.map((row) => row.address));
  return [...current, ...incoming.filter((row) => !seen.has(row.address))];
}
