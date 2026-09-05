# Onboarding Acquisition Survey Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ask every dashboard user who has not answered yet, once, "Where did you hear about e2a?" on a blocking `/welcome` page, and store the answer write-once on the `users` row.

**Architecture:** Three nullable columns on `users` (migration 120). A config flag `onboarding_survey.enabled` (default off) gates the write path and a new `onboarding_survey_pending` boolean on `GET /api/auth/me`; `PATCH /api/auth/me` accepts a nested `onboarding_survey` object. The app shell redirects a pending user to `/welcome` and renders that page without the sidebar. Hosted operators flip the flag in their deployment config; self-host sees no behaviour change.

**Tech Stack:** Go 1.2x server (`internal/auth`, `internal/identity`, `internal/config`, pgx), Postgres migrations (`migrations/`), Next.js 15 app router dashboard (`web/`, Jest + Testing Library).

**Spec:** private ops repo, `docs/design/onboarding-survey.md` (decisions D1–D4 and the API contract). The contract is restated here where a task needs it.

## Global Constraints

- Migration file name: `migrations/120_users_acquisition_survey.sql`; every statement idempotent (`IF NOT EXISTS`, constraint adds wrapped in `DO $$ ... EXCEPTION WHEN duplicate_object`).
- Source enum, exact strings, in this order: `search`, `ai_assistant`, `github`, `x_twitter`, `hn_reddit`, `content`, `mcp_directory`, `word_of_mouth`, `other`, `skipped`.
- Display labels, exact copy: Search engine · ChatGPT / Claude / another AI assistant · GitHub · X / Twitter · Hacker News / Reddit · YouTube, podcast, or blog · MCP directory · Friend or colleague · Other.
- Page heading, exact copy: `Where did you hear about e2a?`. Buttons: `Continue`, `Skip`. Detail placeholder: `Tell us more (optional)`.
- `detail` limit: 200 characters (Unicode code points), after trimming.
- Error bodies for the survey path are JSON: `{"error":"onboarding_survey_already_answered"}` (409), `{"error":"onboarding_survey_disabled"}` (404). Other validation failures keep the handler's existing plain-text `http.Error` 400 style.
- No customer data, real addresses, or hosted identifiers in code, tests, fixtures, or commit messages. Fixtures use `*.test` / `example.com`.
- Work happens in the worktree `.worktrees/onboarding-survey` on branch `feat/onboarding-survey`; never touch the root checkout.
- DB-backed Go tests need the local test Postgres (`E2A_TEST_DATABASE_URL` defaults to `localhost:5433`, already running on this machine; check with `pg_isready -h localhost -p 5433`). Tests skip, not fail, when it is down, so confirm with `-v` that they actually ran.
- Web commands run inside `web/` (`npm ci` once in the worktree, then `npx jest <path>`, `npm run lint`, `npx tsc --noEmit`).
- Two agents may share this worktree (Go track and web track). Commit with explicit paths only, `git commit -m "..." -- <files>`, never a bare `git commit` or `git add -A`, so one track never sweeps the other's staged files into its commit. If `index.lock` exists, wait a few seconds and retry.
- Commit after every task with a conventional-commit subject and the session trailer:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01WPerbtyZtci8gFA8kem8Cm
  ```

---

## File map

| File | Responsibility |
|---|---|
| `migrations/120_users_acquisition_survey.sql` | the three columns + two CHECK constraints |
| `internal/identity/migrate_test.go` | migration shape + idempotency test (append) |
| `internal/identity/acquisition.go` | the source enum and `IsAcquisitionSource` |
| `internal/identity/acquisition_test.go` | enum test |
| `internal/identity/store.go` | `User.AcquisitionAnsweredAt`, `RecordAcquisitionSurvey`, `ErrAcquisitionSurveyAnswered`, widened scans |
| `internal/identity/store_acquisition_test.go` | store tests (write-once, concurrency) |
| `internal/config/config.go` | `OnboardingSurveyConfig` + env override |
| `internal/config/config_test.go` | env override test (append) |
| `config.example.yaml` | documented block |
| `internal/auth/auth.go` | `SetOnboardingSurveyEnabled`, `meResponse`, `writeMe`, PATCH survey branch |
| `internal/auth/auth_test.go` | handler tests (append) |
| `cmd/e2a/main.go` | wire the flag into `UserAuth` |
| `web/src/lib/acquisitionSources.ts` | option list, labels, detail limit |
| `web/src/lib/acquisitionSources.test.ts` | enum test |
| `web/src/app/components/types.ts` | `UserInfo.onboarding_survey_pending`, `UpdateMeRequest.onboarding_survey` |
| `web/src/app/(app)/AppLayoutClient.tsx` | redirect gate + chrome-less `/welcome` render |
| `web/src/app/(app)/layout.test.tsx` | gate tests (append) + `next/navigation` mock |
| `web/src/app/(app)/welcome/page.tsx` | the survey page |
| `web/src/app/(app)/welcome/page.test.tsx` | page tests |

---

### Task 1: Migration 120

**Files:**
- Create: `migrations/120_users_acquisition_survey.sql`
- Test: `internal/identity/migrate_test.go` (append)

**Interfaces:**
- Produces: columns `users.acquisition_source TEXT NULL`, `users.acquisition_detail TEXT NULL`, `users.acquisition_answered_at TIMESTAMPTZ NULL`; constraints `users_acquisition_source_check`, `users_acquisition_answered_check`.

- [ ] **Step 1: Write the failing migration test**

Append to `internal/identity/migrate_test.go` (package `identity_test`; `testutil.TestDB` applies every embedded migration, so re-applying 120 proves idempotency):

```go
func TestUsersAcquisitionSurveyMigrationIsNullableIdempotentAndConstrained(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	sql, err := migrations.FS.ReadFile("120_users_acquisition_survey.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second migration application: %v", err)
	}
	for _, column := range []string{"acquisition_source", "acquisition_detail", "acquisition_answered_at"} {
		var nullable, defaultValue string
		if err := pool.QueryRow(ctx, `SELECT is_nullable,COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name=$1`, column).Scan(&nullable, &defaultValue); err != nil {
			t.Fatalf("column %s: %v", column, err)
		}
		if nullable != "YES" || defaultValue != "" {
			t.Fatalf("column %s nullable=%q default=%q", column, nullable, defaultValue)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, name, google_subject) VALUES ('usr_mig108', 'mig108@example.test', 'M', 'sub-mig108')`); err != nil {
		t.Fatal(err)
	}
	// Unknown source is rejected by the CHECK.
	if _, err := pool.Exec(ctx, `UPDATE users SET acquisition_source='carrier_pigeon', acquisition_answered_at=now() WHERE id='usr_mig108'`); err == nil {
		t.Fatal("unknown acquisition_source was accepted")
	}
	// Source without timestamp is rejected (both-null-or-both-set).
	if _, err := pool.Exec(ctx, `UPDATE users SET acquisition_source='github' WHERE id='usr_mig108'`); err == nil {
		t.Fatal("acquisition_source without acquisition_answered_at was accepted")
	}
	// Valid pair is accepted.
	if _, err := pool.Exec(ctx, `UPDATE users SET acquisition_source='github', acquisition_answered_at=now() WHERE id='usr_mig108'`); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/identity/ -run TestUsersAcquisitionSurveyMigration -count=1`
Expected: FAIL with `open 120_users_acquisition_survey.sql: file does not exist`. If it says SKIP, start the test DB (`make docker-up`) and rerun.

- [ ] **Step 3: Write the migration**

`migrations/120_users_acquisition_survey.sql`:

```sql
-- Onboarding acquisition survey ("Where did you hear about e2a?").
-- Write-once per user; NULL = not yet asked, 'skipped' = asked and
-- declined. The dashboard only shows the survey when the server's
-- onboarding_survey.enabled flag is on, so these columns stay NULL on
-- deployments that never enable it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_source TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_detail TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_answered_at TIMESTAMPTZ;

DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_acquisition_source_check
        CHECK (acquisition_source IS NULL OR acquisition_source IN (
            'search', 'ai_assistant', 'github', 'x_twitter', 'hn_reddit',
            'content', 'mcp_directory', 'word_of_mouth', 'other', 'skipped'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Source and timestamp are set together or not at all.
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_acquisition_answered_check
        CHECK ((acquisition_source IS NULL) = (acquisition_answered_at IS NULL));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/identity/ -run TestUsersAcquisitionSurveyMigration -count=1 -v`
Expected: PASS (and the line `--- PASS`, not `--- SKIP`).

- [ ] **Step 5: Commit**

```bash
git add migrations/120_users_acquisition_survey.sql internal/identity/migrate_test.go
git commit -m "feat(db): add users acquisition survey columns (migration 120)"
```

---

### Task 2: Identity layer — enum, user field, write-once store method

**Files:**
- Create: `internal/identity/acquisition.go`, `internal/identity/acquisition_test.go`, `internal/identity/store_acquisition_test.go`
- Modify: `internal/identity/store.go` (`User` struct ~line 306; `GetUserByID` ~5944; `UpdateUserName` ~5960; `GetUserSession` ~5990)

**Interfaces:**
- Consumes: Task 1 columns.
- Produces:
  - `identity.AcquisitionSources []string`, `identity.AcquisitionSourceSkipped = "skipped"`, `identity.IsAcquisitionSource(s string) bool`
  - `identity.User.AcquisitionAnsweredAt *time.Time` (json `-`), populated by `GetUserSession`, `GetUserByID`, `UpdateUserName`, `RecordAcquisitionSurvey`
  - `identity.ErrAcquisitionSurveyAnswered error`
  - `func (s *Store) RecordAcquisitionSurvey(ctx context.Context, userID, source string, detail *string) (*User, error)`

- [ ] **Step 1: Write the failing enum test**

`internal/identity/acquisition_test.go`:

```go
package identity_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

func TestAcquisitionSourcesMatchMigrationEnum(t *testing.T) {
	want := []string{"search", "ai_assistant", "github", "x_twitter", "hn_reddit",
		"content", "mcp_directory", "word_of_mouth", "other", "skipped"}
	if len(identity.AcquisitionSources) != len(want) {
		t.Fatalf("len = %d, want %d", len(identity.AcquisitionSources), len(want))
	}
	for i, s := range want {
		if identity.AcquisitionSources[i] != s {
			t.Errorf("[%d] = %q, want %q", i, identity.AcquisitionSources[i], s)
		}
		if !identity.IsAcquisitionSource(s) {
			t.Errorf("IsAcquisitionSource(%q) = false", s)
		}
	}
	for _, bad := range []string{"", "Search", "carrier_pigeon", " github"} {
		if identity.IsAcquisitionSource(bad) {
			t.Errorf("IsAcquisitionSource(%q) = true", bad)
		}
	}
	if identity.AcquisitionSourceSkipped != "skipped" {
		t.Errorf("AcquisitionSourceSkipped = %q", identity.AcquisitionSourceSkipped)
	}
}
```

- [ ] **Step 2: Write the failing store test**

`internal/identity/store_acquisition_test.go`:

```go
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
```

- [ ] **Step 3: Run both to verify they fail**

Run: `go test ./internal/identity/ -run 'TestAcquisitionSources|TestRecordAcquisitionSurvey' -count=1`
Expected: build FAIL, `undefined: identity.AcquisitionSources` and `undefined: identity.ErrAcquisitionSurveyAnswered`.

- [ ] **Step 4: Implement the enum**

`internal/identity/acquisition.go`:

```go
package identity

import "errors"

// AcquisitionSources is the closed answer set for the onboarding survey
// ("Where did you hear about e2a?"). It must match the CHECK constraint
// in migrations/120_users_acquisition_survey.sql exactly — the values
// are the analytics enum, so they are code, not config.
var AcquisitionSources = []string{
	"search",
	"ai_assistant",
	"github",
	"x_twitter",
	"hn_reddit",
	"content",
	"mcp_directory",
	"word_of_mouth",
	"other",
	AcquisitionSourceSkipped,
}

// AcquisitionSourceSkipped records "asked, declined". It counts as
// answered so the survey never reappears.
const AcquisitionSourceSkipped = "skipped"

// ErrAcquisitionSurveyAnswered is returned by RecordAcquisitionSurvey when
// the user already has an answer on file. The first answer is kept.
var ErrAcquisitionSurveyAnswered = errors.New("acquisition survey already answered")

// IsAcquisitionSource reports whether s is exactly one of AcquisitionSources.
func IsAcquisitionSource(s string) bool {
	for _, v := range AcquisitionSources {
		if v == s {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Add the user field and widen the loaders**

In `internal/identity/store.go`, add to `User` (after `CreatedAt`):

```go
	// AcquisitionAnsweredAt is when the onboarding survey was answered or
	// skipped; nil = not yet asked. Loaded by the session/ID loaders that
	// feed /api/auth/me, hidden from API JSON (the auth handler derives a
	// boolean from it).
	AcquisitionAnsweredAt *time.Time `json:"-"`
```

Then change these three queries so each SELECT/RETURNING list ends with `acquisition_answered_at` and each `Scan` ends with `&u.AcquisitionAnsweredAt`:

- `GetUserByID`: `SELECT id, email, name, google_subject, created_at, account_class, acquisition_answered_at FROM users WHERE id = $1` → `.Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)`
- `UpdateUserName`: `RETURNING id, email, name, google_subject, created_at, acquisition_answered_at` → `.Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AcquisitionAnsweredAt)`
- `GetUserSession`: `SELECT u.id, u.email, u.name, u.google_subject, u.created_at, u.account_class, u.acquisition_answered_at ...` → `.Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)`

Leave the other user loaders (`CreateOrGetUser`, `GetUserByEmail`, the OAuth-principal join) alone: a freshly created user has no answer, and those callers never serialize the survey state.

- [ ] **Step 6: Implement the store method**

Append to `internal/identity/store.go` directly after `UpdateUserName` (`errors`, `fmt`, and `pgx` are already imported in this file, verified):

```go
// RecordAcquisitionSurvey stores the onboarding survey answer for a user,
// write-once: the UPDATE is conditioned on acquisition_answered_at IS
// NULL so two concurrent submits cannot both win. A no-op UPDATE is
// disambiguated with a follow-up lookup — ErrAcquisitionSurveyAnswered
// when the user exists, the lookup's not-found error otherwise. Source
// validity is the caller's job (the handler maps it to a 400), but the
// value is re-checked here so no path can write outside the enum.
func (s *Store) RecordAcquisitionSurvey(ctx context.Context, userID, source string, detail *string) (*User, error) {
	if !IsAcquisitionSource(source) {
		return nil, fmt.Errorf("invalid acquisition source %q", source)
	}
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`UPDATE users
		 SET acquisition_source = $1, acquisition_detail = $2, acquisition_answered_at = now()
		 WHERE id = $3 AND acquisition_answered_at IS NULL
		 RETURNING id, email, name, google_subject, created_at, account_class, acquisition_answered_at`,
		source, detail, userID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := s.GetUserByID(ctx, userID); lookupErr != nil {
			return nil, lookupErr
		}
		return nil, ErrAcquisitionSurveyAnswered
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}
```

- [ ] **Step 7: Run the identity package tests**

Run: `go test ./internal/identity/ -count=1`
Expected: PASS, including the four new tests and the migration test. Any pre-existing test that scans users columns positionally will fail here if a Scan list was left short; fix the Scan, not the test.

- [ ] **Step 8: Commit**

```bash
git add internal/identity/acquisition.go internal/identity/acquisition_test.go internal/identity/store.go internal/identity/store_acquisition_test.go
git commit -m "feat(identity): write-once acquisition survey store method"
```

---

### Task 3: Config flag

**Files:**
- Modify: `internal/config/config.go` (`Config` struct ~line 44; `OutboundFooterConfig` block ~127 as the pattern; env overrides ~562)
- Modify: `internal/config/config_test.go` (append)
- Modify: `config.example.yaml` (after the `outbound_footer` block ~line 296)

**Interfaces:**
- Produces: `config.Config.OnboardingSurvey config.OnboardingSurveyConfig` with `Enabled bool` (yaml `onboarding_survey.enabled`, env `E2A_ONBOARDING_SURVEY_ENABLED`).

- [ ] **Step 1: Write the failing config tests**

Append to `internal/config/config_test.go` (look at `TestLoadConfigEnvOverrides` ~line 92 for how a temp YAML file is written and loaded; reuse its helper if there is one, otherwise `os.WriteTemp` a file with the minimal required keys the other tests use):

```go
func TestOnboardingSurveyDefaultsOffAndLoadsFromYAML(t *testing.T) {
	cfg := loadConfigFromYAML(t, minimalConfigYAML)
	if cfg.OnboardingSurvey.Enabled {
		t.Fatal("onboarding_survey.enabled should default to false")
	}
	cfg = loadConfigFromYAML(t, minimalConfigYAML+"\nonboarding_survey:\n  enabled: true\n")
	if !cfg.OnboardingSurvey.Enabled {
		t.Fatal("onboarding_survey.enabled=true not loaded from YAML")
	}
}

