package agent

import (
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/logredact"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// The magic-link flow is split into GET (render a confirmation page) and
// POST (execute the action). This keeps email-client URL scanners —
// Gmail, Outlook Safe Links, Slack unfurls, corporate mail gateways —
// from accidentally approving or rejecting on behalf of the reviewer
// when they preview the link. The reviewer has to actually load the
// page in a browser and click the confirm button.
//
// GET reads the token from ?t=; POST reads it from a hidden form field.
// The token is never transmitted in a way that lets a passive URL
// follower trigger the state change.

// --- GET confirmation pages ---

// ApproveMagicLinkHandler serves the /v1/approve magic-link endpoint as a
// self-contained handler — GET renders the token-gated confirmation page,
// POST executes the approval. The chi root (internal/httpapi) registers it
// directly; the legacy gorilla/mux no longer carries these routes.
func (a *API) ApproveMagicLinkHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.handleApproveMagicLinkGet(w, r)
		case http.MethodPost:
			a.handleApproveMagicLinkPost(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// RejectMagicLinkHandler is the /v1/reject counterpart of
// ApproveMagicLinkHandler.
func (a *API) RejectMagicLinkHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.handleRejectMagicLinkGet(w, r)
		case http.MethodPost:
			a.handleRejectMagicLinkPost(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (a *API) handleApproveMagicLinkGet(w http.ResponseWriter, r *http.Request) {
	a.renderConfirmPage(w, r, approvaltoken.ActionApprove)
}

func (a *API) handleRejectMagicLinkGet(w http.ResponseWriter, r *http.Request) {
	a.renderConfirmPage(w, r, approvaltoken.ActionReject)
}

// renderConfirmPage validates the token, loads the pending message, and
// renders an HTML page with a POST form targeting the same endpoint.
// The body preview lives on this page — not in the notification email —
// so sensitive content stays behind a token-gated server render rather
// than passing through the reviewer's mail infrastructure.
func (a *API) renderConfirmPage(w http.ResponseWriter, r *http.Request, endpointAction string) {
	if a.approvalSigner == nil {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("t")
	claims, status, errMsg := a.verifyMagicToken(token, endpointAction)
	if errMsg != "" {
		writeMagicMessage(w, status, pageTitleForAction(endpointAction, "Invalid link"), errMsg)
		return
	}

	// Load message via ownership-scoped read so the confirmation page
	// surfaces the real detail (recipients, subject, body preview).
	userID, _, err := a.store.ResolveOutboundOwner(r.Context(), claims.MessageID)
	if err != nil {
		writeMagicMessage(w, http.StatusNotFound, "Message not found",
			"This message no longer exists.")
		return
	}
	msg, err := a.store.GetOutboundMessageForUser(r.Context(), claims.MessageID, userID)
	if err != nil {
		writeMagicMessage(w, http.StatusNotFound, "Message not found",
			"This message no longer exists.")
		return
	}
	if msg.Status != identity.MessageStatusPendingReview {
		writeMagicMessage(w, http.StatusConflict, "Already resolved",
			"This message has already been approved, rejected, or expired.")
		return
	}

	writeConfirmPage(w, http.StatusOK, endpointAction, token, msg)
}

// --- POST executors ---

func (a *API) handleApproveMagicLinkPost(w http.ResponseWriter, r *http.Request) {
	if a.approvalSigner == nil {
		http.NotFound(w, r)
		return
	}
	token := extractFormToken(r)
	claims, status, errMsg := a.verifyMagicToken(token, approvaltoken.ActionApprove)
	if errMsg != "" {
		writeMagicMessage(w, status, "Invalid confirmation", errMsg)
		return
	}
	userID, agentID, err := a.store.ResolveOutboundOwner(r.Context(), claims.MessageID)
	if err != nil {
		writeMagicMessage(w, http.StatusNotFound, "Message not found",
			"This message no longer exists.")
		return
	}
	a.magicApprove(w, r, claims.MessageID, userID, agentID)
}

func (a *API) handleRejectMagicLinkPost(w http.ResponseWriter, r *http.Request) {
	if a.approvalSigner == nil {
		http.NotFound(w, r)
		return
	}
	token := extractFormToken(r)
	claims, status, errMsg := a.verifyMagicToken(token, approvaltoken.ActionReject)
	if errMsg != "" {
		writeMagicMessage(w, status, "Invalid confirmation", errMsg)
		return
	}
	userID, _, err := a.store.ResolveOutboundOwner(r.Context(), claims.MessageID)
	if err != nil {
		writeMagicMessage(w, http.StatusNotFound, "Message not found",
			"This message no longer exists.")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "magic-link rejection"
	}
	// Human-facing HTML form: CLAMP to the shared reject-reason cap rather
	// than failing the whole rejection over a long note — the reviewer's
	// decision matters more than the note's tail. The /v1 API path rejects
	// (422) instead; both count Unicode code points against the same
	// identity.MaxRejectReasonLen.
	reason = clampRunes(reason, identity.MaxRejectReasonLen)
	a.magicReject(w, r, claims.MessageID, userID, reason)
}

// clampRunes truncates s to at most n Unicode code points (runes, matching
// the OpenAPI maxLength semantics of the equivalent /v1 field — never bytes,
// which could split a multi-byte character).
func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// extractFormToken pulls the token from standard form encoding. Request
// body is expected to be application/x-www-form-urlencoded since the
// confirmation page submits a plain HTML form.
func extractFormToken(r *http.Request) string {
	_ = r.ParseForm()
	return r.FormValue("t")
}

// verifyMagicToken runs the HMAC + exp + action whitelist checks and
// returns either claims or an (HTTP status, user-visible message) pair.
// Shared by GET and POST so both paths reject the same inputs the same
// way.
//
// HMAC verification uses the deployment signer (a.approvalSigner, keyed
// on cfg.Signing.HMACSecret) — the sole signer for magic-link tokens.
func (a *API) verifyMagicToken(token, endpointAction string) (*approvaltoken.Claims, int, string) {
	if token == "" {
		return nil, http.StatusBadRequest,
			"This approval link is missing its token."
	}

	var (
		claims *approvaltoken.Claims
		err    error
	)
	if a.approvalSigner != nil {
		claims, err = a.approvalSigner.Verify(token)
	} else {
		err = approvaltoken.ErrInvalidToken
	}
	if err != nil {
		if errors.Is(err, approvaltoken.ErrTokenExpired) {
			return nil, http.StatusGone,
				"This approval link has expired. Visit the dashboard to review pending messages."
		}
		return nil, http.StatusBadRequest, "This approval link isn't valid."
	}
	if claims.Action != endpointAction {
		return nil, http.StatusBadRequest,
			"This approval link isn't valid for this action."
	}
	return claims, 0, ""
}

// --- Action implementations (called after POST + token verify) ---

func (a *API) magicApprove(w http.ResponseWriter, r *http.Request, messageID, userID, agentID string) {
	agent, err := a.store.GetAgentByID(r.Context(), agentID)
	if err != nil {
		log.Printf("[api] magic-approve: agent lookup failed: msg=%s agent=%s err=%v", messageID, agentID, err)
		writeMagicMessage(w, http.StatusInternalServerError,
			"Something went wrong",
			"We couldn't find the agent for this message. Try the dashboard.")
		return
	}
	if !agent.DomainVerified {
		writeMagicMessage(w, http.StatusForbidden,
			"Agent not verified",
			"Cannot approve: the agent's domain is no longer verified.")
		return
	}

	// Transition + enqueue onto QueueOutbound (the SendWorker submits). Self-sends
	// fall through to the local loopback path below.
	if a.outboundEnq == nil {
		writeMagicMessage(w, http.StatusInternalServerError, "Send failed",
			"The outbound delivery queue is unavailable. Try again from the dashboard.")
		return
	}
	draft, derr := a.store.GetOutboundMessageForUser(r.Context(), messageID, userID)
	if derr != nil {
		writeMagicMessage(w, http.StatusNotFound, "Message not found", "This message no longer exists.")
		return
	}
	// Suppression enforcement — same owner-scoped, fail-closed check as the
	// dashboard approve (ApprovePendingCore). The magic-link path takes no
	// reviewer edits, so the stored To/CC/BCC set is the final one. On
	// refusal the hold stays pending_review (nothing has run yet) and the
	// reviewer sees why; clearing the suppression makes the same link work.
	sendReq, buildErr := buildSendRequestFromMessage(draft)
	if buildErr != nil {
		writeMagicMessage(w, http.StatusBadRequest, "Cannot send", "The held message is invalid.")
		return
	}
	if supErr := a.checkSuppressionStrict(r.Context(), userID, agent.ID, sendReq); supErr != nil {
		writeMagicMessage(w, supErr.Status, "Cannot send", html.EscapeString(supErr.Msg))
		return
	}
	if uerr := prepareManagedUnsubscribe(r.Context(), a.unsubscribeIssuer, a.fromDomain, userID, agent, &sendReq, true); uerr != nil {
		writeMagicMessage(w, uerr.Status, "Cannot send", html.EscapeString(uerr.Msg))
		return
	}
	sent, handled, aerr := a.approveOutboundAsyncWithRequest(r.Context(), agent, messageID, userID, draft, identity.PendingApprovalEdit{}, sendReq, nil)
	if aerr != nil {
		writeMagicApproveError(w, messageID, aerr)
		return
	}
	if handled {
		log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s to_count=%d to_domains=%v approved=magic-link:user:%s delivery=async",
			sent.ID, sent.Type, sent.Status, agent.EmailAddress(), len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients), userID)
		writeMagicResult(w, http.StatusOK, "Approved",
			fmt.Sprintf("Your message to %s has been queued for delivery.", html.EscapeString(firstRecipient(sent.ToRecipients))),
			viewMessageCTA(agent.EmailAddress(), sent.ID))
		return
	}

	sent, err = a.approveSelfSend(r.Context(), agent, messageID, userID, identity.PendingApprovalEdit{}, nil)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrMessageNotFound):
			writeMagicMessage(w, http.StatusNotFound,
				"Message not found",
				"This message no longer exists.")
		case errors.Is(err, identity.ErrNotPendingApproval):
			writeMagicMessage(w, http.StatusConflict,
				"Already resolved",
				"This message has already been approved, rejected, or expired.")
		default:
			var ve *outbound.ValidationError
			if errors.As(err, &ve) {
				writeMagicMessage(w, http.StatusBadRequest, "Cannot send",
					html.EscapeString(ve.Error()))
				return
			}
			log.Printf("[api] magic-approve send failed: msg=%s err=%v", messageID, err)
			writeMagicMessage(w, http.StatusInternalServerError,
				"Send failed",
				"The message couldn't be sent. Try again from the dashboard.")
		}
		return
	}

	a.recordLoopbackUsage(r.Context(), userID, agent)
	log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s to_count=%d to_domains=%v approved=magic-link:user:%s",
		sent.ID, sent.Type, sent.Status, agent.EmailAddress(), len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients), userID)

	writeMagicResult(w, http.StatusOK,
		"Approved",
		fmt.Sprintf("Your message to %s has been sent.", html.EscapeString(firstRecipient(sent.ToRecipients))),
		viewMessageCTA(agent.EmailAddress(), sent.ID))
}

