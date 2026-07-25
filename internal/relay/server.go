package relay

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/piguard"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
)

type Server struct {
	smtpServer *smtp.Server
	store      *identity.Store
	// outbox is the transactional event log. The inbound trigger writes the
	// messages row and the webhook_events row in a single transaction (per
	// design §4.2); the drain fans out to subscribers and enqueues River jobs.
	outbox   webhookpub.Outbox
	hub      *ws.Hub
	usage    usage.UsageTracker
	enforcer limits.Enforcer // optional; when nil, inbound caps are not enforced
	// screen is the content-screening engine (Slice 4). Runs the per-agent inbound
	// scan when inbound_scan='on'. Always non-nil (built with the dependency-free
	// heuristics detector); external providers plug in behind the same interface.
	screen     *piguard.Engine
	smtpDomain string
	// outboundFromDomain is the domain used in envelope MAIL FROM for mail we
	// originate (e.g. "send.e2a.dev"). Inbound messages whose envelope MAIL FROM
	// matches this domain are trusted same-platform traffic and may surface
	// X-E2A-Conversation-ID directly; external senders fall back to the
	// In-Reply-To lookup so they cannot forge conversation IDs.
	outboundFromDomain string
	// inboundAsync routes inbound through the queue-first River pipeline
	// (E2A_INBOUND_MODE=async): the session durably accepts to inbound_intake +
	// enqueues a processing job before 250 instead of processing inline. A nil
	// inboundEnq forces the synchronous path regardless (fail-safe).
	inboundAsync bool
	inboundEnq   InboundEnqueuer
	authenticate AuthenticationChecker
	// proxyTrusted is the compiled form of cfg.SMTP.ProxyTrustedCIDRs. When
	// non-empty, ListenAndServe wraps the TCP listener so only peers in these
	// CIDRs may present a PROXY protocol header (see proxy.go).
	proxyTrusted []netip.Prefix
	// metrics records the SMTP acceptance SLI (docs/observability.md). Optional;
	// nil (the default) disables recording. The cmd/e2a runtime always sets it.
	metrics Metrics
}

// Metrics is the narrow slice of telemetry.Metrics the relay emits. Injectable
// so tests don't need a real backend; satisfied by *telemetry.Log / telemetry.NoOp.
type Metrics interface {
	// SMTPInbound records the terminal outcome of one SMTP intake decision.
	// outcome ∈ {accepted, accepted_dedup, tempfail, rejected_unknown_recipient,
	// rejected_unverified_domain, rejected_quota}; seconds is DATA processing
	// time (0 for RCPT-stage rejections, which have no DATA phase).
	SMTPInbound(outcome string, seconds float64)
}

// AuthenticationChecker evaluates the connection and message identities used
// by the inbound trust gate. It is injectable so end-to-end tests can exercise
// deterministic DMARC-pass paths without depending on public DNS.
type AuthenticationChecker func(context.Context, net.IP, string, string, []byte, emailauth.AuthorIdentity) *emailauth.Authentication

// InboundEnqueuer inserts the inbound_process job in the SMTP accept-tx (the same
// transaction as the inbound_intake insert). *inboundprocess.Jobs satisfies it.
// Injected via SetInboundEnqueuer; nil keeps inbound on the synchronous path.
type InboundEnqueuer interface {
	EnqueueInboundProcessTx(ctx context.Context, tx pgx.Tx, intakeID string) (int64, error)
}

// SetInboundEnqueuer wires the shared River client's inbound enqueuer, enabling the
// queue-first accept path when E2A_INBOUND_MODE=async.
func (s *Server) SetInboundEnqueuer(e InboundEnqueuer) { s.inboundEnq = e }

// SetOutbox wires the transactional outbox. The inbound trigger commits the
// messages row and the webhook_events outbox row in a single transaction (per
// design §4.2); the drain fans out and enqueues River delivery jobs.
func (s *Server) SetOutbox(o webhookpub.Outbox) { s.outbox = o }

// SetAuthenticationChecker replaces the default DNS-backed evaluator. This is
// intended for deterministic tests; production leaves the default in place.
func (s *Server) SetAuthenticationChecker(check AuthenticationChecker) {
	if check != nil {
		s.authenticate = check
	}
}

// SetEnforcer wires in the resource-limits enforcer used to reject
// inbound recipients whose owner has hit the message-flow or storage
// cap. When nil (the default) every RCPT TO is accepted as far as the
// limits subsystem is concerned — handy for tests and for self-host
// operators who run without limits enabled. The cmd/e2a runtime always
// sets it.
func (s *Server) SetEnforcer(e limits.Enforcer) { s.enforcer = e }

// SetMetrics wires in the SLI recorder. When nil (the default) recording is a
// no-op — handy for tests and self-host operators running without telemetry.
func (s *Server) SetMetrics(m Metrics) { s.metrics = m }

// recordSMTPInbound is the nil-safe recording seam every intake outcome goes
// through. Units: exactly one accepted/accepted_dedup/tempfail call per DATA
// transaction (never per recipient); rejected_* calls are per rejected RCPT
// command — one transaction can emit several rejections and still accept.
func (s *Server) recordSMTPInbound(outcome string, seconds float64) {
	if s.metrics != nil {
		s.metrics.SMTPInbound(outcome, seconds)
	}
}

