package config

import (
	"errors"
	"fmt"
	"log"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultSenderIdentityFixtureTTL is how long a fixture sending identity is
// assumed to still be in use. A conformance or prober fixture that outlives a
// day has been abandoned by the run that made it.
const defaultSenderIdentityFixtureTTL = 24 * time.Hour

// defaultSenderIdentityReclaimMinAge is the age floor for reclaiming an
// orphaned fixture identity. A week is far longer than any test run and far
// longer than any plausible clock skew, so an identity that clears it is
// abandoned rather than merely idle.
const defaultSenderIdentityReclaimMinAge = 168 * time.Hour

// defaultSenderIdentityReclaimMaxPerSweep bounds deletions per reaper job. Set
// so a systematic mistake costs a handful of identities and a loud log rather
// than an account's worth, while still draining a realistic leak backlog
// (single digits, accumulated over weeks) within a couple of hourly sweeps.
const defaultSenderIdentityReclaimMaxPerSweep = 5

// splitAndTrim splits a comma-separated env value into a clean slice (trimmed,
// no empties). Used for list-valued overrides like SNS topic ARNs.
func splitAndTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// placeholderHMACSecret is the example value shipped in config.example.yaml.
// It is deliberately >=32 bytes so the documented `cp config.example.yaml
// config.yaml && make run` path boots in development without edits — see
// deriveOAuthSigningKey in internal/oauth/provider.go, which requires
// >=32 regardless of env. It must still be overridden in any production
// deployment: the server refuses to start with this exact value when
// env: production. Keep this in sync with config.example.yaml's
// signing.hmac_secret.
const placeholderHMACSecret = "change-me-in-production-this-is-not-a-real-secret"

// minHMACSecretBytes is the minimum HMAC secret length enforced in
// production. RFC 2104 §3 recommends keys be at least the output length
// of the hash function (32 bytes for SHA-256) — anything shorter weakens
// the MAC's security margin to brute-force range.
const minHMACSecretBytes = 32

type Config struct {
	SMTP             SMTPConfig             `yaml:"smtp"`
	HTTP             HTTPConfig             `yaml:"http"`
	Database         DatabaseConfig         `yaml:"database"`
	OAuth            OAuthConfig            `yaml:"oauth"`
	OIDC             OIDCConfig             `yaml:"oidc"`
	Delegated        DelegatedConfig        `yaml:"delegated"`
	Provisioning     ProvisioningConfig     `yaml:"provisioning"`
	Signing          SigningConfig          `yaml:"signing"`
	OutboundSMTP     OutboundSMTPConfig     `yaml:"outbound_smtp"`
	Inbound          InboundConfig          `yaml:"inbound"`
	WebhookFanout    WebhookFanoutConfig    `yaml:"webhook_fanout"`
	Webhook          WebhookConfig          `yaml:"webhook"`
	SenderIdentity   SenderIdentityConfig   `yaml:"sender_identity"`
	DeliveryFeedback DeliveryFeedbackConfig `yaml:"delivery_feedback"`
	SendingRamp      SendingRampConfig      `yaml:"sending_ramp"`
	Limits           LimitsConfig           `yaml:"limits"`
	RateLimits       RateLimitsConfig       `yaml:"rate_limits"`
	Trash            TrashConfig            `yaml:"trash"`
	Metrics          MetricsConfig          `yaml:"metrics"`
	OutboundFooter   OutboundFooterConfig   `yaml:"outbound_footer"`
	Notifications    NotificationsConfig    `yaml:"notifications"`
	Env              string                 `yaml:"env"` // "development" or "production"
	// DeploymentName names WHICH deployment of e2a this process is, for
	// classification metadata e2a attaches to the resources it creates in
	// external systems (today: the e2a-env tag on provisioned SES sender
	// identities — internal/senderidentity/tags.go). It is deliberately NOT
	// Env: Env is a development/production MODE switch that gates security
	// behavior, and both the hosted prod and hosted staging deployments run
	// env: "production", so Env provably cannot tell them apart.
	//
	// Recognized values are "prod" and "staging" — the closed vocabulary
	// internal/senderidentity stamps, mirrored here only to normalize the
	// operator's input (that package owns the tag values, and re-screens
	// whatever it is given). Empty — the default, and what every self-host
	// leaves it at — means unset: the metadata is simply omitted. An
	// unrecognized value is logged once at load and treated as unset rather
	// than rejected: nothing about serving mail depends on this, so a typo
	// must never stop a server from booting.
	// Override with E2A_DEPLOYMENT_NAME.
	DeploymentName string `yaml:"deployment_name"`
	// SharedDomain enables slug-based agent registration. When set
	// (e.g. "agents.example.com"), users can register agents with just a
	// slug and get `<slug>@<shared_domain>` provisioned without DNS
	// setup. Empty disables slug registration — every agent must use a
	// custom domain that the user owns and verifies. The shared domain
	// itself is reserved: it cannot be claimed as a custom domain.
	SharedDomain string `yaml:"shared_domain"`
}

func (c *Config) IsProduction() bool {
	return c.Env == "production"
}

type SMTPConfig struct {
	ListenAddr string `yaml:"listen_addr"`
	Domain     string `yaml:"domain"`
	TLSCert    string `yaml:"tls_cert"`
	TLSKey     string `yaml:"tls_key"`
	// ProxyTrustedCIDRs gates PROXY protocol (v1/v2) parsing on the inbound
	// listener: only connections whose source matches a listed CIDR may
	// present a PROXY header. Empty (default) = PROXY parsing off.
	ProxyTrustedCIDRs []string `yaml:"proxy_trusted_cidrs"`
}

type HTTPConfig struct {
	ListenAddr string `yaml:"listen_addr"`
	// PublicURL is the externally visible base URL of the *web app* — the
	// domain that serves the dashboard, HITL magic-link pages, and the
	// OAuth login/consent UI. Absolute links in notification emails and the
	// OAuth authorization_endpoint are built from it. Example:
	// "https://e2a.example.com". If empty, features that need absolute URLs
	// gracefully degrade.
	PublicURL string `yaml:"public_url"`
	// APIURL is the externally visible base URL of the *programmatic API* —
	// the host the SDKs/MCP target (e.g. "https://api.e2a.dev"). It is the
	// OAuth issuer identity and the base for the token/registration/
	// revocation/jwks endpoints, so it should match the host the MCP
	// resource is served from (RFC 9728: clients expect the issuer to live
	// with the API). Defaults to PublicURL when unset, so single-host
	// deployments and self-hosters need not set it.
	APIURL string `yaml:"api_url"`
}

// MetricsConfig controls the Prometheus exposition listener. Disabled by
// default (zero behavior change for existing deployments); when enabled the
// server binds a SEPARATE listener for GET /metrics so the exposition
// surface is never reachable through the public API host. The default bind
// is loopback-only — operators scraping from another host front it with
// their own network policy, deliberately.
type MetricsConfig struct {
	// Enabled turns the Prometheus backend + /metrics listener on.
	// Override with E2A_METRICS_ENABLED.
	Enabled bool `yaml:"enabled"`
	// ListenAddr is the bind address for the metrics listener.
	// Override with E2A_METRICS_LISTEN_ADDR.
	ListenAddr string `yaml:"listen_addr"`
	// Build is the bounded release/image identifier attached to every sample.
	// Override with E2A_METRICS_BUILD. Empty is exposed as "unknown".
	Build string `yaml:"build"`
}

// OutboundFooterConfig controls an optional operator-configured footer
// appended to all SMTP-egress outbound mail at composition time (before the
// composed-size check, the MIME build, and DKIM signing, and above the managed
// unsubscribe line). Fully inert when Enabled is false (the default) — zero
// behavior change for existing deployments. Never applied to self-send
// loopback delivery or to non-standard account classes (internal/system/demo).
//
// Per-account gating rides account_limits.outbound_footer_enabled: a present
// row decides for itself; DefaultEnabled covers accounts with NO row. Text and
// HTML are operator-trusted content (config, not user input) — the HTML
// fragment is appended verbatim to the HTML part, so the operator owns its
// escaping. Both empty with Enabled true = a no-op, not an error.
type OutboundFooterConfig struct {
	// Enabled is the master switch for the feature.
	// Override with E2A_OUTBOUND_FOOTER_ENABLED.
	Enabled bool `yaml:"enabled"`
	// DefaultEnabled applies to accounts with NO account_limits row.
	DefaultEnabled bool `yaml:"default_enabled"`
	// Text is the plain-text footer, appended after a blank line + the
	// RFC 3676 signature separator ("-- " on its own line, trailing space
	// included, so mail clients trim it when quoting a reply). Empty = no
	// text-part append.
	Text string `yaml:"text"`
	// HTML is the HTML fragment appended to the HTML part when the message
	// has one. Empty = no HTML-part append.
	HTML string `yaml:"html"`
}

type DatabaseConfig struct {
	URL string `yaml:"url"`
}

type OAuthConfig struct {
	GoogleClientID     string `yaml:"google_client_id"`
	GoogleClientSecret string `yaml:"google_client_secret"`
	RedirectURL        string `yaml:"redirect_url"`
	// SigningKey is the PEM-encoded RSA private key (PKCS#1 or PKCS#8) used to
	// sign auth.md agent-identity JWTs + access tokens (Slice 5b). The public
	// half is published at /.well-known/jwks.json. Empty ⇒ the agent-auth
	// surface is disabled (JWKS serves an empty set). Supplied via
	// E2A_OAUTH_SIGNING_KEY; never generated or persisted by e2a.
	SigningKey string `yaml:"signing_key"`
	// SigningKID is the key id advertised in the JWKS and stamped on every
	// issued JWT (E2A_OAUTH_SIGNING_KID; default "v1"). Rotation advertises a
	// new kid, then retires the old after the longest token TTL.
	SigningKID string `yaml:"signing_kid"`
}

// OIDCConfig enables a generic OpenID Connect Authorization Code login for
// existing e2a users. It is off by default; when disabled no OIDC routes are
// registered. The provider must include UserIDClaim in its ID token and the
// claim must name an existing users.id. OIDC login never provisions users.
type OIDCConfig struct {
	// Enabled turns the feature on. Override with E2A_OIDC_ENABLED.
	Enabled bool `yaml:"enabled"`
	// IssuerURL is the exact expected ID-token issuer and discovery base URL.
	IssuerURL string `yaml:"issuer_url"`
	// ClientID and ClientSecret identify this confidential e2a web client.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// RedirectURL is the registered absolute callback URL.
	RedirectURL string `yaml:"redirect_url"`
	// UserIDClaim names the ID-token claim containing an existing users.id.
	UserIDClaim string `yaml:"user_id_claim"`
}

type SigningConfig struct {
	HMACSecret string `yaml:"hmac_secret"`
}

// DelegatedClaimConfig names one required context claim on delegated
// tokens and, optionally, the closed set of values it may carry.
type DelegatedClaimConfig struct {
	Name          string   `yaml:"name"`
	AllowedValues []string `yaml:"allowed_values"`
}

// delegatedReservedClaims are the registered/validation claim names the
// delegated verifier already owns; configuring one as a required or
// forbidden claim is a policy contradiction and fails Validate.
var delegatedReservedClaims = map[string]struct{}{
	"iss": {}, "sub": {}, "aud": {}, "exp": {}, "iat": {}, "nbf": {},
	"jti": {}, "azp": {}, "scope": {}, "typ": {}, "alg": {},
}

// DelegatedConfig enables verification of externally issued OAuth 2.0
// access tokens (RFC 9068 `at+jwt`) minted by one configured OIDC
// issuer, mapped to local users via external_principal_mappings. Off by
// default; when disabled, delegated-owned tokens are still never handed
// to any other credential path — they just always fail authentication.
// Every value here is deployment data: nothing operator-specific lives
// in source or defaults.
type DelegatedConfig struct {
	// Enabled turns the verifier on. Override with E2A_DELEGATED_ENABLED.
	Enabled bool `yaml:"enabled"`
	// IssuerURL is the discovery base and the exact byte-for-byte `iss`
	// comparison value — no alias or trailing-slash normalization. Must
	// be https when env=production.
	IssuerURL string `yaml:"issuer_url"`
	// Audience is the exact single-string `aud` (array claims are
	// rejected even when they contain this value).
	Audience string `yaml:"audience"`
	// AuthorizedParty is the exact required `azp` claim value.
	AuthorizedParty string `yaml:"authorized_party"`
	// RequiredScope is the exact singleton scope string the token must
	// carry — the whole claim, not a member of a set.
	RequiredScope string `yaml:"required_scope"`
	// AllowedAlgorithms is the closed signature-algorithm allowlist. Only
	// RS256 and ES256 are supported.
	AllowedAlgorithms []string `yaml:"allowed_algorithms"`
	// MaxTokenLifetimeSeconds bounds exp - iat.
	MaxTokenLifetimeSeconds int `yaml:"max_token_lifetime_seconds"`
	// ClockSkewSeconds admits an iat up to this far in the future and an
	// exp up to this far in the past; it never relaxes the lifetime rule.
	ClockSkewSeconds int `yaml:"clock_skew_seconds"`
	// RequiredClaims must each be present as bounded nonempty strings.
	RequiredClaims []DelegatedClaimConfig `yaml:"required_claims"`
	// ForbiddenClaims must be absent entirely (null still rejects).
	ForbiddenClaims []string `yaml:"forbidden_claims"`
}

// ProvisioningConfig gates the internal user-provisioning endpoint
// (POST /api/internal/users/provision). It is off by default; when disabled
// the endpoint 503s. An external control plane calls it to create an e2a
// user idempotently — keyed by the caller's external_ref — ahead of that
// user's first sign-in. The endpoint is deliberately generic: every
// deployment-specific value (who the control plane is, which secret it
// signs with) lives here, not in source.
type ProvisioningConfig struct {
	// Enabled turns the endpoint on. Override with E2A_PROVISIONING_ENABLED.
	Enabled bool `yaml:"enabled"`
	// Secret is the shared HMAC key the control plane signs each request
	// body with (hex digest in X-E2A-Internal-Signature). Env-only via
	// E2A_PROVISIONING_SECRET — secrets never go in the yaml file. Must be
	// set to the same value on both ends; empty with enabled=true fails
	// Validate.
	Secret string `yaml:"-"`
}

type OutboundSMTPConfig struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	FromDomain string `yaml:"from_domain"`
	// RequireTLS fails the send if STARTTLS can't be negotiated, instead
	// of silently relaying in cleartext (a network attacker can strip the
	// STARTTLS capability from the server's EHLO to force this). Pointer
	// so an unset value can default to true in production while staying
	// off for dev relays (e.g. Mailpit on :1025 with no TLS). Regardless
	// of this flag, PLAIN auth is never sent over a cleartext connection.
	RequireTLS *bool `yaml:"require_tls"`
	// MessageIDDomain is the domain the upstream provider stamps on the
	// Message-ID header of relayed mail (SES: "<region>.amazonses.com").
	// SES's SMTP 250 response returns the assigned id BARE — without this
	// domain — so the relay appends it to make the captured id match the
	// on-wire Message-ID; a mismatch there breaks In-Reply-To/References
	// threading on replies to the agent's own outbound. Empty (the default)
	// derives it from a standard SES Host (email-smtp.<region>.amazonaws.com);
	// set it explicitly for non-standard endpoints (e.g. VPC endpoints).
	MessageIDDomain string `yaml:"message_id_domain"`
}

