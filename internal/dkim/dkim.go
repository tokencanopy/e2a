// Package dkim wraps github.com/emersion/go-msgauth/dkim for the
// per-domain signing path.
//
// Responsibilities split:
//
//   - GenerateKeypair generates a fresh RSA-2048 keypair and returns the
//     three things the rest of the system needs: a DNS-friendly selector,
//     the base64 public key for the user's DNS TXT value, and the
//     PKCS#1-DER private key for BYTEA storage.
//
//   - Sign takes a fully composed RFC 5322 message body, looks up the
//     matching keypair by selector + domain, and returns the message
//     with a DKIM-Signature header prepended. Callers that don't have a
//     key (legacy domains, the seeded shared domain) skip this and the
//     downstream SMTP relay falls back to the deployment-level signing
//     it has always done.
//
//   - DNSRecord renders the TXT record the user must publish to make
//     their key resolvable. The shape is fixed by RFC 6376 §3.6.1; the
//     selector convention "e2a{YYYYMM}" matches what we tell users in
//     the Get-started UI.
//
// The 2048-bit RSA choice mirrors what every major mailbox provider
// recommends for new selectors as of 2026. Ed25519 keys are smaller but
// not all receivers verify them yet — switch when adoption catches up.
package dkim

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/textproto"
	"strings"
	"time"

	msgauth "github.com/emersion/go-msgauth/dkim"
)

// SelectorForNow returns the selector used for new keypairs at the
// current wall-clock month. The "e2a" prefix scopes the selector to
// this product so users hosting both e2a and another mail provider
// under the same domain can keep selectors disjoint. The YYYYMM tail
// lets us rotate selectors monthly without colliding with existing
// records — the rotated row reuses the same column, but selector
// changes mean DNS lookups land on a new key.
func SelectorForNow() string {
	return SelectorForTime(time.Now().UTC())
}

// SelectorForTime is the testable variant of SelectorForNow.
func SelectorForTime(t time.Time) string {
	return fmt.Sprintf("e2a%04d%02d", t.Year(), int(t.Month()))
}

// Keypair is the result of GenerateKeypair. PrivateKeyDER is suitable
// for direct BYTEA storage; PublicKeyDNS is the literal "p=" value for
// the TXT record.
type Keypair struct {
	Selector      string
	PublicKeyDNS  string
	PrivateKeyDER []byte
}

// GenerateKeypair mints a fresh RSA-2048 keypair scoped to the current
// month's selector. PrivateKeyDER is PKCS#1 DER (parseable with
// x509.ParsePKCS1PrivateKey); PublicKeyDNS is the base64 SPKI value
// with the PEM header/footer/newlines stripped so it can be pasted
// straight into a TXT record's "p=" field.
func GenerateKeypair() (*Keypair, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("rsa keygen: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	return &Keypair{
		Selector:      SelectorForNow(),
		PublicKeyDNS:  base64.StdEncoding.EncodeToString(pubDER),
		PrivateKeyDER: x509.MarshalPKCS1PrivateKey(key),
	}, nil
}

// signedHeaderCandidates is the whitelist of stable headers worth covering
// when they are present. Signing every header is brittle — receivers reject
// messages whose intermediary MTAs rewrite or fold a covered header — so the
// list stays narrow and keeps DMARC alignment intact while tolerating typical
// send-via-SES rewrites. Date is deliberately excluded because SES replaces
// even a supplied value with its acceptance timestamp.
//
// Presence matters: see signedHeaderKeys for why a header on this list is
// only signed when the message actually carries it.
var signedHeaderCandidates = []string{
	"From", "To", "Cc", "Subject",
	"Message-ID", "In-Reply-To", "References",
	"MIME-Version", "Content-Type", "Reply-To",
	"List-Unsubscribe", "List-Unsubscribe-Post",
}

