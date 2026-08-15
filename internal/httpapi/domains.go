package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/mailfrom"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

// DNSRecord is one row in the unified DomainView.DNSRecords array. It supersedes
// the legacy split of `dns_records` (an mx/txt/dkim object) + `sending_dns_records`
// (an array): every record the customer must publish — inbound and sending — now
// carries its own `purpose` and `status`, mirroring how peers (Resend/SendGrid)
// model DNS setup. MX records carry their priority in the dedicated `priority`
// field (TXT records leave it null) rather than embedding it in `value`.
type DNSRecord struct {
	Type     string `json:"type" doc:"DNS record type. MX or TXT."`
	Name     string `json:"name" doc:"Record name (host). The apex domain for ownership/inbound_mx; an FQDN for dkim/mail_from records."`
	Value    string `json:"value" doc:"Record value. For MX records this is the mail-server host only; the priority is in the priority field."`
	Priority *int   `json:"priority" doc:"MX priority. Null for non-MX records."`
	Purpose  string `json:"purpose" doc:"What the record is for. Open set; tolerate unknown values. Known values: ownership, inbound_mx, inbound_mx_wildcard, dkim, mail_from_mx, mail_from_spf. inbound_mx_wildcard is the OPTIONAL wildcard MX (*.<domain>) that routes inbound mail for every subdomain to the e2a relay in one record; publish it only if you run agents on subdomains of this domain (MX records do not inherit)."`
	Status   string `json:"status" doc:"Persisted verification state of this DNS record — the stored domain state, updated when verification checks and the SES reconciler run, NOT a live DNS probe result. Open set; tolerate unknown values. Known values: verified (record confirmed), pending (not yet confirmed — awaiting publish/propagation or an SES result), missing (documented for forward compatibility; not currently emitted), failed (verification failed, or pending exceeded its TTL). Inbound records (ownership, inbound_mx) become verified once inbound verification passes, which requires BOTH the ownership TXT and the inbound MX. Sending records reflect their own SES axis: the dkim record follows the DKIM axis, while mail_from_mx and mail_from_spf follow the custom MAIL FROM axis, so a domain with working DKIM but a broken MAIL FROM (or the reverse) shows exactly which record to fix rather than failing all three. Before SES has reported a per-axis result (pre-provision rows) the sending records fall back to the all-or-nothing sending_status rollup; consult sending_error for the failure reason. The domain-level sending_status field remains the all-or-nothing rollup summary. POST /v1/domains/{domain}/verify reports the LIVE probe outcome for the same records in probe vocabulary (found/missing/deferred/mismatch) — persisted state and live probe outcome are deliberately distinct axes; do not map one vocabulary onto the other."`
}

// DomainCapabilities is the per-axis rollup of what a domain can actually do:
// receive mail (inbound) and send mail as its own address (outbound). The two
// axes are already independent in the data model — inbound is proven by the
// ownership TXT plus the inbound MX, while outbound is the async SES sending
// identity (DKIM + custom MAIL FROM) that the reconciler drives on its own
// schedule — and they are already reported separately per DNS record.
//
// Derived, never stored: `inbound` restates `verified` and `outbound` restates
// the `sending_status` rollup, so this object cannot drift from them. It exists
// so the two axes have one home whose name stays accurate as they diverge; the
// legacy `verified` boolean names only the inbound axis while reading as though
// it covered the domain as a whole.
type DomainCapabilities struct {
	Inbound  string `json:"inbound" doc:"Whether this domain can RECEIVE mail — the inbound axis. Restates the legacy verified boolean: verified once inbound verification has passed (which requires BOTH the ownership TXT and the inbound MX), pending otherwise. Open set; tolerate unknown values (new values may be added). Known values: verified, pending — those are the only two this axis emits, because it restates a boolean. There is deliberately NO inbound failure state today: a missing, unreachable, or wrong MX leaves the axis pending indefinitely rather than reporting failed, so treat pending as \"not proven yet\" and not as \"in flight\". Diagnose which record is at fault via dns_records[].status or the live probe on POST /v1/domains/{domain}/verify — unlike the outbound axis, there is no inbound equivalent of sending_error."`
	Outbound string `json:"outbound" doc:"Whether agents on this domain can SEND as their own address — the outbound axis. Restates the domain-level sending_status rollup over the async SES sending identity (DKIM + custom MAIL FROM). Open set; tolerate unknown values. Known values: none (not provisioned), pending (provisioning, or awaiting DNS publish/propagation), verified (agents send as their own address, DKIM-aligned), failed (consult sending_error). Independent of inbound: a domain can be outbound-pending while inbound is verified, and the per-record statuses in dns_records show which record to fix."`
}

