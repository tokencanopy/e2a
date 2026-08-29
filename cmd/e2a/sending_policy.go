package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// sendingProtectionFlags carries the local operator commands added by the
// sending-protection slice (Task 2). Exactly one command flag may be set per
// invocation; each runs after migrations and exits without starting the
// server, following the -bootstrap-email pattern.
type sendingProtectionFlags struct {
	inspect      bool
	activate     bool
	register     bool
	attest       bool
	capabilities bool

	expectedGeneration int64
	expectedPolicySHA  string
	grandfather        bool

	expectedAttestationRevision int64
	expectedAttestationSHA      string
	activeBillingDigest         string
	activeBillingContract       int
	rollbackBillingDigest       string
	rollbackBillingContract     int

	reason string
}

func (f *sendingProtectionFlags) commandRequested() bool {
	return f.inspect || f.activate || f.register || f.attest || f.capabilities
}

func (f *sendingProtectionFlags) selectedCount() int {
	n := 0
	for _, set := range []bool{f.inspect, f.activate, f.register, f.attest, f.capabilities} {
		if set {
			n++
		}
	}
	return n
}

// cliActor identifies the invoking operator in audit rows. Best-effort OS
// identity; the nonblank -reason carries the human context.
func cliActor() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return "cli:" + u.Username
	}
	return "cli:local-operator"
}

// runSendingProtectionCommand executes the selected command and returns once
// it is done; the caller exits. Every mutation is CAS-guarded in the store —
// this layer's job is explicit arguments, the reviewed-hash equality check,
// and printing nothing secret.
func runSendingProtectionCommand(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, f *sendingProtectionFlags, stdout io.Writer) error {
	if f.selectedCount() != 1 {
		return errors.New("exactly one sending-protection command may be given per invocation")
	}

	source, err := sendingpolicy.SourceFromConfig(cfg)
	if err != nil {
		return err
	}
	policy, err := sendingpolicy.FromConfig(cfg)
	if err != nil {
		return err
	}
	module := sendingpolicy.NewModule(pool)

	switch {
	case f.inspect:
		return runPolicyInspect(ctx, module, source, policy, stdout)
	case f.activate:
		return runPolicyActivate(ctx, module, policy, f, stdout)
	case f.register:
		return runOperatorRegister(ctx, module, f, stdout)
	case f.attest:
		return runRuntimeAttest(ctx, module, f, stdout)
	case f.capabilities:
		return runPrintCapabilities(source, policy, stdout)
	}
	return errors.New("no sending-protection command selected")
}

// runPolicyInspect is the dry-run/readback: the stored state an operator needs
// for the next CAS, and the hash of the policy this config would activate. The
// policy payload contains no secrets, so printing its canonical form is the
// review surface.
func runPolicyInspect(ctx context.Context, module *sendingpolicy.Module, source sendingpolicy.PolicySource, policy sendingpolicy.RuntimePolicy, stdout io.Writer) error {
	stored, err := module.InspectPolicy(ctx)
	if err != nil {
		return err
	}
	configHash, err := sendingpolicy.Hash(policy)
	if err != nil {
		return err
	}
	canonical, err := sendingpolicy.CanonicalBytes(policy)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "runtime_policy_source:    %s\n", source)
	fmt.Fprintf(stdout, "stored_generation:        %d\n", stored.Generation)
	fmt.Fprintf(stdout, "stored_policy_sha256:     %s\n", stored.PolicySHA256)
	fmt.Fprintf(stdout, "stored_activated_at:      %s\n", stored.ActivatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(stdout, "stored_activated_by:      %s\n", stored.ActivatedBy)
	fmt.Fprintf(stdout, "config_policy_sha256:     %s\n", configHash)
	if configHash == stored.PolicySHA256 {
		fmt.Fprintf(stdout, "status:                   config matches the stored generation\n")
	} else {
		fmt.Fprintf(stdout, "status:                   config DIFFERS from the stored generation; activation would advance to %d\n", stored.Generation+1)
	}
	fmt.Fprintf(stdout, "config_policy_canonical:  %s\n", canonical)
	return nil
}