// writeMagicApproveError renders the magic-link HTML error page for an async
// approve failure, matching the sync approve path's status mapping.
func writeMagicApproveError(w http.ResponseWriter, messageID string, err error) {
	switch {
	case errors.Is(err, identity.ErrMessageNotFound):
		writeMagicMessage(w, http.StatusNotFound, "Message not found", "This message no longer exists.")
	case errors.Is(err, identity.ErrNotPendingApproval):
		writeMagicMessage(w, http.StatusConflict, "Already resolved",
			"This message has already been approved, rejected, or expired.")
	case outbound.IsComposedSizeError(err):
		writeMagicMessage(w, http.StatusRequestEntityTooLarge, "Cannot send",
			"The composed message is too large. Trim it and approve again from the dashboard.")
	default:
		var ve *outbound.ValidationError
		if errors.As(err, &ve) {
			writeMagicMessage(w, http.StatusBadRequest, "Cannot send", html.EscapeString(ve.Error()))
			return
		}
		log.Printf("[api] magic-approve accept failed: msg=%s err=%v", messageID, err)
		writeMagicMessage(w, http.StatusInternalServerError, "Send failed",
			"The message couldn't be sent. Try again from the dashboard.")
	}
}

func (a *API) magicReject(w http.ResponseWriter, r *http.Request, messageID, userID, reason string) {
	rejected, err := a.store.RejectPending(r.Context(), messageID, userID, reason)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrMessageNotFound):
			writeMagicMessage(w, http.StatusNotFound, "Message not found",
				"This message no longer exists.")
		case errors.Is(err, identity.ErrNotPendingApproval):
			writeMagicMessage(w, http.StatusConflict, "Already resolved",
				"This message has already been approved, rejected, or expired.")
		default:
			log.Printf("[api] magic-reject failed: msg=%s err=%v", messageID, err)
			writeMagicMessage(w, http.StatusInternalServerError, "Rejection failed",
				"We couldn't reject this message. Try again from the dashboard.")
		}
		return
	}
	log.Printf("[mail:%s] dir=outbound type=%s status=%s rejected_by=magic-link:user:%s reason_len=%d",
		rejected.ID, rejected.Type, rejected.Status, userID, utf8.RuneCountInString(reason))

	writeMagicMessage(w, http.StatusOK, "Rejected",
		"The message has been discarded and will not be sent.")
}

