package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExportMessageScheduledAtIsDocumentedAndMarked closes the drift where the
// account export's Message record gained scheduled_at with neither a
// description nor a stability marker, while its MessageView/MessageSummaryView
// siblings carried both — so the export alone presented a beta field as stable.
func TestExportMessageScheduledAtIsDocumentedAndMarked(t *testing.T) {
	schemas := renderedSchemas(t)
	for _, name := range []string{"Message", "MessageView", "MessageSummaryView"} {
		property, ok := schemas[name]["properties"].(map[string]any)["scheduled_at"].(map[string]any)
		if !ok {
			t.Errorf("%s.scheduled_at is missing", name)
			continue
		}
		if got := property["x-stability-level"]; got != "beta" {
			t.Errorf("%s.scheduled_at x-stability-level = %#v, want beta", name, got)
		}
		description, _ := property["description"].(string)
		if !strings.HasPrefix(description, "Beta: scheduled sending may change") {
			t.Errorf("%s.scheduled_at description = %q, want the shared beta lead-in", name, description)
		}
		if got := property["format"]; got != "date-time" {
			t.Errorf("%s.scheduled_at format = %#v, want date-time", name, got)
		}
	}
}

// TestExportMessageOmitsThreadID pins the deliberate asymmetry the audit
// flagged. thread_id is a server-owned read projection of e2a's mailbox-local
// reply topology, not a fact about the user's data — the same reasoning that
// already keeps it out of stored events and webhook payloads. Adding it to a
// GDPR Art. 15 dump would export our internal graph, so the export omits it on
// purpose and this test is the record of that choice.
func TestExportMessageOmitsThreadID(t *testing.T) {
	schemas := renderedSchemas(t)
	properties, _ := schemas["Message"]["properties"].(map[string]any)
	if _, present := properties["thread_id"]; present {
		t.Error("the account-export Message record must not carry thread_id — it is a server-owned projection, not the account's data")
	}
	for _, name := range []string{"MessageView", "MessageSummaryView"} {
		if _, present := schemas[name]["properties"].(map[string]any)["thread_id"]; !present {
			t.Errorf("%s.thread_id is missing — the live read views DO expose the beta projection", name)
		}
	}
}

// TestOutreachOperationsAllStateAgentScope closes the one description gap in
// the outreach quartet: all four route through resolveOutreachAgent →
// requireAgentAccess, so all four must say so. deleteEngagement did not, which
// read as if un-enrolling were account-only.
func TestOutreachOperationsAllStateAgentScope(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Description string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	want := map[string]bool{
		"listEngagements": false, "getEngagement": false,
		"upsertEngagement": false, "deleteEngagement": false,
	}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if _, tracked := want[op.OperationID]; !tracked {
				continue
			}
			want[op.OperationID] = true
			if !strings.Contains(op.Description, "Agent-scoped credentials may") {
				t.Errorf("%s does not state its agent-scoped access, though the handler permits it", op.OperationID)
			}
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("operation %s was not found in the document", id)
		}
	}
}

func renderedSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	return doc.Components.Schemas
}
