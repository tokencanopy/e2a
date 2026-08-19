package senderidentity

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

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
	accountID          string // for the identity ARN TagResource needs (arn:aws:ses:<region>:<accountID>:identity/<domain>)
	deleteConfirmDelay func(attempt int) time.Duration
}

const (
	managedIdentityTagKey   = "e2a-managed"
	managedIdentityTagValue = "sender-identity-v1"
	deleteConfirmAttempts   = 6
)

// NewSESProvider wraps a pre-built SES API (or stub). region feeds the MAIL
// FROM MX record target; accountID feeds the identity ARN adoption's
// TagResource call needs (GetEmailIdentity/ListEmailIdentities never return
// one).
func NewSESProvider(api sesAPI, region, accountID string) *SESProvider {
	return &SESProvider{api: api, region: region, accountID: accountID}
}

// NewSESProviderFromConfig builds a provider from ambient AWS config
// (env/instance role) for the given region. It resolves the caller's AWS
// account ID via STS once at startup (GetCallerIdentity requires no IAM
// grant of its own) because adoption's TagResource call needs a fully
// qualified identity ARN and neither GetEmailIdentity nor ListEmailIdentities
// returns one.
func NewSESProviderFromConfig(ctx context.Context, region string) (*SESProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	callerIdentity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("resolve aws account id: %w", err)
	}
	var accountID string
	if callerIdentity.Account != nil {
		accountID = *callerIdentity.Account
	}
	return &SESProvider{api: sesv2.NewFromConfig(cfg), region: region, accountID: accountID}, nil
}

