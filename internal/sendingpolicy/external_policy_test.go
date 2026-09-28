package sendingpolicy

import (
	"strings"
	"testing"
)

// The external-sending-access object is optional precisely so a legacy
// payload keeps its reviewed hash. These tests pin that upgrade contract and
// the strictness of the new object.

func TestExternalAccessAbsentKeepsGenerationZeroHash(t *testing.T) {
	p := DisabledPolicy()
	if p.ExternalSendingAccess != nil {
		t.Fatal("generation zero must not carry external_sending_access")
	}
	hash, err := Hash(p)
	if err != nil {
		t.Fatal(err)
	}
	if hash != generationZeroSHA256 {
		t.Fatalf("absent object changed the legacy hash: %s", hash)
	}
	if p.ExternalSendingMode() != ModeDisabled {
		t.Fatalf("absent object must read as disabled, got %q", p.ExternalSendingMode())
	}
	parsed, err := ParsePolicy([]byte(generationZeroCanonical))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExternalSendingAccess != nil {
		t.Fatal("parsing a legacy payload must leave the object absent")
	}
}

func TestExternalAccessPresentChangesHashAndRoundTrips(t *testing.T) {
	p := DisabledPolicy()
	p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	hash, err := Hash(p)
	if err != nil {
		t.Fatal(err)
	}
	if hash == generationZeroSHA256 {
		t.Fatal("a present object must produce a different reviewed hash")
	}
	canonical, err := CanonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"external_sending_access":{"accounts_created_at_or_after":"2026-10-01T00:00:00Z","mode":"enforce"}`) {
		t.Fatalf("canonical form missing object: %s", canonical)
	}
	parsed, err := ParsePolicy(canonical)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Hash(parsed)
	if err != nil || again != hash {
		t.Fatalf("round trip hash %s != %s (err %v)", again, hash, err)
	}
}

func TestExternalAccessValidationIsStrict(t *testing.T) {
	cases := map[string]ExternalSendingAccessPolicy{
		"unknown mode":        {Mode: "on", AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"},
		"empty mode":          {Mode: "", AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"},
		"missing cutoff":      {Mode: ModeShadow},
		"not rfc3339":         {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01"},
		"non-utc offset":      {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T02:00:00+02:00"},
		"fractional seconds":  {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00.5Z"},
		"lowercase separator": {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01t00:00:00z"},
	}
	for name, esa := range cases {
		esa := esa
		t.Run(name, func(t *testing.T) {
			p := DisabledPolicy()
			p.ExternalSendingAccess = &esa
			if err := p.Validate(); err == nil {
				t.Fatalf("expected %+v to be rejected", esa)
			}
		})
	}
}

func TestExternalAccessParseRejectsUnknownNestedField(t *testing.T) {
	raw := strings.TrimSuffix(generationZeroCanonical, "}") +
		`,"external_sending_access":{"mode":"shadow","accounts_created_at_or_after":"2026-10-01T00:00:00Z","allow_all":true}}`
	if _, err := ParsePolicy([]byte(raw)); err == nil {
		t.Fatal("an unknown nested key must fail closed")
	}
}

func TestExternalAccessNormalizedCopiesObject(t *testing.T) {
	p := DisabledPolicy()
	p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	n := p.normalized()
	n.ExternalSendingAccess.Mode = ModeEnforce
	if p.ExternalSendingAccess.Mode != ModeShadow {
		t.Fatal("normalized must not alias the caller's object")
	}
}

func TestExternalAccessRejectsRemovedPaidPlanCodesKey(t *testing.T) {
	raw := strings.TrimSuffix(generationZeroCanonical, "}") +
		`,"external_sending_access":{"accounts_created_at_or_after":"2026-10-01T00:00:00Z","mode":"enforce","paid_plan_codes":["pro"]}}`
	if _, err := ParsePolicy([]byte(raw)); err == nil {
		t.Fatal("plan codes are not authorization; the key must be rejected like any unknown key")
	}
}

func TestExternalAccessUnlocksAbsentKeepsHashAndMeansAll(t *testing.T) {
	legacy := DisabledPolicy()
	legacy.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	canonical, err := CanonicalBytes(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "unlocks") {
		t.Fatalf("an absent unlock set must not appear in the canonical form: %s", canonical)
	}
	if got := legacy.ExternalSendingAccess.AvailableUnlocks(); len(got) != 3 ||
		got[0] != UnlockOperatorApproval || got[1] != UnlockVerifiedDomain || got[2] != UnlockPaidEntitlement {
		t.Fatalf("absent set must mean every unlock in canonical order, got %v", got)
	}
	for _, u := range []ExternalUnlock{UnlockOperatorApproval, UnlockVerifiedDomain, UnlockPaidEntitlement} {
		if !legacy.ExternalSendingAccess.Allows(u) {
			t.Fatalf("absent set must allow %s", u)
		}
	}
	if legacy.ExternalSendingAccess.Allows("everything") {
		t.Fatal("an unknown unlock is never allowed")
	}
}

func TestExternalAccessUnlocksCanonicalAndRoundTrip(t *testing.T) {
	a := DisabledPolicy()
	a.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z",
		Unlocks: []ExternalUnlock{UnlockPaidEntitlement, UnlockOperatorApproval}}
	b := DisabledPolicy()
	b.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z",
		Unlocks: []ExternalUnlock{UnlockOperatorApproval, UnlockPaidEntitlement}}
	ha, err := Hash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := Hash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("two orderings of one unlock set must hash identically")
	}
	if a.ExternalSendingAccess.Unlocks[0] != UnlockPaidEntitlement {
		t.Fatal("hashing must not reorder the caller's slice")
	}
	canonical, err := CanonicalBytes(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"unlocks":["operator_approval","paid_entitlement"]`) {
		t.Fatalf("canonical form = %s", canonical)
	}
	parsed, err := ParsePolicy(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Hash(parsed); again != ha {
		t.Fatal("round trip changed the hash")
	}
	only := DisabledPolicy()
	only.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z",
		Unlocks: []ExternalUnlock{UnlockOperatorApproval}}
	legacy := DisabledPolicy()
	legacy.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	ho, _ := Hash(only)
	hl, _ := Hash(legacy)
	if ho == hl {
		t.Fatal("narrowing the unlock set must change the reviewed hash")
	}
	if only.ExternalSendingAccess.Allows(UnlockPaidEntitlement) || only.ExternalSendingAccess.Allows(UnlockVerifiedDomain) {
		t.Fatal("[operator_approval] must allow nothing else")
	}
}

