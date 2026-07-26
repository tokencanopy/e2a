package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutboundModeConfigurationRemoved is a source-level contract guard. Outbound
// delivery is always queue-first for GA, so neither the legacy environment switch
// nor its configuration model may quietly return in a later refactor.
func TestOutboundModeConfigurationRemoved(t *testing.T) {
	t.Helper()
	files := []string{
		"config.go",
		filepath.Join("..", "..", "cmd", "e2a", "main.go"),
		filepath.Join("..", "..", "config.example.yaml"),
	}
	forbidden := []string{"E2A_OUTBOUND_MODE", "OutboundConfig", "cfg.Outbound.Mode"}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, token := range forbidden {
			if strings.Contains(string(body), token) {
				t.Errorf("%s still contains removed outbound-mode token %q", path, token)
			}
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(`
smtp:
  listen_addr: ":3025"
  domain: "test.e2a.dev"
http:
  listen_addr: ":9090"
database:
  url: "postgres://test:test@localhost/test"
signing:
  hmac_secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
env: "production"
outbound_smtp:
  host: "smtp.example.com"
  port: 465
  from_domain: "mail.e2a.dev"
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.SMTP.ListenAddr != ":3025" {
		t.Errorf("SMTP.ListenAddr = %q, want :3025", cfg.SMTP.ListenAddr)
	}
	if cfg.SMTP.Domain != "test.e2a.dev" {
		t.Errorf("SMTP.Domain = %q, want test.e2a.dev", cfg.SMTP.Domain)
	}
	if cfg.HTTP.ListenAddr != ":9090" {
		t.Errorf("HTTP.ListenAddr = %q, want :9090", cfg.HTTP.ListenAddr)
	}
	if cfg.Database.URL != "postgres://test:test@localhost/test" {
		t.Errorf("Database.URL = %q", cfg.Database.URL)
	}
	if cfg.Signing.HMACSecret != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("Signing.HMACSecret = %q", cfg.Signing.HMACSecret)
	}
	if cfg.Env != "production" {
		t.Errorf("Env = %q, want production", cfg.Env)
	}
	if cfg.OutboundSMTP.Host != "smtp.example.com" {
		t.Errorf("OutboundSMTP.Host = %q", cfg.OutboundSMTP.Host)
	}
	if cfg.OutboundSMTP.Port != 465 {
		t.Errorf("OutboundSMTP.Port = %d, want 465", cfg.OutboundSMTP.Port)
	}
	if cfg.OutboundSMTP.FromDomain != "mail.e2a.dev" {
		t.Errorf("OutboundSMTP.FromDomain = %q", cfg.OutboundSMTP.FromDomain)
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
database:
  url: "postgres://original"
signing:
  hmac_secret: "original"
`), 0644)

	// Only secrets get env overrides
	t.Setenv("E2A_DATABASE_URL", "postgres://override")
	t.Setenv("E2A_HMAC_SECRET", "override-secret")
	t.Setenv("E2A_OUTBOUND_SMTP_USERNAME", "smtp-user")
	t.Setenv("E2A_OUTBOUND_SMTP_PASSWORD", "smtp-pass")
	// A non-PEM sentinel: the config layer only copies the string through to
	// cfg.OAuth.SigningKey (parsing happens later in agentauth.NewSigner), so
	// this needs no real key — and deliberately omits the "BEGIN ... PRIVATE
	// KEY" armor so secret scanners don't false-positive on a test fixture.
	t.Setenv("E2A_OAUTH_SIGNING_KEY", "signing-key-sentinel-not-a-real-pem")
	t.Setenv("E2A_OAUTH_SIGNING_KID", "k7")
	t.Setenv("E2A_WEBHOOK_INTERNAL_SINK_URL", "http://prober:8090/sink")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.OAuth.SigningKey == "" || cfg.OAuth.SigningKID != "k7" {
		t.Errorf("expected env override for OAuth signing key/kid, got key=%q kid=%q", cfg.OAuth.SigningKey, cfg.OAuth.SigningKID)
	}

	if cfg.Database.URL != "postgres://override" {
		t.Errorf("expected env override for Database.URL, got %q", cfg.Database.URL)
	}
	if cfg.Signing.HMACSecret != "override-secret" {
		t.Errorf("expected env override for HMACSecret, got %q", cfg.Signing.HMACSecret)
	}
	if cfg.OutboundSMTP.Username != "smtp-user" {
		t.Errorf("expected env override for OutboundSMTP.Username, got %q", cfg.OutboundSMTP.Username)
	}
	if cfg.OutboundSMTP.Password != "smtp-pass" {
		t.Errorf("expected env override for OutboundSMTP.Password, got %q", cfg.OutboundSMTP.Password)
	}
	if cfg.Webhook.InternalSinkURL != "http://prober:8090/sink" {
		t.Errorf("expected env override for Webhook.InternalSinkURL, got %q", cfg.Webhook.InternalSinkURL)
	}
}