// NotificationsConfig carries settings for the platform's operational
// notification emails: HITL approval requests (internal/hitlnotify) and the
// webhook health warning/disabled alerts (internal/webhooknotify). Both
// senders read these settings.
type NotificationsConfig struct {
	// FromAddress is the sender address for operational notification
	// emails. Optional. When empty, each sender falls back to its OWN
	// fixed local part on outbound_smtp.from_domain (approvals@ and
	// webhooks@ respectively, kept distinct so a time-boxed approval stays
	// filterable apart from routine webhook mail), so self-hosted
	// deployments work with zero configuration. Setting this collapses
	// both senders into a single identity.
	// The value MUST be an address the deployment's sending identity is
	// allowed to send as. Deliberately configuration, never a constant: a
	// hardcoded operator address would make every self-host try to send
	// as an identity it does not own. Override with
	// E2A_NOTIFICATIONS_FROM_ADDRESS.
	FromAddress string `yaml:"from_address"`
	// ReplyTo, when set, adds a Reply-To header to those emails. Use it
	// when the sending domain (from_address / from_domain) is relay-only
	// with no mailbox behind it, so replies land in a real support inbox
	// on another domain — the same From-on-the-relay-domain +
	// Reply-To-at-the-real-inbox pattern the product's shared-domain
	// sends already use. Optional: empty means replies follow the sender
	// address — webhook mail emits no Reply-To, while approval mail emits
	// one pointing at its own sender (its long-standing behaviour, kept so
	// unconfigured deployments are byte-identical to before). The sane
	// default when from_address is itself a real mailbox. Override with
	// E2A_NOTIFICATIONS_REPLY_TO.
	ReplyTo string `yaml:"reply_to"`
}