// --- HTML rendering ---
//
// These pages are served stand-alone by the Go binary and are most often
// opened from a phone email client, so they need to be self-contained:
// inline SVG brand mark, inline CSS, no external font loads, no JS. The
// Loft visual tokens (cream surfaces, canopy-green actions, gold spark
// accents, ink consoles, the
// 6px / 10px radii, etc.) are inlined to match web/src/app/globals.css —
// keep them in sync if the design system shifts.

func pageTitleForAction(action, fallback string) string {
	switch action {
	case approvaltoken.ActionApprove:
		return "Approve message"
	case approvaltoken.ActionReject:
		return "Reject message"
	default:
		return fallback
	}
}

// loftCommonCSS is the shared style block for both the confirm page and
// the post-action result page. Mirrors the tokens in globals.css.
const loftCommonCSS = `
:root {
  --bg: #FAF7F2;
  --bg-panel: #FFFFFF;
  --bg-elev: #F2ECE2;
  --ink: #1A1714;
  --ink-elev: #23201C;
  --ink-fg: #E8E3D8;
  --ink-fg-muted: #8C857A;
  --ink-border: #2E2A24;
  --fg: #1A1714;
  --fg-muted: #6E665B;
  --fg-subtle: #9A9082;
  --border: #E5DED3;
  --border-sub: #EFE9DD;
  --accent: #C17D2B;
  --accent-soft: #F6EAD3;
  --accent-strong: #8A5214;
  --accent-fill: #2B4033;
  --danger-bg: #FBE3E0;
  --danger-strong: #A82020;
  --success: #0F7A4D;
  --success-bg: #DFF3E8;
  --f-ui: "Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
  --f-mono: "JetBrains Mono", "SF Mono", ui-monospace, Menlo, monospace;
  --f-editorial: "Fraunces", Georgia, serif;
  --r-sm: 4px;
  --r-md: 6px;
  --r-lg: 10px;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
body {
  background: var(--bg);
  color: var(--fg);
  font-family: var(--f-ui);
  font-size: 14px;
  line-height: 1.5;
  -webkit-font-smoothing: antialiased;
  text-rendering: optimizeLegibility;
}
.wrap {
  max-width: 560px;
  margin: 0 auto;
  padding: 32px 20px 48px;
}
.brand {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 22px;
  text-decoration: none;
  color: var(--fg);
}
.brand-mark {
  width: 32px;
  height: 32px;
  border-radius: 7px;
  background: var(--fg);
  color: var(--bg);
  font-family: var(--f-mono);
  font-weight: 700;
  font-size: 12px;
  letter-spacing: -0.04em;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.brand-text {
  font-family: var(--f-mono);
  font-weight: 700;
  font-size: 14px;
  letter-spacing: -0.02em;
  line-height: 1.1;
}
.brand-tag {
  font-size: 11px;
  color: var(--fg-muted);
  display: block;
}
.eyebrow {
  font-family: var(--f-mono);
  font-size: 11px;
  font-weight: 600;
  color: var(--accent-strong);
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.card {
  background: var(--bg-panel);
  border: 1px solid var(--border);
  border-radius: var(--r-lg);
  overflow: hidden;
}
.card-head {
  padding: 22px 22px 18px;
}
.card-head h1 {
  font-size: 26px;
  font-weight: 700;
  letter-spacing: -0.012em;
  margin: 8px 0 6px;
  color: var(--fg);
  line-height: 1.15;
}
.lede {
  font-size: 14px;
  color: var(--fg-muted);
  margin: 0;
  line-height: 1.55;
}
.details {
  margin: 0;
  padding: 0 22px 18px;
  display: grid;
  grid-template-columns: 90px 1fr;
  row-gap: 8px;
  column-gap: 12px;
  font-size: 13px;
}
.details dt {
  font-family: var(--f-mono);
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-subtle);
  padding-top: 2px;
}
.details dd {
  margin: 0;
  color: var(--fg);
  word-break: break-word;
}
.details dd code {
  font-family: var(--f-mono);
  font-size: 12px;
  background: var(--bg-elev);
  border: 1px solid var(--border-sub);
  padding: 1px 6px;
  border-radius: var(--r-sm);
}
.details dd strong {
  font-weight: 600;
}
.preview-label {
  padding: 0 22px 8px;
  font-family: var(--f-mono);
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-subtle);
}
.preview {
  margin: 0 22px;
  background: var(--ink);
  color: var(--ink-fg);
  border: 1px solid var(--ink-border);
  border-radius: var(--r-lg);
  padding: 14px 16px;
  white-space: pre-wrap;
  font-family: var(--f-mono);
  font-size: 12.5px;
  line-height: 1.6;
  max-height: 260px;
  overflow: auto;
}
.actions {
  padding: 18px 22px 22px;
  border-top: 1px solid var(--border);
  margin-top: 18px;
  background: var(--bg-elev);
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
}
form { margin: 0; display: contents; }
.field {
  padding: 16px 22px 0;
}
.field label {
  display: block;
  font-size: 12px;
  color: var(--fg-muted);
  margin-bottom: 4px;
}
.field input {
  width: 100%;
  padding: 9px 11px;
  font-family: inherit;
  font-size: 14px;
  color: var(--fg);
  background: var(--bg-panel);
  border: 1px solid var(--border);
  border-radius: var(--r-md);
}
.field input:focus {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
  border-color: var(--accent);
}
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-family: var(--f-ui);
  font-size: 14px;
  font-weight: 500;
  padding: 10px 18px;
  border-radius: var(--r-md);
  border: 1px solid transparent;
  cursor: pointer;
  text-decoration: none;
  line-height: 1;
}
.btn-primary {
  background: var(--accent-fill);
  color: #fff;
}
.btn-primary:hover { background: #223529; }
.btn-danger {
  background: var(--bg-panel);
  color: var(--danger-strong);
  border-color: var(--danger-bg);
}
.btn-danger:hover { background: var(--danger-bg); }
.btn-ghost {
  background: transparent;
  color: var(--fg-muted);
  padding: 10px 6px;
  font-size: 13px;
}
.btn-ghost:hover { color: var(--fg); }
.footnote {
  margin-top: 18px;
  font-size: 12px;
  color: var(--fg-subtle);
  line-height: 1.55;
}
.banner {
  padding: 16px 18px;
  border-radius: var(--r-md);
  font-size: 14px;
  line-height: 1.55;
  margin-bottom: 16px;
}
.banner-success {
  background: var(--success-bg);
  color: var(--success);
  border: 1px solid var(--success-bg);
}
.banner-warn {
  background: #FFF1D1;
  color: #8F5F00;
  border: 1px solid #FFF1D1;
}
.banner-danger {
  background: var(--danger-bg);
  color: var(--danger-strong);
  border: 1px solid var(--danger-bg);
}
.result h1 {
  font-family: var(--f-editorial);
  font-style: italic;
  font-weight: 400;
  font-size: clamp(32px, 8vw, 44px);
  letter-spacing: -0.012em;
  color: var(--fg);
  margin: 14px 0 8px;
  line-height: 1.05;
}
.result p {
  font-size: 15px;
  color: var(--fg-muted);
  line-height: 1.55;
  margin: 0 0 18px;
}
.muted { color: var(--fg-muted); }
@media (max-width: 480px) {
  .card-head, .actions, .field { padding-left: 16px; padding-right: 16px; }
  .preview, .preview-label { margin-left: 16px; margin-right: 16px; }
  .preview-label { padding-left: 0; padding-right: 0; }
  .details {
    grid-template-columns: 1fr;
    padding-left: 16px;
    padding-right: 16px;
    row-gap: 12px;
  }
  .details dt { padding-top: 0; }
  .btn { width: 100%; }
  .actions .btn-ghost { width: auto; }
}
`

