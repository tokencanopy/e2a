package httpapi

import (
	"context"
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
// These tests assert the whole N x N matrix: every list endpoint rejects
// every other list endpoint's cursor, and still walks its own.

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

// cursorBindingServer wires every cursor-minting account-level list with a
// keyset-honoring fake, so each endpoint can mint a REAL cursor through the
// real handler — the foreign cursors under test are genuine API output, not
// hand-built payloads.
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

	srv := httptest.NewServer(New(Deps{
		Authenticator: bearerGood,
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
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cursorBoundEndpoints is every list endpoint the binding must cover. Each
// yields >1 page at limit=1, so `mint` below always returns a real cursor.
var cursorBoundEndpoints = []struct {
	name string
	path string
}{
	{"agents", "/v1/agents?limit=1"},
	{"domains", "/v1/domains?limit=1"},
	{"webhooks", "/v1/webhooks?limit=1"},
	{"templates", "/v1/templates?limit=1"},
	{"api_keys", "/v1/account/api-keys?limit=1"},
	{"reviews", "/v1/reviews?limit=1"},
	{"events", "/v1/events?limit=1"},
	{"account_suppressions", "/v1/account/suppressions?limit=1"},
	{"contacts", "/v1/contacts?limit=1"},
	// Both of these were ALSO accepting foreign cursors pre-fix and were
	// missed by the original bug report's list of eight: deliveries with no
	// status filter matched a foreign cursor's empty status, and
	// starter-templates decoded a foreign cursor's absent alias as "" and
	// silently re-served page 1. Verified against a clean origin/main.
	{"webhook_deliveries", "/v1/webhooks/wh_1/deliveries?limit=1"},
	{"starter_templates", "/v1/starter-templates?limit=1"},
}

// mint drives one real first-page request and returns its next_cursor.
func mint(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	code, body := getJSON(t, srv.URL+path, "good")
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
		cursors[ep.name] = mint(t, srv, ep.path)
	}

	for _, target := range cursorBoundEndpoints {
		for _, source := range cursorBoundEndpoints {
			if source.name == target.name {
				continue
			}
			t.Run(source.name+"_cursor_on_"+target.name, func(t *testing.T) {
				u := srv.URL + target.path + "&cursor=" + url.QueryEscape(cursors[source.name])
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

// TestCursor_ContactsCursorOnEventsIsRejected pins the exact live
// reproduction from staging: a contacts cursor replayed on /v1/events used
// to return 200 with an empty items array while the same query without a
// cursor returned rows.
func TestCursor_ContactsCursorOnEventsIsRejected(t *testing.T) {
	srv := cursorBindingServer(t)
	contactsCursor := mint(t, srv, "/v1/contacts?limit=1")

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
// break normal pagination. Each endpoint's own cursor still advances, and
// page 2 is disjoint from page 1.
func TestCursor_OwnCursorStillWalks(t *testing.T) {
	srv := cursorBindingServer(t)
	for _, ep := range cursorBoundEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			code, first := getJSON(t, srv.URL+ep.path, "good")
			if code != http.StatusOK {
				t.Fatalf("page 1: status %d body %v", code, first)
			}
			cursor, _ := first["next_cursor"].(string)
			if cursor == "" {
				t.Fatalf("page 1 produced no cursor: %v", first)
			}
			code, second := getJSON(t, srv.URL+ep.path+"&cursor="+url.QueryEscape(cursor), "good")
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

// TestCursor_ResourceIsSignedNotJustTagged: a client cannot re-target a
// cursor by rewriting the resource, because the resource lives inside the
// HMAC-covered payload.
func TestCursor_ResourceIsSignedNotJustTagged(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	pos := keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "x_1"}
	asEvents, err := EncodeCursor(secret, cursorEvents, pos)
	if err != nil {
		t.Fatal(err)
	}
	asAgents, err := EncodeCursor(secret, cursorAgents, pos)
	if err != nil {
		t.Fatal(err)
	}
	if asEvents == asAgents {
		t.Fatal("the resource must change the cursor bytes")
	}
	var out keysetCursor
	if err := DecodeCursor([]string{secret}, cursorAgents, asEvents, &out); err != ErrCursorResourceMismatch {
		t.Fatalf("events cursor decoded as agents: %v", err)
	}
	// ErrCursorResourceMismatch must still satisfy the broad sentinel so
	// callers that only branch on ErrInvalidCursor keep working.
	if !errors.Is(ErrCursorResourceMismatch, ErrInvalidCursor) {
		t.Fatal("ErrCursorResourceMismatch must wrap ErrInvalidCursor")
	}
	if err := DecodeCursor([]string{secret}, cursorEvents, asEvents, &out); err != nil {
		t.Fatalf("own-resource decode: %v", err)
	}
	if out != pos {
		t.Fatalf("round-trip mismatch: %+v want %+v", out, pos)
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
	cursorA := mint(t, srv, "/v1/webhooks/wh_a/deliveries?limit=1")

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
	deleted, err := EncodeCursor(srv.deps.CursorSecret, cursorReviews,
		keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "r_1", Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.decodeKeyset(cursorReviews, deleted); err == nil {
		t.Fatal("decodeKeyset accepted a trash-view cursor")
	}
	live, err := EncodeCursor(srv.deps.CursorSecret, cursorReviews,
		keysetCursor{CreatedAt: time.Unix(1700000000, 0).UTC(), ID: "r_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, id, err := srv.decodeKeyset(cursorReviews, live); err != nil || id != "r_1" {
		t.Fatalf("decodeKeyset rejected a live cursor: id=%q err=%v", id, err)
	}
}
