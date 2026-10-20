package sendingpolicy_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestKeyInventoryRetainedReferencesAndPermanentRegistry(t *testing.T) {
	m, pool := newModule(t)
	ctx := context.Background()
	// No account or message is needed: retained security evidence outlives both.
	for _, q := range []string{
		`INSERT INTO sending_feedback_correlations (correlation_id,operation_id,submission_attempt,policy_subject_ref,purpose,tenant_mode,expires_at) VALUES ('corr_live','op_live',1,'subject_test','customer_message','none',NULL),('corr_expired','op_expired',1,'subject_test','customer_message','none',now()-interval '1 second')`,
		`INSERT INTO sending_feedback_recipients (correlation_id,recipient_hmac,hmac_key_version) VALUES ('corr_live','a',7),('corr_expired','b',8)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	before := mustInspect(t, m)
	got, err := m.KeyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.PolicyGeneration != before.Generation || got.PolicySHA256 != before.PolicySHA256 || got.SelectedRecipientVersion != 1 {
		t.Fatalf("invalid policy snapshot: %+v", got)
	}
	if !reflect.DeepEqual(got.RequiredHMACVersions, []int{7}) {
		t.Fatalf("retained keys = %v", got.RequiredHMACVersions)
	}
	if len(got.RecipientRegistry) != 1 || got.RecipientRegistry[0].Version != 1 {
		t.Fatalf("registry = %+v", got.RecipientRegistry)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"corr_live", "op_live", "@", "created_by"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("inventory leaks %q", private)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE sending_feedback_correlations SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	got, err = m.KeyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredHMACVersions) != 0 || len(got.RecipientRegistry) != 1 {
		t.Fatalf("expiration erased registry or retained expired key: %+v", got)
	}
	after := mustInspect(t, m)
	if before.Generation != after.Generation || before.PolicySHA256 != after.PolicySHA256 {
		t.Fatal("inventory mutated policy")
	}
}

func TestKeyInventoryCorruptPolicyFailsClosed(t *testing.T) {
	m, pool := newModule(t)
	if _, err := pool.Exec(context.Background(), `UPDATE sending_protection_runtime_policy SET policy_sha256=repeat('0',64)`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.KeyInventory(context.Background()); err == nil {
		t.Fatal("corrupt policy accepted")
	}
}

func TestKeyInventoryNoticeNamespacesAndUnresolvedReference(t *testing.T) {
	m, pool := newModule(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO sending_protection_notice_events(id,account_ref,kind,reason_code,budget_scope,ledger_day,expires_at) VALUES('notice_test','account_test','budget_violation','budget_limit','account_daily',current_date,now()+interval '1 day')`,
		`INSERT INTO sending_protection_notice_deliveries(event_id,audience,current_operation_id) VALUES('notice_test','owner','owner_op'),('notice_test','operator','operator_op')`,
		`INSERT INTO sending_budget_reservations(operation_id,submission_attempt,policy_subject_ref,purpose,day,units,probation,state,call_state,authorization_nonce,notice_recipient_version,notice_recipient_commitment) VALUES ('owner_op',1,'subject_test','violation_operational',current_date,1,false,'confirmed','authorized','nonce_owner',23,'owner_hmac'),('operator_op',1,'subject_test','violation_operational',current_date,1,false,'confirmed','authorized','nonce_operator',42,'operator_commitment')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.KeyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RequiredHMACVersions, []int{23}) || !reflect.DeepEqual(got.RequiredRecipientVersions, []int{42}) {
		t.Fatalf("namespaces mixed: %+v", got)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM sending_protection_notice_deliveries WHERE audience='owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.KeyInventory(ctx); err == nil {
		t.Fatal("ambiguous reference silently omitted")
	}
	if _, err = pool.Exec(ctx, `UPDATE sending_budget_reservations SET expires_at=now()-interval '1 second' WHERE operation_id='owner_op'`); err != nil {
		t.Fatal(err)
	}
	got, err = m.KeyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredHMACVersions) != 0 || !reflect.DeepEqual(got.RequiredRecipientVersions, []int{42}) {
		t.Fatalf("expiration = %+v", got)
	}
}

func TestKeyInventoryBoundAndRedaction(t *testing.T) {
	m, pool := newModule(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sending_operator_recipient_versions(logical_version,commitment_key_id,recipient_commitment,created_by) SELECT v,repeat('a',64),repeat('b',64),'PRIVATE_ACTOR_SENTINEL' FROM generate_series(10000,14096) v`); err != nil {
		t.Fatal(err)
	}
	got, err := m.KeyInventory(ctx)
	if err == nil {
		t.Fatal("oversized registry accepted")
	}
	if err.Error() != "sendingpolicy: key inventory unavailable" || got.SchemaVersion != 0 {
		t.Fatalf("partial result or unsafe error: %+v %v", got, err)
	}
}

func TestKeyInventoryPinsExpiredCurrentAuthorization(t *testing.T) {
	m, pool := newModule(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO sending_protection_notice_events(id,account_ref,kind,reason_code,budget_scope,ledger_day,expires_at) VALUES('notice_expired','account_test','budget_violation','budget_limit','account_daily',current_date,now()+interval '1 day')`,
		`INSERT INTO sending_protection_notice_deliveries(event_id,audience,current_operation_id) VALUES('notice_expired','owner','expired_op')`,
		`INSERT INTO sending_provider_operations(operation_id,policy_subject_ref,purpose,current_attempt,expires_at) VALUES('expired_op','subject_test','violation_operational',2,now()-interval '1 day')`,
		`INSERT INTO sending_budget_reservations(operation_id,submission_attempt,policy_subject_ref,purpose,day,units,probation,state,call_state,authorization_nonce,notice_recipient_version,notice_recipient_commitment,expires_at) VALUES ('expired_op',1,'subject_test','violation_operational',current_date,1,false,'confirmed','authorized','nonce_old',23,'old_hmac',now()-interval '1 day'),('expired_op',2,'subject_test','violation_operational',current_date,1,false,'confirmed','authorized','nonce_current',24,'current_hmac',now()-interval '1 day')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.KeyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RequiredHMACVersions, []int{24}) {
		t.Fatalf("expired current authorization must pin key, superseded must not: %v", got.RequiredHMACVersions)
	}
}