func TestValidateProductionRejectsPlaceholderHMAC(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "production"
signing:
  hmac_secret: "change-me-in-production"
`), 0644)

	if _, err := Load(cfgPath); err == nil {
		t.Fatal("Load should refuse placeholder HMAC secret in production")
	}
}

func TestValidateProductionRejectsEmptyHMAC(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "production"
signing:
  hmac_secret: ""
`), 0644)

	if _, err := Load(cfgPath); err == nil {
		t.Fatal("Load should refuse empty HMAC secret in production")
	}
}

func TestValidateProductionRejectsShortHMAC(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "production"
signing:
  hmac_secret: "tooshort"
`), 0644)

	if _, err := Load(cfgPath); err == nil {
		t.Fatal("Load should refuse HMAC secret shorter than 32 bytes in production")
	}
}

func TestValidateProductionAcceptsLongHMAC(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "production"
signing:
  hmac_secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
`), 0644)

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load should accept 64-byte HMAC secret in production, got: %v", err)
	}
}

func TestValidateDevelopmentAllowsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
signing:
  hmac_secret: "change-me-in-production"
`), 0644)

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load should accept placeholder in development, got: %v", err)
	}
}

func TestLoadConfigOIDCDefaultsDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.OIDC.Enabled {
		t.Error("OIDC.Enabled should default to false")
	}
}

func TestLoadConfigOIDCEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	t.Setenv("E2A_OIDC_ENABLED", "true")
	t.Setenv("E2A_OIDC_ISSUER_URL", "https://issuer.example.com")
	t.Setenv("E2A_OIDC_CLIENT_ID", "e2a")
	t.Setenv("E2A_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("E2A_OIDC_REDIRECT_URL", "https://e2a.example.com/api/auth/oidc/callback")
	t.Setenv("E2A_OIDC_USER_ID_CLAIM", "e2a_user_id")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.OIDC.Enabled {
		t.Error("expected OIDC.Enabled = true from env override")
	}
	if cfg.OIDC.IssuerURL != "https://issuer.example.com" {
		t.Errorf("OIDC.IssuerURL = %q", cfg.OIDC.IssuerURL)
	}
	if cfg.OIDC.ClientID != "e2a" {
		t.Errorf("OIDC.ClientID = %q", cfg.OIDC.ClientID)
	}
	if cfg.OIDC.ClientSecret != "secret" {
		t.Errorf("OIDC.ClientSecret = %q", cfg.OIDC.ClientSecret)
	}
	if cfg.OIDC.RedirectURL != "https://e2a.example.com/api/auth/oidc/callback" {
		t.Errorf("OIDC.RedirectURL = %q", cfg.OIDC.RedirectURL)
	}
	if cfg.OIDC.UserIDClaim != "e2a_user_id" {
		t.Errorf("OIDC.UserIDClaim = %q", cfg.OIDC.UserIDClaim)
	}
}

func TestValidateOIDCEnabledRequiresAllFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
oidc:
  enabled: true
`), 0644)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load should refuse oidc.enabled with missing fields")
	}
	for _, want := range []string{"issuer_url", "client_id", "client_secret", "redirect_url", "user_id_claim"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %v", want, err)
		}
	}
}

func TestValidateOIDCEnabledAcceptsFullyConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
oidc:
  enabled: true
  issuer_url: "https://issuer.example.com"
  client_id: "e2a"
  client_secret: "secret"
  redirect_url: "https://e2a.example.com/api/auth/oidc/callback"
  user_id_claim: "e2a_user_id"
`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load should accept a fully configured oidc block, got: %v", err)
	}
	if !cfg.OIDC.Enabled {
		t.Error("expected OIDC.Enabled = true")
	}
}

func TestValidateOIDCEnabledRequiresAbsoluteHTTPURLs(t *testing.T) {
	tests := []struct {
		name        string
		issuerURL   string
		redirectURL string
		want        string
	}{
		{name: "relative issuer", issuerURL: "/issuer", redirectURL: "https://e2a.example.com/api/auth/oidc/callback", want: "issuer_url"},
		{name: "issuer query", issuerURL: "https://issuer.example.com?tenant=one", redirectURL: "https://e2a.example.com/api/auth/oidc/callback", want: "issuer_url"},
		{name: "relative redirect", issuerURL: "https://issuer.example.com", redirectURL: "/api/auth/oidc/callback", want: "redirect_url"},
		{name: "non-http redirect", issuerURL: "https://issuer.example.com", redirectURL: "javascript:alert(1)", want: "redirect_url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			body := fmt.Sprintf(`
