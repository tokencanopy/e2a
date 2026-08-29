package sendingpolicy

import (
	"strings"
	"testing"
)

// These fixtures are byte-identical to the synthetic values in the ops repo's
// scripts/versioned-secret-test-harness.sh, so both sides of the B1a/B2 seam
// are exercised against the same payloads. Every address is a .test domain and
// every key is a constant byte pattern: no real key or operator mailbox may
// appear in this file.
const (
	fixtureKey1 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 32 × 0x00
	fixtureKey2 = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" // 32 × 0x01
	fixtureCKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI" // 32 × 0x02

	fixtureAddress1 = "operator-one@example.test"
	fixtureAddress2 = "operator-two@example.test"

	hmacV1     = `{"active":1,"keys":{"1":"` + fixtureKey1 + `"}}`
	hmacV2     = `{"active":2,"keys":{"1":"` + fixtureKey1 + `","2":"` + fixtureKey2 + `"}}`
	operatorV1 = `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"` + fixtureAddress1 + `"}}`
	operatorV2 = `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"` + fixtureAddress1 + `","2":"` + fixtureAddress2 + `"}}`
)

// Golden values derived independently (Python hmac/hashlib) from the documented
// construction, not from this package. If the implementation drifts from the
// spec's labels, separators, or version encoding, these stop matching.
const (
	goldenKeyID        = "537d6ecb77e69e381932ce8ccd901c2ee215ffd7786258c5be80a7f12e479466"
	goldenCommitmentV1 = "fd6a1959aa4058d319b742a9d97ef15af794a5c522c5c54dce31f05359b4e41b"
	goldenCommitmentV2 = "1724a54beaba66f07f7e2ebce0c772aa444f669e22f345c1534375631151c1f3"
)

func TestLoadKeyringAcceptsFixtures(t *testing.T) {
	k, err := LoadKeyring(hmacV1)
	if err != nil {
		t.Fatalf("load v1 keyring: %v", err)
	}
	if k.ActiveVersion() != 1 {
		t.Errorf("active = %d, want 1", k.ActiveVersion())
	}

	rotated, err := LoadKeyring(hmacV2)
	if err != nil {
		t.Fatalf("load v2 keyring: %v", err)
	}
	if rotated.ActiveVersion() != 2 {
		t.Errorf("active = %d, want 2", rotated.ActiveVersion())
	}
	if got := rotated.Versions(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("versions = %v, want [1 2]", got)
	}
}

// TestKeyringRotationKeepsVerifying is the whole point of a keyring: material
// signed before a rotation must still verify after it, or feedback correlated
// under the old key becomes permanently unattributable.
func TestKeyringRotationKeepsVerifying(t *testing.T) {
	old, err := LoadKeyring(hmacV1)
	if err != nil {
		t.Fatalf("load old: %v", err)
	}
	msg := []byte("correlation-payload")
	version, mac := old.Sign(msg)

	rotated, err := LoadKeyring(hmacV2)
	if err != nil {
		t.Fatalf("load rotated: %v", err)
	}
	if !rotated.Verify(version, msg, mac) {
		t.Error("a rotated keyring must still verify material signed under version 1")
	}
	if rotated.Verify(version, []byte("tampered"), mac) {
		t.Error("verification must fail for a different message")
	}
}

// TestKeyringVerifyRejectsUnknownVersion pins the fail-closed rule: an unknown
// version must not silently fall back to the active key.
func TestKeyringVerifyRejectsUnknownVersion(t *testing.T) {
	k, err := LoadKeyring(hmacV1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	msg := []byte("payload")
	_, mac := k.Sign(msg)
	if k.Verify(99, msg, mac) {
		t.Error("an unknown key version must never verify")
	}
}

func TestLoadKeyringRejects(t *testing.T) {
	shortKey := "YWJj" // "abc" — 3 decoded bytes, far under the 32-byte floor
	for name, raw := range map[string]string{
		"empty":                   ``,
		"not json":                `not-json`,
		"array":                   `[]`,
		"trailing content":        hmacV1 + `{}`,
		"unknown field":           `{"active":1,"keys":{"1":"` + fixtureKey1 + `"},"extra":true}`,
		"missing active":          `{"keys":{"1":"` + fixtureKey1 + `"}}`,
		"active is bool":          `{"active":true,"keys":{"1":"` + fixtureKey1 + `"}}`,
		"active absent from keys": `{"active":3,"keys":{"1":"` + fixtureKey1 + `"}}`,
		"no keys":                 `{"active":1,"keys":{}}`,
		"zero version":            `{"active":1,"keys":{"0":"` + fixtureKey1 + `"}}`,
		"leading zero version":    `{"active":1,"keys":{"01":"` + fixtureKey1 + `"}}`,
		"oversized version":       `{"active":1,"keys":{"1234567890":"` + fixtureKey1 + `"}}`,
		"key too short":           `{"active":1,"keys":{"1":"` + shortKey + `"}}`,
		"key not base64url":       `{"active":1,"keys":{"1":"AAAA+AAA/AAA=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`,
		"key padded":              `{"active":1,"keys":{"1":"` + fixtureKey1 + `="}}`,
		"control byte":            "{\"active\":1,\"keys\":{\"1\":\"\x01\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKeyring(raw); err == nil {
				t.Fatalf("expected %s keyring to be rejected", name)
			}
		})
	}
}

