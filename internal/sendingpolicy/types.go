package sendingpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file is the closed vocabulary of provider authorization: who is asking,
// which pools they consume, and what a caller is allowed to hold in its hand
// between the decision and the socket.
//
// Every type here has unexported fields on purpose. A caller outside this
// package can hold a reference but cannot forge one, cannot widen one, and
// cannot read a decision out of one that the module did not put there. That is
// the whole security argument for the gate: authority is not a string a worker
// can assemble, it is a durable row this package validated under lock.

// Purpose is the closed set of reasons e2a may hand a message to SES. It is
// derived by a server-side constructor from a durable source row and persisted
// on the provider operation; no caller, River argument, or MIME header can
// select or change it. The values match the CHECK constraint on
// sending_provider_operations.purpose.
type Purpose string

const (
	// PurposeCustomerMessage is mail a customer's agent composed.
	PurposeCustomerMessage Purpose = "customer_message"
	// PurposeCustomerNotification is platform mail a customer's own action
	// triggered — HITL approval requests and webhook-health warnings. It is
	// attributed to and budgeted against that customer precisely because a
	// customer can cause an unbounded amount of it.
	PurposeCustomerNotification Purpose = "customer_notification"
	// PurposeCriticalOperational is a pause notice. It must survive an abuse
	// wave that has exhausted every customer pool, so it draws on its own.
	PurposeCriticalOperational Purpose = "critical_operational"
	// PurposeViolationOperational is a budget-violation or global-guardrail
	// notice. Separate from critical so limit-driven mail — which an attacker
	// can provoke — cannot starve pause notices.
	PurposeViolationOperational Purpose = "violation_operational"
	// PurposePublicFeedback is the unauthenticated /api/feedback fan-out to a
	// fixed configured recipient set. It has no customer to attribute to but
	// still shares provider reputation, so it consumes the global pools.
	PurposePublicFeedback Purpose = "public_feedback_notification"
	// PurposeTrustedSystem is first-party prober/conformance traffic from
	// system and internal accounts. Unbudgeted by design: it is not
	// customer-triggerable, so its compromise is a credential incident rather
	// than an abuse-policy question.
	PurposeTrustedSystem Purpose = "trusted_system"
)

func (p Purpose) valid() bool {
	switch p {
	case PurposeCustomerMessage, PurposeCustomerNotification,
		PurposeCriticalOperational, PurposeViolationOperational,
		PurposePublicFeedback, PurposeTrustedSystem:
		return true
	}
	return false
}

// isCustomer reports whether this purpose is attributed to, budgeted against,
// and blocked by a customer account's sending state.
func (p Purpose) isCustomer() bool {
	return p == PurposeCustomerMessage || p == PurposeCustomerNotification
}

// isOperational reports whether this purpose draws on one of the two
// operational pools rather than any customer pool.
func (p Purpose) isOperational() bool {
	return p == PurposeCriticalOperational || p == PurposeViolationOperational
}

// SystemPolicySubject is the fixed policy subject for operational and
// public-feedback mail. It is a sentinel reference rather than a real account
// row: no customer state may authorize or block a pause notice, and there is
// no users row to pause. Phase 7 binds it to the e2a-system SES tenant.
const SystemPolicySubject = "e2a-system"

// Scope is a budget counter dimension. The values match the CHECK constraint
// on sending_budget_counters.scope.
type Scope string

const (
	ScopeGlobalAll          Scope = "global_all"
	ScopeGlobalProbation    Scope = "global_probation"
	ScopeAccountDaily       Scope = "account_daily"
	ScopeAccountSharedDaily Scope = "account_shared_daily"
	ScopeGlobalCritical     Scope = "global_critical"
	ScopeGlobalViolation    Scope = "global_violation"
)

// Fixed scope IDs for the pools that are not keyed by an account.
const (
	scopeIDAllCustomers = "all-customers"
	scopeIDProbation    = "probation"
	scopeIDCritical     = "critical-operational"
	scopeIDViolation    = "violation-operational"
)

// scopeLockRank is the normative lock order for budget counters, and the only
// place that order is written down as data. Every transaction that touches
// more than one counter sorts by it.
//
// Ordering is not a performance detail here. Two workers that acquire
// global_all and account_daily in opposite orders deadlock under exactly the
// load this system exists to survive, and Postgres resolves that by killing
// one transaction — which, on the final authorization path, means a message
// that should have been held instead errors out. An operation may skip a key
// it does not need, but it may never reorder the keys it does take.
var scopeLockRank = map[Scope]int{
	ScopeGlobalAll:          1,
	ScopeGlobalProbation:    2,
	ScopeAccountDaily:       3,
	ScopeAccountSharedDaily: 4,
	ScopeGlobalCritical:     5,
	ScopeGlobalViolation:    6,
}

