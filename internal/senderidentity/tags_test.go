package senderidentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// tagMap flattens the SES tag list a Provision call sent, so a test can assert
// the EXACT set (an unexpected extra tag fails too — the reaper will read these
// as authority, so a stray tag is as much a defect as a missing one).
func tagMap(tags []sestypes.Tag) map[string]string {
	out := map[string]string{}
	for _, tg := range tags {
		if tg.Key == nil || tg.Value == nil {
			continue
		}
		out[*tg.Key] = *tg.Value
	}
	return out
}

func testDKIMKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return x509.MarshalPKCS1PrivateKey(key)
}

// TestSESProvider_ProvisionIdentityTags pins the full classification tag set a
// freshly created identity carries, case by case. The point of the tags is that
// "is this identity safe to delete?" becomes answerable from AWS alone, so each
// case asserts the WHOLE map: a missing tag and a spurious one are both defects.
//
// Every value is derived at runtime and every derivation is best-effort — the
// absent-input cases below (no deployment name, unknown account class, no owner,
// no build) prove that a missing input drops exactly ONE tag and never fails the
// provision.
func TestSESProvider_ProvisionIdentityTags(t *testing.T) {
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	const createdRFC3339 = "2026-03-04T05:06:07Z"
	const ttl = 24 * time.Hour
	const expiresRFC3339 = "2026-03-05T05:06:07Z"

	tests := []struct {
		name       string
		deployment string
		build      string
		fixtureTTL time.Duration
		meta       ProvisionMeta
		want       map[string]string
	}{
		{
			name:       "standard account on prod → customer, no expires",
			deployment: "prod",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "demo account is a customer too (never auto-deletable)",
			deployment: "staging",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "demo"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "staging",
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "internal account → fixture with an expiry",
			deployment: "staging",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_int", AccountClass: "internal"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "staging",
				purposeTagKey:         purposeFixture,
				createdTagKey:         createdRFC3339,
				expiresTagKey:         expiresRFC3339,
				userTagKey:            "user_int",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "system (prober) account → fixture with an expiry",
			deployment: "prod",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_sys", AccountClass: "system"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				purposeTagKey:         purposeFixture,
				createdTagKey:         createdRFC3339,
				expiresTagKey:         expiresRFC3339,
				userTagKey:            "user_sys",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "deployment name unset (self-host) → env omitted, everything else stands",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "unrecognized deployment name behaves as unset, never as a literal tag",
			deployment: "production",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "account class unknown (lookup failed) → purpose AND expires omitted",
			deployment: "prod",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "unrecognized account class is not a purpose → omitted, never guessed",
			deployment: "prod",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "platinum"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "empty owner → user tag omitted",
			deployment: "prod",
			build:      "1.7.11",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				provisionerTagKey:     "1.7.11",
			},
		},
		{
			name:       "no build string → provisioner tag omitted",
			deployment: "prod",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
			},
		},
		{
			name:       "build string SES would reject is dropped, not sent",
			deployment: "prod",
			build:      "1.7.11 (dirty#7)",
			fixtureTTL: ttl,
			meta:       ProvisionMeta{UserID: "user_abc", AccountClass: "standard"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "prod",
				purposeTagKey:         purposeCustomer,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_abc",
			},
		},
		{
			name:       "fixture TTL disabled → fixture purpose without an expiry",
			deployment: "staging",
			build:      "1.7.11",
			fixtureTTL: 0,
			meta:       ProvisionMeta{UserID: "user_int", AccountClass: "internal"},
			want: map[string]string{
				managedIdentityTagKey: managedIdentityTagValue,
				envTagKey:             "staging",
				purposeTagKey:         purposeFixture,
				createdTagKey:         createdRFC3339,
				userTagKey:            "user_int",
				provisionerTagKey:     "1.7.11",
			},
		},
	}

	pkcs1 := testDKIMKey(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubSESAPI{}
			p := NewSESProvider(stub, "us-east-2", testAccountID).
				WithIdentityTags(tt.deployment, tt.build, tt.fixtureTTL)
			p.now = func() time.Time { return created }

			res, err := p.Provision(context.Background(), "acme.example", "e2a202603", pkcs1, tt.meta)
			if err != nil {
				t.Fatalf("Provision error: %v — tag construction must never fail a provision", err)
			}
			if res.Status != StatusPending {
				t.Fatalf("status = %q, want pending", res.Status)
			}
			if stub.createInput == nil {
				t.Fatal("CreateEmailIdentity was never called")
			}
			if got := tagMap(stub.createInput.Tags); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identity tags = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSESProvider_ProvisionTagsDefaultToOwnershipOnly proves an SESProvider that
// was never given tag configuration (every test constructing one by hand, and
// any self-host wiring that skips WithIdentityTags) still emits the ownership
// anchor plus only the tags it can actually derive — the created stamp. This is
// the regression guard for isManagedIdentity's input: the ownership tag's key
// and value are unchanged by this feature.
func TestSESProvider_ProvisionTagsDefaultToOwnershipOnly(t *testing.T) {
	stub := &stubSESAPI{}
	p := NewSESProvider(stub, "us-east-2", testAccountID)

	if _, err := p.Provision(context.Background(), "acme.example", "sel", testDKIMKey(t), ProvisionMeta{}); err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	got := tagMap(stub.createInput.Tags)
	if got[managedIdentityTagKey] != managedIdentityTagValue {
		t.Fatalf("ownership tag = %q, want %q", got[managedIdentityTagKey], managedIdentityTagValue)
	}
	if _, ok := got[createdTagKey]; !ok {
		t.Fatalf("created tag missing: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("unconfigured provider emitted %v, want only ownership + created", got)
	}
	// The tag e2a's own ownership check reads must still be recognizable.
	if !isManagedIdentity(&sesv2.GetEmailIdentityOutput{Tags: stub.createInput.Tags}) {
		t.Fatal("isManagedIdentity no longer recognizes a freshly provisioned identity")
	}
}

// TestSESProvider_AdoptionWritesOnlyTheOwnershipTag pins that adoption is
// untouched by this feature: an untagged pre-existing identity is still tagged
// with exactly the ownership anchor. Classification tags describe what e2a
// CREATED; back-dating them onto an adopted identity would invent a creation
// time and an owner classification that were never observed.
func TestSESProvider_AdoptionWritesOnlyTheOwnershipTag(t *testing.T) {
	pkcs1 := testDKIMKey(t)
	stub := &stubSESAPI{
		createErr: &sestypes.AlreadyExistsException{},
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &sestypes.DkimAttributes{
				SigningAttributesOrigin: sestypes.DkimSigningAttributesOriginExternal,
				Status:                  sestypes.DkimStatusSuccess,
				Tokens:                  []string{"e2a202607"},
			},
		},
	}
	p := NewSESProvider(stub, "us-east-2", testAccountID).
		WithIdentityTags("prod", "1.7.11", 24*time.Hour)

	if _, err := p.Provision(context.Background(), "legacy.example", "e2a202607", pkcs1, ProvisionMeta{UserID: "user_abc", AccountClass: "standard"}); err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if len(stub.tagInputs) != 1 {
		t.Fatalf("want exactly one TagResource call, got %d", len(stub.tagInputs))
	}
	want := map[string]string{managedIdentityTagKey: managedIdentityTagValue}
	if got := tagMap(stub.tagInputs[0].Tags); !reflect.DeepEqual(got, want) {
		t.Fatalf("adoption tags = %v, want %v", got, want)
	}
}

// TestProvisionWorker_StampsOwnerClassification proves the worker resolves the
// domain owner's account class and hands it (plus the owner id) to the provider,
// which is the only way the SES-side tags can be derived at all.
func TestProvisionWorker_StampsOwnerClassification(t *testing.T) {
	const domain = "classified.example"
	const owner = "user_1"

	store := newFakeStore()
	store.setStatus(domain, StatusNone)
	store.setOwner(domain, owner)
	store.setProvisionInputs("sel1", []byte("der"), true)
	store.accountClass[owner] = "internal"
	prov := NewFakeProvider()
	w := &ProvisionWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	want := []ProvisionMeta{{UserID: owner, AccountClass: "internal"}}
	if !reflect.DeepEqual(prov.ProvisionMetas, want) {
		t.Fatalf("provision meta = %+v, want %+v", prov.ProvisionMetas, want)
	}
}

// TestProvisionWorker_AccountClassLookupFailureStillProvisions is the fail-OPEN
// half of the contract. Tagging is metadata for a LATER, fail-CLOSED deletion
// decision: an untagged identity is simply one the reaper must leave alone, so a
// class lookup that errors must degrade to a partial tag set and never turn a
// customer's domain verification into a failure.
func TestProvisionWorker_AccountClassLookupFailureStillProvisions(t *testing.T) {
	const domain = "lookup-broke.example"
	const owner = "user_1"

	store := newFakeStore()
	store.setStatus(domain, StatusNone)
	store.setOwner(domain, owner)
	store.setProvisionInputs("sel1", []byte("der"), true)
	store.accountClassErr = errors.New("db down")
	prov := NewFakeProvider()
	w := &ProvisionWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
		t.Fatalf("Work: %v — a class lookup failure must never fail provisioning", err)
	}
	if len(prov.ProvisionCalls) != 1 {
		t.Fatalf("Provision calls = %v, want exactly one", prov.ProvisionCalls)
	}
	want := []ProvisionMeta{{UserID: owner}}
	if !reflect.DeepEqual(prov.ProvisionMetas, want) {
		t.Fatalf("provision meta = %+v, want the owner with no class", prov.ProvisionMetas)
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusPending {
		t.Fatalf("status = %q, want pending", got)
	}
}

// TestStoreAdapterAccountClassIsOptional covers the self-host / test wiring that
// passes no account-class source: it must read as "unknown", not as an error the
// worker would have to handle.
func TestStoreAdapterAccountClassIsOptional(t *testing.T) {
	a := NewStoreAdapter(&fakeRawStore{}, nil)
	class, err := a.AccountClassForUser(context.Background(), "user_1")
	if err != nil || class != "" {
		t.Fatalf("AccountClassForUser = (%q, %v), want (\"\", nil)", class, err)
	}

	a = NewStoreAdapter(&fakeRawStore{}, func(ctx context.Context, userID string) (string, error) {
		return "system", nil
	})
	class, err = a.AccountClassForUser(context.Background(), "user_1")
	if err != nil || class != "system" {
		t.Fatalf("AccountClassForUser = (%q, %v), want (\"system\", nil)", class, err)
	}
}

// TestTagValueAccepted pins the SES tag-value character/length rule the builder
// screens against. It exists because an unscreened operator-supplied build
// string is the one tag value e2a does not control, and SES rejects the WHOLE
// CreateEmailIdentity call on a bad tag — which would turn cosmetic metadata
// into a provisioning outage.
func TestTagValueAccepted(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1.7.11", true},
		{"sha-0a1b2c3", true},
		{"2026-03-04T05:06:07Z", true},
		{"user_01JABCDEF", true},
		{"prod", true},
		{"", false},
		{"1.7.11 (dirty#7)", false},
		{"build\nname", false},
		{strings.Repeat("v", 256), true},
		{strings.Repeat("v", 257), false},
	}
	for _, tt := range tests {
		if got := tagValueAccepted(tt.value); got != tt.want {
			t.Errorf("tagValueAccepted(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}
