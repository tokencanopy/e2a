package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/mailparse"
)

// attachmentDownloadTTL bounds how long a minted download URL stays valid. Short
// because an agent fetches immediately after asking; long enough to cover a
// retry. The URL is a bearer capability (the token authorizes the download), so
// a tight TTL bounds leakage to logs/history.
const attachmentDownloadTTL = 15 * time.Minute

// attachmentInlineMaxBytes caps the size eligible for `?inline=true` base64. Past
// this, the caller must use the download_url — base64 of a larger file would blow
// an agent's context (the whole point of §6a #5).
const attachmentInlineMaxBytes = 256 * 1024

// AttachmentStore mints (and verifies) a short-lived download for one attachment.
// The native adapter (below) returns a capability-token URL to e2a's own
// streaming route; a future object-storage adapter could return a presigned URL
// instead — the metadata/contract shape ({download_url, expires_at}) is the same.
type AttachmentStore interface {
	// DownloadURL returns a short-lived URL the caller (or a sandboxed tool) can
	// GET for the bytes, plus its expiry.
	DownloadURL(agentEmail, messageID string, index int, ttl time.Duration) (downloadURL string, expiresAt time.Time, err error)
	// VerifyDownload reports whether `token` authorizes downloading attachment
	// `index` of `messageID` (and is unexpired). Identity is NOT in the token;
	// the download handler additionally binds the message to the path agent.
	VerifyDownload(token, messageID string, index int) bool
}

// nativeAttachmentStore mints capability tokens with the deployment HMAC secret
// (HMAC-SHA256, the same crypto family as the HITL magic-link) and points URLs at
// e2a's own /…/attachments/{index}/download route. Zero external dependency. The
// token is purpose-scoped: it binds message_id + index + expiry, so it can only
// download the exact attachment it was minted for and only until it expires.
type nativeAttachmentStore struct {
	secret    []byte
	publicURL string // base, e.g. https://api.e2a.dev (no trailing slash)
}

// NewNativeAttachmentStore returns the default (zero-dependency) attachment store.
func NewNativeAttachmentStore(secret, publicURL string) AttachmentStore {
	return &nativeAttachmentStore{secret: []byte(secret), publicURL: strings.TrimRight(publicURL, "/")}
}

// attachmentTokenPayload is the signed, opaque-to-the-client capability payload.
func attachmentTokenPayload(messageID string, index int, expUnix int64) string {
	return fmt.Sprintf("%s|%d|%d", messageID, index, expUnix)
}

