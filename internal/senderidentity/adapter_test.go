package senderidentity

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// fakeRawStore implements RawStore, recording the primitive (string/JSON) form
// SetSendingStatus receives so the adapter's conversion can be asserted.
type fakeRawStore struct {
	lastStatus         string
	lastDkimStatus     string
	lastMailFromStatus string
	lastErrMsg         string
	lastRecordsJSON    []byte
	getStatusReturn    string
}

func (f *fakeRawStore) WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) error {
	return fn(ctx)
}

func (f *fakeRawStore) LoadSendingIdentityState(ctx context.Context, domain string) (string, string, bool, string, string, []byte, string, error) {
	return "inc-1", "user_1", true, f.getStatusReturn, "sel", []byte("der"), "inc-applied", nil
}

func (f *fakeRawStore) SetSendingStatusForIncarnation(ctx context.Context, domain, incarnation, status, dkimStatus, mailFromStatus, errMsg string, recordsJSON []byte) error {
	f.lastStatus = status
	f.lastDkimStatus = dkimStatus
	f.lastMailFromStatus = mailFromStatus
	f.lastErrMsg = errMsg
	f.lastRecordsJSON = recordsJSON
	return nil
}

func (f *fakeRawStore) TouchSendingCheckedForIncarnation(ctx context.Context, domain, incarnation string) error {
	return nil
}

func (f *fakeRawStore) MarkSendingIdentityManaged(context.Context, string, string) error { return nil }
func (f *fakeRawStore) MarkSendingIdentityApplied(context.Context, string, string) error { return nil }
func (f *fakeRawStore) ForgetSendingIdentityManaged(context.Context, string) error       { return nil }
func (f *fakeRawStore) ListManagedSendingIdentityDomains(context.Context) ([]string, map[string]bool, error) {
	return nil, nil, nil
}

func TestStoreAdapter_SetSendingStatus(t *testing.T) {
	raw := &fakeRawStore{}
	store := NewStoreAdapter(raw)
	records := []DNSRecord{{Type: "TXT", Name: "_dmarc", Value: "v=DMARC1"}}

	if err := store.SetSendingStatus(context.Background(), "example.com", "inc-1", StatusVerified, StatusVerified, StatusFailed, "ok", records); err != nil {
		t.Fatalf("SetSendingStatus: %v", err)
	}
	if raw.lastStatus != "verified" {
		t.Fatalf("status not converted to string: got %q", raw.lastStatus)
	}
	if raw.lastDkimStatus != "verified" || raw.lastMailFromStatus != "failed" {
		t.Fatalf("per-axis statuses not converted: dkim=%q mailFrom=%q", raw.lastDkimStatus, raw.lastMailFromStatus)
	}
	if raw.lastErrMsg != "ok" {
		t.Fatalf("errMsg = %q", raw.lastErrMsg)
	}
	var gotRecords []DNSRecord
	if err := json.Unmarshal(raw.lastRecordsJSON, &gotRecords); err != nil {
		t.Fatalf("records not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(gotRecords, records) {
		t.Fatalf("records round-trip mismatch: got %+v want %+v", gotRecords, records)
	}
}

func TestStoreAdapter_SetSendingStatus_NoRecords(t *testing.T) {
	raw := &fakeRawStore{}
	store := NewStoreAdapter(raw)
	if err := store.SetSendingStatus(context.Background(), "example.com", "inc-1", StatusPending, "", "", "", nil); err != nil {
		t.Fatalf("SetSendingStatus: %v", err)
	}
	if raw.lastRecordsJSON != nil {
		t.Fatalf("expected nil records JSON for empty records, got %q", raw.lastRecordsJSON)
	}
	if raw.lastDkimStatus != "" || raw.lastMailFromStatus != "" {
		t.Fatalf("expected empty per-axis statuses, got dkim=%q mailFrom=%q", raw.lastDkimStatus, raw.lastMailFromStatus)
	}
}

func TestStoreAdapter_LoadSendingIdentityState(t *testing.T) {
	raw := &fakeRawStore{getStatusReturn: "pending"}
	store := NewStoreAdapter(raw)
	got, err := store.LoadSendingIdentityState(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetSendingStatus: %v", err)
	}
	if got.Status != StatusPending || got.Incarnation != "inc-1" || got.Owner != "user_1" || !got.Verified || got.AppliedIncarnation != "inc-applied" {
		t.Fatalf("primitive state conversion failed: got %+v", got)
	}
}
