package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/idna"
)

// Identity tombstones (docs/design/account-soft-deletion.md §4.2, §4.5).
//
// A purged account leaves behind keyed digests of the identifiers it held —
// its login subject(s), its email, and (for abuse) every domain it verified —
// so the same identity cannot come straight back as a fresh account. The
// digests are HMAC-SHA256 under a DEDICATED tombstone key, never the
// feedback keyring, so retiring a feedback key can never erase a tombstone.

// Tombstone kinds and classes. They mirror the CHECK constraints in
// migration 122.
const (
	TombstoneKindLoginSubject = "login_subject"
	TombstoneKindEmail        = "email"
	TombstoneKindDomain       = "domain"
	// TombstoneKindAgentAddress holds a full agent address on a shared
	// (platform) domain, so an abuse-closed account's slugs cannot be
	// re-created by a fresh account.
	TombstoneKindAgentAddress = "agent_address"
	// TombstoneKindEmailDomain holds the domain part of an abuse-closed
	// owner's email when it is not a public webmail provider.
	TombstoneKindEmailDomain = "email_domain"

	TombstoneClassRecentDeletion = "recent_deletion"
	TombstoneClassAbuse          = "abuse"
)

// AbuseTombstoneHold is how long an abuse tombstone (and the deleted-account
// summary of an abuse-paused account) is kept after purge: two years, the
// hosted policy the privacy page discloses.
var AbuseTombstoneHold = 2 * 365 * 24 * time.Hour

// MinRecentDeletionHold is the floor of a recent_deletion tombstone: an
// identity is held for at least this long after any purge, whatever the
// deployment's trash window.
var MinRecentDeletionHold = 30 * 24 * time.Hour

// ErrRegistrationRefused is returned by the signup entry points when an
// identifier is held by a live identity tombstone. Surfaces as
// 403 registration_refused (domains keep the existing domain_taken).
var ErrRegistrationRefused = errors.New("identity: registration refused for a recently deleted or closed identity")

// ErrTombstoneKeyUnavailable is returned when tombstones are enabled but the
// tombstone key is not configured or cannot cover a live tombstone. Signup
// treats it as a dependency error (503), never as an empty table; purge skips
// the account rather than purge it without tombstones.
var ErrTombstoneKeyUnavailable = errors.New("identity: tombstone key unavailable")

// ErrAccountTrashed is returned when an operation resolves to an account that
// is in the trash and the operation must not act on it (provisioning replay,
// external-principal attach). Surfaces as 409 account_trashed.
var ErrAccountTrashed = errors.New("identity: account is in the trash")

// TombstoneKeyring is the versioned tombstone key. New digests are written
// under the active (highest) version; lookups compute the digest under every
// configured version so tombstones written before a rotation keep matching
// while their key stays configured.
type TombstoneKeyring struct {
	keys   map[int][]byte
	active int
}

// minTombstoneKeyBytes is the minimum decoded key length.
const minTombstoneKeyBytes = 32

// ParseTombstoneKeyring parses E2A_TOMBSTONE_KEY: a comma-separated list of
// `v<N>:<base64 secret>` entries (standard or URL-safe base64, padded or not),
// each decoding to at least 32 bytes. The highest version is active. An empty
// value returns (nil, nil) — "not configured". A malformed value is an error
// that never echoes the secret.
func ParseTombstoneKeyring(raw string) (*TombstoneKeyring, error) {
	return ParseTombstoneKeyringWithActive(raw, "")
}

// ParseTombstoneKeyringWithActive is ParseTombstoneKeyring with an explicit
// active selector (E2A_TOMBSTONE_KEY_ACTIVE, `v<N>`), so a rotation can be
// two-phase: deploy the new version KNOWN but inactive everywhere, then
// promote it. Empty selects the highest configured version. A selector naming
// a version that is not configured is an error.
func ParseTombstoneKeyringWithActive(raw, active string) (*TombstoneKeyring, error) {
	kr, err := parseTombstoneKeyring(raw)
	if err != nil || kr == nil {
		if err == nil && strings.TrimSpace(active) != "" {
			return nil, errors.New("E2A_TOMBSTONE_KEY_ACTIVE is set but E2A_TOMBSTONE_KEY is not")
		}
		return kr, err
	}
	if a := strings.TrimSpace(active); a != "" {
		v, err := strconv.Atoi(strings.TrimPrefix(a, "v"))
		if err != nil || !strings.HasPrefix(a, "v") || v <= 0 {
			return nil, fmt.Errorf("E2A_TOMBSTONE_KEY_ACTIVE must be v<version> (got %q)", a)
		}
		if !kr.Has(v) {
			return nil, fmt.Errorf("E2A_TOMBSTONE_KEY_ACTIVE selects v%d, which E2A_TOMBSTONE_KEY does not configure", v)
		}
		kr.active = v
	}
	return kr, nil
}