func (n *nativeAttachmentStore) sign(messageID string, index int, expUnix int64) string {
	payload := attachmentTokenPayload(messageID, index, expUnix)
	mac := hmac.New(sha256.New, n.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (n *nativeAttachmentStore) DownloadURL(agentEmail, messageID string, index int, ttl time.Duration) (string, time.Time, error) {
	// Fail closed if wired with no secret — an empty HMAC key makes every token
	// trivially forgeable. Production config already rejects empty/short secrets,
	// but defend here too so a future miswiring can't silently open the route.
	if len(n.secret) == 0 {
		return "", time.Time{}, fmt.Errorf("attachment store: empty signing secret")
	}
	exp := time.Now().Add(ttl)
	tok := n.sign(messageID, index, exp.Unix())
	u := fmt.Sprintf("%s/v1/agents/%s/messages/%s/attachments/%d/download?token=%s",
		n.publicURL,
		url.PathEscape(agentEmail),
		url.PathEscape(messageID),
		index,
		url.QueryEscape(tok),
	)
	return u, exp, nil
}

func (n *nativeAttachmentStore) VerifyDownload(token, messageID string, index int) bool {
	if len(n.secret) == 0 {
		return false // never validate against an empty key (see DownloadURL)
	}
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return false
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	// Recompute the MAC over the presented payload and constant-time compare.
	mac := hmac.New(sha256.New, n.secret)
	mac.Write(payload)
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return false
	}
	// Parse message_id|index|exp and bind to the requested attachment + check expiry.
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 {
		return false
	}
	wantIndex, err := strconv.Atoi(parts[1])
	if err != nil || wantIndex != index || parts[0] != messageID {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || !time.Now().Before(time.Unix(expUnix, 0)) {
		return false // reject at/after the exact expiry second
	}
	return true
}

// ── HTTP surface ─────────────────────────────────────────────────────────────

type attachmentParam struct {
	Address   string `path:"email"`
	MessageID string `path:"id"`
	Index     int    `path:"index" minimum:"0"`
	Inline    bool   `query:"inline" doc:"When true, also include the bytes as base64 in 'data' — ONLY for attachments <= 256 KB; larger inline requests are rejected (413). Default false (use download_url)."`
}

// AttachmentView is the metadata + short-lived download for one attachment.
// Bytes are NOT included unless inline was requested for a small file.
// SizeBytes is the DECODED payload size (Content-Transfer-Encoding undone) —
// exactly the byte count download_url serves and the count the inline cap is
// checked against. NOT the encoded size inside the raw MIME, and NOT the
// message-level size_bytes (raw MIME length of the whole message).
type AttachmentView struct {
	Index       int       `json:"index"`
	Filename    string    `json:"filename,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	ContentID   string    `json:"content_id,omitempty" doc:"Content-ID (angle brackets stripped) for an inline part referenced by a body 'cid:' URL, e.g. an embedded image. Omitted for non-inline attachments."`
	SizeBytes   int       `json:"size_bytes" doc:"DECODED attachment payload size in bytes (Content-Transfer-Encoding undone) — exactly what download_url serves and what the 256 KB inline cap is checked against; not the encoded size inside the raw MIME."`
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at" format:"date-time"`
	// Data is the base64 bytes, present ONLY when inline=true and the attachment
	// is within the inline size cap.
	Data string `json:"data,omitempty"`
}

type attachmentOutput struct {
	Body AttachmentView
}

func (s *Server) registerAttachments() {
	registerOp(s.API, huma.Operation{
		OperationID: "getAttachment",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/messages/{id}/attachments/{index}",
		Summary:     "Get an attachment (metadata + short-lived download URL)",
		Description: "Returns one attachment's metadata plus a short-lived `download_url` (+ `expires_at`) to fetch the bytes out of band — so binary content never streams through an agent's context. Pass `?inline=true` to also receive base64 `data` for small attachments (<= 256 KB); larger inline requests are rejected with 413 attachment_too_large. `index` is the 0-based attachment index from the message's `attachments[]`.",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"413": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Payload Too Large — code attachment_too_large: `?inline=true` was requested for an attachment over the 256 KB inline cap. Fetch the bytes via download_url instead; the metadata (and this error) tells you the size."),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleGetAttachment)
}

func (s *Server) handleGetAttachment(ctx context.Context, in *attachmentParam) (*attachmentOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.GetMessage == nil || s.deps.AttachmentStore == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "attachment retrieval unavailable")
	}
	msg, err := s.deps.GetMessage(ctx, in.MessageID, ag.ID)
	if err != nil || msg == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "message not found")
	}
	att, ok := mailparse.AttachmentAt(msg.RawMessage, in.Index)
	if !ok {
		return nil, NewError(http.StatusNotFound, "attachment_not_found", "no attachment at that index")
	}
	dl, exp, err := s.deps.AttachmentStore.DownloadURL(in.Address, in.MessageID, in.Index, attachmentDownloadTTL)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to mint download url")
	}
	view := AttachmentView{
		Index:       in.Index,
		Filename:    att.Filename,
		ContentType: att.ContentType,
		ContentID:   att.ContentID,
		SizeBytes:   len(att.Data),
		DownloadURL: dl,
		ExpiresAt:   exp.UTC(),
	}
	if in.Inline {
		if len(att.Data) > attachmentInlineMaxBytes {
			return nil, NewError(http.StatusRequestEntityTooLarge, "attachment_too_large",
				fmt.Sprintf("attachment is %d bytes; inline is capped at %d — use download_url instead", len(att.Data), attachmentInlineMaxBytes))
		}
		view.Data = base64.StdEncoding.EncodeToString(att.Data)
	}
	return &attachmentOutput{Body: view}, nil
}

