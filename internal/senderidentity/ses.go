package senderidentity

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/tokencanopy/e2a/internal/mailfrom"
)

// sesAPI is the slice of the SES v2 client the provider uses. Narrowing to an
// interface lets the status-mapping logic be unit-tested with a stub (the
// AWS-touching calls themselves are exercised only against live SES).
type sesAPI interface {
	CreateEmailIdentity(ctx context.Context, in *sesv2.CreateEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error)
	PutEmailIdentityDkimSigningAttributes(ctx context.Context, in *sesv2.PutEmailIdentityDkimSigningAttributesInput, optFns ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityDkimSigningAttributesOutput, error)
	PutEmailIdentityMailFromAttributes(ctx context.Context, in *sesv2.PutEmailIdentityMailFromAttributesInput, optFns ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityMailFromAttributesOutput, error)
	GetEmailIdentity(ctx context.Context, in *sesv2.GetEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
	DeleteEmailIdentity(ctx context.Context, in *sesv2.DeleteEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.DeleteEmailIdentityOutput, error)
	ListEmailIdentities(ctx context.Context, in *sesv2.ListEmailIdentitiesInput, optFns ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error)
	TagResource(ctx context.Context, in *sesv2.TagResourceInput, optFns ...func(*sesv2.Options)) (*sesv2.TagResourceOutput, error)
}

// SESProvider is the real Provider backed by AWS SES v2. It registers
// domain sending identities with BYODKIM, reusing e2a's per-domain DKIM key
// so the DKIM d= aligns with the From domain (DMARC passes on DKIM
// alignment), and configures a custom MAIL FROM subdomain (bounce.<domain>) so
// the Return-Path aligns too (SPF passes on the From org-domain → no "via e2a").
// Every created identity carries an e2a ownership tag. An existing untagged
// identity is ADOPTED (tagged, then treated as owned) only when it is provably
// e2a's own — see canAdoptIdentity — which is what lets a domain created before
// the ownership tag shipped recover instead of being permanently stranded.
// Every other untagged identity is never adopted or mutated; IAM independently
// applies the same resource-tag condition to close the client-side
// check/mutation race.
type SESProvider struct {
	api                sesAPI
	region             string // for the custom MAIL FROM MX target (feedback-smtp.<region>.amazonses.com)
	deleteConfirmDelay func(attempt int) time.Duration

	// refuseAdoption, when true, makes Provision/Status refuse to adopt ANY
	// untagged identity regardless of what canAdoptIdentity would otherwise
	// conclude (see the doc on canAdoptIdentity's "Reachability bound" —
	// batch C finding 10). It does NOT affect fresh CreateEmailIdentity
	// calls: a brand-new identity is always tagged as e2a's own on creation,
	// so there is nothing to "adopt" there. Zero value (false) preserves
	// every existing caller's behavior, including every direct-struct-literal
	// SESProvider in this package's tests — production-only enforcement is
	// opt-IN via NewSESProviderFromConfig's production parameter, not opt-out.
	refuseAdoption bool

	// accountID/partition resolution feeds the identity ARN adoption's
	// TagResource call needs (arn:<partition>:ses:<region>:<accountID>:
	// identity/<domain>) — GetEmailIdentity/ListEmailIdentities never return
	// one. It is LAZY and NON-FATAL: only adoption ever needs it, so a
	// failure here must never take down every other Provider capability (or
	// the whole server — cmd/e2a/main.go log.Fatalf's on
	// NewSESProviderFromConfig's returned error). resolveIdentity is nil when
	// the caller already supplied the account ID directly (NewSESProvider);
	// identityForAdoption then short-circuits to it (and the "aws" commercial
	// partition default, since there is no STS ARN to derive it from on that
	// path) without ever touching STS. See identityForAdoption.
	//
	// accountIDMu guards the rest of this block AND bounds one resolution
	// attempt to accountIDResolveTimeout, so a wedged STS call cannot hold
	// the lock (and therefore every concurrent adoption attempt) forever.
	// ONLY a successful resolution is cached (accountIDResolved=true,
	// permanent for the process); a failure is cached for
	// accountIDRetryCooldown only, then retried by the next caller — see
	// identityForAdoption's doc for why a permanently-cached failure is
	// unacceptable here.
	accountIDMu       sync.Mutex
	accountID         string
	partition         string
	accountIDResolved bool
	accountIDErr      error
	accountIDFailedAt time.Time
	resolveIdentity   func(ctx context.Context) (accountID, partition string, err error)
}

const (
	managedIdentityTagKey   = "e2a-managed"
	managedIdentityTagValue = "sender-identity-v1"
	deleteConfirmAttempts   = 6

	// accountIDResolveTimeout bounds a single STS GetCallerIdentity attempt,
	// deliberately independent of the caller's own context deadline (see
	// identityForAdoption). Generous for a single AWS API round trip, tight
	// enough that a wedged call cannot hold accountIDMu for long.
	accountIDResolveTimeout = 10 * time.Second
	// accountIDRetryCooldown bounds how long a failed resolution is cached
	// before the next caller retries STS. Long enough that a page of ~25
	// reaper candidates hitting a real STS outage doesn't hammer it with 25
	// near-simultaneous calls; short enough that a transient blip self-heals
	// within the SAME hourly sweep instead of requiring a process restart.
	accountIDRetryCooldown = 30 * time.Second
)

// NewSESProvider wraps a pre-built SES API (or stub) with an ALREADY-KNOWN
// AWS account id. region feeds the MAIL FROM MX record target; accountID
// feeds the identity ARN adoption's TagResource call needs. No STS call is
// ever made on this path, so there is no ARN to derive a partition from —
// the ARN is built against the "aws" commercial partition. Every current
// production caller (NewSESProviderFromConfig, via main.go) resolves the
// partition from STS instead; this constructor exists for tests and any
// future caller that already knows its account id out-of-band.
func NewSESProvider(api sesAPI, region, accountID string) *SESProvider {
	return &SESProvider{api: api, region: region, accountID: accountID, partition: "aws"}
}

// NewSESProviderFromConfig builds a provider from ambient AWS config
// (env/instance role) for the given region. Unlike the account id, loading
// the ambient config itself is local (env/file reads, no network round-trip)
// and stays synchronous here — a genuinely broken/missing AWS config is a
// legitimate reason to fail startup. The AWS account id AND partition
// adoption's TagResource call needs are resolved LAZILY on first adoption
// attempt (see identityForAdoption) rather than here: STS GetCallerIdentity
// is a network call, and resolving it eagerly at construction — as this used
// to do — let a transient STS blip, an egress allowlist covering only SES,
// an SCP deny, or an IMDS hiccup fail server startup entirely (main.go wraps
// provider construction in log.Fatalf) for a value only adoption needs.
//
// production gates adoption itself (batch C finding 10), independent of the
// account-id/STS plumbing above: canAdoptIdentity's "reachability bound" —
// that adoption is only ever attempted for a domain e2a's own DNS probe
// already confirmed the caller controls — holds only when domain
// verification actually enforces that probe, which
// internal/agent/api.go's checkDomainRecords short-circuits to
// unconditionally-"found" whenever !production. Passing production=false
// (i.e. cfg.Env != "production") makes Provision/Status refuse to adopt any
// untagged identity outright, regardless of what canAdoptIdentity would
// otherwise conclude, closing that gap for any non-production deployment
// that nonetheless configures sender_identity.ses_region against real AWS
// (e.g. a misconfigured `env`, or a self-hoster testing against live SES).
func NewSESProviderFromConfig(ctx context.Context, region string, production bool) (*SESProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	stsClient := sts.NewFromConfig(cfg)
	return &SESProvider{
		api:            sesv2.NewFromConfig(cfg),
		region:         region,
		refuseAdoption: !production,
		resolveIdentity: func(ctx context.Context) (string, string, error) {
			out, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return "", "", fmt.Errorf("resolve aws account id: %w", err)
			}
			return identityFromCallerIdentity(out)
		},
	}, nil
}

