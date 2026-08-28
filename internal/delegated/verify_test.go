package delegated

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// --- harness ---

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordedMetrics struct {
	mu       sync.Mutex
	outcomes []string
}

func (m *recordedMetrics) DelegatedJWKSRefresh(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, outcome)
}

func (m *recordedMetrics) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.outcomes...)
}

// issuerServer is a synthetic OIDC issuer: discovery + JWKS with one RSA
// and one EC key, swappable at runtime.
type issuerServer struct {
	srv        *httptest.Server
	mu         sync.Mutex
	keys       []jose.JSONWebKey
	rawJWKS    []byte // when set, served verbatim instead of keys
	jwksCalls  atomic.Int64
	jwksStatus int
}

func newIssuerServer(t *testing.T) (*issuerServer, *rsa.PrivateKey, *ecdsa.PrivateKey) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuerServer{jwksStatus: http.StatusOK}
	is.keys = []jose.JSONWebKey{
		{Key: rsaKey.Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"},
		{Key: ecKey.Public(), KeyID: "ec-1", Algorithm: "ES256", Use: "sig"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   is.srv.URL,
			"jwks_uri": is.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		is.jwksCalls.Add(1)
		is.mu.Lock()
		raw, status, keys := is.rawJWKS, is.jwksStatus, append([]jose.JSONWebKey(nil), is.keys...)
		is.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if raw != nil {
			_, _ = w.Write(raw)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is, rsaKey, ecKey
}

func (is *issuerServer) setKeys(keys []jose.JSONWebKey) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.rawJWKS = nil
	is.keys = keys
}

func (is *issuerServer) setRawJWKS(b []byte) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.rawJWKS = b
}

func (is *issuerServer) setStatus(code int) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.jwksStatus = code
}

const (
	testAudience = "https://api.example.test"
	testAzp      = "example-console"
	testScope    = "account"
)

func testConfig(issuer string) Config {
	return Config{
		IssuerURL:         issuer,
		Audience:          testAudience,
		AuthorizedParty:   testAzp,
		RequiredScope:     testScope,
		AllowedAlgorithms: []string{"RS256", "ES256"},
		MaxTokenLifetime:  120 * time.Second,
		ClockSkew:         5 * time.Second,
		RequiredClaims: []RequiredClaim{
			{Name: "workspace_id"},
			{Name: "membership_id"},
			{Name: "workspace_role", AllowedValues: []string{"owner", "admin", "member"}},
		},
		ForbiddenClaims: []string{"client_id", "credential_id", "runtime_id", "sponsor_id"},
	}
}

// newTestVerifier builds a verifier against the synthetic issuer and
// waits for discovery (which also primes the key cache).
func newTestVerifier(t *testing.T, is *issuerServer, clock *fakeClock, metrics RefreshMetrics) *Verifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	v, err := NewVerifier(ctx, testConfig(is.srv.URL), metrics,
		WithClock(clock.Now), WithDiscoveryDone(done))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not complete")
	}
	return v
}

// signCompact signs signingInput (b64header.b64payload) manually so tests
// can craft arbitrary bytes go-jose would refuse to serialize.
func signCompact(t *testing.T, signingInput string, key any) string {
	t.Helper()
	digest := sha256.Sum256([]byte(signingInput))
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	default:
		t.Fatalf("unsupported key %T", key)
		return ""
	}
}

