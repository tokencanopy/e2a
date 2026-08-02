package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/unsubscribe"
)

const publicUnsubscribeToken = "u1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type publicUnsubscribeFixture struct {
	mu           sync.Mutex
	rows         map[string]identity.AgentSuppression
	addCalls     int
	hookCalls    atomic.Int32
	resolveCalls atomic.Int32
	scope        identity.UnsubscribeScope
}

func newPublicUnsubscribeServer(t *testing.T, mutate func(*Deps, *publicUnsubscribeFixture)) (*httptest.Server, *publicUnsubscribeFixture) {
	t.Helper()
	handler, fixture := newPublicUnsubscribeHandler(t, mutate)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, fixture
}

func newPublicUnsubscribeHandler(t *testing.T, mutate func(*Deps, *publicUnsubscribeFixture)) (http.Handler, *publicUnsubscribeFixture) {
	t.Helper()
	fixture := &publicUnsubscribeFixture{
		rows:  make(map[string]identity.AgentSuppression),
		scope: identity.UnsubscribeScope{UserID: "u_1", AgentID: "sender@example.com", Address: "recipient@example.net"},
	}
	wantHash := string(unsubscribe.Hash(publicUnsubscribeToken))
	deps := Deps{
		ResolveUnsubscribeToken: func(_ context.Context, tokenHash []byte) (*identity.UnsubscribeScope, error) {
			fixture.resolveCalls.Add(1)
			if string(tokenHash) != wantHash {
				return nil, nil
			}
			scope := fixture.scope
			return &scope, nil
		},
		AddAgentSuppressionFromTokenScope: func(ctx context.Context, scope identity.UnsubscribeScope, hook identity.AgentSuppressionTxHook) (identity.AgentSuppression, bool, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.addCalls++
			key := scope.UserID + "\x00" + scope.AgentID + "\x00" + scope.Address
			if existing, ok := fixture.rows[key]; ok {
				return existing, false, nil
			}
			row := identity.AgentSuppression{AgentEmail: scope.AgentID, Address: scope.Address, Source: "unsubscribe"}
			if hook != nil {
				if err := hook(ctx, nil, identity.AgentSuppressionHookScope{UserID: scope.UserID, AgentID: scope.AgentID, Address: scope.Address, Source: "unsubscribe"}); err != nil {
					return identity.AgentSuppression{}, false, err
				}
			}
			fixture.rows[key] = row
			return row, true, nil
		},
		AgentSuppressionAddedHook: func(_ context.Context, _ pgx.Tx, got identity.AgentSuppressionHookScope) error {
			if got.UserID != "u_1" || got.AgentID != "sender@example.com" || got.Address != "recipient@example.net" || got.Source != "unsubscribe" {
				return errors.New("hook received incomplete scope")
			}
			fixture.hookCalls.Add(1)
			return nil
		},
	}
	if mutate != nil {
		mutate(&deps, fixture)
	}
	return New(deps), fixture
}

func assertPublicUnsubscribeSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Fatalf("Content-Security-Policy = %q, want restrictive policy with self form action", csp)
	}
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("response set cookies: %v", cookies)
	}
}

func doPublicUnsubscribe(t *testing.T, client *http.Client, method, endpoint, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicUnsubscribeSecurityHeaders(t, resp)
	return resp, string(b)
}