type DomainView struct {
	Domain   string `json:"domain"`
	Verified bool   `json:"verified"`
	// Capabilities restates `verified` (inbound) and `sending_status` (outbound)
	// as one per-axis object. Prefer it over reading those two fields separately:
	// it is the surface that stays accurate as the axes diverge.
	Capabilities      DomainCapabilities `json:"capabilities"`
	VerificationToken string             `json:"verification_token"`
	// DNSRecords is the unified, purpose-tagged set of records the customer must
	// publish. ALL applicable records are returned at register time (they are
	// deterministic), so onboarding is a single paste — sending records do not
	// wait for the domain to verify.
	DNSRecords    []DNSRecord `json:"dns_records" nullable:"false"`
	CreatedAt     time.Time   `json:"created_at"`
	VerifiedAt    *time.Time  `json:"verified_at,omitempty"`
	LastCheckedAt *time.Time  `json:"last_checked_at,omitempty"`
	AgentCount    int         `json:"agent_count"`
	// Sender identity (decision 4 / Slice 4). Independent of `verified`
	// (inbound ownership): the async SES sending identity that lets agents
	// on this domain send as their own address. This is the rollup over the
	// dkim + mail_from_* records' per-record status; poll GET /domains/{domain}
	// to watch it go pending → verified|failed.
	SendingStatus        string     `json:"sending_status" doc:"Async SES sending-identity state (rollup). Open set; tolerate unknown values. Known values: none, pending, verified, failed."`
	SendingError         string     `json:"sending_error,omitempty"`
	SendingLastCheckedAt *time.Time `json:"sending_last_checked_at,omitempty"`
	// SendingRamp is platform-managed and deliberately read-only. Customers can
	// observe why accepted mail is waiting but cannot weaken the reputation guard.
	SendingRamp SendingRampView `json:"sending_ramp"`
}

type SendingRampView struct {
	Status                string     `json:"status" doc:"Platform-managed sending-ramp state. Open set; known values: inactive, ramping, complete, exempt."`
	DailyRecipientLimit   int        `json:"daily_recipient_limit" doc:"Current UTC-day recipient allowance. Zero means no ramp cap applies."`
	RecipientsUsedToday   int        `json:"recipients_used_today" doc:"Recipient capacity reserved for the current UTC day, including submissions whose provider outcome is still pending."`
	ResetsAt              *time.Time `json:"resets_at,omitempty"`
	ActiveDays            int        `json:"active_days" doc:"UTC days that reached the provider-accepted volume threshold."`
	RampDays              int        `json:"ramp_days"`
	EstimatedCompletionAt *time.Time `json:"estimated_completion_at,omitempty" doc:"Earliest estimated completion assuming every remaining UTC day reaches the provider-accepted volume threshold."`
}

func sendingRampView(snap sendramp.Snapshot, now time.Time) SendingRampView {
	status := snap.Status
	if status == "" {
		status = sendramp.StatusInactive
	}
	v := SendingRampView{
		Status:              status,
		DailyRecipientLimit: snap.DailyLimit,
		RecipientsUsedToday: snap.UsedToday,
		ActiveDays:          snap.ActiveDays,
		RampDays:            snap.RampDays,
	}
	if status == sendramp.StatusRamping {
		u := now.UTC()
		reset := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
		v.ResetsAt = &reset
		remaining := snap.RampDays - snap.ActiveDays
		if remaining < 0 {
			remaining = 0
		}
		// The final ramp-day cap remains in force through its UTC day. The
		// domain becomes complete on the following rollover, not immediately
		// after the final day reaches its confirmed-volume threshold.
		estimate := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, remaining+1)
		v.EstimatedCompletionAt = &estimate
	}
	return v
}