// runPolicyActivate performs one reviewed CAS. There is no separately mounted
// policy file: the payload is built from the same validated config the server
// would use, and mutation requires the operator to present the exact hash they
// reviewed. Any mismatch is zero writes.
func runPolicyActivate(ctx context.Context, module *sendingpolicy.Module, policy sendingpolicy.RuntimePolicy, f *sendingProtectionFlags, stdout io.Writer) error {
	if strings.TrimSpace(f.reason) == "" {
		return errors.New("-activate-sending-protection-policy requires a nonblank -reason")
	}
	if f.expectedGeneration < 0 {
		return errors.New("-activate-sending-protection-policy requires -expected-generation")
	}
	if f.expectedPolicySHA == "" {
		return errors.New("-activate-sending-protection-policy requires -expected-policy-sha256")
	}

	configHash, err := sendingpolicy.Hash(policy)
	if err != nil {
		return err
	}
	if !strings.EqualFold(f.expectedPolicySHA, configHash) {
		return fmt.Errorf("reviewed hash %s does not match this config's policy (%s); zero writes performed",
			f.expectedPolicySHA, configHash)
	}

	snapshot, err := module.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration:               f.expectedGeneration,
		Policy:                           policy,
		Actor:                            cliActor(),
		Reason:                           f.reason,
		GrandfatherCurrentSendingDomains: f.grandfather,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "activated_generation:  %d\n", snapshot.Generation)
	fmt.Fprintf(stdout, "policy_sha256:         %s\n", snapshot.PolicySHA256)
	if f.grandfather {
		fmt.Fprintf(stdout, "grandfathering:        completed in the same transaction\n")
	}
	return nil
}

// runOperatorRegister reads the local operator map and inserts only absent
// registry rows. The map itself is never printed; the registry stores only
// commitments.
func runOperatorRegister(ctx context.Context, module *sendingpolicy.Module, f *sendingProtectionFlags, stdout io.Writer) error {
	if strings.TrimSpace(f.reason) == "" {
		return errors.New("-register-sending-protection-operator-recipients requires a nonblank -reason")
	}
	raw, ok := os.LookupEnv(sendingpolicy.EnvOperatorRecipientsMap)
	if !ok {
		return fmt.Errorf("%s is not set; registration reads the local versioned secret map", sendingpolicy.EnvOperatorRecipientsMap)
	}
	recipients, err := sendingpolicy.LoadOperatorRecipients(raw)
	if err != nil {
		return err
	}

	inserted, err := module.RegisterOperatorRecipients(ctx, recipients, cliActor())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "commitment_key_id:   %s\n", recipients.KeyID())
	fmt.Fprintf(stdout, "versions_configured: %v\n", recipients.Versions())
	fmt.Fprintf(stdout, "versions_inserted:   %v\n", inserted)
	if len(inserted) == 0 {
		fmt.Fprintf(stdout, "status:              idempotent replay; registry already matches the local map\n")
	}
	return nil
}

// runRuntimeAttest performs the CAS-only billing attestation write. Contract
// levels must be explicit and non-negative; digests may legitimately be empty
// (the migration-seeded revision-0 state, and its abort fence, carry empty
// digests).
func runRuntimeAttest(ctx context.Context, module *sendingpolicy.Module, f *sendingProtectionFlags, stdout io.Writer) error {
	if strings.TrimSpace(f.reason) == "" {
		return errors.New("-attest-sending-protection-runtime requires a nonblank -reason")
	}
	if f.expectedAttestationRevision < 0 {
		return errors.New("-attest-sending-protection-runtime requires -expected-attestation-revision")
	}
	if f.expectedAttestationSHA == "" {
		return errors.New("-attest-sending-protection-runtime requires -expected-attestation-sha256")
	}
	if f.activeBillingContract < 0 || f.rollbackBillingContract < 0 {
		return errors.New("-attest-sending-protection-runtime requires -active-billing-contract and -rollback-billing-contract")
	}

	attestation, err := module.AttestRuntime(ctx, sendingpolicy.RuntimeAttestationRequest{
		ExpectedRevision:        f.expectedAttestationRevision,
		ExpectedSHA256:          strings.ToLower(f.expectedAttestationSHA),
		ActiveBillingDigest:     f.activeBillingDigest,
		ActiveBillingContract:   f.activeBillingContract,
		RollbackBillingDigest:   f.rollbackBillingDigest,
		RollbackBillingContract: f.rollbackBillingContract,
		Actor:                   cliActor(),
		Reason:                  f.reason,
	})
	if err != nil {
		return err
	}
	newHash, err := sendingpolicy.AttestationHash(attestation)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "attestation_revision: %d\n", attestation.Revision)
	fmt.Fprintf(stdout, "attestation_sha256:   %s\n", newHash)
	return nil
}

// runPrintCapabilities emits the machine-readable capability marker the ops
// deploy gate compares across slots. Commitments only — never addresses, key
// material, or customer data.
func runPrintCapabilities(source sendingpolicy.PolicySource, policy sendingpolicy.RuntimePolicy, stdout io.Writer) error {
	secrets, err := sendingpolicy.LoadSecretsFromEnv(source, policy)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(sendingpolicy.BuildCapabilities(source, secrets))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", payload)
	return nil
}