func TestOnboardingSurveyEnvOverride(t *testing.T) {
	t.Setenv("E2A_ONBOARDING_SURVEY_ENABLED", "true")
	cfg := loadConfigFromYAML(t, minimalConfigYAML)
	if !cfg.OnboardingSurvey.Enabled {
		t.Fatal("E2A_ONBOARDING_SURVEY_ENABLED=true did not override")
	}
	t.Setenv("E2A_ONBOARDING_SURVEY_ENABLED", "false")
	cfg = loadConfigFromYAML(t, minimalConfigYAML+"\nonboarding_survey:\n  enabled: true\n")
	if cfg.OnboardingSurvey.Enabled {
		t.Fatal("E2A_ONBOARDING_SURVEY_ENABLED=false did not override YAML true")
	}
}
```

The existing tests inline their temp-file setup, so add these two helpers at the bottom of the test file (the YAML is `TestLoadConfig`'s, which `Load` accepts):

```go
const minimalConfigYAML = `
smtp:
  listen_addr: ":3025"
  domain: "test.e2a.dev"
http:
  listen_addr: ":9090"
database:
  url: "postgres://test:test@localhost/test"
signing:
  hmac_secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
env: "production"
outbound_smtp:
  host: "smtp.example.com"
  port: 465
  from_domain: "mail.e2a.dev"
`

