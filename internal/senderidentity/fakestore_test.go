package senderidentity

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/domainteardown"
)

// fakeStore is an in-memory Store for unit tests. Concurrency-safe so it can
// be driven from worker goroutines if needed. Domains not present in the
// status map are treated as "gone" → GetSendingStatus returns pgx.ErrNoRows.
type fakeStore struct {
	mu         sync.Mutex
	mutationMu sync.Mutex
	mutationOn bool

	// status holds the current sending status per domain. Absence ⇒ row gone.
	status map[string]Status
	// owners maps domain → owning user_id ("" or absent ⇒ no owner).
	owners       map[string]string
	incarnations map[string]string
	verified     map[string]bool
	managed      map[string]string
	applied      map[string]string
	// managedTouched models the ledger row's updated_at. Entries seeded
	// directly into `managed` without a timestamp read as ancient, so tests
	// that don't care about the drain window keep their old behavior.
	managedTouched       map[string]time.Time
	providerPendingSince map[string]time.Time
	teardownState        map[string]domainteardown.State

	// provisionInputs feeds SendingProvisionInputs.
	selector  string
	privKey   []byte
	inputsOK  bool
	inputsErr error

	// forced errors for the write/read methods (for retry-path tests).
	setStatusErr    error
	touchErr        error
	getStatusErr    error
	forgetErr       error
	markAppliedErr  error
	domainExistsErr error

	// recorded calls
	SetStatusCalls []setStatusCall
	TouchCalls     []string
	// ForgetCalls / FinalizeTombstoneCalls record every successful call to
	// each deletion method separately (they share forgetErr for injected
	// failures, but distinct call logs let a test prove WHICH of the two
	// disjoint branches — ownership-failure vs. genuine-teardown — actually
	// fired, rather than only observing the shared side effect of both
	// (deleting the same map entry).
	ForgetCalls            []string
	FinalizeTombstoneCalls []string
	listManagedCalls       int
	listManagedPageCalls   int
	lookupManagedCalls     int
	domainExistsCalls      int
}

type setStatusCall struct {
	Domain         string
	Status         Status
	DkimStatus     Status
	MailFromStatus Status
	ErrMsg         string
	Records        []DNSRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		status:               map[string]Status{},
		owners:               map[string]string{},
		incarnations:         map[string]string{},
		verified:             map[string]bool{},
		managed:              map[string]string{},
		applied:              map[string]string{},
		managedTouched:       map[string]time.Time{},
		providerPendingSince: map[string]time.Time{},
		teardownState:        map[string]domainteardown.State{},
	}
}

func (s *fakeStore) ageProviderPending(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerPendingSince[domain] = time.Now().Add(-48 * time.Hour)
}

// touchTombstone stamps the ledger row as mutated "now" (what the delete tx
// and MarkSendingIdentityManaged do in production).
func (s *fakeStore) touchTombstone(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managedTouched[domain] = time.Now()
}

// ageTombstone backdates the ledger row past any drain window.
func (s *fakeStore) ageTombstone(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managedTouched[domain] = time.Now().Add(-24 * time.Hour)
}

func (s *fakeStore) setStatus(domain string, st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[domain] = st
	if s.incarnations[domain] == "" {
		s.incarnations[domain] = domain + "-incarnation"
	}
	s.verified[domain] = true
}

// deleteDomain models a genuine `domains` row delete: gone from status
// (LoadSendingIdentityState reads pgx.ErrNoRows, mirroring the real d.domain
// join) AND ownerless (DomainOwner reads "" for an absent row in production,
// COALESCE(d.user_id::text, "") — never leave a stale owner behind).
func (s *fakeStore) deleteDomain(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.status, domain)
	delete(s.incarnations, domain)
	delete(s.verified, domain)
	delete(s.owners, domain)
}

func (s *fakeStore) setOwner(domain, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[domain] = owner
}

func (s *fakeStore) setVerified(domain string, verified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verified[domain] = verified
}

func (s *fakeStore) setProvisionInputs(selector string, key []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selector = selector
	s.privKey = key
	s.inputsOK = ok
}