// InboundConfig selects the inbound processing model (inbound-message-pipeline-
// river.md). Mode="sync" (the default) is the historical path: the SMTP session runs
// parse/screen/persist/deliver inline before 250. Mode="async" opts into the
// queue-first River pipeline: the session durably accepts the raw MIME to
// inbound_intake + enqueues a processing job atomically before 250, and the
// internal/inboundprocess worker does the processing off the SMTP critical path.
// Override with E2A_INBOUND_MODE. Any value other than "async" is treated as "sync"
// (fail-safe to the unchanged path).
type InboundConfig struct {
	Mode string `yaml:"mode"`
}

// WebhookFanoutConfig selects the webhook fan-out execution model (webhook-fanout-
// river-migration.md). Mode="legacy" (the default) drains webhook_events →
// webhook_subscriber_deliveries via the in-process webhookpub.OutboxWorker
// (LISTEN/NOTIFY + poll + SKIP-LOCKED lease). Mode="river" opts into the River
// pipeline: PublishTx/PublishBestEffortTx enqueue a webhook_fanout job in the event's
// tx and the webhookpub.FanOutWorker does the match/insert/enqueue off the drain loop.
// Override with E2A_WEBHOOK_FANOUT_MODE. Any value other than "river" is treated as
// "legacy" (fail-safe to the unchanged path). Wired in Slice 2; unused under legacy.
type WebhookFanoutConfig struct {
	Mode string `yaml:"mode"`
}

