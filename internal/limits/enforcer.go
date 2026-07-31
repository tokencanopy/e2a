package limits

import (
	"context"
	"sync"
	"time"

	"github.com/tokencanopy/e2a/internal/usage"
)

// Counter is the subset of *usage.Store the enforcer needs. Declared
// here as an interface so tests can supply a fake without standing up
// the full usage store.
type Counter interface {
	CountAgentsByUser(ctx context.Context, userID string) (int, error)
	CountDomainsByUser(ctx context.Context, userID string) (int, error)
	MessagesThisMonth(ctx context.Context, userID string) (int, error)
	GetStorageBytes(ctx context.Context, userID string) (int64, error)
}

// limitsReader is the subset of *Store the enforcer needs. Declared as
// an interface so tests can supply a fake without a real Postgres pool.
type limitsReader interface {
	Get(ctx context.Context, userID string) (Limits, bool, error)
}

// DBEnforcer is the production Enforcer: reads account_limits + falls
// back to operator Defaults, counts current resources via the usage
// store, and caches the resolved Limits in-process for cacheTTL to keep
// hot paths off the DB on every send.
//
// The cache only stores the *Limits* (caps), not the *counts*. Counts
// must always reflect the live database — caching them would mean a
// just-created agent or just-sent message wouldn't show up until TTL
// expiry, which would either let users exceed caps or let the dashboard
// lie about current usage. Reading the count is one bounded query per
// check; the win from caching limits is avoiding the join into
// account_limits, which is the costlier read.
type DBEnforcer struct {
	store    limitsReader
	counter  Counter
	defaults Defaults
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedLimits
}

// cachedLimits is one user's cache slot. It doubles as an invalidation
// tombstone: Invalidate does not delete the slot, it bumps gen and
// zeroes expires (a zero expires is always in the past, so cacheGet
// treats it as a miss). Retaining the slot is what lets a fill that was
// already in flight discover that it raced an invalidation.
//
// gen is the per-user invalidation generation. Get samples it BEFORE
// the DB read and hands it back to cachePut, which stores the fill only
// if gen still matches. Without that check this interleaving silently
// defeats an explicit invalidation:
//
//	T1 Get        -> cache miss, reads the OLD account_limits row
//	T2 sidecar    -> commits NEW limits, POSTs /api/internal/limits/invalidate
//	T2 Invalidate -> evicts the entry
//	T1 cachePut   -> stores the PRE-write limits with a FRESH TTL
//
// A user who had just upgraded would then keep hitting 402
// limit_exceeded for up to cacheTTL (60s in prod) even though billing
// did everything right — defeating the one mechanism billing has to
// make an upgrade take effect immediately. A counter rather than a
// timestamp because two events inside one clock tick must still
// compare as different.
type cachedLimits struct {
	limits  Limits
	expires time.Time
	gen     uint64
}

// NewEnforcer constructs the production enforcer. cacheTTL of 0 disables
// the cache (every Get hits the DB) — useful for tests that mutate
// account_limits and want immediate visibility.
func NewEnforcer(store *Store, counter Counter, defaults Defaults, cacheTTL time.Duration) *DBEnforcer {
	return &DBEnforcer{
		store:    store,
		counter:  counter,
		defaults: defaults,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cachedLimits),
	}
}

// newEnforcerWithReader is the test seam: lets unit tests inject a fake
// limitsReader so they don't need a Postgres pool.
func newEnforcerWithReader(reader limitsReader, counter Counter, defaults Defaults, cacheTTL time.Duration) *DBEnforcer {
	return &DBEnforcer{
		store:    reader,
		counter:  counter,
		defaults: defaults,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cachedLimits),
	}
}

func (e *DBEnforcer) Get(ctx context.Context, userID string) (Limits, error) {
	cached, gen, ok := e.cacheGet(userID)
	if ok {
		return cached, nil
	}
	// gen was sampled together with the miss, i.e. strictly BEFORE the
	// DB read below. cachePut re-checks it so a fill that raced an
	// Invalidate is dropped instead of re-caching pre-write limits.
	row, found, err := e.store.Get(ctx, userID)
	if err != nil {
		return Limits{}, err
	}
	var resolved Limits
	if found {
		resolved = row
	} else {
		resolved = Limits{
			PlanCode:         e.defaults.PlanCode,
			MaxAgents:        e.defaults.MaxAgents,
			MaxDomains:       e.defaults.MaxDomains,
			MaxMessagesMonth: e.defaults.MaxMessagesMonth,
			MaxStorageBytes:  e.defaults.MaxStorageBytes,
		}
	}
	e.cachePut(userID, resolved, gen)
	return resolved, nil
}

// Invalidate evicts the user's cached Limits and bumps their
// invalidation generation, so any fill already in flight (one that read
// account_limits before the caller's write committed) is discarded by
// cachePut rather than stored with a fresh TTL.
//
// The generation advances even for a user with NO cached entry. That is
// deliberate and load-bearing: an uncached user is precisely the racing
// case (miss → DB read → invalidate → cachePut), so skipping the
// tombstone when the key is absent would reintroduce the exact bug this
// guard exists to fix — the fill would come back at gen 0, match, and
// cache the pre-write limits. The cost is that a caller can mint a map
// entry for any key, so callers must bound what they pass;
// handleInvalidateLimits validates the user-id shape for that reason.
func (e *DBEnforcer) Invalidate(userID string) {
	if e.cacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureCacheLocked()
	// Leave a tombstone rather than deleting: the bumped generation has
	// to survive so a racing cachePut can see that it lost. expires is
	// left zero, which cacheGet reads as already-expired (a miss).
	e.cache[userID] = cachedLimits{gen: e.cache[userID].gen + 1}
}

