package delivery

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MessageIDHeader is the stable e2a correlation marker (async-send-contract
// §3.1 / async-message-pipeline §4): stamped on the outbound wire message at
// submit time (internal/outbound.Sender.SubmitOnce) and echoed back by SES in
// notification payloads (`mail.headers`) when the SES configuration set has
// "include original headers" enabled. SES overrides supplied Message-ID/Date
// headers on the wire, so this custom header is the only submit-time value
// that survives into notifications — it correlates feedback for the
// SMTP-accept↔mark-sent crash window, where the provider_message_id from the
// 250 response was never captured.
const MessageIDHeader = "X-E2A-Message-ID"

// EventKind is the normalized SES event category.
type EventKind string

const (
	KindDelivery      EventKind = "Delivery"
	KindBounce        EventKind = "Bounce"
	KindComplaint     EventKind = "Complaint"
	KindDeliveryDelay EventKind = "DeliveryDelay"
	KindSend          EventKind = "Send"
	KindReject        EventKind = "Reject"
	KindOther         EventKind = "Other"
)

// impliesProviderAcceptance reports whether this notification kind can only
// occur AFTER SES accepted the submission. Send/Delivery/DeliveryDelay/Bounce/
// Complaint all describe a message SES took responsibility for; Reject
// explicitly means SES refused the submission, and Other is unknown — neither
// is provider-accept evidence.
func (k EventKind) impliesProviderAcceptance() bool {
	switch k {
	case KindSend, KindDelivery, KindDeliveryDelay, KindBounce, KindComplaint:
		return true
	}
	return false
}

// RecipientOutcome is the delivery result for one address from one SES event.
type RecipientOutcome struct {
	Address  string
	Status   Status
	Detail   string
	Suppress bool // hard bounce or complaint → add to the suppression list
}

// Event is a normalized SES notification: which message, which event, and the
// per-recipient outcomes. The message rollup is the worst status across
// Recipients by Merge precedence.
type Event struct {
	Kind            EventKind
	SESMessageID    string // the mail.messageId — correlates to messages.provider_message_id
	ProviderEventID string
	OccurredAt      time.Time
	// E2AMessageID is the e2a message id echoed back by SES from the
	// MessageIDHeader stamped at submit time (`mail.headers` — present only
	// when the SES configuration set enables "include original headers").
	// It is the correlation fallback for the SMTP-accept↔mark-sent crash
	// window; empty when headers are absent or the marker isn't among them.
	E2AMessageID string
	Recipients   []RecipientOutcome
	// BounceType / BounceSubType carry the SES bounce classification (Bounce
	// events only; empty otherwise). BounceType is normalized to the stable
	// event vocabulary — permanent | transient | undetermined — the value
	// email.bounced's bounce_type field emits; BounceSubType is the raw SES
	// bounceSubType (e.g. General, NoEmail, MailboxFull).
	BounceType    string
	BounceSubType string
	// ComplaintSubType is the raw SES complaintSubType (Complaint events
	// only; empty otherwise). A suppression-list value means SES did not
	// deliver, which the detector must not count as a genuine complaint.
	ComplaintSubType string
	// AttemptCorrelationID is the ProviderAttemptHeader SES echoed back from
	// the submitted headers: the deletion-resistant correlation fallback.
	AttemptCorrelationID string
}

// sesNotification is the SES event JSON carried in the SNS Message field.
// Config-set event publishing uses `eventType`; legacy identity notifications
// use `notificationType` — accept either.
type sesNotification struct {
	EventType        string `json:"eventType"`
	NotificationType string `json:"notificationType"`
	Mail             struct {
		MessageID   string   `json:"messageId"`
		Destination []string `json:"destination"`
		// Headers is the submitted message's original header list, echoed by
		// SES when "include original headers" is enabled on the configuration
		// set. HeadersTruncated marks a partial list (very large header
		// blocks); whatever IS present is still usable.
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		HeadersTruncated bool `json:"headersTruncated"`
	} `json:"mail"`
	Bounce *struct {
		BounceType        string `json:"bounceType"` // Permanent | Transient | Undetermined
		BounceSubType     string `json:"bounceSubType"`
		BouncedRecipients []struct {
			EmailAddress   string `json:"emailAddress"`
			DiagnosticCode string `json:"diagnosticCode"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint *struct {
		ComplainedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"complainedRecipients"`
		ComplaintFeedbackType string `json:"complaintFeedbackType"`
		ComplaintSubType      string `json:"complaintSubType"`
	} `json:"complaint"`
	Delivery *struct {
		Recipients []string `json:"recipients"`
	} `json:"delivery"`
	DeliveryDelay *struct {
		DelayedRecipients []struct {
			EmailAddress   string `json:"emailAddress"`
			DiagnosticCode string `json:"diagnosticCode"`
		} `json:"delayedRecipients"`
	} `json:"deliveryDelay"`
	Reject *struct {
		Reason string `json:"reason"` // e.g. "Bad content"
	} `json:"reject"`
}

// ParseSESNotification parses the SES event JSON (the decoded SNS Message
// body) into a normalized Event. Returns an error for malformed JSON or a
// missing message id; an unrecognized event kind yields KindOther with no
// recipient outcomes (caller no-ops).
func ParseSESNotification(messageBody []byte) (*Event, error) {
	var n sesNotification
	if err := json.Unmarshal(messageBody, &n); err != nil {
		return nil, fmt.Errorf("parse SES notification: %w", err)
	}
	typ := n.EventType
	if typ == "" {
		typ = n.NotificationType
	}
	if n.Mail.MessageID == "" {
		return nil, fmt.Errorf("SES notification missing mail.messageId")
	}
	ev := &Event{SESMessageID: n.Mail.MessageID}
	for _, h := range n.Mail.Headers {
		switch {
		case strings.EqualFold(h.Name, MessageIDHeader) && ev.E2AMessageID == "":
			// Defensive trim: the marker is stamped bare, but tolerate an
			// angle-bracketed echo.
			ev.E2AMessageID = strings.Trim(strings.TrimSpace(h.Value), "<>")
		case strings.EqualFold(h.Name, ProviderAttemptHeader) && ev.AttemptCorrelationID == "":
			if v := strings.TrimSpace(h.Value); validAttemptCorrelationID(v) {
				ev.AttemptCorrelationID = v
			}
		}
	}

	switch typ {
	case "Delivery":
		ev.Kind = KindDelivery
		recips := n.Mail.Destination
		if n.Delivery != nil && len(n.Delivery.Recipients) > 0 {
			recips = n.Delivery.Recipients
		}
		for _, a := range recips {
			ev.Recipients = append(ev.Recipients, RecipientOutcome{Address: norm(a), Status: StatusDelivered})
		}
	case "Bounce":
		ev.Kind = KindBounce
		if n.Bounce != nil {
			ev.BounceType = normalizeBounceType(n.Bounce.BounceType)
			ev.BounceSubType = n.Bounce.BounceSubType
			// Only a Permanent (hard) bounce suppresses; Transient/Undetermined
			// are recorded as bounced but not auto-suppressed (decision 9: never
			// suppress on a single unverified/soft signal).
			hard := ev.BounceType == "permanent"
			for _, r := range n.Bounce.BouncedRecipients {
				ev.Recipients = append(ev.Recipients, RecipientOutcome{
					Address: norm(r.EmailAddress), Status: StatusBounced,
					Detail: r.DiagnosticCode, Suppress: hard,
				})
			}
		}
	case "Complaint":
		ev.Kind = KindComplaint
		if n.Complaint != nil {
			ev.ComplaintSubType = n.Complaint.ComplaintSubType
			for _, r := range n.Complaint.ComplainedRecipients {
				ev.Recipients = append(ev.Recipients, RecipientOutcome{
					Address: norm(r.EmailAddress), Status: StatusComplained,
					Detail: n.Complaint.ComplaintFeedbackType, Suppress: true,
				})
			}
		}
	case "DeliveryDelay":
		ev.Kind = KindDeliveryDelay
		if n.DeliveryDelay != nil {
			for _, r := range n.DeliveryDelay.DelayedRecipients {
				ev.Recipients = append(ev.Recipients, RecipientOutcome{
					Address: norm(r.EmailAddress), Status: StatusDeferred, Detail: r.DiagnosticCode,
				})
			}
		}
	case "Send":
		ev.Kind = KindSend
	case "Reject":
		// SES rejected the ALREADY-ACCEPTED message itself (e.g. a virus was
		// detected in the content) — a message-level verdict, so every envelope
		// recipient terminally fails; reject.reason carries the classification
		// ("Bad content"). NEVER suppresses: a Reject is about this message's
		// content, not the recipient addresses (decision 9 suppresses only on
		// hard bounce or complaint).
		ev.Kind = KindReject
		var reason string
		if n.Reject != nil {
			reason = n.Reject.Reason
		}
		for _, a := range n.Mail.Destination {
			ev.Recipients = append(ev.Recipients, RecipientOutcome{Address: norm(a), Status: StatusFailed, Detail: reason})
		}
	default:
		ev.Kind = KindOther
	}
	return ev, nil
}

func norm(addr string) string { return strings.ToLower(strings.TrimSpace(addr)) }

// maxE2AMessageIDLen bounds a plausible e2a message id ("msg_" + 32 hex chars
// today; headroom for future id shapes without accepting arbitrary strings).
const maxE2AMessageIDLen = 64

// validE2AMessageID reports whether s is shaped like an e2a message id
// (`msg_` prefix + [A-Za-z0-9_-] tail, bounded length) — the pre-lookup shape
// check on the header-echoed correlation marker. The value only ever arrives
// off a signature-verified SNS notification, but it originated inside a
// message header block, so it is validated before being used as a lookup key.
func validE2AMessageID(s string) bool {
	const prefix = "msg_"
	if !strings.HasPrefix(s, prefix) || len(s) <= len(prefix) || len(s) > maxE2AMessageIDLen {
		return false
	}
	for _, r := range s[len(prefix):] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// validAttemptCorrelationID reports whether s is shaped like the attempt
// marker the adapter stamps (`cor_` + hex). Same rationale as
// validE2AMessageID: the value came off a signed notification but originated
// in a header block, so it is shape-checked before it becomes a lookup key.
func validAttemptCorrelationID(s string) bool {
	const prefix = "cor_"
	if !strings.HasPrefix(s, prefix) || len(s) <= len(prefix) || len(s) > maxE2AMessageIDLen {
		return false
	}
	for _, r := range s[len(prefix):] {
		switch {
		case r >= 'a' && r <= 'f', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// FeedbackFor reduces a parsed event to the processor's input: every
// recipient the notification named, normalized and deduplicated, plus the
// two correlation keys and the retained subtypes.
func (ev *Event) FeedbackFor() ProviderFeedback {
	seen := map[string]bool{}
	var recipients []string
	for _, r := range ev.Recipients {
		a := norm(r.Address)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		recipients = append(recipients, a)
	}
	return ProviderFeedback{
		ProviderEventID:      ev.ProviderEventID,
		OccurredAt:           ev.OccurredAt,
		Kind:                 ev.Kind,
		BounceType:           ev.BounceType,
		BounceSubType:        ev.BounceSubType,
		ComplaintSubType:     ev.ComplaintSubType,
		ProviderMessageID:    ev.SESMessageID,
		AttemptCorrelationID: ev.AttemptCorrelationID,
		Recipients:           recipients,
	}
}

// normalizeBounceType maps SES's bounceType (Permanent | Transient |
// Undetermined, case per SES docs) to the stable event vocabulary. Anything
// unrecognized — including a missing value — is "undetermined", so
// email.bounced's required bounce_type is always one of the three enums.
func normalizeBounceType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "permanent":
		return "permanent"
	case "transient":
		return "transient"
	default:
		return "undetermined"
	}
}