// WebhookConfig carries webhook-delivery settings other than the fan-out engine
// choice. InternalSinkURL, when set, names a single trusted internal sink URL
// (the e2a-prober's /sink) that is EXEMPT from the production HTTPS-required +
// SSRF private-IP delivery guards. It must exactly match the probe webhook's
// registered URL. Safe: it is a server-operator config value (never attacker
// input), matched by exact string equality, and the probe webhook is created by
// the privileged prober `seed`, not the public registration API (which rejects
// http:// + private hosts). Override with E2A_WEBHOOK_INTERNAL_SINK_URL. Empty
// (the default) disables the exemption.
type WebhookConfig struct {
	InternalSinkURL string `yaml:"internal_sink_url"`
}

// DeliveryFeedbackConfig controls outbound delivery feedback (decision 9 /
// Slice 4b). When SESConfigurationSet is set, outbound mail is tagged so SES
// publishes delivery/bounce/complaint events; SNSTopicARNs is the fail-closed
// allow-list of SNS topics the public notifications endpoint accepts (empty =
// reject all). Both empty (the default) disables the feature: no event header,
// the endpoint rejects everything. Override with
// E2A_DELIVERY_SES_CONFIGURATION_SET and E2A_DELIVERY_SNS_TOPIC_ARNS (comma-separated).
type DeliveryFeedbackConfig struct {
	SESConfigurationSet string   `yaml:"ses_configuration_set"`
	SNSTopicARNs        []string `yaml:"sns_topic_arns"`
}

// SenderIdentityConfig controls custom-domain sender identity (decision 4 /
// Slice 4). When SESRegion is set (e.g. "us-east-1"), domain verification
// registers an SES BYODKIM sending identity and, once verified, outbound mail
// uses the agent's own address as From. Empty (the default) disables it:
// sending_status stays "none" and outbound uses the relay From — the
// fail-closed default for dev/self-host without SES. Override SESRegion with
// E2A_SENDER_IDENTITY_SES_REGION.
type SenderIdentityConfig struct {
	SESRegion string `yaml:"ses_region"`
	// LegacyJobCompat is phase 1 of the two-phase blue/green rollout for the
	// versioned sender-identity job lanes: while true, this binary PRODUCES
	// the legacy job kinds (consumable by the previous release) and CONSUMES
	// both lanes, so rolling this deploy back strands no teardown work. This
	// does not make the previous binary's provider mutations ownership-safe;
	// hosted operators must follow the documented mutation freeze and IAM
	// sequence. Flip to false once this release is the stable rollback target.
	// Default false — single-instance deployments have no blue/green overlap.
	// Override via E2A_SENDER_IDENTITY_LEGACY_JOB_COMPAT.
	LegacyJobCompat bool `yaml:"legacy_job_compat"`
	// FixtureTTL is how long a FIXTURE sending identity — one provisioned for
	// a non-customer account class (internal dogfooding, synthetic monitoring)
	// — is expected to stay useful. It is stamped onto the identity as the
	// e2a-expires tag so a later cleanup pass can tell an abandoned test
	// fixture from a live customer domain without joining against this
	// server's database. Customer identities never carry it.
	//
	// Written as a Go duration string ("24h", "90m"); yaml.v3 decodes a
	// duration field from a string only, so a bare `0` is a type error —
	// spell the disabled case "0s". Defaults to 24h (a conformance/prober
	// fixture that outlives a day has been abandoned). Zero disables the tag
	// without disabling the rest of the classification metadata. Override via
	// E2A_SENDER_IDENTITY_FIXTURE_TTL; a malformed value there is logged and
	// ignored rather than fatal, for the same reason as DeploymentName.
	FixtureTTL time.Duration `yaml:"fixture_ttl"`

	// ReapOrphans ARMS the reaper's orphan-identity reclaim. An orphan is a
	// provider identity with no ledger row and no domain row; today the reaper
	// only alerts on them, and crashed test runs leak them until a human
	// sweeps by hand. Default false: with the flag off the reaper runs the
	// entire deletion decision and logs what it WOULD delete (or why it
	// refused) without touching the provider — the observe-only mode an
	// operator is expected to run for days before arming.
	//
	// Arming is NOT sufficient on its own: ReclaimZones must also be set, and
	// every guard in senderidentity.orphanReclaimable must pass. Deletion goes
	// through the same ownership-verifying Deprovision path the managed-ledger
	// phase uses.
	ReapOrphans bool `yaml:"reap_orphans"`
	// ReclaimZones bounds reclaim to identity names at or under a DNS zone
	// that holds only e2a's OWN test fixtures (e.g. the conformance/prober
	// zone). This is the strongest of the reclaim guards — a customer domain
	// is never under the test zone — and matching is on a label boundary, so
	// zone "example.test" covers "a.example.test" and "example.test" itself
	// but never "evilexample.test".
	//
	// EMPTY (the default) means reclaim NOTHING. An empty list is never read
	// as "any zone".
	ReclaimZones []string `yaml:"reclaim_zones"`
	// ReclaimMinAge is the absolute age floor an identity must clear (by its
	// e2a-created tag) before it can be reclaimed, checked INDEPENDENTLY of
	// its expiry tag so that clock skew or a mis-set FixtureTTL cannot make a
	// brand-new identity instantly reclaimable. Written as a Go duration
	// string; yaml.v3 decodes a duration only from a string, so spell zero
	// "0s". Defaults to 168h (7 days). Zero or negative reclaims nothing — an
	// unset floor is treated as an unconfigured policy, not as a waiver.
	ReclaimMinAge time.Duration `yaml:"reclaim_min_age"`
	// ReclaimMaxPerSweep caps how many identities one reaper JOB may delete.
	// The orphan phase is paginated across River jobs, so this is a per-page
	// budget rather than a global one (see reapProviderOrphanPage): it exists
	// so a systematic mistake costs a handful of identities and a loud log
	// rather than the account. Defaults to 5; 0 reclaims nothing.
	ReclaimMaxPerSweep int `yaml:"reclaim_max_per_sweep"`
}

