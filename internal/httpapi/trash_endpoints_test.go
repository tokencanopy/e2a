package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

var errUnauthorizedTest = errors.New("unauthorized")

// trashCalls records which trash dep a request landed on, so tests can assert
// the soft/permanent routing.
type trashCalls struct {
	softMsg, purgeMsg, restoreMsg     int
	softAgent, hardAgent, restoreAg   int
	lastMessageID, lastMessageAgent   string
	lastAgentID                       string
	lastListDeleted, lastListDeleted2 bool
}

// withTrashDeps wires happy-path fakes for every trash dep, recording calls.
func withTrashDeps(c *trashCalls) func(*Deps) {
	return func(d *Deps) {
		d.DeleteMessage = func(ctx context.Context, messageID, agentID string) error {
			c.softMsg++
			c.lastMessageID, c.lastMessageAgent = messageID, agentID
			return nil
		}
		d.PurgeMessage = func(ctx context.Context, messageID, agentID string) error {
			c.purgeMsg++
			c.lastMessageID, c.lastMessageAgent = messageID, agentID
			return nil
		}
		// Restore answers with the message view itself (the store builds it
		// inside the restore transaction), so the fake returns the row.
		d.RestoreMessage = func(ctx context.Context, messageID, agentID string) (*identity.Message, error) {
			c.restoreMsg++
			return &identity.Message{
				ID: messageID, AgentID: agentID, Direction: "inbound",
				Sender: "alice@gmail.com", Subject: "restored",
				ThreadID:  assignedThreadID,
				CreatedAt: time.Unix(1700000000, 0).UTC(),
			}, nil
		}
		d.DeleteAgent = func(ctx context.Context, agentID, userID string) error {
			c.softAgent++
			c.lastAgentID = agentID
			return nil
		}
		d.PermanentDeleteAgent = func(ctx context.Context, agentID, userID string) (int64, error) {
			c.hardAgent++
			c.lastAgentID = agentID
			return 4, nil
		}
		// Restore answers with the LIVE agent the store read inside the
		// restore transaction — hence sampleAgent (no deleted_at).
		d.RestoreAgent = func(ctx context.Context, agentID, userID string) (*identity.AgentIdentity, error) {
			c.restoreAg++
			a := sampleAgent()
			return &a, nil
		}
	}
}

// trashedSampleAgent is sampleAgent moved to the trash.
func trashedSampleAgent() identity.AgentIdentity {
	a := sampleAgent()
	dt := time.Unix(1700001000, 0).UTC()
	a.DeletedAt = &dt
	return a
}

func TestDeleteMessageMovesToTrash(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c))
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if body["deleted"] != true || body["id"] != "msg_1" {
		t.Fatalf("unexpected body %v", body)
	}
	if c.softMsg != 1 || c.purgeMsg != 0 {
		t.Fatalf("soft=%d purge=%d, want soft delete only", c.softMsg, c.purgeMsg)
	}
	if c.lastMessageID != "msg_1" || c.lastMessageAgent != "support@acme.com" {
		t.Fatalf("dep got (%q, %q)", c.lastMessageID, c.lastMessageAgent)
	}
}

