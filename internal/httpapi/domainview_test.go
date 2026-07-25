package httpapi

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

func TestSendingRampViewIsReadOnlyProgressSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	view := sendingRampView(sendramp.Snapshot{
		Status:      sendramp.StatusRamping,
		ActiveDays:  3,
		RampDays:    30,
		DailyLimit:  251,
		UsedToday:   93,
		TargetDaily: 2000,
	}, now)

	if view.Status != "ramping" || view.DailyRecipientLimit != 251 || view.RecipientsUsedToday != 93 {
		t.Fatalf("view = %+v, want ramping 93/251", view)
	}
	wantReset := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	if view.ResetsAt == nil || !view.ResetsAt.Equal(wantReset) {
		t.Fatalf("resets_at = %v, want %v", view.ResetsAt, wantReset)
	}
	wantCompletion := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if view.EstimatedCompletionAt == nil || !view.EstimatedCompletionAt.Equal(wantCompletion) {
		t.Fatalf("estimated_completion_at = %v, want first UTC rollover after the final active ramp day", view.EstimatedCompletionAt)
	}
}

// byPurpose indexes a domainView's unified DNS-records array by purpose so the
// assertions can address records by what they're for rather than by position.
func byPurpose(recs []DNSRecord) map[string]DNSRecord {
	m := make(map[string]DNSRecord, len(recs))
	for _, r := range recs {
		m[r.Purpose] = r
	}
	return m
}

// TestDomainViewInboundStatus: inbound records (ownership, inbound_mx) follow
// `verified`; with the sending feature off there are no mail_from records and
// the dkim record only appears when a key is stored.
func TestDomainViewInboundStatus(t *testing.T) {
	s := New(Deps{SMTPDomain: "mx.e2a.dev"}) // SESRegion empty ⇒ sending off

	t.Run("verified, no key", func(t *testing.T) {
		v := s.domainView(&identity.Domain{Domain: "acme.com", Verified: true, VerificationToken: "tok"})
		m := byPurpose(v.DNSRecords)
		if len(v.DNSRecords) != 3 {
			t.Fatalf("want exactly ownership+inbound_mx+inbound_mx_wildcard, got %d: %+v", len(v.DNSRecords), v.DNSRecords)
		}
		own := m["ownership"]
		if own.Type != "TXT" || own.Value != "tok" || own.Status != "verified" {
			t.Fatalf("ownership record wrong: %+v", own)
		}
		mx := m["inbound_mx"]
		if mx.Type != "MX" || mx.Value != "mx.e2a.dev" || mx.Priority == nil || *mx.Priority != 10 || mx.Status != "verified" {
			t.Fatalf("inbound_mx record wrong: %+v", mx)
		}
		// Wildcard MX is guidance, not part of parent verification. Its status
		// must never mirror the apex and falsely claim it was probed.
		wc := m["inbound_mx_wildcard"]
		if wc.Type != "MX" || wc.Name != "*.acme.com" || wc.Value != "mx.e2a.dev" || wc.Priority == nil || *wc.Priority != 10 || wc.Status != "pending" {
			t.Fatalf("inbound_mx_wildcard record wrong: %+v", wc)
		}
		if _, ok := m["dkim"]; ok {
			t.Fatalf("no stored key ⇒ no dkim record: %+v", v.DNSRecords)
		}
		if _, ok := m["mail_from_mx"]; ok {
			t.Fatalf("feature off ⇒ no mail_from records: %+v", v.DNSRecords)
		}
	})

	t.Run("unverified ⇒ inbound pending", func(t *testing.T) {
		v := s.domainView(&identity.Domain{Domain: "pending.com", Verified: false, VerificationToken: "tok"})
		m := byPurpose(v.DNSRecords)
		if m["ownership"].Status != "pending" || m["inbound_mx"].Status != "pending" {
			t.Fatalf("unverified domain inbound records must be pending: %+v", v.DNSRecords)
		}
	})

	t.Run("dkim present but feature off ⇒ dkim pending", func(t *testing.T) {
		v := s.domainView(&identity.Domain{
			Domain: "acme.com", Verified: true, VerificationToken: "tok",
			DKIMSelector: "e2a202606", DKIMPublicKey: "PUBKEY",
		})
		m := byPurpose(v.DNSRecords)
		dk, ok := m["dkim"]
		if !ok {
			t.Fatalf("stored key ⇒ dkim record expected: %+v", v.DNSRecords)
		}
		if dk.Type != "TXT" || dk.Status != "pending" {
			t.Fatalf("dkim status with feature off must be pending: %+v", dk)
		}
		if _, ok := m["mail_from_mx"]; ok {
			t.Fatalf("feature off ⇒ still no mail_from records: %+v", v.DNSRecords)
		}
	})
}

