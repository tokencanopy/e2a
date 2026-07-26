package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/filterquery"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

type qFixture struct {
	key          string
	sender       string
	subject      string
	labels       []string
	created      time.Time
	attachments  int
	conversation string
	id           string
}

func seedQAgent(t *testing.T, store *identity.Store, ctx context.Context) string {
	t.Helper()
	const domain = "qdiff.example.com"
	user, err := store.CreateOrGetUser(ctx, "owner-qdiff@example.com", "Owner", "google-qdiff")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return agent.ID
}

func seedQFixtures(t *testing.T, pool *pgxpool.Pool, store *identity.Store, agentID string) map[string]qFixture {
	t.Helper()
	ctx := context.Background()
	day := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	fixtures := []qFixture{
		{key: "before", sender: "before@example.com", subject: "xnowy", labels: []string{"archive"}, created: day.Add(-time.Nanosecond)},
		{key: "start", sender: "start@example.com", subject: "100 percent", labels: []string{"start"}, created: day},
		{key: "alice", sender: "alice@corp.com", subject: "Quarterly report", labels: []string{"urgent", "q3"}, created: day.Add(time.Hour), conversation: "qdiff-conversation"},
		{key: "alert", sender: "bob@alerts.io", subject: "CPU alert", labels: []string{"alerts"}, created: day.Add(2 * time.Hour), attachments: 2},
		{key: "newsletter", sender: "carol@news.net", subject: "Weekly digest", labels: []string{"newsletter"}, created: day.Add(3 * time.Hour)},
		{key: "follow", sender: "ALICE@corp.com", subject: "Follow-up", labels: []string{"follow-up"}, created: day.Add(4 * time.Hour)},
		{key: "empty", sender: "dave@x.com", subject: "", labels: []string{}, created: day.Add(5 * time.Hour)},
		{key: "literal", sender: "eve@percent.com", subject: "100% sure _now_ slash\\path", labels: []string{"urgent"}, created: day.Add(6 * time.Hour)},
		{key: "wildcard", sender: "frank@star.com", subject: "a*b literal", labels: []string{}, created: day.Add(7 * time.Hour)},
		{key: "unicode", sender: "日本語@例.jp", subject: "こんにちは 世界", labels: []string{"urgent", "日本"}, created: day.Add(8 * time.Hour)},
		{key: "end", sender: "end@example.com", subject: "At next day", labels: []string{"end"}, created: day.AddDate(0, 0, 1)},
		{key: "after", sender: "after@example.com", subject: "After next day", labels: []string{"after"}, created: day.AddDate(0, 0, 1).Add(time.Microsecond)},
	}

	for i := range fixtures {
		fx := &fixtures[i]
		message, err := store.CreateInboundMessage(ctx, "", agentID, fx.sender, "bot@qdiff.example.com",
			fmt.Sprintf("<qdiff-%s@example.com>", fx.key), fx.subject, fx.conversation, "", []byte("From: "+fx.sender+"\r\nSubject: "+fx.subject+"\r\n\r\nx"),
			nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
		if err != nil {
			t.Fatalf("seed %s: %v", fx.key, err)
		}
		fx.id = message.ID

		attachmentsValue := make([]map[string]any, fx.attachments)
		for attachment := range attachmentsValue {
			attachmentsValue[attachment] = map[string]any{
				"filename":     fmt.Sprintf("attachment-%d.pdf", attachment),
				"content_type": "application/pdf",
				"index":        attachment,
				"size_bytes":   10,
			}
		}
		attachments, err := json.Marshal(attachmentsValue)
		if err != nil {
			t.Fatalf("marshal attachments for %s: %v", fx.key, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE messages SET labels = $1, attachments_json = $2::jsonb, created_at = $3 WHERE id = $4`, fx.labels, attachments, fx.created, fx.id); err != nil {
			t.Fatalf("set fixture %s: %v", fx.key, err)
		}
	}

	byKey := make(map[string]qFixture, len(fixtures))
	for _, fx := range fixtures {
		byKey[fx.key] = fx
	}
	return byKey
}

func fixtureIDs(t *testing.T, byKey map[string]qFixture, keys ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		fx, ok := byKey[key]
		if !ok {
			t.Fatalf("unknown fixture key %q", key)
		}
		ids = append(ids, fx.id)
	}
	sort.Strings(ids)
	return ids
}

func listedIDs(messages []identity.Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestQFilterDifferential(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := seedQAgent(t, store, ctx)
	byKey := seedQFixtures(t, pool, store, agentID)

	allDay := []string{"start", "alice", "alert", "newsletter", "follow", "empty", "literal", "wildcard", "unicode"}
	allButUrgent := []string{"before", "start", "alert", "newsletter", "follow", "empty", "wildcard", "end", "after"}
	allButAlice := []string{"before", "start", "alert", "newsletter", "empty", "literal", "wildcard", "unicode", "end", "after"}
	allAfterStart := append(append([]string{}, allDay...), "end", "after")
	allOutsideDay := []string{"before", "end", "after"}
	queries := []struct {
		q    string
		keys []string
	}{
		{q: `label:urgent`, keys: []string{"alice", "literal", "unicode"}},
		{q: `label:urgent OR label:alerts`, keys: []string{"alice", "alert", "literal", "unicode"}},
		{q: `label:urgent OR label:alerts AND has:attachment`, keys: []string{"alert"}},
		{q: `(label:urgent OR label:follow-up) AND NOT has:attachment`, keys: []string{"alice", "follow", "literal", "unicode"}},
		{q: `NOT label:urgent`, keys: allButUrgent},
		{q: `from:alice`, keys: []string{"alice", "follow"}},
		{q: `from:*@corp.com`, keys: []string{"alice", "follow"}},
		{q: `from = "alice@corp.com"`, keys: []string{"alice", "follow"}},
		{q: `from != "alice@corp.com"`, keys: allButAlice},
		{q: `subject:100%`, keys: []string{"literal"}},
		{q: `subject:_now_`, keys: []string{"literal"}},
		{q: `subject:"\\path"`, keys: []string{"literal"}},
		{q: `subject:"a*b literal"`, keys: []string{"wildcard"}},
		{q: `subject:こんにちは`, keys: []string{"unicode"}},
		{q: `has:attachment`, keys: []string{"alert"}},
		{q: `created<2026-07-01`, keys: []string{"before"}},
		{q: `created<=2026-07-01`, keys: append([]string{"before"}, allDay...)},
		{q: `created=2026-07-01`, keys: allDay},
		{q: `created!=2026-07-01`, keys: allOutsideDay},
		{q: `created>2026-07-01`, keys: []string{"end", "after"}},
		{q: `created>=2026-07-01`, keys: allAfterStart},
	}

	for _, tc := range queries {
		t.Run(tc.q, func(t *testing.T) {
			expr, err := filterquery.Parse(tc.q, identity.MessagesQRegistry())
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.q, err)
			}
			messages, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
				AgentID: agentID, Direction: "inbound", Status: "all", Limit: 100, Q: expr,
			})
			if err != nil {
				t.Fatalf("GetMessagesByAgent(%q): %v", tc.q, err)
			}
			if got, want := listedIDs(messages), fixtureIDs(t, byKey, tc.keys...); !reflect.DeepEqual(got, want) {
				t.Errorf("q=%q IDs=%v, want %v", tc.q, got, want)
			}
		})
	}
}

func TestQFilterComposesAfterFlatFilters(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := seedQAgent(t, store, ctx)
	byKey := seedQFixtures(t, pool, store, agentID)
	expr, err := filterquery.Parse(`from:corp.com`, identity.MessagesQRegistry())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	day := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	messages, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID: agentID, Direction: "inbound", Status: "all", Limit: 100,
		From: "alice", SubjectContains: "quarter", ConversationID: "qdiff-conversation",
		Since: day, Until: day.AddDate(0, 0, 1), Labels: []string{"urgent"}, Q: expr,
	})
	if err != nil {
		t.Fatalf("GetMessagesByAgent: %v", err)
	}
	if got, want := listedIDs(messages), fixtureIDs(t, byKey, "alice"); !reflect.DeepEqual(got, want) {
		t.Errorf("IDs=%v, want %v", got, want)
	}
}

func TestQFilterPropagatesEmissionErrorBeforeQuery(t *testing.T) {
	sentinel := errors.New("emit failure")
	registry, err := filterquery.NewRegistry(filterquery.FieldSpec{
		Name: "boom", Ops: []string{":"},
		Coerce: func(raw string, quoted bool) (any, error) { return raw, nil },
		Emit:   func(*filterquery.Comparison, *filterquery.EmitCtx) (string, error) { return "", sentinel },
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	expr, err := filterquery.Parse(`boom:value`, registry)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store := identity.NewStore(nil)
	_, err = store.GetMessagesByAgent(context.Background(), identity.MessageListFilter{AgentID: "agent_not_queried", Q: expr})
	if !errors.Is(err, sentinel) {
		t.Fatalf("GetMessagesByAgent error = %v, want wrapped %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "emit q filter") {
		t.Errorf("error = %q, want q-emission context", err)
	}
}