func TestDeleteMessageHeldConflict(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.DeleteMessage = func(ctx context.Context, messageID, agentID string) error {
			return identity.ErrMessageHeld
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_held", "good", nil)
	if code != 409 || errCode(body) != "message_held" {
		t.Fatalf("want 409 message_held, got %d %v", code, body)
	}
}

func TestDeleteMessageNotFound(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.DeleteMessage = func(ctx context.Context, messageID, agentID string) error {
			return identity.ErrMessageNotFound
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_gone", "good", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("want 404 not_found, got %d %v", code, body)
	}
}

func TestDeleteMessagePermanentRequiresConfirm(t *testing.T) {
	// Missing/wrong confirm with permanent=true is the same caller mistake the
	// declarative DeleteConfirm guard rejects on the schema-required delete
	// ops, so it must carry the identical error contract: 422 invalid_request.
	for name, query := range map[string]string{
		"missing":    "?permanent=true",
		"wrong case": "?permanent=true&confirm=delete",
	} {
		t.Run(name, func(t *testing.T) {
			var c trashCalls
			srv := testServer(t, withTrashDeps(&c))
			code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1"+query, "good", nil)
			if code != 422 || errCode(body) != "invalid_request" {
				t.Fatalf("want 422 invalid_request, got %d %v", code, body)
			}
			if c.purgeMsg != 0 || c.softMsg != 0 {
				t.Fatal("no dep should be reached without confirm=DELETE")
			}
		})
	}
}

// TestDeleteMessagePermanentConfirmPrecedesScopeAndLookup: the confirm guard
// answers first — matching the declarative delete ops, where Huma validates
// the schema-required confirm before the handler runs, i.e. before any scope
// check or resource resolution. The 422 discloses only public spec knowledge,
// never resource existence or the caller's scope standing.
func TestDeleteMessagePermanentConfirmPrecedesScopeAndLookup(t *testing.T) {
	// Nonexistent agent/message: 422, not 404 — the guard runs before
	// agent/message resolution.
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c))
	code, body := sendJSON(t, "DELETE",
		srv.URL+"/v1/agents/ghost%40acme.com/messages/msg_ghost?permanent=true", "good", nil)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("missing confirm on nonexistent target: want 422 invalid_request, got %d %v", code, body)
	}

	// Agent-scoped credential: 422, not 403 — the guard runs before the
	// account-scope check, same as Huma's schema validation would.
	srv2 := testServer(t, withTrashDeps(&c), func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			if r.Header.Get("Authorization") == "Bearer agentkey" {
				return &identity.Principal{
					User:    &identity.User{ID: "u_1", Email: "owner@acme.com"},
					Scope:   identity.ScopeAgent,
					AgentID: "support@acme.com",
				}, nil
			}
			return nil, errUnauthorizedTest
		}
	})
	code, body = sendJSON(t, "DELETE",
		srv2.URL+"/v1/agents/support%40acme.com/messages/msg_1?permanent=true", "agentkey", nil)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("missing confirm with agent-scoped key: want 422 invalid_request, got %d %v", code, body)
	}
	if c.purgeMsg != 0 || c.softMsg != 0 {
		t.Fatal("no dep should be reached without confirm=DELETE")
	}
}

func TestDeleteMessagePermanent(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c))
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1?permanent=true&confirm=DELETE", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if body["deleted"] != true || body["id"] != "msg_1" {
		t.Fatalf("unexpected body %v", body)
	}
	if c.purgeMsg != 1 || c.softMsg != 0 {
		t.Fatalf("soft=%d purge=%d, want purge only", c.softMsg, c.purgeMsg)
	}
}

func TestDeleteMessagePermanentNotInTrash(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.PurgeMessage = func(ctx context.Context, messageID, agentID string) error {
			return identity.ErrNotInTrash
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_live?permanent=true&confirm=DELETE", "good", nil)
	if code != 409 || errCode(body) != "not_in_trash" {
		t.Fatalf("want 409 not_in_trash, got %d %v", code, body)
	}
}

func TestDeleteMessagePermanentSendInProgress(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.PurgeMessage = func(ctx context.Context, messageID, agentID string) error {
			return identity.ErrSendInProgress
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_sending?permanent=true&confirm=DELETE", "good", nil)
	if code != 409 || errCode(body) != "send_in_progress" {
		t.Fatalf("want 409 send_in_progress, got %d %v", code, body)
	}
}

func TestDeleteMessageSoftSendInProgress(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.DeleteMessage = func(ctx context.Context, messageID, agentID string) error {
			return identity.ErrSendInProgress
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_sending", "good", nil)
	if code != 409 || errCode(body) != "send_in_progress" {
		t.Fatalf("want 409 send_in_progress, got %d %v", code, body)
	}
}

func TestRestoreMessage(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c))
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1/restore", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if c.restoreMsg != 1 {
		t.Fatalf("restore called %d times", c.restoreMsg)
	}
	if body["id"] != "msg_1" || body["subject"] != "restored" {
		t.Fatalf("unexpected restored view %v", body)
	}
	if _, present := body["deleted_at"]; present {
		t.Fatalf("restored view must omit deleted_at, got %v", body["deleted_at"])
	}
	if _, present := body["thread_id"]; present {
		t.Fatalf("restore response must omit thread_id, got %v", body["thread_id"])
	}
}