// TestDomainViewSendingRecords: with the sending feature on (SESRegion set) the
// mail_from_mx + mail_from_spf records are returned deterministically — even
// before SES provisioning (sending_status none) — and the dkim + mail_from
// statuses track the sending_status rollup.
func TestDomainViewSendingRecords(t *testing.T) {
	s := New(Deps{SMTPDomain: "mx.e2a.dev", SESRegion: "us-east-1"})

	base := func(sendingStatus string) *identity.Domain {
		return &identity.Domain{
			Domain: "acme.com", Verified: true, VerificationToken: "tok",
			DKIMSelector: "e2a202606", DKIMPublicKey: "PUBKEY",
			SendingStatus: sendingStatus,
		}
	}

	// Pre-provision (none): records present + deterministic, status pending.
	t.Run("pre-provision none ⇒ records present, pending", func(t *testing.T) {
		v := s.domainView(base("none"))
		m := byPurpose(v.DNSRecords)
		mfmx, ok := m["mail_from_mx"]
		if !ok {
			t.Fatalf("feature on ⇒ mail_from_mx must be present even pre-provision: %+v", v.DNSRecords)
		}
		if mfmx.Type != "MX" || mfmx.Name != "bounce.acme.com" ||
			mfmx.Value != "feedback-smtp.us-east-1.amazonses.com" ||
			mfmx.Priority == nil || *mfmx.Priority != 10 || mfmx.Status != "pending" {
			t.Fatalf("mail_from_mx shape/status wrong: %+v", mfmx)
		}
		mfspf, ok := m["mail_from_spf"]
		if !ok || mfspf.Type != "TXT" || mfspf.Name != "bounce.acme.com" ||
			mfspf.Value != "v=spf1 include:amazonses.com ~all" || mfspf.Status != "pending" {
			t.Fatalf("mail_from_spf shape/status wrong: %+v", mfspf)
		}
		if mfspf.Priority != nil {
			t.Fatalf("TXT mail_from_spf must have null priority: %+v", mfspf)
		}
		if m["dkim"].Status != "pending" {
			t.Fatalf("dkim follows sending_status (none⇒pending): %+v", m["dkim"])
		}
		// Inbound still follows `verified`, independent of sending.
		if m["ownership"].Status != "verified" {
			t.Fatalf("inbound status must remain independent of sending: %+v", m["ownership"])
		}
	})

	t.Run("sending verified ⇒ sending records verified", func(t *testing.T) {
		m := byPurpose(s.domainView(base("verified")).DNSRecords)
		for _, p := range []string{"dkim", "mail_from_mx", "mail_from_spf"} {
			if m[p].Status != "verified" {
				t.Fatalf("%s should be verified: %+v", p, m[p])
			}
		}
	})

	t.Run("sending failed ⇒ sending records failed", func(t *testing.T) {
		m := byPurpose(s.domainView(base("failed")).DNSRecords)
		for _, p := range []string{"dkim", "mail_from_mx", "mail_from_spf"} {
			if m[p].Status != "failed" {
				t.Fatalf("%s should be failed: %+v", p, m[p])
			}
		}
	})

	t.Run("sending pending ⇒ sending records pending", func(t *testing.T) {
		m := byPurpose(s.domainView(base("pending")).DNSRecords)
		for _, p := range []string{"dkim", "mail_from_mx", "mail_from_spf"} {
			if m[p].Status != "pending" {
				t.Fatalf("%s should be pending: %+v", p, m[p])
			}
		}
	})
}

