package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// Cursors are signed, so a tampered or garbage cursor was already rejected
// on every list endpoint. What was NOT rejected was a validly-signed cursor
// minted by a DIFFERENT endpoint: every account-level list shares the same
// (created_at, id) payload shape, so a /v1/contacts cursor decoded cleanly
// on /v1/events and was applied as a real keyset anchor. The account scope
// held — no cross-tenant leakage — but the anchor was meaningless, so the
// endpoint returned an arbitrarily truncated page (typically `{"items":[]}`)
// and the client concluded "no events" when there were plenty. A silent
// wrong answer is worse than a 400.
//
// The same wrong-answer class exists across ACCOUNTS: account A's cursor on
// account B's /v1/events verified under the deployment-global secret and
// anchored B's (still hard-scoped) query at a meaningless position. The
// account therefore binds too — via the MAC key (see cursorKey), so the
// rejection is a plain invalid_cursor indistinguishable from a forgery.
//
// These tests assert both axes: every list endpoint rejects every other
// list endpoint's cursor AND every other account's cursor for the same
// endpoint, while still walking its own.

// cursorRow is the shape all the fakes below key on: a keyset position and
// an id, seeded newest-first so each endpoint yields >1 page at limit=1.
type cursorRow struct {
	id string
	at time.Time
}

