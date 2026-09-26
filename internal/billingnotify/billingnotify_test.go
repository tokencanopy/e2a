package billingnotify

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
)

func run(t *testing.T, state StateReader, poster Poster, mode string) (error, int) {
	t.Helper()
	calls := 0
	j := New(state)
	if poster != nil {
		j.SetPoster(func(ctx context.Context, userID, m string) error {
			calls++
			return poster(ctx, userID, m)
		})
	}
	w := &Worker{jobs: j}
	err := w.Work(context.Background(), &river.Job[Args]{Args: Args{UserID: "usr_1", Mode: mode}})
	return err, calls
}

func live(bool) StateReader {
	return func(context.Context, string) (bool, bool, error) { return true, false, nil }
}

func TestWorkerPostsACurrentNotice(t *testing.T) {
	trashed := func(context.Context, string) (bool, bool, error) { return true, true, nil }
	ok := func(context.Context, string, string) error { return nil }
	if err, n := run(t, trashed, ok, ModeTrash); err != nil || n != 1 {
		t.Fatalf("trash notice for a trashed account: err=%v posts=%d", err, n)
	}
	if err, n := run(t, live(true), ok, ModeRestore); err != nil || n != 1 {
		t.Fatalf("restore notice for a live account: err=%v posts=%d", err, n)
	}
}

func TestWorkerSkipsANoticeTheAccountMovedPast(t *testing.T) {
	ok := func(context.Context, string, string) error { return nil }
	gone := func(context.Context, string) (bool, bool, error) { return false, false, nil }
	trashed := func(context.Context, string) (bool, bool, error) { return true, true, nil }
	for name, tc := range map[string]struct {
		state StateReader
		mode  string
	}{
		"trash after restore":   {live(true), ModeTrash},
		"restore after retrash": {trashed, ModeRestore},
		"purged (trash)":        {gone, ModeTrash},
		"purged (restore)":      {gone, ModeRestore},
	} {
		if err, n := run(t, tc.state, ok, tc.mode); err != nil || n != 0 {
			t.Errorf("%s: err=%v posts=%d, want a silent skip", name, err, n)
		}
	}
}

func TestWorkerRetriesFailuresButNotA404(t *testing.T) {
	boom := errors.New("billing 503")
	if err, _ := run(t, live(true), func(context.Context, string, string) error { return boom }, ModeRestore); !errors.Is(err, boom) {
		t.Fatalf("a billing failure must be returned for retry, got %v", err)
	}
	if err, n := run(t, live(true), func(context.Context, string, string) error { return ErrNotFound }, ModeRestore); err != nil || n != 1 {
		t.Fatalf("an old service's 404 must complete the job: err=%v posts=%d", err, n)
	}
}

func TestWorkerSnoozesUntilThePosterIsBound(t *testing.T) {
	err, _ := run(t, live(true), nil, ModeRestore)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("unbound poster: err=%v, want a snooze", err)
	}
}