func TestExternalAccessUnlocksValidation(t *testing.T) {
	cases := map[string][]ExternalUnlock{
		"empty set":                 {},
		"missing operator_approval": {UnlockVerifiedDomain, UnlockPaidEntitlement},
		"unknown entry":             {UnlockOperatorApproval, "plan_code"},
		"blank entry":               {UnlockOperatorApproval, ""},
		"duplicate entry":           {UnlockOperatorApproval, UnlockOperatorApproval},
		"wrong case":                {"Operator_Approval"},
	}
	for name, unlocks := range cases {
		unlocks := unlocks
		t.Run(name, func(t *testing.T) {
			p := DisabledPolicy()
			p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z", Unlocks: unlocks}
			if err := p.Validate(); err == nil {
				t.Fatalf("expected unlocks %v to be rejected", unlocks)
			}
		})
	}
	for _, ok := range [][]ExternalUnlock{
		nil,
		{UnlockOperatorApproval},
		{UnlockOperatorApproval, UnlockVerifiedDomain},
		{UnlockOperatorApproval, UnlockPaidEntitlement},
		{UnlockPaidEntitlement, UnlockVerifiedDomain, UnlockOperatorApproval},
	} {
		p := DisabledPolicy()
		p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z", Unlocks: ok}
		if err := p.Validate(); err != nil {
			t.Fatalf("unlocks %v must be valid: %v", ok, err)
		}
	}
	// A stored payload carrying an explicit empty list must fail closed on
	// read, exactly like a config that spells one.
	raw := strings.TrimSuffix(generationZeroCanonical, "}") +
		`,"external_sending_access":{"accounts_created_at_or_after":"2026-10-01T00:00:00Z","mode":"enforce","unlocks":[]}}`
	if _, err := ParsePolicy([]byte(raw)); err == nil {
		t.Fatal("a stored empty unlock set must be rejected")
	}
}

func TestExternalAccessExplicitFullUnlockSetHashesLikeOmitted(t *testing.T) {
	omitted := DisabledPolicy()
	omitted.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	full := DisabledPolicy()
	full.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z",
		Unlocks: []ExternalUnlock{UnlockPaidEntitlement, UnlockOperatorApproval, UnlockVerifiedDomain}}
	ho, err := Hash(omitted)
	if err != nil {
		t.Fatal(err)
	}
	hf, err := Hash(full)
	if err != nil {
		t.Fatal(err)
	}
	if ho != hf {
		t.Fatal("an explicit full unlock set must canonicalize to the omitted form")
	}
	if full.ExternalSendingAccess.Unlocks == nil {
		t.Fatal("canonicalizing must not mutate the caller's value")
	}
	// A duplicate-padded list is still rejected before canonicalization.
	dup := DisabledPolicy()
	dup.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z",
		Unlocks: []ExternalUnlock{UnlockOperatorApproval, UnlockOperatorApproval, UnlockPaidEntitlement}}
	if err := dup.Validate(); err == nil {
		t.Fatal("duplicates must be rejected")
	}
}
