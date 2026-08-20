package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v3/jwt"
	"github.com/gorilla/mux"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/usage"
)

func startFeedbackSMTPServer(t *testing.T) (host string, port int, received <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	messages := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 feedback-test ready\r\n")
		var data []string
		inData := false
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					messages <- strings.Join(data, "\n")
					fmt.Fprint(conn, "250 Ok feedback-test-id\r\n")
					inData = false
					continue
				}
				data = append(data, line)
				continue
			}
			if strings.EqualFold(line, "DATA") {
				inData = true
				fmt.Fprint(conn, "354 Go ahead\r\n")
				continue
			}
			if strings.EqualFold(line, "QUIT") {
				fmt.Fprint(conn, "221 Bye\r\n")
				return
			}
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, messages
}

func startHangingFeedbackSMTPServer(t *testing.T) (host string, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})
	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func TestFeedbackGitHubTimeoutStillDeliversEmail(t *testing.T) {
	releaseGitHub := make(chan struct{})
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseGitHub:
		}
	}))
	defer githubServer.Close()
	defer close(releaseGitHub)

	oldBaseURL := githubAPIBaseURL
	githubAPIBaseURL = githubServer.URL
	defer func() { githubAPIBaseURL = oldBaseURL }()
	oldTimeout := feedbackGitHubTimeout
	feedbackGitHubTimeout = 50 * time.Millisecond
	defer func() { feedbackGitHubTimeout = oldTimeout }()

	t.Setenv("GITHUB_FEEDBACK_APP_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_INSTALLATION_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "pat")
	t.Setenv("GITHUB_FEEDBACK_REPO", "owner/repo")
	t.Setenv("FEEDBACK_NOTIFY_TO", "feedback@example.com")
	t.Setenv("FEEDBACK_NOTIFY_CC", "")

	smtpHost, smtpPort, smtpMessages := startFeedbackSMTPServer(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpHost, Port: smtpPort})
	sender := outbound.NewSender(relay, "test.e2a.dev")
	api := NewAPI(nil, sender, relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	started := time.Now()
	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(`{"category":"bug","message":"GitHub is stuck"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 because email delivered", resp.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("feedback returned after %s, want bounded GitHub failure within 1s", elapsed)
	}

	var message string
	select {
	case message = <-smtpMessages:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for feedback email")
	}
	if !strings.Contains(message, "GitHub issue: creation FAILED") {
		t.Fatalf("email did not report GitHub failure:\n%s", message)
	}
}

func TestFeedbackEmailTimeoutReturnsAfterGitHubDelivery(t *testing.T) {
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.example/owner/repo/issues/1"}`))
	}))
	defer githubServer.Close()

	oldBaseURL := githubAPIBaseURL
	githubAPIBaseURL = githubServer.URL
	defer func() { githubAPIBaseURL = oldBaseURL }()
	oldTimeout := feedbackEmailTimeout
	feedbackEmailTimeout = 50 * time.Millisecond
	defer func() { feedbackEmailTimeout = oldTimeout }()

	t.Setenv("GITHUB_FEEDBACK_APP_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_INSTALLATION_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "pat")
	t.Setenv("GITHUB_FEEDBACK_REPO", "owner/repo")
	t.Setenv("FEEDBACK_NOTIFY_TO", "feedback@example.com")
	t.Setenv("FEEDBACK_NOTIFY_CC", "")

	smtpHost, smtpPort := startHangingFeedbackSMTPServer(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpHost, Port: smtpPort})
	sender := outbound.NewSender(relay, "test.e2a.dev")
	api := NewAPI(nil, sender, relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	started := time.Now()
	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(`{"category":"general","message":"SMTP is stuck"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 because GitHub delivered", resp.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("feedback returned after %s, want bounded SMTP failure within 1s", elapsed)
	}
}

// A GitHub credential configured with no GITHUB_FEEDBACK_REPO must refuse to
// file — never default to the operator's tokencanopy/e2a. /api/feedback is
// unauthenticated, so a silent default there would let anyone who can reach
// this deployment file arbitrary-text issues on the operator's public
// tracker using this deployment's own GitHub credential, with the
// submitter's email in the body.
func TestFeedbackNoRepoConfigured_RefusesToFileRatherThanDefaultingToOperatorRepo(t *testing.T) {
	issueRequested := false
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issueRequested = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.example/tokencanopy/e2a/issues/1"}`))
	}))
	defer githubServer.Close()

	oldBaseURL := githubAPIBaseURL
	githubAPIBaseURL = githubServer.URL
	defer func() { githubAPIBaseURL = oldBaseURL }()

	t.Setenv("GITHUB_FEEDBACK_APP_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_INSTALLATION_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "pat")
	t.Setenv("GITHUB_FEEDBACK_REPO", "") // deliberately unset — the bug under test
	t.Setenv("FEEDBACK_NOTIFY_TO", "feedback@example.com")
	t.Setenv("FEEDBACK_NOTIFY_CC", "")

	smtpHost, smtpPort, smtpMessages := startFeedbackSMTPServer(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpHost, Port: smtpPort})
	sender := outbound.NewSender(relay, "test.e2a.dev")
	api := NewAPI(nil, sender, relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/feedback", "application/json", bytes.NewBufferString(`{"category":"bug","message":"no repo configured"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (email channel still delivers)", resp.StatusCode)
	}

	if issueRequested {
		t.Fatal("a GitHub issue-create request was sent even though GITHUB_FEEDBACK_REPO was unset — must never default to the operator's repo")
	}

	var message string
	select {
	case message = <-smtpMessages:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for feedback email")
	}
	if !strings.Contains(message, "GitHub issue: creation FAILED") {
		t.Fatalf("email did not report the missing-repo failure:\n%s", message)
	}
}