// domainView builds the unified DNS-records array. Status derivation:
//
//   - Inbound records (ownership, inbound_mx) follow `verified`: the domain is
//     either verified (TXT/MX confirmed) or pending.
//   - Sending records reflect their OWN persisted SES axis: the dkim record
//     follows SendingDkimStatus and the mail_from_* records follow
//     SendingMailFromStatus (via sendingAxisStatus), so a domain with good DKIM
//     but a broken MAIL FROM (or the reverse) shows which record to fix instead
//     of failing all three. When an axis is empty (pre-migration-049 rows, or
//     before the reconciler has recorded one) the record falls back to the
//     all-or-nothing `sending_status` rollup.
//
// mail_from_mx + mail_from_spf are emitted ONLY when the sending feature is
// enabled (SESRegion set) and are computed deterministically here (no SES call),
// so they appear at register time regardless of provisioning state. The httpapi
// layer must not import senderidentity (AWS SDK/River), so the MAIL FROM record
// shapes are mirrored locally from mailfrom.Domain + the region.
func (s *Server) domainView(d *identity.Domain) DomainView {
	sendingStatus := d.SendingStatus
	if sendingStatus == "" {
		sendingStatus = "none" // pre-migration-030 rows read as none
	}

	inboundStatus := "pending"
	if d.Verified {
		inboundStatus = "verified"
	}
	// Per-axis: each sending record reflects its own SES axis, falling back to
	// the all-or-nothing sending_status rollup when the axis is unset.
	dkimRec := sendingAxisStatus(d.SendingDkimStatus, sendingStatus)
	mailFromRec := sendingAxisStatus(d.SendingMailFromStatus, sendingStatus)
	mxPriority := 10

	records := []DNSRecord{
		// ownership — prove control of the domain (also drives the SPF check).
		{Type: "TXT", Name: d.Domain, Value: d.VerificationToken, Purpose: "ownership", Status: inboundStatus},
		// inbound_mx — route inbound mail to the e2a relay.
		{Type: "MX", Name: d.Domain, Value: s.deps.SMTPDomain, Priority: &mxPriority, Purpose: "inbound_mx", Status: inboundStatus},
		// inbound_mx_wildcard — OPTIONAL: route inbound mail for EVERY subdomain
		// (`*.<domain>`) to the e2a relay in one record. MX records do not
		// inherit, so an agent on a subdomain of this (verified) domain — e.g.
		// otto@acme.<domain> — only receives mail once a subdomain MX exists.
		// One wildcard covers all such subdomains. Only needed if the user runs
		// subdomain agents; send-only or apex-only setups can ignore it. It is not
		// independently probed or persisted, so it must never mirror the apex and
		// falsely claim verification.
		{Type: "MX", Name: "*." + d.Domain, Value: s.deps.SMTPDomain, Priority: &mxPriority, Purpose: "inbound_mx_wildcard", Status: "pending"},
	}

	// dkim — surfaced for rows that have a stored per-domain key (migration 014+).
	if d.DKIMSelector != "" && d.DKIMPublicKey != "" {
		name, value := dkim.DNSRecord(d.DKIMSelector, d.Domain, d.DKIMPublicKey)
		records = append(records, DNSRecord{Type: "TXT", Name: name, Value: value, Purpose: "dkim", Status: dkimRec})
	}

	// mail_from_* — the custom MAIL FROM subdomain's MX + SPF, deterministic and
	// returned regardless of provisioning state, but only when sending is enabled.
	if s.deps.SESRegion != "" {
		mf := mailfrom.Domain(d.Domain)
		records = append(records,
			DNSRecord{Type: "MX", Name: mf, Value: fmt.Sprintf("feedback-smtp.%s.amazonses.com", s.deps.SESRegion), Priority: &mxPriority, Purpose: "mail_from_mx", Status: mailFromRec},
			DNSRecord{Type: "TXT", Name: mf, Value: "v=spf1 include:amazonses.com ~all", Purpose: "mail_from_spf", Status: mailFromRec},
		)
	}

	return DomainView{
		Domain:   d.Domain,
		Verified: d.Verified,
		// Reuses the same two locals the DNS records are stamped from, so the
		// object can never disagree with dns_records[].status or the rollup.
		Capabilities: DomainCapabilities{
			Inbound:  inboundStatus,
			Outbound: sendingStatus,
		},
		VerificationToken:    d.VerificationToken,
		DNSRecords:           records,
		CreatedAt:            d.CreatedAt,
		VerifiedAt:           d.VerifiedAt,
		LastCheckedAt:        d.LastCheckedAt,
		AgentCount:           d.AgentCount,
		SendingStatus:        sendingStatus,
		SendingError:         d.SendingError,
		SendingLastCheckedAt: d.SendingLastCheckedAt,
		SendingRamp:          SendingRampView{Status: sendramp.StatusInactive},
	}
}