// identityFromCallerIdentity extracts the account id and ARN partition,
// treating a nil OR EMPTY Account as an error rather than silently yielding
// an empty account id segment in the identity ARN
// (arn:<partition>:ses:<region>::identity/<domain> — missing the account id
// component entirely, which TagResource would then reject or, worse,
// resolve unpredictably). An empty string is exactly the malformed-ARN shape
// this guard exists to prevent, so it is rejected the same way a nil pointer
// is — a defensive check, since AWS is not documented to ever return "" for
// a present Account field. The
// partition comes from STS's own Arn field (arn:<partition>:sts::...): AWS
// commercial is "aws", GovCloud is "aws-us-gov", China is "aws-cn" —
// hardcoding "aws" would build an invalid ARN and fail every TagResource
// call in the other two partitions. A nil or unparseable Arn degrades to the
// "aws" default rather than failing account id resolution entirely, since
// GetCallerIdentity's Account field (not Arn) is the one AWS guarantees; Arn
// is documented but not contractually required to be present. Split out from
// NewSESProviderFromConfig so it's unit-testable without a real STS call.
func identityFromCallerIdentity(out *sts.GetCallerIdentityOutput) (accountID, partition string, err error) {
	if out == nil || out.Account == nil || *out.Account == "" {
		return "", "", errors.New("sts GetCallerIdentity returned no account id")
	}
	partition = "aws"
	if out.Arn != nil {
		if p, ok := arnPartition(*out.Arn); ok {
			partition = p
		}
	}
	return *out.Account, partition, nil
}