// counterKey identifies one row of sending_budget_counters.
type counterKey struct {
	Scope   Scope
	ScopeID string
}

// sortCounterKeys puts a set of scope keys into the normative lock order. Ties
// inside one scope (impossible today — every scope has one ID per operation)
// fall back to the scope ID so the order is total.
func sortCounterKeys(keys []counterKey) {
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := scopeLockRank[keys[i].Scope], scopeLockRank[keys[j].Scope]
		if ri != rj {
			return ri < rj
		}
		return keys[i].ScopeID < keys[j].ScopeID
	})
}

// Audience is the closed recipient class of a protection notice. Owner mail
// goes to the affected customer; operator mail goes to the version of the
// operator mailbox map that the runtime policy currently selects.
type Audience string

const (
	AudienceOwner    Audience = "owner"
	AudienceOperator Audience = "operator"
)

func (a Audience) valid() bool {
	return a == AudienceOwner || a == AudienceOperator
}

// AcceptanceDecision is what an API acceptance surface learns from
// PrepareExternalTx: whether this account may still queue outbound mail at all.
// It deliberately carries no budget verdict — budgets are decided immediately
// before the provider call, not at acceptance, so that a queued message is held
// rather than rejected when the account runs out of daily capacity.
type AcceptanceDecision string

const (
	// AcceptanceAccept means the send may be durably queued.
	AcceptanceAccept AcceptanceDecision = "accept"
	// AcceptanceSendingPaused means the account is paused; the caller rejects
	// the request rather than queueing mail that can never leave.
	AcceptanceSendingPaused AcceptanceDecision = "sending_paused"
)

// Reason codes for a hold. These are machine-readable and appear in metrics and
// lifecycle events, so they are part of the operator contract even though no
// public API surfaces them in this slice.
const (
	ReasonAccountPaused       = "account_paused"
	ReasonAccountDailyBudget  = "account_daily_budget_exhausted"
	ReasonAccountSharedBudget = "account_shared_daily_budget_exhausted"
	ReasonGlobalAllBudget     = "global_all_budget_exhausted"
	ReasonGlobalProbation     = "global_probation_budget_exhausted"
	ReasonGlobalCritical      = "global_critical_budget_exhausted"
	ReasonGlobalViolation     = "global_violation_budget_exhausted"
	ReasonTenantNotReady      = "ses_tenant_not_ready"
	ReasonRecipientSuperseded = "notice_recipient_superseded"
	ReasonAccountDeleted      = "account_deleted"
	// ReasonSourceUnavailable means the durable source row this operation was
	// derived from is gone, so there is nothing left to send.
	ReasonSourceUnavailable = "source_unavailable"
	// ReasonNoticeSettled means the notice delivery already reached a terminal
	// state; re-sending it would duplicate a logical notice.
	ReasonNoticeSettled = "notice_already_settled"
	// ReasonTenantUnnamed means the policy requires a tenant header but the
	// account has no tenant name to send.
	ReasonTenantUnnamed = "ses_tenant_unnamed"
	// ReasonClassChanged means the message's reputation class stopped matching
	// the immutable class its operation was derived from, so the operation no
	// longer describes the send.
	ReasonClassChanged = "reputation_class_changed"
)

// Decision is allow-or-hold. A hold carries the earliest time a retry could
// plausibly succeed — for a daily budget that is the next UTC midnight, which
// lets the worker snooze rather than spin.
//
// Terminal separates "come back later" from "this can never proceed". Without
// it every hold reads as retryable, and a worker faced with an operation that
// is permanently void — its account deleted, its notice already sent, its
// reputation class no longer the one it was derived from — would snooze on it
// forever instead of failing the message once. A terminal hold carries no
// RetryAt because there is no time at which the answer changes.
type Decision struct {
	Allow    bool
	Reason   string
	RetryAt  time.Time
	Terminal bool
}

func allowDecision() Decision { return Decision{Allow: true} }

func holdDecision(reason string, retryAt time.Time) Decision {
	return Decision{Allow: false, Reason: reason, RetryAt: retryAt}
}

// terminalHold is a hold no retry can clear. The caller must fail the message
// rather than reschedule it.
func terminalHold(reason string) Decision {
	return Decision{Allow: false, Reason: reason, Terminal: true}
}

