package identity

import (
	"testing"
	"time"
)

func TestThreadAmbiguityLogIsRateLimited(t *testing.T) {
	threadAmbiguityNextLogUnix.Store(0)
	now := time.Unix(1_000_000, 0)

	if !maybeLogThreadAmbiguity(now, 2, 2) {
		t.Fatal("first ambiguity log was suppressed")
	}
	if maybeLogThreadAmbiguity(now.Add(59*time.Second), 3, 2) {
		t.Fatal("ambiguity log inside rate-limit window was emitted")
	}
	if !maybeLogThreadAmbiguity(now.Add(time.Minute), 3, 2) {
		t.Fatal("ambiguity log after rate-limit window was suppressed")
	}
}