func TestRestoreMessageNotInTrash(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.RestoreMessage = func(ctx context.Context, messageID, agentID string) (*identity.Message, error) {
			return nil, identity.ErrNotInTrash
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/messages/msg_live/restore", "good", nil)
	if code != 409 || errCode(body) != "not_in_trash" {
		t.Fatalf("want 409 not_in_trash, got %d %v", code, body)
	}
}

func TestGetTrashedMessageRemainsDirectlyReadable(t *testing.T) {
	deletedAt := time.Unix(1700001000, 0).UTC()
	srv := testServer(t, func(d *Deps) {
		d.GetMessage = func(ctx context.Context, messageID, agentID string) (*identity.Message, error) {
			return &identity.Message{
				ID: messageID, AgentID: agentID, Direction: "inbound",
				Sender: "alice@gmail.com", Subject: "trashed",
				CreatedAt: time.Unix(1700000000, 0).UTC(), DeletedAt: &deletedAt,
			}, nil
		}
	})
	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/messages/msg_trashed", "good", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200 for direct GET of trashed message, got %d %v", code, body)
	}
	if body["id"] != "msg_trashed" || body["deleted_at"] == nil {
		t.Fatalf("trashed direct-read must include id and deleted_at, got %v", body)
	}
}

func TestGetMessageDocumentsTrashVisibility(t *testing.T) {
	op := New(Deps{}).API.OpenAPI().Paths["/v1/agents/{email}/messages/{id}"].Get
	if op == nil {
		t.Fatal("getMessage operation missing")
	}
	description := strings.ToLower(op.Description)
	for _, required := range []string{"direct get", "deleted_at", "ordinary lists", "reply", "purged"} {
		if !strings.Contains(description, required) {
			t.Errorf("getMessage description must document %q in trash visibility contract: %q", required, op.Description)
		}
	}
}

func TestMessageTrashLifecycleFreezesVisibilityAndReplyTargets(t *testing.T) {
	deleted := false
	deliverCalls := 0
	deletedAt := time.Unix(1700001000, 0).UTC()
	message := func() identity.Message {
		msg := identity.Message{
			ID: "msg_1", AgentID: "support@acme.com", Direction: "inbound",
			Sender: "alice@example.com", Recipient: "support@acme.com",
			Subject: "Help", ConversationID: "conv_1",
			RawMessage: []byte("From: alice@example.com\r\nTo: support@acme.com\r\nSubject: Help\r\n\r\nhello"),
			CreatedAt:  time.Unix(1700000000, 0).UTC(),
		}
		if deleted {
			msg.DeletedAt = &deletedAt
		}
		return msg
	}

	srv := testServer(t, func(d *Deps) {
		d.DeleteMessage = func(ctx context.Context, messageID, agentID string) error {
			if messageID != "msg_1" || agentID != "support@acme.com" {
				return identity.ErrMessageNotFound
			}
			deleted = true
			return nil
		}
		d.ListMessages = func(ctx context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			if f.Deleted != deleted {
				return nil, nil
			}
			return []identity.Message{message()}, nil
		}
		d.GetMessage = func(ctx context.Context, messageID, agentID string) (*identity.Message, error) {
			msg := message()
			return &msg, nil // direct GET is intentionally any-state
		}
		d.GetConversation = func(ctx context.Context, agentID, conversationID string) (*identity.ConversationDetail, error) {
			detail := &identity.ConversationDetail{ConversationSummary: identity.ConversationSummary{ID: conversationID}}
			if !deleted {
				detail.Messages = []identity.Message{message()}
			}
			return detail, nil
		}
		d.GetRepliableMessage = func(ctx context.Context, messageID string) (*identity.Message, error) {
			if deleted {
				return nil, identity.ErrMessageNotFound
			}
			msg := message()
			return &msg, nil
		}
		d.DeliverOutbound = func(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, msgType, replyToEmailMessageID string, referenced *identity.Message, idemCompleteTx agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			deliverCalls++
			return &agent.OutboundResult{MessageID: "unexpected"}, nil
		}
	})

	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1", "good", nil)
	if code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("soft delete: want 200 deletion receipt, got %d %v", code, body)
	}

	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/messages", "good", nil)
	if code != http.StatusOK || len(body["items"].([]any)) != 0 {
		t.Fatalf("ordinary list must hide trashed message, got %d %v", code, body)
	}
	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/messages?deleted=true", "good", nil)
	items := body["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["deleted_at"] == nil {
		t.Fatalf("trash list must expose deleted_at, got %d %v", code, body)
	}
	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1", "good", nil)
	if code != http.StatusOK || body["deleted_at"] == nil {
		t.Fatalf("direct GET must keep trashed message readable, got %d %v", code, body)
	}
	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/conversations/conv_1", "good", nil)
	if code != http.StatusOK || len(body["messages"].([]any)) != 0 {
		t.Fatalf("conversation must hide trashed message, got %d %v", code, body)
	}

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{path: "/reply", body: map[string]any{"text": "reply"}},
		{path: "/forward", body: map[string]any{"to": []string{"bob@example.com"}, "text": "forward"}},
	} {
		code, body = sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1"+tc.path, "good", tc.body)
		if code != http.StatusNotFound || errCode(body) != "not_found" {
			t.Errorf("%s must reject trashed target with 404 not_found, got %d %v", tc.path, code, body)
		}
	}
	if deliverCalls != 0 {
		t.Fatalf("reply/forward invoked delivery %d times for trashed target", deliverCalls)
	}
}