// TestDomainViewCapabilities pins the derived per-axis capabilities object. The
// load-bearing property is that it CANNOT drift: it is a restatement of state
// the view already computes, not a second source of truth. `inbound` must always
// equal the status stamped on the inbound DNS records, and `outbound` must always
// equal the domain-level sending_status rollup.
func TestDomainViewCapabilities(t *testing.T) {
	s := New(Deps{SMTPDomain: "mx.e2a.dev", SESRegion: "us-east-1"})

	cases := []struct {
		name          string
		verified      bool
		sendingStatus string
		wantInbound   string
		wantOutbound  string
	}{
		{"just registered, nothing published", false, "", "pending", "none"},
		{"inbound verified, sending unprovisioned", true, "none", "verified", "none"},
		{"inbound verified, sending in flight", true, "pending", "verified", "pending"},
		{"both axes good", true, "verified", "verified", "verified"},
		{"inbound verified, sending failed", true, "failed", "verified", "failed"},
		// The axes are independent in BOTH directions. This combination is the
		// shape an outbound-only (send-only) domain would report.
		{"inbound pending, sending verified", false, "verified", "pending", "verified"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := s.domainView(&identity.Domain{
				Domain: "acme.com", Verified: tc.verified, VerificationToken: "tok",
				DKIMSelector: "e2a202606", DKIMPublicKey: "PUBKEY",
				SendingStatus: tc.sendingStatus,
			})
			if v.Capabilities.Inbound != tc.wantInbound {
				t.Errorf("capabilities.inbound = %q, want %q", v.Capabilities.Inbound, tc.wantInbound)
			}
			if v.Capabilities.Outbound != tc.wantOutbound {
				t.Errorf("capabilities.outbound = %q, want %q", v.Capabilities.Outbound, tc.wantOutbound)
			}
			// Anti-drift, inbound: same value the inbound records are stamped with.
			m := byPurpose(v.DNSRecords)
			if v.Capabilities.Inbound != m["ownership"].Status || v.Capabilities.Inbound != m["inbound_mx"].Status {
				t.Errorf("capabilities.inbound %q must equal the inbound record statuses (ownership=%q inbound_mx=%q)",
					v.Capabilities.Inbound, m["ownership"].Status, m["inbound_mx"].Status)
			}
			// Anti-drift, outbound: exactly the rollup field, normalization included.
			if v.Capabilities.Outbound != v.SendingStatus {
				t.Errorf("capabilities.outbound %q must equal the sending_status rollup %q",
					v.Capabilities.Outbound, v.SendingStatus)
			}
		})
	}

	// capabilities.outbound is deliberately the ROLLUP, not a per-axis value: when
	// DKIM and MAIL FROM disagree, the domain cannot send as its own address, so
	// the capability is the all-or-nothing answer while dns_records[].status
	// carries the per-record detail. Guards against someone "improving" this into
	// a per-axis derivation and silently reporting a capability the domain lacks.
	t.Run("outbound stays the rollup when SES axes diverge", func(t *testing.T) {
		v := s.domainView(&identity.Domain{
			Domain: "acme.com", Verified: true, VerificationToken: "tok",
			DKIMSelector: "e2a202606", DKIMPublicKey: "PUBKEY",
			SendingStatus:         "failed",
			SendingDkimStatus:     "verified",
			SendingMailFromStatus: "failed",
		})
		if v.Capabilities.Outbound != "failed" {
			t.Fatalf("capabilities.outbound = %q, want the failed rollup even though the DKIM axis is verified", v.Capabilities.Outbound)
		}
		if m := byPurpose(v.DNSRecords); m["dkim"].Status != "verified" {
			t.Fatalf("per-record detail must still show the healthy DKIM axis: %+v", m["dkim"])
		}
	})
}