func NewServer(cfg *config.Config, store *identity.Store, usage usage.UsageTracker, hub *ws.Hub) *Server {
	s := &Server{
		store:              store,
		hub:                hub,
		usage:              usage,
		screen:             buildScreenEngine(),
		smtpDomain:         cfg.SMTP.Domain,
		outboundFromDomain: cfg.OutboundSMTP.FromDomain,
		inboundAsync:       cfg.Inbound.Mode == "async",
		authenticate:       emailauth.CheckAuthenticationForAuthorWithHELO,
	}

	be := &backend{relay: s}
	smtpSrv := smtp.NewServer(be)
	smtpSrv.Addr = cfg.SMTP.ListenAddr
	smtpSrv.Domain = cfg.SMTP.Domain
	smtpSrv.ReadTimeout = 30 * time.Second
	smtpSrv.WriteTimeout = 30 * time.Second
	smtpSrv.MaxMessageBytes = 10 * 1024 * 1024 // 10MB
	smtpSrv.MaxRecipients = 50
	smtpSrv.AllowInsecureAuth = !cfg.IsProduction()

	if cfg.SMTP.TLSCert != "" && cfg.SMTP.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SMTP.TLSCert, cfg.SMTP.TLSKey)
		if err != nil {
			log.Fatalf("failed to load TLS cert: %v", err)
		}
		smtpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	s.smtpServer = smtpSrv

	// config.Load runs Validate, which rejects malformed CIDRs before NewServer
	// can be reached; a parse failure here therefore just leaves proxyTrusted
	// nil (PROXY parsing disabled) rather than changing NewServer's signature —
	// but warn so a bypassed Validate doesn't silently drop the feature.
	trusted, err := parseTrustedCIDRs(cfg.SMTP.ProxyTrustedCIDRs)
	if err != nil {
		log.Printf("smtp proxy_trusted_cidrs: %v — PROXY parsing disabled (config.Validate normally rejects this at startup)", err)
	}
	s.proxyTrusted = trusted

	return s
}

// geminiDetectorTimeout / geminiDetectorEnabled / buildScreenEngine moved to
// internal/inboundscreen (shared with the loopback inbound-leg screeners);
// these thin aliases keep this package's call sites and internal tests stable.
const geminiDetectorTimeout = inboundscreen.GeminiDetectorTimeout

func geminiDetectorEnabled() bool { return inboundscreen.GeminiDetectorEnabled() }

func buildScreenEngine() *piguard.Engine { return inboundscreen.BuildEngine() }

func (s *Server) ListenAndServe() error {
	l, err := net.Listen("tcp", s.smtpServer.Addr)
	if err != nil {
		return err
	}
	if len(s.proxyTrusted) > 0 {
		log.Printf("SMTP PROXY protocol enabled for %d trusted CIDR(s)", len(s.proxyTrusted))
		l = wrapProxyListener(l, s.proxyTrusted)
	}
	log.Printf("SMTP relay listening on %s", s.smtpServer.Addr)
	return s.smtpServer.Serve(l)
}

func (s *Server) Close() error {
	return s.smtpServer.Close()
}

// SMTP backend implementation

type backend struct {
	relay *Server
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	var remoteIP net.IP
	if addr, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		remoteIP = addr.IP
	} else {
		// Reachable only via a trusted proxy peer presenting a non-TCP-family
		// PROXY header; the session degrades to SPF=none, so make the
		// misbehaving front-end visible.
		log.Printf("relay: non-TCP remote address %v (%T); SPF will be skipped for this session", c.Conn().RemoteAddr(), c.Conn().RemoteAddr())
	}
	sid := newSessionID()
	log.Printf("[%s] new session from %s", sid, remoteIP)
	return &session{relay: b.relay, id: sid, remoteIP: remoteIP, heloDomain: c.Hostname()}, nil
}

func newSessionID() string {
	b := make([]byte, 4)
	crand.Read(b)
	return hex.EncodeToString(b)
}

type session struct {
	relay      *Server
	id         string
	from       string
	recipients []string
	remoteIP   net.IP
	heloDomain string
}

func (s *session) AuthPlain(username, password string) error {
	return nil // Auth handled at identity layer, not SMTP layer
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	log.Printf("[%s] [%s] MAIL FROM", s.id, from)
	return nil
}