// loftFaviconDataURI is web/public/favicon.svg inlined as a data: URI so
// the favicon doesn't depend on the static-asset layer being reachable
// from wherever these pages are served. Bytes match the source SVG
// exactly — regenerate via `base64 < web/public/favicon.svg` if you
// re-render the brand mark.
const loftFaviconDataURI = "data:image/svg+xml;base64,PD94bWwgdmVyc2lvbj0iMS4wIj8+PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIzMiIgaGVpZ2h0PSIzMiIgdmlld0JveD0iMCAwIDMyIDMyIiByb2xlPSJpbWciIGFyaWEtbGFiZWw9ImUyYSI+ICA8cmVjdCB3aWR0aD0iMzIiIGhlaWdodD0iMzIiIHJ4PSI3IiBmaWxsPSIjMUExNzE0Ij48L3JlY3Q+ICA8dGV4dCB4PSIxNiIgeT0iMjMuNSIgdGV4dC1hbmNob3I9Im1pZGRsZSIgZm9udC1mYW1pbHk9InVpLXNhbnMtc2VyaWYsIHN5c3RlbS11aSwgLWFwcGxlLXN5c3RlbSwgJiMzOTtIZWx2ZXRpY2EgTmV1ZSYjMzk7LCBBcmlhbCwgc2Fucy1zZXJpZiIgZm9udC13ZWlnaHQ9IjcwMCIgZm9udC1zaXplPSIyNCIgbGV0dGVyLXNwYWNpbmc9Ii0xIiBmaWxsPSIjRThFM0Q4Ij4yPC90ZXh0Pjwvc3ZnPg=="