func parseTombstoneKeyring(raw string) (*TombstoneKeyring, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	kr := &TombstoneKeyring{keys: map[int][]byte{}}
	for i, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		label, secret, ok := strings.Cut(entry, ":")
		if !ok || !strings.HasPrefix(label, "v") {
			return nil, fmt.Errorf("tombstone key entry %d: want v<version>:<base64 secret>", i+1)
		}
		version, err := strconv.Atoi(strings.TrimPrefix(label, "v"))
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("tombstone key entry %d: version must be a positive integer", i+1)
		}
		if _, dup := kr.keys[version]; dup {
			return nil, fmt.Errorf("tombstone key entry %d: duplicate version v%d", i+1, version)
		}
		key, err := decodeTombstoneSecret(secret)
		if err != nil {
			return nil, fmt.Errorf("tombstone key v%d: secret is not valid base64", version)
		}
		if len(key) < minTombstoneKeyBytes {
			return nil, fmt.Errorf("tombstone key v%d: secret must decode to at least %d bytes", version, minTombstoneKeyBytes)
		}
		kr.keys[version] = key
		if version > kr.active {
			kr.active = version
		}
	}
	return kr, nil
}

func decodeTombstoneSecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// ActiveVersion is the version new digests are written under.
func (k *TombstoneKeyring) ActiveVersion() int {
	if k == nil {
		return 0
	}
	return k.active
}

// Versions lists the configured versions, highest first.
func (k *TombstoneKeyring) Versions() []int {
	if k == nil {
		return nil
	}
	out := make([]int, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// Has reports whether version v is configured.
func (k *TombstoneKeyring) Has(v int) bool {
	if k == nil {
		return false
	}
	_, ok := k.keys[v]
	return ok
}

// Digest computes the keyed digest of an already-normalized value under
// version v. The kind is bound into the MAC input so an email and a domain
// with equal text never share a digest.
func (k *TombstoneKeyring) Digest(v int, kind, normalized string) ([]byte, bool) {
	if k == nil {
		return nil, false
	}
	key, ok := k.keys[v]
	if !ok {
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(normalized))
	return mac.Sum(nil), true
}

// HexDigest is Digest under the active version, hex-encoded — the form the
// deleted-account summary stores for subjects and verified domains.
func (k *TombstoneKeyring) HexDigest(kind, normalized string) string {
	d, ok := k.Digest(k.ActiveVersion(), kind, normalized)
	if !ok {
		return ""
	}
	return hex.EncodeToString(d)
}

// TombstonePolicy is the store's tombstone configuration. Enabled is the
// hosted flag (trash.identity_tombstones); Keyring may be nil when the key is
// not configured, which makes every tombstone operation fail closed.
type TombstonePolicy struct {
	Enabled bool
	Keyring *TombstoneKeyring
}

// SetTombstonePolicy installs the tombstone configuration. Called once at
// startup, before the store serves traffic.
func (s *Store) SetTombstonePolicy(p TombstonePolicy) { s.tombstones = p }

// TombstonePolicy returns the installed tombstone configuration.
func (s *Store) TombstonePolicy() TombstonePolicy { return s.tombstones }

// TombstoneIdentifier is one (kind, value) pair to hold or check. Value is the
// raw identifier; it is normalized by NormalizeTombstoneValue before hashing.
type TombstoneIdentifier struct {
	Kind  string
	Value string
}

// NormalizeTombstoneValue canonicalizes an identifier before hashing so the
// trivially-equivalent spellings of one identity share a tombstone:
//
//   - email: trimmed and lower-cased; the local part loses any +suffix; for
//     gmail-hosted domains (gmail.com, googlemail.com) dots in the local part
//     are folded and the domain canonicalizes to gmail.com; the domain part is
//     IDNA-normalized to ASCII.
//   - login subject: byte-exact (subjects are opaque issuer-assigned values).
//   - domain: trimmed, lower-cased, trailing dot removed, IDNA to ASCII.
func NormalizeTombstoneValue(kind, value string) string {
	switch kind {
	case TombstoneKindEmail:
		return normalizeTombstoneEmail(value)
	case TombstoneKindDomain, TombstoneKindEmailDomain:
		return normalizeTombstoneDomain(value)
	case TombstoneKindAgentAddress:
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return value
	}
}

// publicWebmailDomains are email providers whose domain identifies nobody: an
// abuse hold never closes them.
var publicWebmailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true,
	"live.com": true, "yahoo.com": true, "icloud.com": true, "proton.me": true,
	"protonmail.com": true, "aol.com": true, "gmx.com": true, "gmx.net": true,
	"yandex.com": true, "yandex.ru": true, "mail.com": true,
}

// emailDomainOf returns the normalized domain part of an email, or "".
func emailDomainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return normalizeTombstoneDomain(email[at+1:])
}

