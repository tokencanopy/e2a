package outbound

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/mail"
	"strings"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/mailfrom"
)

func appendUnsubscribeFooter(textBody, htmlBody, agentAddress, link string) (string, string) {
	textBody += "\n\nUnsubscribe from emails sent by " + agentAddress + ": " + link
	if htmlBody != "" {
		htmlBody += `<p>Unsubscribe from emails sent by ` + html.EscapeString(agentAddress) + `: <a href="` + html.EscapeString(link) + `">Unsubscribe</a></p>`
	}
	return textBody, htmlBody
}

func addUnsubscribeHeaders(message []byte, link string) []byte {
	headers := []byte("List-Unsubscribe: <" + sanitizeHeaderValue(link) + ">\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
	return append(headers, message...)
}

// DKIMKeyLookup returns the DKIM selector and PKCS#1 DER private key
// bytes for a domain. Empty selector OR empty key means "no key
// available — skip signing". Implementations should NOT return an
// error for the not-found case; that's a normal flow during the
// migration window when older domains haven't been keyed yet.
//
// Method name carries the "Internal" suffix to flag the boundary:
// this is NOT user-input-safe. The caller must have already
// authenticated and authorized the from-domain (e.g. via the agent
// layer's ownership check on the sender). A handler that ever calls
// this with a user-supplied domain string becomes a "sign as
// anyone" primitive.
type DKIMKeyLookup interface {
	GetDKIMKeyInternal(ctx context.Context, domain string) (selector string, privateKey []byte, err error)
}

// Attachment is a base64-encoded file attachment.
type Attachment struct {
	Filename    string `json:"filename" example:"report.pdf" doc:"Attachment filename. Maximum 1024 UTF-8 bytes; longer names are rejected as invalid_attachment."`
	ContentType string `json:"content_type" example:"application/pdf"`
	Data        string `json:"data" example:"base64-encoded-content" doc:"Base64-encoded file content. Each attachment must be ≤ 10 MiB decoded; a message may carry at most 10 attachments totaling ≤ 25 MiB decoded."` // base64-encoded
} // @name Attachment

// SendRequest is the outbound email contract.
//
// References is the full ancestor Message-ID chain for a reply, oldest →
// newest. When non-empty, it is written verbatim into the References:
// header so receiving mail clients can anchor the reply to an existing
// thread by matching ANY id in the chain — required for multi-party
// threads where the immediate-parent Message-ID may not be in every
// participant's mailbox. When empty but ReplyToMessageID is set, the
// References header falls back to a single id (legacy behavior).
type SendRequest struct {
	From     string   `json:"from,omitempty"`
	To       []string `json:"to"`
	CC       []string `json:"cc,omitempty"`
	BCC      []string `json:"bcc,omitempty"`
	Subject  string   `json:"subject"`
	Body     string   `json:"body"`
	HTMLBody string   `json:"html_body,omitempty"`
	// ReplyTo, when set, overrides the Reply-To header (which otherwise
	// defaults to the agent's own address). Replies land here instead of at the
	// From address. Must be a single RFC 5322 address, optionally with a display
	// name; validated at the API edge.
	ReplyTo          string              `json:"reply_to,omitempty"`
	ReplyToMessageID string              `json:"reply_to_message_id"`
	References       []string            `json:"references,omitempty"`
	ConversationID   string              `json:"conversation_id,omitempty"`
	Attachments      []Attachment        `json:"attachments,omitempty"`
	Unsubscribe      *UnsubscribeOptions `json:"unsubscribe,omitempty"`
}

type UnsubscribeOptions struct {
	Mode string `json:"mode"`
	URL  string `json:"-"`
}

// ManagedUnsubscribeIntent reconstructs the unresolved composition intent
// persisted on a held message. The recipient-bound URL is minted only when the
// final recipient set is approved.
func ManagedUnsubscribeIntent(enabled bool) *UnsubscribeOptions {
	if !enabled {
		return nil
	}
	return &UnsubscribeOptions{Mode: "managed"}
}

// SuppressionRemediation returns the shared operator guidance for an address
// blocked by either account-wide or exact-agent suppression scope.
func SuppressionRemediation(agentID string) string {
	agentID = strings.ToLower(strings.TrimSpace(agentID))
	return " — remove every applicable suppression before retrying; if both scopes contain an address, remove both (account-wide: DELETE /v1/account/suppressions/{address}; agent-scoped: DELETE /v1/agents/" +
		agentID + "/suppressions/{address}?confirm=DELETE)"
}

// SendResult contains the result of a successful send, including the
// canonicalized recipient lists for persistence.
type SendResult struct {
	MessageID string   `json:"message_id"`
	Method    string   `json:"method"`  // "smtp"
	SentAs    string   `json:"sent_as"` // "own_address" | "relay" (decision 4 fallback)
	To        []string `json:"-"`       // canonicalized To recipients
	CC        []string `json:"-"`       // canonicalized CC recipients
	BCC       []string `json:"-"`       // canonicalized BCC recipients
	// Raw is the exact composed MIME placed on the wire (post-DKIM, post-SES
	// header). Persisted as messages.raw_message so the agent gets a readable
	// "Sent folder" — a mailbox keeps both sides of a conversation.
	Raw []byte `json:"-"`
}

// ValidationError indicates a caller error (invalid addresses, no visible recipients).
// Handlers should map this to HTTP 400.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// IsValidationError returns true if err is a ValidationError.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

type Sender struct {
	smtpRelay  *SMTPRelay
	fromDomain string
	// dkimLookup is optional. When non-nil, Send asks it for a private
	// key for the From-header domain and prepends a DKIM-Signature
	// header before handing the message to the relay. A nil lookup
	// (older callers, unit tests, dev mode without a store) bypasses
	// signing entirely — the relay falls back to whatever
	// deployment-level signing it has always done.
	dkimLookup DKIMKeyLookup
	// sendingStatus is optional (decision 4 / Slice 4). When set AND the
	// agent's verified custom domain has sending_status == "verified", the
	// From header uses the agent's OWN address instead of the relay
	// "… via e2a" rewrite. nil, an unverified domain, a lookup error, or any
	// non-verified status all fall back to the relay From (fail-closed): we
	// never send unaligned mail under a customer domain.
	sendingStatus SendingStatusLookup
	// sesConfigSet, when set, is added as the X-SES-CONFIGURATION-SET header so
	// SES publishes delivery/bounce/complaint events (decision 9 / Slice 4b).
	// Empty (the default) = no header, no events — dev/self-host without SES.
	sesConfigSet string
}

// SetSESConfigurationSet enables SES event publishing for outbound mail by
// tagging each message with the given configuration set. Optional-setter
// pattern; empty leaves event publishing off.
func (s *Sender) SetSESConfigurationSet(name string) { s.sesConfigSet = name }

// SendingStatusLookup returns a domain's sending_status string
// ("none"|"pending"|"verified"|"failed"). *identity.Store satisfies it.
// Kept as a string interface so outbound does not import senderidentity
// (and its River + AWS SDK deps).
type SendingStatusLookup interface {
	GetSendingStatus(ctx context.Context, domain string) (string, error)
}

// SetSendingStatusLookup enables own-address From for sending-verified
// domains. Optional-setter pattern (cf. relay.SetOutbox) so existing
// NewSender/NewSenderWithDKIM call sites and tests are unaffected.
func (s *Sender) SetSendingStatusLookup(l SendingStatusLookup) { s.sendingStatus = l }

// useOwnAddressFrom reports whether outbound for this agent may use its own
// address as the From header. Fail-closed: every uncertain path returns false.
func (s *Sender) useOwnAddressFrom(agent *identity.AgentIdentity) bool {
	if s.sendingStatus == nil || agent == nil || !agent.DomainVerified || agent.RegisteredDomainName() == "" {
		return false
	}
	// "verified" mirrors senderidentity.StatusVerified (not imported here).
	status, err := s.sendingStatus.GetSendingStatus(context.Background(), agent.RegisteredDomainName())
	if err != nil {
		return false
	}
	return status == "verified"
}

// envelopeSender returns the SMTP MAIL FROM (Return-Path) for an outbound
// message: the aligned custom MAIL FROM (bounces@bounce.<domain>) when the
// domain is sending-verified, else the e2a-owned relay address (fail-closed).
// Pure (no store hit) so it's unit-testable; `own` is resolved once by Send.
func envelopeSender(own bool, agentDomain, fromDomain string) string {
	if own {
		return mailfrom.EnvelopeSender(agentDomain)
	}
	return fmt.Sprintf("agent@%s", fromDomain)
}

func NewSender(smtpRelay *SMTPRelay, fromDomain string) *Sender {
	return &Sender{
		smtpRelay:  smtpRelay,
		fromDomain: fromDomain,
	}
}

// NewSenderWithDKIM is NewSender with per-domain DKIM signing enabled.
// The lookup is queried once per send; key misses silently skip signing
// rather than fail the send.
func NewSenderWithDKIM(smtpRelay *SMTPRelay, fromDomain string, dkimLookup DKIMKeyLookup) *Sender {
	return &Sender{
		smtpRelay:  smtpRelay,
		fromDomain: fromDomain,
		dkimLookup: dkimLookup,
	}
}

// composed is the full result of composing an outbound message, up to (but not
// including) the SMTP submit. Shared by Send (looping submit), SendOnce (single
// submit), and the async accept path (persist bytes; River worker submits) so
// every path puts byte-identical mail on the wire.
type composed struct {
	envelopeFrom string   // SMTP MAIL FROM (Return-Path)
	envelope     []string // to+cc+bcc — the RCPT TO set
	wire         []byte   // DKIM-signed + X-SES-CONFIGURATION-SET header (what SES receives)
	sentBody     []byte   // DKIM-signed, NO SES header (the retained Sent-folder copy)
	sentAs       string   // "own_address" | "relay"
	to, cc, bcc  []string // canonicalized visible/blind recipients
}

// ComposeResult is the public view of a composed-but-not-yet-sent message, for
// the async accept path (internal/agent's DeliverOutbound). The accept-tx
// persists Raw as messages.raw_message and EnvelopeFrom/SentAs on the row; the
// River worker (internal/outboundsend) reloads them and submits via SubmitOnce.
// Raw is the Sent-folder copy (no SES config-set header) — SubmitOnce re-attaches
// that header at submit time, exactly as Send does, so the recipient copy and the
// stored copy stay identical to the synchronous path.
type ComposeResult struct {
	EnvelopeFrom string
	Recipients   []string // envelope (to+cc+bcc) for RCPT TO
	SentAs       string
	Method       string // always "smtp"
	Raw          []byte // Sent-folder bytes (DKIM-signed, no SES header)
	To, CC, BCC  []string
}

// Send normalizes recipients, composes, and sends an email via SMTP relay
// (the historical retrying submit). Returns a ValidationError for caller errors
// (bad addresses, no visible recipients) and a plain error for transport failures.
func (s *Sender) Send(agent *identity.AgentIdentity, req SendRequest) (*SendResult, error) {
	c, err := s.compose(agent, req)
	if err != nil {
		return nil, err
	}
	sesMessageID, err := s.smtpRelay.Send(c.envelopeFrom, c.envelope, c.wire)
	if err != nil {
		return nil, fmt.Errorf("smtp relay: %w", err)
	}
	return &SendResult{
		MessageID: sesMessageID,
		Method:    "smtp",
		SentAs:    c.sentAs,
		To:        c.to,
		CC:        c.cc,
		BCC:       c.bcc,
		Raw:       c.sentBody,
	}, nil
}

// SendOnce is Send with a SINGLE SMTP submit and no internal retry loop — the
// entry point for a caller that owns its own retry envelope. Behaviorally
// identical to Send except it calls smtpRelay.SendOnce. (The async pipeline does
// NOT use this — it persists ComposeForAccept's bytes and the River worker
// submits them via SubmitOnce — but it is the direct single-attempt analogue.)
func (s *Sender) SendOnce(agent *identity.AgentIdentity, req SendRequest) (*SendResult, error) {
	c, err := s.compose(agent, req)
	if err != nil {
		return nil, err
	}
	sesMessageID, err := s.smtpRelay.SendOnce(c.envelopeFrom, c.envelope, c.wire)
	if err != nil {
		return nil, fmt.Errorf("smtp relay: %w", err)
	}
	return &SendResult{
		MessageID: sesMessageID,
		Method:    "smtp",
		SentAs:    c.sentAs,
		To:        c.to,
		CC:        c.cc,
		BCC:       c.bcc,
		Raw:       c.sentBody,
	}, nil
}

// ComposeForAccept composes an outbound message for the async accept path WITHOUT
// submitting it. The accept-tx persists the returned bytes + envelope so the River
// worker owns the actual SMTP submit; it reuses Send's exact compose stage (same
// recipient normalization, From gating, DKIM) so sync and async are wire-identical.
func (s *Sender) ComposeForAccept(agent *identity.AgentIdentity, req SendRequest) (*ComposeResult, error) {
	c, err := s.compose(agent, req)
	if err != nil {
		return nil, err
	}
	return &ComposeResult{
		EnvelopeFrom: c.envelopeFrom,
		Recipients:   c.envelope,
		SentAs:       c.sentAs,
		Method:       "smtp",
		Raw:          c.sentBody,
		To:           c.to,
		CC:           c.cc,
		BCC:          c.bcc,
	}, nil
}

// SubmitOnce submits the persisted Sent-folder bytes in a SINGLE SMTP attempt
// (River owns retries) and returns the provider Message-ID. It attaches two
// wire-time headers post-DKIM (never in the signed header set):
//
//   - X-E2A-Message-ID (delivery.MessageIDHeader) — the stable e2a correlation
//     marker (async-send-contract §3.1). SES overrides supplied Message-ID/Date
//     headers, but echoes original headers back in its notifications
//     (mail.headers, when "include original headers" is enabled on the
//     configuration set), so this is the value that correlates feedback for
//     the SMTP-accept↔mark-sent crash window. Unlike the config-set header SES
//     does NOT strip it — recipients see it too; it is deliberately a stable
//     public marker. Stamped at submit time (not compose time) so messages
//     accepted before this header existed still carry it on re-drive.
//
//   - X-SES-CONFIGURATION-SET — re-attached because raw_message is stored
//     WITHOUT it (SES strips it before delivery; the recipient/Sent-folder
//     copy must not carry it).
//
// Keeping the header logic here (not in the worker) means Send and the async
// path share one source of truth for what SES actually receives.
func (s *Sender) SubmitOnce(messageID, envelopeFrom string, recipients []string, sentBody []byte) (string, error) {
	return s.smtpRelay.SendOnce(envelopeFrom, recipients, s.applySESConfigSet(applyCorrelationHeader(sentBody, messageID)))
}

// applyCorrelationHeader prepends the X-E2A-Message-ID marker. The id is
// server-minted, but sanitize anyway — this is a header write. Empty id
// (defensive) = no header.
func applyCorrelationHeader(message []byte, messageID string) []byte {
	if messageID == "" {
		return message
	}
	return append([]byte(delivery.MessageIDHeader+": "+sanitizeHeaderValue(messageID)+"\r\n"), message...)
}

// compose runs the full recipient-normalization + From-gating + MIME-compose +
// DKIM + SES-header stage shared by Send / SendOnce / ComposeForAccept.
func (s *Sender) compose(agent *identity.AgentIdentity, req SendRequest) (*composed, error) {
	to, cc, bcc, envelope, err := NormalizeRecipients(agent, s.fromDomain, req)
	if err != nil {
		return nil, err
	}
	if req.Unsubscribe != nil {
		if req.Unsubscribe.Mode != "managed" || req.Unsubscribe.URL == "" {
			return nil, &ValidationError{Message: "managed unsubscribe URL is unavailable"}
		}
		if len(envelope) != 1 {
			return nil, &ValidationError{Message: "managed unsubscribe requires exactly one recipient"}
		}
		req.Body, req.HTMLBody = appendUnsubscribeFooter(req.Body, req.HTMLBody, agent.EmailAddress(), req.Unsubscribe.URL)
	}
	if err := ValidateAttachmentFilenames(req.Attachments); err != nil {
		return nil, err
	}
	if total := ComposedSize(req.Subject, req.Body, req.HTMLBody, req.Attachments); total > MaxComposedMessageBytes {
		return nil, &ComposedSizeError{ActualBytes: total, MaxBytes: MaxComposedMessageBytes}
	}

	// Compose headers
	displayName := agent.Name
	if displayName == "" {
		displayName = agent.EmailAddress()
	}
	// Resolve the sending-verified gate once (it hits the sending_status store),
	// then derive both the header From and the envelope Return-Path from it.
	own := s.useOwnAddressFrom(agent)
	// Envelope MAIL FROM (Return-Path): the aligned custom MAIL FROM
	// (bounce.<domain>) for a verified domain — SPF authenticates the From
	// org-domain → no Gmail "via e2a" — else the e2a-owned relay address
	// (fail-closed: SPF passes for the relay, e2a captures bounces). Verified now
	// requires the custom MAIL FROM to be live, so the subdomain's MX exists and
	// bounces still reach SES's feedback handler.
	envelopeFrom := envelopeSender(own, agent.RegisteredDomainName(), s.fromDomain)
	// Header From: the agent's OWN address once sending-verified (DKIM-aligned →
	// DMARC passes, replies reach the agent directly); else the "… via e2a"
	// rewrite (fail-closed default).
	var headerFrom string
	sentAs := "relay"
	if own {
		headerFrom = fmt.Sprintf("%q <%s>", displayName, agent.EmailAddress())
		sentAs = "own_address"
	} else {
		headerFrom = fmt.Sprintf("%q <%s>", displayName+" via e2a", envelopeFrom)
	}
	// Reply-To defaults to the agent's own address so replies reach it directly;
	// a caller-supplied req.ReplyTo overrides that (e.g. routing replies to a
	// shared inbox or a different agent). Header-injection is neutralized by
	// sanitizeHeaderValue in the compose layer; address validity is enforced at
	// the API edge.
	replyTo := agent.EmailAddress()
	if req.ReplyTo != "" {
		replyTo = req.ReplyTo
	}

	var message []byte
	if len(req.Attachments) > 0 {
		message, err = ComposeMessageWithAttachments(headerFrom, to, cc, req.Subject, req.Body, req.HTMLBody, req.ReplyToMessageID, req.References, s.fromDomain, replyTo, req.ConversationID, req.Attachments)
	} else if req.HTMLBody != "" {
		message, err = ComposeMultipartMessage(headerFrom, to, cc, req.Subject, req.Body, req.HTMLBody, req.ReplyToMessageID, req.References, s.fromDomain, replyTo, req.ConversationID)
	} else {
		message, err = ComposeMessage(headerFrom, to, cc, req.Subject, req.Body, "text/plain", req.ReplyToMessageID, req.References, s.fromDomain, replyTo, req.ConversationID)
	}
	if err != nil {
		return nil, fmt.Errorf("compose message: %w", err)
	}
	if req.Unsubscribe != nil {
		message = addUnsubscribeHeaders(message, req.Unsubscribe.URL)
	}

	// Per-domain DKIM signing. Choose the signing
	// domain from the agent's verified custom domain when available —
	// for shared agents that falls back to s.fromDomain. Failures here
	// are logged and skipped: an unsigned message gets through the
	// deployment-level DKIM that the SMTP relay (SES) attaches at the
	// edge, which is what we did before this change anyway.
	if s.dkimLookup != nil {
		signingDomain := s.fromDomain
		if agent != nil && agent.DomainVerified && agent.RegisteredDomainName() != "" {
			signingDomain = agent.RegisteredDomainName()
		}
		if signed, ok := s.signMessage(message, signingDomain); ok {
			message = signed
		}
	}

	// Snapshot the recipient-facing bytes for the retained "Sent folder" copy:
	// DKIM-signed, but BEFORE the e2a-internal SES configuration-set header (SES
	// strips that before delivery, so the recipient never sees it).
	sentBody := message

	// Attach the SES configuration-set header (decision 9 / Slice 4b) so SES
	// publishes delivery/bounce/complaint events for this message. Prepended
	// AFTER DKIM signing so it is never in the signed header set (SES strips it
	// before delivery; signing it would break the signature). Empty when SES
	// event publishing is not configured (dev/self-host) — no header, no events.
	message = s.applySESConfigSet(message)

	return &composed{
		envelopeFrom: envelopeFrom,
		envelope:     envelope,
		wire:         message,
		sentBody:     sentBody,
		sentAs:       sentAs,
		to:           to,
		cc:           cc,
		bcc:          bcc,
	}, nil
}

// NormalizeRecipients is the single canonical envelope resolver used both for
// managed-unsubscribe binding and final MIME composition.
func NormalizeRecipients(agent *identity.AgentIdentity, fromDomain string, req SendRequest) ([]string, []string, []string, []string, error) {
	agentAliases := []string{strings.ToLower(agent.EmailAddress()), strings.ToLower(fmt.Sprintf("agent@%s", fromDomain))}
	to, err := normalizeAddrs(req.To)
	if err != nil {
		return nil, nil, nil, nil, &ValidationError{Message: fmt.Sprintf("invalid To address: %v", err)}
	}
	cc, err := normalizeAddrs(req.CC)
	if err != nil {
		return nil, nil, nil, nil, &ValidationError{Message: fmt.Sprintf("invalid CC address: %v", err)}
	}
	bcc, err := normalizeAddrs(req.BCC)
	if err != nil {
		return nil, nil, nil, nil, &ValidationError{Message: fmt.Sprintf("invalid BCC address: %v", err)}
	}
	to, cc, bcc = removeAddrs(to, agentAliases), removeAddrs(cc, agentAliases), removeAddrs(bcc, agentAliases)
	to, cc, bcc = dedupe(to), dedupe(cc), dedupe(bcc)
	cc = removeAddrs(cc, to)
	bcc = removeAddrs(bcc, to)
	bcc = removeAddrs(bcc, cc)
	if len(to) == 0 && len(cc) == 0 {
		return nil, nil, nil, nil, &ValidationError{Message: "no valid recipients"}
	}
	envelope := append(append(append(make([]string, 0, len(to)+len(cc)+len(bcc)), to...), cc...), bcc...)
	return to, cc, bcc, envelope, nil
}

// applySESConfigSet prepends the X-SES-CONFIGURATION-SET header when configured,
// returning the wire bytes SES receives. Prepended AFTER DKIM signing so it is
// never in the signed header set (SES strips it before delivery; signing it would
// break the signature). Empty config = no header (dev/self-host without SES).
func (s *Sender) applySESConfigSet(message []byte) []byte {
	if s.sesConfigSet == "" {
		return message
	}
	return append([]byte("X-SES-CONFIGURATION-SET: "+s.sesConfigSet+"\r\n"), message...)
}

// signMessage looks up a DKIM keypair for the given domain and returns
// a signed copy of the message. Returns (nil, false) when no key is
// stored for the domain or when signing fails — callers proceed with
// the unsigned message rather than failing the send.
func (s *Sender) signMessage(message []byte, domain string) ([]byte, bool) {
	if s.dkimLookup == nil || domain == "" {
		return nil, false
	}
	selector, privKey, err := s.dkimLookup.GetDKIMKeyInternal(context.Background(), domain)
	if err != nil {
		log.Printf("[sender] dkim key lookup for %s: %v", domain, err)
		return nil, false
	}
	if selector == "" || len(privKey) == 0 {
		return nil, false
	}
	signed, err := dkim.Sign(message, domain, selector, privKey)
	if err != nil {
		log.Printf("[sender] dkim sign for %s failed (sending unsigned): %v", domain, err)
		return nil, false
	}
	return signed, true
}

// normalizeAddrs parses and lowercases a list of email addresses.
// Returns an error if any address is unparseable.
func normalizeAddrs(addrs []string) ([]string, error) {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		parsed, err := mail.ParseAddress(a)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", a, err)
		}
		if err := ValidateMailboxAddress(parsed.Address); err != nil {
			return nil, fmt.Errorf("%q: %w", a, err)
		}
		out = append(out, strings.ToLower(parsed.Address))
	}
	return out, nil
}