// OperationRef names one durable provider operation. Purpose, attribution, and
// shared-reputation class are captured here for the caller's convenience, but
// they are advisory: every Gate method reloads the row under lock and uses the
// stored values, so a forged or stale ref grants exactly no authority.
type OperationRef struct {
	id            string
	purpose       Purpose
	sourceAccount string
	policySubject string
	shared        bool

	// recipients is set only by PreparePublicFeedback, whose configured
	// envelope has nowhere durable to live and never crosses a process
	// boundary. Every other purpose resolves its envelope from a locked row at
	// final authorization, and a reference that arrives here by any other
	// route carries none — which is what makes a deserialized feedback
	// reference useless rather than dangerous.
	recipients []string
}

// ID exposes the opaque operation identifier for logging and for the River
// argument round-trip. It is not a capability.
func (r OperationRef) ID() string { return r.id }

// Purpose exposes the derived purpose for metrics. Advisory, as above.
func (r OperationRef) Purpose() Purpose { return r.purpose }

// IsZero reports an unset reference.
func (r OperationRef) IsZero() bool { return r.id == "" }

// operationRefJSON is the versioned wire form. Only the ID crosses the
// boundary: a River job that outlives a process replacement must be able to
// find its operation again, and nothing more. Migration 113 wrote exactly this
// shape into legacy job args, so the version and key names are load-bearing.
type operationRefJSON struct {
	V  int    `json:"v"`
	ID string `json:"id"`
}

const operationRefVersion = 1

// MarshalJSON writes the versioned wire form.
func (r OperationRef) MarshalJSON() ([]byte, error) {
	if r.id == "" {
		return nil, errors.New("sendingpolicy: refusing to marshal an empty operation reference")
	}
	return json.Marshal(operationRefJSON{V: operationRefVersion, ID: r.id})
}

// UnmarshalJSON reads the versioned wire form and yields a reference carrying
// only an ID. The absent purpose/attribution fields are what force every Gate
// method to reload from the database instead of trusting deserialized state.
func (r *OperationRef) UnmarshalJSON(raw []byte) error {
	var wire operationRefJSON
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("sendingpolicy: decode operation reference: %w", err)
	}
	if wire.V != operationRefVersion {
		return fmt.Errorf("sendingpolicy: unsupported operation reference version %d", wire.V)
	}
	if strings.TrimSpace(wire.ID) == "" {
		return errors.New("sendingpolicy: operation reference has no id")
	}
	*r = OperationRef{id: wire.ID}
	return nil
}

// AttemptRef names one durable submission attempt: the module-allocated
// ordinal on a provider operation. River's own job.Attempt is deliberately not
// used — River retries and provider submissions are different clocks, and
// conflating them is how one logical message ends up making several
// unaccounted SES calls.
type AttemptRef struct {
	operationID string
	attempt     int

	// recipients carries PreparePublicFeedback's in-memory envelope from
	// Reserve to ConsumeAttempt. It is not part of the reference's identity:
	// the durable row is what every check reads, and a caller that fabricates
	// this field still cannot make ConsumeAttempt authorize an envelope the
	// operation's purpose does not permit.
	recipients []string
}

// OperationID exposes the operation this attempt belongs to, for logging.
func (a AttemptRef) OperationID() string { return a.operationID }

// Attempt exposes the durable submission ordinal, for logging.
func (a AttemptRef) Attempt() int { return a.attempt }

// IsZero reports an unset reference.
func (a AttemptRef) IsZero() bool { return a.operationID == "" || a.attempt <= 0 }

// NotificationSource is the closed set of durable rows that may produce a
// customer notification.
type NotificationSource string

const (
	// NotificationHITLMessage is a pending message awaiting human approval.
	NotificationHITLMessage NotificationSource = "hitl_message"
	// NotificationWebhookHealth is a webhook warning/disabled episode.
	NotificationWebhookHealth NotificationSource = "webhook_health"
)

// NotificationRef names one supported notification source row. It has no
// exported constructor taking a purpose: the two constructors below are the
// only way to make one, which is what stops a caller from labelling its own
// mail as operational to escape a budget.
type NotificationRef struct {
	source NotificationSource
	id     string
}

// NewHITLNotificationRef references a pending outbound message whose approval
// request is being sent. PrepareNotificationTx locks the owning agent and then
// the message, and requires the message to still be outbound and still awaiting
// review before deriving anything from it.
func NewHITLNotificationRef(messageID string) NotificationRef {
	return NotificationRef{source: NotificationHITLMessage, id: messageID}
}