env: "development"
oidc:
  enabled: true
  issuer_url: %q
  client_id: "e2a"
  client_secret: "secret"
  redirect_url: %q
  user_id_claim: "e2a_user_id"
`, test.issuerURL, test.redirectURL)
			if err := os.WriteFile(cfgPath, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(cfgPath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want invalid %s", err, test.want)
			}
		})
	}
}

func TestValidateOIDCDisabledIgnoresEmptyFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
oidc:
  enabled: false
`), 0644)

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load should accept oidc.enabled=false with empty fields, got: %v", err)
	}
}

func TestIsProduction(t *testing.T) {
	prod := &Config{Env: "production"}
	dev := &Config{Env: "development"}

	if !prod.IsProduction() {
		t.Error("expected IsProduction() to return true for production")
	}
	if dev.IsProduction() {
		t.Error("expected IsProduction() to return false for development")
	}
}

func TestSendingRampDefaultsOverridesAndValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cfg, err := Load(write("default-ramp.yaml", "env: development\n"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.SendingRamp.Enabled || cfg.SendingRamp.StartDaily != 50 || cfg.SendingRamp.TargetDaily != 2000 || cfg.SendingRamp.RampDays != 30 {
		t.Fatalf("sending ramp defaults = %+v, want disabled 50/2000/30", cfg.SendingRamp)
	}

	cfg, err = Load(write("ramp.yaml", `
env: development
sending_ramp:
  enabled: true
  start_daily: 75
  target_daily: 3000
  ramp_days: 21
`))
	if err != nil {
		t.Fatalf("Load override: %v", err)
	}
	if !cfg.SendingRamp.Enabled || cfg.SendingRamp.StartDaily != 75 || cfg.SendingRamp.TargetDaily != 3000 || cfg.SendingRamp.RampDays != 21 {
		t.Fatalf("sending ramp override = %+v", cfg.SendingRamp)
	}

	if _, err := Load(write("too-small.yaml", "env: development\nsending_ramp:\n  enabled: true\n  start_daily: 49\n")); err == nil {
		t.Fatal("Load should reject an enabled start_daily below the API's 50-recipient message maximum")
	}
}

// Trash retention: defaults to 30 days, yaml + env override, and a value
// below 1 day is refused at startup (the stable API promises soft-deleted
// resources stay restorable — see internal/identity.TrashRetention).
func TestTrashRetentionDefaultOverrideAndValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Absent → default 30.
	cfg, err := Load(write("default.yaml", "env: \"development\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trash.RetentionDays != 30 {
		t.Errorf("default Trash.RetentionDays = %d, want 30", cfg.Trash.RetentionDays)
	}

	// YAML override.
	cfg, err = Load(write("yaml.yaml", "env: \"development\"\ntrash:\n  retention_days: 7\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trash.RetentionDays != 7 {
		t.Errorf("yaml Trash.RetentionDays = %d, want 7", cfg.Trash.RetentionDays)
	}

	// Env override wins over yaml.
	t.Setenv("E2A_TRASH_RETENTION_DAYS", "90")
	cfg, err = Load(write("env.yaml", "env: \"development\"\ntrash:\n  retention_days: 7\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trash.RetentionDays != 90 {
		t.Errorf("env Trash.RetentionDays = %d, want 90", cfg.Trash.RetentionDays)
	}
	t.Setenv("E2A_TRASH_RETENTION_DAYS", "")

	// Below 1 day → refused.
	if _, err := Load(write("zero.yaml", "env: \"development\"\ntrash:\n  retention_days: 0\n")); err == nil {
		t.Error("Load should reject trash.retention_days: 0")
	}
	if _, err := Load(write("neg.yaml", "env: \"development\"\ntrash:\n  retention_days: -3\n")); err == nil {
		t.Error("Load should reject a negative trash.retention_days")
	}
}

func TestSMTPProxyTrustedCIDRsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	t.Setenv("E2A_SMTP_PROXY_TRUSTED_CIDRS", "172.30.0.0/24, 10.0.0.0/8")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.SMTP.ProxyTrustedCIDRs; len(got) != 2 || got[0] != "172.30.0.0/24" || got[1] != "10.0.0.0/8" {
		t.Fatalf("ProxyTrustedCIDRs = %v", got)
	}
}

func TestSMTPProxyTrustedCIDRsValidateRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
smtp:
  proxy_trusted_cidrs: ["172.30.0.0/24", "not-a-cidr"]
`), 0644)

	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "not-a-cidr") {
		t.Fatalf("Load error = %v, want error naming the malformed CIDR", err)
	}
}

func TestSMTPProxyTrustedCIDRsValidateRejectsCatchAll(t *testing.T) {
	dir := t.TempDir()
	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		cfgPath := filepath.Join(dir, "config.yaml")
		os.WriteFile(cfgPath, []byte(`
env: "development"
smtp:
  proxy_trusted_cidrs: ["`+cidr+`"]
`), 0644)
		_, err := Load(cfgPath)
		if err == nil || !strings.Contains(err.Error(), cidr) {
			t.Fatalf("Load error = %v, want rejection naming %q (trusting every peer enables source-IP spoofing)", err, cidr)
		}
	}
}

func TestLoadConfigRateLimitsDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
database:
  url: "postgres://test:test@localhost/test"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.RateLimits.PollPerMinute; got != 240 {
		t.Errorf("default RateLimits.PollPerMinute = %d, want 240", got)
	}
}

func TestLoadConfigRateLimitsOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
database:
  url: "postgres://test:test@localhost/test"
rate_limits:
  poll_per_minute: 600
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.RateLimits.PollPerMinute; got != 600 {
		t.Errorf("RateLimits.PollPerMinute = %d, want 600", got)
	}
}

func TestLoadConfigMetricsDefaultsDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Error("Metrics.Enabled should default to false")
	}
	if cfg.Metrics.ListenAddr != "127.0.0.1:9091" {
		t.Errorf("Metrics.ListenAddr default = %q, want 127.0.0.1:9091", cfg.Metrics.ListenAddr)
	}
}

func TestLoadConfigMetricsYAMLAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("env: \"development\"\nmetrics:\n  enabled: true\n  listen_addr: \"127.0.0.1:9999\"\n"), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.Metrics.Enabled || cfg.Metrics.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("yaml metrics block not honored: %+v", cfg.Metrics)
	}

	// Env wins over yaml (repo convention).
	t.Setenv("E2A_METRICS_ENABLED", "false")
	t.Setenv("E2A_METRICS_LISTEN_ADDR", "127.0.0.1:9092")
	cfg, err = Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Error("E2A_METRICS_ENABLED=false should override yaml true")
	}
	if cfg.Metrics.ListenAddr != "127.0.0.1:9092" {
		t.Errorf("Metrics.ListenAddr = %q, want env override", cfg.Metrics.ListenAddr)
	}
}

func TestLoadConfigProvisioningDefaultsDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Provisioning.Enabled {
		t.Error("Provisioning.Enabled should default to false")
	}
	if cfg.Provisioning.Secret != "" {
		t.Errorf("Provisioning.Secret should default to empty, got %q", cfg.Provisioning.Secret)
	}
}

func TestLoadConfigProvisioningEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`env: "development"`), 0644)

	t.Setenv("E2A_PROVISIONING_ENABLED", "true")
	t.Setenv("E2A_PROVISIONING_SECRET", "test-provisioning-secret")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.Provisioning.Enabled {
		t.Error("expected Provisioning.Enabled = true from env override")
	}
	if cfg.Provisioning.Secret != "test-provisioning-secret" {
		t.Errorf("Provisioning.Secret = %q", cfg.Provisioning.Secret)
	}
}

func TestValidateProvisioningEnabledRequiresSecret(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
provisioning:
  enabled: true
`), 0644)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load should refuse provisioning.enabled without a secret")
	}
	if !strings.Contains(err.Error(), "provisioning") {
		t.Errorf("expected error to mention provisioning, got: %v", err)
	}
}