// ensureCacheLocked lazily allocates the cache map. Both write paths
// (Invalidate, cachePut) call it because writing to a nil map panics, and
// Invalidate became a WRITE in the generation change — before it, it only
// deleted, which is safe on a nil map. The constructors always allocate, so
// this only covers a zero-value DBEnforcer built inside the package (the
// fields are unexported, so no external package can construct one that way).
// Caller must hold e.mu.
func (e *DBEnforcer) ensureCacheLocked() {
	if e.cache == nil {
		e.cache = make(map[string]cachedLimits)
	}
}

func (e *DBEnforcer) CheckAgentCreate(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	count, err := e.counter.CountAgentsByUser(ctx, userID)
	if err != nil {
		return err
	}
	if count >= lim.MaxAgents {
		return &LimitExceededError{
			Resource: "agents",
			Limit:    lim.MaxAgents,
			Current:  count,
			Limits:   lim,
		}
	}
	return nil
}

func (e *DBEnforcer) CheckDomainCreate(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	count, err := e.counter.CountDomainsByUser(ctx, userID)
	if err != nil {
		return err
	}
	if count >= lim.MaxDomains {
		return &LimitExceededError{
			Resource: "domains",
			Limit:    lim.MaxDomains,
			Current:  count,
			Limits:   lim,
		}
	}
	return nil
}

// CheckMessageSend enforces both the month-flow cap and the storage
// stock cap. Either being exceeded blocks the operation. The flow cap
// is checked first because it's the cheaper read; if both would fail,
// the user sees "messages_month" as the reason, which is the easier one
// to explain ("you sent N this month").
func (e *DBEnforcer) CheckMessageSend(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	msgCount, err := e.counter.MessagesThisMonth(ctx, userID)
	if err != nil {
		return err
	}
	if msgCount >= lim.MaxMessagesMonth {
		return &LimitExceededError{
			Resource: "messages_month",
			Limit:    lim.MaxMessagesMonth,
			Current:  msgCount,
			Limits:   lim,
		}
	}
	storage, err := e.counter.GetStorageBytes(ctx, userID)
	if err != nil {
		return err
	}
	if storage >= lim.MaxStorageBytes {
		// Storage values can exceed int32 (MaxStorageBytes is BIGINT in
		// the DB and may be 100 GiB+ on Scale). The LimitExceededError
		// fields are int — that's 32-bit on 32-bit platforms. Clamp via
		// safeInt64ToInt so we don't roll over on unusual targets; the
		// real value is also surfaced verbatim in the Limits field for
		// callers that need bytes-accurate access.
		return &LimitExceededError{
			Resource: "storage_bytes",
			Limit:    safeInt64ToInt(lim.MaxStorageBytes),
			Current:  safeInt64ToInt(storage),
			Limits:   lim,
		}
	}
	return nil
}

// safeInt64ToInt clamps an int64 to int.Max{,Min} so a downstream JSON
// encode never silently produces a negative number from int overflow
// on 32-bit Go targets. Production runs 64-bit; this is purely
// defensive against a future 32-bit build (or an ARM32 host).
func safeInt64ToInt(v int64) int {
	const maxInt = int64(^uint(0) >> 1)
	if v > maxInt {
		return int(maxInt)
	}
	return int(v)
}

// cacheGet returns the cached limits when they are present and unexpired.
// It always returns the user's current invalidation generation alongside
// the result — sampled under the same lock acquisition as the lookup —
// so a caller that observes a miss can pass that generation to cachePut
// and have the store rejected if an Invalidate landed in between.
func (e *DBEnforcer) cacheGet(userID string) (Limits, uint64, bool) {
	if e.cacheTTL <= 0 {
		return Limits{}, 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.cache[userID]
	if !ok || time.Now().After(c.expires) {
		return Limits{}, c.gen, false
	}
	return c.limits, c.gen, true
}

// cachePut stores a fill only if the user's invalidation generation is
// still the one the caller sampled before its DB read. A stale fill is
// dropped outright rather than stored with a shorter TTL: the next
// Get simply re-reads account_limits, which is the correct post-write
// value and costs one query.
func (e *DBEnforcer) cachePut(userID string, l Limits, gen uint64) {
	if e.cacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cache[userID].gen != gen {
		return
	}
	e.ensureCacheLocked()
	e.cache[userID] = cachedLimits{limits: l, expires: time.Now().Add(e.cacheTTL), gen: gen}
}

// Compile-time check: DBEnforcer satisfies the Enforcer interface and
// the Counter dependency is satisfied by *usage.Store.
var (
	_ Enforcer = (*DBEnforcer)(nil)
	_ Counter  = (*usage.Store)(nil)
)