func b64seg(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// tokenSpec drives mintToken. Zero values take the valid defaults.
type tokenSpec struct {
	header    map[string]any // replaces default header entries by key
	set       map[string]any // claim overrides / additions
	del       []string       // claims to delete
	rawClaims []byte         // full payload override
	key       any            // signing key (default: RSA)
}

func mintToken(t *testing.T, clock *fakeClock, spec tokenSpec, rsaKey *rsa.PrivateKey) string {
	t.Helper()
	header := map[string]any{"typ": TokenType, "alg": "RS256", "kid": "rsa-1"}
	for k, val := range spec.header {
		if val == nil {
			delete(header, k)
		} else {
			header[k] = val
		}
	}
	now := clock.Now().Unix()
	claims := map[string]any{
		"iss": "", // filled by caller via set (issuer URL is dynamic)
		"aud": testAudience, "azp": testAzp, "scope": testScope,
		"sub": "principal-1", "jti": "jti-1",
		"iat": now, "exp": now + 120,
		"workspace_id": "ws-1", "membership_id": "mem-1", "workspace_role": "owner",
	}
	for k, val := range spec.set {
		claims[k] = val
	}
	for _, k := range spec.del {
		delete(claims, k)
	}
	var payload []byte
	if spec.rawClaims != nil {
		payload = spec.rawClaims
	} else {
		var err error
		payload, err = json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
	}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	key := spec.key
	if key == nil {
		key = rsaKey
	}
	return signCompact(t, b64seg(hb)+"."+b64seg(payload), key)
}

// mint is the valid-token shorthand for a given issuer.
func mint(t *testing.T, clock *fakeClock, issuer string, rsaKey *rsa.PrivateKey, mutate func(*tokenSpec)) string {
	spec := tokenSpec{set: map[string]any{"iss": issuer}}
	if mutate != nil {
		mutate(&spec)
	}
	if _, ok := spec.set["iss"]; !ok {
		spec.set["iss"] = issuer
	}
	return mintToken(t, clock, spec, rsaKey)
}

// --- classification ---

func TestClassify(t *testing.T) {
	hdr := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return b64seg(b)
	}
	valid := hdr(map[string]any{"typ": "at+jwt", "alg": "RS256", "kid": "k"}) + ".eyJhIjoxfQ.c2ln"
	cases := []struct {
		name   string
		rawLen int
		bearer string
		want   bool
	}{
		{"at+jwt owned", len(valid) + 7, valid, true},
		{"agent-style typ not owned", 60, hdr(map[string]any{"typ": "JWT", "alg": "RS256"}) + ".eyJhIjoxfQ.c2ln", false},
		{"missing typ not owned", 60, hdr(map[string]any{"alg": "RS256"}) + ".eyJhIjoxfQ.c2ln", false},
		{"uppercase AT+JWT not owned (exact match)", 70, hdr(map[string]any{"typ": "AT+JWT"}) + ".eyJhIjoxfQ.c2ln", false},
		{"two segments", 40, hdr(map[string]any{"typ": "at+jwt"}) + ".eyJhIjoxfQ", false},
		{"four segments", 60, hdr(map[string]any{"typ": "at+jwt"}) + ".eyJhIjoxfQ.c2ln.extra", false},
		{"empty payload segment", 50, hdr(map[string]any{"typ": "at+jwt"}) + "..c2ln", false},
		{"padded base64 header", 60, hdr(map[string]any{"typ": "at+jwt"}) + "=.eyJhIjoxfQ.c2ln", false},
		{"not json header", 30, b64seg([]byte("hello")) + ".eyJhIjoxfQ.c2ln", false},
		{"api key shape", 40, "e2a_acct_0123456789abcdef", false},
		{"oauth token shape", 40, "ate2a_0123456789abcdef", false},
		{"raw authorization over cap", MaxAuthorizationBytes + 1, valid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.rawLen, tc.bearer); got != tc.want {
				t.Fatalf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyToleratesOtherHeaderMembers is the §10.4 no-fallthrough
// invariant at the classification seam: an at+jwt whose OTHER protected
// header members are oversized or oddly typed (a 129-char kid, a
// non-string alg, a numeric kid) is still delegated-owned. Applying the
// verification-time byte/type pins during classification would misroute
// such a token to a non-delegated credential path — the exact bug this
// guards. The verification-time pins still reject these tokens later, but
// as delegated 401s (see TestVerifyRejections), never as API-key probes.
func TestClassifyToleratesOtherHeaderMembers(t *testing.T) {
	hdr := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return b64seg(b) + ".eyJhIjoxfQ.c2ln"
	}
	cases := []struct {
		name   string
		bearer string
	}{
		{"kid over 128 bytes", hdr(map[string]any{"typ": "at+jwt", "alg": "RS256", "kid": strings.Repeat("k", 129)})},
		{"alg is an object", hdr(map[string]any{"typ": "at+jwt", "alg": map[string]any{"x": 1}, "kid": "k1"})},
		{"kid is a number", hdr(map[string]any{"typ": "at+jwt", "alg": "RS256", "kid": 5})},
		{"alg over 16 bytes", hdr(map[string]any{"typ": "at+jwt", "alg": strings.Repeat("R", 17), "kid": "k1"})},
		{"unknown array member", hdr(map[string]any{"typ": "at+jwt", "crit": []string{"a", "b"}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !Classify(len(tc.bearer)+7, tc.bearer) {
				t.Fatalf("at+jwt with an odd OTHER header member must stay delegated-owned")
			}
		})
	}
}

func TestClassifyHeaderSizeBoundary(t *testing.T) {
	// Decoded protected header of exactly MaxProtectedHeaderBytes is
	// classifiable; one byte more is not.
	build := func(decodedLen int) string {
		pad := decodedLen - len(`{"typ":"at+jwt","pad":""}`)
		h := fmt.Sprintf(`{"typ":"at+jwt","pad":"%s"}`, strings.Repeat("x", pad))
		if len(h) != decodedLen {
			t.Fatalf("construction error: %d != %d", len(h), decodedLen)
		}
		return b64seg([]byte(h)) + ".eyJhIjoxfQ.c2ln"
	}
	if !Classify(MaxProtectedHeaderBytes+100, build(MaxProtectedHeaderBytes)) {
		t.Fatal("header at limit should classify")
	}
	if Classify(MaxProtectedHeaderBytes+100, build(MaxProtectedHeaderBytes+1)) {
		t.Fatal("header at limit+1 must not classify")
	}
}

func TestClassifyCompactSizeBoundary(t *testing.T) {
	hdrSeg := b64seg([]byte(`{"typ":"at+jwt"}`))
	build := func(total int) string {
		rest := total - len(hdrSeg) - 2 // two dots
		payloadLen := rest / 2
		sigLen := rest - payloadLen
		tok := hdrSeg + "." + strings.Repeat("A", payloadLen) + "." + strings.Repeat("A", sigLen)
		if len(tok) != total {
			t.Fatalf("construction error: %d != %d", len(tok), total)
		}
		return tok
	}
	// A bare (schemeless) credential exercises the compact cap alone; with
	// a "Bearer " prefix the raw-header cap binds 7 bytes sooner.
	if !Classify(MaxCompactJWTBytes, build(MaxCompactJWTBytes)) {
		t.Fatal("compact token at limit should classify")
	}
	if Classify(MaxCompactJWTBytes+1, build(MaxCompactJWTBytes+1)) {
		t.Fatal("compact token at limit+1 must not classify")
	}
}

// --- verification ---

func TestVerifyHappyPathBothAlgorithms(t *testing.T) {
	is, rsaKey, ecKey := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)

	rsaTok := mint(t, clock, is.srv.URL, rsaKey, nil)
	claims, err := v.Verify(context.Background(), rsaTok)
	if err != nil {
		t.Fatalf("RS256 verify: %v", err)
	}
	if claims.Issuer != is.srv.URL || claims.Subject != "principal-1" {
		t.Fatalf("unexpected claims %+v", claims)
	}

	ecTok := mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
		s.header = map[string]any{"alg": "ES256", "kid": "ec-1"}
		s.key = ecKey
	})
	if _, err := v.Verify(context.Background(), ecTok); err != nil {
		t.Fatalf("ES256 verify: %v", err)
	}
}

