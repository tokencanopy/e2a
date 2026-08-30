package senderidentity

import (
	"time"

	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Classification tags stamped on every sending identity e2a CREATES. They
// exist so "is this identity safe to delete?" is answerable from the provider
// account alone, without a correct join against this server's Postgres ledger
// — the join that a reaper cannot make when it is pointed at an AWS account
// whose database is gone, restored from a stale backup, or simply not the one
// that provisioned the identity.
//
// The keys here and the value vocabularies below are HARDCODED, not
// configurable, precisely because the writer (this package's provisioner) and
// the future reader (the reaper that will be given delete authority on the
// strength of these tags) must be unable to drift apart. Every VALUE is
// derived at runtime.
//
// Tagging is strictly best-effort and fails OPEN: a value that cannot be
// derived omits its one tag and never fails the provision. That asymmetry is
// deliberate and only safe because the eventual deletion decision fails
// CLOSED — an identity missing a tag is one the reaper must leave alone, so
// the worst case of a dropped tag is an identity a human has to classify, not
// one that gets deleted by mistake.
const (
	// managedIdentityTagKey/Value live in ses.go: they are the pre-existing
	// OWNERSHIP anchor isMangedIdentity reads, load-bearing for adoption and
	// for the IAM resource-tag condition, and are deliberately untouched here.

	// envTagKey names the deployment that created the identity ("prod" |
	// "staging"), so a shared AWS account can tell one deployment's
	// identities from another's. Omitted when the deployment is unnamed —
	// the self-host default.
	envTagKey = "e2a-env"
	// purposeTagKey classifies the OWNER, not the domain: purposeCustomer for
	// a real account, purposeFixture for e2a's own internal/monitoring
	// accounts. Only fixtures are ever candidates for automatic cleanup.
	purposeTagKey = "e2a-purpose"
	// createdTagKey is the provider-side creation stamp (RFC3339, UTC). SES
	// does not report a creation time for an identity, so without this tag
	// age is unknowable from AWS alone.
	createdTagKey = "e2a-created"
	// expiresTagKey is created + the fixture TTL, written ONLY for
	// purposeFixture. A customer identity carries no expiry at all: there is
	// no time after which deleting a customer's sending identity is correct.
	expiresTagKey = "e2a-expires"
	// userTagKey is the owning user id, so an identity can be traced back to
	// an account without the ledger.
	userTagKey = "e2a-user"
	// provisionerTagKey is the build/release that created the identity —
	// which code wrote the rest of these tags.
	provisionerTagKey = "e2a-provisioner"
)

// Deployment names recognized for the e2a-env tag. This is the closed
// vocabulary; internal/config mirrors these two literals to normalize operator
// input, but this package is the authority and re-screens whatever it is
// given, so a name that reaches here unrecognized drops the tag rather than
// writing an unknown value the reaper would then have to interpret.
const (
	DeploymentProd    = "prod"
	DeploymentStaging = "staging"
)

// Purpose values for the e2a-purpose tag. Closed vocabulary, derived from the
// owner's usage account class:
//
//   - "standard" / "demo"    → purposeCustomer. A demo account is user-facing
//     and its domain is a real customer domain in every way that matters for
//     deletion, so it is deliberately NOT a fixture.
//   - "internal" / "system"  → purposeFixture. Internal dogfooding and
//     synthetic-monitoring probes; these are the identities a cleanup pass
//     exists for.
//
// Anything else — including the empty string a failed lookup yields — is left
// UNCLASSIFIED rather than guessed. purposeCustomer is not a safe default to
// assume (it would hide abandoned fixtures forever) and purposeFixture
// certainly is not (it would eventually mark a customer's identity
// deletable), so the honest answer is no tag.
const (
	purposeCustomer = "customer"
	purposeFixture  = "fixture"
)

// maxTagValueLen is the SES tag-value length limit.
const maxTagValueLen = 256

// ProvisionMeta is the ledger-side context a Provider may stamp onto an
// identity it creates. Every field is optional: an absent value drops the tag
// it would have fed and nothing else. The caller resolves these best-effort
// (see the worker's provisionMeta), so a store lookup that fails yields a
// partial ProvisionMeta rather than an error.
type ProvisionMeta struct {
	// UserID is the domain owner's user id (SendingIdentityState.Owner).
	UserID string
	// AccountClass is the owner's usage account class ("standard",
	// "internal", "system", "demo"). Empty means "not known" — not
	// "standard": see the purpose vocabulary above for why the difference
	// matters.
	AccountClass string
}

// WithIdentityTags configures the runtime-derived classification tags this
// provider stamps on identities it creates. Every argument is optional:
// deploymentName "" (or an unrecognized name) omits the env tag,
// provisionerBuild "" omits the provisioner tag, and fixtureTTL <= 0 omits the
// expiry tag. An SESProvider that never gets this call still writes the
// ownership anchor and the creation stamp, so the zero value is a working
// provider, not a broken one.
func (p *SESProvider) WithIdentityTags(deploymentName, provisionerBuild string, fixtureTTL time.Duration) *SESProvider {
	p.deploymentName = deploymentName
	p.provisionerBuild = provisionerBuild
	p.fixtureTTL = fixtureTTL
	return p
}

// clock is the provider's time source, injectable so the created/expires
// stamps are assertable in tests.
func (p *SESProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// identityTags builds the tag list for a newly created identity. It never
// returns an error and never returns an empty list: the ownership tag is
// unconditional, and every classification tag is appended only when its value
// is both derivable and something SES will accept.
func (p *SESProvider) identityTags(meta ProvisionMeta) []sestypes.Tag {
	tags := []sestypes.Tag{{
		Key:   awsString(managedIdentityTagKey),
		Value: awsString(managedIdentityTagValue),
	}}
	add := func(key, value string) {
		if !tagValueAccepted(value) {
			return
		}
		tags = append(tags, sestypes.Tag{Key: awsString(key), Value: awsString(value)})
	}

	switch p.deploymentName {
	case DeploymentProd, DeploymentStaging:
		add(envTagKey, p.deploymentName)
	}

	purpose := purposeForAccountClass(meta.AccountClass)
	add(purposeTagKey, purpose)

	created := p.clock().UTC()
	add(createdTagKey, created.Format(time.RFC3339))
	if purpose == purposeFixture && p.fixtureTTL > 0 {
		add(expiresTagKey, created.Add(p.fixtureTTL).Format(time.RFC3339))
	}

	add(userTagKey, meta.UserID)
	add(provisionerTagKey, p.provisionerBuild)
	return tags
}

// purposeForAccountClass maps a usage account class onto the closed purpose
// vocabulary, returning "" for anything it does not recognize (see the
// vocabulary's doc comment). The class strings are usage.AccountClass values
// spelled literally so this package does not import internal/usage — the
// workers already speak plain strings for exactly this reason (see RawStore).
func purposeForAccountClass(class string) string {
	switch class {
	case "standard", "demo":
		return purposeCustomer
	case "internal", "system":
		return purposeFixture
	default:
		return ""
	}
}

// tagValueAccepted screens a value against the SES tag-value rule (letters,
// digits, spaces, and + - = . _ : / @, up to 256 characters). It exists
// because SES rejects the WHOLE CreateEmailIdentity call on one bad tag: the
// provisioner build string is operator-supplied config that e2a does not
// control, and without this screen a stray character in it would turn
// cosmetic metadata into a provisioning outage for every custom domain. An
// empty value is also refused, which is what makes "omit the tag" the uniform
// behavior for every underivable value.
func tagValueAccepted(value string) bool {
	if value == "" || len(value) > maxTagValueLen {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '+', r == '-', r == '=', r == '.', r == '_', r == ':', r == '/', r == '@':
		default:
			return false
		}
	}
	return true
}