// signedHeaderKeys narrows the candidate list to headers the message
// actually has, always keeping From (RFC 6376 § 5.4 requires it in h=).
//
// Why this matters: listing a header in h= that is absent at signing time
// is "oversigning" — the signer hashes it as empty, and a verifier that
// later finds the header present computes a different hash and reports
// the signature as broken. That is deliberate tamper-detection, and it
// fires on legitimate traffic the moment a downstream MTA adds one of
// these headers.
//
// It did. internal/outbound deliberately omits Message-ID so SES can
// assign one (and we capture it from the SMTP response), while this list
// oversigned Message-ID — so SES added the header after signing and
// invalidated the signature on every single outbound message. Delivered
// mail carried three DKIM signatures: SES's own, SES's BYODKIM signature
// for the sending domain, and ours, permanently failing.
//
// DMARC survived on the passing aligned signature, which is why this went
// unnoticed, but a signature that never verifies is worse than no
// signature: some receivers weigh a broken one against sender reputation.
//
// The trade-off is losing oversigning's protection against a header being
// added in transit. That protection was worth nothing here — it was
// producing a 100% invalid signature — and the narrow candidate list
// means the exposure is a header the message chose not to set.
//
// Detection deliberately uses net/textproto, the same parser msgauth
// reaches through go-message, because the two must agree. The failure
// modes are not symmetric:
//
//   - Claiming a header is present when the signer disagrees puts it in
//     h= unsigned-but-claimed, which is the oversigning bug all over
//     again: a broken signature on every message.
//   - Claiming it is absent when the signer would have seen it merely
//     leaves that header uncovered. The signature still verifies.
//
// So when the header block is odd — a line missing its colon aborts
// textproto mid-block, and "Subject : x" parses to the key "Subject "
// with the space retained — this narrows h= rather than widening it, and
// degrades to a valid signature over fewer headers. A more permissive
// hand-rolled scan would find headers msgauth then does not, which is
// the dangerous direction. Composed messages never take these paths;
// headerWriter emits "Key: value" and strips CR/LF.
func signedHeaderKeys(message []byte) []string {
	count := map[string]int{}
	hdr, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(message))).ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		// Unparseable header block: fall back to the full candidate list
		// rather than signing nothing. msgauth will fail the sign and the
		// caller sends unsigned, which is the existing behavior.
		return signedHeaderCandidates
	}
	for k, values := range hdr {
		count[k] = len(values)
	}

	keys := make([]string, 0, 2*len(signedHeaderCandidates))
	for _, k := range signedHeaderCandidates {
		n := count[textproto.CanonicalMIMEHeaderKey(k)]
		if n == 0 {
			// From must be covered even when absent (RFC 6376 § 5.4).
			if k == "From" {
				keys = append(keys, k)
			}
			continue
		}
		// N+1 oversigning: list a present header once more than it
		// occurs. A verifier binds each listed instance from the bottom
		// up, so the extra entry asserts "there is no further instance".
		// Without it, a hop can PREPEND a second Subject: the verifier
		// still matches the original and reports pass, while the MUA
		// displays the attacker's copy. Confirmed both ways — listing
		// once accepts the spoof, listing n+1 times rejects it.
		for i := 0; i < n+1; i++ {
			keys = append(keys, k)
		}
	}
	return keys
}

// Sign prepends a DKIM-Signature header to the given RFC 5322 message
// body, signed with the supplied private key for "{selector}.{domain}".
//
// Only the headers in signedHeaderCandidates that the message actually
// carries are covered — see signedHeaderKeys.
func Sign(message []byte, domain, selector string, privateKeyDER []byte) ([]byte, error) {
	if domain == "" || selector == "" {
		return nil, fmt.Errorf("dkim: domain and selector required")
	}
	if len(privateKeyDER) == 0 {
		return nil, fmt.Errorf("dkim: empty private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(privateKeyDER)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	opts := &msgauth.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 key,
		HeaderCanonicalization: msgauth.CanonicalizationRelaxed,
		BodyCanonicalization:   msgauth.CanonicalizationRelaxed,
		HeaderKeys:             signedHeaderKeys(message),
	}

	var signed bytes.Buffer
	if err := msgauth.Sign(&signed, bytes.NewReader(message), opts); err != nil {
		return nil, fmt.Errorf("dkim sign: %w", err)
	}
	return signed.Bytes(), nil
}

// DNSRecord renders the TXT record the user must publish. Returns the
// hostname (left of the apex) and the record value.
//
// Example for selector "e2a202605" + domain "mail.acme.com":
//
//	name  = "e2a202605._domainkey.mail.acme.com"
//	value = "v=DKIM1; k=rsa; p=MIIBIjANBgkq..."
func DNSRecord(selector, domain, publicKeyDNS string) (string, string) {
	name := fmt.Sprintf("%s._domainkey.%s", selector, domain)
	value := fmt.Sprintf("v=DKIM1; k=rsa; p=%s", publicKeyDNS)
	return name, value
}

// ExtractPublicKeyFromTXT pulls the "p=" payload out of a TXT record's
// raw value, trimming any whitespace mail systems sometimes inject when
// splitting the record across multiple strings. Returns "" if the
// payload is missing — callers treat that as "key not yet published".
func ExtractPublicKeyFromTXT(txt string) string {
	const marker = "p="
	i := strings.Index(txt, marker)
	if i < 0 {
		return ""
	}
	tail := txt[i+len(marker):]
	// "p=" must be the last tag in the record per RFC 6376 §3.6.1, but
	// we still defensively cut at the next ";" in case operators
	// reorder tags. Whitespace is stripped because TXT records longer
	// than 255 chars get split with quoted segments — joiners may
	// introduce stray spaces.
	if j := strings.Index(tail, ";"); j >= 0 {
		tail = tail[:j]
	}
	return strings.Join(strings.Fields(tail), "")
}