// NewWebhookHealthNotificationRef references a webhook whose health episode is
// being reported to its owner.
func NewWebhookHealthNotificationRef(webhookID string) NotificationRef {
	return NotificationRef{source: NotificationWebhookHealth, id: webhookID}
}

// ProtectionNoticeRef names one already-committed notice event and audience.
// The event row must exist: notices are enqueued by the transaction that
// detects the violation, and the drain worker only ever resumes them.
type ProtectionNoticeRef struct {
	eventID  string
	audience Audience
}

// NewProtectionNoticeRef references one notice event/audience pair.
func NewProtectionNoticeRef(eventID string, audience Audience) ProtectionNoticeRef {
	return ProtectionNoticeRef{eventID: eventID, audience: audience}
}

// PublicFeedbackRef names one server-generated /api/feedback submission and the
// complete fixed recipient set configured for it. Request data reaches neither
// field: the ID is minted by the handler and the recipients come from
// configuration, so a submitter cannot add, replace, or redirect a recipient.
type PublicFeedbackRef struct {
	submissionID string
	recipients   []string
}

// NewPublicFeedbackRef references one submission and its configured envelope.
func NewPublicFeedbackRef(submissionID string, recipients []string) PublicFeedbackRef {
	return PublicFeedbackRef{submissionID: submissionID, recipients: append([]string(nil), recipients...)}
}

// SettlementOutcome is the closed set of authoritative provider results that
// move the ramp ledger. Retryable and ambiguous results are deliberately
// absent: they leave the reservation standing, because a message that might
// have been delivered must not release ramp capacity.
type SettlementOutcome string

const (
	// SettlementProviderAccepted means SES took responsibility for the message.
	SettlementProviderAccepted SettlementOutcome = "provider_accepted"
	// SettlementProviderPermanentlyRejected means SES definitively refused it.
	SettlementProviderPermanentlyRejected SettlementOutcome = "provider_permanently_rejected"
)

func (o SettlementOutcome) valid() bool {
	return o == SettlementProviderAccepted || o == SettlementProviderPermanentlyRejected
}

// ProviderSettlement pairs an attempt with its authoritative outcome.
type ProviderSettlement struct {
	Attempt AttemptRef
	Outcome SettlementOutcome
	// ProviderMessageID is the id SES assigned when it accepted the message.
	// It is bound to the attempt's feedback correlation so delivery feedback
	// that arrives by provider id — the common case — resolves to the same
	// attempt as feedback that arrives by the random attempt header. Only an
	// accepted settlement may carry one; a rejection has nothing to bind.
	ProviderMessageID string
}

// ErrProviderMessageIDConflict means an attempt is being settled with a
// different provider message id than the one already bound to it. One attempt
// is exactly one DATA transaction and SES assigns exactly one id to it, so a
// second, different id is evidence of two physical sends for one charged
// attempt — the invariant this module exists to hold — and is never absorbed.
var ErrProviderMessageIDConflict = errors.New("sendingpolicy: attempt already settled with a different provider message id")

// NormalizeProviderMessageID reduces a provider message id to the bare form the
// provider itself reports in delivery feedback.
//
// The SMTP relay returns SES's id angle-bracketed and qualified with the
// region domain (<id@us-east-2.amazonses.com>) because that is the on-wire
// Message-ID replies must anchor on; SES's SNS feedback carries the same id
// BARE. The correlation row exists so feedback can find its attempt, and two
// writers — the synchronous worker and the delayed feedback finalizer — must
// agree on one spelling or the second one is refused as a conflict. Every
// write and comparison goes through this function; readers should too.
func NormalizeProviderMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	if at := strings.IndexByte(id, '@'); at >= 0 {
		id = id[:at]
	}
	return strings.TrimSpace(id)
}

// TenantMode is the closed tenant-header state carried by an authorization.
// It is resolved under the final account-control lock, so a job that was
// enqueued before a tenant flip still submits with the post-flip header.
type TenantMode string

const (
	// TenantModeNone means no X-SES-TENANT header. This is every deployment
	// before phase 7 and every self-host.
	TenantModeNone TenantMode = "none"
	// TenantModeRequired means the exact named tenant must be sent.
	TenantModeRequired TenantMode = "required"
)

