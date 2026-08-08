package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

const assignedThreadID = "thr_0123456789abcdef0123456789abcdef"

func TestMessageReadHandlersProjectThreadIDOnlyWhenAssigned(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.ListMessages = func(context.Context, identity.MessageListFilter) ([]identity.Message, error) {
			return []identity.Message{
				{
					ID: "msg_threaded", Direction: "inbound", Recipient: "support@acme.com",
					Subject: "threaded", InboxStatus: "unread", ThreadID: assignedThreadID,
					CreatedAt: time.Unix(1700000200, 0).UTC(),
				},
				{
					ID: "msg_legacy", Direction: "inbound", Recipient: "support@acme.com",
					Subject: "legacy", InboxStatus: "unread",
					CreatedAt: time.Unix(1700000100, 0).UTC(),
				},
			}, nil
		}
		d.GetMessage = func(_ context.Context, messageID, agentID string) (*identity.Message, error) {
			msg := &identity.Message{
				ID: messageID, AgentID: agentID, Direction: "inbound",
				Recipient: agentID, Subject: messageID, InboxStatus: "unread",
				CreatedAt: time.Unix(1700000200, 0).UTC(),
			}
			if messageID == "msg_threaded" {
				msg.ThreadID = assignedThreadID
			}
			return msg, nil
		}
	})

	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages?read_status=all", "good")
	if code != 200 {
		t.Fatalf("list status %d body %v", code, body)
	}
	items := body["items"].([]any)
	threaded := items[0].(map[string]any)
	if threaded["thread_id"] != assignedThreadID {
		t.Fatalf("threaded list item thread_id = %v, want %q", threaded["thread_id"], assignedThreadID)
	}
	legacy := items[1].(map[string]any)
	if _, present := legacy["thread_id"]; present {
		t.Fatalf("legacy list item must omit thread_id, got %v", legacy["thread_id"])
	}

	code, body = getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_threaded", "good")
	if code != 200 || body["thread_id"] != assignedThreadID {
		t.Fatalf("threaded detail = status %d body %v", code, body)
	}
	code, body = getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_legacy", "good")
	if code != 200 {
		t.Fatalf("legacy detail status %d body %v", code, body)
	}
	if _, present := body["thread_id"]; present {
		t.Fatalf("legacy detail must omit thread_id, got %v", body["thread_id"])
	}
}

func TestSharedMessageConvertersDoNotProjectThreadID(t *testing.T) {
	msg := identity.Message{
		ID: "msg_shared", Direction: "inbound", ThreadID: assignedThreadID,
		CreatedAt: time.Unix(1700000200, 0).UTC(),
	}
	for name, view := range map[string]any{
		"summary": messageSummaryFromIdentity(msg),
		"detail":  messageViewFromIdentity(&msg),
	} {
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("%s unmarshal: %v", name, err)
		}
		if _, present := wire["thread_id"]; present {
			t.Fatalf("%s shared converter must omit thread_id, got %v", name, wire["thread_id"])
		}
	}
}

func TestConversationProjectionDoesNotPopulateThreadID(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.GetConversation = func(context.Context, string, string) (*identity.ConversationDetail, error) {
			return &identity.ConversationDetail{
				ConversationSummary: identity.ConversationSummary{
					ID: "conv_1", MessageCount: 1,
					FirstMessageAt: time.Unix(1700000200, 0).UTC(),
					LastMessageAt:  time.Unix(1700000200, 0).UTC(),
				},
				Messages: []identity.Message{{
					ID: "msg_threaded", Direction: "inbound", ThreadID: assignedThreadID,
					CreatedAt: time.Unix(1700000200, 0).UTC(),
				}},
			}, nil
		}
	})

	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/conversations/conv_1", "good")
	if code != 200 {
		t.Fatalf("status %d body %v", code, body)
	}
	message := body["messages"].([]any)[0].(map[string]any)
	if _, present := message["thread_id"]; present {
		t.Fatalf("conversation message must omit thread_id, got %v", message["thread_id"])
	}
}