// arnPartition extracts the partition segment from an ARN
// ("arn:<partition>:<service>:...", e.g. "aws", "aws-us-gov", "aws-cn").
func arnPartition(arn string) (string, bool) {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) < 3 || parts[0] != "arn" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// identityForAdoption resolves the AWS account id and ARN partition
// adoption's TagResource call needs. Nothing else Provision/Status/
// Deprovision does requires it — only adoption calls TagResource — so a
// resolution failure must never be fatal: it degrades adoptIdentity to a
// wrapped ErrIdentityNotOwned ("cannot adopt"), exactly the pre-adoption-PR
// behavior, while every other capability keeps working.
//
// CACHES ONLY SUCCESS, permanently, for the process — a failure is cached
// for accountIDRetryCooldown only (batch C finding 1). This used to be a
// sync.Once, which caches the FIRST result forever, success or failure. That
// is a correctness bug, not just a missed optimization: the cached ctx
// belongs to the FIRST caller — a River job — and internal/jobs/jobs.go sets
// no JobTimeout, so River's 1-minute default applies. reapManagedIdentityPage
// processes up to 25 domains per job with several SES round trips each, so
// on the very first post-upgrade sweep — exactly when the untagged
// population is largest — the job deadline can land mid-STS-call. A
// sync.Once would then cache context.DeadlineExceeded FOREVER: every LATER
// adoption attempt (any domain, any job, for the rest of the process's
// life) would return a wrapped ErrIdentityNotOwned, every owned-but-untagged
// domain would flip to `failed`, and a domain.sending_failed webhook would
// fire to each affected customer — recoverable only by a process restart.
// That is the exact failure shape this whole PR exists to fix, just moved
// one layer down. Fixed two ways together:
//
//   - resolveIdentity now runs on context.WithoutCancel(ctx) with its own
//     explicit accountIDResolveTimeout, detached from whatever deadline the
//     FIRST caller's ctx happened to carry.
//   - the cache is a mutex, not sync.Once: only a SUCCESSFUL resolution is
//     permanent; a failure is remembered for accountIDRetryCooldown and then
//     retried by the next caller. Adoption is already rate-limited to
//     roughly hourly per domain by the reaper, so retrying on the next
//     attempt is not hammering STS — the cooldown exists only to keep a
//     single sweep's ~25-domain page from firing 25 near-simultaneous calls
//     at a genuinely down STS, not to suppress legitimate retry.
func (p *SESProvider) identityForAdoption(ctx context.Context) (accountID, partition string, err error) {
	if p.resolveIdentity == nil {
		return p.accountID, p.partition, nil // supplied directly at construction (NewSESProvider)
	}
	p.accountIDMu.Lock()
	defer p.accountIDMu.Unlock()
	if p.accountIDResolved {
		return p.accountID, p.partition, nil
	}
	if !p.accountIDFailedAt.IsZero() && time.Since(p.accountIDFailedAt) < accountIDRetryCooldown {
		return "", "", p.accountIDErr
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountIDResolveTimeout)
	defer cancel()
	accountID, partition, err = p.resolveIdentity(resolveCtx)
	if err != nil {
		p.accountIDErr = err
		p.accountIDFailedAt = time.Now()
		log.Printf("[senderidentity] cannot resolve AWS account id for adoption (STS GetCallerIdentity failed): %v — will retry after %s; adoption reports identities as not-owned until then", err, accountIDRetryCooldown)
		return "", "", err
	}
	p.accountID, p.partition = accountID, partition
	p.accountIDResolved = true
	return accountID, partition, nil
}

func (p *SESProvider) Provision(ctx context.Context, domain, dkimSelector string, dkimPrivateKeyDER []byte) (Result, error) {
	privB64, err := pkcs8Base64(dkimPrivateKeyDER)
	if err != nil {
		// A malformed key is not retryable — fail closed with a reason.
		return Result{Status: StatusFailed, Error: "dkim private key not usable for BYODKIM: " + err.Error()}, nil
	}
	dkimAttributes := &sestypes.DkimSigningAttributes{
		DomainSigningSelector:         &dkimSelector,
		DomainSigningPrivateKey:       &privB64,
		DomainSigningAttributesOrigin: sestypes.DkimSigningAttributesOriginExternal,
	}
	_, err = p.api.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity:         &domain,
		DkimSigningAttributes: dkimAttributes,
		Tags: []sestypes.Tag{{
			Key:   awsString(managedIdentityTagKey),
			Value: awsString(managedIdentityTagValue),
		}},
	})
	if err != nil {
		// AlreadyExists means the domain may belong to an older registration.
		// Update BYODKIM explicitly before touching MAIL FROM: Create's input is
		// ignored on this path, and keeping the old selector/key would make a
		// re-registered domain fail verification forever.
		var already *sestypes.AlreadyExistsException
		if !errors.As(err, &already) {
			return Result{}, err // transient/permission — retry
		}
		existing, getErr := p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
		if getErr != nil {
			return Result{}, getErr
		}
		if !isManagedIdentity(existing) {
			evidence := AdoptionEvidence{Selector: dkimSelector, HasPrivateKey: len(dkimPrivateKeyDER) > 0}
			if ok, reason := p.adoptionDecision(existing, evidence); !ok {
				// Wrap the refusal reason (batch C finding 2): canAdoptIdentity
				// now reports WHICH criterion failed, so the operator-facing ALERT
				// log at both worker.go call sites (which logs %v of this error)
				// can distinguish a missing IAM grant from a mismatched selector
				// from a genuinely foreign identity, instead of all three
				// reporting the identical bare ErrIdentityNotOwned.
				return Result{}, fmt.Errorf("%w: %s", ErrIdentityNotOwned, reason)
			}
			if err := p.adoptIdentity(ctx, domain); err != nil {
				return Result{}, err // transient/permission on the tag call — retry
			}
		}
		// Put's nested shape deliberately omits DomainSigningAttributesOrigin.
		// SES rejects every nested Origin value for this operation; EXTERNAL
		// belongs only on the top-level SigningAttributesOrigin field.
		putAttributes := &sestypes.DkimSigningAttributes{
			DomainSigningSelector:   &dkimSelector,
			DomainSigningPrivateKey: &privB64,
		}
		if _, err := p.api.PutEmailIdentityDkimSigningAttributes(ctx, &sesv2.PutEmailIdentityDkimSigningAttributesInput{
			EmailIdentity:           &domain,
			SigningAttributesOrigin: sestypes.DkimSigningAttributesOriginExternal,
			SigningAttributes:       putAttributes,
		}); err != nil {
			return Result{}, err // transient/permission — retry
		}
	}

	// Configure the custom MAIL FROM subdomain (Return-Path alignment). Idempotent.
	// USE_DEFAULT_VALUE: if the customer's MX later breaks, SES falls back to its
	// own MAIL FROM rather than dropping mail — deliverability-safe (the send path
	// only uses the aligned envelope once status==verified, which requires the MX).
	mfDomain := mailfrom.Domain(domain)
	if _, err := p.api.PutEmailIdentityMailFromAttributes(ctx, &sesv2.PutEmailIdentityMailFromAttributesInput{
		EmailIdentity:       &domain,
		MailFromDomain:      &mfDomain,
		BehaviorOnMxFailure: sestypes.BehaviorOnMxFailureUseDefaultValue,
	}); err != nil {
		return Result{}, err // transient/permission — retry
	}

	return Result{Status: StatusPending, DNSRecords: mailFromRecords(domain, p.region)}, nil
}