// ProviderHeaders is what the SMTP adapter is allowed to learn from a token.
// The adapter derives its provider-owned headers only from these values and
// never accepts them as separate parameters, which is what makes header
// smuggling a compile-time impossibility rather than a review question.
type ProviderHeaders struct {
	AttemptCorrelationID string
	TenantRequired       bool
	TenantName           string
}

// ProviderAuthorization is the single-use permission to make exactly one SES
// call for exactly one durable attempt. It cannot be constructed outside this
// package, cannot be widened, and is worthless without the durable nonce that
// RedeemProviderCall consumes.
type ProviderAuthorization struct {
	attempt       AttemptRef
	correlationID string
	purpose       Purpose
	nonce         string

	// recipients is the exact final authorized envelope, normalized and
	// deduplicated, in stable order; recipientSet is the same value as a
	// membership test. The durable provenance rows store keyed HMACs of these
	// addresses, never the addresses themselves, but the in-memory comparison
	// is over plaintext: the token already holds the envelope it authorized,
	// so hashing it again to compare it with itself would add ceremony, not
	// safety.
	recipients   []string
	recipientSet map[string]struct{}

	tenantMode TenantMode
	tenantName string

	// notice is set only for a protection notice, whose recipient is resolved
	// at final authorization rather than supplied by the caller.
	notice *noticeBinding
}

// noticeBinding carries the protection-notice identity a redemption must
// re-prove immediately before the socket opens.
//
// One version/commitment pair covers both audiences because both answer the
// same question — "is this still the right recipient?" — with different
// evidence. For an operator delivery the version is the logical mailbox-map
// version and the commitment is its keyed commitment, so rotating the map
// retires the attempt. For an owner delivery the version is the feedback HMAC
// key version and the commitment is the HMAC of the address itself, so an owner
// who edits their email retires it. Neither form stores an address.
type noticeBinding struct {
	eventID             string
	audience            Audience
	deliveryAttempt     int
	recipientVersion    int
	recipientCommitment []byte
}

// IsZero reports an unset authorization.
func (a ProviderAuthorization) IsZero() bool { return a.attempt.IsZero() }

// Attempt exposes the durable attempt this token authorizes, for logging.
func (a ProviderAuthorization) Attempt() AttemptRef { return a.attempt }

// Purpose exposes the derived purpose, for metrics.
func (a ProviderAuthorization) Purpose() Purpose { return a.purpose }

// AuthorizedRecipients returns a defensive copy of the exact final envelope.
//
// Only the protection notifier uses it: that path is the one caller that does
// not already know its recipient, because the address is resolved under lock at
// final authorization and deliberately never persisted in plaintext. Every
// other caller composed its own envelope and must not re-derive one here.
func (a ProviderAuthorization) AuthorizedRecipients() []string {
	out := make([]string, len(a.recipients))
	copy(out, a.recipients)
	return out
}

// ErrEnvelopeMismatch means the envelope a caller is about to submit is not the
// envelope that was authorized. It is returned before any redemption or network
// I/O, because a mismatch here is either a bug or an attempt to reuse one
// authorization for different recipients.
var ErrEnvelopeMismatch = errors.New("sendingpolicy: envelope does not match the authorized recipients")

// ValidateEnvelope proves the caller's actual envelope is the authorized one
// and returns the provider header values derived from the token.
//
// THE CONTRACT: submit exactly AuthorizedRecipients(). Ordering and letter case
// are free — those are presentation. The COUNT is not: the envelope must carry
// one entry per distinct mailbox, because the adapter issues one RCPT TO per
// entry and the budget charged one unit per distinct mailbox.
//
// So a caller must collapse its own To/Cc/Bcc overlap before submitting. That
// is a real constraint on the SMTP adapter — a reply-all naming the same
// mailbox in To and Cc is an ordinary message, and reassembling the raw header
// lists would be rejected here. Handing back AuthorizedRecipients() is not a
// workaround for that; it is the intended call, and it is the only envelope
// this token was ever priced for.
//
// The alternative — silently deduplicating whatever arrives — is what makes an
// envelope of fifty case-variant spellings of one mailbox pass as "the same
// recipient" while SES receives fifty RCPT TO commands. That is a 50x
// reputation amplifier on one unit of budget, which is precisely the quantity
// this module exists to bound.
func (a ProviderAuthorization) ValidateEnvelope(recipients []string) (ProviderHeaders, error) {
	if a.IsZero() {
		return ProviderHeaders{}, errors.New("sendingpolicy: authorization is empty")
	}
	normalized, raw, err := normalizeEnvelopeCounted(recipients)
	if err != nil {
		return ProviderHeaders{}, err
	}
	// The submission may not contain more entries than it has distinct
	// mailboxes. Collapsing duplicates here instead would be a 50x reputation
	// amplifier: one charged unit authorizes one mailbox, but an envelope of
	// fifty case-variant spellings of that mailbox normalizes down to the same
	// single authorized address while SES still receives fifty RCPT TO
	// commands — fifty chances to bounce, against one unit of budget. The
	// accounting rule that duplicates count once only holds if the thing
	// submitted is the deduplicated set, so that is what a caller must submit.
	if raw != len(normalized) {
		return ProviderHeaders{}, ErrEnvelopeMismatch
	}
	if len(normalized) != len(a.recipientSet) {
		return ProviderHeaders{}, ErrEnvelopeMismatch
	}
	for _, addr := range normalized {
		if _, ok := a.recipientSet[addr]; !ok {
			return ProviderHeaders{}, ErrEnvelopeMismatch
		}
	}
	return ProviderHeaders{
		AttemptCorrelationID: a.correlationID,
		TenantRequired:       a.tenantMode == TenantModeRequired,
		TenantName:           a.tenantName,
	}, nil
}