// TestDeleteAgentDefaultIsSoft pins the routing: a plain confirmed delete
// lands on the SOFT dep, never the permanent one.
func TestDeleteAgentDefaultIsSoft(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c))
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com?confirm=DELETE", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if body["messages_deleted"] != float64(0) {
		t.Fatalf("want messages_deleted:0, got %v", body)
	}
	if c.softAgent != 1 || c.hardAgent != 0 {
		t.Fatalf("soft=%d hard=%d, want soft only", c.softAgent, c.hardAgent)
	}
}

// TestDeleteAgentPermanentFromTrash: permanent=true resolves via the
// any-state getter, so an agent already in the trash can be purged.
func TestDeleteAgentPermanentFromTrash(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c), func(d *Deps) {
		// The live getter no longer sees the agent…
		d.GetAgent = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return nil, identity.ErrMessageNotFound
		}
		// …but the any-state getter does.
		d.GetAgentAnyState = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if address == "support@acme.com" {
				a := trashedSampleAgent()
				return &a, nil
			}
			return nil, identity.ErrMessageNotFound
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com?confirm=DELETE&permanent=true", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if body["messages_deleted"] != float64(4) {
		t.Fatalf("want messages_deleted:4, got %v", body)
	}
	if c.hardAgent != 1 || c.softAgent != 0 {
		t.Fatalf("soft=%d hard=%d, want hard only", c.softAgent, c.hardAgent)
	}
}

func TestDeleteAgentPermanentSendInProgress(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.PermanentDeleteAgent = func(ctx context.Context, agentID, userID string) (int64, error) {
			return 0, identity.ErrSendInProgress
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com?confirm=DELETE&permanent=true", "good", nil)
	if code != 409 || errCode(body) != "send_in_progress" {
		t.Fatalf("want 409 send_in_progress, got %d %v", code, body)
	}
}

func TestRestoreAgent(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c), func(d *Deps) {
		d.GetAgentAnyState = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			a := trashedSampleAgent()
			return &a, nil
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/restore", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if c.restoreAg != 1 {
		t.Fatalf("restore called %d times", c.restoreAg)
	}
	// The response is the store's LIVE in-transaction read (sampleAgent), so
	// no deleted_at.
	if body["email"] != "support@acme.com" {
		t.Fatalf("unexpected restored agent %v", body)
	}
	if _, present := body["deleted_at"]; present {
		t.Fatalf("restored agent must omit deleted_at, got %v", body)
	}
}

