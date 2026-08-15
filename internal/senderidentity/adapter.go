package senderidentity

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tokencanopy/e2a/internal/domainteardown"
)

// RawStore is the primitive persistence surface implemented by
// *identity.Store. It deliberately speaks plain strings / JSON bytes so the
// core identity package does NOT import senderidentity (and thus does not pull
// River + the AWS SDK into its dependency graph). NewStoreAdapter wraps it
// into the typed Store the workers consume.
type RawStore interface {
	WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) error
	LoadSendingIdentityState(ctx context.Context, domain string) (incarnation, owner string, verified bool, status, selector string, privateKeyDER []byte, appliedIncarnation string, ledgerUpdatedAt time.Time, err error)
	SetSendingStatusForIncarnation(ctx context.Context, domain, incarnation, status, dkimStatus, mailFromStatus, errMsg string, recordsJSON []byte) error
	TouchSendingCheckedForIncarnation(ctx context.Context, domain, incarnation string) error
	MarkSendingIdentityManaged(ctx context.Context, domain, incarnation string) error
	MarkSendingIdentityApplied(ctx context.Context, domain, incarnation string) error
	SendingIdentityLedgerExpired(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error)
	ObserveSendingIdentityProviderPending(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error)
	ClearSendingIdentityProviderPending(ctx context.Context, domain, incarnation string) error
	ForgetSendingIdentityManaged(ctx context.Context, domain string) error
	FinalizeSendingIdentityTombstone(ctx context.Context, domain string, olderThan time.Duration) error
	SetDomainTeardownState(ctx context.Context, domain string, state domainteardown.State) error
	ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error)
	DomainExists(ctx context.Context, domain string) (bool, error)
}

// NewStoreAdapter bridges a RawStore (e.g. *identity.Store) to the typed
// Store the workers use, converting Status ↔ string and DNSRecord ↔ JSON.
func NewStoreAdapter(raw RawStore) Store { return &storeAdapter{raw: raw} }

type storeAdapter struct{ raw RawStore }

func (a *storeAdapter) WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) error {
	return a.raw.WithSendingIdentityMutationLock(ctx, domain, fn)
}

func (a *storeAdapter) LoadSendingIdentityState(ctx context.Context, domain string) (SendingIdentityState, error) {
	incarnation, owner, verified, status, selector, key, applied, ledgerAt, err := a.raw.LoadSendingIdentityState(ctx, domain)
	return SendingIdentityState{
		Incarnation:        incarnation,
		Owner:              owner,
		Verified:           verified,
		Status:             Status(status),
		Selector:           selector,
		PrivateKey:         key,
		AppliedIncarnation: applied,
		LedgerUpdatedAt:    ledgerAt,
	}, err
}

func (a *storeAdapter) SetSendingStatus(ctx context.Context, domain, incarnation string, status, dkimStatus, mailFromStatus Status, errMsg string, records []DNSRecord) error {
	var recordsJSON []byte
	if len(records) > 0 {
		b, err := json.Marshal(records)
		if err != nil {
			return err
		}
		recordsJSON = b
	}
	return a.raw.SetSendingStatusForIncarnation(ctx, domain, incarnation, string(status), string(dkimStatus), string(mailFromStatus), errMsg, recordsJSON)
}

func (a *storeAdapter) TouchSendingChecked(ctx context.Context, domain, incarnation string) error {
	return a.raw.TouchSendingCheckedForIncarnation(ctx, domain, incarnation)
}

func (a *storeAdapter) MarkSendingIdentityManaged(ctx context.Context, domain, incarnation string) error {
	return a.raw.MarkSendingIdentityManaged(ctx, domain, incarnation)
}

func (a *storeAdapter) MarkSendingIdentityApplied(ctx context.Context, domain, incarnation string) error {
	return a.raw.MarkSendingIdentityApplied(ctx, domain, incarnation)
}

func (a *storeAdapter) SendingIdentityLedgerExpired(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	return a.raw.SendingIdentityLedgerExpired(ctx, domain, incarnation, olderThan)
}

func (a *storeAdapter) ObserveSendingIdentityProviderPending(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	return a.raw.ObserveSendingIdentityProviderPending(ctx, domain, incarnation, olderThan)
}

func (a *storeAdapter) ClearSendingIdentityProviderPending(ctx context.Context, domain, incarnation string) error {
	return a.raw.ClearSendingIdentityProviderPending(ctx, domain, incarnation)
}

func (a *storeAdapter) ForgetSendingIdentityManaged(ctx context.Context, domain string) error {
	return a.raw.ForgetSendingIdentityManaged(ctx, domain)
}

func (a *storeAdapter) ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error) {
	return a.raw.ListManagedSendingIdentityDomains(ctx)
}

func (a *storeAdapter) FinalizeSendingIdentityTombstone(ctx context.Context, domain string, olderThan time.Duration) error {
	return a.raw.FinalizeSendingIdentityTombstone(ctx, domain, olderThan)
}

func (a *storeAdapter) SetDomainTeardownState(ctx context.Context, domain string, state domainteardown.State) error {
	return a.raw.SetDomainTeardownState(ctx, domain, state)
}

func (a *storeAdapter) DomainExists(ctx context.Context, domain string) (bool, error) {
	return a.raw.DomainExists(ctx, domain)
}
