package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// fakeInboundLookup is a stub that satisfies hitlParentLookup so we can
// drive attachReferencesChain without a live Postgres. Each test sets up
// the fields it cares about; everything else is zero-valued.
type fakeInboundLookup struct {
	calledWith struct {
		agentID        string
		emailMessageID string
	}
	wantAgentID   string // when non-empty, t.Errorf if a different agent is queried
	returnInbound *identity.Message
	returnErr     error
}

func (f *fakeInboundLookup) GetMessageByEmailMessageID(_ context.Context, agentID, emailMessageID string) (*identity.Message, error) {
	f.calledWith.agentID = agentID
	f.calledWith.emailMessageID = emailMessageID
	return f.returnInbound, f.returnErr
}

func TestAttachReferencesChain_NoReplyToMessageID(t *testing.T) {
	// /send (not a reply): nothing to do, lookup must not happen.
	lookup := &fakeInboundLookup{}
	req := outbound.SendRequest{To: []string{"a@host"}}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	if lookup.calledWith.agentID != "" {
		t.Errorf("lookup invoked unexpectedly: agentID=%q", lookup.calledWith.agentID)
	}
	if req.References != nil {
		t.Errorf("References = %v, want nil for non-reply", req.References)
	}
}

func TestAttachReferencesChain_HappyPath_BuildsChain(t *testing.T) {
	// HITL approval: parent inbound has a prior References chain, the
	// rebuilt chain must include both prior IDs and the parent's own ID.
	parentRaw := []byte("References: <u1@gmail> <a1@e2a>\r\nFrom: u@x\r\n\r\nbody")
	lookup := &fakeInboundLookup{
		returnInbound: &identity.Message{RawMessage: parentRaw},
	}
	req := outbound.SendRequest{ReplyToMessageID: "<parent@e2a>"}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	if lookup.calledWith.agentID != "agent-1" {
		t.Errorf("agentID = %q, want agent-1", lookup.calledWith.agentID)
	}
	if lookup.calledWith.emailMessageID != "<parent@e2a>" {
		t.Errorf("emailMessageID = %q, want <parent@e2a>", lookup.calledWith.emailMessageID)
	}
	want := []string{"<u1@gmail>", "<a1@e2a>", "<parent@e2a>"}
	if !reflect.DeepEqual(req.References, want) {
		t.Errorf("References = %v, want %v", req.References, want)
	}
}

func TestAttachReferencesChain_InboundExpired_FallsBack(t *testing.T) {
	// Parent inbound row has expired (TTL elapsed) — lookup returns
	// ErrNoRows. We must NOT fail the send: References stays nil and
	// the compose layer falls back to single-id legacy behavior.
	lookup := &fakeInboundLookup{returnErr: errors.New("sql: no rows in result set")}
	req := outbound.SendRequest{ReplyToMessageID: "<parent@e2a>"}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	if req.References != nil {
		t.Errorf("References = %v, want nil (fall back to single-id on lookup failure)", req.References)
	}
}

func TestAttachReferencesChain_InboundMissingNoErr(t *testing.T) {
	// Defensive: lookup returns (nil, nil). Treat the same as expired —
	// don't crash, fall back to legacy.
	lookup := &fakeInboundLookup{}
	req := outbound.SendRequest{ReplyToMessageID: "<parent@e2a>"}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	if req.References != nil {
		t.Errorf("References = %v, want nil", req.References)
	}
}

func TestAttachReferencesChain_OutboundParent_BuildsChain(t *testing.T) {
	// A held reply whose parent is an OUTBOUND message the agent sent
	// (reply-to-own-message). The direction-agnostic lookup resolves it and
	// the References chain is rebuilt exactly as for an inbound parent —
	// previously this no-op'd because the lookup filtered direction='inbound'.
	parentRaw := []byte("References: <orig@x>\r\nFrom: agent@e2a\r\nTo: bob@x\r\n\r\nsent body")
	lookup := &fakeInboundLookup{
		returnInbound: &identity.Message{Direction: "outbound", RawMessage: parentRaw},
	}
	req := outbound.SendRequest{ReplyToMessageID: "<sent@e2a>"}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	want := []string{"<orig@x>", "<sent@e2a>"}
	if !reflect.DeepEqual(req.References, want) {
		t.Errorf("References = %v, want %v", req.References, want)
	}
}

func TestAttachReferencesChain_TopOfThread(t *testing.T) {
	// Parent inbound has no prior References / In-Reply-To (top of
	// thread). Rebuilt chain is just the parent's own Message-ID.
	parentRaw := []byte("From: u@x\r\nSubject: Hi\r\n\r\nbody")
	lookup := &fakeInboundLookup{
		returnInbound: &identity.Message{RawMessage: parentRaw},
	}
	req := outbound.SendRequest{ReplyToMessageID: "<u1@gmail>"}

	attachReferencesChain(context.Background(), lookup, "agent-1", &req)

	want := []string{"<u1@gmail>"}
	if !reflect.DeepEqual(req.References, want) {
		t.Errorf("References = %v, want %v", req.References, want)
	}
}