func TestVerifyRejections(t *testing.T) {
	is, rsaKey, ecKey := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)
	issuer := is.srv.URL
	now := clock.Now().Unix()

	otherRSA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	longASCII := func(n int) string { return strings.Repeat("a", n) }
	// Supplementary-plane rune: U+20000, 4 UTF-8 bytes, 1 code point.
	supp := func(n int) string { return strings.Repeat("\U00020000", n) }

	cases := []struct {
		name   string
		mutate func(*tokenSpec)
	}{
		{"bad signature (wrong key)", func(s *tokenSpec) { s.key = otherRSA }},
		{"alg none", func(s *tokenSpec) { s.header = map[string]any{"alg": "none"} }},
		{"alg HS256", func(s *tokenSpec) { s.header = map[string]any{"alg": "HS256"} }},
		{"missing kid", func(s *tokenSpec) { s.header = map[string]any{"kid": nil} }},
		{"kid over 128 bytes", func(s *tokenSpec) { s.header = map[string]any{"kid": longASCII(129)} }},
		{"alg/key mismatch (ES256 header, RSA kid)", func(s *tokenSpec) {
			s.header = map[string]any{"alg": "ES256", "kid": "rsa-1"}
			s.key = ecKey
		}},
		{"issuer mismatch", func(s *tokenSpec) { s.set["iss"] = issuer + "/" }},
		{"audience mismatch", func(s *tokenSpec) { s.set["aud"] = "https://other.example.test" }},
		{"audience array with right value", func(s *tokenSpec) { s.set["aud"] = []string{testAudience} }},
		{"audience array with only right value", func(s *tokenSpec) { s.set["aud"] = []any{testAudience} }},
		{"missing azp", func(s *tokenSpec) { s.del = []string{"azp"} }},
		{"azp mismatch", func(s *tokenSpec) { s.set["azp"] = "other-console" }},
		{"scope superset containing account", func(s *tokenSpec) { s.set["scope"] = "account extra" }},
		{"scope empty", func(s *tokenSpec) { s.set["scope"] = "" }},
		{"scope different", func(s *tokenSpec) { s.set["scope"] = "agent" }},
		{"missing scope", func(s *tokenSpec) { s.del = []string{"scope"} }},
		{"missing sub", func(s *tokenSpec) { s.del = []string{"sub"} }},
		{"empty sub", func(s *tokenSpec) { s.set["sub"] = "" }},
		{"sub over 128 code points", func(s *tokenSpec) { s.set["sub"] = longASCII(129) }},
		{"sub at 129 supplementary code points", func(s *tokenSpec) { s.set["sub"] = supp(129) }},
		{"missing jti", func(s *tokenSpec) { s.del = []string{"jti"} }},
		{"empty jti", func(s *tokenSpec) { s.set["jti"] = "" }},
		{"missing iat", func(s *tokenSpec) { s.del = []string{"iat"} }},
		{"missing exp", func(s *tokenSpec) { s.del = []string{"exp"} }},
		{"iat 6s in the future (skew is 5)", func(s *tokenSpec) {
			s.set["iat"] = now + 6
			s.set["exp"] = now + 126
		}},
		{"expired 6s ago (skew is 5)", func(s *tokenSpec) {
			s.set["iat"] = now - 126
			s.set["exp"] = now - 6
		}},
		{"exp equals iat", func(s *tokenSpec) {
			s.set["iat"] = now
			s.set["exp"] = now
		}},
		{"lifetime 121s", func(s *tokenSpec) {
			s.set["iat"] = now
			s.set["exp"] = now + 121
		}},
		{"iat as string", func(s *tokenSpec) { s.set["iat"] = "123" }},
		{"exp fractional", func(s *tokenSpec) { s.set["exp"] = float64(now) + 0.5 }},
		{"exp over int64", func(s *tokenSpec) { s.set["exp"] = json.Number("99999999999999999999") }},
		{"forbidden claim client_id", func(s *tokenSpec) { s.set["client_id"] = "abc" }},
		{"forbidden claim null credential_id", func(s *tokenSpec) { s.set["credential_id"] = nil }},
		{"forbidden claim runtime_id", func(s *tokenSpec) { s.set["runtime_id"] = "r" }},
		{"forbidden claim sponsor_id", func(s *tokenSpec) { s.set["sponsor_id"] = "s" }},
		{"missing workspace_id", func(s *tokenSpec) { s.del = []string{"workspace_id"} }},
		{"missing membership_id", func(s *tokenSpec) { s.del = []string{"membership_id"} }},
		{"missing workspace_role", func(s *tokenSpec) { s.del = []string{"workspace_role"} }},
		{"workspace_role not allowed", func(s *tokenSpec) { s.set["workspace_role"] = "superadmin" }},
		{"workspace_id as object", func(s *tokenSpec) { s.set["workspace_id"] = map[string]string{"id": "x"} }},
		{"workspace_id as array", func(s *tokenSpec) { s.set["workspace_id"] = []string{"x"} }},
		{"workspace_id as number", func(s *tokenSpec) { s.set["workspace_id"] = 7 }},
		{"workspace_id over 129 code points", func(s *tokenSpec) { s.set["workspace_id"] = longASCII(129) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := mint(t, clock, issuer, rsaKey, tc.mutate)
			_, err := v.Verify(context.Background(), tok)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

func TestVerifyBoundaryAcceptance(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)
	now := clock.Now().Unix()

	cases := []struct {
		name   string
		mutate func(*tokenSpec)
	}{
		{"lifetime exactly 120s", nil},
		{"iat 5s in the future (at skew)", func(s *tokenSpec) {
			s.set["iat"] = now + 5
			s.set["exp"] = now + 125
		}},
		{"expired 4s ago (within skew)", func(s *tokenSpec) {
			s.set["iat"] = now - 124
			s.set["exp"] = now - 4
		}},
		{"sub at 128 ASCII code points", func(s *tokenSpec) { s.set["sub"] = strings.Repeat("a", 128) }},
		{"sub at 128 supplementary code points / 512 bytes", func(s *tokenSpec) {
			s.set["sub"] = strings.Repeat("\U00020000", 128)
		}},
		{"multibyte workspace_id within limits", func(s *tokenSpec) {
			s.set["workspace_id"] = strings.Repeat("é", 128) // 128 cp, 256 bytes
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := mint(t, clock, is.srv.URL, rsaKey, tc.mutate)
			if _, err := v.Verify(context.Background(), tok); err != nil {
				t.Fatalf("boundary case should verify: %v", err)
			}
		})
	}
}

func TestVerifyClaimsObjectShape(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)
	issuer := is.srv.URL
	now := clock.Now().Unix()

	base := func() map[string]any {
		return map[string]any{
			"iss": issuer, "aud": testAudience, "azp": testAzp, "scope": testScope,
			"sub": "p", "jti": "j", "iat": now, "exp": now + 120,
			"workspace_id": "w", "membership_id": "m", "workspace_role": "owner",
		}
	}

	t.Run("32 claims accepted, 33 rejected", func(t *testing.T) {
		claims := base() // 11 members
		for i := 0; len(claims) < MaxTopLevelClaims; i++ {
			claims[fmt.Sprintf("extra_%02d", i)] = "x"
		}
		b, _ := json.Marshal(claims)
		tok := mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("32 claims should verify: %v", err)
		}
		claims["one_more"] = "x"
		b, _ = json.Marshal(claims)
		tok = mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("33 claims must reject, got %v", err)
		}
	})

	t.Run("duplicate claim rejected", func(t *testing.T) {
		b, _ := json.Marshal(base())
		// Inject a duplicate sub before the closing brace.
		raw := b[:len(b)-1]
		raw = append(raw, []byte(`,"sub":"other"}`)...)
		tok := mintToken(t, clock, tokenSpec{rawClaims: raw}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("duplicate claim must reject, got %v", err)
		}
	})

	t.Run("claim name at 128 bytes accepted, 129 rejected", func(t *testing.T) {
		claims := base()
		claims[strings.Repeat("n", MaxClaimNameBytes)] = "x"
		b, _ := json.Marshal(claims)
		tok := mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("128-byte claim name should verify: %v", err)
		}
		delete(claims, strings.Repeat("n", MaxClaimNameBytes))
		claims[strings.Repeat("n", MaxClaimNameBytes+1)] = "x"
		b, _ = json.Marshal(claims)
		tok = mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("129-byte claim name must reject, got %v", err)
		}
	})

	t.Run("non-ASCII claim name rejected", func(t *testing.T) {
		claims := base()
		claims["namé"] = "x"
		b, _ := json.Marshal(claims)
		tok := mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("non-ASCII claim name must reject, got %v", err)
		}
	})

	t.Run("C0-control claim name rejected", func(t *testing.T) {
		claims := base()
		claims["na\tme"] = "x"
		b, _ := json.Marshal(claims)
		tok := mintToken(t, clock, tokenSpec{rawClaims: b}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("control-character claim name must reject, got %v", err)
		}
	})

	t.Run("claims array payload rejected", func(t *testing.T) {
		tok := mintToken(t, clock, tokenSpec{rawClaims: []byte(`["not","an","object"]`)}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("array claims must reject, got %v", err)
		}
	})

	t.Run("payload at 8192 decoded bytes verifies, 8193 rejects", func(t *testing.T) {
		pad := func(total int) []byte {
			claims := base()
			b, _ := json.Marshal(claims)
			need := total - len(b) - len(`,"pad":""`)
			claims["pad"] = strings.Repeat("x", need)
			out, _ := json.Marshal(claims)
			if len(out) != total {
				t.Fatalf("construction error: %d != %d", len(out), total)
			}
			return out
		}
		tok := mintToken(t, clock, tokenSpec{rawClaims: pad(MaxPayloadBytes)}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("payload at limit should verify: %v", err)
		}
		tok = mintToken(t, clock, tokenSpec{rawClaims: pad(MaxPayloadBytes + 1)}, rsaKey)
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("payload at limit+1 must reject, got %v", err)
		}
	})
}