func TestThreadIDIsNotCallerWritable(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(
		t,
		"POST",
		srv.URL+"/v1/agents/support%40acme.com/messages",
		"good",
		map[string]any{
			"to":        []string{"recipient@example.net"},
			"subject":   "not caller owned",
			"text":      "hello",
			"thread_id": assignedThreadID,
		},
	)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("caller-supplied thread_id: want 422 invalid_request, got %d %v", code, body)
	}
}

func TestThreadIDOpenAPIContract(t *testing.T) {
	doc := renderSpec(t)
	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)

	for _, schemaName := range []string{"MessageSummaryView", "MessageView"} {
		schema := schemas[schemaName].(map[string]any)
		properties := schemaProps(t, doc, schemaName)
		property := properties["thread_id"].(map[string]any)
		if property["x-stability-level"] != "beta" {
			t.Errorf("%s.thread_id stability = %v, want beta", schemaName, property["x-stability-level"])
		}
		if description, _ := property["description"].(string); !strings.Contains(description, "Beta:") {
			t.Errorf("%s.thread_id description must explain beta status, got %q", schemaName, description)
		}
		if schema["x-stability-level"] != nil {
			t.Errorf("%s must remain stable, got schema stability %v", schemaName, schema["x-stability-level"])
		}
		required, _ := schema["required"].([]any)
		for _, name := range required {
			if name == "thread_id" {
				t.Errorf("%s.thread_id must remain optional", schemaName)
			}
		}
		for _, internalField := range []string{"thread_parent_id", "rfc_message_id_key"} {
			if _, present := properties[internalField]; present {
				t.Errorf("%s must not expose internal field %s", schemaName, internalField)
			}
		}
	}

	paths := doc["paths"].(map[string]any)
	for path := range paths {
		if strings.Contains(path, "/threads") {
			t.Errorf("unexpected thread endpoint %q", path)
		}
	}
	listOperation := paths["/v1/agents/{email}/messages"].(map[string]any)["get"].(map[string]any)
	if listOperation["x-stability-level"] != nil {
		t.Errorf("listMessages operation must remain stable, got %v", listOperation["x-stability-level"])
	}
	for _, raw := range listOperation["parameters"].([]any) {
		parameter := raw.(map[string]any)
		if parameter["name"] == "thread_id" {
			t.Error("listMessages must not add a thread_id filter")
		}
	}
	detailOperation := paths["/v1/agents/{email}/messages/{id}"].(map[string]any)["get"].(map[string]any)
	if detailOperation["x-stability-level"] != nil {
		t.Errorf("getMessage operation must remain stable, got %v", detailOperation["x-stability-level"])
	}

	requestSet, _ := specReachability(t, doc)
	var containsThreadProperty func(any) bool
	containsThreadProperty = func(node any) bool {
		switch value := node.(type) {
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				if _, present := properties["thread_id"]; present {
					return true
				}
			}
			for _, child := range value {
				if containsThreadProperty(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if containsThreadProperty(child) {
					return true
				}
			}
		}
		return false
	}
	for schemaName := range requestSet {
		if containsThreadProperty(schemas[schemaName]) {
			t.Errorf("request-reachable schema %s must not accept thread_id", schemaName)
		}
	}

	for _, schemaName := range []string{
		"SendResultView",
		"EmailReceivedData",
		"EmailSentData",
		"EmailFailedData",
		"EmailDeliveredData",
		"EmailBouncedData",
		"EmailComplainedData",
		"Message",
	} {
		properties := schemaProps(t, doc, schemaName)
		for _, field := range []string{"thread_id", "thread_parent_id", "rfc_message_id_key"} {
			if _, present := properties[field]; present {
				t.Errorf("%s must not expose internal field %s", schemaName, field)
			}
		}
	}
}