// IsPublicWebmailDomain reports whether d is a public webmail provider.
func IsPublicWebmailDomain(d string) bool {
	return publicWebmailDomains[normalizeTombstoneDomain(d)]
}

// signupIdentifiers is what every signup entry point checks for an email:
// the address and its domain (only non-webmail domains are ever held).
func signupEmailIdentifiers(email string) []TombstoneIdentifier {
	ids := []TombstoneIdentifier{{Kind: TombstoneKindEmail, Value: email}}
	if d := emailDomainOf(email); d != "" {
		ids = append(ids, TombstoneIdentifier{Kind: TombstoneKindEmailDomain, Value: d})
	}
	return ids
}

func normalizeTombstoneDomain(d string) string {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if ascii, err := idna.Lookup.ToASCII(d); err == nil {
		return ascii
	}
	return d
}

func normalizeTombstoneEmail(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	at := strings.LastIndex(e, "@")
	if at <= 0 || at == len(e)-1 {
		return e
	}
	local, domain := e[:at], normalizeTombstoneDomain(e[at+1:])
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	if domain == "gmail.com" || domain == "googlemail.com" {
		local = strings.ReplaceAll(local, ".", "")
		domain = "gmail.com"
	}
	return local + "@" + domain
}

// externalPrincipalSubject is the login-subject value a delegated external
// principal is held under: the issuer is bound in so two issuers' equal
// subjects never collide.
func externalPrincipalSubject(issuer, subject string) string {
	return "ext:" + issuer + "\n" + subject
}

// tombstoneDigestSet is the digest of one identifier under every configured
// key version.
type tombstoneDigestSet struct {
	kind     string
	versions []int
	digests  [][]byte
}

func (k *TombstoneKeyring) digestsFor(id TombstoneIdentifier) tombstoneDigestSet {
	norm := NormalizeTombstoneValue(id.Kind, id.Value)
	set := tombstoneDigestSet{kind: id.Kind}
	for _, v := range k.Versions() {
		d, _ := k.Digest(v, id.Kind, norm)
		set.versions = append(set.versions, v)
		set.digests = append(set.digests, d)
	}
	return set
}

// lockTombstoneDigests takes a transaction-scoped advisory lock on the active
// digest of every identifier, in sorted order, so a signup check and a purge's
// tombstone write for the same identifier serialize.
func lockTombstoneDigests(ctx context.Context, tx pgx.Tx, kr *TombstoneKeyring, ids []TombstoneIdentifier) error {
	var keys []string
	for _, id := range ids {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		d, ok := kr.Digest(kr.ActiveVersion(), id.Kind, NormalizeTombstoneValue(id.Kind, id.Value))
		if !ok {
			return ErrTombstoneKeyUnavailable
		}
		keys = append(keys, id.Kind+":"+hex.EncodeToString(d))
	}
	sort.Strings(keys)
	for i, key := range keys {
		if i > 0 && keys[i-1] == key {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key); err != nil {
			return err
		}
	}
	return nil
}