func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("[%s] [%s] RCPT TO: %s", s.id, s.from, to)

	// Reject unknown or unverified recipients at SMTP level.
	// The sender's mail server will generate a bounce notification.
	ctx := context.Background()
	agent, err := s.relay.resolveAgent(ctx, to)
	if err != nil {
		log.Printf("[%s] [%s] rejecting %s: no agent found", s.id, s.from, to)
		s.relay.recordSMTPInbound("rejected_unknown_recipient", 0)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "recipient not found"}
	}
	if !agent.DomainVerified {
		log.Printf("[%s] [%s] rejecting %s: domain not verified", s.id, s.from, to)
		s.relay.recordSMTPInbound("rejected_unverified_domain", 0)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "recipient not found"}
	}

	// Reject at SMTP envelope time if the recipient's owner has hit the
	// message-flow or storage cap. 552 (mailbox quota exceeded, RFC 5321
	// §4.2.2) tells the upstream MTA to bounce the message back to the
	// original sender with a deliverable error rather than retry. We
	// fail open on enforcer errors (DB hiccup) so a transient outage
	// doesn't lose mail — the storage trigger is the safety net.
	if s.relay.enforcer != nil && agent.UserID != "" {
		if err := s.relay.enforcer.CheckMessageSend(ctx, agent.UserID); err != nil {
			if le, ok := limits.IsLimitExceeded(err); ok {
				log.Printf("[%s] [%s] rejecting %s: limit exceeded (%s)", s.id, s.from, to, le.Resource)
				s.relay.recordSMTPInbound("rejected_quota", 0)
				return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 2, 2}, Message: "mailbox quota exceeded"}
			}
			log.Printf("[%s] [%s] limits check error (failing open): %v", s.id, s.from, err)
		}
	}

	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	ctx := context.Background()
	// SLI clock: DATA processing time, measured from DATA entry to the terminal
	// accept/tempfail decision (recorded in deliverMessages / acceptInbound).
	start := time.Now()
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	// Extract threading info once, for the DATA log + the async accept path (the
	// per-recipient processing recomputes its own threadInfo from the raw body).
	threadInfo := extractThreadInfo(body)
	senderEmail := extractEmail(s.from) // prefer From header for the log
	if threadInfo.From != "" {
		senderEmail = threadInfo.From
	}
	log.Printf("[%s] [%s] DATA recipients=%v size=%d bytes", s.id, senderEmail, s.recipients, len(body))

	// Queue-first async path (E2A_INBOUND_MODE=async): durably accept the raw MIME to
	// inbound_intake + enqueue a River processing job, all before 250 — processing
	// happens off the SMTP critical path. Falls back to the synchronous inline path
	// when the enqueuer isn't wired (fail-safe).
	if s.relay.inboundAsync && s.relay.inboundEnq != nil {
		return s.acceptInbound(ctx, body, threadInfo, start)
	}
	return s.deliverMessages(ctx, body, start)
}

// deliverMessages resolves each recipient and processes the message synchronously
// (the E2A_INBOUND_MODE=sync path): it calls processInbound inline with no
// post-persist hook. A persist failure surfaces as a 451 so the sending MTA retries
// (never a silent 250). The async path enqueues to River instead (see the accept-tx).
//
// start is the DATA-entry time; the terminal accept/tempfail decision records ONE
// SMTPInbound observation per transaction (never per recipient — the loop is fan-out).
func (s *session) deliverMessages(ctx context.Context, body []byte, start time.Time) error {
	for _, rcpt := range s.recipients {
		in := inboundInput{
			Body:         body,
			EnvelopeFrom: extractEmail(s.from),
			HELODomain:   s.heloDomain,
			RemoteIP:     s.remoteIP,
			Recipient:    rcpt,
			TraceID:      s.id,
		}
		if derr := s.relay.processInbound(ctx, in, nil); derr != nil {
			if errors.Is(derr, identity.ErrRecipientGone) {
				continue // recipient's agent is gone — skip it (historical skip+continue)
			}
			// Persist failed — return a transient SMTP error so the sending MTA
			// retries the whole message (RFC 5321 §4.5.4.1) instead of us silently
			// losing it under a 250. Multi-recipient caveat: a retry re-delivers to
			// already-succeeded recipients (the sync path has no dedup) — duplicate
			// beats loss, and the queue-first path (E2A_INBOUND_MODE=async) dedups.
			log.Printf("[%s] persist failed for %s → 451 (sender will retry): %v", s.id, rcpt, derr)
			s.relay.recordSMTPInbound("tempfail", time.Since(start).Seconds())
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure storing message; please retry"}
		}
	}
	s.relay.recordSMTPInbound("accepted", time.Since(start).Seconds())
	return nil
}

// inboundInput is the connection-derived data processInbound needs — the same facts
// the live SMTP session holds and the intake row persists for the async worker (which
// cannot recompute EnvelopeFrom/RemoteIP after the session closes).
type inboundInput struct {
	Body         []byte
	EnvelopeFrom string    // SMTP MAIL FROM
	HELODomain   string    // SMTP HELO/EHLO identity (null-reverse-path SPF)
	RemoteIP     net.IP    // connecting IP (SPF); the worker parses the stored text form
	Recipient    string    // RCPT TO
	TraceID      string    // log correlation (session id / intake id)
	IntakeID     string    // durable async intake id; empty on synchronous SMTP
	IntakeJobID  int64     // River inbound_process job id; zero on synchronous SMTP
	IntakeAt     time.Time // durable queue acceptance time
}

// postPersistHook runs INSIDE processInbound's persist transaction, after the
// messages insert + event publish, given the new message id. The async worker passes
// MarkInboundIntakeProcessedTx so the intake flips to 'processed' atomically with the
// message (its idempotency gate); the sync path passes nil.
type postPersistHook func(ctx context.Context, tx pgx.Tx, messageID string) error