// handleAttachmentDownload streams one attachment's bytes. It is a RAW chi route
// (not Huma, not bearer): the capability TOKEN authorizes the download, so the
// URL can be handed to a sandboxed tool without leaking the agent's credential —
// the same model as the HITL magic-link. The token binds message+index; the path
// {email} additionally binds the message to its owning agent (GetMessage is
// keyed by agent id), so a valid token can only stream the exact attachment it
// was minted for.
func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit FIRST. This raw capability-token route sits OUTSIDE the
	// Huma rate-limit middleware, and each accepted hit runs GetAgent +
	// GetMessage + a full MIME re-parse — so throttle before any of that work to
	// bound token-replay and index/message probing. Keyed by client IP (no bearer
	// here); shares the per-IP convention of the registration/feedback limiters.
	if s.deps.DownloadLimit != nil {
		ok, retryAfter, limit, remaining, reset := s.deps.DownloadLimit(clientIP(r))
		w.Header().Set("RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("RateLimit-Reset", strconv.Itoa(reset))
		if !ok {
			secs := int(retryAfter.Round(time.Second).Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeRawError(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded",
				map[string]any{"retry_after_seconds": secs})
			return
		}
	}

	email := identity.NormalizeEmail(chi.URLParam(r, "email"))
	id := chi.URLParam(r, "id")
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		writeRawError(w, r, http.StatusBadRequest, "invalid_request", "invalid attachment index", nil)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		writeRawError(w, r, http.StatusUnauthorized, "unauthorized", "token query parameter required", nil)
		return
	}
	if s.deps.AttachmentStore == nil || s.deps.GetAgent == nil || s.deps.GetMessage == nil {
		writeRawError(w, r, http.StatusInternalServerError, "internal_error", "attachment download unavailable", nil)
		return
	}
	// Capability check: the token must authorize exactly this message+index.
	if !s.deps.AttachmentStore.VerifyDownload(token, id, index) {
		writeRawError(w, r, http.StatusForbidden, "forbidden", "invalid or expired download token", nil)
		return
	}
	// Bind the message to the path agent (GetMessage is keyed by agent id), so a
	// token can't be replayed against a path naming a different agent.
	ag, err := s.deps.GetAgent(r.Context(), email)
	if err != nil || ag == nil {
		writeRawError(w, r, http.StatusNotFound, "not_found", "agent not found", nil)
		return
	}
	msg, err := s.deps.GetMessage(r.Context(), id, ag.ID)
	if err != nil || msg == nil {
		writeRawError(w, r, http.StatusNotFound, "not_found", "message not found", nil)
		return
	}
	att, ok := mailparse.AttachmentAt(msg.RawMessage, index)
	if !ok {
		writeRawError(w, r, http.StatusNotFound, "attachment_not_found", "attachment not found", nil)
		return
	}
	ctype := att.ContentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(att.Data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if att.Filename != "" {
		// Quote + strip CR/LF to keep the header well-formed.
		safe := strings.NewReplacer("\r", "", "\n", "", "\"", "").Replace(att.Filename)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safe))
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}
	_, _ = w.Write(att.Data)
}

// writeRawError writes the e2a error envelope as JSON to a raw (non-Huma)
// ResponseWriter, stamping the request id like the Huma handler path. Used by the
// raw attachment-download route, which can't return through Huma.
func writeRawError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]any) {
	env := NewError(status, code, msg)
	if details != nil {
		env = env.WithDetails(details)
	}
	writeRawEnvelope(w, r, env)
}
