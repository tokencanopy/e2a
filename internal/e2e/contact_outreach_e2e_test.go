//go:build integration

package e2e_test

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestContactOutreachLoopE2E automates the journey the whole contacts feature
// exists for, over the real HTTP and SMTP surfaces:
//
//	enrol a contact -> send to them -> a real reply arrives -> they drop out
//	of the follow-up query.
//
// It is here rather than as a store test because every defect this feature
// shipped was invisible to the unit suite and only appeared when a real
// message moved through the real path: handlers registered but never wired
// into BuildDeps, and an outbound hook keyed on Message.Recipient, a field the
// terminal path never populates. Both passed every test that used a fake.
func TestContactOutreachLoopE2E(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	_, key, agent := setupDomainAndAgent(t, ts, "raise@outreach.example.com", "outreach.example.com", "", "")
	ctx := context.Background()

	const investor = "partner@fund.vc"
	enrol := func(body string) (int, []byte) {
		return authedJSON(t, "PUT",
			fmt.Sprintf("%s/v1/agents/%s/contacts/%s", ts.HTTPServer.URL, agent.EmailAddress(), investor),
			key.PlaintextKey, body)
	}

	// (1) Enrol, scheduled in the past so the contact is already due.
	status, body := enrol(`{"stage":"touch1","next_action_at":"2020-01-01T00:00:00Z"}`)
	if status != 201 {
		t.Fatalf("enrol status=%d body=%s", status, body)
	}

	engagement := func() identity.ContactEngagement {
		t.Helper()
		e, err := ts.Store.GetEngagement(ctx, agent.UserID, agent.ID, investor)
		if err != nil {
			t.Fatalf("load engagement: %v", err)
		}
		return e
	}
	if e := engagement(); e.OutboundCount != 0 || e.Replied() {
		t.Fatalf("fresh enrolment already has activity: %+v", e)
	}

	// (2) Send a real message. This is the step that proves the outbound hook
	// is wired to the terminal send path and reads the recipient from the
	// array the path actually populates.
	status, body = authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey,
		fmt.Sprintf(`{"to":["%s"],"subject":"Intro","text":"Hello."}`, investor))
	if status != 200 {
		t.Fatalf("send status=%d body=%s", status, body)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if e := engagement(); e.OutboundCount == 1 && e.FirstOutboundAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbound counters never moved after a real send: %+v", engagement())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if e := engagement(); e.Replied() {
		t.Errorf("replied is true before any inbound mail: %+v", e)
	}

	// (3) An UNAUTHENTICATED reply over SMTP must not count.
	//
	// This is the security property, exercised end to end: the header From is
	// spoofable, so mail that fails sender authentication cannot be allowed to
	// mark a contact as having replied — otherwise anyone could remove an
	// investor from someone else's follow-up queue by forging one header.
	// Plain SMTP in a test environment has no SPF/DKIM/DMARC, which makes it
	// exactly the spoofed case.
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Re: Intro\r\n\r\nInterested.",
		investor, agent.EmailAddress())
	if err := smtp.SendMail(ts.SMTPAddr, nil, investor, []string{agent.EmailAddress()}, []byte(msg)); err != nil {
		t.Fatalf("deliver reply: %v", err)
	}
	time.Sleep(2 * time.Second)
	if e := engagement(); e.InboundCount != 0 || e.Replied() {
		t.Errorf("unauthenticated mail marked the contact as replied: inbound=%d replied=%v — "+
			"a spoofed From could evict an investor from someone else's follow-up queue",
			e.InboundCount, e.Replied())
	}

	// (4) Still unreplied, so still due: the contact remains in the follow-up
	// query. The authenticated-reply path — where this flips and the contact
	// drops out — is covered by the relay tests, which can construct mail that
	// passes authentication; a test SMTP client cannot.
	status, body = authedJSON(t, "GET",
		fmt.Sprintf("%s/v1/agents/%s/contacts?replied=false&next_action_before=%s",
			ts.HTTPServer.URL, agent.EmailAddress(), time.Now().UTC().Format(time.RFC3339)),
		key.PlaintextKey, "")
	if status != 200 {
		t.Fatalf("due query status=%d body=%s", status, body)
	}
	if !strings.Contains(string(body), investor) {
		t.Errorf("an enrolled, past-due, unreplied contact is missing from the follow-up query: %s", body)
	}
}

// TestContactOutreachIgnoresUnenrolledRecipientsE2E pins the update-only rule
// over the real send path: ordinary correspondence must not silently populate
// an outreach list with people nobody is running a campaign against.
func TestContactOutreachIgnoresUnenrolledRecipientsE2E(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	_, key, agent := setupDomainAndAgent(t, ts, "raise@noenrol.example.com", "noenrol.example.com", "", "")
	ctx := context.Background()

	status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey,
		`{"to":["stranger@nowhere.vc"],"subject":"one-off","text":"hi"}`)
	if status != 200 {
		t.Fatalf("send status=%d body=%s", status, body)
	}

	time.Sleep(2 * time.Second)
	rows, err := ts.Store.ListEngagements(ctx, agent.UserID, agent.ID,
		identity.EngagementFilter{}, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("list engagements: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a send to an un-enrolled address created %d engagement(s)", len(rows))
	}
}
