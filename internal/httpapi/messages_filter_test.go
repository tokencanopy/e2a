package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/filterquery"
	"github.com/tokencanopy/e2a/internal/identity"
)

func newMessagesFilterTestServer(t *testing.T, opts ...func(*Deps)) *httptest.Server {
	t.Helper()
	base := []func(*Deps){
		func(deps *Deps) {
			deps.GetAgent = func(_ context.Context, address string) (*identity.AgentIdentity, error) {
				if address != "bot@example.com" {
					return nil, errors.New("not found")
				}
				return &identity.AgentIdentity{ID: "bot@example.com", Email: "bot@example.com", UserID: "u_1", DomainVerified: true}, nil
			}
			deps.GetAgentAnyState = deps.GetAgent
			deps.ListMessages = func(_ context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
				if f.AgentID != "bot@example.com" {
					return nil, errors.New("unexpected agent")
				}
				all := []identity.Message{
					{ID: "msg_b", Direction: "inbound", Sender: "b@x.com", Recipient: "bot@example.com", Subject: "B", InboxStatus: "unread", CreatedAt: time.Unix(1700000200, 0).UTC()},
					{ID: "msg_a", Direction: "inbound", Sender: "a@x.com", Recipient: "bot@example.com", Subject: "A", InboxStatus: "unread", CreatedAt: time.Unix(1700000100, 0).UTC()},
				}
				if f.AfterID == "msg_b" {
					return all[1:], nil
				}
				if f.Limit > 0 && len(all) > f.Limit {
					return all[:f.Limit], nil
				}
				return all, nil
			}
		},
	}
	return testServer(t, append(base, opts...)...)
}

func messagesFilterURL(serverURL, filter string) string {
	values := url.Values{
		"direction":   {"inbound"},
		"read_status": {"all"},
	}
	if filter != "" {
		values.Set("filter", filter)
	}
	return serverURL + "/v1/agents/bot@example.com/messages?" + values.Encode()
}

func filterError(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", body)
	}
	if errBody["code"] != "invalid_filter" {
		t.Fatalf("error code = %v, want invalid_filter (body %v)", errBody["code"], body)
	}
	return errBody
}

func TestFilterParamInvalid(t *testing.T) {
	valid500Unicode := "subject:" + strings.Repeat("界", 60)
	valid500Unicode += strings.Repeat(" ", 500-utf8.RuneCountInString(valid500Unicode))
	if got := utf8.RuneCountInString(valid500Unicode); got != 500 {
		t.Fatalf("valid Unicode query length = %d, want 500", got)
	}
	if len(strings.Repeat("界", 60)) >= 200 {
		t.Fatal("test query subject value must remain under the 200-byte field cap")
	}

	tests := []struct {
		name        string
		filter      string
		wantInError string
		wantOK      bool
	}{
		{name: "unknown field", filter: "unknown:thing", wantInError: `unknown field "unknown"`},
		{name: "attachment filtering deferred", filter: "has:attachment", wantInError: `unknown field "has" — supported fields: created, from, label, subject`},
		{name: "forbidden operator", filter: "label=urgent", wantInError: `operator "=" is not allowed on field "label"`},
		{name: "syntax retains column", filter: "label:", wantInError: "(at column 7)"},
		{name: "ASCII over code point limit", filter: "label:" + strings.Repeat("a", 501), wantInError: "filter too long (max 500 chars)"},
		{name: "Unicode over code point limit", filter: strings.Repeat("界", 501), wantInError: "filter too long (max 500 chars)"},
		{name: "NUL is invalid", filter: "subject:\x00", wantInError: "filter must be valid UTF-8 and must not contain NUL"},
		{name: "malformed UTF-8 is invalid", filter: string([]byte("subject:\xff")), wantInError: "filter must be valid UTF-8 and must not contain NUL"},
		{name: "Unicode code point limit accepts bytes over cap", filter: valid500Unicode, wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMessagesFilterTestServer(t)
			code, body := getJSON(t, messagesFilterURL(srv.URL, tc.filter), "good")
			if tc.wantOK {
				if code != 200 {
					t.Fatalf("status = %d, want 200 (body %v)", code, body)
				}
				return
			}
			if code != 400 {
				t.Fatalf("status = %d, want 400 (body %v)", code, body)
			}
			errBody := filterError(t, body)
			message, _ := errBody["message"].(string)
			if !strings.Contains(message, tc.wantInError) {
				t.Errorf("error message = %q, want it to contain %q", message, tc.wantInError)
			}
		})
	}
}

