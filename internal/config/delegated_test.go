package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validDelegatedConfig is a fully populated enabled policy using
// synthetic deployment values.
func validDelegatedConfig() DelegatedConfig {
	return DelegatedConfig{
		Enabled:                 true,
		IssuerURL:               "https://issuer.example.test/oidc",
		Audience:                "https://api.example.test",
		AuthorizedParty:         "example-console",
		RequiredScope:           "account",
		AllowedAlgorithms:       []string{"RS256", "ES256"},
		MaxTokenLifetimeSeconds: 120,
		ClockSkewSeconds:        5,
		RequiredClaims: []DelegatedClaimConfig{
			{Name: "workspace_id"},
			{Name: "membership_id"},
			{Name: "workspace_role", AllowedValues: []string{"owner", "admin", "member"}},
		},
		ForbiddenClaims: []string{"client_id", "credential_id", "runtime_id", "sponsor_id"},
	}
}

func baseConfigWithDelegated(d DelegatedConfig, env string) *Config {
	return &Config{
		Env:       env,
		Signing:   SigningConfig{HMACSecret: strings.Repeat("x", 64)},
		Trash:     TrashConfig{RetentionDays: 30},
		Delegated: d,
	}
}

// TestLoadDelegatedEnabledForcesExplicitClaimPolicy proves the emptied
// claim defaults are load-bearing (N10): enabling the verifier with
// issuer/audience/authorized_party/scope but NO required/forbidden claim
// lists is a startup error — OSS ships no operator-specific claim policy
// to silently fall back on.
func TestLoadDelegatedEnabledForcesExplicitClaimPolicy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
env: "development"
signing:
  hmac_secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
delegated:
  enabled: true
  issuer_url: "https://issuer.example.test/oidc"
  audience: "https://api.example.test"
  authorized_party: "example-console"
  required_scope: "account"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("enabling delegated with no required/forbidden claim lists must fail to load")
	} else if !strings.Contains(err.Error(), "required_claims") && !strings.Contains(err.Error(), "forbidden_claims") {
		t.Fatalf("error should name the missing claim lists, got: %v", err)
	}
}

func TestValidateDelegatedDisabledIsInert(t *testing.T) {
	// A disabled block is never validated — garbage fields must not block
	// startup for deployments that have not opted in.
	d := DelegatedConfig{Enabled: false, IssuerURL: "::not a url::"}
	if err := baseConfigWithDelegated(d, "production").Validate(); err != nil {
		t.Fatalf("disabled delegated config must not be validated: %v", err)
	}
}

func TestValidateDelegatedAcceptsValidConfig(t *testing.T) {
	if err := baseConfigWithDelegated(validDelegatedConfig(), "production").Validate(); err != nil {
		t.Fatalf("valid enabled config rejected: %v", err)
	}
}

func TestValidateDelegatedHTTPIssuerAllowedInDevelopment(t *testing.T) {
	d := validDelegatedConfig()
	d.IssuerURL = "http://localhost:8080/oidc"
	if err := baseConfigWithDelegated(d, "development").Validate(); err != nil {
		t.Fatalf("http issuer must be allowed in development: %v", err)
	}
	if err := baseConfigWithDelegated(d, "production").Validate(); err == nil {
		t.Fatal("http issuer must be fatal in production")
	}
}

func TestValidateDelegatedRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DelegatedConfig)
	}{
		{"missing issuer", func(d *DelegatedConfig) { d.IssuerURL = "" }},
		{"malformed issuer", func(d *DelegatedConfig) { d.IssuerURL = "not-a-url" }},
		{"issuer with query", func(d *DelegatedConfig) { d.IssuerURL = "https://issuer.example.test/oidc?x=1" }},
		{"issuer with fragment", func(d *DelegatedConfig) { d.IssuerURL = "https://issuer.example.test/oidc#frag" }},
		{"missing audience", func(d *DelegatedConfig) { d.Audience = "" }},
		{"missing authorized party", func(d *DelegatedConfig) { d.AuthorizedParty = "" }},
		{"missing scope", func(d *DelegatedConfig) { d.RequiredScope = "" }},
		{"multi-token scope", func(d *DelegatedConfig) { d.RequiredScope = "account extra" }},
		{"empty algorithms", func(d *DelegatedConfig) { d.AllowedAlgorithms = nil }},
		{"unsupported algorithm", func(d *DelegatedConfig) { d.AllowedAlgorithms = []string{"RS256", "HS256"} }},
		{"duplicate algorithm", func(d *DelegatedConfig) { d.AllowedAlgorithms = []string{"RS256", "RS256"} }},
		{"zero lifetime", func(d *DelegatedConfig) { d.MaxTokenLifetimeSeconds = 0 }},
		{"negative lifetime", func(d *DelegatedConfig) { d.MaxTokenLifetimeSeconds = -1 }},
		{"negative skew", func(d *DelegatedConfig) { d.ClockSkewSeconds = -1 }},
		{"empty required claims", func(d *DelegatedConfig) { d.RequiredClaims = nil }},
		{"empty forbidden claims", func(d *DelegatedConfig) { d.ForbiddenClaims = nil }},
		{"duplicate required claim", func(d *DelegatedConfig) {
			d.RequiredClaims = append(d.RequiredClaims, DelegatedClaimConfig{Name: "workspace_id"})
		}},
		{"duplicate forbidden claim", func(d *DelegatedConfig) {
			d.ForbiddenClaims = append(d.ForbiddenClaims, "client_id")
		}},
		{"claim required and forbidden", func(d *DelegatedConfig) {
			d.ForbiddenClaims = append(d.ForbiddenClaims, "workspace_id")
		}},
		{"reserved required claim", func(d *DelegatedConfig) {
			d.RequiredClaims = append(d.RequiredClaims, DelegatedClaimConfig{Name: "iss"})
		}},
		{"reserved forbidden claim", func(d *DelegatedConfig) {
			d.ForbiddenClaims = append(d.ForbiddenClaims, "scope")
		}},
		{"empty claim name", func(d *DelegatedConfig) {
			d.RequiredClaims = append(d.RequiredClaims, DelegatedClaimConfig{Name: ""})
		}},
		{"claim name over 128 bytes", func(d *DelegatedConfig) {
			d.RequiredClaims = append(d.RequiredClaims, DelegatedClaimConfig{Name: strings.Repeat("n", 129)})
		}},
		{"non-ASCII claim name", func(d *DelegatedConfig) {
			d.ForbiddenClaims = append(d.ForbiddenClaims, "namé")
		}},
		{"empty allowed value", func(d *DelegatedConfig) {
			d.RequiredClaims[2].AllowedValues = []string{"owner", ""}
		}},
		{"duplicate allowed value", func(d *DelegatedConfig) {
			d.RequiredClaims[2].AllowedValues = []string{"owner", "owner"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDelegatedConfig()
			// Deep-enough copies for the slices the mutations touch.
			d.AllowedAlgorithms = append([]string(nil), d.AllowedAlgorithms...)
			d.ForbiddenClaims = append([]string(nil), d.ForbiddenClaims...)
			claims := make([]DelegatedClaimConfig, len(d.RequiredClaims))
			copy(claims, d.RequiredClaims)
			for i := range claims {
				claims[i].AllowedValues = append([]string(nil), claims[i].AllowedValues...)
			}
			d.RequiredClaims = claims
			tc.mutate(&d)
			if err := baseConfigWithDelegated(d, "development").Validate(); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}