// testAppKey generates a throwaway RSA key and returns it plus its
// base64-encoded PKCS#1 PEM (the canonical GITHUB_FEEDBACK_APP_PRIVATE_KEY
// env form).
func testAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, base64.StdEncoding.EncodeToString(pemBytes)
}

func TestParseGitHubAppKey(t *testing.T) {
	key, b64 := testAppKey(t)

	got, err := parseGitHubAppKey(b64)
	if err != nil {
		t.Fatalf("base64 form: %v", err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("base64 form: wrong key")
	}

	// Raw PEM with literal `\n` escapes (the other accepted env form).
	pemBytes, _ := base64.StdEncoding.DecodeString(b64)
	escaped := strings.ReplaceAll(string(pemBytes), "\n", `\n`)
	got, err = parseGitHubAppKey(escaped)
	if err != nil {
		t.Fatalf("escaped PEM form: %v", err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("escaped PEM form: wrong key")
	}

	if _, err := parseGitHubAppKey("not-a-key"); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestExchangeInstallationToken(t *testing.T) {
	key, b64 := testAppKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/123/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok, err := jwt.ParseSigned(raw)
		if err != nil {
			t.Errorf("parse app jwt: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var claims jwt.Claims
		if err := tok.Claims(&key.PublicKey, &claims); err != nil {
			t.Errorf("verify app jwt signature: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.Issuer != "456" {
			t.Errorf("iss = %q, want %q", claims.Issuer, "456")
		}
		if claims.Expiry == nil || claims.IssuedAt == nil {
			t.Error("app jwt missing iat/exp")
		} else if d := claims.Expiry.Time().Sub(claims.IssuedAt.Time()); d < 9*time.Minute || d > 11*time.Minute {
			t.Errorf("app jwt lifetime = %s, want ~10m", d)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"token": "ghs_test_token"})
	}))
	defer srv.Close()

	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	tok, err := exchangeInstallationToken(context.Background(), "456", "123", b64)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "ghs_test_token" {
		t.Errorf("token = %q, want %q", tok, "ghs_test_token")
	}
}

func TestExchangeInstallationToken_ErrorStatus(t *testing.T) {
	_, b64 := testAppKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	if _, err := exchangeInstallationToken(context.Background(), "456", "999", b64); err == nil {
		t.Error("expected error on non-201 exchange response")
	}
}

func TestFeedbackGitHubClient_Precedence(t *testing.T) {
	// Hermetic env — none of the four vars leak in from the host.
	t.Setenv("GITHUB_FEEDBACK_APP_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_INSTALLATION_ID", "")
	t.Setenv("GITHUB_FEEDBACK_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "")

	// Nothing set → channel off.
	if c, err := feedbackGitHubClient(context.Background()); c != nil || err != nil {
		t.Errorf("no env: got client=%v err=%v, want nil,nil", c, err)
	}

	// PAT fallback.
	t.Setenv("GITHUB_FEEDBACK_TOKEN", "pat")
	if c, err := feedbackGitHubClient(context.Background()); c == nil || err != nil {
		t.Errorf("PAT: got client=%v err=%v, want client,nil", c, err)
	}

	// Partial app config → loud log, still PAT.
	t.Setenv("GITHUB_FEEDBACK_APP_ID", "456")
	if c, err := feedbackGitHubClient(context.Background()); c == nil || err != nil {
		t.Errorf("partial app config: got client=%v err=%v, want client,nil", c, err)
	}

	// Full app config with a bogus key → hard error, no silent PAT fallback.
	t.Setenv("GITHUB_FEEDBACK_APP_INSTALLATION_ID", "123")
	t.Setenv("GITHUB_FEEDBACK_APP_PRIVATE_KEY", "bogus")
	if c, err := feedbackGitHubClient(context.Background()); err == nil || c != nil {
		t.Errorf("bad app key: got client=%v err=%v, want nil,error", c, err)
	}
}