func loadConfigFromYAML(t *testing.T, yaml string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run TestOnboardingSurvey -count=1`
Expected: build FAIL, `cfg.OnboardingSurvey undefined`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

Add to `Config` after `OutboundFooter`:

```go
	OnboardingSurvey OnboardingSurveyConfig `yaml:"onboarding_survey"`
```

Add the type after `OutboundFooterConfig`:

```go
// OnboardingSurveyConfig gates the dashboard's one-question acquisition
// survey ("Where did you hear about e2a?"). Off by default: the columns
// from migration 120 exist everywhere, but with Enabled false the write
// path on PATCH /api/auth/me returns 404 and GET /api/auth/me reports
// onboarding_survey_pending=false, so the dashboard never shows the page.
// The answer set is code (internal/identity.AcquisitionSources), not config.
type OnboardingSurveyConfig struct {
	// Enabled turns the survey on. Override with E2A_ONBOARDING_SURVEY_ENABLED.
	Enabled bool `yaml:"enabled"`
}
```

Add the env override next to the `E2A_OUTBOUND_FOOTER_ENABLED` one:

```go
	if v := os.Getenv("E2A_ONBOARDING_SURVEY_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.OnboardingSurvey.Enabled = b
		}
	}
```

In `config.example.yaml`, after the `outbound_footer` block:

```yaml
# One-question onboarding survey ("Where did you hear about e2a?") shown
# once to each dashboard user before the rest of the app. Off by default;
# the answer is stored write-once on the user row (migration 120) and is
# only useful to operators who read their own database for analytics.
# Override with E2A_ONBOARDING_SURVEY_ENABLED.
# onboarding_survey:
#   enabled: false
```

- [ ] **Step 4: Run the config tests**

Run: `go test ./internal/config/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): onboarding_survey.enabled flag (default off)"
```

---

### Task 4: `/api/auth/me` contract

**Files:**
- Modify: `internal/auth/auth.go` (`UserAuth` struct ~line 46; `HandleMe` ~466; `HandleUpdateMe` ~490)
- Modify: `internal/auth/auth_test.go` (append; `setupUserAuth` ~18 and `authedJSON` ~157 are the helpers)
- Modify: `cmd/e2a/main.go` (~line 557, right after `auth.NewUserAuth`)

**Interfaces:**
- Consumes: `identity.User.AcquisitionAnsweredAt`, `identity.IsAcquisitionSource`, `identity.AcquisitionSourceSkipped`, `store.RecordAcquisitionSurvey`, `identity.ErrAcquisitionSurveyAnswered`, `config.Config.OnboardingSurvey.Enabled`.
- Produces:
  - `func (ua *UserAuth) SetOnboardingSurveyEnabled(enabled bool)`
  - `GET /api/auth/me` JSON gains `"onboarding_survey_pending": bool`
  - `PATCH /api/auth/me` accepts `{"onboarding_survey":{"source":"...","detail":"..."}}`; 409/404 JSON bodies per Global Constraints.

- [ ] **Step 1: Write the failing handler tests**

Append to `internal/auth/auth_test.go`:

```go
type meBody struct {
	identity.User
	OnboardingSurveyPending bool `json:"onboarding_survey_pending"`
}

