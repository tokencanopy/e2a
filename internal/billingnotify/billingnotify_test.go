package billingnotify_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/billingnotify"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func seedUser(t *testing.T, pool *pgxpool.Pool, id string, trashed bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, google_subject, deleted_at) VALUES ($1, $1 || '@example.test', 'sub-' || $1, CASE WHEN $2 THEN now() END)`,
		id, trashed); err != nil {
		t.Fatal(err)
	}
}

func setTrashed(t *testing.T, pool *pgxpool.Pool, id string, trashed bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deleted_at = CASE WHEN $2 THEN now() END WHERE id = $1`, id, trashed); err != nil {
		t.Fatal(err)
	}
}

func work(j *billingnotify.Jobs, userID, mode string) error {
	w := firstWorker(j)
	return w.Work(context.Background(), &river.Job[billingnotify.Args]{Args: billingnotify.Args{UserID: userID, Mode: mode}})
}

// firstWorker extracts the registered worker.
func firstWorker(j *billingnotify.Jobs) *billingnotify.Worker {
	return billingnotify.NewWorkerForTest(j)
}

type recorder struct {
	mu    sync.Mutex
	posts []string
	err   error
}

func (r *recorder) post(_ context.Context, _ string, mode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, mode)
	return r.err
}

func TestWorkerPostsCurrentNoticesAndSkipsStaleOnes(t *testing.T) {
	pool := testutil.TestDB(t)
	seedUser(t, pool, "usr_bn_trashed", true)
	seedUser(t, pool, "usr_bn_live", false)
	rec := &recorder{}
	j := billingnotify.New(pool)
	j.SetPoster(rec.post)
	for _, tc := range []struct{ user, mode string }{
		{"usr_bn_trashed", billingnotify.ModeTrash},   // current → post
		{"usr_bn_live", billingnotify.ModeRestore},    // current → post
		{"usr_bn_live", billingnotify.ModeTrash},      // stale → skip
		{"usr_bn_trashed", billingnotify.ModeRestore}, // stale → skip
		{"usr_bn_gone", billingnotify.ModeTrash},      // purged → skip
	} {
		if err := work(j, tc.user, tc.mode); err != nil {
			t.Fatalf("%s/%s: %v", tc.user, tc.mode, err)
		}
	}
	if len(rec.posts) != 2 || rec.posts[0] != "trash" || rec.posts[1] != "restore" {
		t.Fatalf("posts = %v, want exactly the two current notices", rec.posts)
	}
}

func TestWorkerRetriesFailuresButNotA404(t *testing.T) {
	pool := testutil.TestDB(t)
	seedUser(t, pool, "usr_bn_retry", false)
	j := billingnotify.New(pool)
	boom := errors.New("billing 503")
	j.SetPoster((&recorder{err: boom}).post)
	if err := work(j, "usr_bn_retry", billingnotify.ModeRestore); !errors.Is(err, boom) {
		t.Fatalf("a billing failure must be returned for retry, got %v", err)
	}
	j.SetPoster((&recorder{err: billingnotify.ErrNotFound}).post)
	if err := work(j, "usr_bn_retry", billingnotify.ModeRestore); err != nil {
		t.Fatalf("an old service's 404 must complete the job: %v", err)
	}
}

func TestWorkerSnoozesUntilThePosterIsBound(t *testing.T) {
	pool := testutil.TestDB(t)
	err := work(billingnotify.New(pool), "usr_any", billingnotify.ModeRestore)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("unbound poster: err=%v, want a snooze", err)
	}
}

// TestConcurrentTrashAndRestoreNoticesEndOnTheFinalState is the race the
// per-account lock closes: a trash notice reads "trashed" and is mid-post when
// the account is restored and its restore notice starts. The restore notice
// must wait for the trash post to finish, so the LAST post billing sees is the
// restore — the account's final state.
func TestConcurrentTrashAndRestoreNoticesEndOnTheFinalState(t *testing.T) {
	pool := testutil.TestDB(t)
	seedUser(t, pool, "usr_bn_race", true)

	var mu sync.Mutex
	var order []string
	inTrashPost := make(chan struct{})
	releaseTrash := make(chan struct{})
	j := billingnotify.New(pool)
	j.SetPoster(func(_ context.Context, _ string, mode string) error {
		if mode == billingnotify.ModeTrash {
			close(inTrashPost)
			<-releaseTrash // the trash POST is slow
		}
		mu.Lock()
		order = append(order, mode)
		mu.Unlock()
		return nil
	})

	trashDone := make(chan error, 1)
	go func() { trashDone <- work(j, "usr_bn_race", billingnotify.ModeTrash) }()
	<-inTrashPost

	setTrashed(t, pool, "usr_bn_race", false) // the restore commits
	restoreDone := make(chan error, 1)
	go func() { restoreDone <- work(j, "usr_bn_race", billingnotify.ModeRestore) }()

	select {
	case err := <-restoreDone:
		t.Fatalf("the restore notice posted while the trash post was in flight (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseTrash)
	if err := <-trashDone; err != nil {
		t.Fatal(err)
	}
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[len(order)-1] != billingnotify.ModeRestore {
		t.Fatalf("post order = %v, want the restore last (the final state)", order)
	}
}