// TestKeyringErrorsAreRedacted is a security assertion, not a formatting one.
// These payloads are HMAC keys; a startup error that echoed one would leak it
// into logs, journals, and crash reports.
func TestKeyringErrorsAreRedacted(t *testing.T) {
	_, err := LoadKeyring(`{"active":1,"keys":{"1":"YWJj"}}`)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "YWJj") {
		t.Errorf("error must not echo key material: %v", err)
	}

	_, err = LoadKeyring(`{"active":9,"keys":{"1":"` + fixtureKey1 + `"}}`)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), fixtureKey1) {
		t.Errorf("error must not echo key material: %v", err)
	}
}

func TestLoadOperatorRecipientsDerivesGoldenCommitments(t *testing.T) {
	o, err := LoadOperatorRecipients(operatorV2)
	if err != nil {
		t.Fatalf("load operator map: %v", err)
	}
	if o.KeyID() != goldenKeyID {
		t.Errorf("key id = %s, want %s", o.KeyID(), goldenKeyID)
	}
	for version, want := range map[int]string{1: goldenCommitmentV1, 2: goldenCommitmentV2} {
		got, ok := o.Commitment(version)
		if !ok {
			t.Fatalf("version %d missing", version)
		}
		if got != want {
			t.Errorf("commitment v%d = %s, want %s", version, got, want)
		}
	}
}

// TestOperatorCommitmentIsStableAcrossMapGrowth is the rotation contract: adding
// version 2 must not change version 1's commitment, because the registry row
// for version 1 is append-only and can never be rewritten.
func TestOperatorCommitmentIsStableAcrossMapGrowth(t *testing.T) {
	one, err := LoadOperatorRecipients(operatorV1)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	two, err := LoadOperatorRecipients(operatorV2)
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	c1, _ := one.Commitment(1)
	c2, _ := two.Commitment(1)
	if c1 != c2 {
		t.Errorf("version 1 commitment changed when version 2 was added: %s vs %s", c1, c2)
	}
	if one.KeyID() != two.KeyID() {
		t.Error("key identity must not depend on how many versions the map carries")
	}
}

// TestOperatorCommitmentBindsVersionAndMailbox proves the NUL-separated
// construction actually binds both inputs — a commitment that ignored either
// would let a rotation appear complete when the mailbox never changed.
func TestOperatorCommitmentBindsVersionAndMailbox(t *testing.T) {
	sameMailboxDifferentVersion := `{"commitment_key":"` + fixtureCKey + `","recipients":{"2":"` + fixtureAddress1 + `"}}`
	o, err := LoadOperatorRecipients(sameMailboxDifferentVersion)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, _ := o.Commitment(2)
	if got == goldenCommitmentV1 {
		t.Error("the same mailbox at a different version must commit differently")
	}
}

// TestOperatorMailboxIsNormalized covers the ops harness's mixed-case case: the
// stored commitment must be over the normalized form, so two spellings of one
// address cannot produce two different registry rows.
func TestOperatorMailboxIsNormalized(t *testing.T) {
	mixed := `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"Operator-One@Example.TEST"}}`
	o, err := LoadOperatorRecipients(mixed)
	if err != nil {
		t.Fatalf("load mixed-case map: %v", err)
	}
	if mailbox, _ := o.Mailbox(1); mailbox != fixtureAddress1 {
		t.Errorf("mailbox = %q, want normalized %q", mailbox, fixtureAddress1)
	}
	if got, _ := o.Commitment(1); got != goldenCommitmentV1 {
		t.Errorf("commitment must be over the normalized mailbox, got %s", got)
	}
}