// SendingRampConfig is an operator-owned safety policy for newly verified
// custom sender domains. It is intentionally not user-configurable through the
// public API. Values are snapshotted when a domain first sends, so later config
// changes do not reshape an in-flight ramp.
type SendingRampConfig struct {
	Enabled     bool `yaml:"enabled"`
	StartDaily  int  `yaml:"start_daily"`
	TargetDaily int  `yaml:"target_daily"`
	RampDays    int  `yaml:"ramp_days"`
}

// LimitsConfig is the operator-configured fallback applied to any user
// who does not yet have a row in account_limits. The hosted billing
// sidecar populates rows for paying customers; self-hosted operators
// who do not run a billing service rely on these defaults for every
// user. Defaults below intentionally lean generous so a self-host that
// never touches the limits subsystem is not accidentally throttled.
//
// Hosted-service operators who want every brand-new signup capped to a
// "free" shape should set these to the Free-tier numbers — the sidecar
// will then overwrite them on upgrade.
type LimitsConfig struct {
	PlanCode         string `yaml:"plan_code"`
	MaxAgents        int    `yaml:"max_agents"`
	MaxDomains       int    `yaml:"max_domains"`
	MaxMessagesMonth int    `yaml:"max_messages_month"`
	// MaxMessagesDay is the optional per-UTC-day outbound send cap applied
	// to users without an account_limits row. Unset/absent (the default)
	// means no daily policy — self-host behavior is untouched. 0 hard-blocks
	// all sends, consistent with the other caps. Hosted deployments set it
	// to the Free tier's daily allowance; the billing sidecar overrides it
	// per account via the max_messages_day column.
	MaxMessagesDay  *int  `yaml:"max_messages_day"`
	MaxStorageBytes int64 `yaml:"max_storage_bytes"`
	// CacheTTLSeconds controls how long resolved Limits are cached
	// in-process. The cache covers the account_limits read only; current
	// usage counts are always live. Set to 0 to disable caching
	// (recommended for tests that mutate account_limits and want
	// immediate visibility).
	CacheTTLSeconds int `yaml:"cache_ttl_seconds"`
	// InternalAPISecret is the shared HMAC secret the external limits
	// provisioner (e.g. the hosted billing sidecar) uses to authenticate
	// to /api/internal/limits/invalidate. When empty (the self-host
	// default), that endpoint returns 503 — no provisioner, no
	// invalidation. Must be set to the same value on both ends.
	InternalAPISecret string `yaml:"internal_api_secret"`
	// BillingHookURL is the URL the OSS server POSTs to when a user
	// deletes their account, so the external billing service (e.g.
	// the hosted billing sidecar's /api/internal/billing/cancel) can
	// cancel the user's Stripe subscription. Empty disables the call
	// — appropriate for self-host without billing. The same
	// InternalAPISecret signs the POST body.
	BillingHookURL string `yaml:"billing_hook_url"`
}

// RateLimitsConfig tunes server-side request rate limits. A zero value
// means "use the built-in default", so configs that omit the section
// keep current behavior.
type RateLimitsConfig struct {
	// PollPerMinute is the shared per-user budget for the authenticated
	// read (polling) endpoints: list/get messages, conversations, and
	// webhooks. The bucket is keyed per USER and shared by every reader
	// the account runs — each agent's polling loop plus the dashboard,
	// whose thread view fetches message bodies individually. Size it for
	// the whole account, not a single client. Default 240.
	PollPerMinute int `yaml:"poll_per_minute"`
}

