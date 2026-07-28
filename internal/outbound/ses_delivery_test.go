package outbound

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/dkim"
)

const (
	sesDeliveryDate      = "Tue, 28 Jul 2026 05:00:00 +0000"
	sesDeliveryMessageID = "<010f019fa635d175-0000@us-east-2.amazonses.com>"
)

// The production failure this reproduces, and the reason it needs both the
// composer and the signer fixes in one tree.
//
// A real send from a production agent was delivered with three DKIM
// signatures, and e2a's own was the one that always failed:
//
//	dkim=pass  d=<domain>       s=e2a...     (SES BYODKIM)
//	dkim=pass  d=amazonses.com
//	dkim=fail  d=<domain>       s=e2a...     reason="signature verification failed"
//
// Two independent mutations can produce that, and they break different
// halves of the signature:
//
//   - SES stamps its own Message-ID. If h= claims Message-ID while the
//     submitted message has none, adding it breaks the HEADER hash.
//     Fixed in internal/dkim (sign only headers actually present).
//   - A relay wrapping an over-length line inserts a break the relaxed
//     body canonicalisation cannot absorb, breaking the BODY hash.
//     Fixed in internal/outbound (encode over-length lines).
//
// Neither package can test the pair on its own, so this is the only place
// the whole delivery path is asserted. Both halves must hold at once: a
// message can fail on either hash, and fixing one hides nothing about the
// other.

// sesDeliver applies the SES-owned header and transport transformations that
// can affect e2a's pre-SES DKIM signature.
func sesDeliver(t *testing.T, raw []byte) []byte {
	t.Helper()
	normalised := mtaNormaliseLineEndings(t, raw)
	wrapped, _ := mtaHardWrap(t, normalised)
	// SES replaces Date and assigns Message-ID. The composer deliberately sends
	// no Message-ID, but it does send Date so non-SES relays receive valid mail.
	delivered := replaceHeaderForDelivery(t, wrapped, "Date", sesDeliveryDate)
	return append([]byte("Message-ID: "+sesDeliveryMessageID+"\r\n"), delivered...)
}

func replaceHeaderForDelivery(t *testing.T, raw []byte, name, value string) []byte {
	t.Helper()
	needle := []byte("\r\n" + name + ": ")
	start := bytes.Index(raw, needle)
	if start < 0 {
		t.Fatalf("message has no %s header for SES to replace", name)
	}
	start += 2
	endRel := bytes.Index(raw[start:], []byte("\r\n"))
	if endRel < 0 {
		t.Fatalf("%s header has no line ending", name)
	}
	end := start + endRel
	replacement := []byte(name + ": " + value)
	if bytes.Equal(raw[start:end], replacement) {
		t.Fatalf("replacement %s header equals the submitted value; mutation is inert", name)
	}
	out := make([]byte, 0, len(raw)-end+start+len(replacement))
	out = append(out, raw[:start]...)
	out = append(out, replacement...)
	out = append(out, raw[end:]...)
	return out
}

func parsedHeaders(t *testing.T, raw []byte) mail.Header {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message headers: %v", err)
	}
	return msg.Header
}

func TestSESDeliverAppliesModeledMutations(t *testing.T) {
	const submittedDate = "Fri, 22 May 2026 12:00:00 +0000"
	raw := []byte("From: bot@example.com\r\n" +
		"To: alice@elsewhere.test\r\n" +
		"Date: " + submittedDate + "\r\n\r\n" +
		"first bare-LF line\n" + strings.Repeat("a", 1500) + "\nlast line\n")

	delivered := sesDeliver(t, raw)
	headers := parsedHeaders(t, delivered)
	if got := headers.Get("Date"); got != sesDeliveryDate {
		t.Errorf("delivered Date = %q, want SES value %q", got, sesDeliveryDate)
	}
	if got := headers.Get("Message-ID"); got != sesDeliveryMessageID {
		t.Errorf("delivered Message-ID = %q, want SES value %q", got, sesDeliveryMessageID)
	}

	separator := bytes.Index(delivered, []byte("\r\n\r\n"))
	if separator < 0 {
		t.Fatal("delivered message has no header/body separator")
	}
	body := delivered[separator+4:]
	for i, b := range body {
		if b == '\n' && (i == 0 || body[i-1] != '\r') {
			t.Fatalf("body still contains a bare LF at byte %d", i)
		}
	}
	for i, line := range bytes.Split(body, []byte("\r\n")) {
		if len(line) > maxLineOctets {
			t.Errorf("delivered body line %d is %d octets, exceeds %d", i, len(line), maxLineOctets)
		}
	}
}