// checkTombstonesTx refuses with ErrRegistrationRefused when any identifier is
// held by a live tombstone, after taking the digest advisory locks. With the
// policy disabled it checks nothing. With it enabled and no key, or with a
// live tombstone written under a key version this process cannot compute, it
// fails closed with ErrTombstoneKeyUnavailable.
func (s *Store) checkTombstonesTx(ctx context.Context, tx pgx.Tx, ids []TombstoneIdentifier) error {
	p := s.tombstones
	if !p.Enabled {
		return nil
	}
	if p.Keyring == nil {
		return ErrTombstoneKeyUnavailable
	}
	if err := lockTombstoneDigests(ctx, tx, p.Keyring, ids); err != nil {
		return err
	}
	if err := ensureTombstoneVersionsCoveredTx(ctx, tx, p.Keyring); err != nil {
		return err
	}
	for _, id := range ids {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		set := p.Keyring.digestsFor(id)
		for i, v := range set.versions {
			var held bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (
				    SELECT 1 FROM identity_tombstones
				     WHERE kind = $1 AND digest = $2 AND key_version = $3 AND expires_at > now())`,
				set.kind, set.digests[i], v,
			).Scan(&held); err != nil {
				return fmt.Errorf("identity: check tombstone: %w", err)
			}
			if held {
				return ErrRegistrationRefused
			}
		}
	}
	return nil
}

// ensureTombstoneVersionsCoveredTx fails closed when a live tombstone was
// written under a key version this process does not hold: such a tombstone
// could never match, so treating the table as authoritative would silently
// reopen every identity it holds.
func ensureTombstoneVersionsCoveredTx(ctx context.Context, tx pgx.Tx, kr *TombstoneKeyring) error {
	var missing bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM identity_tombstones
		     WHERE expires_at > now() AND NOT (key_version = ANY($1::int[])))`,
		kr.Versions(),
	).Scan(&missing); err != nil {
		return fmt.Errorf("identity: check tombstone key coverage: %w", err)
	}
	if missing {
		return ErrTombstoneKeyUnavailable
	}
	return nil
}

// writeTombstonesTx upserts one tombstone per identifier under the active key
// version. A replay (a resumed purge) keeps the later expiry and the stronger
// class, so re-running can only extend a hold, never shorten or weaken it.
func writeTombstonesTx(ctx context.Context, tx pgx.Tx, kr *TombstoneKeyring, ids []TombstoneIdentifier, class, accountRef string, expiresAt time.Time) (int, error) {
	if err := lockTombstoneDigests(ctx, tx, kr, ids); err != nil {
		return 0, err
	}
	written := 0
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		norm := NormalizeTombstoneValue(id.Kind, id.Value)
		if seen[id.Kind+"\x00"+norm] {
			continue
		}
		seen[id.Kind+"\x00"+norm] = true
		d, ok := kr.Digest(kr.ActiveVersion(), id.Kind, norm)
		if !ok {
			return written, ErrTombstoneKeyUnavailable
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (kind, digest, key_version) DO UPDATE
			   SET expires_at = GREATEST(identity_tombstones.expires_at, EXCLUDED.expires_at),
			       class = CASE WHEN identity_tombstones.class = 'abuse' OR EXCLUDED.class = 'abuse'
			                    THEN 'abuse' ELSE 'recent_deletion' END,
			       account_ref = CASE WHEN EXCLUDED.class = 'abuse' AND identity_tombstones.class <> 'abuse'
			                          THEN EXCLUDED.account_ref ELSE identity_tombstones.account_ref END`,
			id.Kind, d, kr.ActiveVersion(), class, accountRef, expiresAt,
		); err != nil {
			return written, fmt.Errorf("identity: write tombstone: %w", err)
		}
		written++
	}
	return written, nil
}

// RecentDeletionHold is how long a recent_deletion tombstone is kept after a
// purge at `now`: the longest of the deployment's trash window, the remainder
// of the current quota period (the UTC calendar month), and the 30-day floor.
// Closing the window at the period boundary is what stops erase-and-return
// from resetting a monthly allowance.
func RecentDeletionHold(now time.Time) time.Duration {
	now = now.UTC()
	nextPeriod := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	hold := nextPeriod.Sub(now)
	if AccountTrashRetention > hold {
		hold = AccountTrashRetention
	}
	if TrashRetention > hold {
		hold = TrashRetention
	}
	if MinRecentDeletionHold > hold {
		hold = MinRecentDeletionHold
	}
	return hold
}

// TombstoneKeyStatus is the operator view of the tombstone key: which versions
// are configured and how many live tombstones depend on each version.
type TombstoneKeyStatus struct {
	Enabled           bool          `json:"enabled"`
	Configured        bool          `json:"configured"`
	ActiveVersion     int           `json:"active_version"`
	ConfiguredVersion []int         `json:"known_versions"`
	LiveByVersion     map[int]int64 `json:"live_tombstones_by_version"`
	MissingVersions   []int         `json:"missing_versions"`
}

// InspectTombstoneKeys reports the key coverage of the live tombstone table:
// the active version (what new tombstones are written under), every known
// version (what lookups try), and how many live tombstones use each. A
// rotation is two-phase: deploy the new version known-but-inactive
// (E2A_TOMBSTONE_KEY_ACTIVE pinned to the old one), then promote it; keep the
// old version known until LiveByVersion shows no live tombstone still depends
// on it (the longest hold is the abuse hold). A keyed digest cannot be recomputed
// under a new key without the plaintext identifier, which e2a deliberately
// never stores, so rotation retires old versions by expiry, not by re-keying.
func (s *Store) InspectTombstoneKeys(ctx context.Context) (*TombstoneKeyStatus, error) {
	p := s.tombstones
	st := &TombstoneKeyStatus{
		Enabled:           p.Enabled,
		Configured:        p.Keyring != nil,
		ActiveVersion:     p.Keyring.ActiveVersion(),
		ConfiguredVersion: p.Keyring.Versions(),
		LiveByVersion:     map[int]int64{},
		MissingVersions:   []int{},
	}
	if st.ConfiguredVersion == nil {
		st.ConfiguredVersion = []int{}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key_version, count(*) FROM identity_tombstones WHERE expires_at > now() GROUP BY key_version ORDER BY key_version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var n int64
		if err := rows.Scan(&v, &n); err != nil {
			return nil, err
		}
		st.LiveByVersion[v] = n
		if !p.Keyring.Has(v) {
			st.MissingVersions = append(st.MissingVersions, v)
		}
	}
	return st, rows.Err()
}

// ExtendAccountTombstones moves every live tombstone of accountRef to
// expire no earlier than `until` (an operator lever for a closed identity).
// Returns the number of rows touched.
func (s *Store) ExtendAccountTombstones(ctx context.Context, accountRef string, until time.Time, actor, reason string) (int64, error) {
	var n int64
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE identity_tombstones SET expires_at = GREATEST(expires_at, $2)
			  WHERE account_ref = $1 AND expires_at > now()`, accountRef, until)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return recordTombstoneEventTx(ctx, tx, accountRef, "extend", n, actor, reason)
	})
	return n, err
}

