// Exit codes are the CLI's scripting contract. Harness scripts (tether, hooks,
// CI lanes) branch on these instead of parsing output, so the values are
// frozen once published — add new codes, never renumber.
//
// HELD exists because a review-held send is an HTTP 2xx success with response
// status `pending_review`, but the recipient got nothing. Scripts that treat
// every 2xx as success would silently report into a queue nobody reads — the
// exact failure mode that bit the tether harness twice. The distinct async
// response status `accepted` means the message is durably queued for delivery
// and exits OK. The CLI branches on the response body's status, not HTTP status.
// A terminal failed or unknown 2xx outcome gets its own non-retry exit: the
// returned message id proves the server created an observable result, so a
// fresh retry could send a duplicate. Callers should inspect that message.
export const EXIT = {
  OK: 0,
  /** Network, server, or unexpected error. */
  ERROR: 1,
  /** Bad flags or arguments. */
  USAGE: 2,
  /** Send held for review (pending_review) — NOT delivered. */
  HELD: 3,
  /** Bad credentials or wrong key scope for the operation. */
  AUTH: 4,
  /**
   * Permanent request error (404 / 409 / 422 …) — retrying the identical
   * invocation cannot succeed. Distinguished from ERROR so retry-on-1
   * wrappers don't hammer a typo'd message id or an unverified domain.
   */
  REQUEST: 5,
  /** A deadline-bounded wait (`listen --once --until`) expired unmatched. */
  TIMEOUT: 6,
  /** A persisted send failed or returned an unknown outcome; do not retry. */
  SEND_OUTCOME: 7,
  /**
   * Diagnostics (`doctor`) completed with warnings only — nothing is broken,
   * but something deserves attention. Distinct from OK so CI can gate on a
   * fully clean environment without parsing the report.
   */
  WARN: 8,
  /**
   * Diagnostics found a definite configuration failure (missing DNS record,
   * unregistered domain, auto-disabled webhook, wrong deployment URL).
   * Distinct from ERROR (1) so retry-on-transient wrappers don't loop on a
   * problem that only a human fixing configuration can resolve, and from
   * REQUEST (5) because nothing about the invocation itself was wrong.
   */
  CONFIG: 9,
  /**
   * The account is read-only (`account_read_only`): its sending is paused
   * pending an abuse review, so every write is refused. Distinct from AUTH
   * (the credential is fine) and REQUEST (nothing about the invocation was
   * wrong): no write will succeed until an operator resumes the account, so
   * wrappers must neither retry nor rotate keys — contact support.
   */
  READ_ONLY: 10,
} as const;

export function exitCodeForAPIError(error: { code: string; retryable: boolean }): number {
  if (error.code === "unauthorized" || error.code === "forbidden") return EXIT.AUTH;
  if (error.code === "account_read_only") return EXIT.READ_ONLY;
  return error.retryable ? EXIT.ERROR : EXIT.REQUEST;
}

/** Write a message to stderr and exit with the given code. */
export function fail(code: number, message: string): never {
  process.stderr.write(message.endsWith("\n") ? message : message + "\n");
  process.exit(code);
}