// rawPool opens a plain connection to the same test database setupUserAuth
// used, for reading columns no store method exposes. Store has no pool
// accessor by design.
func rawPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("rawPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder) meBody {
	t.Helper()
	var got meBody
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return got
}

func TestHandleMe_SurveyPendingFollowsFlagAndAnswer(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	// Flag off (the default): never pending.
	w := httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); got.OnboardingSurveyPending {
		t.Fatal("pending=true with the flag off")
	}

	ua.SetOnboardingSurveyEnabled(true)
	w = httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	got := decodeMe(t, w)
	if !got.OnboardingSurveyPending {
		t.Fatal("pending=false for an unanswered user with the flag on")
	}

	if _, err := store.RecordAcquisitionSurvey(ctx, got.ID, "github", nil); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); got.OnboardingSurveyPending {
		t.Fatal("pending=true after answering")
	}
}

func TestHandleUpdateMe_SurveyHappyPathEveryValue(t *testing.T) {
	for _, source := range identity.AcquisitionSources {
		t.Run(source, func(t *testing.T) {
			ua, _, token := setupUserAuth(t)
			pool := rawPool(t)
			ua.SetOnboardingSurveyEnabled(true)
			body := `{"onboarding_survey":{"source":"` + source + `"}}`
			if source == "other" {
				body = `{"onboarding_survey":{"source":"other","detail":"  a newsletter  "}}`
			}
			w := httptest.NewRecorder()
			ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := decodeMe(t, w)
			if got.OnboardingSurveyPending {
				t.Error("pending still true in the PATCH response")
			}
			var stored, detail string
			if err := pool.QueryRow(context.Background(),
				`SELECT acquisition_source, COALESCE(acquisition_detail,'') FROM users WHERE id=$1`, got.ID).Scan(&stored, &detail); err != nil {
				t.Fatal(err)
			}
			if stored != source {
				t.Errorf("stored source = %q, want %q", stored, source)
			}
			if source == "other" && detail != "a newsletter" {
				t.Errorf("detail = %q, want trimmed 'a newsletter'", detail)
			}
		})
	}
}

