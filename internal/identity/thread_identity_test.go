package identity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/rfcmessageid"
)

func TestNewThreadID(t *testing.T) {
	t.Parallel()

	first := NewThreadID()
	second := NewThreadID()

	if !IsValidThreadID(first) {
		t.Fatalf("NewThreadID() = %q, want thr_ followed by 32 lowercase hex characters", first)
	}
	if first == second {
		t.Fatalf("NewThreadID() returned the same value twice: %q", first)
	}
}

func TestIsValidThreadID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		id   string
		want bool
	}{
		{name: "valid", id: "thr_0123456789abcdef0123456789abcdef", want: true},
		{name: "empty", id: "", want: false},
		{name: "wrong prefix", id: "conv_0123456789abcdef0123456789abcdef", want: false},
		{name: "too short", id: "thr_0123456789abcdef", want: false},
		{name: "uppercase", id: "thr_0123456789abcdef0123456789abcdeF", want: false},
		{name: "non hex", id: "thr_0123456789abcdef0123456789abcdeg", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsValidThreadID(tc.id); got != tc.want {
				t.Fatalf("IsValidThreadID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestMessageThreadFieldsStayOutOfInternalJSON(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(Message{
		ID:              "msg_1",
		ThreadID:        "thr_0123456789abcdef0123456789abcdef",
		ThreadParentID:  "msg_parent",
		RFCMessageIDKey: "<CaseSensitive.Left@example.com>",
	})
	if err != nil {
		t.Fatalf("Marshal(Message): %v", err)
	}
	for _, forbidden := range []string{"thread_id", "thread_parent_id", "rfc_message_id_key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("internal Message JSON unexpectedly contains %q: %s", forbidden, raw)
		}
	}
}

func TestPrepareInboundThreadCandidatesBoundsRawEvidenceInspection(t *testing.T) {
	t.Parallel()

	field := make([]RFCMessageIDCandidate, rfcmessageid.MaxTokens+1)
	field[0] = RFCMessageIDCandidate{
		Original:  "<outside-budget@example.net>",
		Canonical: "<outside-budget@example.net>",
	}
	for i := 1; i < len(field); i++ {
		field[i] = RFCMessageIDCandidate{
			Original:  "<nearest@example.net>",
			Canonical: "<nearest@example.net>",
		}
	}

	got := prepareInboundThreadCandidates(InboundThreadEvidence{InReplyTo: field})
	if len(got) != 1 {
		t.Fatalf("candidate count = %d, want 1 from the nearest %d raw entries", len(got), rfcmessageid.MaxTokens)
	}
	if got[0].Canonical != "<nearest@example.net>" {
		t.Fatalf("candidate = %q, want nearest raw candidate", got[0].Canonical)
	}
}
