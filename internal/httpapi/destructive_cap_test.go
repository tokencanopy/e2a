package httpapi

import (
	"sync"
	"testing"
)

// The per-account cap is the guard against the failure mode where several
// unbounded cascades run at once, each holding a pooled connection: six
// concurrent permanent agent deletes saturated the pool, starved the readiness
// probe, and took a whole slot out of the load balancer.
func TestAcquireDestructiveCapsConcurrencyPerAccount(t *testing.T) {
	s := &Server{}

	for i := 0; i < maxConcurrentDestructive; i++ {
		if !s.acquireDestructive("user-a") {
			t.Fatalf("acquire %d/%d was refused below the cap", i+1, maxConcurrentDestructive)
		}
	}
	if s.acquireDestructive("user-a") {
		t.Fatalf("acquire above the cap of %d succeeded", maxConcurrentDestructive)
	}

	// The cap is per account: a busy tenant must not block anyone else.
	if !s.acquireDestructive("user-b") {
		t.Error("a different account was blocked by user-a's in-flight work")
	}

	// Releasing frees exactly one slot, so sequential deletes never stall.
	s.releaseDestructive("user-a")
	if !s.acquireDestructive("user-a") {
		t.Error("a slot freed by release was not reusable")
	}
}

// A refused acquire must not consume a slot, or the account would leak its way
// to a permanent lockout.
func TestRefusedAcquireDoesNotLeakSlots(t *testing.T) {
	s := &Server{}
	for i := 0; i < maxConcurrentDestructive; i++ {
		s.acquireDestructive("u")
	}
	for i := 0; i < 25; i++ {
		if s.acquireDestructive("u") {
			t.Fatal("acquire above the cap succeeded")
		}
	}
	for i := 0; i < maxConcurrentDestructive; i++ {
		s.releaseDestructive("u")
	}
	if !s.acquireDestructive("u") {
		t.Error("account stayed locked out after every in-flight delete finished: refusals leaked slots")
	}
}

// The counter is touched from concurrent requests, so it must be race-free and
// must never admit more than the cap.
func TestAcquireDestructiveIsConcurrencySafe(t *testing.T) {
	s := &Server{}
	const goroutines = 64

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.acquireDestructive("shared") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != maxConcurrentDestructive {
		t.Errorf("admitted %d concurrent destructive ops, want exactly %d", admitted, maxConcurrentDestructive)
	}
}