// processInbound runs the full inbound chain — parse, SPF/DKIM/DMARC, ingestion
// gate, content screening, persist, and event publish — for one recipient. It is the
// SINGLE implementation shared by the synchronous SMTP session and the async River
// worker (internal/inboundprocess); the post-persist hook is the only difference.
//
// Returns a non-nil error ONLY when the message could not be durably persisted (the
// caller maps that to a 451 / a River retry). A recipient that no longer resolves to
// an agent is a no-op (nil) — the mailbox is gone. Screening/metering fail open.
func (srv *Server) processInbound(ctx context.Context, in inboundInput, hook postPersistHook) error {
	// Recompute the connection-derived context from the raw bytes — identical whether
	// we are in the live session or replaying a persisted intake row.
	author := emailauth.ParseAuthorIdentity(in.Body)
	threadInfo := extractThreadInfoWithAuthor(in.Body, author)
	envelopeFrom := extractEmail(in.EnvelopeFrom)
	headerFrom := threadInfo.From
	// SPF/DKIM/DMARC against the TRUE envelope MAIL FROM (RFC 7208), not the From
	// header — else SPF-alignment is a tautology (adversarial review F5).
	authentication := srv.authenticate(ctx, in.RemoteIP, envelopeFrom, in.HELODomain, in.Body, author)
	log.Printf("[%s] [%s] email auth from %s (envelope %s): SPF=%s DKIM=%d DMARC=%s", in.TraceID, headerFrom, in.RemoteIP, envelopeFrom, authentication.SPF.Status, len(authentication.DKIM), authentication.DMARC.Status)

	agent, err := srv.resolveAgent(ctx, in.Recipient)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err // genuine transient resolve failure — retryable (451 / River)
	}
	if errors.Is(err, pgx.ErrNoRows) || agent == nil {
		// Recipient's agent was deleted between accept and processing — NOT a transient
		// error. Return the ErrRecipientGone sentinel: the async worker marks the
		// intake terminally (so it doesn't linger 'accepted' with orphaned raw MIME)
		// and the sync path skips the recipient. Returning a plain error here would
		// burn the whole retry envelope (~5.5h) and re-meter the undeliverable message
		// on every attempt. (GetAgentByID returns ErrNoRows, not (nil,nil), for a gone
		// agent — the nil check is defensive.)
		log.Printf("[%s] recipient %s no longer resolves to an agent — dropping", in.TraceID, in.Recipient)
		return identity.ErrRecipientGone
	}
	rcpt := in.Recipient
	body := in.Body

	messageID := identity.NewMessageID()

	// Inbound trust policy ingestion gate (decision 10 / Slice 7a). Evaluate the
	// agent's policy against the claimed From identity plus the DMARC verdict,
	// NOT Reply-To (attacker-controllable). A non-match is FLAGGED — still delivered,
	// marked on the row, and emits email.flagged so operators get a signal.
	//
	// senderResolvable fails the gate closed for shared-relay "via e2a" mail,
	// which authenticates but carries no per-agent identity (#299).
	policyDecision := inboundpolicy.EvaluateIngestion(agent.InboundPolicy, agent.InboundAllowlist, headerFrom, srv.senderResolvable(headerFrom), string(authentication.DMARC.Status))

	// Content screening (Slice 4): run the per-agent inbound scan and record the
	// audit trail (protection_events) + the denormalized verdict. Detection +
	// annotation only here — review/block holds are a later slice, so the message
	// still delivers.
	screenRes := srv.screenInbound(ctx, agent, messageID, headerFrom, body, authentication, policyDecision)

	lookup := func(ctx context.Context, ids []string) (string, error) {
		return srv.store.LookupConversationID(ctx, agent.ID, ids)
	}
	conversationID := resolveConversationID(ctx, threadInfo, srv.envelopeFromTrusted(in.EnvelopeFrom), lookup)

	// All inbound is persisted to the pollable inbox. There is no per-agent
	// webhook delivery anymore (push is via /v1/webhooks subscriptions), so
	// nothing is "pending delivery" — every received message starts unread.
	deliveryStatus := "unread"

	// Built inside the persistence transaction after lifecycle appends so every
	// push channel carries the exact committed canonical transition objects.
	var event webhookpub.Event

	// email.flagged (decision 10): fired in addition to email.received when the
	// ingestion policy didn't match, so operators get a signal that this message
	// is untrusted. Deterministic id keeps MTA retries idempotent.
	// email.flagged fires for a delivered gate-flag. When the message is HELD
	// (review/block), delivery is suppressed and email.flagged is not fired: a
	// block emits email.blocked, a review emits email.review_requested (below).
	var flaggedEvent *webhookpub.Event
	if policyDecision.Flagged && !screenRes.Hold {
		fe := webhookpub.NewEvent(webhookpub.EventEmailFlagged, agent.UserID, map[string]interface{}{
			"message_id":      messageID,
			"conversation_id": conversationID,
			"direction":       "inbound",
			"agent_email":     agent.EmailAddress(),
			// Carry the RFC 5322 and SMTP identities separately, plus the complete
			// authentication evidence used by the inbound policy.
			"header_from":    optionalString(headerFrom),
			"envelope_from":  optionalString(envelopeFrom),
			"authentication": authentication,
			"reply_to":       threadInfo.ReplyTo,
			"delivered_to":   rcpt,
			"subject":        threadInfo.Subject,
			"policy":         agent.InboundPolicy,
			"reason":         policyDecision.Reason,
		})
		fe.AgentID = agent.ID
		fe.ConversationID = conversationID
		fe.MessageID = messageID
		fe.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailFlagged)
		flaggedEvent = &fe
	}

	// email.blocked: fired when the applied action is block — the message is
	// accept-then-quarantined (review_rejected, dropped, no human). It is the only
	// signal a subscriber gets for a dropped inbound message, so emit it regardless of
	// producer. reason_source mirrors the protection_events audit vocabulary
	// (sender_gate / inbound_scan). Deterministic id keeps MTA retries idempotent.
	var blockedEvent *webhookpub.Event
	if screenRes.Blocked() {
		be := webhookpub.NewEvent(webhookpub.EventEmailBlocked, agent.UserID, map[string]interface{}{
			"message_id":      messageID,
			"conversation_id": conversationID,
			"agent_email":     agent.EmailAddress(),
			"direction":       "inbound",
			"header_from":     optionalString(headerFrom),
			"envelope_from":   optionalString(envelopeFrom),
			"authentication":  authentication,
			"delivered_to":    rcpt,
			"subject":         threadInfo.Subject,
			"reason":          screenRes.Reason,
			"reason_source":   screenRes.Denorm.ReviewReason,
		})
		be.AgentID = agent.ID
		be.ConversationID = conversationID
		be.MessageID = messageID
		be.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailBlocked)
		blockedEvent = &be
	}

	// email.review_requested: fired when the applied action is review — the message is
	// held as pending_review awaiting a human / TTL. The same event fires for
	// outbound HITL holds (direction-aware); carries the review TTL (approval_expires_at) and
	// reason_source so a subscriber can drive a review queue from push. Deterministic
	// id keeps MTA retries idempotent.
	var pendingReviewEvent *webhookpub.Event
	if screenRes.Review() {
		pe := webhookpub.NewEvent(webhookpub.EventEmailReviewRequested, agent.UserID, map[string]interface{}{
			"message_id":          messageID,
			"conversation_id":     conversationID,
			"agent_email":         agent.EmailAddress(),
			"direction":           "inbound",
			"header_from":         optionalString(headerFrom),
			"envelope_from":       optionalString(envelopeFrom),
			"authentication":      authentication,
			"delivered_to":        rcpt,
			"subject":             threadInfo.Subject,
			"reason":              screenRes.Reason,
			"reason_source":       screenRes.Denorm.ReviewReason,
			"approval_expires_at": screenRes.Denorm.ApprovalExpiresAt,
		})
		pe.AgentID = agent.ID
		pe.ConversationID = conversationID
		pe.MessageID = messageID
		pe.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailReviewRequested)
		pendingReviewEvent = &pe
	}

	// Record the inbound message with full content and its canonical
	// authentication evidence.
	// toRecipients/cc come from the parsed To:/Cc: headers and are the
	// same across every fan-out delivery for this inbound message.
	//
	// Message, canonical lifecycle, optional outbox rows, and async intake
	// completion share one transaction. A failure at any boundary leaves none of
	// them committed; synchronous SMTP returns 451 and async River safely retries.
	// When no outbox is wired, the same transaction still commits the message and
	// lifecycle ledger, preserving the test/self-host compatibility seam.
	var inboundMsg *identity.Message
	err = srv.store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		inboundMsg, txErr = srv.store.CreateInboundMessageAuthenticatedInTx(
			ctx, tx, messageID, agent.ID, identity.InboundAuth{HeaderFrom: headerFrom, EnvelopeFrom: envelopeFrom, Authentication: authentication}, rcpt,
			threadInfo.MessageID, threadInfo.Subject, conversationID,
			deliveryStatus, body,
			policyDecision.Flagged, policyDecision.Reason,
			threadInfo.To, threadInfo.CC, threadInfo.ReplyTo,
			screenRes.Denorm,
		)
		if txErr != nil {
			return txErr
		}

		correlations := messagelifecycle.SafeCorrelationIDs(map[string]string{
			"email_message_id": threadInfo.MessageID,
		})
		transitions := make([]messagelifecycle.MessageLifecycleTransition, 0, 3)
		accepted, txErr := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: messageID, DedupeKey: "acceptance", Direction: "inbound",
			ReasonCode:     messagelifecycle.ReasonAcceptanceInboundSMTP,
			CorrelationIDs: correlations, OccurredAt: inboundMsg.CreatedAt,
		})
		if txErr != nil {
			return txErr
		}
		transitions = append(transitions, accepted)

		authReason, txErr := messagelifecycle.AuthenticationReason(string(authentication.DMARC.Status))
		if txErr != nil {
			return txErr
		}
		authEvidence, txErr := messagelifecycle.SafeAuthenticationEvidence(authentication)
		if txErr != nil {
			return txErr
		}
		authenticated, txErr := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: messageID, DedupeKey: "authentication:dmarc", Direction: "inbound",
			ReasonCode: authReason, Evidence: authEvidence,
			CorrelationIDs: correlations, OccurredAt: inboundMsg.CreatedAt,
		})
		if txErr != nil {
			return txErr
		}
		transitions = append(transitions, authenticated)

		if in.IntakeID != "" {
			queuedAt := in.IntakeAt
			if queuedAt.IsZero() {
				queuedAt = inboundMsg.CreatedAt
			}
			queueCorrelations := map[string]string{}
			if in.IntakeJobID != 0 {
				queueCorrelations["job_id"] = strconv.FormatInt(in.IntakeJobID, 10)
			}
			queued, appendErr := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
				MessageID: messageID, DedupeKey: "queue:inbound_processing", Direction: "inbound",
				ReasonCode:     messagelifecycle.ReasonQueueInboundProcessing,
				CorrelationIDs: queueCorrelations, OccurredAt: queuedAt,
			})
			if appendErr != nil {
				return appendErr
			}
			transitions = append(transitions, queued)
		}

		payload := buildEmailReceivedPayload(
			messageID, conversationID, headerFrom, envelopeFrom, rcpt, threadInfo.Subject, threadInfo, authentication, agent,
			inboundMsg.CreatedAt, eventpayload.AttachmentMetadata(body),
		)
		payload.LifecycleTransitions = messagelifecycle.MergeTransitions(transitions, nil)
		event = webhookpub.NewEvent(webhookpub.EventEmailReceived, agent.UserID, payload)
		event.AgentID = agent.ID
		event.ConversationID = conversationID
		event.MessageID = messageID
		// Deterministic event identity makes a producer retry reuse the stored
		// envelope; redelivery never rebuilds lifecycle observations.
		event.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailReceived)

		// Held messages (review/block) are persisted but NOT delivered — suppress
		// the email.received push (the agent only ever sees released messages).
		if srv.outbox != nil && !screenRes.Hold {
			if txErr = srv.outbox.PublishTx(ctx, tx, event); txErr != nil {
				return txErr
			}
		}
		if srv.outbox != nil && flaggedEvent != nil {
			if txErr = srv.outbox.PublishTx(ctx, tx, *flaggedEvent); txErr != nil {
				return txErr
			}
		}
		if srv.outbox != nil && blockedEvent != nil {
			if txErr = srv.outbox.PublishTx(ctx, tx, *blockedEvent); txErr != nil {
				return txErr
			}
		}
		if srv.outbox != nil && pendingReviewEvent != nil {
			if txErr = srv.outbox.PublishTx(ctx, tx, *pendingReviewEvent); txErr != nil {
				return txErr
			}
		}
		// Async worker: flip the intake to 'processed' ATOMICALLY with the message
		// + events, so a crash re-drive finds 'processed' and no-ops (the worker's
		// idempotency gate). nil on the synchronous path.
		if hook != nil {
			if txErr = hook(ctx, tx, messageID); txErr != nil {
				return txErr
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, identity.ErrIntakeAlreadyProcessed) {
			// Benign at-least-once re-drive: a prior attempt already processed this
			// intake, so the persist tx rolled back (no duplicate). Not a failure —
			// the async worker treats this sentinel as done. Don't log it as an error.
			return err
		}
		// Do NOT swallow — surface to deliverMessages so the SMTP session returns a
		// 451 and the sender retries, instead of a 250 that silently drops the mail.
		log.Printf("[%s] [%s] failed to record inbound message: %v", in.TraceID, headerFrom, err)
		return err
	}
	_ = inboundMsg

	// Record inbound usage AFTER the message is durably persisted (fail-open — never
	// block inbound email). The async worker retries a failed attempt, so metering
	// before the persist tx would over-count — billing an undeliverable message once
	// per attempt. Post-persist + the worker's already-processed no-op gate ⇒ once per
	// delivered message. Mirrors the outbound send path.
	if agent.UserID != "" {
		srv.usage.RecordAndCheck(ctx, agent.UserID, agent.ID, agent.Domain, "inbound")
	}

	// Append the screening audit rows (gate + scan violations) best-effort. Soft-ref
	// + deterministic ids make this idempotent under MTA retry, so it's safe outside
	// the message transaction.
	srv.writeProtectionEvents(ctx, messageID, screenRes.Events)

	// Update contact-engagement counters for this sender, best-effort and OUTSIDE
	// the message transaction — same rationale as the metering above: nothing
	// about outreach bookkeeping may block or reject real inbound mail, and a
	// failed statement inside the tx would abort the persist. Drift is corrected
	// and reported by ReconcileEngagementCounts.
	//
	// Update-only: this never creates an engagement, so a stranger writing in
	// does not silently appear in anyone's outreach list. And because `replied`
	// is computed as last_inbound_at > first_outbound_at, mail that arrives
	// before any outbound correctly does not count as a reply.
	//
	// Unauthenticated senders and screened messages do not count as replies. Sender
	// authentication is mandatory because the From header is spoofable — a sender
	// could claim any identity (e.g. MAIL FROM anything, From: victim@example.com)
	// and falsely mark the agent as having replied to that contact. Held/blocked
	// messages never reached the agent, so they must not update the engagement record.
	if agent.UserID != "" && headerFrom != "" && authentication.Passed() && !screenRes.Hold && !screenRes.Blocked() {
		if _, rerr := srv.store.RecordInboundActivity(ctx, agent.UserID, agent.ID,
			headerFrom, conversationID, time.Now().UTC()); rerr != nil {
			log.Printf("[contacts] outreach reply counters not updated for %s: %v", messageID, rerr)
		}
	}

	slug, _, _ := strings.Cut(rcpt, "@")

	log.Printf("[mail:%s] dir=inbound from=%s to=%s slug=%s conv_id=%s subject=%q verified=%t",
		messageID, headerFrom, rcpt, slug, conversationID, threadInfo.Subject, authentication.Passed())

	// Inbound events (email.received + flagged/blocked/review_requested variants)
	// are written to the outbox (webhook_events) above, in the message tx; the
	// drain fans them out and enqueues River delivery jobs. No in-process
	// fan-out here.

	// Best-effort WebSocket notification for any connected agent. The
	// /v1/webhooks subscriber resource (driven by the outbox drain) is the
	// durable push path; WS is an opportunistic live-tail on top of it,
	// available to every agent regardless of how it's configured.
	//
	// The frame is the exact committed versioned envelope the webhook channel
	// delivers. Loading it after the transaction commits keeps the live-tail and
	// reconnect-drain paths byte-identical and ensures neither path rebuilds an
	// event under the durable event id.
	if srv.hub != nil && !screenRes.Hold && srv.hub.IsConnected(agent.ID) {
		if notification, loadErr := srv.store.GetEventEnvelope(ctx, messageID, webhookpub.EventEmailReceived); loadErr == nil && len(notification) > 0 {
			if srv.hub.Send(agent.ID, notification) {
				log.Printf("[mail:%s] ws_notify=sent slug=%s", messageID, slug)
			}
		} else {
			log.Printf("[mail:%s] ws_notify=envelope_unavailable slug=%s err=%v", messageID, slug, loadErr)
		}
	}
	return nil
}