func TestValidateProvisioningEnabledAcceptsSecret(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "development"
provisioning:
  enabled: true
`), 0644)

	t.Setenv("E2A_PROVISIONING_SECRET", "test-provisioning-secret")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load should accept provisioning.enabled with a secret, got: %v", err)
	}
	if !cfg.Provisioning.Enabled {
		t.Error("expected Provisioning.Enabled = true")
	}
}

func TestProvisioningSecretIsEnvOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Secrets never go in the yaml file — the field is yaml:"-", so a
	// secret written here must be ignored (and enabled-without-env-secret
	// must therefore fail validation).
	os.WriteFile(cfgPath, []byte(`
env: "development"
provisioning:
  enabled: true
  secret: "must-be-ignored"
`), 0644)

	if _, err := Load(cfgPath); err == nil {
		t.Fatal("Load should ignore a yaml provisioning secret and refuse enabled-without-env-secret")
	}
}

func TestValidateProvisioningProductionRequiresStrongSecret(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
env: "production"
provisioning:
  enabled: true
`), 0644)

	t.Setenv("E2A_HMAC_SECRET", strings.Repeat("a", 32))
	t.Setenv("E2A_PROVISIONING_SECRET", "short")

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load should refuse a short provisioning secret in production")
	}
	if !strings.Contains(err.Error(), "provisioning secret") {
		t.Errorf("expected error to mention provisioning secret, got: %v", err)
	}
}
