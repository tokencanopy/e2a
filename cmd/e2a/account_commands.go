package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
)

// accountCommandFlags selects one account-deletion operator command. These
// read or adjust what outlives an account purge — the deleted-account
// summary and the identity tombstones — and are deliberately operator-only:
// no HTTP, SDK or MCP route exposes them.
type accountCommandFlags struct {
	inspectDeleted       bool
	inspectTombstoneKeys bool
	extendTombstones     bool
	revokeTombstones     bool
	deletedAccountID     string
	holdDays             int
}

func (f *accountCommandFlags) selected() int {
	n := 0
	for _, b := range []bool{f.inspectDeleted, f.inspectTombstoneKeys, f.extendTombstones, f.revokeTombstones} {
		if b {
			n++
		}
	}
	return n
}

func (f *accountCommandFlags) commandRequested() bool { return f.selected() > 0 }

func runAccountCommand(ctx context.Context, store *identity.Store, f *accountCommandFlags, reason string, stdout io.Writer) error {
	if f.selected() != 1 {
		return errors.New("exactly one account command may be given per invocation")
	}
	if f.inspectTombstoneKeys {
		st, err := store.InspectTombstoneKeys(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, st)
	}
	id := strings.TrimSpace(f.deletedAccountID)
	if id == "" {
		return errors.New("this command requires -deleted-account-id")
	}
	switch {
	case f.inspectDeleted:
		sum, err := store.GetDeletedAccountSummary(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no deleted-account summary is retained for %s (never purged with tombstones enabled, or past its hold)", id)
		}
		if err != nil {
			return err
		}
		return printJSON(stdout, sum)
	case f.extendTombstones:
		if f.holdDays <= 0 {
			return errors.New("-extend-identity-tombstones requires a positive -tombstone-hold-days")
		}
		n, err := store.ExtendAccountTombstones(ctx, id, time.Now().Add(time.Duration(f.holdDays)*24*time.Hour))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "tombstones_extended: %d\n", n)
		return nil
	case f.revokeTombstones:
		if strings.TrimSpace(reason) == "" {
			return errors.New("-revoke-identity-tombstones requires a nonblank -reason")
		}
		n, err := store.RevokeAccountTombstones(ctx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "tombstones_revoked: %d\nactor: %s\nreason: %s\n", n, cliActor(), reason)
		return nil
	}
	return errors.New("no account command selected")
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