// sendingAxisStatus derives a sending record's per-record status from its OWN
// persisted SES axis (sending_dkim_status for the dkim record;
// sending_mail_from_status for the mail_from_* records). When the axis is empty
// it falls back to the all-or-nothing sending_status rollup, so pre-migration-049
// rows and domains that have not yet been polled behave exactly as before. SES
// reports the DKIM and custom MAIL FROM axes independently, so this is what lets
// a domain with good DKIM but a broken MAIL FROM (or the reverse) surface the
// specific failed record instead of failing all three.
func sendingAxisStatus(axis, sendingStatus string) string {
	if axis == "" {
		return sendingRecordStatus(sendingStatus)
	}
	return sendingRecordStatus(axis)
}

// sendingRecordStatus maps a sending status value (a domain-level rollup OR a
// single SES axis) onto the per-record status carried by the sending records
// (dkim, mail_from_*). The records are deterministic and shown before
// provisioning, so an unprovisioned (none) or in-flight (pending) value reads as
// `pending`; a hard failure as `failed`; success as `verified`. Unknown values
// fall through to `pending` (open set).
func sendingRecordStatus(sendingStatus string) string {
	switch sendingStatus {
	case "verified":
		return "verified"
	case "failed":
		return "failed"
	default:
		return "pending"
	}
}

// enqueueSenderProvision schedules SES sending-identity provisioning for a
// previously verified domain when the dep is wired (no-op otherwise). Newly
// verified domains use VerifyDomain's transactional outbox; this best-effort
// path is only the explicit forced refresh.
func (s *Server) enqueueSenderProvision(ctx context.Context, domain string) {
	if s.deps.EnqueueSenderProvision != nil {
		s.deps.EnqueueSenderProvision(ctx, domain)
	}
}

// DomainCheckResult is the live-DNS diagnostic surfaced by verify.
type DomainCheckResult struct {
	TXTFound bool
	MX       string
	SPF      string
	DKIM     string
}