// cursorRows seeds n rows whose id is EXACTLY the value the endpoint puts in
// its cursor (email for agents, domain for domains, address for
// suppressions, …) — otherwise the fake's keyset predicate would compare a
// bare id against a suffixed one and never advance.
func cursorRows(prefix, suffix string, n int) []cursorRow {
	base := time.Unix(1700000000, 0).UTC()
	out := make([]cursorRow, n)
	for i := 0; i < n; i++ {
		out[i] = cursorRow{
			// Lexically descending in step with recency, matching the
			// (created_at DESC, id DESC) order the real stores use.
			id: fmt.Sprintf("%s_%02d%s", prefix, 90-i, suffix),
			at: base.Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}

// page applies the shared newest-first keyset predicate and the limit.
func page(rows []cursorRow, limit int, afterAt time.Time, afterID string) []cursorRow {
	out := []cursorRow{}
	for _, r := range rows {
		if !afterKey(r.at, r.id, afterAt, afterID) {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Two accounts, each with its own agent, so the matrix can vary the
// principal as well as the endpoint. Bearer "good" is u_1 (as everywhere
// else in this package); bearer "other" is u_2.
const (
	bindingAgentA = "agent-a@acme.com" // owned by u_1
	bindingAgentB = "agent-b@acme.com" // owned by u_2
)

func cursorBindingAuth(r *http.Request) (*identity.User, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer good":
		return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
	case "Bearer other":
		return &identity.User{ID: "u_2", Email: "owner@bee.com"}, nil
	}
	return nil, errors.New("unauthorized")
}

// cursorBindingServer wires every cursor-minting list endpoint with a
// keyset-honoring fake, so each endpoint can mint a REAL cursor through the
// real handler — the foreign cursors under test are genuine API output, not
// hand-built payloads. Account-level fakes serve the same rows to both
// accounts (the binding under test rejects the cursor before any query);
// agent-level fakes key rows on the agent, so each account paginates its
// own agent's data.
func cursorBindingServer(t *testing.T) *httptest.Server {
	t.Helper()
	agents := cursorRows("agt", "@acme.com", 3)
	domains := cursorRows("dom", ".com", 3)
	webhooks := cursorRows("whk", "", 3)
	templates := cursorRows("tpl", "", 3)
	apiKeys := cursorRows("apk", "", 3)
	reviews := cursorRows("rvw", "", 3)
	events := cursorRows("evt", "", 3)
	supps := cursorRows("sup", "@x.com", 3)
	contacts := cursorRows("cnt", "", 3)
	deliveries := cursorRows("dlv", "", 3)
	msgs := map[string][]cursorRow{bindingAgentA: cursorRows("msa", "", 3), bindingAgentB: cursorRows("msb", "", 3)}
	convos := map[string][]cursorRow{bindingAgentA: cursorRows("cva", "", 3), bindingAgentB: cursorRows("cvb", "", 3)}
	engs := map[string][]cursorRow{bindingAgentA: cursorRows("ega", "", 3), bindingAgentB: cursorRows("egb", "", 3)}
	agSupps := map[string][]cursorRow{bindingAgentA: cursorRows("asa", "@x.com", 3), bindingAgentB: cursorRows("asb", "@x.com", 3)}
	lifecycles := map[string][]cursorRow{bindingAgentA: cursorRows("mla", "", 3), bindingAgentB: cursorRows("mlb", "", 3)}

	srv := httptest.NewServer(New(Deps{
		Authenticator: cursorBindingAuth,
		EventsEnabled: true,

		ListAgents: func(_ context.Context, _ string, limit int, at time.Time, id string) ([]identity.AgentIdentity, error) {
			out := []identity.AgentIdentity{}
			for _, r := range page(agents, limit, at, id) {
				out = append(out, identity.AgentIdentity{ID: r.id, Domain: "acme.com", UserID: "u_1", CreatedAt: r.at})
			}
			return out, nil
		},
		ListDomains: func(_ context.Context, _ string, limit int, at time.Time, domain string) ([]identity.Domain, error) {
			out := []identity.Domain{}
			for _, r := range page(domains, limit, at, domain) {
				out = append(out, identity.Domain{Domain: r.id, Verified: true, CreatedAt: r.at})
			}
			return out, nil
		},
		ListWebhooks: func(_ context.Context, _ string, limit int, at time.Time, id string) ([]identity.Webhook, error) {
			out := []identity.Webhook{}
			for _, r := range page(webhooks, limit, at, id) {
				out = append(out, identity.Webhook{ID: r.id, URL: "https://x.com/h", Events: []string{"email.received"}, Enabled: true, CreatedAt: r.at})
			}
			return out, nil
		},
		ListTemplates: func(_ context.Context, _ string, limit int, at time.Time, id string) ([]identity.TemplateSummary, error) {
			out := []identity.TemplateSummary{}
			for _, r := range page(templates, limit, at, id) {
				out = append(out, identity.TemplateSummary{ID: r.id, UserID: "u_1", Name: "n", Alias: r.id, Subject: "s", CreatedAt: r.at, UpdatedAt: r.at})
			}
			return out, nil
		},
		ListAPIKeys: func(_ context.Context, _ string, limit int, at time.Time, id string) ([]identity.APIKey, error) {
			out := []identity.APIKey{}
			for _, r := range page(apiKeys, limit, at, id) {
				out = append(out, identity.APIKey{ID: r.id, UserID: "u_1", Name: "k", KeyPrefix: "e2a_acct_ab", Scope: "account", CreatedAt: r.at})
			}
			return out, nil
		},
		ListReviews: func(_ context.Context, _ string, limit int, at time.Time, id string) ([]identity.ReviewListItem, error) {
			out := []identity.ReviewListItem{}
			for _, r := range page(reviews, limit, at, id) {
				out = append(out, identity.ReviewListItem{
					ID: r.id, AgentID: "support@acme.dev", Direction: "inbound",
					Sender: "s@x.com", To: []string{"support@acme.dev"},
					Subject: "held", Status: "pending_review", CreatedAt: r.at,
				})
			}
			return out, nil
		},
		ListEvents: func(_ context.Context, q EventQuery) ([]agent.EventView, error) {
			out := []agent.EventView{}
			for _, r := range page(events, q.Limit, q.CursorCreatedAt, q.CursorID) {
				out = append(out, agent.EventView{ID: r.id, Type: "email.sent", Status: "delivered", CreatedAt: r.at})
			}
			return out, nil
		},
		ListSuppressions: func(_ context.Context, _ string, limit int, at time.Time, addr string) ([]identity.Suppression, error) {
			out := []identity.Suppression{}
			for _, r := range page(supps, limit, at, addr) {
				out = append(out, identity.Suppression{Address: r.id, Source: "bounce", CreatedAt: r.at})
			}
			return out, nil
		},
		GetContact: func(_ context.Context, _, _ string) (identity.Contact, error) {
			return identity.Contact{}, identity.ErrContactNotFound
		},
		ListContacts: func(_ context.Context, _ string, _ identity.ContactFilter, limit int, at time.Time, id string) ([]identity.Contact, error) {
			out := []identity.Contact{}
			for _, r := range page(contacts, limit, at, id) {
				out = append(out, identity.Contact{ID: r.id, Address: r.id + "@x.com", Source: "manual", CreatedAt: r.at, UpdatedAt: r.at})
			}
			return out, nil
		},
		GetWebhook: func(_ context.Context, id, userID string) (*identity.Webhook, error) {
			return &identity.Webhook{ID: id, UserID: userID}, nil
		},
		ListDeliveries: func(_ context.Context, _, _ string, limit int, at time.Time, id string) ([]webhook.SubscriberDelivery, error) {
			out := []webhook.SubscriberDelivery{}
			for _, r := range page(deliveries, limit, at, id) {
				out = append(out, webhook.SubscriberDelivery{
					ID: r.id, WebhookID: "wh_1", EventType: "email.received",
					Status: "delivered", CreatedAt: r.at,
				})
			}
			return out, nil
		},

		// Agent-parameterized collections: one agent per account.
		GetAgent: func(_ context.Context, address string) (*identity.AgentIdentity, error) {
			switch a := identity.NormalizeEmail(address); a {
			case bindingAgentA:
				return &identity.AgentIdentity{ID: a, Email: a, Domain: "acme.com", UserID: "u_1"}, nil
			case bindingAgentB:
				return &identity.AgentIdentity{ID: a, Email: a, Domain: "acme.com", UserID: "u_2"}, nil
			}
			return nil, identity.ErrAgentNotFound
		},
		ListMessages: func(_ context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			out := []identity.Message{}
			for _, r := range page(msgs[f.AgentID], f.Limit, f.AfterTime, f.AfterID) {
				out = append(out, identity.Message{
					ID: r.id, AgentID: f.AgentID, Direction: "inbound",
					Recipient: f.AgentID, Subject: "s", InboxStatus: "unread", CreatedAt: r.at,
				})
			}
			return out, nil
		},
		ListConversations: func(_ context.Context, f identity.ConversationListFilter) ([]identity.ConversationSummary, error) {
			out := []identity.ConversationSummary{}
			for _, r := range page(convos[f.AgentID], f.Limit, f.AfterLastMessageAt, f.AfterConversationID) {
				out = append(out, identity.ConversationSummary{
					ID: r.id, LastMessageAt: r.at, FirstMessageAt: r.at,
					MessageCount: 1, InboundCount: 1, LatestSubject: "s", LatestSender: "x@x.com",
				})
			}
			return out, nil
		},
		ListEngagements: func(_ context.Context, _, agentID string, _ identity.EngagementFilter, limit int, at time.Time, id string) ([]identity.ContactEngagement, error) {
			out := []identity.ContactEngagement{}
			for _, r := range page(engs[agentID], limit, at, id) {
				out = append(out, identity.ContactEngagement{
					ID: r.id, AgentEmail: agentID, Address: r.id + "@x.com", ContactID: r.id,
					Stage: "active", CreatedAt: r.at, UpdatedAt: r.at,
				})
			}
			return out, nil
		},
		ListAgentSuppressions: func(_ context.Context, _, agentID string, limit int, at time.Time, addr string) ([]identity.AgentSuppression, error) {
			out := []identity.AgentSuppression{}
			for _, r := range page(agSupps[agentID], limit, at, addr) {
				out = append(out, identity.AgentSuppression{AgentEmail: agentID, Address: r.id, Source: "manual", CreatedAt: r.at})
			}
			return out, nil
		},
		ListMessageLifecycle: func(_ context.Context, messageID, agentID string) ([]messagelifecycle.MessageLifecycleTransition, error) {
			out := []messagelifecycle.MessageLifecycleTransition{}
			for _, r := range lifecycles[agentID] {
				out = append(out, messagelifecycle.MessageLifecycleTransition{
					ID: r.id, MessageID: messageID, Direction: "inbound", OccurredAt: r.at,
				})
			}
			return out, nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cursorBoundEndpoints is every cursor-minting collection the binding must
// cover — all sixteen. pathA is account A's instance of the collection and
// pathB account B's; they differ only for the agent-parameterized lists
// (each account walks its own agent). Each yields >1 page at limit=1, so
// `mint` below always returns a real cursor.
var cursorBoundEndpoints = []struct {
	name  string
	pathA string
	pathB string
}{
	{"agents", "/v1/agents?limit=1", "/v1/agents?limit=1"},
	{"domains", "/v1/domains?limit=1", "/v1/domains?limit=1"},
	{"webhooks", "/v1/webhooks?limit=1", "/v1/webhooks?limit=1"},
	{"templates", "/v1/templates?limit=1", "/v1/templates?limit=1"},
	{"api_keys", "/v1/account/api-keys?limit=1", "/v1/account/api-keys?limit=1"},
	{"reviews", "/v1/reviews?limit=1", "/v1/reviews?limit=1"},
	{"events", "/v1/events?limit=1", "/v1/events?limit=1"},
	{"account_suppressions", "/v1/account/suppressions?limit=1", "/v1/account/suppressions?limit=1"},
	{"contacts", "/v1/contacts?limit=1", "/v1/contacts?limit=1"},
	// Both of these were ALSO accepting foreign cursors pre-fix and were
	// missed by the original bug report's list of eight: deliveries with no
	// status filter matched a foreign cursor's empty status, and
	// starter-templates decoded a foreign cursor's absent alias as "" and
	// silently re-served page 1. Verified against a clean origin/main.
	{"webhook_deliveries", "/v1/webhooks/wh_1/deliveries?limit=1", "/v1/webhooks/wh_1/deliveries?limit=1"},
	{"starter_templates", "/v1/starter-templates?limit=1", "/v1/starter-templates?limit=1"},
	// The five agent-scoped collections were already foreign-cursor-safe via
	// their mandatory agent/filter fingerprints, so their presence here is
	// coverage of the blanket rule, not a behavior fix.
	{"messages", "/v1/agents/agent-a%40acme.com/messages?limit=1", "/v1/agents/agent-b%40acme.com/messages?limit=1"},
	{"conversations", "/v1/agents/agent-a%40acme.com/conversations?limit=1", "/v1/agents/agent-b%40acme.com/conversations?limit=1"},
	{"engagements", "/v1/agents/agent-a%40acme.com/contacts?limit=1", "/v1/agents/agent-b%40acme.com/contacts?limit=1"},
	{"agent_suppressions", "/v1/agents/agent-a%40acme.com/suppressions?limit=1", "/v1/agents/agent-b%40acme.com/suppressions?limit=1"},
	{"message_lifecycle", "/v1/agents/agent-a%40acme.com/messages/msg_1/lifecycle?limit=1", "/v1/agents/agent-b%40acme.com/messages/msg_1/lifecycle?limit=1"},
}

// mint drives one real first-page request as `bearer` and returns its
// next_cursor.
func mint(t *testing.T, srv *httptest.Server, path, bearer string) string {
	t.Helper()
	code, body := getJSON(t, srv.URL+path, bearer)
	if code != http.StatusOK {
		t.Fatalf("%s: first page status %d body %v", path, code, body)
	}
	next, ok := body["next_cursor"].(string)
	if !ok || next == "" {
		t.Fatalf("%s: expected a next_cursor on the first page, got %v", path, body["next_cursor"])
	}
	return next
}

// TestCursor_ForeignEndpointCursorRejected is the regression test for the
// bug: a cursor minted by endpoint A, replayed on endpoint B, must be a 400
// invalid_cursor — never a silently-anchored (and usually empty) page.
func TestCursor_ForeignEndpointCursorRejected(t *testing.T) {
	srv := cursorBindingServer(t)

	cursors := make(map[string]string, len(cursorBoundEndpoints))
	for _, ep := range cursorBoundEndpoints {
		cursors[ep.name] = mint(t, srv, ep.pathA, "good")
	}

	for _, target := range cursorBoundEndpoints {
		for _, source := range cursorBoundEndpoints {
			if source.name == target.name {
				continue
			}
			t.Run(source.name+"_cursor_on_"+target.name, func(t *testing.T) {
				u := srv.URL + target.pathA + "&cursor=" + url.QueryEscape(cursors[source.name])
				code, body := getJSON(t, u, "good")
				if code != http.StatusBadRequest {
					t.Fatalf("foreign cursor accepted: status %d body %v", code, body)
				}
				if errCode(body) != "invalid_cursor" {
					t.Fatalf("want code invalid_cursor, got %v", body)
				}
			})
		}
	}
}

// TestCursor_ForeignAccountCursorRejected is the second binding axis: a
// cursor minted by account A, presented by account B on the SAME collection,
// must be a 400 invalid_cursor — pre-fix it verified under the
// deployment-global secret and silently mispositioned B's (still
// account-scoped) query. Both accounts' own cursors keep walking.
func TestCursor_ForeignAccountCursorRejected(t *testing.T) {
	srv := cursorBindingServer(t)
	for _, ep := range cursorBoundEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			cursorA := mint(t, srv, ep.pathA, "good")

			// Baseline: account B's own view of the collection has rows.
			code, body := getJSON(t, srv.URL+ep.pathB, "other")
			if code != http.StatusOK {
				t.Fatalf("baseline for account B: status %d body %v", code, body)
			}
			if items, _ := body["items"].([]any); len(items) == 0 {
				t.Fatalf("baseline for account B returned no rows; the fixture is wrong")
			}

			// A's cursor in B's hands: rejected, indistinguishable from a forgery.
			code, body = getJSON(t, srv.URL+ep.pathB+"&cursor="+url.QueryEscape(cursorA), "other")
			if code != http.StatusBadRequest {
				t.Fatalf("foreign-account cursor accepted: status %d body %v", code, body)
			}
			if errCode(body) != "invalid_cursor" {
				t.Fatalf("want code invalid_cursor, got %v", body)
			}

			// The counterweight: A→A and B→B continuations both stay valid.
			code, body = getJSON(t, srv.URL+ep.pathA+"&cursor="+url.QueryEscape(cursorA), "good")
			if code != http.StatusOK {
				t.Fatalf("account A's own cursor rejected: status %d body %v", code, body)
			}
			cursorB := mint(t, srv, ep.pathB, "other")
			code, body = getJSON(t, srv.URL+ep.pathB+"&cursor="+url.QueryEscape(cursorB), "other")
			if code != http.StatusOK {
				t.Fatalf("account B's own cursor rejected: status %d body %v", code, body)
			}
		})
	}
}

// TestCursor_ContactsCursorOnEventsIsRejected pins the exact live
// reproduction from staging: a contacts cursor replayed on /v1/events used
// to return 200 with an empty items array while the same query without a
// cursor returned rows.
func TestCursor_ContactsCursorOnEventsIsRejected(t *testing.T) {
	srv := cursorBindingServer(t)
	contactsCursor := mint(t, srv, "/v1/contacts?limit=1", "good")

	// Sanity: without a cursor, /v1/events has rows to return.
	code, body := getJSON(t, srv.URL+"/v1/events?limit=3", "good")
	if code != http.StatusOK {
		t.Fatalf("baseline events status %d", code)
	}
	if items, _ := body["items"].([]any); len(items) == 0 {
		t.Fatalf("baseline /v1/events returned no items; the fixture is wrong")
	}

	code, body = getJSON(t, srv.URL+"/v1/events?limit=3&cursor="+url.QueryEscape(contactsCursor), "good")
	if code != http.StatusBadRequest || errCode(body) != "invalid_cursor" {
		t.Fatalf("contacts cursor on /v1/events: want 400 invalid_cursor, got %d %v", code, body)
	}
}

// TestCursor_OwnCursorStillWalks is the counterweight: binding must not
// break normal pagination. Each endpoint's own cursor still advances for
// each account, and page 2 is disjoint from page 1.
func TestCursor_OwnCursorStillWalks(t *testing.T) {
	srv := cursorBindingServer(t)
	for _, ep := range cursorBoundEndpoints {
		for _, acct := range []struct{ bearer, path string }{
			{"good", ep.pathA},
			{"other", ep.pathB},
		} {
			t.Run(ep.name+"_"+acct.bearer, func(t *testing.T) {
				code, first := getJSON(t, srv.URL+acct.path, acct.bearer)
				if code != http.StatusOK {
					t.Fatalf("page 1: status %d body %v", code, first)
				}
				cursor, _ := first["next_cursor"].(string)
				if cursor == "" {
					t.Fatalf("page 1 produced no cursor: %v", first)
				}
				code, second := getJSON(t, srv.URL+acct.path+"&cursor="+url.QueryEscape(cursor), acct.bearer)
				if code != http.StatusOK {
					t.Fatalf("page 2: status %d body %v", code, second)
				}
				firstItems, _ := first["items"].([]any)
				secondItems, _ := second["items"].([]any)
				if len(firstItems) != 1 || len(secondItems) != 1 {
					t.Fatalf("want 1 item per page, got %d and %d", len(firstItems), len(secondItems))
				}
				if fmt.Sprint(firstItems[0]) == fmt.Sprint(secondItems[0]) {
					t.Fatalf("page 2 repeated page 1's row: %v", firstItems[0])
				}
			})
		}
	}
}

// TestCursor_ResourceIsSignedNotJustTagged: a client cannot re-target a
// cursor by rewriting the resource, because the resource lives inside the
// HMAC-covered payload.
func TestCursor_ResourceIsSignedNotJustTagged(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	pos := keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "x_1"}
	asEvents, err := EncodeCursor(secret, "u_1", cursorEvents, pos)
	if err != nil {
		t.Fatal(err)
	}
	asAgents, err := EncodeCursor(secret, "u_1", cursorAgents, pos)
	if err != nil {
		t.Fatal(err)
	}
	if asEvents == asAgents {
		t.Fatal("the resource must change the cursor bytes")
	}
	var out keysetCursor
	if err := DecodeCursor([]string{secret}, "u_1", cursorAgents, asEvents, &out); err != ErrCursorResourceMismatch {
		t.Fatalf("events cursor decoded as agents: %v", err)
	}
	// ErrCursorResourceMismatch must still satisfy the broad sentinel so
	// callers that only branch on ErrInvalidCursor keep working.
	if !errors.Is(ErrCursorResourceMismatch, ErrInvalidCursor) {
		t.Fatal("ErrCursorResourceMismatch must wrap ErrInvalidCursor")
	}
	if err := DecodeCursor([]string{secret}, "u_1", cursorEvents, asEvents, &out); err != nil {
		t.Fatalf("own-resource decode: %v", err)
	}
	if out != pos {
		t.Fatalf("round-trip mismatch: %+v want %+v", out, pos)
	}
}

// TestCursor_AccountBindsViaMACKeyNotEnvelope pins the shape of the account
// binding: the account changes the MAC (so a foreign account's cursor fails
// verification as a plain ErrInvalidCursor, indistinguishable from a
// forgery) but never appears in the envelope — the envelope is signed, not
// encrypted, and cursors travel in query strings and logs. Secret rotation
// must keep working through the derived key.
func TestCursor_AccountBindsViaMACKeyNotEnvelope(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	pos := keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "x_1"}
	asA, err := EncodeCursor(secret, "u_alpha", cursorEvents, pos)
	if err != nil {
		t.Fatal(err)
	}
	asB, err := EncodeCursor(secret, "u_bravo", cursorEvents, pos)
	if err != nil {
		t.Fatal(err)
	}
	if asA == asB {
		t.Fatal("the account must change the cursor bytes")
	}
	// Identical payload segment, differing only in the MAC: the account is
	// in the key, not the envelope.
	payloadA, _, _ := strings.Cut(asA, ".")
	payloadB, _, _ := strings.Cut(asB, ".")
	if payloadA != payloadB {
		t.Fatalf("payload segments differ — the account leaked into the envelope:\n%s\n%s", payloadA, payloadB)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadA)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "u_alpha") {
		t.Fatalf("plaintext account ID in the envelope: %s", raw)
	}
	var out keysetCursor
	if err := DecodeCursor([]string{secret}, "u_bravo", cursorEvents, asA, &out); err != ErrInvalidCursor {
		t.Fatalf("foreign-account cursor: want plain ErrInvalidCursor, got %v", err)
	}
	if err := DecodeCursor([]string{secret}, "u_alpha", cursorEvents, asA, &out); err != nil {
		t.Fatalf("own-account decode: %v", err)
	}
	if out != pos {
		t.Fatalf("round-trip mismatch: %+v want %+v", out, pos)
	}
	// Rotation: a cursor signed under an old secret keeps verifying when the
	// old secret is still in the accepted list — derivation happens per
	// candidate secret inside the loop.
	if err := DecodeCursor([]string{"a-newer-secret-value-padding-32ch", secret}, "u_alpha", cursorEvents, asA, &out); err != nil {
		t.Fatalf("rotation verify through derived key: %v", err)
	}
}

// --- parent binding on parameterized lists ---

// deliveriesBindingServer serves a distinct delivery log per webhook, so a
// cursor minted on one webhook can be replayed on the other.
func deliveriesBindingServer(t *testing.T) *httptest.Server {
	t.Helper()
	perWebhook := map[string][]cursorRow{
		"wh_a": cursorRows("dla", "", 3),
		"wh_b": cursorRows("dlb", "", 3),
	}
	srv := httptest.NewServer(New(Deps{
		Authenticator: bearerGood,
		GetWebhook: func(_ context.Context, id, userID string) (*identity.Webhook, error) {
			// BOTH webhooks are owned by the caller — this is a correctness
			// bug, not a leak, and the fixture has to reflect that.
			if _, ok := perWebhook[id]; !ok {
				return nil, identity.ErrWebhookNotFound
			}
			return &identity.Webhook{ID: id, UserID: userID}, nil
		},
		ListDeliveries: func(_ context.Context, webhookID, status string, limit int, at time.Time, id string) ([]webhook.SubscriberDelivery, error) {
			out := []webhook.SubscriberDelivery{}
			for _, r := range page(perWebhook[webhookID], limit, at, id) {
				out = append(out, webhook.SubscriberDelivery{
					ID: r.id, WebhookID: webhookID, EventType: "email.received",
					Status: "delivered", CreatedAt: r.at,
				})
			}
			return out, nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeliveriesCursor_ForeignWebhookRejected: /v1/webhooks/{id}/deliveries is
// parameterized, so the resource discriminator alone is not enough — it only
// proves the cursor came from *some* deliveries list. Without the parent
// webhook pinned, webhook A's keyset anchor was handed to webhook B's query.
func TestDeliveriesCursor_ForeignWebhookRejected(t *testing.T) {
	srv := deliveriesBindingServer(t)
	cursorA := mint(t, srv, "/v1/webhooks/wh_a/deliveries?limit=1", "good")

	// Baseline: webhook B has rows of its own to return.
	code, body := getJSON(t, srv.URL+"/v1/webhooks/wh_b/deliveries?limit=3", "good")
	if code != http.StatusOK {
		t.Fatalf("baseline wh_b status %d body %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) == 0 {
		t.Fatalf("baseline wh_b returned no deliveries; the fixture is wrong")
	}

	code, body = getJSON(t, srv.URL+"/v1/webhooks/wh_b/deliveries?limit=3&cursor="+url.QueryEscape(cursorA), "good")
	if code != http.StatusBadRequest || errCode(body) != "invalid_cursor" {
		t.Fatalf("webhook A cursor on webhook B: want 400 invalid_cursor, got %d %v", code, body)
	}
}

// TestDeliveriesCursor_OwnWebhookStillWalks is the counterweight: pinning the
// parent must not break a normal walk of one webhook's own delivery log.
func TestDeliveriesCursor_OwnWebhookStillWalks(t *testing.T) {
	srv := deliveriesBindingServer(t)
	ids := walkPages(t, srv, "/v1/webhooks/wh_a/deliveries?limit=1", "id")
	assertNoDupes(t, ids, 3)
	for _, id := range ids {
		if !strings.HasPrefix(id, "dla_") {
			t.Fatalf("wh_a walk returned a foreign row %q: %v", id, ids)
		}
	}
}

// TestDecodeKeyset_RejectsTrashViewCursor pins the L1 assertion: decodeKeyset
// is the no-trash-view variant, so a Deleted cursor must be rejected rather
// than silently treated as a live-list position. Unreachable today (agents is
// the only trash-view collection and it uses decodeKeysetView); asserted so a
// future ?deleted= endpoint that keeps calling decodeKeyset fails loudly
// instead of silently losing its view binding.
func TestDecodeKeyset_RejectsTrashViewCursor(t *testing.T) {
	srv := &Server{deps: Deps{CursorSecret: "0123456789abcdef0123456789abcdef"}}
	deleted, err := EncodeCursor(srv.deps.CursorSecret, "u_1", cursorReviews,
		keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "r_1", Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.decodeKeyset("u_1", cursorReviews, deleted); err == nil {
		t.Fatal("decodeKeyset accepted a trash-view cursor")
	}
	live, err := EncodeCursor(srv.deps.CursorSecret, "u_1", cursorReviews,
		keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "r_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, id, err := srv.decodeKeyset("u_1", cursorReviews, live); err != nil || id != "r_1" {
		t.Fatalf("decodeKeyset rejected a live cursor: id=%q err=%v", id, err)
	}
}