// TombstoneAuditRetention keeps the operator audit of tombstone overrides a
// little longer than the longest hold it can affect.
var TombstoneAuditRetention = AbuseTombstoneHold + 90*24*time.Hour

// recordTombstoneEventTx writes the operator audit row for a tombstone
// override in the same transaction as the override.
func recordTombstoneEventTx(ctx context.Context, tx pgx.Tx, accountRef, action string, rows int64, actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len([]rune(reason)) > 1000 {
		return errors.New("identity: a tombstone override requires an actor and a nonblank reason of at most 1000 characters")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO identity_tombstone_events (id, account_ref, action, rows_affected, actor, reason, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))`,
		"ite_"+generateID(), accountRef, action, rows, actor, reason, TombstoneAuditRetention.Seconds())
	if err != nil {
		return fmt.Errorf("identity: record tombstone event: %w", err)
	}
	return nil
}

// RevokeAccountTombstones reopens a purged account's identifiers. It works
// from the digest set recorded in the account's deleted-account summary, not
// from account_ref: a digest the upsert attributed to this account may also be
// held for ANOTHER purged account (the same email or domain), and that other
// account's hold must survive. A digest listed in another live summary is
// kept and counted in kept. Without a summary it falls back to the rows
// attributed to accountRef.
func (s *Store) RevokeAccountTombstones(ctx context.Context, accountRef, actor, reason string) (revoked, kept int64, err error) {
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		qerr := tx.QueryRow(ctx,
			`SELECT identity_digests FROM deleted_account_summaries WHERE account_ref = $1`, accountRef).Scan(&raw)
		if errors.Is(qerr, pgx.ErrNoRows) {
			tag, err := tx.Exec(ctx, `DELETE FROM identity_tombstones WHERE account_ref = $1`, accountRef)
			if err != nil {
				return err
			}
			revoked = tag.RowsAffected()
			return recordTombstoneEventTx(ctx, tx, accountRef, "revoke", revoked, actor, reason)
		}
		if qerr != nil {
			return qerr
		}
		var digests []IdentityDigest
		if err := json.Unmarshal(raw, &digests); err != nil {
			return fmt.Errorf("decode identity digests: %w", err)
		}
		for _, d := range digests {
			bin, err := hex.DecodeString(d.Digest)
			if err != nil {
				return fmt.Errorf("decode digest: %w", err)
			}
			// Does another live purged account hold this digest? An abuse
			// summary holds every identifier it recorded; a recent-deletion
			// summary holds only its login subjects and email.
			var otherRef, otherClass *string
			var otherUntil *time.Time
			if err := tx.QueryRow(ctx, `
				SELECT account_ref, retention_class, expires_at FROM deleted_account_summaries
				 WHERE account_ref <> $1 AND expires_at > now()
				   AND identity_digests @> jsonb_build_array(jsonb_build_object('kind', $2::text, 'digest', $3::text))
				   AND (retention_class = 'abuse' OR $2 IN ('login_subject', 'email'))
				 ORDER BY (retention_class = 'abuse') DESC, expires_at DESC
				 LIMIT 1`,
				accountRef, d.Kind, d.Digest).Scan(&otherRef, &otherClass, &otherUntil); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if otherRef != nil {
				// Keep the hold, but at the OTHER account's class and expiry,
				// not at whatever this account's upsert left behind.
				if _, err := tx.Exec(ctx, `
					UPDATE identity_tombstones SET class = $4, expires_at = $5, account_ref = $6
					 WHERE kind = $1 AND digest = $2 AND key_version = $3`,
					d.Kind, bin, d.KeyVersion, *otherClass, *otherUntil, *otherRef); err != nil {
					return err
				}
				kept++
				continue
			}
			tag, err := tx.Exec(ctx,
				`DELETE FROM identity_tombstones WHERE kind = $1 AND digest = $2 AND key_version = $3`,
				d.Kind, bin, d.KeyVersion)
			if err != nil {
				return err
			}
			revoked += tag.RowsAffected()
		}
		return recordTombstoneEventTx(ctx, tx, accountRef, "revoke", revoked, actor, reason)
	})
	return revoked, kept, err
}

// IdentityDigest is one keyed identifier digest recorded in a deleted-account
// summary (hex digest, the key version it was computed under).
type IdentityDigest struct {
	Kind       string `json:"kind"`
	Digest     string `json:"digest"`
	KeyVersion int    `json:"key_version"`
}

// EscalateDeletedAccountToAbuse is the operator lever for "they purged before
// we classified": from the purged account's summary it writes (or extends)
// an abuse-class tombstone for every recorded identifier digest and extends
// the summary to the abuse hold. Works entirely from digests — no plaintext is
// needed or available. Returns the number of tombstones written.
func (s *Store) EscalateDeletedAccountToAbuse(ctx context.Context, accountRef, actor, reason string) (int, error) {
	written := 0
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT identity_digests FROM deleted_account_summaries WHERE account_ref = $1 FOR UPDATE`, accountRef,
		).Scan(&raw); err != nil {
			return err
		}
		var digests []IdentityDigest
		if err := json.Unmarshal(raw, &digests); err != nil {
			return fmt.Errorf("decode identity digests: %w", err)
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		until := now.Add(AbuseTombstoneHold)
		// Take every digest lock first, in sorted order — the same order the
		// purge and signup paths lock in — so escalation cannot deadlock
		// against a concurrent purge.
		keys := make([]string, 0, len(digests))
		for _, d := range digests {
			keys = append(keys, d.Kind+":"+d.Digest)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 && keys[i-1] == k {
				continue
			}
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, k); err != nil {
				return err
			}
		}
		for _, d := range digests {
			bin, err := hex.DecodeString(d.Digest)
			if err != nil || len(bin) != 32 || d.KeyVersion <= 0 {
				return fmt.Errorf("summary holds a malformed digest for kind %s", d.Kind)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at)
				VALUES ($1, $2, $3, 'abuse', $4, $5)
				ON CONFLICT (kind, digest, key_version) DO UPDATE
				   SET class = 'abuse',
				       expires_at = GREATEST(identity_tombstones.expires_at, EXCLUDED.expires_at),
				       account_ref = EXCLUDED.account_ref`,
				d.Kind, bin, d.KeyVersion, accountRef, until); err != nil {
				return fmt.Errorf("escalate tombstone: %w", err)
			}
			written++
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deleted_account_summaries
			   SET retention_class = 'abuse', expires_at = GREATEST(expires_at, $2)
			 WHERE account_ref = $1`, accountRef, until); err != nil {
			return err
		}
		return recordTombstoneEventTx(ctx, tx, accountRef, "escalate_abuse", int64(written), actor, reason)
	})
	return written, err
}

// DeleteExpiredTombstones removes tombstones past their hold (and expired
// tombstone-override audit rows).
func (s *Store) DeleteExpiredTombstones(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM identity_tombstones WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM identity_tombstone_events WHERE expires_at <= now()`); err != nil {
		return tag.RowsAffected(), err
	}
	return tag.RowsAffected(), nil
}