func TestSignatureSurvivesFullSESDelivery(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	// The body of the message that actually went out and exposed this,
	// reduced to its structural features: ASCII prose, bare LF line
	// breaks, blank lines, a bare URL.
	productionShapedBody := "Hi David,\n\n" +
		"I'm Jace, an AI cofounder at Token Canopy. This email is the pitch - " +
		"it's coming from my own verified address, not a human's.\n\n" +
		"Grab 20 minutes:\nhttps://calendar.app.google/w4dKDbrazzVpUa8n8\n\n" +
		"Jace\n"

	cases := []struct {
		name string
		req  SendRequest
	}{
		{"production-shaped ascii body", SendRequest{
			To:      []string{"david@read.test"},
			Subject: "Giving Read agents their own email address",
			Body:    productionShapedBody,
		}},
		{"em dash in the body", SendRequest{
			To:      []string{"alice@elsewhere.test"},
			Subject: "Subject line",
			Body:    "It's GA today — stable /v1 API.\nSecond line.\n",
		}},
		{"over-length ascii line", SendRequest{
			To:      []string{"alice@elsewhere.test"},
			Subject: "Subject line",
			Body:    "intro\n" + strings.Repeat("a", 4000) + "\noutro\n",
		}},
		{"html and text with attachment", SendRequest{
			To:       []string{"alice@elsewhere.test"},
			Subject:  "Subject — line",
			Body:     "Plain — text\nsecond line\n",
			HTMLBody: "<p>HTML — body</p>",
			Attachments: []Attachment{{
				Filename: "report.txt", ContentType: "text/plain", Data: strings.Repeat("aGk=", 500),
			}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signed := composeViaSender(t, kp, tc.req)

			// Preconditions: both fixes must actually be in effect, so a
			// pass cannot come from the mutations being inapplicable.
			headers := parsedHeaders(t, signed)
			if got := headers.Get("Message-ID"); got != "" {
				t.Fatalf("composer emitted Message-ID %q; the SES-stamp scenario no longer applies", got)
			}
			if got := headers.Get("Date"); got == "" {
				t.Fatal("composer emitted no Date for SES to replace")
			}
			if err := verifyDKIM(t, signed, kp); err != nil {
				t.Fatalf("signature invalid before delivery: %v", err)
			}

			if err := verifyDKIM(t, sesDeliver(t, signed), kp); err != nil {
				t.Errorf("signature failed after full SES delivery: %v", err)
			}
		})
	}
}

// The header/body hazards must demonstrably break an unprotected signature.
// TestSESDeliverAppliesModeledMutations separately pins that sesDeliver invokes
// every modeled transformation rather than silently becoming a no-op.
func TestSESDeliveryMutationsAreLoadBearing(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	const headers = "From: bot@example.com\r\nTo: alice@elsewhere.test\r\n" +
		"Subject: hi\r\nDate: Fri, 22 May 2026 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n\r\n"

	t.Run("over-length line breaks the body hash", func(t *testing.T) {
		signed, err := dkim.Sign([]byte(headers+strings.Repeat("a", 1500)+"\r\n"),
			dkimTestDomain, kp.Selector, kp.PrivateKeyDER)
		if err != nil {
			t.Fatal(err)
		}
		mutated, didWrap := mtaHardWrap(t, signed)
		if !didWrap {
			t.Fatal("wrap simulation did not fire on a 1500-octet line")
		}
		if err := verifyDKIM(t, mutated, kp); err == nil {
			t.Error("expected the wrap to break the body hash; the composer fix is not load-bearing here")
		}
	})

	t.Run("added Message-ID breaks the header hash when oversigned", func(t *testing.T) {
		// Force the pre-fix behaviour by signing a message that DOES carry
		// a Message-ID, then having the relay rewrite it - the same header
		// hash mismatch oversigning produced.
		signed, err := dkim.Sign([]byte("Message-ID: <ours@example.com>\r\n"+headers+"body\r\n"),
			dkimTestDomain, kp.Selector, kp.PrivateKeyDER)
		if err != nil {
			t.Fatal(err)
		}
		mutated := bytes.Replace(signed,
			[]byte("Message-ID: <ours@example.com>"),
			[]byte("Message-ID: <ses@us-east-2.amazonses.com>"), 1)
		if bytes.Equal(mutated, signed) {
			t.Fatal("Message-ID rewrite did not apply; test is inert")
		}
		if err := verifyDKIM(t, mutated, kp); err == nil {
			t.Error("expected a rewritten signed Message-ID to break the header hash")
		}
	})
}
