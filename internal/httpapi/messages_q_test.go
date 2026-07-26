package httpapi

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/filterquery"
	"github.com/tokencanopy/e2a/internal/identity"
)

func messagesQURL(srvURL, q string) string {
	values := url.Values{}
	values.Set("direction", "inbound")
	values.Set("read_status", "all")
	values.Set("q", q)
	return srvURL + "/v1/agents/support%40acme.com/messages?" + values.Encode()
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

func TestQParamInvalid(t *testing.T) {
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
		q           string
		wantInError string
		wantOK      bool
	}{
		{name: "unknown field", q: "unknown:thing", wantInError: `unknown field "unknown"`},
		{name: "forbidden operator", q: "label=urgent", wantInError: `operator "=" is not allowed on field "label"`},
		{name: "syntax retains column", q: "label:", wantInError: "(at column 7)"},
		{name: "ASCII over code point limit", q: "label:" + strings.Repeat("a", 501), wantInError: "q filter too long (max 500 chars)"},
		{name: "Unicode over code point limit", q: strings.Repeat("界", 501), wantInError: "q filter too long (max 500 chars)"},
		{name: "NUL is invalid", q: "subject:\x00", wantInError: "q filter must be valid UTF-8 and must not contain NUL"},
		{name: "malformed UTF-8 is invalid", q: string([]byte("subject:\xff")), wantInError: "q filter must be valid UTF-8 and must not contain NUL"},
		{name: "Unicode code point limit accepts bytes over cap", q: valid500Unicode, wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			code, body := getJSON(t, messagesQURL(srv.URL, tc.q), "good")
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

func TestQParamCursorPinning(t *testing.T) {
	srv := testServer(t)
	page1URL := messagesQURL(srv.URL, "label:urgent") + "&limit=1"
	code, body := getJSON(t, page1URL, "good")
	if code != 200 {
		t.Fatalf("page 1 status = %d, want 200 (body %v)", code, body)
	}
	cursor, ok := body["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("page 1 next_cursor = %v, want non-empty string", body["next_cursor"])
	}

	continuation := func(q string) (int, map[string]any) {
		return getJSON(t, messagesQURL(srv.URL, q)+"&limit=1&cursor="+url.QueryEscape(cursor), "good")
	}
	if code, body := continuation("label:urgent"); code != 200 {
		t.Fatalf("identical q continuation status = %d, want 200 (body %v)", code, body)
	}
	for _, q := range []string{"label:other", ""} {
		code, body := continuation(q)
		if code != 400 {
			t.Fatalf("continuation q=%q status = %d, want 400 (body %v)", q, code, body)
		}
		if errBody, _ := body["error"].(map[string]any); errBody["code"] != "invalid_cursor" {
			t.Fatalf("continuation q=%q error = %v, want invalid_cursor", q, body)
		}
	}

	// A cursor created without q must also reject adding q on page two.
	code, body = getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages?direction=inbound&read_status=all&limit=1", "good")
	if code != 200 {
		t.Fatalf("no-q page 1 status = %d, want 200 (body %v)", code, body)
	}
	noQCursor := body["next_cursor"].(string)
	code, body = getJSON(t, messagesQURL(srv.URL, "label:urgent")+"&limit=1&cursor="+url.QueryEscape(noQCursor), "good")
	if code != 400 {
		t.Fatalf("adding q continuation status = %d, want 400 (body %v)", code, body)
	}
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "invalid_cursor" {
		t.Fatalf("adding q continuation error = %v, want invalid_cursor", body)
	}
}

func TestQParamReachesStore(t *testing.T) {
	var captured struct {
		sync.Mutex
		filter identity.MessageListFilter
	}
	srv := testServer(t, func(deps *Deps) {
		deps.ListMessages = func(_ context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			captured.Lock()
			captured.filter = f
			captured.Unlock()
			return nil, nil
		}
	})

	code, body := getJSON(t, messagesQURL(srv.URL, "label:urgent"), "good")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body %v)", code, body)
	}
	captured.Lock()
	filter := captured.filter
	captured.Unlock()
	if filter.Q == nil {
		t.Fatal("store filter Q = nil, want parsed expression")
	}
	fragment, args, err := filter.Q.Emit(filterquery.PostgresDialect{}, 1)
	if err != nil {
		t.Fatalf("Q.Emit: %v", err)
	}
	if fragment != "(m.labels @> $1)" {
		t.Errorf("Q SQL = %q, want %q", fragment, "(m.labels @> $1)")
	}
	if want := []any{[]string{"urgent"}}; !reflect.DeepEqual(args, want) {
		t.Errorf("Q args = %#v, want %#v", args, want)
	}
}

func TestQComposesWithFlatParams(t *testing.T) {
	var captured struct {
		sync.Mutex
		filter identity.MessageListFilter
	}
	srv := testServer(t, func(deps *Deps) {
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
	values.Set("q", "label:urgent")
	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages?"+values.Encode(), "good")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body %v)", code, body)
	}
	captured.Lock()
	filter := captured.filter
	captured.Unlock()
	if filter.From != "alice" {
		t.Errorf("store filter From = %q, want alice", filter.From)
	}
	if filter.Q == nil {
		t.Fatal("store filter Q = nil, want parsed expression alongside From")
	}
}