func (s *fakeStore) lastSetStatus() (setStatusCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.SetStatusCalls) == 0 {
		return setStatusCall{}, false
	}
	return s.SetStatusCalls[len(s.SetStatusCalls)-1], true
}

func (s *fakeStore) SendingProvisionInputs(ctx context.Context, domain string) (string, []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputsErr != nil {
		return "", nil, false, s.inputsErr
	}
	return s.selector, s.privKey, s.inputsOK, nil
}

func (s *fakeStore) WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) error {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	s.mu.Lock()
	s.mutationOn = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.mutationOn = false
		s.mu.Unlock()
	}()
	return fn(ctx)
}

func (s *fakeStore) mutationHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutationOn
}

func (s *fakeStore) LoadSendingIdentityState(ctx context.Context, domain string) (SendingIdentityState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getStatusErr != nil {
		return SendingIdentityState{}, s.getStatusErr
	}
	status, ok := s.status[domain]
	if !ok {
		return SendingIdentityState{}, pgx.ErrNoRows
	}
	if s.inputsErr != nil {
		return SendingIdentityState{}, s.inputsErr
	}
	return SendingIdentityState{
		Incarnation:        s.incarnations[domain],
		Owner:              s.owners[domain],
		Verified:           s.verified[domain],
		Status:             status,
		Selector:           s.selector,
		PrivateKey:         append([]byte(nil), s.privKey...),
		AppliedIncarnation: s.applied[domain],
		LedgerUpdatedAt:    s.managedTouched[domain],
	}, nil
}

func (s *fakeStore) SetSendingStatus(ctx context.Context, domain, incarnation string, status, dkimStatus, mailFromStatus Status, errMsg string, records []DNSRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setStatusErr != nil {
		return s.setStatusErr
	}
	if s.incarnations[domain] != incarnation {
		return pgx.ErrNoRows
	}
	s.SetStatusCalls = append(s.SetStatusCalls, setStatusCall{Domain: domain, Status: status, DkimStatus: dkimStatus, MailFromStatus: mailFromStatus, ErrMsg: errMsg, Records: records})
	s.status[domain] = status
	return nil
}

func (s *fakeStore) TouchSendingChecked(ctx context.Context, domain, incarnation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touchErr != nil {
		return s.touchErr
	}
	if s.incarnations[domain] != incarnation {
		return pgx.ErrNoRows
	}
	s.TouchCalls = append(s.TouchCalls, domain)
	return nil
}

func (s *fakeStore) GetSendingStatus(ctx context.Context, domain string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getStatusErr != nil {
		return "", s.getStatusErr
	}
	st, ok := s.status[domain]
	if !ok {
		return "", pgx.ErrNoRows // domain deleted mid-flight
	}
	return st, nil
}

func (s *fakeStore) DomainOwner(ctx context.Context, domain string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[domain], nil
}

func (s *fakeStore) MarkSendingIdentityManaged(ctx context.Context, domain, incarnation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managed[domain] = incarnation
	delete(s.applied, domain)
	delete(s.providerPendingSince, domain)
	return nil
}

func (s *fakeStore) MarkSendingIdentityApplied(ctx context.Context, domain, incarnation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markAppliedErr != nil {
		return s.markAppliedErr
	}
	if s.managed[domain] != incarnation {
		return pgx.ErrNoRows
	}
	s.applied[domain] = incarnation
	delete(s.providerPendingSince, domain)
	return nil
}

func (s *fakeStore) SendingIdentityLedgerExpired(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed[domain] != incarnation {
		return false, nil
	}
	touched, ok := s.managedTouched[domain]
	return ok && time.Since(touched) > olderThan, nil
}

func (s *fakeStore) ObserveSendingIdentityProviderPending(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed[domain] != incarnation {
		return false, nil
	}
	since, ok := s.providerPendingSince[domain]
	if !ok {
		s.providerPendingSince[domain] = time.Now()
		return false, nil
	}
	return time.Since(since) > olderThan, nil
}

func (s *fakeStore) ClearSendingIdentityProviderPending(ctx context.Context, domain, incarnation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed[domain] == incarnation {
		delete(s.providerPendingSince, domain)
	}
	return nil
}

