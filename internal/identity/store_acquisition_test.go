package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func newAcquisitionTestUser(t *testing.T) (*pgxpool.Pool, *identity.Store, *identity.User) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	u, err := store.CreateOrGetUser(context.Background(), "survey@example.test", "Survey", "sub-survey-1")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	return pool, store, u
}

func TestRecordAcquisitionSurvey_SetsAllColumnsAndIsVisibleOnReload(t *testing.T) {
	ctx := context.Background()
	pool, store, u := newAcquisitionTestUser(t)

	before, err := store.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.AcquisitionAnsweredAt != nil {
		t.Fatalf("fresh user AcquisitionAnsweredAt = %v, want nil", before.AcquisitionAnsweredAt)
	}

	detail := "a newsletter"
	got, err := store.RecordAcquisitionSurvey(ctx, u.ID, "other", &detail)
	if err != nil {
		t.Fatalf("RecordAcquisitionSurvey: %v", err)
	}
	if got.AcquisitionAnsweredAt == nil {
		t.Fatal("returned user has nil AcquisitionAnsweredAt")
	}

	var source, storedDetail string
	if err := pool.QueryRow(ctx, `SELECT acquisition_source, acquisition_detail FROM users WHERE id=$1`, u.ID).Scan(&source, &storedDetail); err != nil {
		t.Fatal(err)
	}
	if source != "other" || storedDetail != "a newsletter" {
		t.Errorf("stored (%q, %q), want (other, a newsletter)", source, storedDetail)
	}

	// Every loader that feeds /api/auth/me sees the answer.
	sess, err := store.CreateUserSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	viaSession, err := store.GetUserSession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if viaSession.AcquisitionAnsweredAt == nil {
		t.Error("GetUserSession did not load AcquisitionAnsweredAt")
	}
	viaName, err := store.UpdateUserName(ctx, u.ID, "Renamed")
	if err != nil {
		t.Fatal(err)
	}
	if viaName.AcquisitionAnsweredAt == nil {
		t.Error("UpdateUserName did not return AcquisitionAnsweredAt")
	}
}

func TestRecordAcquisitionSurvey_IsWriteOnce(t *testing.T) {
	ctx := context.Background()
	pool, store, u := newAcquisitionTestUser(t)

	if _, err := store.RecordAcquisitionSurvey(ctx, u.ID, "github", nil); err != nil {
		t.Fatal(err)
	}
	_, err := store.RecordAcquisitionSurvey(ctx, u.ID, "search", nil)
	if !errors.Is(err, identity.ErrAcquisitionSurveyAnswered) {
		t.Fatalf("second write err = %v, want ErrAcquisitionSurveyAnswered", err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT acquisition_source FROM users WHERE id=$1`, u.ID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "github" {
		t.Errorf("first answer overwritten: %q", source)
	}
}

func TestRecordAcquisitionSurvey_ConcurrentSubmitsYieldOneWinner(t *testing.T) {
	ctx := context.Background()
	_, store, u := newAcquisitionTestUser(t)

	const n = 8
	var wg sync.WaitGroup
	wins := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.RecordAcquisitionSurvey(ctx, u.ID, "hn_reddit", nil); err == nil {
				wins <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(wins)
	if got := len(wins); got != 1 {
		t.Fatalf("winners = %d, want exactly 1", got)
	}
}

func TestRecordAcquisitionSurvey_UnknownUserAndBadSource(t *testing.T) {
	ctx := context.Background()
	_, store, u := newAcquisitionTestUser(t)

	if _, err := store.RecordAcquisitionSurvey(ctx, "usr_does_not_exist", "github", nil); err == nil || errors.Is(err, identity.ErrAcquisitionSurveyAnswered) {
		t.Fatalf("unknown user err = %v, want a not-found error, not ErrAcquisitionSurveyAnswered", err)
	}
	if _, err := store.RecordAcquisitionSurvey(ctx, u.ID, "carrier_pigeon", nil); err == nil {
		t.Fatal("bad source accepted")
	}
}