// --- JWKS cache and refresh policy ---

func TestJWKSRotationUnknownKidRefresh(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	metrics := &recordedMetrics{}
	v := newTestVerifier(t, is, clock, metrics)

	// Rotate: publish a second RSA key, sign with it.
	nextKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is.setKeys([]jose.JSONWebKey{
		{Key: rsaKey.Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"},
		{Key: nextKey.Public(), KeyID: "rsa-2", Algorithm: "RS256", Use: "sig"},
	})
	tok := mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
		s.header = map[string]any{"kid": "rsa-2"}
		s.key = nextKey
	})
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("rotated key should verify after refresh: %v", err)
	}
	found := false
	for _, o := range metrics.all() {
		if o == "success" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a success refresh outcome, got %v", metrics.all())
	}
}

func TestJWKSUnknownKidAfterSuccessfulRefreshIsInvalid(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	metrics := &recordedMetrics{}
	v := newTestVerifier(t, is, clock, metrics)

	tok := mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
		s.header = map[string]any{"kid": "nope"}
	})
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown kid after successful refresh must be invalid (401), got %v", err)
	}
	sawAbsent := false
	for _, o := range metrics.all() {
		if o == "key_absent" {
			sawAbsent = true
		}
	}
	if !sawAbsent {
		t.Fatalf("expected key_absent outcome, got %v", metrics.all())
	}
}