// mailFromRecords are the two records the customer must publish for the custom
// MAIL FROM subdomain: an MX to SES's regional feedback handler and an SPF TXT
// so SPF authenticates (and aligns to) the From org-domain. Shared by the SES
// provider and the FakeProvider so tests assert the real shape.
func mailFromRecords(domain, region string) []DNSRecord {
	mf := mailfrom.Domain(domain)
	return []DNSRecord{
		{Type: "MX", Name: mf, Value: fmt.Sprintf("10 feedback-smtp.%s.amazonses.com", region)},
		{Type: "TXT", Name: mf, Value: "v=spf1 include:amazonses.com ~all"},
	}
}

func (p *SESProvider) Status(ctx context.Context, domain string, evidence AdoptionEvidence) (Result, error) {
	out, err := p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
	if err != nil {
		var notFound *sestypes.NotFoundException
		if errors.As(err, &notFound) {
			return Result{}, ErrIdentityNotFound
		}
		return Result{}, err
	}
	if !isManagedIdentity(out) {
		// Self-heals an ownership tag removed out-of-band on an identity e2a
		// otherwise still provably owns (matching BYODKIM selector), not just
		// the initial adoption of a pre-tag-release identity.
		if ok, reason := p.adoptionDecision(out, evidence); !ok {
			// See the identical comment in Provision: the reason string lets
			// the caller's ALERT log distinguish WHY adoption was refused.
			return Result{}, fmt.Errorf("%w: %s", ErrIdentityNotOwned, reason)
		}
		if err := p.adoptIdentity(ctx, domain); err != nil {
			return Result{}, err // transient/permission on the tag call — retry
		}
	}
	// Re-emit the MAIL FROM records on every poll so the verify/failed transition
	// preserves them (ReconcileWorker writes res.DNSRecords) — a verified domain's
	// view keeps showing the MX/SPF the customer must KEEP published.
	dkimAxis, mailFromAxis := sesAxisStatuses(out)
	return Result{
		Status:         mapSESStatus(out),
		DkimStatus:     dkimAxis,
		MailFromStatus: mailFromAxis,
		DNSRecords:     mailFromRecords(domain, p.region),
	}, nil
}