// TestDomainViewSendingPerAxis is the key regression for the per-axis fix: when
// SES reports the DKIM and custom MAIL FROM axes separately, each sending record
// must reflect its OWN axis instead of the all-or-nothing sending_status rollup,
// so the user can tell which record to fix. The rollup field itself is unchanged.
func TestDomainViewSendingPerAxis(t *testing.T) {
	s := New(Deps{SMTPDomain: "mx.e2a.dev", SESRegion: "us-east-1"})

	base := func() *identity.Domain {
		return &identity.Domain{
			Domain: "acme.com", Verified: true, VerificationToken: "tok",
			DKIMSelector: "e2a202606", DKIMPublicKey: "PUBKEY",
		}
	}

	// The headline case: good DKIM, broken MAIL FROM. The rollup is failed
	// (all-or-nothing), but the dkim record must read verified while the
	// mail_from_* records read failed.
	t.Run("dkim verified + mail_from failed ⇒ records disagree", func(t *testing.T) {
		d := base()
		d.SendingStatus = "failed" // rollup stays all-or-nothing
		d.SendingDkimStatus = "verified"
		d.SendingMailFromStatus = "failed"
		v := s.domainView(d)
		m := byPurpose(v.DNSRecords)
		if m["dkim"].Status != "verified" {
			t.Fatalf("dkim should follow its OWN axis (verified): %+v", m["dkim"])
		}
		if m["mail_from_mx"].Status != "failed" || m["mail_from_spf"].Status != "failed" {
			t.Fatalf("mail_from records should follow their OWN axis (failed): mx=%+v spf=%+v", m["mail_from_mx"], m["mail_from_spf"])
		}
		// The rollup field is the summary and must be untouched.
		if v.SendingStatus != "failed" {
			t.Fatalf("domain-level sending_status rollup must stay failed: %q", v.SendingStatus)
		}
	})

	// Reverse mixed case: broken DKIM, good MAIL FROM.
	t.Run("dkim failed + mail_from verified ⇒ records disagree", func(t *testing.T) {
		d := base()
		d.SendingStatus = "failed"
		d.SendingDkimStatus = "failed"
		d.SendingMailFromStatus = "verified"
		m := byPurpose(s.domainView(d).DNSRecords)
		if m["dkim"].Status != "failed" {
			t.Fatalf("dkim should be failed: %+v", m["dkim"])
		}
		if m["mail_from_mx"].Status != "verified" || m["mail_from_spf"].Status != "verified" {
			t.Fatalf("mail_from records should be verified: mx=%+v spf=%+v", m["mail_from_mx"], m["mail_from_spf"])
		}
	})

	// Fallback: when the per-axis columns are empty (pre-migration-049 rows or
	// pre-provision), every sending record falls back to the rollup — preserving
	// the old behavior gracefully.
	t.Run("empty axes ⇒ fall back to rollup", func(t *testing.T) {
		d := base()
		d.SendingStatus = "verified"
		// SendingDkimStatus / SendingMailFromStatus left empty.
		m := byPurpose(s.domainView(d).DNSRecords)
		for _, p := range []string{"dkim", "mail_from_mx", "mail_from_spf"} {
			if m[p].Status != "verified" {
				t.Fatalf("%s should fall back to rollup (verified): %+v", p, m[p])
			}
		}
	})

	// Partial fallback: dkim axis recorded, mail_from axis still empty ⇒ dkim
	// follows its axis, mail_from falls back to the rollup.
	t.Run("one axis set, one empty ⇒ per-axis + fallback mix", func(t *testing.T) {
		d := base()
		d.SendingStatus = "pending"
		d.SendingDkimStatus = "verified"
		// SendingMailFromStatus empty ⇒ falls back to rollup (pending).
		m := byPurpose(s.domainView(d).DNSRecords)
		if m["dkim"].Status != "verified" {
			t.Fatalf("dkim should follow its axis (verified): %+v", m["dkim"])
		}
		if m["mail_from_mx"].Status != "pending" || m["mail_from_spf"].Status != "pending" {
			t.Fatalf("mail_from should fall back to rollup (pending): mx=%+v spf=%+v", m["mail_from_mx"], m["mail_from_spf"])
		}
	})
}