func TestFilterParamCursorPinning(t *testing.T) {
	srv := newMessagesFilterTestServer(t)
	page1URL := messagesFilterURL(srv.URL, "label:urgent") + "&limit=1"
	code, body := getJSON(t, page1URL, "good")
	if code != 200 {
		t.Fatalf("page 1 status = %d, want 200 (body %v)", code, body)
	}
	cursor, ok := body["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("page 1 next_cursor = %v, want non-empty string", body["next_cursor"])
	}

	continuation := func(filter string) (int, map[string]any) {
		return getJSON(t, messagesFilterURL(srv.URL, filter)+"&limit=1&cursor="+url.QueryEscape(cursor), "good")
	}
	if code, body := continuation("label:urgent"); code != 200 {
		t.Fatalf("identical filter continuation status = %d, want 200 (body %v)", code, body)
	}
	for _, filter := range []string{"label:other", ""} {
		code, body := continuation(filter)
		if code != 400 {
			t.Fatalf("continuation filter=%q status = %d, want 400 (body %v)", filter, code, body)
		}
		if errBody, _ := body["error"].(map[string]any); errBody["code"] != "invalid_cursor" {
			t.Fatalf("continuation filter=%q error = %v, want invalid_cursor", filter, body)
		}
	}

	// A cursor created without filter must also reject adding filter on page two.
	code, body = getJSON(t, messagesFilterURL(srv.URL, "")+"&limit=1", "good")
	if code != 200 {
		t.Fatalf("no-filter page 1 status = %d, want 200 (body %v)", code, body)
	}
	noFilterCursor := body["next_cursor"].(string)
	code, body = getJSON(t, messagesFilterURL(srv.URL, "label:urgent")+"&limit=1&cursor="+url.QueryEscape(noFilterCursor), "good")
	if code != 400 {
		t.Fatalf("adding filter continuation status = %d, want 400 (body %v)", code, body)
	}
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "invalid_cursor" {
		t.Fatalf("adding filter continuation error = %v, want invalid_cursor", body)
	}
}

func TestListMessagesQIsNotAStructuredFilterAlias(t *testing.T) {
	srv := newMessagesFilterTestServer(t)
	code, body := getJSON(t,
		srv.URL+"/v1/agents/bot@example.com/messages?direction=inbound&read_status=all&q=unknown:value",
		"good",
	)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v; q must be ignored as an unknown query parameter", code, body)
	}
}

func TestFilterParamReachesStore(t *testing.T) {
	var captured struct {
		sync.Mutex
		filter identity.MessageListFilter
	}
	srv := newMessagesFilterTestServer(t, func(deps *Deps) {
		deps.ListMessages = func(_ context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			captured.Lock()
			captured.filter = f
			captured.Unlock()
			return nil, nil
		}
	})

	code, body := getJSON(t, messagesFilterURL(srv.URL, "label:urgent"), "good")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body %v)", code, body)
	}
	captured.Lock()
	filter := captured.filter
	captured.Unlock()
	if filter.Filter == nil {
		t.Fatal("store Filter = nil, want parsed expression")
	}
	fragment, args, err := filter.Filter.Emit(filterquery.PostgresDialect{}, 1)
	if err != nil {
		t.Fatalf("Filter.Emit: %v", err)
	}
	if fragment != "(m.labels @> $1)" {
		t.Errorf("Filter SQL = %q, want %q", fragment, "(m.labels @> $1)")
	}
	if want := []any{[]string{"urgent"}}; !reflect.DeepEqual(args, want) {
		t.Errorf("Filter args = %#v, want %#v", args, want)
	}
}

func TestFilterComposesWithFlatParams(t *testing.T) {
	var captured struct {
		sync.Mutex
		filter identity.MessageListFilter
	}
	srv := newMessagesFilterTestServer(t, func(deps *Deps) {
		deps.ListMessages = func(_ context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			captured.Lock()
			captured.filter = f
			captured.Unlock()
			return nil, nil
		}
	})
	values := url.Values{}
	values.Set("direction", "inbound")
	values.Set("read_status", "all")
	values.Set("from", "alice")
	values.Set("filter", "label:urgent")
	code, body := getJSON(t, srv.URL+"/v1/agents/bot@example.com/messages?"+values.Encode(), "good")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body %v)", code, body)
	}
	captured.Lock()
	filter := captured.filter
	captured.Unlock()
	if filter.From != "alice" {
		t.Errorf("store filter From = %q, want alice", filter.From)
	}
	if filter.Filter == nil {
		t.Fatal("store Filter = nil, want parsed expression alongside From")
	}
}