func TestHandleUpdateMe_SurveyValidation(t *testing.T) {
	ua, _, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	long := strings.Repeat("é", 201) // 201 code points, 402 bytes
	cases := []struct {
		name string
		body string
	}{
		{"unknown source", `{"onboarding_survey":{"source":"carrier_pigeon"}}`},
		{"empty source", `{"onboarding_survey":{"source":""}}`},
		{"missing source", `{"onboarding_survey":{"detail":"x"}}`},
		{"detail too long", `{"onboarding_survey":{"source":"other","detail":"` + long + `"}}`},
		{"detail with skipped", `{"onboarding_survey":{"source":"skipped","detail":"why"}}`},
		{"bad name blocks whole request", `{"name":"","onboarding_survey":{"source":"github"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, tc.body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
	// Nothing was written by any rejected request.
	w := httptest.NewRecorder()
	ua.HandleMe(w, authedRequest("GET", "/api/auth/me", token))
	if got := decodeMe(t, w); !got.OnboardingSurveyPending {
		t.Fatal("a rejected request recorded an answer")
	}
	// 200 code points of multibyte text is allowed.
	ok := strings.Repeat("é", 200)
	w = httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"other","detail":"`+ok+`"}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("200-char detail: status = %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateMe_SurveyWriteOnceReturns409(t *testing.T) {
	ua, _, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	first := httptest.NewRecorder()
	ua.HandleUpdateMe(first, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"github"}}`))
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	ua.HandleUpdateMe(second, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"search"}}`))
	if second.Code != http.StatusConflict {
		t.Fatalf("second: status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), `"onboarding_survey_already_answered"`) {
		t.Errorf("body = %s", second.Body.String())
	}
}

func TestHandleUpdateMe_SurveyDisabledReturns404(t *testing.T) {
	ua, _, token := setupUserAuth(t) // flag stays off
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"onboarding_survey":{"source":"github"}}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"onboarding_survey_disabled"`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleUpdateMe_NameAndSurveyTogether(t *testing.T) {
	ua, _, token := setupUserAuth(t)
	ua.SetOnboardingSurveyEnabled(true)
	w := httptest.NewRecorder()
	ua.HandleUpdateMe(w, authedJSON("PATCH", "/api/auth/me", token, `{"name":"Jamie","onboarding_survey":{"source":"word_of_mouth"}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeMe(t, w)
	if got.Name != "Jamie" || got.OnboardingSurveyPending {
		t.Errorf("got name=%q pending=%v, want Jamie/false", got.Name, got.OnboardingSurveyPending)
	}
}
```

Add `"github.com/jackc/pgx/v5/pgxpool"` and `"github.com/tokencanopy/e2a/internal/testutil"` to the test file's imports if they are not already there.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/auth/ -run 'Survey' -count=1`
Expected: build FAIL, `ua.SetOnboardingSurveyEnabled undefined`.

- [ ] **Step 3: Implement in `internal/auth/auth.go`**

Add the field to `UserAuth`:

```go
	// onboardingSurveyEnabled mirrors config.OnboardingSurvey.Enabled. When
	// false, /api/auth/me never reports the survey as pending and the
	// survey branch of PATCH returns 404.
	onboardingSurveyEnabled bool
```

Add after `NewUserAuth`:

```go
// SetOnboardingSurveyEnabled turns the onboarding survey on or off for
// this handler set. Called once from main after construction.
func (ua *UserAuth) SetOnboardingSurveyEnabled(enabled bool) {
	ua.onboardingSurveyEnabled = enabled
}

// meResponse is the /api/auth/me shape: the user record plus the one
// derived field the dashboard's app shell gates on.
type meResponse struct {
	*identity.User
	OnboardingSurveyPending bool `json:"onboarding_survey_pending"`
}

func (ua *UserAuth) writeMe(w http.ResponseWriter, u *identity.User) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, meResponse{
		User:                    u,
		OnboardingSurveyPending: ua.onboardingSurveyEnabled && u.AcquisitionAnsweredAt == nil,
	})
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": code})
}
```

`writeJSON` (auth.go:32) only encodes; it sets no status and no header, so the order above (header, status, encode) is correct.

Change `HandleMe`'s last two lines to `ua.writeMe(w, user)`.

Replace `HandleUpdateMe` from the request struct down:

```go
	var req struct {
		Name             *string `json:"name"`
		OnboardingSurvey *struct {
			Source string  `json:"source"`
			Detail *string `json:"detail"`
		} `json:"onboarding_survey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Name == nil && req.OnboardingSurvey == nil {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}

	// Validate everything before writing anything, so a bad field in one
	// half never leaves the other half applied.
	var name string
	if req.Name != nil {
		name = *req.Name
		if name != strings.TrimSpace(name) {
			http.Error(w, "name must not have leading or trailing whitespace", http.StatusBadRequest)
			return
		}
		if len(name) < minDisplayNameLen || len(name) > maxDisplayNameLen {
			http.Error(w, "name must be 1–80 characters", http.StatusBadRequest)
			return
		}
	}

	var surveyDetail *string
	if req.OnboardingSurvey != nil {
		if !ua.onboardingSurveyEnabled {
			writeJSONError(w, http.StatusNotFound, "onboarding_survey_disabled")
			return
		}
		if !identity.IsAcquisitionSource(req.OnboardingSurvey.Source) {
			http.Error(w, "onboarding_survey.source is not a known value", http.StatusBadRequest)
			return
		}
		if req.OnboardingSurvey.Detail != nil {
			d := strings.TrimSpace(*req.OnboardingSurvey.Detail)
			if d != "" {
				if req.OnboardingSurvey.Source == identity.AcquisitionSourceSkipped {
					http.Error(w, "onboarding_survey.detail is not allowed with source \"skipped\"", http.StatusBadRequest)
					return
				}
				if utf8.RuneCountInString(d) > maxAcquisitionDetailLen {
					http.Error(w, "onboarding_survey.detail must be at most 200 characters", http.StatusBadRequest)
					return
				}
				surveyDetail = &d
			}
		}
	}

	updated := user
	if req.Name != nil {
		u, err := ua.store.UpdateUserName(r.Context(), user.ID, name)
		if err != nil {
			http.Error(w, "failed to update profile", http.StatusInternalServerError)
			return
		}
		updated = u
	}
	if req.OnboardingSurvey != nil {
		u, err := ua.store.RecordAcquisitionSurvey(r.Context(), user.ID, req.OnboardingSurvey.Source, surveyDetail)
		if errors.Is(err, identity.ErrAcquisitionSurveyAnswered) {
			writeJSONError(w, http.StatusConflict, "onboarding_survey_already_answered")
			return
		}
		if err != nil {
			http.Error(w, "failed to record survey", http.StatusInternalServerError)
			return
		}
		updated = u
	}

	ua.writeMe(w, updated)
}
```

Add `const maxAcquisitionDetailLen = 200` next to the display-name constants. `errors` and `unicode/utf8` are already imported in auth.go.

- [ ] **Step 4: Wire the flag in `cmd/e2a/main.go`**

Directly after `userAuth := auth.NewUserAuth(&cfg.OAuth, store, cfg.IsProduction())`:

```go
	userAuth.SetOnboardingSurveyEnabled(cfg.OnboardingSurvey.Enabled)
```

- [ ] **Step 5: Run the auth package and build**

Run: `go build ./... && go test ./internal/auth/ -count=1`
Expected: PASS. `TestHandleMe_ReturnsCurrentUser` and the existing `TestHandleUpdateMe_*` still pass unchanged (they decode into `identity.User`, which ignores the extra field).

- [ ] **Step 6: Commit**

```bash
git add internal/auth/auth.go internal/auth/auth_test.go cmd/e2a/main.go
git commit -m "feat(auth): onboarding survey on /api/auth/me (pending flag, write-once PATCH)"
```

---

### Task 5: Dashboard types and option list

**Files:**
- Create: `web/src/lib/acquisitionSources.ts`, `web/src/lib/acquisitionSources.test.ts`
- Modify: `web/src/app/components/types.ts` (`UserInfo` ~line 6; `UpdateMeRequest` ~line 309)

**Interfaces:**
- Produces:
  - `ACQUISITION_SOURCES: ReadonlyArray<{ value: AcquisitionSource; label: string }>` (nine visible options; `skipped` is not listed)
  - `type AcquisitionSource = "search" | ... | "skipped"`
  - `ACQUISITION_DETAIL_MAX = 200`
  - `UserInfo.onboarding_survey_pending?: boolean`
  - `UpdateMeRequest = { name?: string; onboarding_survey?: { source: AcquisitionSource; detail?: string } }`

- [ ] **Step 1: Write the failing test**

`web/src/lib/acquisitionSources.test.ts`:

```ts
import { ACQUISITION_SOURCES, ACQUISITION_DETAIL_MAX } from "./acquisitionSources";

describe("acquisitionSources", () => {
  it("lists the nine visible options in server enum order, without skipped", () => {
    expect(ACQUISITION_SOURCES.map((o) => o.value)).toEqual([
      "search",
      "ai_assistant",
      "github",
      "x_twitter",
      "hn_reddit",
      "content",
      "mcp_directory",
      "word_of_mouth",
      "other",
    ]);
  });

  it("uses the agreed labels", () => {
    expect(ACQUISITION_SOURCES.map((o) => o.label)).toEqual([
      "Search engine",
      "ChatGPT / Claude / another AI assistant",
      "GitHub",
      "X / Twitter",
      "Hacker News / Reddit",
      "YouTube, podcast, or blog",
      "MCP directory",
      "Friend or colleague",
      "Other",
    ]);
  });

  it("caps detail at 200 characters", () => {
    expect(ACQUISITION_DETAIL_MAX).toBe(200);
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run (in `web/`): `npx jest src/lib/acquisitionSources.test.ts`
Expected: FAIL, cannot find module `./acquisitionSources`.

- [ ] **Step 3: Implement**

`web/src/lib/acquisitionSources.ts`:

```ts
// Answer set for the onboarding survey ("Where did you hear about e2a?").
// Values mirror internal/identity.AcquisitionSources and the CHECK in
// migration 120 exactly; labels are display-only and never stored.
// "skipped" is a valid value the page sends from the Skip action but is
// never offered as a choice.
export type AcquisitionSource =
  | "search"
  | "ai_assistant"
  | "github"
  | "x_twitter"
  | "hn_reddit"
  | "content"
  | "mcp_directory"
  | "word_of_mouth"
  | "other"
  | "skipped";

export const ACQUISITION_SOURCES: ReadonlyArray<{ value: AcquisitionSource; label: string }> = [
  { value: "search", label: "Search engine" },
  { value: "ai_assistant", label: "ChatGPT / Claude / another AI assistant" },
  { value: "github", label: "GitHub" },
  { value: "x_twitter", label: "X / Twitter" },
  { value: "hn_reddit", label: "Hacker News / Reddit" },
  { value: "content", label: "YouTube, podcast, or blog" },
  { value: "mcp_directory", label: "MCP directory" },
  { value: "word_of_mouth", label: "Friend or colleague" },
  { value: "other", label: "Other" },
];

// Server-enforced ceiling for the free-text detail (code points, trimmed).
export const ACQUISITION_DETAIL_MAX = 200;
```

In `web/src/app/components/types.ts`:

```ts
export type UserInfo = {
  id: string;
  email: string;
  name: string;
  created_at: string;
  // True when the server's onboarding survey is enabled and this user has
  // not answered or skipped it yet. Optional so older fixtures type-check;
  // treat a missing value as false.
  onboarding_survey_pending?: boolean;
};
```

and

```ts
// Request body for PATCH /api/auth/me. `name` edits the display name;
// `onboarding_survey` records the write-once acquisition answer (409 if
// already answered, 404 when the server has the survey disabled).
export type UpdateMeRequest = {
  name?: string;
  onboarding_survey?: {
    source: AcquisitionSource;
    detail?: string;
  };
};
```

with `import type { AcquisitionSource } from "../../lib/acquisitionSources";` at the top of `types.ts`.

- [ ] **Step 4: Run the test and the type check**

Run (in `web/`): `npx jest src/lib/acquisitionSources.test.ts && npx tsc --noEmit`
Expected: PASS; tsc clean (the settings page's `{ name: draft }` body still satisfies the widened type).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/acquisitionSources.ts web/src/lib/acquisitionSources.test.ts web/src/app/components/types.ts
git commit -m "feat(web): acquisition survey option list and /me types"
```

---

### Task 6: App-shell gate

**Files:**
- Modify: `web/src/app/(app)/AppLayoutClient.tsx` (hooks at the top of `AppLayout` ~line 21; loading/no-user branches ~83-118)
- Modify: `web/src/app/(app)/layout.test.tsx` (mocks ~line 12-40, `signedIn` fixture ~44; append tests)

**Interfaces:**
- Consumes: `UserInfo.onboarding_survey_pending`.
- Produces: pending users are redirected to `/welcome`; `/welcome` renders `children` without sidebar/mobile header while pending, and redirects to `/inboxes` once not pending.

- [ ] **Step 1: Add the navigation mock and failing tests**

In `web/src/app/(app)/layout.test.tsx`, after the `next/link` mock add:

```tsx
const mockReplace = jest.fn();
let mockPathname = "/inboxes";
jest.mock("next/navigation", () => ({
  usePathname: () => mockPathname,
  useRouter: () => ({ replace: mockReplace, push: jest.fn(), back: jest.fn() }),
}));
```

Widen the `mockAuth` type's `user` to include `onboarding_survey_pending?: boolean`, and in `beforeEach` add `mockReplace.mockReset(); mockPathname = "/inboxes";`.

Append:

```tsx
describe("(app) layout — onboarding survey gate", () => {
  const pendingUser = {
    user: { ...signedIn.user, onboarding_survey_pending: true },
    loading: false,
  };

  it("redirects a pending user away from any app route to /welcome and hides the chrome", () => {
    mockAuth = pendingUser;
    mockPathname = "/api-keys";
    render(<AppLayout><p>page body</p></AppLayout>);
    expect(mockReplace).toHaveBeenCalledWith("/welcome");
    expect(screen.queryByText("page body")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open menu" })).not.toBeInTheDocument();
  });

  it("renders /welcome without the sidebar or mobile header while pending", () => {
    mockAuth = pendingUser;
    mockPathname = "/welcome";
    render(<AppLayout><p>survey body</p></AppLayout>);
    expect(mockReplace).not.toHaveBeenCalled();
    expect(screen.getByText("survey body")).toBeInTheDocument();
    expect(screen.queryByText("Inboxes")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open menu" })).not.toBeInTheDocument();
  });

  it("bounces a non-pending user off /welcome to /inboxes", () => {
    mockAuth = signedIn;
    mockPathname = "/welcome";
    render(<AppLayout><p>survey body</p></AppLayout>);
    expect(mockReplace).toHaveBeenCalledWith("/inboxes");
    expect(screen.queryByText("survey body")).not.toBeInTheDocument();
  });

  it("leaves a non-pending user on a normal route alone", () => {
    mockAuth = signedIn;
    mockPathname = "/inboxes";
    render(<AppLayout><p>page body</p></AppLayout>);
    expect(mockReplace).not.toHaveBeenCalled();
    expect(screen.getByText("page body")).toBeInTheDocument();
  });

  it("does not redirect while auth is still loading or signed out", () => {
    mockAuth = { user: null, loading: true };
    mockPathname = "/inboxes";
    const { unmount } = render(<AppLayout><p>page body</p></AppLayout>);
    expect(mockReplace).not.toHaveBeenCalled();
    unmount();
    mockAuth = { user: null, loading: false };
    render(<AppLayout><p>page body</p></AppLayout>);
    expect(mockReplace).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Run to verify the new tests fail**

Run (in `web/`): `npx jest "src/app/\(app\)/layout.test.tsx"`
Expected: the five new tests FAIL (no redirect, chrome present); the pre-existing tests still PASS with the mock in place.

- [ ] **Step 3: Implement the gate in `AppLayoutClient.tsx`**

Add imports:

```tsx
import { usePathname, useRouter } from "next/navigation";
```

Inside `AppLayout`, right after `const { user, loading } = useAuth();`:

```tsx
  const pathname = usePathname();
  const router = useRouter();
  // Onboarding survey gate. The server decides "pending" (flag on AND
  // unanswered); this shell only routes on it. The two redirects are
  // mutually exclusive on the pending bit, so no state satisfies both
  // and there is no loop: pending → must be on /welcome; not pending →
  // must not be.
  const surveyPending = Boolean(user?.onboarding_survey_pending);
  const onWelcome = pathname === "/welcome";
  const surveyRedirecting = Boolean(user) && !loading && surveyPending !== onWelcome;
  useEffect(() => {
    if (!surveyRedirecting) return;
    router.replace(surveyPending ? "/welcome" : "/inboxes");
  }, [surveyRedirecting, surveyPending, router]);
```

Extract the existing loading JSX into a constant so it can be reused, then add the two new branches after the `if (!user)` block:

```tsx
  const loadingScreen = (
    <div
      className="min-h-screen flex items-center justify-center"
      style={{ background: "var(--bg)", color: "var(--fg)" }}
    >
      <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
        Loading...
      </p>
    </div>
  );

  if (loading) {
    return loadingScreen;
  }

  if (!user) {
    /* unchanged sign-in branch */
  }

  if (surveyRedirecting) {
    return loadingScreen;
  }

  if (onWelcome) {
    // Survey pending and already on /welcome: render it alone. No
    // sidebar, no mobile header — every link would just bounce back.
    return (
      <div className="min-h-screen" style={{ background: "var(--bg)" }} data-app-surface="">
        {children}
      </div>
    );
  }
```

The `useEffect` must sit with the other hooks above the early returns (React hook order).

- [ ] **Step 4: Run the layout tests, the other shell tests, and the type check**

Run (in `web/`): `npx jest "src/app/\(app\)/layout" "src/app/\(app\)/responsive" && npx tsc --noEmit && npm run lint`
Expected: all PASS, lint clean. If `layout.pendingPolling.test.tsx` renders the shell and now throws about `next/navigation`, add the same three-line mock to it.

- [ ] **Step 5: Commit**

```bash
git add "web/src/app/(app)/AppLayoutClient.tsx" "web/src/app/(app)/layout.test.tsx"
git commit -m "feat(web): gate the app shell on the onboarding survey"
```

---

### Task 7: The `/welcome` page

**Files:**
- Create: `web/src/app/(app)/welcome/page.tsx`, `web/src/app/(app)/welcome/page.test.tsx`

**Interfaces:**
- Consumes: `ACQUISITION_SOURCES`, `ACQUISITION_DETAIL_MAX`, `AcquisitionSource`, `UpdateMeRequest`, `useAuth().setUser`, `useRouter().replace`.
- Produces: the page at `/welcome`.

- [ ] **Step 1: Write the failing page tests**

`web/src/app/(app)/welcome/page.test.tsx`:

```tsx
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import WelcomePage from "./page";

const mockReplace = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ replace: mockReplace, push: jest.fn(), back: jest.fn() }),
  usePathname: () => "/welcome",
}));

const mockSetUser = jest.fn();
const baseUser = {
  id: "usr_1",
  email: "alice@example.test",
  name: "Alice",
  created_at: "2026-01-01T00:00:00Z",
  onboarding_survey_pending: true,
};
jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => ({ user: baseUser, loading: false, setUser: mockSetUser, signOut: jest.fn() }),
}));

const fetchMock = jest.fn();

beforeEach(() => {
  mockReplace.mockReset();
  mockSetUser.mockReset();
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

function okResponse(body: unknown) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
}
function errResponse(status: number) {
  return { ok: false, status, json: async () => ({}), text: async () => "nope" };
}

function lastPatchBody() {
  const [, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1];
  return JSON.parse((init as RequestInit).body as string);
}

describe("/welcome", () => {
  it("renders the question, all nine options, and a disabled Continue", () => {
    render(<WelcomePage />);
    expect(screen.getByRole("heading", { name: "Where did you hear about e2a?" })).toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(9);
    expect(screen.getByRole("radio", { name: "Friend or colleague" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.queryByPlaceholderText("Tell us more (optional)")).not.toBeInTheDocument();
  });

  it("submits the chosen source, pushes the response into auth, and goes to /inboxes", async () => {
    const updated = { ...baseUser, onboarding_survey_pending: false };
    fetchMock.mockResolvedValue(okResponse(updated));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "GitHub" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/auth/me");
    expect((init as RequestInit).method).toBe("PATCH");
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "github" } });
    expect(mockSetUser).toHaveBeenCalledWith(updated);
  });

  it("reveals the detail field for Other, enforces the limit, and sends it", async () => {
    fetchMock.mockResolvedValue(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "Other" }));
    const detail = screen.getByPlaceholderText("Tell us more (optional)");
    expect(detail).toHaveAttribute("maxLength", "200");
    await userEvent.type(detail, "a newsletter");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "other", detail: "a newsletter" } });
  });

  it("Skip records skipped and leaves", async () => {
    fetchMock.mockResolvedValue(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(lastPatchBody()).toEqual({ onboarding_survey: { source: "skipped" } });
  });

  it("treats 409 as done", async () => {
    fetchMock.mockResolvedValue(errResponse(409));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "Search engine" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(mockSetUser).toHaveBeenCalledWith({ ...baseUser, onboarding_survey_pending: false });
  });

  it("shows an error on a 500 and keeps the form and Skip usable", async () => {
    fetchMock.mockResolvedValueOnce(errResponse(500));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("radio", { name: "MCP directory" }));
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/try again or skip/i);
    expect(mockReplace).not.toHaveBeenCalled();
    expect(screen.getByRole("radio", { name: "MCP directory" })).toBeChecked();
    fetchMock.mockResolvedValueOnce(okResponse({ ...baseUser, onboarding_survey_pending: false }));
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
  });

  it("Skip still leaves when the network is down", async () => {
    fetchMock.mockRejectedValue(new Error("offline"));
    render(<WelcomePage />);
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith("/inboxes"));
    expect(mockSetUser).toHaveBeenCalledWith({ ...baseUser, onboarding_survey_pending: false });
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run (in `web/`): `npx jest "src/app/\(app\)/welcome"`
Expected: FAIL, cannot find module `./page`.

- [ ] **Step 3: Implement the page**

`web/src/app/(app)/welcome/page.tsx`:

```tsx
"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "../../components/AuthProvider";
import type { UpdateMeRequest, UserInfo } from "../../components/types";
import {
  ACQUISITION_DETAIL_MAX,
  ACQUISITION_SOURCES,
  type AcquisitionSource,
} from "../../../lib/acquisitionSources";

// One-question onboarding survey. The app shell routes a user here while
// the server reports onboarding_survey_pending and renders this page
// without the sidebar; answering (or skipping) flips the flag through
// PATCH /api/auth/me and the shell lets the user through.
//
// Test selectors (heading text, option labels, button names, placeholder)
// are stable — page.test.tsx depends on them.

type SurveyBody = NonNullable<UpdateMeRequest["onboarding_survey"]>;

export default function WelcomePage() {
  const router = useRouter();
  const { user, setUser } = useAuth();
  const [source, setSource] = useState<AcquisitionSource | null>(null);
  const [detail, setDetail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const leave = (updated: UserInfo | null) => {
    if (updated) {
      setUser(updated);
    } else if (user) {
      setUser({ ...user, onboarding_survey_pending: false });
    }
    router.replace("/inboxes");
  };

  const send = async (body: SurveyBody): Promise<"ok" | "done" | "failed"> => {
    const res = await fetch("/api/auth/me", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ onboarding_survey: body }),
    });
    if (res.ok) {
      leave((await res.json()) as UserInfo);
      return "ok";
    }
    if (res.status === 409) {
      // Answered elsewhere (another tab). Nothing to redo.
      leave(null);
      return "done";
    }
    return "failed";
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!source || busy) return;
    setBusy(true);
    setError("");
    const trimmed = detail.trim();
    const body: SurveyBody =
      source === "other" && trimmed ? { source, detail: trimmed } : { source };
    try {
      if ((await send(body)) === "failed") {
        setError("Something went wrong. You can try again or skip for now.");
        setBusy(false);
      }
    } catch {
      setError("Something went wrong. You can try again or skip for now.");
      setBusy(false);
    }
  };

  const handleSkip = async () => {
    if (busy) return;
    setBusy(true);
    // Skip must never trap the user: whatever the server says (or if it
    // cannot be reached), leave. An unrecorded skip is asked again next
    // login, which is the right failure mode.
    try {
      if ((await send({ source: "skipped" })) === "failed") leave(null);
    } catch {
      leave(null);
    }
  };

  return (
    <main className="min-h-screen flex items-center justify-center px-6 py-12">
      <form
        onSubmit={handleSubmit}
        className="w-full"
        style={{ maxWidth: 560 }}
        aria-busy={busy}
      >
        <p
          className="text-[11px] font-medium uppercase tracking-wider mb-3"
          style={{ color: "var(--fg-muted)" }}
        >
          Welcome to e2a
        </p>
        <h1
          className="mb-2"
          style={{
            fontFamily: "var(--f-ui)",
            fontSize: 24,
            fontWeight: 600,
            color: "var(--fg)",
            letterSpacing: "-0.01em",
          }}
        >
          Where did you hear about e2a?
        </h1>
        <p className="text-[13px] mb-6" style={{ color: "var(--fg-muted)" }}>
          One question, then you&apos;re in. It helps us know where to show up.
        </p>

        <fieldset className="space-y-2 mb-5" disabled={busy}>
          <legend className="sr-only">Where did you hear about e2a?</legend>
          {ACQUISITION_SOURCES.map((opt) => {
            const active = source === opt.value;
            return (
              <label
                key={opt.value}
                className="flex items-center gap-3 px-4 py-3 cursor-pointer text-[13px] transition"
                style={{
                  background: active ? "var(--accent-soft)" : "var(--bg-panel)",
                  color: active ? "var(--accent-strong)" : "var(--fg)",
                  border: active ? "1px solid var(--accent-strong)" : "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                <input
                  type="radio"
                  name="acquisition_source"
                  value={opt.value}
                  checked={active}
                  onChange={() => setSource(opt.value)}
                  className="accent-current"
                />
                <span className="font-medium">{opt.label}</span>
              </label>
            );
          })}
        </fieldset>

        {source === "other" && (
          <div className="mb-5">
            <input
              type="text"
              value={detail}
              onChange={(e) => setDetail(e.target.value)}
              maxLength={ACQUISITION_DETAIL_MAX}
              placeholder="Tell us more (optional)"
              aria-label="Tell us more (optional)"
              disabled={busy}
              className="w-full px-3 py-2 text-[13px]"
              style={{
                background: "var(--bg-panel)",
                color: "var(--fg)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            />
          </div>
        )}

        {error && (
          <p role="alert" className="text-[13px] mb-4" style={{ color: "var(--danger)" }}>
            {error}
          </p>
        )}

        <div className="flex items-center justify-between gap-4">
          <button
            type="button"
            onClick={handleSkip}
            disabled={busy}
            className="text-[13px] underline"
            style={{ color: "var(--fg-muted)" }}
          >
            Skip
          </button>
          <button
            type="submit"
            disabled={!source || busy}
            className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
            style={{
              background: "var(--accent-fill)",
              color: "var(--accent-fg)",
              borderRadius: "var(--r-md)",
            }}
          >
            Continue
          </button>
        </div>
      </form>
    </main>
  );
}
```

`var(--danger)` is the token the settings page uses for its error text.

- [ ] **Step 4: Run the page tests, lint, types**

Run (in `web/`): `npx jest "src/app/\(app\)/welcome" && npm run lint && npx tsc --noEmit`
Expected: all seven tests PASS; lint and tsc clean.

- [ ] **Step 5: Commit**

```bash
git add "web/src/app/(app)/welcome/page.tsx" "web/src/app/(app)/welcome/page.test.tsx"
git commit -m "feat(web): /welcome onboarding survey page"
```

---

### Task 8: Full verification and PR

**Files:** none new.

- [ ] **Step 1: Full Go suite for the touched packages plus a build**

Run: `go build ./... && go vet ./internal/auth/ ./internal/identity/ ./internal/config/ && go test ./internal/auth/ ./internal/identity/ ./internal/config/ ./internal/agent/ -count=1`
Expected: PASS. `internal/agent` is included because it registers the `/api/auth/me` routes and has broad handler tests that exercise user loading.

- [ ] **Step 2: Full web suite and production build**

Run (in `web/`): `npx jest && npm run lint && npx tsc --noEmit && npm run build`
Expected: PASS, no new warnings.

- [ ] **Step 3: OpenAPI golden**

Run: `go test ./internal/httpapi/ -run TestSpecGoldenNoDrift -count=1`
Expected: PASS unchanged (the `/api/auth/*` routes are not in the spec; if this fails, the change touched a `/v1` surface it should not have).

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin feat/onboarding-survey
gh pr create -R tokencanopy/e2a --base main --head feat/onboarding-survey \
  --title "feat: onboarding acquisition survey (/welcome, users.acquisition_*, onboarding_survey flag)" \
  --body-file docs/superpowers/plans/2026-09-03-onboarding-survey-pr-body.md
```

PR body (write the file first; delete it after `gh pr create`, it is not committed):

```markdown
One-question "Where did you hear about e2a?" survey shown once to each dashboard user, stored write-once on `users`, off by default.

- migration 120: `users.acquisition_source / acquisition_detail / acquisition_answered_at` + CHECKs
- config: `onboarding_survey.enabled` (default false; env `E2A_ONBOARDING_SURVEY_ENABLED`)
- `GET /api/auth/me` → `onboarding_survey_pending` (flag on AND unanswered)
- `PATCH /api/auth/me` → `onboarding_survey: {source, detail?}`; 409 on re-submit, 404 when disabled
- web: app-shell gate → `/welcome` (no sidebar), Skip always leaves
- self-host: zero behaviour change unless the flag is set

Not a `/v1` change; no SDK/CLI/MCP surface. Operators enable it in their deployment config.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01WPerbtyZtci8gFA8kem8Cm
```

- [ ] **Step 5: Report**

Stop at the PR. Do not merge; do not watch CI beyond reading `gh pr checks -R tokencanopy/e2a <number>` once. Report the PR URL, the test commands run and their results, and anything skipped.
