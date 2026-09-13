package ws

import "nhooyr.io/websocket"

// WebSocket close-code contract (GA-frozen; documented in docs/api.md
// "Real-time delivery (WebSocket)" → "Connection lifecycle & close codes").
//
// The server enforces one connection per agent: a newer connection for the
// same agent supersedes the current one. Before this contract, the replaced
// path reused 1008 (policy violation) — the same code a genuine rejection
// would use — so an SDK could not tell "your own newer connection took over
// (stop)" from "the server refuses you (stop)" from a transient drop
// (reconnect), and two of a user's own processes would steal the socket from
// each other in a reconnect loop.
//
//	code | reason token   | meaning                                   | client action
//	-----+----------------+-------------------------------------------+---------------------------
//	1000 | (empty)        | normal closure                            | none (don't reconnect)
//	1001 | shutting_down  | server shutdown/restart (deploy)          | reconnect with backoff
//	1001 | ping_timeout   | server dropped an unresponsive connection | reconnect with backoff
//	1008 | (message)      | genuine policy rejection after upgrade    | stop; do NOT reconnect
//	1011 | (message)      | internal server error                     | reconnect with backoff
//	4000 | replaced       | a newer connection for this agent         | stop; do NOT reconnect
//	     |                | superseded this one                       |
//	4001–4999               reserved for future e2a-specific fatal    | stop; do NOT reconnect
//	                         conditions (e.g. agent deleted)          |
//
// Reason strings on e2a-specific closes are short stable snake_case tokens
// ("replaced", "shutting_down", "ping_timeout") — safe to branch on, though
// clients should branch on the CODE first. Handshake rejections (bad key,
// wrong agent, cross-tenant) happen before the upgrade and are HTTP error
// envelopes (401/403/404), never close codes.
const (
	// StatusReplaced (4000) — this connection was superseded by a NEWER
	// connection for the same agent (one-connection-per-agent policy). This is
	// benign from the server's perspective, but the superseded client must NOT
	// auto-reconnect: reconnecting would steal the socket back from its
	// replacement and loop.
	StatusReplaced          websocket.StatusCode = 4000
	StatusCredentialRevoked websocket.StatusCode = 4001
)

// Stable close-reason tokens (the machine-readable part of the contract).
const (
	ReasonReplaced          = "replaced"
	ReasonShuttingDown      = "shutting_down"
	ReasonPingTimeout       = "ping_timeout"
	ReasonCredentialRevoked = "credential_revoked"
)