func (p *SESProvider) Provision(ctx context.Context, domain, dkimSelector string, dkimPrivateKeyDER []byte) (Result, error) {
	privB64, err := pkcs8Base64(dkimPrivateKeyDER)
	if err != nil {
		// A malformed key is not retryable — fail closed with a reason.
		return Result{Status: StatusFailed, Error: "dkim private key not usable for BYODKIM: " + err.Error()}, nil
	}
	dkimAttributes := &ststypes.DkimSigningAttributes{
		DomainSigningSelector:         &dkimSelector,
		DomainSigningPrivateKey:       &privB64,
		DomainSigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
	}
	_, err = p.api.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity:         &domain,
		DkimSigningAttributes: dkimAttributes,
		Tags: []ststypes.Tag{{
			Key:   awsString(managedIdentityTagKey),
			Value: awsString(managedIdentityTagValue),
		}},
	})
	if err != nil {
		// AlreadyExists means the domain may belong to an older registration.
		// Update BYODKIM explicitly before touching MAIL FROM: Create's input is
		// ignored on this path, and keeping the old selector/key would make a
		// re-registered domain fail verification forever.
		var already *ststypes.AlreadyExistsException
		if !errors.As(err, &already) {
			return Result{}, err // transient/permission — retry
		}
		existing, getErr := p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
		if getErr != nil {
			return Result{}, getErr
		}
		if !isManagedIdentity(existing) {
			if !canAdoptIdentity(existing, dkimSelector, len(dkimPrivateKeyDER) > 0) {
				return Result{}, ErrIdentityNotOwned
			}
			if err := p.adoptIdentity(ctx, domain); err != nil {
				return Result{}, err // transient/permission on the tag call — retry
			}
		}
		// Put's nested shape deliberately omits DomainSigningAttributesOrigin.
		// SES rejects every nested Origin value for this operation; EXTERNAL
		// belongs only on the top-level SigningAttributesOrigin field.
		putAttributes := &ststypes.DkimSigningAttributes{
			DomainSigningSelector:   &dkimSelector,
			DomainSigningPrivateKey: &privB64,
		}
		if _, err := p.api.PutEmailIdentityDkimSigningAttributes(ctx, &sesv2.PutEmailIdentityDkimSigningAttributesInput{
			EmailIdentity:           &domain,
			SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
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
		BehaviorOnMxFailure: ststypes.BehaviorOnMxFailureUseDefaultValue,
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

func (p *SESProvider) Status(ctx context.Context, domain, expectedSelector string, haveKeyMaterial bool) (Result, error) {
	out, err := p.api.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: &domain})
	if err != nil {
		var notFound *ststypes.NotFoundException
		if errors.As(err, &notFound) {
			return Result{}, ErrIdentityNotFound
		}
		return Result{}, err
	}
	if !isManagedIdentity(out) {
		// Self-heals an ownership tag removed out-of-band on an identity e2a
		// otherwise still provably owns (matching BYODKIM selector), not just
		// the initial adoption of a pre-tag-release identity.
		if !canAdoptIdentity(out, expectedSelector, haveKeyMaterial) {
			return Result{}, ErrIdentityNotOwned
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
		var notFound *ststypes.NotFoundException
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
		var notFound *ststypes.NotFoundException
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
			var notFound *ststypes.NotFoundException
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
//   - haveKeyMaterial is true: e2a actually has DKIM private key material on
//     file for this domain — NOT merely a stored selector. expectedSelector
//     non-empty is a DIFFERENT fact than key material being present:
//     LoadSendingIdentityState can return a non-empty selector with a
//     nil/empty private key (e.g. mid domain-reclaim — see
//     internal/identity's key lifecycle), and adopting on the selector alone
//     would tag an identity e2a cannot actually sign for. That is not inert:
//     the caller's own no-key branch (worker.go) then finds the identity
//     tagged and calls Deprovision, which now SUCCEEDS and DELETES it —
//     pre-adoption behavior refused that delete outright. Provision passes
//     len(dkimPrivateKeyDER)>0 directly, since it already receives the raw
//     key bytes. Status's own interface carries no key bytes, so ITS caller
//     (worker.go, at both provider.Status call sites) is responsible for
//     passing accurate signal — see Provider.Status's doc.
//   - expectedSelector is non-empty: e2a has a stored DKIM selector on file
//     for this domain.
//   - the identity's DKIM was configured via BYODKIM (SigningAttributesOrigin
//     == EXTERNAL). Only e2a's own Provision supplies signing key material;
//     AWS_SES (Easy DKIM, SES-generated keys) can never be e2a's doing.
//   - DkimAttributes.Status == SUCCESS: SES has independently matched the
//     private key material IT holds against the DNS TXT published at
//     <selector>._domainkey.<domain> — that is what DKIM verification means
//     for a BYODKIM identity, and it costs no extra provider call (Get
//     already returns it). This is materially stronger than the token match
//     below: dkim.SelectorForNow's monthly convention is a publicly-derivable
//     OSS constant with zero entropy of its own, so the token match alone
//     proves only that SOME identity was configured with e2a's naming
//     scheme — not that SES has cryptographically confirmed e2a's key
//     against that domain's DNS. Requiring SUCCESS closes that gap without
//     e2a resolving DNS itself (deliberately out of scope for the provider
//     layer — a DNS-comparing follow-up is a possible future hardening, not
//     required here).
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
//     Easy-DKIM CNAME tokens) matches expectedSelector EXACTLY.
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
func canAdoptIdentity(out *sesv2.GetEmailIdentityOutput, expectedSelector string, haveKeyMaterial bool) bool {
	if out == nil || out.DkimAttributes == nil || expectedSelector == "" || !haveKeyMaterial {
		return false
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
		return false
	}
	if out.DkimAttributes.SigningAttributesOrigin != ststypes.DkimSigningAttributesOriginExternal {
		return false
	}
	// SES has cryptographically matched the key material IT holds against the
	// DNS TXT at <selector>._domainkey.<domain> — see the doc comment above
	// for why this is required strictly, with no PENDING fallback.
	if out.DkimAttributes.Status != ststypes.DkimStatusSuccess {
		return false
	}
	for _, token := range out.DkimAttributes.Tokens {
		if token == expectedSelector {
			return true
		}
	}
	return false
}

// adoptIdentity applies the ownership tag to an identity canAdoptIdentity has
// already cleared. TagResource needs the identity's full ARN — neither
// GetEmailIdentity nor ListEmailIdentities returns one — so it's built from
// the region and the account ID resolved once at construction time.
func (p *SESProvider) adoptIdentity(ctx context.Context, domain string) error {
	arn := p.identityARN(domain)
	_, err := p.api.TagResource(ctx, &sesv2.TagResourceInput{
		ResourceArn: &arn,
		Tags: []ststypes.Tag{{
			Key:   awsString(managedIdentityTagKey),
			Value: awsString(managedIdentityTagValue),
		}},
	})
	return err
}

// identityARN builds the SES v2 email-identity ARN for domain:
// arn:aws:ses:<region>:<account-id>:identity/<domain>.
func (p *SESProvider) identityARN(domain string) string {
	return fmt.Sprintf("arn:aws:ses:%s:%s:identity/%s", p.region, p.accountID, domain)
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
		if id.IdentityType == ststypes.IdentityTypeDomain && id.IdentityName != nil {
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
	dkim := ststypes.DkimStatusNotStarted
	if out.DkimAttributes != nil {
		dkim = out.DkimAttributes.Status
	}
	mf := ststypes.MailFromDomainStatusPending
	if out.MailFromAttributes != nil {
		mf = out.MailFromAttributes.MailFromDomainStatus
	}
	if dkim == ststypes.DkimStatusFailed || mf == ststypes.MailFromDomainStatusFailed {
		return StatusFailed
	}
	if dkim == ststypes.DkimStatusSuccess && out.VerifiedForSendingStatus && mf == ststypes.MailFromDomainStatusSuccess {
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
	dkimRaw := ststypes.DkimStatusNotStarted
	if out.DkimAttributes != nil {
		dkimRaw = out.DkimAttributes.Status
	}
	mfRaw := ststypes.MailFromDomainStatusPending
	if out.MailFromAttributes != nil {
		mfRaw = out.MailFromAttributes.MailFromDomainStatus
	}
	return mapSESDkimStatus(dkimRaw), mapSESMailFromStatus(mfRaw)
}

// mapSESDkimStatus folds a single SES DKIM axis state onto our Status.
func mapSESDkimStatus(s ststypes.DkimStatus) Status {
	switch s {
	case ststypes.DkimStatusSuccess:
		return StatusVerified
	case ststypes.DkimStatusFailed:
		return StatusFailed
	default: // PENDING / NOT_STARTED / TEMPORARY_FAILURE
		return StatusPending
	}
}

// mapSESMailFromStatus folds a single SES custom-MAIL-FROM axis state onto our
// Status.
func mapSESMailFromStatus(s ststypes.MailFromDomainStatus) Status {
	switch s {
	case ststypes.MailFromDomainStatusSuccess:
		return StatusVerified
	case ststypes.MailFromDomainStatusFailed:
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