func (p *SESProvider) Deprovision(ctx context.Context, domain string) error {
	out, err := p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
	if err != nil {
		var notFound *sestypes.NotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	if !isManagedIdentity(out) {
		return ErrIdentityNotOwned
	}
	_, err = p.api.DeleteEmailIdentity(ctx, &sesv2.DeleteEmailIdentityInput{EmailIdentity: &domain})
	if err != nil {
		var notFound *sestypes.NotFoundException
		if errors.As(err, &notFound) {
			return nil // already gone — idempotent success
		}
		return err
	}
	// DELETE success is followed by bounded absence polling so the HTTP
	// domain-delete boundary can safely release DNS without false-red failures
	// during normal SES eventual consistency. The outer job/reaper remains the
	// durable retry path if this short confirmation window is exhausted.
	for attempt := 0; attempt < deleteConfirmAttempts; attempt++ {
		_, err = p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
		if err != nil {
			var notFound *sestypes.NotFoundException
			if errors.As(err, &notFound) {
				return nil
			}
			return err
		}
		if attempt+1 < deleteConfirmAttempts {
			delay := p.confirmDelay(attempt)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return errors.New("senderidentity: provider identity still present after delete")
}

func (p *SESProvider) confirmDelay(attempt int) time.Duration {
	if p.deleteConfirmDelay != nil {
		return p.deleteConfirmDelay(attempt)
	}
	delay := 100 * time.Millisecond * time.Duration(1<<attempt)
	if delay > time.Second {
		return time.Second
	}
	return delay
}

func isManagedIdentity(out *sesv2.GetEmailIdentityOutput) bool {
	if out == nil {
		return false
	}
	for _, tag := range out.Tags {
		if tag.Key != nil && tag.Value != nil && *tag.Key == managedIdentityTagKey && *tag.Value == managedIdentityTagValue {
			return true
		}
	}
	return false
}

// canAdoptIdentity reports whether an UNTAGGED existing identity is provably
// e2a's own, making the missing ownership tag a migration artifact (created
// before release v1.7.8 introduced it, or removed out-of-band) rather than
// evidence of a foreign identity. All of the following must hold:
//
//   - the identity carries no foreign configuration: ConfigurationSetName is
//     nil AND Policies is empty. e2a's own Provision never sets either, so
//     either one present is affirmative evidence this identity is doing
//     something e2a didn't ask for — and adoption would silently KEEP it
//     (Provision/Status never touch these fields, so nothing would ever
//     clear them). A configuration set controls where SES routes delivery/
//     bounce/complaint feedback for this identity's sends, including
//     recipient addresses; a policy can grant another AWS account send
//     permissions on it. This is checked first and independent of the
//     BYODKIM/selector reasoning below.
//   - evidence.HasPrivateKey is true: e2a actually has DKIM private key
//     material on file for this domain — NOT merely a stored selector.
//     evidence.Selector non-empty is a DIFFERENT fact than key material being
//     present: LoadSendingIdentityState can return a non-empty selector with
//     a nil/empty private key (e.g. mid domain-reclaim — see
//     internal/identity's key lifecycle), and adopting on the selector alone
//     would tag an identity e2a cannot actually sign for. That is not inert:
//     the caller's own no-key branch (worker.go) then finds the identity
//     tagged and calls Deprovision, which now SUCCEEDS and DELETES it —
//     pre-adoption behavior refused that delete outright. Provision builds
//     its AdoptionEvidence with HasPrivateKey: len(dkimPrivateKeyDER)>0
//     directly, since it already receives the raw key bytes. Status's own
//     interface carries no key bytes, so ITS caller (worker.go, at both
//     provider.Status call sites) is responsible for passing accurate
//     evidence — see Provider.Status's doc.
//   - evidence.Selector is non-empty: e2a has a stored DKIM selector on file
//     for this domain.
//   - the identity's DKIM was configured via BYODKIM (SigningAttributesOrigin
//     == EXTERNAL). Only e2a's own Provision supplies signing key material;
//     AWS_SES (Easy DKIM, SES-generated keys) can never be e2a's doing.
//   - DkimAttributes.Status == SUCCESS: SES has independently matched
//     whatever private key material IT HOLDS against the DNS TXT published
//     at <selector>._domainkey.<domain> — that is what DKIM verification
//     means for a BYODKIM identity, and it costs no extra provider call (Get
//     already returns it). This is materially stronger than the token match
//     below: dkim.SelectorForNow's monthly convention is a publicly-derivable
//     OSS constant with zero entropy of its own, so the token match alone
//     proves only that SOME identity was configured with e2a's naming
//     scheme — not that the key SES holds for it matches that domain's DNS
//     at all. Requiring SUCCESS closes THAT gap without e2a resolving DNS
//     itself (deliberately out of scope for the provider layer — a
//     DNS-comparing follow-up is a possible future hardening, not required
//     here). CAUTION — SUCCESS is not proof the key is e2a's OWN key: for a
//     hypothetical foreign BYODKIM identity (some other application's, in a
//     shared AWS account) that already reached provider-side SUCCESS, this
//     bit is equally SUCCESS — it is that OTHER app's key SES matched, not
//     e2a's. SUCCESS rules out an identity that was never verified at all;
//     it is the exact-selector match below, combined with the reachability
//     bound (this domain is independently DNS-verified as caller-controlled
//     — see below), that does the actual ownership work.
//     Design choice, made explicit because it trades off against a real
//     population: SUCCESS is required STRICTLY, with no fallback for
//     PENDING. A legacy BYODKIM identity that is still provider-side PENDING
//     is refused (ErrIdentityNotOwned) rather than adopted. This is
//     deliberately conservative rather than exhaustive: every domain this
//     feature exists to unstrand was, by construction, already at e2a's own
//     `sending_status=verified` before the ownership-tag regression that
//     stranded it (mapSESStatus's all-or-nothing rollup already required
//     DKIM SUCCESS to reach `verified`), so a legacy identity's SES-side DKIM
//     status does not change independent of the tag — the incident
//     population is SUCCESS by construction. A genuinely-still-PENDING
//     legacy identity was never verified through e2a in the first place and
//     is not this feature's target; refusing it fails exactly as adoption
//     not existing at all would have. Revisit only with concrete evidence of
//     a real non-SUCCESS legacy population left behind by this rule.
//   - the selector SES reports installed (DkimAttributes.Tokens — for an
//     EXTERNAL-origin identity this holds the BYODKIM selector, not a set of
//     Easy-DKIM CNAME tokens) matches evidence.Selector EXACTLY.
//
// The selector match is scoped to one domain by construction: both sides come
// from a single domain's GetEmailIdentity call and a single domain's stored
// row, so this can never adopt a DIFFERENT domain's identity even though the
// monthly selector convention (dkim.SelectorForNow) is not itself globally
// unique. This is the security-critical negative case: a too-loose adoption
// rule would let e2a silently take over another application's SES identity in
// a shared AWS account — the exact scenario the ownership tag exists to
// prevent. Every other combination (no key material, AWS_SES origin, a
// mismatched or absent selector, DKIM not yet SUCCESS) must keep returning
// ErrIdentityNotOwned.
//
// Reachability bound on attacker-controlled input: adoption is only ever
// ATTEMPTED for a domain e2a's own database has independently confirmed the
// caller controls. Provision/Status are invoked from
// internal/senderidentity/worker.go only once LoadSendingIdentityState
// reports Verified==true (domains.verified=true) — which itself requires a
// live DNS probe of BOTH the ownership TXT record AND an apex MX record
// pointing at the e2a relay (see internal/httpapi/domains.go's
// handleVerifyDomain/VerifyDomain). No customer can aim adoption at a domain
// they don't control; a matching selector token is necessary but this
// verification gate is what makes it sufficient to trust at all.
//
// CAVEAT (batch C finding 10): that bound holds only when the DNS probe
// above actually runs. internal/agent/api.go's checkDomainRecords
// short-circuits to TXTFound=true/MX="found" for EVERY domain whenever
// !production (config env != "production") — a deliberate dev/test
// convenience so local flows work without real DNS. In that mode ANY
// customer-typed domain reaches domains.verified=true instantly, with no
// ownership proof at all, and this function has no way to tell. This
// function itself does not gate on production — that would need threading
// deployment-environment awareness through a pure per-identity decision
// function used from a security-critical, exhaustively-table-tested code
// path. Instead SESProvider.refuseAdoption (set from
// NewSESProviderFromConfig's production parameter, itself
// cfg.IsProduction()) refuses adoption at the Provision/Status call sites
// BEFORE this function ever runs — see adoptionDecision. A misconfigured
// non-production deployment that nonetheless points sender_identity at real
// AWS therefore still cannot silently take over a pre-existing SES identity
// via adoption; it can only fall back to ErrIdentityNotOwned exactly like a
// genuinely foreign one.
func canAdoptIdentity(out *sesv2.GetEmailIdentityOutput, evidence AdoptionEvidence) (ok bool, reason string) {
	if out == nil {
		return false, "no provider identity to evaluate"
	}
	if out.DkimAttributes == nil {
		return false, "provider identity has no DKIM attributes"
	}
	if evidence.Selector == "" {
		return false, "no DKIM selector on file for this domain"
	}
	if !evidence.HasPrivateKey {
		return false, "no DKIM private key material on file for this domain"
	}
	// A configuration set or an identity policy is FOREIGN configuration e2a
	// never writes (Provision never sets either). Adopting an identity that
	// carries one would keep it: (a) ConfigurationSetName controls where SES
	// routes delivery/bounce/complaint feedback — including recipient
	// addresses — for every send through this identity; when
	// delivery_feedback.ses_configuration_set is unset (self-hosts, e2a's own
	// staging) SES falls back to the IDENTITY's default config set, so
	// adopting silently hands that traffic's feedback to whoever owns the
	// set. (b) An identity policy (e.g. granting ses:SendRawEmail to another
	// AWS account) would survive adoption and keep applying. Either one means
	// this identity is not purely e2a's — refuse it exactly like a foreign
	// identity, before any of the BYODKIM/selector checks below.
	if out.ConfigurationSetName != nil || len(out.Policies) > 0 {
		return false, "identity carries foreign configuration (a configuration set or an identity policy e2a never writes)"
	}
	if out.DkimAttributes.SigningAttributesOrigin != sestypes.DkimSigningAttributesOriginExternal {
		return false, "DKIM signing origin is not BYODKIM (AWS_SES/Easy DKIM can never be e2a's own doing)"
	}
	// SES has cryptographically matched the key material IT holds against the
	// DNS TXT at <selector>._domainkey.<domain> — see the doc comment above
	// for why this is required strictly, with no PENDING fallback.
	if out.DkimAttributes.Status != sestypes.DkimStatusSuccess {
		return false, fmt.Sprintf("DKIM verification status is %q, not SUCCESS", out.DkimAttributes.Status)
	}
	for _, token := range out.DkimAttributes.Tokens {
		if token == evidence.Selector {
			return true, ""
		}
	}
	return false, "installed DKIM selector does not match the selector e2a has on file for this domain"
}

// adoptionDecision applies the non-production adoption guard (refuseAdoption
// — batch C finding 10) on top of canAdoptIdentity's pure per-identity
// criteria, and always returns a non-empty reason on refusal so both
// Provision and Status can build a diagnosable, wrapped ErrIdentityNotOwned
// regardless of which check refused (finding 2).
func (p *SESProvider) adoptionDecision(out *sesv2.GetEmailIdentityOutput, evidence AdoptionEvidence) (ok bool, reason string) {
	if p.refuseAdoption {
		return false, "adoption is refused outside production: domain verification's DNS gate (which adoption's reachability bound depends on) is not enforced when env != \"production\" — see canAdoptIdentity's doc"
	}
	return canAdoptIdentity(out, evidence)
}

// adoptIdentity applies the ownership tag to an identity canAdoptIdentity has
// already cleared. TagResource needs the identity's full ARN — neither
// GetEmailIdentity nor ListEmailIdentities returns one — so it's built from
// the region and the account ID/partition resolved lazily (see
// identityForAdoption). An unresolvable account id degrades to
// ErrIdentityNotOwned rather than building a malformed/empty-account ARN or
// blocking indefinitely.
func (p *SESProvider) adoptIdentity(ctx context.Context, domain string) error {
	accountID, partition, err := p.identityForAdoption(ctx)
	if err != nil {
		return fmt.Errorf("%w: aws account id unavailable: %w", ErrIdentityNotOwned, err)
	}
	arn := identityARN(partition, p.region, accountID, domain)
	_, err = p.api.TagResource(ctx, &sesv2.TagResourceInput{
		ResourceArn: &arn,
		Tags: []sestypes.Tag{{
			Key:   awsString(managedIdentityTagKey),
			Value: awsString(managedIdentityTagValue),
		}},
	})
	if err != nil {
		return classifyAdoptionError(err)
	}
	log.Printf("[senderidentity] adopted pre-existing identity for domain %s (tagged %s=%s)", domain, managedIdentityTagKey, managedIdentityTagValue)
	return nil
}

// classifyAdoptionError maps a TagResource error that can never succeed on
// retry into a wrapped ErrIdentityNotOwned, and leaves everything else
// (throttling, 5xx, network) untouched for the caller to retry. Both
// Provision and Status call adoptIdentity and, pre-fix, returned its raw
// error with a "transient/permission — retry" comment; for a permanent error
// that meant the reaper returned an error on EVERY hourly sweep forever
// (never resolving, never flagged prominently), and the reconcile path burned
// its whole attempt budget before mislabeling the domain "verification timed
// out" — sending operators chasing DNS instead of the real IAM problem.
//
//   - AccessDeniedException: the runtime IAM principal lacks ses:TagResource
//     (or a resource-tag/request-tag condition denies it) for this specific
//     identity ARN — will never succeed without an IAM change. This is
//     unmodeled by the SES v2 SDK's generated exception types (AWS returns
//     IAM-level denials generically, not as a service-specific shape), so it
//     is matched by smithy's APIError.ErrorCode() rather than errors.As on a
//     concrete *sestypes type.
//   - BadRequestException: a malformed/invalid ARN — will never succeed
//     without a code change.
//
// A TagResource NotFoundException is handled SEPARATELY below, not folded in
// here: it means the identity vanished between the GetEmailIdentity that fed
// canAdoptIdentity and this TagResource call (a delete raced adoption), not
// that the ARN was wrong or foreign — see below.
//
// Verified against the live prod IAM policy (2026-08): ses:TagResource IS
// allowed for e2a.dev's own runtime principal, so the catastrophic
// every-sweep-red variant isn't active in production today — but a
// self-hoster following the design doc's narrower, CONDITIONED grant would
// hit exactly this.
func classifyAdoptionError(err error) error {
	if err == nil {
		return nil
	}
	// NotFoundException on TagResource means the identity resolved fine
	// moments ago (canAdoptIdentity ran against a fresh GetEmailIdentity) but
	// is gone NOW — a delete raced adoption, not evidence of a foreign
	// identity. Misclassifying this as ErrIdentityNotOwned produced a wrong
	// customer-visible sending_error ("not managed by e2a" for an identity
	// that simply disappeared) and a spurious domain.sending_failed webhook.
	// ErrIdentityNotFound is what callers already treat as "repair/converge"
	// (reconcileProviderIdentity's repairMissingIdentity branch re-enters
	// convergeWorkerIdentity, which re-creates a fresh, e2a-tagged identity).
	var notFound *sestypes.NotFoundException
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", ErrIdentityNotFound, err)
	}
	var badRequest *sestypes.BadRequestException
	if errors.As(err, &badRequest) {
		// Double %w: ErrIdentityNotOwned drives caller control flow
		// (errors.Is), while the original error stays reachable for logs/
		// diagnostics (also via errors.Is, and through %v in any wrapping
		// fmt.Errorf upstream).
		return fmt.Errorf("%w: %w", ErrIdentityNotOwned, err)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException" {
		return fmt.Errorf("%w: %w", ErrIdentityNotOwned, err)
	}
	return err
}

// identityARN builds the SES v2 email-identity ARN for domain:
// arn:<partition>:ses:<region>:<account-id>:identity/<domain>. partition is
// derived from STS's own ARN (see identityFromCallerIdentity) rather than
// hardcoded "aws": GovCloud (aws-us-gov) and China (aws-cn) use a different
// partition, and a hardcoded "aws" there builds an ARN TagResource rejects
// as invalid, permanently failing adoption in those partitions.
func identityARN(partition, region, accountID, domain string) string {
	return fmt.Sprintf("arn:%s:ses:%s:%s:identity/%s", partition, region, accountID, domain)
}

func awsString(value string) *string { return &value }

func (p *SESProvider) List(ctx context.Context) ([]string, error) {
	var out []string
	token := ""
	for {
		page, next, err := p.ListPage(ctx, token, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

func (p *SESProvider) ListPage(ctx context.Context, nextToken string, limit int) ([]string, string, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 1000 {
		limit = 1000
	}
	pageSize := int32(limit)
	input := &sesv2.ListEmailIdentitiesInput{PageSize: &pageSize}
	if nextToken != "" {
		input.NextToken = awsString(nextToken)
	}
	resp, err := p.api.ListEmailIdentities(ctx, input)
	if err != nil {
		return nil, "", err
	}
	out := make([]string, 0, len(resp.EmailIdentities))
	for _, id := range resp.EmailIdentities {
		if id.IdentityType == sestypes.IdentityTypeDomain && id.IdentityName != nil {
			out = append(out, *id.IdentityName)
		}
	}
	next := ""
	if resp.NextToken != nil {
		next = *resp.NextToken
	}
	return out, next, nil
}

// mapSESStatus folds SES's verification axes onto our Status. Verified requires
// ALL of: the identity verified for sending, DKIM succeeded (aligned signature),
// AND the custom MAIL FROM succeeded (aligned Return-Path) — all-or-nothing
// (design Q2), so reaching `verified` means there is genuinely no "via e2a". A
// hard failure on either DKIM or MAIL FROM is terminal; anything else is pending.
func mapSESStatus(out *sesv2.GetEmailIdentityOutput) Status {
	dkim := sestypes.DkimStatusNotStarted
	if out.DkimAttributes != nil {
		dkim = out.DkimAttributes.Status
	}
	mf := sestypes.MailFromDomainStatusPending
	if out.MailFromAttributes != nil {
		mf = out.MailFromAttributes.MailFromDomainStatus
	}
	if dkim == sestypes.DkimStatusFailed || mf == sestypes.MailFromDomainStatusFailed {
		return StatusFailed
	}
	if dkim == sestypes.DkimStatusSuccess && out.VerifiedForSendingStatus && mf == sestypes.MailFromDomainStatusSuccess {
		return StatusVerified
	}
	return StatusPending
}

// sesAxisStatuses reads SES's two INDEPENDENT sending axes off the identity and
// maps each onto our Status enum, so each sending DNS record can reflect its OWN
// axis rather than the all-or-nothing rollup. Mirrors mapSESStatus's per-axis
// treatment: SUCCESS → verified, FAILED → failed, and everything else
// (PENDING / NOT_STARTED / TEMPORARY_FAILURE) → pending. The defaults match
// mapSESStatus (missing DKIM attrs read as not-started→pending; missing MAIL
// FROM attrs read as pending). Note: unlike mapSESStatus the DKIM axis here does
// NOT fold in VerifiedForSendingStatus — that gate belongs to the rollup, not to
// the DKIM record's own state.
func sesAxisStatuses(out *sesv2.GetEmailIdentityOutput) (dkim Status, mailFrom Status) {
	dkimRaw := sestypes.DkimStatusNotStarted
	if out.DkimAttributes != nil {
		dkimRaw = out.DkimAttributes.Status
	}
	mfRaw := sestypes.MailFromDomainStatusPending
	if out.MailFromAttributes != nil {
		mfRaw = out.MailFromAttributes.MailFromDomainStatus
	}
	return mapSESDkimStatus(dkimRaw), mapSESMailFromStatus(mfRaw)
}

// mapSESDkimStatus folds a single SES DKIM axis state onto our Status.
func mapSESDkimStatus(s sestypes.DkimStatus) Status {
	switch s {
	case sestypes.DkimStatusSuccess:
		return StatusVerified
	case sestypes.DkimStatusFailed:
		return StatusFailed
	default: // PENDING / NOT_STARTED / TEMPORARY_FAILURE
		return StatusPending
	}
}

// mapSESMailFromStatus folds a single SES custom-MAIL-FROM axis state onto our
// Status.
func mapSESMailFromStatus(s sestypes.MailFromDomainStatus) Status {
	switch s {
	case sestypes.MailFromDomainStatusSuccess:
		return StatusVerified
	case sestypes.MailFromDomainStatusFailed:
		return StatusFailed
	default: // PENDING / TEMPORARY_FAILURE
		return StatusPending
	}
}

// pkcs8Base64 converts a stored PKCS#1 DER RSA private key to the single-line
// base64 PKCS#8 form SES BYODKIM expects.
func pkcs8Base64(pkcs1DER []byte) (string, error) {
	key, err := x509.ParsePKCS1PrivateKey(pkcs1DER)
	if err != nil {
		return "", fmt.Errorf("parse pkcs1: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal pkcs8: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pkcs8), nil
}