// VerifyDomainView mirrors the legacy VerifyDomainResponse.
//
// The mx/spf/dkim fields report the LIVE DNS probe outcome of this
// verification attempt in probe vocabulary (found/missing/deferred/mismatch).
// This is deliberately a different axis — and a different word set — from the
// PERSISTED per-record state in DomainView.dns_records[].status
// (verified/pending/missing/failed): a probe answers "what did DNS return just
// now", the persisted status answers "what has the platform recorded about
// this record". Keep the two vocabularies distinct; see domains_vocab_test.go.
type VerifyDomainView struct {
	Domain     string     `json:"domain"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	MX         string     `json:"mx,omitempty" doc:"Live DNS probe outcome for the inbound MX record from THIS verification attempt — not the persisted domain state (that is dns_records[].status on GET /v1/domains/{domain}, which uses the deliberately distinct persisted vocabulary verified/pending/missing/failed). Open set; tolerate unknown values. Known values: found (an MX record on the apex domain points at the e2a relay host), missing (no apex MX points at the relay, or the DNS lookup failed). The MX probe gates verification together with the ownership TXT: verified flips true only when both are present."`
	SPF        string     `json:"spf,omitempty" doc:"Live DNS probe outcome for the apex SPF record from THIS verification attempt — not the persisted domain state (that is dns_records[].status on GET /v1/domains/{domain}, which uses the deliberately distinct persisted vocabulary verified/pending/missing/failed). Advisory diagnostic only: SPF does not gate verified. Open set; tolerate unknown values. Known values: found (an apex TXT record starts with v=spf1 and includes the e2a relay's send domain), missing (no such TXT record, or the DNS lookup failed). This probes the APEX SPF authorizing the relay; it is not the mail_from_spf record (the custom MAIL FROM subdomain's SPF), whose persisted state is reported in dns_records[].status."`
	DKIM       string     `json:"dkim,omitempty" doc:"Live DNS probe outcome for the domain's DKIM record ({selector}._domainkey.{domain}) from THIS verification attempt — not the persisted domain state (that is dns_records[].status on GET /v1/domains/{domain}, which uses the deliberately distinct persisted vocabulary verified/pending/missing/failed). Advisory diagnostic: DKIM does not gate verified. Open set; tolerate unknown values. Known values: found (a TXT at the selector carries a p= key equal to the issued one; a match wins over a stale key during rotation), missing (a keypair is issued but no p= payload is published at the selector, or the DNS lookup failed), deferred (the probe was skipped because no per-domain DKIM keypair is stored for this domain yet — legacy pre-keying rows; NOT a DNS-propagation wait), mismatch (a DKIM record IS published at the selector but its key doesn't match the issued one — almost always a truncated/clipped TXT: the value is ~400 chars and must be published in full, ending in 'AQAB'; re-publish the complete DKIM record, do not just wait)."`
}

// listDomainsOutput uses the shared Page[T] envelope (items + next_cursor). The
// list is keyset-paginated on (created_at, domain) — domain is the unique
// tiebreak. See listAgentsOutput.
type listDomainsOutput struct {
	Body Page[DomainView]
}

// listDomainsInput carries the standard cursor/limit (PageParams).
type listDomainsInput struct {
	PageParams
}
type domainOutput struct{ Body DomainView }
type domainCreateOutput struct{ Body DomainView }

// DomainParam is the path input. The domain segment is matched raw; chi/Huma
// URL-decode it.
type DomainParam struct {
	Domain string `path:"domain"`
}

func (s *Server) registerDomains() {
	registerOp(s.API, huma.Operation{
		OperationID: "listDomains", Method: http.MethodGet, Path: "/v1/domains",
		Summary: "List domains", Tags: []string{"domains"},
		Description: "List the domains owned by the authenticated account, newest first, with cursor pagination.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListDomains)

	registerOp(s.API, huma.Operation{
		OperationID: "getDomain", Method: http.MethodGet, Path: "/v1/domains/{domain}",
		Summary: "Get a domain", Tags: []string{"domains"},
		Security: []map[string][]string{{"bearer": {}}},
	}, s.handleGetDomain)

	registerOp(s.API, huma.Operation{
		OperationID: "registerDomain", Method: http.MethodPost, Path: "/v1/domains",
		Summary: "Register a domain", Tags: []string{"domains"},
		Security: []map[string][]string{{"bearer": {}}}, DefaultStatus: http.StatusCreated,
		Responses: map[string]*huma.Response{
			"402": s.limitExceededResponse(),
			"409": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Conflict — the domain is already claimed by another account (code domain_taken)."),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleRegisterDomain)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteDomain", Method: http.MethodDelete, Path: "/v1/domains/{domain}",
		Summary: "Delete a domain", Tags: []string{"domains"},
		Description: "Deletes the domain (refused with 400 domain_has_agents while any live or trashed agent exists on it) and commits durable teardown of its sending identity. The provider-side identity is normally removed before the response returns; otherwise sending_teardown is pending (durable retries, including provider-disabled managed identities) or manual_review (an identity exists but ownership cannot be established). Repeat the same DELETE after the domain row is gone to poll its owner-scoped durable receipt. Keep DNS published unless sending_teardown is confirmed; treat missing or unknown values as not confirmed. Re-registering the domain invalidates the old receipt. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain, sending_teardown}).",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleDeleteDomain)

	registerOp(s.API, huma.Operation{
		OperationID: "verifyDomain", Method: http.MethodPost, Path: "/v1/domains/{domain}/verify",
		Summary: "Verify a domain", Tags: []string{"domains"},
		Description: "Probe the domain's published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleVerifyDomain)
}

// verifyDomainOutput carries the diagnostic body. Both the verified and the
// not-yet-verified outcomes are 200 (Huma's default) — callers branch on
// VerifyDomainView.verified, never on the HTTP status.
type verifyDomainOutput struct {
	Body VerifyDomainView
}

func (s *Server) handleVerifyDomain(ctx context.Context, in *DomainParam) (*verifyDomainOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.deps.LookupDomain(ctx, in.Domain, user.ID)
	if err != nil || d == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "domain not found")
	}
	// Best-effort touch of last_checked_at — the probe runs regardless.
	if s.deps.TouchDomainChecked != nil {
		_ = s.deps.TouchDomainChecked(ctx, in.Domain, user.ID)
	}
	check := s.deps.VerifyProbe(in.Domain, d.VerificationToken, d.DKIMSelector, d.DKIMPublicKey)

	// Already verified: short-circuit but surface the latest diagnostic.
	// Still (re-)enqueue sender provisioning so this endpoint doubles as the
	// forced sending re-check for a domain whose sending_status is pending/failed.
	if d.Verified {
		s.enqueueSenderProvision(ctx, d.Domain)
		return &verifyDomainOutput{Body: VerifyDomainView{
			Domain: d.Domain, Verified: true, VerifiedAt: d.VerifiedAt,
			MX: check.MX, SPF: check.SPF, DKIM: check.DKIM,
		}}, nil
	}
	// Verification requires BOTH the ownership TXT and the inbound MX. The MX
	// gate (added with the per-record status array) is what makes
	// `inbound_mx.status: "verified"` honest: status is derived from the
	// domain's `verified` flag, so `verified` must actually imply the MX is
	// published — otherwise a TXT-only verify would claim a "verified" MX while
	// inbound mail silently bounces. A domain can't receive mail without the MX,
	// so requiring it for `verified` is also the correct inbound semantics.
	// Not-yet-published is the normal `verified:false` outcome — return 200 with
	// the diagnostic so callers see exactly which record is missing. This is NOT
	// an HTTP error (a missing TXT/MX is expected while DNS propagates); clients
	// poll by re-calling and branching on `verified`, never on the status code.
	if !check.TXTFound || check.MX != "found" {
		return &verifyDomainOutput{Body: VerifyDomainView{
			Domain: d.Domain, Verified: false, MX: check.MX, SPF: check.SPF, DKIM: check.DKIM,
		}}, nil
	}
	if err := s.deps.VerifyDomain(ctx, in.Domain, user.ID); err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to verify domain")
	}
	// Newly verified (inbound ownership): the production VerifyDomain dep
	// atomically commits the sender-identity job with this state transition.
	// Re-read for verified_at; fall back to the bare success shape.
	updated, err := s.deps.LookupDomain(ctx, in.Domain, user.ID)
	if err != nil || updated == nil {
		return &verifyDomainOutput{Body: VerifyDomainView{
			Domain: in.Domain, Verified: true, MX: check.MX, SPF: check.SPF, DKIM: check.DKIM,
		}}, nil
	}
	return &verifyDomainOutput{Body: VerifyDomainView{
		Domain: updated.Domain, Verified: true, VerifiedAt: updated.VerifiedAt,
		MX: check.MX, SPF: check.SPF, DKIM: check.DKIM,
	}}, nil
}

func (s *Server) handleListDomains(ctx context.Context, in *listDomainsInput) (*listDomainsOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	// The keyset tiebreak for domains is the domain string (its unique key), so
	// the cursor's `id` slot carries the after-domain.
	afterCreatedAt, afterDomain, err := s.decodeKeyset(user.ID, cursorDomains, in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	domains, err := s.deps.ListDomains(ctx, user.ID, limit+1, afterCreatedAt, afterDomain)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list domains")
	}
	hasMore := len(domains) > limit
	if hasMore {
		domains = domains[:limit]
	}
	items := make([]DomainView, 0, len(domains))
	for i := range domains {
		view, err := s.domainViewWithRamp(ctx, user.ID, &domains[i])
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to read sending ramp status")
		}
		items = append(items, view)
	}
	var nextCursor string
	if hasMore {
		last := domains[len(domains)-1]
		if nextCursor, err = s.encodeKeyset(user.ID, cursorDomains, last.CreatedAt, last.Domain); err != nil {
			return nil, err
		}
	}
	return &listDomainsOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleGetDomain(ctx context.Context, in *DomainParam) (*domainOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if in.Domain == "" {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "domain is required")
	}
	d, err := s.deps.LookupDomain(ctx, in.Domain, user.ID)
	if err != nil || d == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "domain not found")
	}
	view, err := s.domainViewWithRamp(ctx, user.ID, d)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to read sending ramp status")
	}
	return &domainOutput{Body: view}, nil
}

// RegisterDomainRequest is the create-domain body. `domain` is required (D-4):
// it is the only field, so leaving it optional generated an SDK signature that
// compiled with no domain and failed at runtime.
type RegisterDomainRequest struct {
	Domain string `json:"domain"`
}
type registerDomainInput struct {
	Body RegisterDomainRequest
}

func (s *Server) handleRegisterDomain(ctx context.Context, in *registerDomainInput) (*domainCreateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	normalized, err := agent.ValidateDomain(in.Body.Domain)
	if err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_domain", err.Error())
	}
	if s.deps.SharedDomain != "" && strings.EqualFold(normalized, s.deps.SharedDomain) {
		return nil, NewError(http.StatusBadRequest, "reserved_domain", "reserved domain")
	}
	// The create cap applies to creates. A same-owner claim is idempotent —
	// ClaimOrCreateDomain returns the existing row untouched — so charging it
	// against max_domains 402s a caller for re-POSTing a domain they already
	// hold, naming a resource the request would not have consumed.
	//
	// "Already owned" is decided by the same user-scoped lookup GET uses, so
	// this cannot become a bypass: another account's row is invisible to it
	// and stays charged (then rejected by the claim as a conflict), and a
	// parent/child claim is a genuinely new row that no exact-match lookup
	// finds. Anything other than a clean hit — dep unwired, lookup failed,
	// nil row — falls through to enforcing, so a limit is never skipped by
	// accident. The check is deliberately outside the claim transaction: it
	// is the same non-transactional shape the cap already had, so two racing
	// creates could still both pass, exactly as before this change.
	alreadyOwned := false
	if s.deps.LookupDomain != nil {
		if existing, lookupErr := s.deps.LookupDomain(ctx, normalized, user.ID); lookupErr == nil && existing != nil {
			alreadyOwned = true
		}
	}
	if !alreadyOwned && s.deps.EnforceDomainCreate != nil {
		if err := s.deps.EnforceDomainCreate(ctx, user.ID); err != nil {
			if env, ok := limitEnvelope(err); ok {
				return nil, env
			}
			return nil, NewError(http.StatusInternalServerError, "internal_error", "limits check failed")
		}
	}
	d, err := s.deps.ClaimDomain(ctx, normalized, user.ID)
	if err != nil {
		if errors.Is(err, identity.ErrReservedDomain) {
			return nil, NewError(http.StatusBadRequest, "reserved_domain", "reserved domain")
		}
		if errors.Is(err, identity.ErrDomainTaken) {
			return nil, NewError(http.StatusConflict, "domain_taken", "domain is already claimed by another account")
		}
		// Any non-taken failure here is a store/lookup error, not a client
		// error (ClaimOrCreateDomain returns a domain, ErrDomainTaken, or a
		// wrapped DB error — never nil, nil), so it is a 500 — the former
		// 400 "domain_unavailable" misclassified it as the caller's fault.
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to register domain")
	}
	view, err := s.domainViewWithRamp(ctx, user.ID, d)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to read sending ramp status")
	}
	return &domainCreateOutput{Body: view}, nil
}

func (s *Server) domainViewWithRamp(ctx context.Context, userID string, d *identity.Domain) (DomainView, error) {
	view := s.domainView(d)
	if s.deps.SendingRampSnapshot == nil {
		return view, nil
	}
	now := time.Now().UTC()
	snapshot, err := s.deps.SendingRampSnapshot(ctx, userID, d.Domain, now)
	if err != nil {
		return DomainView{}, err
	}
	view.SendingRamp = sendingRampView(snapshot, now)
	return view, nil
}

type deleteDomainOutput struct{ Body DeleteDomainResult }

// deleteDomainInput adds the confirmation guard (D-5). Deleting a domain
// deprovisions its SES sending identity and breaks sending for every agent on
// it, so it requires ?confirm=DELETE — uniform with deleteAccount/deleteAgent.
type deleteDomainInput struct {
	Domain string `path:"domain"`
	DeleteConfirm
}

func (s *Server) handleDeleteDomain(ctx context.Context, in *deleteDomainInput) (*deleteDomainOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.deps.LookupDomain(ctx, in.Domain, user.ID); err != nil {
		if s.deps.LookupDomainTeardown != nil {
			teardown, receiptErr := s.deps.LookupDomainTeardown(ctx, in.Domain, user.ID)
			if receiptErr == nil {
				return &deleteDomainOutput{Body: DeleteDomainResult{Deleted: true, Domain: in.Domain, SendingTeardown: string(teardown)}}, nil
			}
			if !errors.Is(receiptErr, pgx.ErrNoRows) {
				return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to read domain teardown receipt")
			}
		}
		return nil, NewError(http.StatusNotFound, "not_found", "domain not found")
	}
	// Confirm is enforced declaratively by Huma (required + enum:[DELETE] on
	// DeleteConfirm): a missing/wrong ?confirm is a 422 before this handler.
	live, trashed, err := s.deps.CountAgentsOnDomain(ctx, in.Domain, user.ID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to check domain agents")
	}
	if live > 0 || trashed > 0 {
		return nil, NewError(http.StatusBadRequest, "domain_has_agents", domainHasAgentsMessage(live, trashed))
	}
	teardown, err := s.deps.DeleteDomain(ctx, in.Domain, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrDomainHasAgents):
			return nil, NewError(http.StatusBadRequest, "domain_has_agents", "cannot delete domain while agents exist — delete its agents first (including any in the trash: they hold the address until restored or permanently deleted)")
		case errors.Is(err, identity.ErrDomainNotFound):
			return nil, NewError(http.StatusNotFound, "not_found", "domain not found")
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to delete domain")
		}
	}
	return &deleteDomainOutput{Body: DeleteDomainResult{Deleted: true, Domain: in.Domain, SendingTeardown: string(teardown)}}, nil
}

// domainHasAgentsMessage explains WHICH agents are blocking a domain delete.
//
// Both live and trashed agents block it — the FK is ON DELETE NO ACTION and a
// trashed agent is still a row that owns its address for the 30-day restore
// window. But the two need different remedies, and a trashed agent does not
// appear in list_agents, so a generic "agents exist" sends the caller hunting
// for agents they cannot see. Naming the count, the state, and the escape turns
// a dead end into a signpost.
func domainHasAgentsMessage(live, trashed int) string {
	switch {
	case live > 0 && trashed > 0:
		return fmt.Sprintf(
			"cannot delete domain: %d live and %d trashed agent(s) still on it. Delete the live ones, then purge the trashed ones (DELETE /v1/agents/{email}?confirm=DELETE&permanent=true) — a trashed agent keeps its address for the 30-day restore window, so it still blocks the domain.",
			live, trashed)
	case live > 0:
		return fmt.Sprintf(
			"cannot delete domain: %d agent(s) still on it. Delete them first, then delete the domain.",
			live)
	default:
		return fmt.Sprintf(
			"cannot delete domain: no live agents, but %d agent(s) on it are in the TRASH and still hold their addresses for the 30-day restore window. They will not appear in list_agents — list them with the deleted filter. To proceed, permanently delete them (DELETE /v1/agents/{email}?confirm=DELETE&permanent=true), then delete the domain.",
			trashed)
	}
}