func TestRestoreAgentNotInTrash(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		// The store is the source of truth for the trash-state decision: a
		// live agent's restore returns ErrNotInTrash.
		d.RestoreAgent = func(ctx context.Context, agentID, userID string) (*identity.AgentIdentity, error) {
			return nil, identity.ErrNotInTrash
		}
		d.GetAgentAnyState = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			a := sampleAgent() // live — DeletedAt nil
			return &a, nil
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/restore", "good", nil)
	if code != 409 || errCode(body) != "not_in_trash" {
		t.Fatalf("want 409 not_in_trash, got %d %v", code, body)
	}
}

// TestListAgentsDeletedView: ?deleted=true routes to the trash lister and the
// rows carry deleted_at.
func TestListAgentsDeletedView(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.ListDeletedAgents = func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error) {
			return []identity.AgentIdentity{trashedSampleAgent()}, nil
		}
	})
	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents?deleted=true", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want the one trashed agent", body)
	}
	row, _ := items[0].(map[string]any)
	if row["deleted_at"] == nil || row["deleted_at"] == "" {
		t.Fatalf("trash row missing deleted_at: %v", row)
	}
	// And the live list still routes to ListAgents (no deleted_at rows).
	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents", "good", nil)
	if code != 200 {
		t.Fatalf("live list: want 200, got %d %v", code, body)
	}
	items, _ = body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("live items = %v", body)
	}
	row, _ = items[0].(map[string]any)
	if _, present := row["deleted_at"]; present {
		t.Fatalf("live row must omit deleted_at: %v", row)
	}
}

// TestListMessagesDeletedView: deleted=true reaches the store filter with
// Deleted=true and trash-view defaults (direction=all, status=all).
func TestListMessagesDeletedView(t *testing.T) {
	var got identity.MessageListFilter
	srv := testServer(t, func(d *Deps) {
		d.ListMessages = func(ctx context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			got = f
			dt := time.Unix(1700001000, 0).UTC()
			return []identity.Message{{
				ID: "msg_t", AgentID: f.AgentID, Direction: "inbound",
				Sender: "alice@gmail.com", Subject: "trashed",
				CreatedAt: time.Unix(1700000000, 0).UTC(), DeletedAt: &dt,
			}}, nil
		}
	})
	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/messages?deleted=true", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	if !got.Deleted {
		t.Error("filter.Deleted not set")
	}
	if got.Direction != "all" || got.Status != "all" {
		t.Errorf("trash-view defaults = (%q, %q), want (all, all)", got.Direction, got.Status)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body)
	}
	row, _ := items[0].(map[string]any)
	if row["deleted_at"] == nil {
		t.Fatalf("trash row missing deleted_at: %v", row)
	}
}

// TestDeleteMessagePermanentIsAccountOnly: the irreversible message purge is
// barred for agent-scoped credentials — a leaked/injected agent key must not
// destroy inbox evidence beyond recovery. The reversible trash stays open to
// the agent (its own inbox hygiene).
func TestDeleteMessagePermanentIsAccountOnly(t *testing.T) {
	var c trashCalls
	srv := testServer(t, withTrashDeps(&c), func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			if r.Header.Get("Authorization") == "Bearer agentkey" {
				return &identity.Principal{
					User:    &identity.User{ID: "u_1", Email: "owner@acme.com"},
					Scope:   identity.ScopeAgent,
					AgentID: "support@acme.com",
				}, nil
			}
			return nil, errUnauthorizedTest
		}
	})
	code, body := sendJSON(t, "DELETE",
		srv.URL+"/v1/agents/support%40acme.com/messages/msg_1?permanent=true&confirm=DELETE", "agentkey", nil)
	if code != 403 {
		t.Fatalf("permanent purge by agent-scoped key: want 403, got %d %v", code, body)
	}
	if c.purgeMsg != 0 {
		t.Fatal("purge dep reached despite scope rejection")
	}
	// The reversible soft delete on its own agent still works.
	code, body = sendJSON(t, "DELETE",
		srv.URL+"/v1/agents/support%40acme.com/messages/msg_1", "agentkey", nil)
	if code != 200 {
		t.Fatalf("soft delete by agent-scoped key on own agent: want 200, got %d %v", code, body)
	}
	if body["deleted"] != true || body["id"] != "msg_1" {
		t.Fatalf("unexpected body %v", body)
	}
}

