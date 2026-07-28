package outbound

import (
	"bytes"
	"mime"
	"net/mail"
	"strings"
	"testing"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/tokencanopy/e2a/internal/dkim"
)

func longestHeaderLine(t *testing.T, raw []byte) int {
	t.Helper()
	head, _, ok := strings.Cut(string(raw), "\r\n\r\n")
	if !ok {
		t.Fatal("no header/body separator")
	}
	worst := 0
	for _, line := range strings.Split(head, "\r\n") {
		if len(line) > worst {
			worst = len(line)
		}
	}
	return worst
}

func manyReferences(n int) []string {
	refs := make([]string, n)
	for i := range refs {
		refs[i] = "<message-" + strings.Repeat("x", 20) + string(rune('a'+i%26)) + "@example.com>"
	}
	return refs
}

func manyRecipients(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "recipient" + strings.Repeat("0", 10) + string(rune('a'+i%26)) + "@elsewhere.test"
	}
	return out
}

// No composed header line may exceed the RFC 5322 limit. A strict relay
// is entitled to reject the whole message over this.
func TestComposedHeadersRespectTheLineLimit(t *testing.T) {
	longSubject := strings.Repeat("これはとても長い件名です", 25)
	refs := manyReferences(50)

	cases := []struct {
		name string
		raw  func() ([]byte, error)
	}{
		{"long q-encoded subject", func() ([]byte, error) {
			return ComposeMessage("agent@bot.example.com", []string{"a@b.test"}, nil,
				longSubject, "body", "text/plain", "", nil, "relay.e2a.dev", "", "")
		}},
		{"deep references chain", func() ([]byte, error) {
			return ComposeMessage("agent@bot.example.com", []string{"a@b.test"}, nil,
				"Re: hi", "body", "text/plain", refs[len(refs)-1], refs, "relay.e2a.dev", "", "")
		}},
		{"many recipients", func() ([]byte, error) {
			return ComposeMessage("agent@bot.example.com", manyRecipients(80), manyRecipients(40),
				"hi", "body", "text/plain", "", nil, "relay.e2a.dev", "", "")
		}},
		{"long subject and references together", func() ([]byte, error) {
			return ComposeMultipartMessage("agent@bot.example.com", manyRecipients(40), nil,
				longSubject, "body", "<p>body</p>", refs[len(refs)-1], refs, "relay.e2a.dev", "", "")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.raw()
			if err != nil {
				t.Fatalf("compose: %v", err)
			}
			if got := longestHeaderLine(t, raw); got > maxHeaderOctets {
				t.Errorf("longest header line = %d octets, exceeds the %d limit", got, maxHeaderOctets)
			}
			if _, err := mail.ReadMessage(strings.NewReader(string(raw))); err != nil {
				t.Errorf("folded message no longer parses: %v", err)
			}
		})
	}
}

// Folding must be transparent: unfolding has to recover the exact value.
func TestFoldedHeadersRoundTrip(t *testing.T) {
	longSubject := strings.Repeat("これはとても長い件名です", 25)
	refs := manyReferences(50)

	raw, err := ComposeMessage("agent@bot.example.com", manyRecipients(80), nil,
		longSubject, "body", "text/plain", refs[len(refs)-1], refs, "relay.e2a.dev", "", "")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	dec := new(mime.WordDecoder)
	gotSubject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if gotSubject != longSubject {
		t.Errorf("subject round trip lost data: got %d runes, want %d",
			len([]rune(gotSubject)), len([]rune(longSubject)))
	}

	gotRefs := strings.Fields(msg.Header.Get("References"))
	if len(gotRefs) != len(refs) {
		t.Fatalf("References round trip = %d ids, want %d", len(gotRefs), len(refs))
	}
	for i := range refs {
		if gotRefs[i] != refs[i] {
			t.Errorf("References[%d] = %q, want %q", i, gotRefs[i], refs[i])
		}
	}

	addrs, err := msg.Header.AddressList("To")
	if err != nil {
		t.Fatalf("parse To: %v", err)
	}
	if len(addrs) != 80 {
		t.Errorf("To round trip = %d addresses, want 80", len(addrs))
	}
}