func TestPublicUnsubscribeGETConfirmsWithoutMutation(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodGet, srv.URL+"/u/"+publicUnsubscribeToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "sender@example.com") || strings.Contains(body, "recipient@example.net") {
		t.Fatalf("confirmation body must identify only sender: %q", body)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.addCalls != 0 || len(fixture.rows) != 0 || fixture.hookCalls.Load() != 0 {
		t.Fatalf("GET mutated state: calls=%d rows=%d hooks=%d", fixture.addCalls, len(fixture.rows), fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeEscapesSenderInConfirmation(t *testing.T) {
	srv, _ := newPublicUnsubscribeServer(t, func(_ *Deps, fixture *publicUnsubscribeFixture) {
		fixture.scope.AgentID = `<script>alert("x")</script>@example.com`
	})
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodGet, srv.URL+"/u/"+publicUnsubscribeToken, "", "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("GET did not safely escape sender: status=%d body=%q", resp.StatusCode, body)
	}
}

func TestPublicUnsubscribeBrowserPOSTAddsExactScope(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	form := url.Values{"confirm": {"unsubscribe"}}.Encode()
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, "application/x-www-form-urlencoded", form)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Unsubscribed") {
		t.Fatalf("browser POST = %d %q, want HTML success", resp.StatusCode, body)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	wantKey := "u_1\x00sender@example.com\x00recipient@example.net"
	if _, ok := fixture.rows[wantKey]; !ok || len(fixture.rows) != 1 || fixture.hookCalls.Load() != 1 {
		t.Fatalf("wrong mutation: rows=%v hooks=%d", fixture.rows, fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeOldTokenDoesNotRequireLiveAgent(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, func(d *Deps, _ *publicUnsubscribeFixture) {
		// A hard-deleted agent cannot be resolved through the live-agent path.
		// The public capability must use only its durable stored scope.
		d.GetAgent = func(context.Context, string) (*identity.AgentIdentity, error) {
			return nil, identity.ErrAgentNotFound
		}
	})
	resp, _ := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, "application/x-www-form-urlencoded", "confirm=unsubscribe")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST with durable old scope = %d, want 200", resp.StatusCode)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.rows) != 1 {
		t.Fatalf("durable old token inserted %d rows, want 1", len(fixture.rows))
	}
}

func TestPublicUnsubscribeRFC8058POSTReturnsEmptySuccess(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, "application/x-www-form-urlencoded", "List-Unsubscribe=One-Click")
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("RFC POST = %d %q, want empty 200", resp.StatusCode, body)
	}
	if fixture.hookCalls.Load() != 1 {
		t.Fatalf("event hooks = %d, want 1", fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeRFC8058POSTAcceptsCharsetParameter(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, "application/x-www-form-urlencoded; charset=UTF-8", rfc8058OneClickBody)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("RFC POST with charset = %d %q, want empty 200", resp.StatusCode, body)
	}
	if fixture.hookCalls.Load() != 1 {
		t.Fatalf("event hooks = %d, want 1", fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeRFC8058POSTAcceptsMultipart(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("List-Unsubscribe", "One-Click"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, responseBody := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, mw.FormDataContentType(), body.String())
	if resp.StatusCode != http.StatusOK || responseBody != "" {
		t.Fatalf("multipart RFC POST = %d %q, want empty 200", resp.StatusCode, responseBody)
	}
	if fixture.hookCalls.Load() != 1 {
		t.Fatalf("event hooks = %d, want 1", fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribePOSTRejectsBodyOverLimit(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	const boundary = "unsubscribe-size-limit"
	// This is an otherwise-valid one-field RFC 8058 multipart request. Its
	// ignored extension header is deliberately large so removing MaxBytesReader
	// would make the request succeed, while the 1 KiB cap must reject it.
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"List-Unsubscribe\"\r\n" +
		"X-Padding: " + strings.Repeat("x", publicUnsubscribeMaxBody) + "\r\n\r\n" +
		"One-Click\r\n--" + boundary + "--\r\n"
	values, err := readPublicUnsubscribeMultipart(multipart.NewReader(strings.NewReader(body), boundary))
	if err != nil || values.Get("List-Unsubscribe") != "One-Click" {
		t.Fatalf("unbounded control request is not valid RFC 8058 input: values=%v err=%v", values, err)
	}
	resp, _ := doPublicUnsubscribe(t, srv.Client(), http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, "multipart/form-data; boundary="+boundary, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized POST = %d, want 400", resp.StatusCode)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.addCalls != 0 || len(fixture.rows) != 0 {
		t.Fatalf("oversized POST mutated state: calls=%d rows=%d", fixture.addCalls, len(fixture.rows))
	}
}

func TestPublicUnsubscribeDuplicateAndConcurrentPOSTAreIdempotent(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	const requests = 12
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/u/"+publicUnsubscribeToken, strings.NewReader("List-Unsubscribe=One-Click"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := srv.Client().Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				errs <- errors.New(resp.Status)
				return
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				errs <- errors.New("missing no-store")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.rows) != 1 || fixture.hookCalls.Load() != 1 {
		t.Fatalf("concurrent POSTs: rows=%d hooks=%d", len(fixture.rows), fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeInvalidRequestsAreGenericAndDoNotMutate(t *testing.T) {
	srv, fixture := newPublicUnsubscribeServer(t, nil)
	tests := []struct {
		name, method, path, contentType, body string
		want                                  int
	}{
		{"malformed token", http.MethodGet, "/u/not-a-token", "", "", http.StatusNotFound},
		{"unknown token", http.MethodGet, "/u/u1_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "", "", http.StatusNotFound},
		{"bad rfc value", http.MethodPost, "/u/" + publicUnsubscribeToken, "application/x-www-form-urlencoded", "List-Unsubscribe=No", http.StatusBadRequest},
		{"wrong media type", http.MethodPost, "/u/" + publicUnsubscribeToken, "application/json", `{"confirm":true}`, http.StatusBadRequest},
	}
	var notFoundBody string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doPublicUnsubscribe(t, srv.Client(), tc.method, srv.URL+tc.path, tc.contentType, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d body=%q, want %d", resp.StatusCode, body, tc.want)
			}
			if tc.want == http.StatusNotFound {
				if strings.Contains(body, "recipient@example.net") || strings.Contains(body, "sender@example.com") || strings.Contains(body, tc.path) {
					t.Fatalf("404 disclosed token scope: %q", body)
				}
				if notFoundBody == "" {
					notFoundBody = body
				} else if body != notFoundBody {
					t.Fatalf("404 bodies differ: %q vs %q", notFoundBody, body)
				}
			}
		})
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.rows) != 0 || fixture.hookCalls.Load() != 0 {
		t.Fatalf("invalid requests mutated state: rows=%d hooks=%d", len(fixture.rows), fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeUnsupportedMethodHasSecurityHeaders(t *testing.T) {
	srv, _ := newPublicUnsubscribeServer(t, nil)
	resp, _ := doPublicUnsubscribe(t, srv.Client(), http.MethodPut, srv.URL+"/u/"+publicUnsubscribeToken, "application/x-www-form-urlencoded", "confirm=unsubscribe")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", resp.StatusCode)
	}
	// RFC 9110 §15.5.6: the 405 must name the supported methods. This raw
	// handler serves exactly GET and POST.
	got := map[string]bool{}
	for _, m := range strings.Split(resp.Header.Get("Allow"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			got[m] = true
		}
	}
	if want := map[string]bool{http.MethodGet: true, http.MethodPost: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Allow = %q (set %v), want exactly %v", resp.Header.Get("Allow"), got, want)
	}
}

// TestPublicUnsubscribeMethodCoverage sweeps every method chi can route against
// /u/{token}. Unlike the /v1 405s, this route's Allow cannot be derived from
// the routing table (it is registered for all methods), so it is driven by
// publicUnsubscribeHandlers instead — this asserts, method by method, that the
// dispatch table and the advertised Allow actually agree.
func TestPublicUnsubscribeMethodCoverage(t *testing.T) {
	handler, _ := newPublicUnsubscribeHandler(t, nil)
	for _, method := range probedMethods {
		t.Run(method, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(method, "/u/"+publicUnsubscribeToken, nil))
			if _, served := publicUnsubscribeHandlers[method]; served {
				// Served methods must never 405 (POST here has no body and so
				// gets a 400 — the point is only that it reached the handler).
				if rr.Code == http.StatusMethodNotAllowed {
					t.Fatalf("%s is in publicUnsubscribeHandlers but got 405", method)
				}
				if allow := rr.Header().Get("Allow"); allow != "" {
					t.Fatalf("%s: non-405 response carries Allow = %q", method, allow)
				}
				return
			}
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d, want 405; body=%q", method, rr.Code, rr.Body.String())
			}
			if allow := rr.Header().Get("Allow"); allow != "GET, POST" {
				t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, POST")
			}
		})
	}
}

func TestPublicUnsubscribeRateLimitRunsBeforeTokenResolution(t *testing.T) {
	var limiterKey string
	srv, fixture := newPublicUnsubscribeServer(t, func(d *Deps, _ *publicUnsubscribeFixture) {
		d.UnsubscribeLimit = func(key string) (bool, time.Duration, int, int, int) {
			limiterKey = key
			return false, 3 * time.Second, 10, 0, 60
		}
	})
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/u/"+publicUnsubscribeToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assertPublicUnsubscribeSecurityHeaders(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "3" {
		t.Fatalf("limited response = %d retry=%q, want 429 retry=3", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if limiterKey != "203.0.113.9" {
		t.Fatalf("limiter key = %q, want trusted CF client IP", limiterKey)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.resolveCalls.Load() != 0 || fixture.addCalls != 0 || len(fixture.rows) != 0 || fixture.hookCalls.Load() != 0 {
		t.Fatalf("limited request did work: resolves=%d adds=%d rows=%d hooks=%d", fixture.resolveCalls.Load(), fixture.addCalls, len(fixture.rows), fixture.hookCalls.Load())
	}
}

func TestPublicUnsubscribeLimiterIsIndependentFromAttachmentBucket(t *testing.T) {
	var attachmentCalls, unsubscribeCalls int
	srv, fixture := newPublicUnsubscribeServer(t, func(d *Deps, _ *publicUnsubscribeFixture) {
		d.DownloadLimit = func(string) (bool, time.Duration, int, int, int) {
			attachmentCalls++
			return false, time.Minute, 0, 0, 60
		}
		d.UnsubscribeLimit = func(string) (bool, time.Duration, int, int, int) {
			unsubscribeCalls++
			return true, 0, 600, 599, 60
		}
	})
	resp, _ := doPublicUnsubscribe(t, srv.Client(), http.MethodGet, srv.URL+"/u/"+publicUnsubscribeToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with exhausted attachment bucket = %d, want 200", resp.StatusCode)
	}
	if attachmentCalls != 0 || unsubscribeCalls != 1 || fixture.resolveCalls.Load() != 1 {
		t.Fatalf("limiter calls: attachment=%d unsubscribe=%d resolves=%d", attachmentCalls, unsubscribeCalls, fixture.resolveCalls.Load())
	}
}

func TestPublicUnsubscribeStoreFailuresDoNotLeakScope(t *testing.T) {
	srv, _ := newPublicUnsubscribeServer(t, func(d *Deps, _ *publicUnsubscribeFixture) {
		d.ResolveUnsubscribeToken = func(context.Context, []byte) (*identity.UnsubscribeScope, error) {
			return nil, errors.New("database contains recipient@example.net")
		}
	})
	resp, body := doPublicUnsubscribe(t, srv.Client(), http.MethodGet, srv.URL+"/u/"+publicUnsubscribeToken, "", "")
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "recipient@example.net") {
		t.Fatalf("storage failure = %d %q, want generic 500", resp.StatusCode, body)
	}
}