const (
	maxSMTPLocalPartOctets = 64
	maxSMTPMailboxOctets   = 254 // 256-byte SMTP path limit minus "<" and ">"
)

// ValidateMailboxAddress enforces SMTP's octet limits on a parsed addr-spec.
// Unicode code-point limits alone are insufficient: a syntactically valid
// SMTPUTF8 local part can occupy four bytes per rune and become an indivisible
// header token that exceeds both the mailbox and header-line limits.
func ValidateMailboxAddress(address string) error {
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return fmt.Errorf("mailbox must contain a local part and domain")
	}
	if n := len(address[:at]); n > maxSMTPLocalPartOctets {
		return fmt.Errorf("mailbox local part is %d octets; maximum is %d", n, maxSMTPLocalPartOctets)
	}
	if n := len(address); n > maxSMTPMailboxOctets {
		return fmt.Errorf("mailbox is %d octets; maximum is %d", n, maxSMTPMailboxOctets)
	}
	return nil
}

// removeAddrs removes any address in exclude from addrs (case-insensitive).
func removeAddrs(addrs []string, exclude []string) []string {
	if len(exclude) == 0 {
		return addrs
	}
	set := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		set[strings.ToLower(e)] = struct{}{}
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if _, ok := set[strings.ToLower(a)]; !ok {
			out = append(out, a)
		}
	}
	return out
}

// dedupe removes duplicate addresses preserving order (case-insensitive).
func dedupe(addrs []string) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		key := strings.ToLower(a)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			out = append(out, a)
		}
	}
	return out
}
