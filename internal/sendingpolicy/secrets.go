package sendingpolicy

import (
	"fmt"
	"os"
)

// ContractLevel is the compiled provider-call closure contract this binary
// advertises. It stays 0 until Task 12's closure guard proves every SES path
// requires authorization; Task 12 is the only change allowed to raise it.
const ContractLevel = 0

// Environment variable names, fixed by the B1a prewire: the ops assemblers
// render exactly these keys, and docker-compose passes them through to the
// server environment.
const (
	EnvFeedbackHMACKeys      = "E2A_SENDING_FEEDBACK_HMAC_KEYS"
	EnvOperatorRecipientsMap = "E2A_SENDING_PROTECTION_OPERATOR_EMAILS"
)

// AllControlsDisabled reports whether every sending-protection control is off.
//
// RampEnabled is deliberately not part of this: the custom-domain ramp predates
// sending protection and self-hosts already run it without the new secrets.
// What the secrets protect -- provider authorization signing and operator
// notices -- only activates with the controls below.
func (p RuntimePolicy) AllControlsDisabled() bool {
	return p.BudgetMode == ModeDisabled &&
		p.DetectorMode == ModeDisabled &&
		p.TenantHeaderMode == TenantHeaderDisabled &&
		p.TenantProvisioningMode == ToggleDisabled &&
		p.TenantSuppressionSyncMode == ToggleDisabled
}

// Secrets bundles the two immutable trust roots a running server holds.
type Secrets struct {
	Keyring    *Keyring
	Recipients *OperatorRecipients
}

// LoadSecretsFromEnv loads and validates both B1a-prewired secrets.
//
// Presence rules follow the plan exactly: a config-source deployment whose
// policy keeps every control disabled may omit them (self-host compatibility
// -- the secrets guard mechanisms that are not running). Database source, or
// any enabled control, requires both; and a value that is PRESENT must always
// be valid regardless of mode, because a malformed secret discovered at
// activation time is far worse than one discovered at boot. Errors are
// redacted by construction -- see keyring.go.
func LoadSecretsFromEnv(source PolicySource, policy RuntimePolicy) (Secrets, error) {
	rawKeys, keysSet := os.LookupEnv(EnvFeedbackHMACKeys)
	rawMap, mapSet := os.LookupEnv(EnvOperatorRecipientsMap)

	required := source == PolicySourceDatabase || !policy.AllControlsDisabled()
	if required {
		if !keysSet {
			return Secrets{}, fmt.Errorf("sendingpolicy: %s is required (policy source %q, controls enabled: %v)",
				EnvFeedbackHMACKeys, source, !policy.AllControlsDisabled())
		}
		if !mapSet {
			return Secrets{}, fmt.Errorf("sendingpolicy: %s is required (policy source %q, controls enabled: %v)",
				EnvOperatorRecipientsMap, source, !policy.AllControlsDisabled())
		}
	}

	var out Secrets
	if keysSet {
		keyring, err := LoadKeyring(rawKeys)
		if err != nil {
			return Secrets{}, err
		}
		out.Keyring = keyring
	}
	if mapSet {
		recipients, err := LoadOperatorRecipients(rawMap)
		if err != nil {
			return Secrets{}, err
		}
		out.Recipients = recipients
	}
	return out, nil
}

// Capabilities is the operator-facing readback printed by
// -print-capabilities and compared across blue/green slots by the ops deploy
// gate. It carries commitments only: no addresses, no key material, no
// customer data.
type Capabilities struct {
	SendingProtectionContract int               `json:"sending_protection_contract"`
	RuntimePolicySource       string            `json:"runtime_policy_source"`
	OperatorCommitments       map[string]string `json:"operator_notice_recipient_commitments"`
}

// BuildCapabilities assembles the readback. A missing operator map yields an
// empty commitments object rather than an error: the self-host disabled mode
// legitimately has none, and the deploy gate treats absence as absence.
func BuildCapabilities(source PolicySource, secrets Secrets) Capabilities {
	commitments := map[string]string{}
	if secrets.Recipients != nil {
		commitments = secrets.Recipients.Commitments()
	}
	return Capabilities{
		SendingProtectionContract: ContractLevel,
		RuntimePolicySource:       string(source),
		OperatorCommitments:       commitments,
	}
}