// TrashConfig tunes the soft-delete (trash) subsystem.
type TrashConfig struct {
	// RetentionDays is how many days a soft-deleted resource (agent inbox
	// or message) stays restorable in the trash before the janitor purges
	// it permanently. This is the knob behind the API contract's "30 days
	// by default (deployment-configurable)" wording — the default is 30.
	// Minimum 1 (a sub-day trash would break the restore promise the
	// stable API documents); the server refuses to start on a lower value.
	// Override with E2A_TRASH_RETENTION_DAYS.
	RetentionDays int `yaml:"retention_days"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		SMTP: SMTPConfig{
			ListenAddr: ":2525",
			Domain:     "e2a.example.com",
		},
		HTTP: HTTPConfig{
			ListenAddr: ":8080",
		},
		// Generous defaults so self-host operators who do not configure
		// `limits:` are not accidentally throttled. Hosted operators
		// override these in config.prod.yaml.
		Limits: LimitsConfig{
			PlanCode:         "default",
			MaxAgents:        1_000_000,
			MaxDomains:       1_000_000,
			MaxMessagesMonth: 1_000_000_000,
			MaxStorageBytes:  1 << 50, // 1 PiB
			CacheTTLSeconds:  60,
		},
		Inbound:       InboundConfig{Mode: "sync"},
		WebhookFanout: WebhookFanoutConfig{Mode: "legacy"},
		SendingRamp: SendingRampConfig{
			StartDaily:  50,
			TargetDaily: 2000,
			RampDays:    30,
		},
		RateLimits: RateLimitsConfig{PollPerMinute: 240},
		Metrics:    MetricsConfig{ListenAddr: "127.0.0.1:9091"},
		Trash:      TrashConfig{RetentionDays: 30},
		// An absent sender_identity block keeps the fixture expiry default;
		// an explicit `fixture_ttl: 0` survives unmarshal and disables it.
		// The reclaim defaults are the SAFE ones: disarmed, no zones (which
		// alone reclaims nothing), a 7-day age floor, and a 5-per-job cap.
		SenderIdentity: SenderIdentityConfig{
			FixtureTTL:         defaultSenderIdentityFixtureTTL,
			ReclaimMinAge:      defaultSenderIdentityReclaimMinAge,
			ReclaimMaxPerSweep: defaultSenderIdentityReclaimMaxPerSweep,
		},
		// Delegated verification is off by default. Only protocol-level
		// values are defaulted (algorithm allowlist, lifetime, skew); the
		// required/forbidden CLAIM lists are deployment identity policy
		// (§10.3 "deployment data") and are deliberately NOT defaulted, so
		// OSS ships no operator-specific claim names or role values. An
		// enabling deployment must state its own claim policy — the enabled
		// validation rejects empty required/forbidden lists.
		Delegated: DelegatedConfig{
			AllowedAlgorithms:       []string{"RS256", "ES256"},
			MaxTokenLifetimeSeconds: 120,
			ClockSkewSeconds:        5,
		},
		Env: "development",
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// E2A_ENV overrides `env:` in config.yaml. Every other config knob
	// already has an env-var override; env: previously had none — the only
	// way to flip it was editing YAML, but the published Docker image bakes
	// config.example.yaml (env: "development") to a fixed path, so every
	// documented deployment ran in development mode permanently regardless
	// of how the operator configured the container. That matters because
	// IsProduction() (below) gates both the production HMAC-secret/length
	// guards in Validate() and the dev-mode DNS-verification short-circuit
	// in internal/agent/api.go's checkDomainRecords.
	if v := os.Getenv("E2A_ENV"); v != "" {
		cfg.Env = v
	}
	if cfg.Env != "development" && cfg.Env != "production" {
		return nil, fmt.Errorf("config: env (or E2A_ENV) must be %q or %q, got %q", "development", "production", cfg.Env)
	}

	// Env overrides — secrets only (never duplicated in yaml)
	if v := os.Getenv("E2A_DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("E2A_INTERNAL_API_SECRET"); v != "" {
		cfg.Limits.InternalAPISecret = v
	}
	if v := os.Getenv("E2A_HMAC_SECRET"); v != "" {
		cfg.Signing.HMACSecret = v
	}
	if v := os.Getenv("E2A_GOOGLE_CLIENT_ID"); v != "" {
		cfg.OAuth.GoogleClientID = v
	}
	if v := os.Getenv("E2A_GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.OAuth.GoogleClientSecret = v
	}
	if v := os.Getenv("E2A_OAUTH_REDIRECT_URL"); v != "" {
		cfg.OAuth.RedirectURL = v
	}
	if v := os.Getenv("E2A_OAUTH_SIGNING_KEY"); v != "" {
		cfg.OAuth.SigningKey = v
	}
	if v := os.Getenv("E2A_OAUTH_SIGNING_KID"); v != "" {
		cfg.OAuth.SigningKID = v
	}
	if v := os.Getenv("E2A_OIDC_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.OIDC.Enabled = b
		}
	}
	if v := os.Getenv("E2A_OIDC_ISSUER_URL"); v != "" {
		cfg.OIDC.IssuerURL = v
	}
	if v := os.Getenv("E2A_OIDC_CLIENT_ID"); v != "" {
		cfg.OIDC.ClientID = v
	}
	if v := os.Getenv("E2A_OIDC_CLIENT_SECRET"); v != "" {
		cfg.OIDC.ClientSecret = v
	}
	if v := os.Getenv("E2A_OIDC_REDIRECT_URL"); v != "" {
		cfg.OIDC.RedirectURL = v
	}
	if v := os.Getenv("E2A_OIDC_USER_ID_CLAIM"); v != "" {
		cfg.OIDC.UserIDClaim = v
	}
	if v := os.Getenv("E2A_DELEGATED_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Delegated.Enabled = b
		}
	}
	if v := os.Getenv("E2A_PROVISIONING_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Provisioning.Enabled = b
		}
	}
	if v := os.Getenv("E2A_PROVISIONING_SECRET"); v != "" {
		cfg.Provisioning.Secret = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_HOST"); v != "" {
		cfg.OutboundSMTP.Host = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.OutboundSMTP.Port = p
		}
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_USERNAME"); v != "" {
		cfg.OutboundSMTP.Username = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_PASSWORD"); v != "" {
		cfg.OutboundSMTP.Password = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_FROM_DOMAIN"); v != "" {
		cfg.OutboundSMTP.FromDomain = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_MESSAGE_ID_DOMAIN"); v != "" {
		cfg.OutboundSMTP.MessageIDDomain = v
	}
	if v := os.Getenv("E2A_NOTIFICATIONS_FROM_ADDRESS"); v != "" {
		cfg.Notifications.FromAddress = v
	}
	if v := os.Getenv("E2A_NOTIFICATIONS_REPLY_TO"); v != "" {
		cfg.Notifications.ReplyTo = v
	}
	if v := os.Getenv("E2A_METRICS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Metrics.Enabled = b
		}
	}
	if v := os.Getenv("E2A_METRICS_LISTEN_ADDR"); v != "" {
		cfg.Metrics.ListenAddr = v
	}
	if v := os.Getenv("E2A_METRICS_BUILD"); v != "" {
		cfg.Metrics.Build = v
	}
	if v := os.Getenv("E2A_OUTBOUND_FOOTER_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.OutboundFooter.Enabled = b
		}
	}
	// An explicit empty listen_addr would otherwise bind ":80" (Go's
	// default) — silently public and usually fatal. Empty means default.
	if cfg.Metrics.ListenAddr == "" {
		cfg.Metrics.ListenAddr = "127.0.0.1:9091"
	}
	if v := os.Getenv("E2A_INBOUND_MODE"); v != "" {
		cfg.Inbound.Mode = v
	}
	if v := os.Getenv("E2A_WEBHOOK_FANOUT_MODE"); v != "" {
		cfg.WebhookFanout.Mode = v
	}
	if v := os.Getenv("E2A_WEBHOOK_INTERNAL_SINK_URL"); v != "" {
		cfg.Webhook.InternalSinkURL = v
	}
	if v := os.Getenv("E2A_OUTBOUND_SMTP_REQUIRE_TLS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.OutboundSMTP.RequireTLS = &b
		}
	}
	if v := os.Getenv("E2A_PUBLIC_URL"); v != "" {
		cfg.HTTP.PublicURL = v
	}
	if v := os.Getenv("E2A_API_URL"); v != "" {
		cfg.HTTP.APIURL = v
	}
	if v := os.Getenv("E2A_SHARED_DOMAIN"); v != "" {
		cfg.SharedDomain = v
	}
	if v := os.Getenv("E2A_SENDER_IDENTITY_SES_REGION"); v != "" {
		cfg.SenderIdentity.SESRegion = v
	}
	if v := os.Getenv("E2A_SENDER_IDENTITY_LEGACY_JOB_COMPAT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("E2A_SENDER_IDENTITY_LEGACY_JOB_COMPAT: %w", err)
		}
		cfg.SenderIdentity.LegacyJobCompat = b
	}
	if v := os.Getenv("E2A_SENDER_IDENTITY_FIXTURE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SenderIdentity.FixtureTTL = d
		} else {
			log.Printf("[config] E2A_SENDER_IDENTITY_FIXTURE_TTL=%q is not a Go duration — ignoring it and keeping %s", v, cfg.SenderIdentity.FixtureTTL)
		}
	}
	if v := os.Getenv("E2A_DEPLOYMENT_NAME"); v != "" {
		cfg.DeploymentName = v
	}
	// Normalize AFTER the env override so a typo in either source degrades the
	// same way. See DeploymentName's doc for why this is not a hard error.
	if name := strings.TrimSpace(cfg.DeploymentName); name != "prod" && name != "staging" {
		if name != "" {
			log.Printf("[config] deployment_name (or E2A_DEPLOYMENT_NAME) = %q is not %q or %q — treating this deployment as unnamed; resources e2a provisions will carry no environment tag", name, "prod", "staging")
		}
		cfg.DeploymentName = ""
	} else {
		cfg.DeploymentName = name
	}
	if v := os.Getenv("E2A_TRASH_RETENTION_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			cfg.Trash.RetentionDays = d
		}
	}
	if v := os.Getenv("E2A_DELIVERY_SES_CONFIGURATION_SET"); v != "" {
		cfg.DeliveryFeedback.SESConfigurationSet = v
	}
	if v := os.Getenv("E2A_DELIVERY_SNS_TOPIC_ARNS"); v != "" {
		cfg.DeliveryFeedback.SNSTopicARNs = splitAndTrim(v)
	}
	if v := os.Getenv("E2A_SMTP_PROXY_TRUSTED_CIDRS"); v != "" {
		cfg.SMTP.ProxyTrustedCIDRs = splitAndTrim(v)
	}

	// Default outbound TLS enforcement to on in production when not set
	// explicitly. SES on :587/:465 always advertises STARTTLS, so this is
	// a no-op for the real prod path and only fails closed if the TLS
	// capability disappears (misconfig or active stripping attack).
	if cfg.OutboundSMTP.RequireTLS == nil {
		v := cfg.IsProduction()
		cfg.OutboundSMTP.RequireTLS = &v
	}

	// Default the API URL (OAuth issuer + token/jwks host) to the web
	// PublicURL when unset, so single-host deployments and self-hosters get
	// the historical behaviour (issuer == public_url) with no new config.
	// Split deployments (web on one host, API/MCP on another) set api_url.
	if cfg.HTTP.APIURL == "" {
		cfg.HTTP.APIURL = cfg.HTTP.PublicURL
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces invariants that must hold before the server starts.
// In production mode the placeholder HMAC secret, an empty secret, and
// secrets shorter than the hash output length are hard rejected —
// running with any of these weakens approval tokens and derived encryption keys
// and approve HITL messages.
func (c *Config) Validate() error {
	// A negative daily cap is always a mistake (it would silently 402 every
	// send); 0 is a legal hard-block and absent/nil means no daily policy.
	if c.Limits.MaxMessagesDay != nil && *c.Limits.MaxMessagesDay < 0 {
		return fmt.Errorf("config: limits.max_messages_day is %d; must be >= 0 (omit the key for no daily cap)", *c.Limits.MaxMessagesDay)
	}
	if c.IsProduction() {
		if c.Signing.HMACSecret == "" {
			return errors.New("config: signing.hmac_secret (or E2A_HMAC_SECRET) must be set when env=production")
		}
		if c.Signing.HMACSecret == placeholderHMACSecret {
			return fmt.Errorf("config: signing.hmac_secret is the example placeholder %q; override it before running env=production", placeholderHMACSecret)
		}
		if len(c.Signing.HMACSecret) < minHMACSecretBytes {
			return fmt.Errorf("config: signing.hmac_secret is %d bytes; production requires at least %d (run `openssl rand -hex 32` to generate)", len(c.Signing.HMACSecret), minHMACSecretBytes)
		}
	}
	if c.HTTP.APIURL != "" {
		apiURL, err := absoluteHTTPURL(c.HTTP.APIURL)
		if err != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
			return errors.New("config: http.api_url must be an absolute http(s) URL without userinfo, query, or fragment")
		}
	}
	if c.Trash.RetentionDays < 1 {
		return fmt.Errorf("config: trash.retention_days must be at least 1 (got %d) — the stable API promises soft-deleted resources stay restorable", c.Trash.RetentionDays)
	}
	for _, cidr := range c.SMTP.ProxyTrustedCIDRs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("config: smtp.proxy_trusted_cidrs: malformed CIDR %q: %w", cidr, err)
		}
		if p.Bits() == 0 {
			return fmt.Errorf("config: smtp.proxy_trusted_cidrs: %q trusts every peer, which lets anyone spoof source IPs for SPF and logging; list only the proxy's own address(es)", cidr)
		}
	}
	if c.OIDC.Enabled {
		var missing []string
		if c.OIDC.IssuerURL == "" {
			missing = append(missing, "issuer_url")
		}
		if c.OIDC.ClientID == "" {
			missing = append(missing, "client_id")
		}
		if c.OIDC.ClientSecret == "" {
			missing = append(missing, "client_secret")
		}
		if c.OIDC.RedirectURL == "" {
			missing = append(missing, "redirect_url")
		}
		if c.OIDC.UserIDClaim == "" {
			missing = append(missing, "user_id_claim")
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: oidc.enabled requires %s to be set", strings.Join(missing, ", "))
		}
		issuerURL, err := absoluteHTTPURL(c.OIDC.IssuerURL)
		if err != nil || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
			return fmt.Errorf("config: oidc.issuer_url must be an absolute http(s) issuer URL without query or fragment")
		}
		redirectURL, err := absoluteHTTPURL(c.OIDC.RedirectURL)
		if err != nil || redirectURL.Fragment != "" {
			return fmt.Errorf("config: oidc.redirect_url must be an absolute http(s) URL without a fragment")
		}
	}
	if c.Delegated.Enabled {
		if err := c.validateDelegated(); err != nil {
			return err
		}
	}
	if c.Provisioning.Enabled {
		if c.Provisioning.Secret == "" {
			return errors.New("config: provisioning.enabled requires the provisioning secret (E2A_PROVISIONING_SECRET) to be set")
		}
		if c.IsProduction() && len(c.Provisioning.Secret) < minHMACSecretBytes {
			return fmt.Errorf("config: provisioning secret is %d bytes; production requires at least %d (run `openssl rand -hex 32` to generate)", len(c.Provisioning.Secret), minHMACSecretBytes)
		}
	}
	if c.SendingRamp.Enabled {
		// The send API accepts up to 50 deduplicated envelope recipients. A
		// lower day-one cap could strand a single accepted message forever,
		// because idle days deliberately do not advance the ramp.
		if c.SendingRamp.StartDaily < 50 {
			return fmt.Errorf("config: sending_ramp.start_daily must be at least 50 when enabled (got %d)", c.SendingRamp.StartDaily)
		}
		if c.SendingRamp.TargetDaily < c.SendingRamp.StartDaily {
			return fmt.Errorf("config: sending_ramp.target_daily must be >= start_daily")
		}
		if c.SendingRamp.RampDays < 1 {
			return fmt.Errorf("config: sending_ramp.ramp_days must be at least 1")
		}
	}
	if v := c.Notifications.FromAddress; v != "" {
		addr, err := mail.ParseAddress(v)
		if err != nil || addr.Address != v {
			return fmt.Errorf("config: notifications.from_address must be a bare email address (got %q)", v)
		}
	}
	if v := c.Notifications.ReplyTo; v != "" {
		addr, err := mail.ParseAddress(v)
		if err != nil || addr.Address != v {
			return fmt.Errorf("config: notifications.reply_to must be a bare email address (got %q)", v)
		}
	}
	return nil
}

// validateDelegated enforces the enabled delegated-verifier policy at
// startup: a malformed static configuration must never boot, while
// issuer NETWORK unavailability is deliberately not checked here — the
// verifier discovers in the background and delegated auth degrades to
// 503 until the issuer answers.
func (c *Config) validateDelegated() error {
	d := &c.Delegated
	var missing []string
	if d.IssuerURL == "" {
		missing = append(missing, "issuer_url")
	}
	if d.Audience == "" {
		missing = append(missing, "audience")
	}
	if d.AuthorizedParty == "" {
		missing = append(missing, "authorized_party")
	}
	if d.RequiredScope == "" {
		missing = append(missing, "required_scope")
	}
	if len(d.AllowedAlgorithms) == 0 {
		missing = append(missing, "allowed_algorithms")
	}
	if len(d.RequiredClaims) == 0 {
		missing = append(missing, "required_claims")
	}
	if len(d.ForbiddenClaims) == 0 {
		missing = append(missing, "forbidden_claims")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: delegated.enabled requires %s to be set", strings.Join(missing, ", "))
	}
	issuerURL, err := absoluteHTTPURL(d.IssuerURL)
	if err != nil || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return errors.New("config: delegated.issuer_url must be an absolute http(s) issuer URL without query or fragment")
	}
	if c.IsProduction() && issuerURL.Scheme != "https" {
		return errors.New("config: delegated.issuer_url must use https when env=production")
	}
	if strings.ContainsAny(d.RequiredScope, " \t\r\n") {
		return errors.New("config: delegated.required_scope must be a single scope token")
	}
	seenAlgs := map[string]struct{}{}
	for _, alg := range d.AllowedAlgorithms {
		if alg != "RS256" && alg != "ES256" {
			return fmt.Errorf("config: delegated.allowed_algorithms contains unsupported algorithm %q (RS256 and ES256 are supported)", alg)
		}
		if _, dup := seenAlgs[alg]; dup {
			return fmt.Errorf("config: delegated.allowed_algorithms lists %q twice", alg)
		}
		seenAlgs[alg] = struct{}{}
	}
	if d.MaxTokenLifetimeSeconds <= 0 {
		return errors.New("config: delegated.max_token_lifetime_seconds must be positive")
	}
	if d.ClockSkewSeconds < 0 {
		return errors.New("config: delegated.clock_skew_seconds must be nonnegative")
	}
	required := map[string]struct{}{}
	for _, rc := range d.RequiredClaims {
		if err := validateDelegatedClaimName(rc.Name, "required_claims"); err != nil {
			return err
		}
		if _, dup := required[rc.Name]; dup {
			return fmt.Errorf("config: delegated.required_claims lists %q twice", rc.Name)
		}
		required[rc.Name] = struct{}{}
		seenVals := map[string]struct{}{}
		for _, av := range rc.AllowedValues {
			if av == "" {
				return fmt.Errorf("config: delegated.required_claims %q has an empty allowed value", rc.Name)
			}
			if _, dup := seenVals[av]; dup {
				return fmt.Errorf("config: delegated.required_claims %q lists allowed value %q twice", rc.Name, av)
			}
			seenVals[av] = struct{}{}
		}
	}
	forbidden := map[string]struct{}{}
	for _, name := range d.ForbiddenClaims {
		if err := validateDelegatedClaimName(name, "forbidden_claims"); err != nil {
			return err
		}
		if _, dup := forbidden[name]; dup {
			return fmt.Errorf("config: delegated.forbidden_claims lists %q twice", name)
		}
		forbidden[name] = struct{}{}
		if _, overlap := required[name]; overlap {
			return fmt.Errorf("config: claim %q cannot be both required and forbidden", name)
		}
	}
	return nil
}

// validateDelegatedClaimName rejects empty, oversized (>128 bytes),
// non-ASCII, control-character, and reserved claim names in the
// delegated claim lists.
func validateDelegatedClaimName(name, list string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("config: delegated.%s claim names must be 1..128 bytes", list)
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7f {
			return fmt.Errorf("config: delegated.%s claim name %q must be printable ASCII", list, name)
		}
	}
	if _, reserved := delegatedReservedClaims[name]; reserved {
		return fmt.Errorf("config: delegated.%s must not list the reserved claim %q — the verifier already owns it", list, name)
	}
	return nil
}

func absoluteHTTPURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid absolute http(s) URL")
	}
	return parsed, nil
}