// TestListAgentsCursorBoundToView: a cursor minted for the live agents list
// must not continue a trash (?deleted=true) listing, and vice versa.
func TestListAgentsCursorBoundToView(t *testing.T) {
	srv := testServer(t)
	liveCursor, err := EncodeCursor("", keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "a@acme.com"})
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents?deleted=true&cursor="+liveCursor, "good", nil)
	if code != 400 || errCode(body) != "invalid_cursor" {
		t.Fatalf("live cursor on trash view: want 400 invalid_cursor, got %d %v", code, body)
	}
	trashCursor, err := EncodeCursor("", keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "a@acme.com", Deleted: true})
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	code, body = sendJSON(t, "GET", srv.URL+"/v1/agents?cursor="+trashCursor, "good", nil)
	if code != 400 || errCode(body) != "invalid_cursor" {
		t.Fatalf("trash cursor on live view: want 400 invalid_cursor, got %d %v", code, body)
	}
}

// TestAgentScopedCannotManageAgentTrash: the hard scope ceiling — an
// agent-scoped credential must not restore (or permanently delete) even its
// own agent.
func TestAgentScopedCannotManageAgentTrash(t *testing.T) {
	srv := testServer(t, withTrashDeps(&trashCalls{}), func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			if r.Header.Get("Authorization") == "Bearer agentkey" {
				return &identity.Principal{
					User:    &identity.User{ID: "u_1", Email: "owner@acme.com"},
					Scope:   identity.ScopeAgent,
					AgentID: "support@acme.com",
				}, nil
			}
			return nil, errUnauthorizedTest
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/restore", "agentkey", nil)
	if code != 403 {
		t.Fatalf("restore by agent-scoped key: want 403, got %d %v", code, body)
	}
	code, body = sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com?confirm=DELETE&permanent=true", "agentkey", nil)
	if code != 403 {
		t.Fatalf("permanent delete by agent-scoped key: want 403, got %d %v", code, body)
	}
}

// TestCreateAgentTrashedAddressConflict covers the address-reserved rule
// (docs/design/trash-soft-delete.md): soft-delete keeps the trashed row's PK,
// so recreating a trashed inbox's address must 409 with a pointer to the
// caller's trash (restore / permanently delete) — not the generic conflict.
func TestCreateAgentTrashedAddressConflict(t *testing.T) {
	var createCalled string
	srv := testServer(t, func(d *Deps) {
		d.CreateAgent = func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error) {
			createCalled = email
			return nil, &pgconn.PgError{Code: "23505", Message: "duplicate key value"}
		}
		trashed := trashedSampleAgent() // support@acme.com, owned by u_1
		d.GetAgentAnyState = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &trashed, nil
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents", "good", map[string]any{
		"email": "support@acme.com", "name": "Bot",
	})
	if code != 409 || errCode(body) != "address_in_trash" {
		t.Fatalf("want 409 address_in_trash, got %d %v", code, body)
	}
	if createCalled != "support@acme.com" {
		t.Fatalf("CreateAgent not invoked with the trashed address; got %q", createCalled)
	}
}

// TestCreateAgentTrashedByOtherUserIsAgentTaken: a trashed inbox owned by
// ANOTHER account (only reachable on the shared domain) must fall back to the
// standard duplicate-agent conflict — never "in your trash", so we don't
// reveal another account's trashed inbox.
func TestCreateAgentTrashedByOtherUserIsAgentTaken(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.CreateAgent = func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error) {
			return nil, &pgconn.PgError{Code: "23505", Message: "duplicate key value"}
		}
		other := trashedSampleAgent()
		other.UserID = "u_someone_else"
		d.GetAgentAnyState = func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &other, nil
		}
	})
	code, body := sendJSON(t, "POST", srv.URL+"/v1/agents", "good", map[string]any{
		"email": "support@acme.com", "name": "Bot",
	})
	if code != 409 || errCode(body) != "agent_taken" {
		t.Fatalf("want 409 agent_taken (not address_in_trash), got %d %v", code, body)
	}
}