// ForgetSendingIdentityManaged mirrors the production guard: never delete the
// ledger row for a domain that still has a live owner (owners[domain] != "",
// matching DomainOwner/COALESCE(d.user_id::text, "") reading non-empty for a
// live owned row). forgetErr simulates a query-level failure and is checked
// first, same as the real conditional DELETE still round-tripping to the DB
// even when its WHERE clause matches nothing.
func (s *fakeStore) ForgetSendingIdentityManaged(ctx context.Context, domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forgetErr != nil {
		return s.forgetErr
	}
	s.ForgetCalls = append(s.ForgetCalls, domain)
	if s.owners[domain] != "" {
		return nil
	}
	delete(s.managed, domain)
	delete(s.applied, domain)
	delete(s.managedTouched, domain)
	delete(s.providerPendingSince, domain)
	return nil
}

// FinalizeSendingIdentityTombstone mirrors the production guard (see
// internal/identity's store.go): it deletes the ledger row only when BOTH
// the age gate (updated_at older than olderThan) AND the live-owner guard
// (owners[domain] == "", matching domains d.user_id IS NOT NULL) allow it —
// "never forget a live owned domain's row" is a store-wide invariant, not
// just ForgetSendingIdentityManaged's.
func (s *fakeStore) FinalizeSendingIdentityTombstone(ctx context.Context, domain string, olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forgetErr != nil {
		return s.forgetErr
	}
	s.FinalizeTombstoneCalls = append(s.FinalizeTombstoneCalls, domain)
	if s.owners[domain] != "" {
		return nil // still live and owned: never finalize
	}
	if touched, ok := s.managedTouched[domain]; ok && time.Since(touched) < olderThan {
		return nil // a mutation inside the drain window: keep the tombstone
	}
	delete(s.managed, domain)
	delete(s.applied, domain)
	delete(s.managedTouched, domain)
	return nil
}

func (s *fakeStore) SetDomainTeardownState(ctx context.Context, domain string, state domainteardown.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardownState[domain] = state
	return nil
}

func (s *fakeStore) DomainExists(ctx context.Context, domain string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.domainExistsCalls++
	if s.domainExistsErr != nil {
		return false, s.domainExistsErr
	}
	_, ok := s.status[domain]
	return ok, nil
}

func (s *fakeStore) ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listManagedCalls++
	domains := make([]string, 0, len(s.managed))
	needs := make(map[string]bool, len(s.managed))
	for domain := range s.managed {
		domains = append(domains, domain)
		needs[domain] = s.applied[domain] != s.managed[domain]
	}
	return domains, needs, nil
}

func (s *fakeStore) ListManagedSendingIdentityDomainsPage(ctx context.Context, afterDomain string, limit int) ([]string, map[string]bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listManagedPageCalls++
	all := make([]string, 0, len(s.managed))
	for domain := range s.managed {
		if domain > afterDomain {
			all = append(all, domain)
		}
	}
	sort.Strings(all)
	hasMore := len(all) > limit
	if hasMore {
		all = all[:limit]
	}
	needs := make(map[string]bool, len(all))
	for _, domain := range all {
		needs[domain] = s.applied[domain] != s.managed[domain]
	}
	return all, needs, hasMore, nil
}

func (s *fakeStore) LookupManagedSendingIdentityDomain(ctx context.Context, domain string) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupManagedCalls++
	incarnation, found := s.managed[domain]
	return found && s.applied[domain] != incarnation, found, nil
}

// recordingFirer captures EventFirer invocations.
type recordingFirer struct {
	mu    sync.Mutex
	calls []firedEvent
}

type firedEvent struct {
	Domain string
	UserID string
	Status Status
	ErrMsg string
}

func (r *recordingFirer) fire() EventFirer {
	return func(ctx context.Context, domain, userID string, status Status, errMsg string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, firedEvent{Domain: domain, UserID: userID, Status: status, ErrMsg: errMsg})
	}
}

func (r *recordingFirer) last() (firedEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return firedEvent{}, false
	}
	return r.calls[len(r.calls)-1], true
}

func (r *recordingFirer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}