// The envelope wrapper ({type, id, schema_version, created_at, data}) is added
// by the publisher when it marshals the Event; this helper only produces the
// typed data subfield (the canonical eventpayload.EmailReceivedData — the same
// struct the WebSocket channel emits, so webhook and WS payloads are identical
// for the same event).
//
// buildEmailReceivedPayload builds the email.received event data. The event is a
// metadata-only NOTIFICATION, not a content carrier: it omits the message body
// (raw_message) that an earlier revision embedded. A subscriber fetches the full
// message — body + attachment bytes — from GET /v1/agents/{recipient}/messages/{message_id}
// using the message_id + recipient carried here (the same notify→fetch model the
// WebSocket listener already uses). This keeps the fan-out bus payload bounded,
// avoids shipping full message PII to every subscriber endpoint, and makes the
// REST resource the single source of truth for content. Attachment METADATA
// (never bytes) does ride on the event — the same extraction the message views
// use, so the indexes agree with the attachment-fetch endpoint.
//
// Authentication evidence stays on the event as metadata. The webhook envelope
// signature authenticates the complete payload in transit.
//
// receivedAt is injected (not time.Now() here) so the golden-fixture test can
// assert the marshaled payload byte-for-byte.
func buildEmailReceivedPayload(
	messageID, conversationID, headerFrom, envelopeFrom, recipient, subject string,
	threadInfo threadInfo,
	authentication *emailauth.Authentication,
	agent *identity.AgentIdentity,
	receivedAt time.Time,
	attachments []eventpayload.AttachmentMetaView,
) eventpayload.EmailReceivedData {
	to := threadInfo.To
	if to == nil {
		to = []string{} // required field: present-but-empty, never null
	}
	cc := threadInfo.CC
	if cc == nil {
		cc = []string{}
	}
	replyTo := threadInfo.ReplyTo
	if replyTo == nil {
		replyTo = []string{}
	}
	return eventpayload.EmailReceivedData{
		MessageID:      messageID,
		AgentEmail:     agent.EmailAddress(),
		Direction:      "inbound",
		ConversationID: conversationID,
		HeaderFrom:     optionalString(headerFrom),
		EnvelopeFrom:   optionalString(envelopeFrom),
		VerifiedDomain: authentication.VerifiedDomain(),
		To:             to,
		CC:             cc,
		ReplyTo:        replyTo,
		Authentication: authentication,
		DeliveredTo:    recipient,
		Subject:        subject,
		// The raw time.Time marshals to RFC3339Nano, matching the precision of
		// the envelope created_at.
		ReceivedAt:  receivedAt,
		Attachments: attachments,
	}
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func splitEmail(addr string) (local, domain string) {
	parts := strings.SplitN(addr, "@", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return addr, ""
}

func (s *session) Reset() {
	s.from = ""
	s.recipients = nil
}

func (s *session) Logout() error {
	return nil
}

// resolveConversationID applies the precedence used to recover the
// application-level thread ID for an inbound message:
//  1. Trusted X-E2A-Conversation-ID header (envelope MAIL FROM matches our
//     own relay). External senders can't forge this because the gate keeps
//     the header from being honored outside same-platform traffic.
//  2. In-Reply-To / References lookup, scoped to the recipient agent's own
//     messages. Covers external replies that thread off our outbound.
//  3. "" (no thread context available).
//
// lookup may be nil in tests; it is invoked only when lookup IDs are present.
func resolveConversationID(ctx context.Context, info threadInfo, trusted bool, lookup func(context.Context, []string) (string, error)) string {
	if info.ConversationID != "" && trusted {
		return info.ConversationID
	}
	var lookupIDs []string
	if info.InReplyTo != "" {
		lookupIDs = append(lookupIDs, info.InReplyTo)
	}
	lookupIDs = append(lookupIDs, info.References...)
	if len(lookupIDs) == 0 || lookup == nil {
		return ""
	}
	cid, err := lookup(ctx, lookupIDs)
	if err != nil {
		return ""
	}
	return cid
}

// envelopeFromTrusted reports whether the SMTP MAIL FROM for this session
// originated from our own outbound relay. Used to gate trust of custom
// X-E2A-* headers that carry app-level context across the SMTP boundary.
//
// Matches the exact configured domain OR any subdomain of it. The subdomain
// case is needed because SES rewrites the envelope MAIL FROM to a bounce
// address on a "MAIL FROM Domain" subdomain (e.g. configured outbound is
// send.e2a.dev but the envelope comes back as
// <bounce-id>@mail.send.e2a.dev). We own all subdomains of our outbound
// domain, so trusting them is the same trust boundary.
func (srv *Server) envelopeFromTrusted(envelopeFrom string) bool {
	if srv.outboundFromDomain == "" {
		return false
	}
	envDomain := strings.ToLower(extractDomain(envelopeFrom))
	trusted := strings.ToLower(srv.outboundFromDomain)
	return envDomain == trusted || strings.HasSuffix(envDomain, "."+trusted)
}

// senderResolvable reports whether the inbound From identity maps to a SPECIFIC
// authenticated sender, so a per-agent inbound allowlist/domain gate can match it.
//
// Mail relayed for a non-sending-verified agent goes out under the shared
// "agent@<outboundFromDomain>" address (internal/outbound/sender.go): it
// authenticates against the relay domain (DMARC passes) but is the SAME address
// for every such agent, so it carries no per-agent identity. Treating it as
// resolvable would let any allowlist that names the relay domain (or that shared
// address) admit every unverified agent indiscriminately — and would never admit
// the specific agent the operator meant. So we report it unresolvable and the
// gate fails closed under allowlist/domain (open is unaffected). #299.
//
// Matches the exact relay domain OR any subdomain, mirroring envelopeFromTrusted:
// a verified agent always sends from its own custom domain, never the relay's.
func (srv *Server) senderResolvable(senderEmail string) bool {
	if srv.outboundFromDomain == "" {
		return true
	}
	dom := strings.ToLower(extractDomain(senderEmail))
	relay := strings.ToLower(srv.outboundFromDomain)
	return dom != relay && !strings.HasSuffix(dom, "."+relay)
}

func extractEmail(addr string) string {
	if parsed, err := mail.ParseAddress(addr); err == nil {
		return parsed.Address
	}
	return addr
}

type threadInfo struct {
	MessageID      string
	Subject        string
	InReplyTo      string
	References     []string
	From           string   // From header (human-readable sender, not SMTP envelope)
	ReplyTo        []string // Reply-To header addresses — empty when absent (RFC 5322 allows multiple)
	To             []string // To: header addresses (one row per fan-out target sees the same list)
	CC             []string // Cc: header addresses
	ConversationID string   // X-E2A-Conversation-ID header, if present
}

// extractThreadInfo parses threading headers from a raw RFC 2822 message.
func extractThreadInfo(body []byte) threadInfo {
	return extractThreadInfoWithAuthor(body, emailauth.ParseAuthorIdentity(body))
}

func extractThreadInfoWithAuthor(body []byte, author emailauth.AuthorIdentity) threadInfo {
	msg, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return threadInfo{}
	}
	var refs []string
	if r := msg.Header.Get("References"); r != "" {
		for _, ref := range strings.Fields(r) {
			refs = append(refs, ref)
		}
	}
	// Use the same single-author parser as DMARC. Ambiguous From fields are not
	// projected as a sender identity.
	fromHeader := author.Address
	return threadInfo{
		MessageID:  msg.Header.Get("Message-Id"),
		Subject:    decodeMIMEHeader(msg.Header.Get("Subject")),
		InReplyTo:  msg.Header.Get("In-Reply-To"),
		References: refs,
		From:       fromHeader,
		// RFC 5322 § 3.6.2 allows multiple addresses in Reply-To. Mirror
		// the same parser used for To/Cc so display names get stripped
		// the same way and the field shape is uniform across consumers.
		ReplyTo:        extractAddressList(msg.Header.Get("Reply-To")),
		To:             extractAddressList(msg.Header.Get("To")),
		CC:             extractAddressList(msg.Header.Get("Cc")),
		ConversationID: msg.Header.Get("X-E2A-Conversation-Id"),
	}
}

// decodeMIMEHeader decodes any RFC 2047 encoded-word runs in a header value
// (e.g. `=?utf-8?Q?caf=C3=A9?=` → `café`). Used for display fields like
// Subject so downstream consumers (DB, list summaries, dashboard) see the
// UTF-8 form instead of the wire-encoded form. If decoding fails the original
// value is returned — preserving the field is better than dropping it.
func decodeMIMEHeader(v string) string {
	if v == "" {
		return v
	}
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(v)
	if err != nil {
		return v
	}
	return decoded
}

// extractAddressList parses an RFC 5322 address-list header (To/Cc) into bare
// email addresses, dropping display names. Returns nil if the header is empty
// or unparseable; group addresses and malformed entries are silently skipped.
func extractAddressList(header string) []string {
	if header == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(header)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Address != "" {
			out = append(out, a.Address)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func extractDomain(addr string) string {
	email := extractEmail(addr)
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// resolveAgent looks up an agent by exact recipient email match only.
func (s *Server) resolveAgent(ctx context.Context, rcpt string) (*identity.AgentIdentity, error) {
	email := extractEmail(rcpt)
	return s.store.GetAgentByID(ctx, email)
}