// Headers that already fit must not be reformatted — the fix targets the
// broken extreme, not every message.
func TestShortHeadersAreUntouched(t *testing.T) {
	raw, err := ComposeMessage("agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"A normal subject", "body", "text/plain", "", nil, "relay.e2a.dev", "", "")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			t.Errorf("a short header was folded unnecessarily:\n%s", head)
		}
	}
}

// Folding happens during composition, which runs before signing, so the
// signature covers the folded form. Relaxed header canonicalisation
// (RFC 6376 § 3.4.2) unfolds and collapses whitespace before hashing, so
// a relay that refolds differently is absorbed too — but that only holds
// if what we emit is well-formed folding in the first place.
func TestFoldedHeadersStillSignAndVerify(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	refs := manyReferences(50)
	raw, err := ComposeMessage("agent@example.com", manyRecipients(80), nil,
		strings.Repeat("これはとても長い件名です", 25), "body", "text/plain",
		refs[len(refs)-1], refs, "relay.e2a.dev", "", "")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	signed, err := dkim.Sign(raw, "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	v, err := msgauth.VerifyWithOptions(bytes.NewReader(signed), &msgauth.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			parts := strings.SplitN(name, "._domainkey.", 2)
			if len(parts) != 2 || parts[0] != kp.Selector || parts[1] != "example.com" {
				return nil, nil
			}
			return []string{"v=DKIM1; k=rsa; p=" + kp.PublicKeyDNS}, nil
		},
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	if len(v) != 1 || v[0].Err != nil {
		t.Errorf("signature over folded headers did not verify: %+v", v)
	}
}

func TestFoldHeaderLine(t *testing.T) {
	t.Run("short line is returned verbatim", func(t *testing.T) {
		line := "Subject: hello there"
		if got := foldHeaderLine(line); got != line {
			t.Errorf("foldHeaderLine = %q, want it unchanged", got)
		}
	})

	t.Run("exactly at the limit is untouched", func(t *testing.T) {
		line := "X-Test: " + strings.Repeat("a b", 400)
		line = line[:maxHeaderOctets]
		if got := foldHeaderLine(line); got != line {
			t.Errorf("a line exactly at the limit was folded")
		}
	})

	t.Run("continuation lines start with a single space", func(t *testing.T) {
		got := foldHeaderLine("References: " + strings.Join(manyReferences(60), " "))
		lines := strings.Split(got, "\r\n")
		if len(lines) < 2 {
			t.Fatal("expected the line to be folded")
		}
		for i, line := range lines[1:] {
			if !strings.HasPrefix(line, " ") {
				t.Errorf("continuation line %d does not begin with WSP: %q", i, line)
			}
			if strings.HasPrefix(line, "  ") {
				t.Errorf("continuation line %d has doubled leading space: %q", i, line)
			}
		}
	})

	t.Run("a value with no foldable space is left alone", func(t *testing.T) {
		// Nothing to fold at: emitting a break mid-token would corrupt it.
		line := "X-Test: " + strings.Repeat("a", 2000)
		if got := foldHeaderLine(line); got != line {
			t.Errorf("unfoldable line was modified:\n%q", got)
		}
	})

	t.Run("never folds inside a quoted string", func(t *testing.T) {
		quoted := `"` + strings.Repeat("word ", 300) + `"`
		got := foldHeaderLine("To: " + quoted + " <a@b.test>, " + strings.Join(manyRecipients(20), ", "))
		for _, line := range strings.Split(got, "\r\n") {
			// A fold inside the quoted display name would leave a line
			// with an odd number of unescaped quotes.
			if strings.Count(line, `"`)%2 != 0 {
				t.Errorf("fold landed inside a quoted string: %q", line)
			}
		}
	})
}