// normalizeEnvelope lowercases, deduplicates, and sorts an envelope recipient
// list, rejecting anything that is not a bare addr-spec.
//
// Lowercasing the whole address — local part included — is a deliberate
// choice. RFC 5321 permits case-sensitive local parts, but no mail system e2a
// submits to distinguishes them, and the value must dedupe and hash identically
// on both sides of the authorization boundary. Treating "A@x" and "a@x" as two
// mailboxes would charge one send twice.
//
// This function is the CHARGING side, where collapsing duplicates is right.
// ValidateEnvelope is the SUBMISSION side, where it is not: see the contract
// there.
func normalizeEnvelope(recipients []string) ([]string, error) {
	out, _, err := normalizeEnvelopeCounted(recipients)
	return out, err
}

// normalizeEnvelopeCounted also reports how many entries the caller actually
// supplied, so ValidateEnvelope can tell "the same mailbox listed in To and Cc"
// (which the caller must collapse before submitting) from "one entry per
// mailbox" (which is what a charged unit buys).
func normalizeEnvelopeCounted(recipients []string) ([]string, int, error) {
	if len(recipients) == 0 {
		return nil, 0, errors.New("sendingpolicy: envelope has no recipients")
	}
	seen := make(map[string]struct{}, len(recipients))
	out := make([]string, 0, len(recipients))
	raw := 0
	for _, entry := range recipients {
		addr, err := normalizeEnvelopeRecipient(entry)
		if err != nil {
			return nil, 0, err
		}
		raw++
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, 0, errors.New("sendingpolicy: envelope has no recipients")
	}
	sort.Strings(out)
	return out, raw, nil
}

// maxEnvelopeRecipientLength bounds one address. RFC 5321's path limit is 256
// including angle brackets; this is the bare addr-spec.
const maxEnvelopeRecipientLength = 254

// normalizeEnvelopeRecipient validates one customer-facing envelope recipient.
//
// This is deliberately laxer than the operator-mailbox rules in keyring.go.
// An operator address is one value we chose and can constrain to ASCII atext;
// a customer envelope recipient is whatever the world's mail systems accept,
// and rejecting a valid destination here would be an outage, not a control.
// What it still refuses is anything that could act as a separator in an SMTP
// or MIME grammar, because those are how one recipient becomes two.
func normalizeEnvelopeRecipient(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return "", errors.New("sendingpolicy: envelope recipient is empty")
	}
	if len(addr) > maxEnvelopeRecipientLength {
		return "", fmt.Errorf("sendingpolicy: envelope recipient is longer than %d bytes", maxEnvelopeRecipientLength)
	}
	for i := 0; i < len(addr); i++ {
		if c := addr[i]; c < 0x21 || c == 0x7f {
			return "", errors.New("sendingpolicy: envelope recipient contains a control or space byte")
		}
	}
	if strings.ContainsAny(addr, "<>,;\"\\()[]") {
		return "", errors.New("sendingpolicy: envelope recipient must be a bare address without display name or route syntax")
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return "", errors.New("sendingpolicy: envelope recipient must be local@domain")
	}
	if strings.IndexByte(addr, '@') != at {
		return "", errors.New("sendingpolicy: envelope recipient must contain exactly one @")
	}
	return strings.ToLower(addr), nil
}
