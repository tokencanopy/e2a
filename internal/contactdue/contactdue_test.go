package contactdue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/contactdue"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// capturingPublisher records what the sweep emitted, so assertions are about
// the wake-ups an agent would actually receive rather than about which store
// method was called.
type capturingPublisher struct {
	got []identity.DueEngagement
	err error
}

func (p *capturingPublisher) PublishContactDue(_ context.Context, d identity.DueEngagement) error {
	if p.err != nil {
		return p.err
	}
	p.got = append(p.got, d)
	return nil
}

func (p *capturingPublisher) addresses() []string {
	var out []string
	for _, d := range p.got {
		out = append(out, d.Address)
	}
	return out
}

// liveAgent creates an account, verified domain, and agent.
func liveAgent(t *testing.T, store *identity.Store, tag string) (*identity.User, string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "due-"+tag+"@example.com", "Owner", "google-due-"+tag)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	domain := tag + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	agent := "raise@" + domain
	if _, err := store.CreateAgent(ctx, agent, domain, "", "https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return user, agent
}

func armPastDue(t *testing.T, store *identity.Store, userID, agent, address string) {
	t.Helper()
	past := time.Now().Add(-time.Hour).UTC()
	p := &past
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(context.Background(), userID, agent, address, &stage, &p, nil); err != nil {
		t.Fatalf("arm %s: %v", address, err)
	}
}

// TestSweepPublishesDueWakeUps drives a REAL sweep against a REAL store — the
// thing the janitor's hourly periodic made impossible to test directly, and the
// main reason this lives in its own package.
//
// It asserts on the payload an agent actually receives, since a wake-up
// carrying no context would force a second round trip and defeat the point.
func TestSweepPublishesDueWakeUps(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := liveAgent(t, store, "sweep")

	meta := map[string]any{"fund": "Example Capital"}
	past := time.Now().Add(-time.Hour).UTC()
	p := &past
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "partner@sweep.vc", &stage, &p, meta); err != nil {
		t.Fatalf("arm: %v", err)
	}

	pub := &capturingPublisher{}
	n, err := contactdue.NewSweeper(store, pub, nil).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(pub.got) != 1 {
		t.Fatalf("published %d (captured %d), want 1", n, len(pub.got))
	}
	d := pub.got[0]
	if d.AgentEmail != agent || d.Address != "partner@sweep.vc" {
		t.Errorf("wake-up addressed wrong: %+v", d)
	}
	if d.Stage != "touch1" {
		t.Errorf("stage = %q, want touch1 — the agent needs it to decide what to send", d.Stage)
	}
	if d.UserID != user.ID {
		t.Errorf("user_id = %q, want %q", d.UserID, user.ID)
	}
}

// TestSweepFiresOncePerSchedule pins the dedupe contract end to end: without
// it the sweep would re-wake the same agent every five minutes forever.
func TestSweepFiresOncePerSchedule(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := liveAgent(t, store, "once")
	armPastDue(t, store, user.ID, agent, "partner@once.vc")

	sweeper := contactdue.NewSweeper(store, &capturingPublisher{}, nil)
	if n, err := sweeper.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("first sweep = %d err=%v, want 1", n, err)
	}
	if n, err := sweeper.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("second sweep = %d err=%v, want 0 — a schedule must wake an agent once", n, err)
	}
}

// TestSweepSkipsSuppressedAndTrashed is the fail-closed pair. A wake-up is an
// invitation to send, so a suppressed contact must never produce one; and
// deleting an agent must stop its outreach immediately rather than 30 days
// later when trash retention expires.
func TestSweepSkipsSuppressedAndTrashed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := liveAgent(t, store, "guards")

	armPastDue(t, store, user.ID, agent, "ok@guards.vc")
	armPastDue(t, store, user.ID, agent, "unsubscribed@guards.vc")
	if _, _, err := store.AddAgentSuppression(ctx, user.ID, agent, "unsubscribed@guards.vc",
		"asked to stop", "unsubscribe", nil); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	pub := &capturingPublisher{}
	if _, err := contactdue.NewSweeper(store, pub, nil).Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := pub.addresses(); len(got) != 1 || got[0] != "ok@guards.vc" {
		t.Errorf("woke %v, want only [ok@guards.vc] — a suppressed contact must never trigger a wake-up", got)
	}

	// Trashing the agent stops the rest of its outreach at once.
	armPastDue(t, store, user.ID, agent, "later@guards.vc")
	if err := store.SoftDeleteAgent(ctx, agent, user.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	pub2 := &capturingPublisher{}
	if _, err := contactdue.NewSweeper(store, pub2, nil).Sweep(ctx); err != nil {
		t.Fatalf("sweep after trash: %v", err)
	}
	if len(pub2.got) != 0 {
		t.Errorf("a trashed agent produced %d wake-up(s) — deletion must stop outreach immediately", len(pub2.got))
	}
}

// TestSweepContinuesPastAPublishFailure pins that one unreachable subscriber
// does not stop every other agent being woken.
func TestSweepContinuesPastAPublishFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := liveAgent(t, store, "resilient")
	armPastDue(t, store, user.ID, agent, "a@resilient.vc")
	armPastDue(t, store, user.ID, agent, "b@resilient.vc")

	pub := &capturingPublisher{err: errors.New("subscriber unreachable")}
	n, err := contactdue.NewSweeper(store, pub, nil).Sweep(ctx)
	if err != nil {
		t.Fatalf("a failing publisher aborted the sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("published = %d, want 0 when every publish fails", n)
	}
	// The claim already consumed the schedule, so the cost is a missed wake-up
	// rather than a duplicate one — the deliberately safer direction.
	pub2 := &capturingPublisher{}
	if n, err := contactdue.NewSweeper(store, pub2, nil).Sweep(ctx); err != nil || n != 0 {
		t.Errorf("re-sweep = %d err=%v; a consumed schedule must not re-fire", n, err)
	}
}