func TestJWKSRefreshRateLimitAndCooldown(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	metrics := &recordedMetrics{}
	v := newTestVerifier(t, is, clock, metrics)

	unknown := func() string {
		return mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
			s.header = map[string]any{"kid": "nope"}
		})
	}

	// First unknown-kid lookup consumes the burst token (refresh runs,
	// kid absent -> 401).
	if _, err := v.Verify(context.Background(), unknown()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want invalid, got %v", err)
	}
	fetches := is.jwksCalls.Load()

	// Immediately after, the bucket is empty: 503 without a fetch.
	if _, err := v.Verify(context.Background(), unknown()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable while rate limited, got %v", err)
	}
	if is.jwksCalls.Load() != fetches {
		t.Fatal("rate-limited attempt must not fetch")
	}

	// Refill: 6 per 60s -> one token after 10s.
	clock.Advance(10 * time.Second)
	if _, err := v.Verify(context.Background(), unknown()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want invalid after refill, got %v", err)
	}
	if is.jwksCalls.Load() != fetches+1 {
		t.Fatal("refilled attempt should fetch once")
	}

	// Now make the issuer fail: next refresh (after refill) arms the
	// 10-second cooldown.
	is.setStatus(http.StatusInternalServerError)
	clock.Advance(10 * time.Second)
	if _, err := v.Verify(context.Background(), unknown()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable on transport failure, got %v", err)
	}
	fetches = is.jwksCalls.Load()

	// Within the cooldown, no fetch happens even with a refilled bucket.
	clock.Advance(9 * time.Second)
	if _, err := v.Verify(context.Background(), unknown()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable during cooldown, got %v", err)
	}
	if is.jwksCalls.Load() != fetches {
		t.Fatal("cooldown attempt must not fetch")
	}

	sawRateLimited, sawTransport := false, false
	for _, o := range metrics.all() {
		switch o {
		case "rate_limited":
			sawRateLimited = true
		case "transport_error":
			sawTransport = true
		}
	}
	if !sawRateLimited || !sawTransport {
		t.Fatalf("expected rate_limited and transport_error outcomes, got %v", metrics.all())
	}
}

