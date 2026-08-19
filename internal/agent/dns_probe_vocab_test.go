package agent

import (
	"bytes"
	"log"
	"sort"
	"strings"
	"testing"
)

// The verify endpoint's mx/spf/dkim fields speak the LIVE PROBE vocabulary —
// found / missing / deferred / mismatch — which is deliberately distinct from
// the PERSISTED per-record vocabulary on DomainView.dns_records[].status
// (verified / pending / missing / failed). A probe answers "what did DNS
// return just now"; the persisted status answers "what has the platform
// recorded about this record". checkDomainRecords (+ classifyDKIM) is the
// only emitter of the probe vocabulary; this test pins its exact value set so
// anyone "harmonizing" probe words into persisted-state words (found→verified,
// missing→pending, …) gets a loud failure here and in
// internal/httpapi/domains_vocab_test.go instead of a silent contract change.

// persistedVocab are the persisted-axis words that must NEVER be emitted by
// the probe ("missing" is legitimately shared between the two documented
// sets, so it is excluded from the leak check).
var persistedOnlyVocab = map[string]bool{"verified": true, "pending": true, "failed": true}

// probeVocab is the exact live-probe value set as shipped:
//   - found    — the probed record is published and matches what e2a expects.
//   - missing  — the record is absent (or the DNS lookup failed).
//   - deferred — DKIM only: probe skipped because no per-domain DKIM keypair
//     is stored yet (legacy pre-keying rows) — NOT a DNS-propagation wait.
//   - mismatch — DKIM only: a record IS published at the selector but its key
//     doesn't match the issued one (almost always a truncated TXT).
//
// mx and spf emit only found/missing today; dkim emits all four.
var probeVocab = []string{"deferred", "found", "mismatch", "missing"}

// TestDNSProbeVocabularyPinned drives every reachable emitting path of
// checkDomainRecords and classifyDKIM and asserts the union of observed
// values equals probeVocab exactly, with no persisted-axis words leaking in.
//
// If this fails because you added a genuinely new probe outcome: update the
// pin AND the VerifyDomainView doc tags (internal/httpapi/domains.go). If it
// fails because a persisted word (verified/pending/failed) appeared: stop —
// the two vocabularies are distinct axes on purpose; do not converge them.
func TestDNSProbeVocabularyPinned(t *testing.T) {
	got := map[string]bool{}
	record := func(c dnsRecordCheck) {
		got[c.MX] = true
		got[c.SPF] = true
		got[c.DKIM] = true
	}

	// Dev short-circuit: everything found; DKIM deferred without a keypair.
	record(checkDomainRecords("example.com", "mx.e2a.test", "tok", "e2a", "MIIBkeyAQAB", false))
	record(checkDomainRecords("example.com", "mx.e2a.test", "tok", "", "", false))

	// Production path against a syntactically invalid name (label > 63 chars —
	// rejected by the resolver client-side, so no real DNS traffic): every
	// lookup errors, exercising the missing defaults; without a keypair the
	// DKIM probe is skipped → deferred.
	invalid := strings.Repeat("x", 64) + ".invalid"
	record(checkDomainRecords(invalid, "mx.e2a.test", "tok", "", "", true))
	record(checkDomainRecords(invalid, "mx.e2a.test", "tok", "e2a", "MIIBkeyAQAB", true))

	// classifyDKIM is the remaining emitter (found/mismatch/missing from live
	// TXT answers — full behavioral coverage lives in
	// classify_dkim_internal_test.go; here we only pin its vocabulary).
	const issued = "MIIBIjANBgkqfullkeyAQAB"
	for _, txts := range [][]string{
		{"v=DKIM1; k=rsa; p=" + issued},        // found
		{"v=DKIM1; k=rsa; p=SOMEOTHERKEYAQAB"}, // mismatch
		{"v=spf1 include:amazonses.com ~all"},  // missing (no p=)
		nil,                                    // missing (no records)
	} {
		got[classifyDKIM(txts, issued)] = true
	}

	var emitted []string
	for v := range got {
		emitted = append(emitted, v)
	}
	sort.Strings(emitted)
	if len(emitted) != len(probeVocab) || strings.Join(emitted, ",") != strings.Join(probeVocab, ",") {
		t.Errorf("live DNS probe vocabulary drifted:\n  got:  %v\n  want: %v\nThe probe vocabulary (verify endpoint) is deliberately distinct from the persisted dns_records[].status vocabulary (verified/pending/missing/failed) — see internal/httpapi/domains_vocab_test.go before changing either.", emitted, probeVocab)
	}
	for v := range got {
		if persistedOnlyVocab[v] {
			t.Errorf("probe emitted persisted-vocabulary value %q — the live-probe axis must never speak persisted-state words (verified/pending/failed)", v)
		}
	}
}

// TestCheckDomainRecordsDevShortCircuitLogsWarning pins that the dev-mode
// short-circuit (production=false) never fires silently: it skips every real
// DNS lookup and reports a domain fully verified with no ownership proof at
// all, which is fine for local dev but would be a silent security-check
// stub — and a domain-ownership bypass on a multi-user deployment — if a
// deployment ended up in this mode unintentionally (e.g. env: production
// never actually set; see TestLoadConfigEnvVarOverridesYAMLEnv in
// internal/config). The production path must stay quiet on this front (it
// has nothing to warn about — it did real lookups).
func TestCheckDomainRecordsDevShortCircuitLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	checkDomainRecords("dev-shortcut.example.test", "mx.e2a.test", "tok", "", "", false /* production */)

	got := buf.String()
	if !strings.Contains(got, "WARNING") || !strings.Contains(got, "dev-shortcut.example.test") {
		t.Errorf("dev-mode short-circuit should log a loud, domain-identifying warning every time it fires; got: %q", got)
	}

	buf.Reset()
	invalid := strings.Repeat("x", 64) + ".invalid" // rejected client-side, no real DNS traffic
	checkDomainRecords(invalid, "mx.e2a.test", "tok", "", "", true /* production */)
	if buf.Len() != 0 {
		t.Errorf("production path should never log the dev-shortcut warning; got: %q", buf.String())
	}
}
