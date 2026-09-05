package sendramp_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// InspectScopeTx decides which budget pool a send charges BEFORE the ramp lock
// is taken, so every branch below is a classification the sending-protection
// gate relies on without a second check. The tests pin each branch in
// isolation against the store's own tables rather than through the gate, so a
// regression here fails in this package instead of surfacing as a mysterious
// pool charge two packages away.

// inspectDomain seeds one user owning one domain in the given sending and ramp
// states and returns the classifier's answer for it.
func inspectDomain(t *testing.T, pool *pgxpool.Pool, suffix, domain, sendingStatus, rampStatus string, seedScope func(userID string)) sendramp.ScopeState {
	t.Helper()
	ctx := context.Background()
	ids := identity.NewStore(pool)
	user, err := ids.CreateOrGetUser(ctx, "inspect-"+suffix+"@example.com", "Inspect", "inspect-"+suffix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if domain != "" {
		if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
			t.Fatalf("ClaimOrCreateDomain: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE domains SET sending_status=$2, sending_ramp_status=$3 WHERE domain=$1`,
			domain, sendingStatus, rampStatus,
		); err != nil {
			t.Fatalf("stamp domain state: %v", err)
		}
	}
	if seedScope != nil {
		seedScope(user.ID)
	}
	return inspect(t, pool, user.ID, domain)
}

func inspect(t *testing.T, pool *pgxpool.Pool, userID, domain string) sendramp.ScopeState {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx)
	state, err := sendramp.InspectScopeTx(ctx, tx, userID, domain)
	if err != nil {
		t.Fatalf("InspectScopeTx: %v", err)
	}
	return state
}

func insertScope(t *testing.T, pool *pgxpool.Pool, userID, scope, status string, activeDays int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO sending_ramp_scopes (user_id,domain,status,active_days,start_daily,target_daily,ramp_days)
		VALUES ($1,$2,$3,$4,50,200,4)`, userID, scope, status, activeDays,
	); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
}

func TestInspectScopeTxTreatsAMissingDomainAsProbation(t *testing.T) {
	pool := testutil.TestDB(t)
	// The user exists; the domain was never registered. Nothing has been
	// proven, so the answer is inactive and not established — never an error,
	// because the gate must classify shared-relay-shaped sends without a row.
	got := inspectDomain(t, pool, "missing", "", "", "", nil)
	if got.Status != sendramp.StatusInactive || got.Established || got.ActiveDays != 0 {
		t.Fatalf("state=%+v, want inactive probation", got)
	}
}

func TestInspectScopeTxEstablishesLegacyExemptDomainsWithoutAScope(t *testing.T) {
	pool := testutil.TestDB(t)
	// Exempt is the grandfather stamp: verified before the ramp existed. It is
	// established on its own say-so and no scope row is consulted or required.
	got := inspectDomain(t, pool, "exempt", "inspect-exempt.example.com", "verified", sendramp.StatusExempt, nil)
	if got.Status != sendramp.StatusExempt || !got.Established {
		t.Fatalf("state=%+v, want established exempt", got)
	}
}

func TestInspectScopeTxEstablishesADomainStampedComplete(t *testing.T) {
	pool := testutil.TestDB(t)
	got := inspectDomain(t, pool, "complete", "inspect-complete.example.com", "verified", sendramp.StatusComplete, nil)
	if got.Status != sendramp.StatusComplete || !got.Established {
		t.Fatalf("state=%+v, want established complete", got)
	}
}

func TestInspectScopeTxKeepsAnUnverifiedIdentityInProbationHoweverOldItsScope(t *testing.T) {
	pool := testutil.TestDB(t)
	// The scope has four qualified days, which would normally establish it.
	// But qualified days vouch for the identity that earned them, and this
	// domain's own SES identity is not verified — a child subdomain rebound
	// onto a parent's progress. The classifier must not read the scope at
	// all: it stays probationary with zero reported days.
	got := inspectDomain(t, pool, "unverified", "inspect-unverified.example.com", "pending", sendramp.StatusRamping,
		func(userID string) { insertScope(t, pool, userID, "example.com", sendramp.StatusRamping, 4) })
	if got.Established || got.ActiveDays != 0 || got.Status != sendramp.StatusRamping {
		t.Fatalf("state=%+v, want unverified identity held in probation with its scope ignored", got)
	}
}

func TestInspectScopeTxTreatsAVerifiedDomainWithNoScopeAsDayZero(t *testing.T) {
	pool := testutil.TestDB(t)
	// Stamped ramping, verified, but the scope has not armed yet: day zero.
	got := inspectDomain(t, pool, "unarmed", "inspect-unarmed.example.com", "verified", sendramp.StatusRamping, nil)
	if got.Established || got.ActiveDays != 0 || got.Status != sendramp.StatusRamping {
		t.Fatalf("state=%+v, want day-zero probation", got)
	}
}

func TestInspectScopeTxReportsACompletedScopeAsComplete(t *testing.T) {
	pool := testutil.TestDB(t)
	// The domain row still says ramping — Reserve stamps `complete` lazily on
	// the next send — but the scope has finished. The scope is authoritative.
	got := inspectDomain(t, pool, "scope-complete", "inspect-scope-complete.example.com", "verified", sendramp.StatusRamping,
		func(userID string) { insertScope(t, pool, userID, "example.com", sendramp.StatusComplete, 4) })
	if got.Status != sendramp.StatusComplete || !got.Established || got.ActiveDays != 4 {
		t.Fatalf("state=%+v, want established complete from the scope", got)
	}
}

func TestInspectScopeTxEstablishesAfterExactlyOneQualifiedDay(t *testing.T) {
	pool := testutil.TestDB(t)
	// Day zero has proved nothing; one qualified day is the bar. The boundary
	// is pinned on both sides because it is the single number that decides
	// whether a custom domain keeps drawing on the shared probation pool.
	for _, tc := range []struct {
		suffix      string
		activeDays  int
		established bool
	}{
		{"day-zero", 0, false},
		{"day-one", 1, true},
	} {
		got := inspectDomain(t, pool, tc.suffix, "inspect-"+tc.suffix+".example.net", "verified", sendramp.StatusRamping,
			func(userID string) {
				insertScope(t, pool, userID, "example.net", sendramp.StatusRamping, tc.activeDays)
			})
		if got.Established != tc.established || got.ActiveDays != tc.activeDays || got.Status != sendramp.StatusRamping {
			t.Errorf("%s: state=%+v, want established=%v active_days=%d", tc.suffix, got, tc.established, tc.activeDays)
		}
	}
}

func TestInspectScopeTxLooksUpTheScopeByRegistrableDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	// The domain row is keyed by the full hostname; the scope is keyed by the
	// registrable domain (eTLD+1). A scope armed at example.org must be found
	// from deep.sub.inspect-registrable.example.org, and a scope mistakenly
	// keyed by the hostname must NOT be.
	hostname := "deep.sub.inspect-registrable.example.org"
	got := inspectDomain(t, pool, "registrable", hostname, "verified", sendramp.StatusRamping,
		func(userID string) {
			insertScope(t, pool, userID, hostname, sendramp.StatusComplete, 9) // decoy: wrong key
			insertScope(t, pool, userID, "example.org", sendramp.StatusRamping, 2)
		})
	if !got.Established || got.ActiveDays != 2 || got.Status != sendramp.StatusRamping {
		t.Fatalf("state=%+v, want the example.org scope (2 days), not the hostname decoy", got)
	}
}
