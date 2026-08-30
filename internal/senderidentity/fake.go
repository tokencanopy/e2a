package senderidentity

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// FakeProvider is an in-memory Provider for tests. It is concurrency-safe
// (the workers call it from River goroutines). Configure per-domain
// behavior with the setters; inspect calls with the recorded slices.
//
// Default behavior with no configuration: Provision returns StatusPending,
// Status returns StatusPending forever, Deprovision succeeds. Tests that
// want a domain to verify call SetStatusSequence or SetStatus.
type FakeProvider struct {
	mu sync.Mutex

	// provisionResult / provisionErr override Provision's return per call.
	provisionResult *Result
	provisionErr    error
	// statusByDomain returns a fixed Status result for a domain's polls.
	statusByDomain map[string]Result
	// statusSeq pops one result per Status call (then holds the last).
	statusSeq map[string][]Result
	// deprovisionErr forces Deprovision to fail (e.g. to exercise retry).
	deprovisionErr error
	// notFoundOnStatus makes Status return ErrIdentityNotFound for a domain.
	notFoundOnStatus map[string]bool
	// statusErr makes Status return an arbitrary (transient) error for a
	// domain — distinct from ErrIdentityNotFound.
	statusErr map[string]error

	// identities is the set of domains the fake "has" at the provider,
	// for List/reaper tests. Provision adds; Deprovision removes.
	identities map[string]bool

	ProvisionCalls []string
	// ProvisionMetas is index-parallel to ProvisionCalls: the classification
	// metadata each Provision call carried. Recorded separately so existing
	// tests keep asserting on the plain domain list.
	ProvisionMetas   []ProvisionMeta
	StatusCalls      []StatusCall
	DeprovisionCalls []string
	ListCalls        int
	ListPageCalls    int
}

// StatusCall records one Status invocation's full argument set. Provider.Status
// carries a caller-supplied AdoptionEvidence beyond the domain — Selector and
// HasPrivateKey — that drives canAdoptIdentity's decision in the real SES
// provider. Recording only the domain (as this fake used to) would let a
// call site regress to passing "", a stale selector, or the wrong boolean
// while every worker test still passed, since the compiler cannot catch a
// wrong-but-same-typed argument. Tests that care about adoption wiring must
// assert on Selector/HaveKey, not just call counts.
type StatusCall struct {
	Domain   string
	Selector string
	HaveKey  bool
}

// NewFakeProvider returns a ready FakeProvider with default behavior.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		statusByDomain:   map[string]Result{},
		statusSeq:        map[string][]Result{},
		notFoundOnStatus: map[string]bool{},
		statusErr:        map[string]error{},
		identities:       map[string]bool{},
	}
}

// SetStatusErr makes Status return err (a transient error, not NotFound) for
// domain.
func (f *FakeProvider) SetStatusErr(domain string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr[domain] = err
}

// SeedIdentity marks domain as having a provider identity (for List/reaper
// tests) without going through Provision.
func (f *FakeProvider) SeedIdentity(domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identities[domain] = true
}

// SetProvisionErr makes the next Provision calls return err.
func (f *FakeProvider) SetProvisionErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisionErr = err
}

// SetProvisionResult overrides the default pending provisioning result.
func (f *FakeProvider) SetProvisionResult(result Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisionResult = &result
}

// SetStatus fixes the Result returned by Status for domain.
func (f *FakeProvider) SetStatus(domain string, r Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusByDomain[domain] = r
}

// SetStatusSequence queues results consumed one-per-Status-call for domain;
// the last result repeats once the sequence drains. Lets a test drive
// pending → pending → verified.
func (f *FakeProvider) SetStatusSequence(domain string, seq ...Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusSeq[domain] = seq
}

// SetStatusNotFound makes Status return ErrIdentityNotFound for domain.
func (f *FakeProvider) SetStatusNotFound(domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notFoundOnStatus[domain] = true
}

// SetDeprovisionErr forces Deprovision to fail.
func (f *FakeProvider) SetDeprovisionErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deprovisionErr = err
}

func (f *FakeProvider) Provision(ctx context.Context, domain, dkimSelector string, dkimPrivateKeyDER []byte, meta ProvisionMeta) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProvisionCalls = append(f.ProvisionCalls, domain)
	f.ProvisionMetas = append(f.ProvisionMetas, meta)
	if f.provisionErr != nil {
		return Result{}, f.provisionErr
	}
	if f.provisionResult != nil {
		return *f.provisionResult, nil
	}
	f.identities[domain] = true
	// Mirror the SES provider: provisioning emits the custom MAIL FROM records
	// the customer must publish (region is illustrative for tests).
	return Result{Status: StatusPending, DNSRecords: mailFromRecords(domain, "us-east-1")}, nil
}

func (f *FakeProvider) Status(ctx context.Context, domain string, evidence AdoptionEvidence) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StatusCalls = append(f.StatusCalls, StatusCall{Domain: domain, Selector: evidence.Selector, HaveKey: evidence.HasPrivateKey})
	if f.notFoundOnStatus[domain] {
		return Result{}, ErrIdentityNotFound
	}
	if err := f.statusErr[domain]; err != nil {
		return Result{}, err
	}
	if seq, ok := f.statusSeq[domain]; ok && len(seq) > 0 {
		r := seq[0]
		if len(seq) > 1 {
			f.statusSeq[domain] = seq[1:]
		}
		return r, nil
	}
	if r, ok := f.statusByDomain[domain]; ok {
		return r, nil
	}
	return Result{Status: StatusPending}, nil
}

func (f *FakeProvider) Deprovision(ctx context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeprovisionCalls = append(f.DeprovisionCalls, domain)
	if f.deprovisionErr != nil {
		return f.deprovisionErr
	}
	delete(f.identities, domain)
	return nil
}

func (f *FakeProvider) List(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListCalls++
	out := make([]string, 0, len(f.identities))
	for d := range f.identities {
		out = append(out, d)
	}
	return out, nil
}

func (f *FakeProvider) ListPage(ctx context.Context, nextToken string, limit int) ([]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListPageCalls++
	all := make([]string, 0, len(f.identities))
	for domain := range f.identities {
		all = append(all, domain)
	}
	sort.Strings(all)
	start := 0
	if nextToken != "" {
		if _, err := fmt.Sscanf(nextToken, "%d", &start); err != nil || start < 0 || start > len(all) {
			return nil, "", fmt.Errorf("invalid fake provider page token %q", nextToken)
		}
	}
	if limit <= 0 {
		limit = 25
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	following := ""
	if end < len(all) {
		following = fmt.Sprintf("%d", end)
	}
	return append([]string(nil), all[start:end]...), following, nil
}
