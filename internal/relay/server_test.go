package relay

import (
	"reflect"
	"testing"
)

func TestExtractEmail(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"alice@example.com", "alice@example.com"},
		{"<alice@example.com>", "alice@example.com"},
		{`"Alice Smith" <alice@example.com>`, "alice@example.com"},
		{"not-an-email", "not-an-email"},
	}

	for _, tt := range tests {
		got := extractEmail(tt.input)
		if got != tt.expected {
			t.Errorf("extractEmail(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"alice@example.com", "example.com"},
		{"<bob@sub.domain.org>", "sub.domain.org"},
		{`"Name" <test@foo.bar>`, "foo.bar"},
		{"no-at-sign", ""},
	}

	for _, tt := range tests {
		got := extractDomain(tt.input)
		if got != tt.expected {
			t.Errorf("extractDomain(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractThreadInfo(t *testing.T) {
	raw := []byte("Message-Id: <abc123@gmail.com>\r\nSubject: Hello Agent\r\nFrom: alice@example.com\r\nTo: bot@agent.example.com\r\n\r\nHi there\r\n")

	info := extractThreadInfo(raw)

	if info.MessageID != "<abc123@gmail.com>" {
		t.Errorf("MessageID = %q, want %q", info.MessageID, "<abc123@gmail.com>")
	}
	if info.Subject != "Hello Agent" {
		t.Errorf("Subject = %q, want %q", info.Subject, "Hello Agent")
	}
}

func TestExtractThreadInfoWithReplyHeaders(t *testing.T) {
	raw := []byte("Message-Id: <reply@gmail.com>\r\nSubject: Re: Hello\r\nIn-Reply-To: <orig@agent.com>\r\nReferences: <first@agent.com> <orig@agent.com>\r\nFrom: alice@example.com\r\nTo: bot@agent.example.com\r\n\r\nReply body\r\n")

	info := extractThreadInfo(raw)

	if info.InReplyTo != "<orig@agent.com>" {
		t.Errorf("InReplyTo = %q, want %q", info.InReplyTo, "<orig@agent.com>")
	}
	if len(info.References) != 2 {
		t.Fatalf("References count = %d, want 2", len(info.References))
	}
	if info.References[0] != "<first@agent.com>" {
		t.Errorf("References[0] = %q, want %q", info.References[0], "<first@agent.com>")
	}
}

func TestExtractThreadInfoReportsMalformedThreadHeaders(t *testing.T) {
	raw := []byte("In-Reply-To: not-a-message-id\r\nReferences: phrase-without-a-token\r\nFrom: alice@example.com\r\n\r\nbody\r\n")

	info := extractThreadInfo(raw)

	if len(info.MalformedThreadHeaders) != 2 || info.MalformedThreadHeaders[0] != "in_reply_to" || info.MalformedThreadHeaders[1] != "references" {
		t.Fatalf("MalformedThreadHeaders = %v, want [in_reply_to references]", info.MalformedThreadHeaders)
	}
	if info.ThreadInReplyTo != nil || info.ThreadReferences != nil {
		t.Fatalf("malformed headers must fail closed, got in_reply_to=%v references=%v", info.ThreadInReplyTo, info.ThreadReferences)
	}
}

func TestExtractThreadInfoReplyTo(t *testing.T) {
	// Outbound messages from the e2a relay set From: to the platform alias and
	// Reply-To: to the real agent address. Inbound should surface Reply-To.
	raw := []byte("Message-Id: <x@send.e2a.dev>\r\nSubject: e2e ping\r\nFrom: \"Alice via e2a\" <agent@send.e2a.dev>\r\nReply-To: test-alice@agent.mnexa.ai\r\nTo: test-bob@agent.mnexa.ai\r\n\r\nHi Bob\r\n")

	info := extractThreadInfo(raw)

	if info.From != "agent@send.e2a.dev" {
		t.Errorf("From = %q, want agent@send.e2a.dev", info.From)
	}
	if !reflect.DeepEqual(info.ReplyTo, []string{"test-alice@agent.mnexa.ai"}) {
		t.Errorf("ReplyTo = %v, want [test-alice@agent.mnexa.ai]", info.ReplyTo)
	}
}

func TestExtractThreadInfoRejectsAmbiguousFrom(t *testing.T) {
	for _, raw := range []string{
		"From: ceo@trusted.example\r\nFrom: signed@trusted.example\r\n\r\nbody",
		"From: ceo@trusted.example, signed@trusted.example\r\n\r\nbody",
	} {
		if got := extractThreadInfo([]byte(raw)).From; got != "" {
			t.Fatalf("From for ambiguous header = %q, want empty", got)
		}
	}
}

func TestExtractThreadInfoNoReplyTo(t *testing.T) {
	raw := []byte("Message-Id: <x@gmail.com>\r\nSubject: Hi\r\nFrom: alice@example.com\r\nTo: bot@agent.example.com\r\n\r\nBody\r\n")

	info := extractThreadInfo(raw)

	if info.ReplyTo != nil {
		t.Errorf("ReplyTo should be nil, got %v", info.ReplyTo)
	}
}

func TestExtractThreadInfoReplyToMultiAddress(t *testing.T) {
	// RFC 5322 § 3.6.2 permits Reply-To to carry multiple addresses; SDK
	// consumers receive the full list so they can fan replies out themselves.
	raw := []byte("Message-Id: <x@example.com>\r\nSubject: Hi\r\n" +
		"From: notifications@mail.example.com\r\n" +
		"Reply-To: \"Alice\" <alice@example.com>, ceo@company.com\r\n" +
		"To: bot@agent.example.com\r\n\r\nBody\r\n")

	info := extractThreadInfo(raw)

	want := []string{"alice@example.com", "ceo@company.com"}
	if !reflect.DeepEqual(info.ReplyTo, want) {
		t.Errorf("ReplyTo = %v, want %v", info.ReplyTo, want)
	}
}

func TestExtractThreadInfoToCcLists(t *testing.T) {
	raw := []byte("Message-Id: <m@gmail.com>\r\nSubject: Group\r\nFrom: alice@example.com\r\n" +
		"To: \"Bot A\" <bot-a@example.com>, bot-b@example.com\r\n" +
		"Cc: watcher@example.com\r\n\r\nbody\r\n")

	info := extractThreadInfo(raw)

	wantTo := []string{"bot-a@example.com", "bot-b@example.com"}
	if !reflect.DeepEqual(info.To, wantTo) {
		t.Errorf("To = %v, want %v", info.To, wantTo)
	}
	if !reflect.DeepEqual(info.CC, []string{"watcher@example.com"}) {
		t.Errorf("CC = %v, want [watcher@example.com]", info.CC)
	}
}

func TestExtractAddressListEmptyAndMalformed(t *testing.T) {
	if got := extractAddressList(""); got != nil {
		t.Errorf("empty header should return nil, got %v", got)
	}
	if got := extractAddressList("not-an-address"); got != nil {
		t.Errorf("malformed header should return nil, got %v", got)
	}
}

func TestExtractThreadInfoConversationID(t *testing.T) {
	raw := []byte("Message-Id: <x@send.e2a.dev>\r\nSubject: e2e ping\r\nFrom: agent@send.e2a.dev\r\nReply-To: alice@agent.mnexa.ai\r\nTo: bob@agent.mnexa.ai\r\nX-E2A-Conversation-ID: 081158ac-bf25-4eb6-a6b0-02828ec670c3\r\n\r\nHi Bob\r\n")

	info := extractThreadInfo(raw)

	if info.ConversationID != "081158ac-bf25-4eb6-a6b0-02828ec670c3" {
		t.Errorf("ConversationID = %q, want the UUID", info.ConversationID)
	}
}

func TestEnvelopeFromTrusted(t *testing.T) {
	srv := &Server{outboundFromDomain: "send.e2a.dev"}

	cases := []struct {
		envelope string
		want     bool
	}{
		{"agent@send.e2a.dev", true},
		{"Agent@Send.E2A.Dev", true},           // case-insensitive domain match
		{"bounce-id@mail.send.e2a.dev", true},  // SES MAIL FROM Domain subdomain
		{"random@deep.sub.send.e2a.dev", true}, // deeper subdomain, still ours
		{"evil@attacker.com", false},           // external sender
		{"", false},                            // blank envelope
		{"agent@other-send.e2a.dev", false},    // near-miss — not a subdomain
		{"agent@evilsend.e2a.dev", false},      // suffix-match attack (no dot before)
	}
	for _, c := range cases {
		if got := srv.envelopeFromTrusted(c.envelope); got != c.want {
			t.Errorf("envelopeFromTrusted(from=%q) = %v, want %v", c.envelope, got, c.want)
		}
	}
}

func TestEnvelopeFromTrustedUnconfigured(t *testing.T) {
	// If outboundFromDomain isn't configured, trust nothing — fail closed.
	srv := &Server{outboundFromDomain: ""}
	if srv.envelopeFromTrusted("agent@send.e2a.dev") {
		t.Error("should not trust any envelope when outboundFromDomain is empty")
	}
}

func TestSenderResolvable(t *testing.T) {
	// #299: the shared "via e2a" relay sender (agent@<outboundFromDomain>) carries
	// no per-agent identity, so the inbound gate must treat it as unresolvable.
	srv := &Server{outboundFromDomain: "send.e2a.dev"}
	cases := []struct {
		sender string
		want   bool
	}{
		{"alice@acme.com", true},          // external sender — resolvable
		{"bob@customer-domain.com", true}, // sending-verified agent's own domain
		{"agent@send.e2a.dev", false},     // shared relay collapse — unresolvable
		{"agent@Send.E2A.Dev", false},     // case-insensitive
		{"x@mail.send.e2a.dev", false},    // subdomain of the relay domain
		{"agent@othersend.e2a.dev", true}, // suffix-match attack — not actually a subdomain
		{"nodomain", true},                // garbage; let normal matching/flagging handle it
	}
	for _, c := range cases {
		if got := srv.senderResolvable(c.sender); got != c.want {
			t.Errorf("senderResolvable(%q) = %v, want %v", c.sender, got, c.want)
		}
	}
}

func TestSenderResolvableUnconfigured(t *testing.T) {
	// With no outboundFromDomain there is no shared-relay address to recognize, so
	// every sender is treated as resolvable (no spurious flagging).
	srv := &Server{outboundFromDomain: ""}
	if !srv.senderResolvable("agent@send.e2a.dev") {
		t.Error("unconfigured relay should treat all senders as resolvable")
	}
}

func TestExtractThreadInfoMissingHeaders(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n\r\nBody only\r\n")

	info := extractThreadInfo(raw)

	if info.MessageID != "" {
		t.Errorf("MessageID should be empty, got %q", info.MessageID)
	}
	if info.Subject != "" {
		t.Errorf("Subject should be empty, got %q", info.Subject)
	}
	if info.InReplyTo != "" {
		t.Errorf("InReplyTo should be empty, got %q", info.InReplyTo)
	}
}

func TestExtractThreadInfoMalformed(t *testing.T) {
	info := extractThreadInfo([]byte("not a valid email"))

	if info.MessageID != "" {
		t.Errorf("MessageID should be empty, got %q", info.MessageID)
	}
	if info.Subject != "" {
		t.Errorf("Subject should be empty, got %q", info.Subject)
	}
}

// TestExtractThreadInfoDecodesEncodedSubject covers RFC 2047 encoded-word
// subjects like the ones SES emits for unicode characters. Storing the wire
// form leaked `=?utf-8?q?...?=` into list summaries, dashboards, and the CLI
// inbox — every consumer that didn't independently re-parse raw_message.
func TestExtractThreadInfoDecodesEncodedSubject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "Q-encoded em dash",
			raw:  "Message-Id: <x@send.e2a.dev>\r\nSubject: =?utf-8?q?e2a_MCP_e2e_=E2=80=94_HITL_approve_path?=\r\nFrom: a@b.com\r\nTo: c@d.com\r\n\r\nHi\r\n",
			want: "e2a MCP e2e — HITL approve path",
		},
		{
			name: "B-encoded utf-8",
			raw:  "Message-Id: <x@send.e2a.dev>\r\nSubject: =?utf-8?B?Y2Fmw6k=?=\r\nFrom: a@b.com\r\nTo: c@d.com\r\n\r\nHi\r\n",
			want: "café",
		},
		{
			name: "plain ASCII passes through untouched",
			raw:  "Message-Id: <x@send.e2a.dev>\r\nSubject: Plain ASCII\r\nFrom: a@b.com\r\nTo: c@d.com\r\n\r\nHi\r\n",
			want: "Plain ASCII",
		},
		{
			name: "malformed encoded-word falls back to raw",
			raw:  "Message-Id: <x@send.e2a.dev>\r\nSubject: =?utf-8?q?broken\r\nFrom: a@b.com\r\nTo: c@d.com\r\n\r\nHi\r\n",
			want: "=?utf-8?q?broken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractThreadInfo([]byte(tc.raw)).Subject
			if got != tc.want {
				t.Errorf("Subject = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionReset(t *testing.T) {
	s := &session{
		from:       "alice@example.com",
		recipients: []string{"bot@agent.example.com"},
	}

	s.Reset()

	if s.from != "" {
		t.Errorf("from should be empty after Reset, got %q", s.from)
	}
	if s.recipients != nil {
		t.Errorf("recipients should be nil after Reset, got %v", s.recipients)
	}
}
