package identity_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestRecordOIDCOwnerEmailProof(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	newUser := func(email, sub string) string {
		u, err := store.CreateOrGetUser(ctx, email, "Owner", sub)
		if err != nil {
			t.Fatalf("CreateOrGetUser: %v", err)
		}
		return u.ID
	}
	read := func(id string) (addr, src *string) {
		if err := pool.QueryRow(ctx, `SELECT owner_email_verified_address, owner_email_verified_source FROM users WHERE id = $1`, id).Scan(&addr, &src); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("records normalized proof with oidc source", func(t *testing.T) {
		id := newUser("oidc-proof-ok@example.com", "sub-oidc-proof-ok")
		ok, err := store.RecordOIDCOwnerEmailProof(ctx, id, "  OIDC-Proof-OK@Example.com ")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		addr, src := read(id)
		if addr == nil || *addr != "oidc-proof-ok@example.com" || src == nil || *src != "oidc" {
			t.Fatalf("proof = %v %v", addr, src)
		}
	})
	t.Run("deleted account", func(t *testing.T) {
		id := newUser("oidc-proof-del@example.com", "sub-oidc-proof-del")
		if _, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		ok, err := store.RecordOIDCOwnerEmailProof(ctx, id, "oidc-proof-del@example.com")
		if err != nil || ok {
			t.Fatalf("ok=%v err=%v, want false,nil", ok, err)
		}
		if addr, _ := read(id); addr != nil {
			t.Fatalf("proof recorded on deleted account: %v", *addr)
		}
	})
	t.Run("email mismatch", func(t *testing.T) {
		id := newUser("oidc-proof-mm@example.com", "sub-oidc-proof-mm")
		ok, err := store.RecordOIDCOwnerEmailProof(ctx, id, "someone-else@example.com")
		if err != nil || ok {
			t.Fatalf("ok=%v err=%v, want false,nil", ok, err)
		}
		if addr, _ := read(id); addr != nil {
			t.Fatalf("proof recorded on mismatch: %v", *addr)
		}
	})
	t.Run("empty inputs", func(t *testing.T) {
		for _, c := range [][2]string{{"", "a@example.com"}, {"usr_x", "  "}} {
			ok, err := store.RecordOIDCOwnerEmailProof(ctx, c[0], c[1])
			if err != nil || ok {
				t.Fatalf("%v: ok=%v err=%v", c, ok, err)
			}
		}
	})
	t.Run("constraint still rejects other sources", func(t *testing.T) {
		id := newUser("oidc-proof-bad@example.com", "sub-oidc-proof-bad")
		_, err := pool.Exec(ctx, `UPDATE users SET owner_email_verified_at = now(), owner_email_verified_address = lower(email), owner_email_verified_source = 'bogus' WHERE id = $1`, id)
		if err == nil {
			t.Fatal("expected check violation for unknown source")
		}
	})
}