func TestJWKSFreshAndStaleWindows(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)

	known := func() string { return mint(t, clock, is.srv.URL, rsaKey, nil) }

	// Kill the issuer's JWKS endpoint: from here on only the cache serves,
	// and every attempted refresh fails (a 500). Record the baseline fetch
	// count so we can prove exactly WHEN a refresh is attempted — the fresh
	// window (600s) must be distinguishable from the stale grace (900s).
	is.setStatus(http.StatusInternalServerError)
	baseFetches := is.jwksCalls.Load()

	// Within the fresh window (<=600s) a known key verifies with NO refresh
	// attempt: the cache is authoritative, so the issuer is never touched.
	clock.Advance(599 * time.Second)
	if _, err := v.Verify(context.Background(), known()); err != nil {
		t.Fatalf("fresh known key should verify during issuer outage: %v", err)
	}
	if got := is.jwksCalls.Load(); got != baseFetches {
		t.Fatalf("a fresh known key must not trigger a refresh: %d fetches, want %d", got, baseFetches)
	}

	// Just past the fresh window a refresh MUST be attempted (this is the
	// 600s turnover §10.7's retention math depends on). The refresh fails
	// (issuer down), so a known kid still serves out of the stale grace —
	// but the fetch was made.
	clock.Advance(2 * time.Second) // age 601s
	if _, err := v.Verify(context.Background(), known()); err != nil {
		t.Fatalf("stale-but-known key should still verify on a failed refresh: %v", err)
	}
	if got := is.jwksCalls.Load(); got != baseFetches+1 {
		t.Fatalf("crossing the 600s fresh window must attempt exactly one refresh: %d fetches, want %d", got, baseFetches+1)
	}

	// Still within the stale grace (600..900s): a known key keeps verifying
	// even though refreshes keep failing (rate-limited / cooldown).
	clock.Advance(298 * time.Second) // age 899s
	if _, err := v.Verify(context.Background(), known()); err != nil {
		t.Fatalf("stale known key within grace should verify: %v", err)
	}

	// Past 900s even a known key is unavailable when refresh fails.
	clock.Advance(2 * time.Second) // age 901s
	if _, err := v.Verify(context.Background(), known()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("known key past stale grace must be unavailable, got %v", err)
	}

	// Issuer recovers: the next allowed refresh restores verification.
	is.setStatus(http.StatusOK)
	clock.Advance(11 * time.Second) // clear cooldown + refill
	if _, err := v.Verify(context.Background(), known()); err != nil {
		t.Fatalf("recovered issuer should verify again: %v", err)
	}
}