func TestLoadOperatorRecipientsRejects(t *testing.T) {
	withRecipient := func(addr string) string {
		return `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"` + addr + `"}}`
	}
	longLocal := strings.Repeat("a", 65)

	for name, raw := range map[string]string{
		"empty":                    ``,
		"not json":                 `nope`,
		"missing recipients":       `{"commitment_key":"` + fixtureCKey + `"}`,
		"empty recipients":         `{"commitment_key":"` + fixtureCKey + `","recipients":{}}`,
		"unknown field":            `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"` + fixtureAddress1 + `"},"x":1}`,
		"commitment key too short": `{"commitment_key":"YWJj","recipients":{"1":"` + fixtureAddress1 + `"}}`,
		"commitment key missing":   `{"recipients":{"1":"` + fixtureAddress1 + `"}}`,
		"zero version":             `{"commitment_key":"` + fixtureCKey + `","recipients":{"0":"` + fixtureAddress1 + `"}}`,
		"oversized version":        `{"commitment_key":"` + fixtureCKey + `","recipients":{"1234567890":"` + fixtureAddress1 + `"}}`,

		"display syntax":      withRecipient("operator<route>@example.test"),
		"unicode local":       withRecipient("opérator@example.test"),
		"leading dot":         withRecipient(".operator@example.test"),
		"trailing dot":        withRecipient("operator.@example.test"),
		"consecutive dots":    withRecipient("op..erator@example.test"),
		"hyphen domain":       withRecipient("operator@-example.test"),
		"single label domain": withRecipient("operator@example"),
		"no at":               withRecipient("operator.example.test"),
		"two ats":             withRecipient("operator@a@example.test"),
		"empty local":         withRecipient("@example.test"),
		"local too long":      withRecipient(longLocal + "@example.test"),
		"comma separated":     withRecipient("a@example.test,b@example.test"),
		"space in address":    withRecipient("operator one@example.test"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadOperatorRecipients(raw); err == nil {
				t.Fatalf("expected %s operator map to be rejected", name)
			}
		})
	}
}

// TestOperatorRejectsControlByteInMailbox is separated out because the NUL has
// to be written as a real byte rather than a JSON escape to exercise the
// envelope check.
func TestOperatorRejectsControlByteInMailbox(t *testing.T) {
	raw := "{\"commitment_key\":\"" + fixtureCKey + "\",\"recipients\":{\"1\":\"operator\x00@example.test\"}}"
	if _, err := LoadOperatorRecipients(raw); err == nil {
		t.Fatal("expected a mailbox containing NUL to be rejected")
	}
}

// TestOperatorRejectsDuplicateNormalizedMailboxes is the plan's explicit case:
// two versions resolving to one mailbox would make an operator believe a
// rotation moved the notice destination when it did not.
func TestOperatorRejectsDuplicateNormalizedMailboxes(t *testing.T) {
	raw := `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"` + fixtureAddress1 + `","2":"Operator-One@EXAMPLE.test"}}`
	_, err := LoadOperatorRecipients(raw)
	if err == nil {
		t.Fatal("expected duplicate normalized mailboxes to be rejected")
	}
	if !strings.Contains(err.Error(), "same mailbox") {
		t.Errorf("error should name the duplication, got %v", err)
	}
}

// TestOperatorErrorsAreRedacted keeps operator addresses out of logs. They are
// secret deployment configuration, not public account data.
func TestOperatorErrorsAreRedacted(t *testing.T) {
	raw := `{"commitment_key":"` + fixtureCKey + `","recipients":{"1":"operator<route>@secret-ops.test"}}`
	err := mustFail(t, raw)
	if strings.Contains(err.Error(), "secret-ops.test") {
		t.Errorf("error must not echo the mailbox: %v", err)
	}
	if strings.Contains(err.Error(), fixtureCKey) {
		t.Errorf("error must not echo the commitment key: %v", err)
	}
}

func mustFail(t *testing.T, raw string) error {
	t.Helper()
	if _, err := LoadOperatorRecipients(raw); err != nil {
		return err
	}
	t.Fatal("expected rejection")
	return nil
}

// TestOperatorItemLimit pins the 128-version boundary the ops resolver enforces,
// on both sides.
func TestOperatorItemLimit(t *testing.T) {
	build := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"commitment_key":"` + fixtureCKey + `","recipients":{`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteByte(',')
			}
			b.WriteString(`"`)
			b.WriteString(itoa(i))
			b.WriteString(`":"operator-`)
			b.WriteString(itoa(i))
			b.WriteString(`@example.test"`)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	if _, err := LoadOperatorRecipients(build(maxSecretItems)); err != nil {
		t.Errorf("%d versions must be accepted: %v", maxSecretItems, err)
	}
	if _, err := LoadOperatorRecipients(build(maxSecretItems + 1)); err == nil {
		t.Errorf("%d versions must be rejected", maxSecretItems+1)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestCommitmentsMapIsDefensiveCopy proves a caller cannot reach in and mutate
// the loaded map — the capability readback hands this to a JSON encoder, and a
// mutation there would desynchronize a slot's advertised commitment from the
// one it actually enforces.
func TestCommitmentsMapIsDefensiveCopy(t *testing.T) {
	o, err := LoadOperatorRecipients(operatorV1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first := o.Commitments()
	first["1"] = "tampered"
	delete(first, "1")

	second := o.Commitments()
	if second["1"] != goldenCommitmentV1 {
		t.Errorf("mutating a returned map changed the loaded state: %v", second)
	}
}