// writeLoftHead writes the shared <!doctype>, <head>, opening <body>, and
// brand block. The caller continues the page body and is responsible for
// closing </body></html>.
func writeLoftHead(w http.ResponseWriter, title string) {
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="theme-color" content="#FAF7F2">
<title>%s — e2a</title>
<link rel="icon" type="image/svg+xml" href="%s">
<style>%s</style>
</head>
<body>
<div class="wrap">
<a class="brand" href="/" aria-label="e2a">
  <span class="brand-mark" aria-hidden="true">2</span>
  <span>
    <span class="brand-text">e2a</span>
    <span class="brand-tag">Email for AI agents</span>
  </span>
</a>
`, html.EscapeString(title), loftFaviconDataURI, loftCommonCSS)
}

// setMagicHeaders applies the shared security headers that any magic-link
// response carries. Same set on every render path.
func setMagicHeaders(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Block embedding so any approved/rejected success page can't be
	// iframed into a page that pretends to be legitimate.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Discourage indexing if a link ever leaks publicly.
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
}

// magicCTA is the single call-to-action button at the foot of the result
// page.
type magicCTA struct {
	Label string
	Href  string
}

// dashboardCTA is the fallback CTA: every error, reject, and
// already-resolved page lands the reviewer on the dashboard because there
// is no one message worth deep-linking to.
var dashboardCTA = magicCTA{Label: "Open the dashboard", Href: "/dashboard"}

// viewMessageCTA deep-links the dashboard's single-message focus view so an
// approving reviewer lands on the message they just approved instead of the
// dashboard root. The href is relative on purpose — these pages are served
// by the same origin as the dashboard, so this works under any public URL.
// Both identifiers are required by the focus view (it resolves the message
// through the agent-scoped detail endpoint); if either is missing there is
// nothing to link to, so fall back to the dashboard.
func viewMessageCTA(agentEmail, messageID string) magicCTA {
	if agentEmail == "" || messageID == "" {
		return dashboardCTA
	}
	return magicCTA{
		Label: "View message",
		// direction=outbound selects the outbound projection on the focus
		// page; a held message resolved through this flow is always outbound.
		Href: "/inboxes/messages/view?email=" + url.QueryEscape(agentEmail) +
			"&id=" + url.QueryEscape(messageID) + "&direction=outbound",
	}
}

// writeMagicMessage renders the post-action / error page with the default
// dashboard CTA.
func writeMagicMessage(w http.ResponseWriter, status int, title, body string) {
	writeMagicResult(w, status, title, body, dashboardCTA)
}

// writeMagicResult renders the post-action / error page. Cream surface,
// inline brand mark up top, big editorial-italic title, ember CTA at the
// foot.
func writeMagicResult(w http.ResponseWriter, status int, title, body string, cta magicCTA) {
	setMagicHeaders(w, status)
	writeLoftHead(w, title)
	fmt.Fprintf(w, `<div class="result">
<span class="eyebrow">%s</span>
<h1>%s</h1>
<p>%s</p>
<p><a class="btn btn-primary" href="%s">%s</a></p>
</div>
</div>
</body>
</html>
`,
		html.EscapeString(bannerEyebrowFor(status)),
		html.EscapeString(title),
		// `body` is sometimes a pre-escaped fragment (the validation-error
		// path runs html.EscapeString itself before calling us). Don't
		// double-escape.
		body,
		html.EscapeString(cta.Href),
		html.EscapeString(cta.Label),
	)
}

// bannerEyebrowFor returns a short uppercase status word that sits above
// the result page's title — "Done" / "Pending" / "Error" depending on
// the HTTP status. Keeps the page calm at a glance.
func bannerEyebrowFor(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "Done"
	case status == http.StatusConflict || status == http.StatusGone:
		return "Already resolved"
	default:
		return "Error"
	}
}

// writeConfirmPage renders the token-gated preview + POST form. This is
// where the body preview lives — the reviewer has already opened the
// link in their browser, so showing held content here is acceptable.
// The form targets the same URL path with method POST so the actual
// state change only fires on an explicit click.
func writeConfirmPage(w http.ResponseWriter, status int, action, token string, msg *identity.Message) {
	setMagicHeaders(w, status)

	var actionPath, submitLabel, eyebrow, title, lede, submitClass string
	switch action {
	case approvaltoken.ActionApprove:
		actionPath = "/v1/approve"
		submitLabel = "Approve & send"
		submitClass = "btn btn-primary"
		eyebrow = "Pending · approve"
		title = "Approve message"
		lede = "This message will be sent from your agent immediately."
	case approvaltoken.ActionReject:
		actionPath = "/v1/reject"
		submitLabel = "Reject"
		submitClass = "btn btn-danger"
		eyebrow = "Pending · reject"
		title = "Reject message"
		lede = "This message will be discarded and not sent."
	}

	bodyPreview := msg.BodyText
	if bodyPreview == "" && msg.BodyHTML != "" {
		bodyPreview = "(HTML only; view full message in the dashboard)"
	}

	toList := strings.Join(msg.ToRecipients, ", ")
	ccList := strings.Join(msg.CC, ", ")
	bccList := strings.Join(msg.BCC, ", ")

	var rows strings.Builder
	fmt.Fprintf(&rows, `<dt>Agent</dt><dd><code>%s</code></dd>`,
		html.EscapeString(msg.AgentID))
	if toList != "" {
		fmt.Fprintf(&rows, `<dt>To</dt><dd>%s</dd>`, html.EscapeString(toList))
	}
	if ccList != "" {
		fmt.Fprintf(&rows, `<dt>Cc</dt><dd>%s</dd>`, html.EscapeString(ccList))
	}
	if bccList != "" {
		fmt.Fprintf(&rows, `<dt>Bcc</dt><dd>%s</dd>`, html.EscapeString(bccList))
	}
	fmt.Fprintf(&rows, `<dt>Subject</dt><dd><strong>%s</strong></dd>`,
		html.EscapeString(msg.Subject))
	if msg.ApprovalExpiresAt != nil {
		fmt.Fprintf(&rows, `<dt>Expires</dt><dd>%s</dd>`,
			html.EscapeString(msg.ApprovalExpiresAt.UTC().Format(time.RFC1123)))
	}

	// Optional rejection-reason input only on /reject.
	reasonField := ""
	if action == approvaltoken.ActionReject {
		reasonField = `<div class="field">
<label for="reason">Reason (optional)</label>
<input id="reason" name="reason" type="text" maxlength="200" placeholder="e.g. wrong tone, bad recipient">
</div>`
	}

	writeLoftHead(w, title)
	fmt.Fprintf(w, `<div class="card">
<form method="POST" action="%s">
<input type="hidden" name="t" value="%s">
<div class="card-head">
<span class="eyebrow">%s</span>
<h1>%s</h1>
<p class="lede">%s</p>
</div>
<dl class="details">%s</dl>
<div class="preview-label">Body preview</div>
<div class="preview">%s</div>
%s
<div class="actions">
<button type="submit" class="%s">%s</button>
<a href="about:blank" class="btn btn-ghost">Close without acting</a>
</div>
</form>
</div>
<p class="footnote">Clicking the button submits the action to e2a. If you close the window without clicking, nothing changes and the message stays pending.</p>
</div>
</body>
</html>
`,
		html.EscapeString(actionPath),
		html.EscapeString(token),
		html.EscapeString(eyebrow),
		html.EscapeString(title),
		html.EscapeString(lede),
		rows.String(),
		html.EscapeString(bodyPreview),
		reasonField,
		submitClass,
		html.EscapeString(submitLabel),
	)
}

func firstRecipient(rs []string) string {
	if len(rs) == 0 {
		return "the recipient"
	}
	return rs[0]
}