// TestJWKSRotatedOutKeyStopsVerifyingOnRefresh proves the fresh-window
// turnover matters for a removed/compromised key: while fresh it still
// verifies from cache, but past 600s the attempted refresh SUCCEEDS with
// the key gone, so it immediately becomes an unknown kid (401) rather
// than lingering to the 900s stale bound.
func TestJWKSRotatedOutKeyStopsVerifyingOnRefresh(t *testing.T) {
	is, rsaKey, ecKey := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)

	rsaTok := func() string { return mint(t, clock, is.srv.URL, rsaKey, nil) }

	// Fresh: the RSA key verifies.
	if _, err := v.Verify(context.Background(), rsaTok()); err != nil {
		t.Fatalf("fresh key should verify: %v", err)
	}

	// Rotate the RSA key OUT (issuer now publishes only the EC key), issuer
	// healthy. While still fresh, the cache keeps verifying the old key.
	is.setKeys([]jose.JSONWebKey{{Key: ecKey.Public(), KeyID: "ec-1", Algorithm: "ES256", Use: "sig"}})
	clock.Advance(599 * time.Second)
	if _, err := v.Verify(context.Background(), rsaTok()); err != nil {
		t.Fatalf("within the fresh window a cached key still verifies: %v", err)
	}

	// Past 600s the refresh succeeds and the rotated-out key is gone: 401,
	// not served from the stale grace.
	clock.Advance(2 * time.Second) // age 601s
	if _, err := v.Verify(context.Background(), rsaTok()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("rotated-out key past the fresh window must be invalid (401), got %v", err)
	}
}

func TestJWKSSingleflight(t *testing.T) {
	is, rsaKey, _ := newIssuerServer(t)
	clock := &fakeClock{now: time.Now()}
	v := newTestVerifier(t, is, clock, nil)

	// Rotate so every goroutine sees an unknown kid at once.
	nextKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is.setKeys([]jose.JSONWebKey{{Key: nextKey.Public(), KeyID: "rsa-2", Algorithm: "RS256", Use: "sig"}})
	before := is.jwksCalls.Load()
	tok := mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
		s.header = map[string]any{"kid": "rsa-2"}
		s.key = nextKey
	})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), tok)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := is.jwksCalls.Load() - before; got != 1 {
		t.Fatalf("singleflight should fetch once, fetched %d times", got)
	}
}

func TestJWKSMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("not-json")},
		{"over byte cap", []byte(`{"keys":[` + strings.Repeat(" ", MaxJWKSBytes) + `]}`)},
		{"over key cap", buildOversizedKeySet(MaxJWKSKeys + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is, rsaKey, _ := newIssuerServer(t)
			clock := &fakeClock{now: time.Now()}
			metrics := &recordedMetrics{}
			v := newTestVerifier(t, is, clock, metrics)

			// Malformed refresh retains the last good set: known keys keep
			// verifying; the unknown-kid token is 503.
			is.setRawJWKS(tc.raw)
			unknown := mint(t, clock, is.srv.URL, rsaKey, func(s *tokenSpec) {
				s.header = map[string]any{"kid": "nope"}
			})
			if _, err := v.Verify(context.Background(), unknown); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("malformed refresh must be unavailable, got %v", err)
			}
			known := mint(t, clock, is.srv.URL, rsaKey, nil)
			if _, err := v.Verify(context.Background(), known); err != nil {
				t.Fatalf("last good set must be retained: %v", err)
			}
		})
	}
}

func buildOversizedKeySet(n int) []byte {
	keys := make([]map[string]string, 0, n)
	// Same public key relabeled n times — enough to trip the count cap.
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwk := jose.JSONWebKey{Key: k.Public(), KeyID: "x", Algorithm: "RS256", Use: "sig"}
	raw, _ := jwk.MarshalJSON()
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	for i := 0; i < n; i++ {
		mi := make(map[string]string, len(m))
		for kk, vv := range m {
			mi[kk] = vv
		}
		mi["kid"] = fmt.Sprintf("k-%03d", i)
		keys = append(keys, mi)
	}
	out, _ := json.Marshal(map[string]any{"keys": keys})
	return out
}

// TestDiscoveryBodyCapped proves an oversized OIDC discovery document is
// not read unbounded. The doc is OTHERWISE valid (correct issuer +
// jwks_uri, and a live JWKS), so without the cap discovery would succeed
// and the token would verify; with the cap the discovery read fails, so
// discovery never completes and delegated tokens stay 503.
func TestDiscoveryBodyCapped(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// Valid fields plus a padded member go-oidc ignores; the whole
		// document is deliberately over maxDiscoveryBytes.
		pad := strings.Repeat("x", maxDiscoveryBytes+1024)
		_, _ = w.Write([]byte(`{"issuer":"` + base + `","jwks_uri":"` + base + `/jwks","pad":"` + pad + `"}`))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: rsaKey.Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &fakeClock{now: time.Now()}
	v, err := NewVerifier(ctx, testConfig(srv.URL), nil, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("construction must not fail on issuer transport: %v", err)
	}
	tok := mint(t, clock, srv.URL, rsaKey, nil)
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an oversized discovery document must leave the verifier unavailable, got %v", err)
	}
}

func TestVerifierNotReadyIsUnavailable(t *testing.T) {
	// An issuer that never answers discovery: the verifier constructs,
	// but delegated tokens are 503 until discovery succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &fakeClock{now: time.Now()}
	v, err := NewVerifier(ctx, testConfig(srv.URL), nil, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("network unavailability must not fail construction: %v", err)
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := mint(t, clock, srv.URL, rsaKey, nil)
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("undiscovered issuer must be unavailable, got %v", err)
	}
}

func TestNewVerifierStaticValidation(t *testing.T) {
	base := testConfig("https://issuer.example.test")
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty issuer", func(c *Config) { c.IssuerURL = "" }},
		{"empty audience", func(c *Config) { c.Audience = "" }},
		{"empty azp", func(c *Config) { c.AuthorizedParty = "" }},
		{"empty scope", func(c *Config) { c.RequiredScope = "" }},
		{"no algorithms", func(c *Config) { c.AllowedAlgorithms = nil }},
		{"unsupported algorithm", func(c *Config) { c.AllowedAlgorithms = []string{"HS256"} }},
		{"zero lifetime", func(c *Config) { c.MaxTokenLifetime = 0 }},
		{"negative skew", func(c *Config) { c.ClockSkew = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.AllowedAlgorithms = append([]string(nil), base.AllowedAlgorithms...)
			tc.mutate(&cfg)
			if _, err := NewVerifier(context.Background(), cfg, nil); err == nil {
				t.Fatal("want construction error")
			}
		})
	}
}
