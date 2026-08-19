package senderidentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestMapSESStatus(t *testing.T) {
	tests := []struct {
		name string
		out  *sesv2.GetEmailIdentityOutput
		want Status
	}{
		{
			name: "all three (sending + dkim + mailfrom) success → verified",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
			},
			want: StatusVerified,
		},
		{
			name: "dkim success + verified for sending but mailfrom pending → pending (all-or-nothing)",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusPending},
			},
			want: StatusPending,
		},
		{
			name: "dkim+mailfrom success but not verified for sending → pending",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: false,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
			},
			want: StatusPending,
		},
		{
			name: "mailfrom failed → failed (even with dkim ok)",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusFailed},
			},
			want: StatusFailed,
		},
		{
			name: "dkim temporary_failure → pending (transient, not stranded as failed)",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusTemporaryFailure},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
			},
			want: StatusPending,
		},
		{
			name: "mailfrom temporary_failure → pending (transient, not stranded)",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusTemporaryFailure},
			},
			want: StatusPending,
		},
		{
			name: "dkim failed → failed",
			out: &sesv2.GetEmailIdentityOutput{
				VerifiedForSendingStatus: true,
				DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusFailed},
			},
			want: StatusFailed,
		},
		{
			name: "dkim pending → pending",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{Status: ststypes.DkimStatusPending},
			},
			want: StatusPending,
		},
		{
			name: "nil dkim attributes → pending",
			out:  &sesv2.GetEmailIdentityOutput{},
			want: StatusPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapSESStatus(tt.out); got != tt.want {
				t.Fatalf("mapSESStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSESAxisStatuses verifies the per-axis extraction: each sending axis (DKIM,
// custom MAIL FROM) maps onto our Status independently, mirroring mapSESStatus's
// per-axis vocabulary (SUCCESS->verified, FAILED->failed, everything else->pending).
func TestSESAxisStatuses(t *testing.T) {
	tests := []struct {
		name         string
		out          *sesv2.GetEmailIdentityOutput
		wantDkim     Status
		wantMailFrom Status
	}{
		{
			name: "both success",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes:     &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes: &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
			},
			wantDkim: StatusVerified, wantMailFrom: StatusVerified,
		},
		{
			// The key mixed case this fix exists for: DKIM is good but the custom
			// MAIL FROM is broken. The axes must disagree.
			name: "dkim success + mailfrom failed (mixed)",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes:     &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
				MailFromAttributes: &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusFailed},
			},
			wantDkim: StatusVerified, wantMailFrom: StatusFailed,
		},
		{
			name: "dkim failed + mailfrom success (reverse mixed)",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes:     &ststypes.DkimAttributes{Status: ststypes.DkimStatusFailed},
				MailFromAttributes: &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
			},
			wantDkim: StatusFailed, wantMailFrom: StatusVerified,
		},
		{
			name: "transient axes -> pending, not failed",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes:     &ststypes.DkimAttributes{Status: ststypes.DkimStatusTemporaryFailure},
				MailFromAttributes: &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusTemporaryFailure},
			},
			wantDkim: StatusPending, wantMailFrom: StatusPending,
		},
		{
			name:     "nil attributes default to pending",
			out:      &sesv2.GetEmailIdentityOutput{},
			wantDkim: StatusPending, wantMailFrom: StatusPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDkim, gotMailFrom := sesAxisStatuses(tt.out)
			if gotDkim != tt.wantDkim || gotMailFrom != tt.wantMailFrom {
				t.Fatalf("sesAxisStatuses = (%q, %q), want (%q, %q)", gotDkim, gotMailFrom, tt.wantDkim, tt.wantMailFrom)
			}
		})
	}
}

// TestSESProvider_StatusReportsAxes proves Status() surfaces BOTH the rollup and
// the per-axis breakdown: in the mixed DKIM-ok/MAILFROM-failed case the rollup
// stays all-or-nothing (failed) while the axes disagree, which is exactly what
// lets the API show only the broken record.
func TestSESProvider_StatusReportsAxes(t *testing.T) {
	stub := &stubSESAPI{getOut: &sesv2.GetEmailIdentityOutput{
		VerifiedForSendingStatus: true,
		DkimAttributes:           &ststypes.DkimAttributes{Status: ststypes.DkimStatusSuccess},
		MailFromAttributes:       &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusFailed},
	}}
	p := NewSESProvider(stub, "us-east-1", testAccountID)
	res, err := p.Status(context.Background(), "acme.com", "", false)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("rollup Status must stay all-or-nothing failed, got %q", res.Status)
	}
	if res.DkimStatus != StatusVerified {
		t.Fatalf("dkim axis should be verified, got %q", res.DkimStatus)
	}
	if res.MailFromStatus != StatusFailed {
		t.Fatalf("mail from axis should be failed, got %q", res.MailFromStatus)
	}
}

func TestPKCS8Base64(t *testing.T) {
	t.Run("valid pkcs1 der round-trips to parseable pkcs8", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		pkcs1 := x509.MarshalPKCS1PrivateKey(key)

		got, err := pkcs8Base64(pkcs1)
		if err != nil {
			t.Fatalf("pkcs8Base64 returned error: %v", err)
		}
		if got == "" {
			t.Fatalf("expected non-empty base64 output")
		}
		der, err := base64.StdEncoding.DecodeString(got)
		if err != nil {
			t.Fatalf("output not valid base64: %v", err)
		}
		if _, err := x509.ParsePKCS8PrivateKey(der); err != nil {
			t.Fatalf("decoded bytes are not valid PKCS#8: %v", err)
		}
	})

	t.Run("garbage bytes return an error", func(t *testing.T) {
		if _, err := pkcs8Base64([]byte("not a real der key")); err == nil {
			t.Fatalf("expected error for garbage input, got nil")
		}
	})
}

// testAccountID is a synthetic AWS account id used only to exercise ARN
// construction; it is not a real account.
const testAccountID = "123456789012"

// TestAccountIDFromCallerIdentity pins finding 4's "empty account is an
// error" fix: a nil Account must never silently produce an account-id-less
// identity ARN (arn:aws:ses:<region>::identity/<domain>).
func TestAccountIDFromCallerIdentity(t *testing.T) {
	t.Run("nil output", func(t *testing.T) {
		if _, err := accountIDFromCallerIdentity(nil); err == nil {
			t.Fatal("want an error for a nil GetCallerIdentityOutput")
		}
	})
	t.Run("nil Account", func(t *testing.T) {
		if _, err := accountIDFromCallerIdentity(&sts.GetCallerIdentityOutput{}); err == nil {
			t.Fatal("want an error for a nil Account, not a silently empty account id")
		}
	})
	t.Run("populated Account round-trips", func(t *testing.T) {
		got, err := accountIDFromCallerIdentity(&sts.GetCallerIdentityOutput{Account: awsString(testAccountID)})
		if err != nil || got != testAccountID {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, testAccountID)
		}
	})
}

// TestSESProvider_AdoptionDegradesWhenAccountIDUnavailable pins finding 4's
// lazy+non-fatal fix directly: a provider whose account id resolution fails
// (simulating an STS blip) must degrade adoption to ErrIdentityNotOwned
// rather than propagate the raw error or build a malformed ARN — and must
// not re-invoke the resolver on every call (sync.Once — a persistent outage
// doesn't hammer STS on every adoption attempt).
func TestSESProvider_AdoptionDegradesWhenAccountIDUnavailable(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	boom := errors.New("sts: GetCallerIdentity blip")
	var resolveCalls int
	stub := &stubSESAPI{
		createErr: &ststypes.AlreadyExistsException{},
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := &SESProvider{
		api:    stub,
		region: "us-east-1",
		resolveAccountID: func(context.Context) (string, error) {
			resolveCalls++
			return "", boom
		},
	}

	if _, err := p.Provision(context.Background(), "legacy.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Provision error = %v, want degraded ErrIdentityNotOwned when the account id is unavailable", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("must not attempt TagResource without a resolvable account id: %+v", stub.tagInputs)
	}
	if stub.dkimInput != nil || stub.mailFromInput != nil {
		t.Fatalf("must not proceed to mutate before adoption is confirmed: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}

	// A second adoption attempt must not re-invoke the resolver.
	if _, err := p.Provision(context.Background(), "legacy.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("second Provision error = %v, want ErrIdentityNotOwned again", err)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolveAccountID called %d times, want exactly 1 (sync.Once)", resolveCalls)
	}
}

// TestSESProvider_NewSESProviderNeverResolvesAccountID proves the explicit-
// accountID constructor path never touches resolveAccountID at all — it is
// simply unset, so accountIDForAdoption returns the supplied id immediately.
func TestSESProvider_NewSESProviderNeverResolvesAccountID(t *testing.T) {
	p := NewSESProvider(&stubSESAPI{}, "us-east-1", testAccountID)
	got, err := p.accountIDForAdoption(context.Background())
	if err != nil || got != testAccountID {
		t.Fatalf("accountIDForAdoption = (%q, %v), want (%q, nil)", got, err, testAccountID)
	}
}

// stubSESAPI implements sesAPI; only the methods under test return real
// behavior, the rest panic if unexpectedly called.
type stubSESAPI struct {
	getErr          error
	delErr          error
	createErr       error
	putDkimErr      error
	putErr          error
	tagErr          error
	unmanaged       bool
	deleted         bool
	deleteLags      bool
	deleteLagReads  int
	deleteRequested bool

	// getOut, when set, is what GetEmailIdentity returns (so Status tests can
	// drive the SES axes). Nil ⇒ an empty output.
	getOut *sesv2.GetEmailIdentityOutput

	// recorders for the Provision path.
	createInput   *sesv2.CreateEmailIdentityInput
	dkimInput     *sesv2.PutEmailIdentityDkimSigningAttributesInput
	mailFromInput *sesv2.PutEmailIdentityMailFromAttributesInput
	listOut       *sesv2.ListEmailIdentitiesOutput
	listErr       error
	listInput     *sesv2.ListEmailIdentitiesInput

	// tagInputs records every TagResource call (adoption path).
	tagInputs []*sesv2.TagResourceInput
}

func (s *stubSESAPI) CreateEmailIdentity(ctx context.Context, in *sesv2.CreateEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error) {
	s.createInput = in
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &sesv2.CreateEmailIdentityOutput{}, nil
}

func (s *stubSESAPI) PutEmailIdentityDkimSigningAttributes(ctx context.Context, in *sesv2.PutEmailIdentityDkimSigningAttributesInput, optFns ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityDkimSigningAttributesOutput, error) {
	s.dkimInput = in
	if s.putDkimErr != nil {
		return nil, s.putDkimErr
	}
	return &sesv2.PutEmailIdentityDkimSigningAttributesOutput{}, nil
}

func (s *stubSESAPI) PutEmailIdentityMailFromAttributes(ctx context.Context, in *sesv2.PutEmailIdentityMailFromAttributesInput, optFns ...func(*sesv2.Options)) (*sesv2.PutEmailIdentityMailFromAttributesOutput, error) {
	s.mailFromInput = in
	if s.putErr != nil {
		return nil, s.putErr
	}
	return &sesv2.PutEmailIdentityMailFromAttributesOutput{}, nil
}

func (s *stubSESAPI) GetEmailIdentity(ctx context.Context, in *sesv2.GetEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error) {
	if s.deleteRequested {
		if s.deleteLagReads > 0 {
			s.deleteLagReads--
		} else {
			s.deleted = true
		}
	}
	if s.deleted {
		return nil, &ststypes.NotFoundException{}
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	out := s.getOut
	if out == nil {
		out = &sesv2.GetEmailIdentityOutput{}
	}
	copyOut := *out
	if !s.unmanaged {
		copyOut.Tags = append([]ststypes.Tag(nil), out.Tags...)
		copyOut.Tags = append(copyOut.Tags, ststypes.Tag{Key: awsString(managedIdentityTagKey), Value: awsString(managedIdentityTagValue)})
	}
	return &copyOut, nil
}

func (s *stubSESAPI) DeleteEmailIdentity(ctx context.Context, in *sesv2.DeleteEmailIdentityInput, optFns ...func(*sesv2.Options)) (*sesv2.DeleteEmailIdentityOutput, error) {
	if s.delErr != nil {
		return nil, s.delErr
	}
	if !s.deleteLags {
		if s.deleteLagReads > 0 {
			s.deleteRequested = true
		} else {
			s.deleted = true
		}
	}
	return &sesv2.DeleteEmailIdentityOutput{}, nil
}

func (s *stubSESAPI) ListEmailIdentities(ctx context.Context, in *sesv2.ListEmailIdentitiesInput, optFns ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error) {
	s.listInput = in
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listOut == nil {
		return &sesv2.ListEmailIdentitiesOutput{}, nil
	}
	return s.listOut, nil
}

func (s *stubSESAPI) TagResource(ctx context.Context, in *sesv2.TagResourceInput, optFns ...func(*sesv2.Options)) (*sesv2.TagResourceOutput, error) {
	s.tagInputs = append(s.tagInputs, in)
	if s.tagErr != nil {
		return nil, s.tagErr
	}
	// Adoption: once tagged, subsequent GetEmailIdentity calls report the
	// identity as managed, mirroring real SES.
	s.unmanaged = false
	return &sesv2.TagResourceOutput{}, nil
}

func TestSESProvider_ListPageIsProviderBounded(t *testing.T) {
	stub := &stubSESAPI{listOut: &sesv2.ListEmailIdentitiesOutput{
		EmailIdentities: []ststypes.IdentityInfo{
			{IdentityType: ststypes.IdentityTypeDomain, IdentityName: awsString("managed.example.test")},
			{IdentityType: ststypes.IdentityTypeEmailAddress, IdentityName: awsString("ignored@example.test")},
		},
		NextToken: awsString("next-page-token"),
	}}
	p := NewSESProvider(stub, "us-east-2", testAccountID)

	domains, next, err := p.ListPage(context.Background(), "current-page-token", 25)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(domains) != 1 || domains[0] != "managed.example.test" || next != "next-page-token" {
		t.Fatalf("ListPage = %v, %q", domains, next)
	}
	if stub.listInput == nil || stub.listInput.PageSize == nil || *stub.listInput.PageSize != 25 || stub.listInput.NextToken == nil || *stub.listInput.NextToken != "current-page-token" {
		t.Fatalf("provider request was not bounded/tokened: %+v", stub.listInput)
	}
}

func TestSESProvider_ProvisionConfiguresMailFrom(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	stub := &stubSESAPI{}
	p := NewSESProvider(stub, "eu-west-1", testAccountID)

	res, err := p.Provision(context.Background(), "acme.com", "e2a202606", pkcs1)
	if err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if res.Status != StatusPending {
		t.Fatalf("want pending after provision, got %q", res.Status)
	}
	if stub.createInput == nil || len(stub.createInput.Tags) != 1 ||
		stub.createInput.Tags[0].Key == nil || *stub.createInput.Tags[0].Key != managedIdentityTagKey ||
		stub.createInput.Tags[0].Value == nil || *stub.createInput.Tags[0].Value != managedIdentityTagValue {
		t.Fatalf("created identity is missing ownership tag: %+v", stub.createInput)
	}
	// Configured the custom MAIL FROM on the identity.
	if stub.mailFromInput == nil || stub.mailFromInput.MailFromDomain == nil ||
		*stub.mailFromInput.MailFromDomain != "bounce.acme.com" {
		t.Fatalf("want MAIL FROM bounce.acme.com, got %+v", stub.mailFromInput)
	}
	if stub.mailFromInput.BehaviorOnMxFailure != ststypes.BehaviorOnMxFailureUseDefaultValue {
		t.Errorf("want USE_DEFAULT_VALUE behavior, got %v", stub.mailFromInput.BehaviorOnMxFailure)
	}
	// Returned the MX + SPF records (region-targeted) for the customer to publish.
	if len(res.DNSRecords) != 2 {
		t.Fatalf("want 2 DNS records, got %d: %+v", len(res.DNSRecords), res.DNSRecords)
	}
	var mx, txt *DNSRecord
	for i := range res.DNSRecords {
		switch res.DNSRecords[i].Type {
		case "MX":
			mx = &res.DNSRecords[i]
		case "TXT":
			txt = &res.DNSRecords[i]
		}
	}
	if mx == nil || mx.Name != "bounce.acme.com" || mx.Value != "10 feedback-smtp.eu-west-1.amazonses.com" {
		t.Errorf("MX record wrong: %+v", mx)
	}
	if txt == nil || txt.Name != "bounce.acme.com" || txt.Value != "v=spf1 include:amazonses.com ~all" {
		t.Errorf("SPF TXT record wrong: %+v", txt)
	}
}

func TestSESProvider_ProvisionAlreadyExistsStillSetsMailFrom(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	// CreateEmailIdentity returns AlreadyExists (idempotent re-provision); MAIL
	// FROM must still be (re)configured.
	stub := &stubSESAPI{createErr: &ststypes.AlreadyExistsException{}}
	p := NewSESProvider(stub, "us-east-1", testAccountID)
	res, err := p.Provision(context.Background(), "acme.com", "sel", pkcs1)
	if err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if res.Status != StatusPending || stub.mailFromInput == nil || stub.dkimInput == nil {
		t.Fatalf("AlreadyExists must replace BYODKIM and set MAIL FROM; status=%q dkim=%+v mailFrom=%+v", res.Status, stub.dkimInput, stub.mailFromInput)
	}
	if stub.dkimInput.SigningAttributesOrigin != ststypes.DkimSigningAttributesOriginExternal ||
		stub.dkimInput.SigningAttributes == nil ||
		stub.dkimInput.SigningAttributes.DomainSigningSelector == nil ||
		*stub.dkimInput.SigningAttributes.DomainSigningSelector != "sel" {
		t.Fatalf("AlreadyExists did not install the replacement BYODKIM selector: %+v", stub.dkimInput)
	}
	wantKey, err := pkcs8Base64(pkcs1)
	if err != nil {
		t.Fatalf("pkcs8Base64: %v", err)
	}
	if stub.dkimInput.SigningAttributes.DomainSigningPrivateKey == nil ||
		*stub.dkimInput.SigningAttributes.DomainSigningPrivateKey != wantKey {
		t.Fatal("AlreadyExists did not install the replacement BYODKIM private key")
	}
	if stub.dkimInput.SigningAttributes.DomainSigningAttributesOrigin != "" {
		t.Fatalf("Put nested signing origin must be omitted, got %q", stub.dkimInput.SigningAttributes.DomainSigningAttributesOrigin)
	}
}

// TestCanAdoptIdentity pins the pure adoption-criteria truth table: ALL of
// key material actually on file, BYODKIM/EXTERNAL origin, DKIM verification
// SUCCESS, and an exact selector match must hold before an untagged identity
// is provably e2a's own. The mismatched-selector, AWS-managed-origin,
// no-key-material, and DKIM-not-yet-SUCCESS cases are the security-critical
// negatives: a too-loose rule here would let e2a silently adopt a foreign
// application's SES identity in a shared AWS account, or tag an identity it
// cannot actually sign for.
func TestCanAdoptIdentity(t *testing.T) {
	tests := []struct {
		name             string
		out              *sesv2.GetEmailIdentityOutput
		expectedSelector string
		haveKeyMaterial  bool
		want             bool
	}{
		{
			name: "external origin + dkim success + key material + matching selector token → adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             true,
		},
		{
			name: "external origin + mismatched selector → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"someone-elses-selector"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name: "aws-managed (easy DKIM) origin, even with a coincidentally matching token → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginAwsSes,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name: "external origin but no expected selector on file → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			// The core fix for finding 2: a stored selector alone is NOT proof
			// e2a has key material — LoadSendingIdentityState can return a
			// non-empty selector with a nil private key (e.g. mid domain-reclaim).
			// Everything else here matches; only haveKeyMaterial is false.
			name: "external origin + dkim success + matching selector but NO key material on file → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  false,
			want:             false,
		},
		{
			// The core fix for finding 3: DKIM PENDING means SES has not yet
			// matched the key it holds against DNS — a weaker signal than the
			// selector-token match alone (which is a publicly-derivable OSS
			// constant). Strict SUCCESS is required, no PENDING fallback.
			name: "external origin + matching selector but DKIM still pending → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusPending,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name: "external origin + matching selector but DKIM failed → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusFailed,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name: "external origin with no tokens at all → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name:             "nil DkimAttributes → not adoptable",
			out:              &sesv2.GetEmailIdentityOutput{},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			name:             "nil output → not adoptable",
			out:              nil,
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			// Security-critical: a configuration set redirects delivery/bounce/
			// complaint feedback (including recipient addresses) to whoever owns
			// it. Adoption must never inherit that even when the BYODKIM/selector
			// checks would otherwise pass.
			name: "configuration set present, otherwise fully matching → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				ConfigurationSetName: awsString("someone-elses-config-set"),
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
		{
			// Security-critical: an identity policy can grant another AWS
			// account send permissions on this identity. Adoption must never
			// leave that in place either.
			name: "identity policy present, otherwise fully matching → not adoptable",
			out: &sesv2.GetEmailIdentityOutput{
				Policies: map[string]string{"cross-account": `{"Effect":"Allow"}`},
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
					Status:                  ststypes.DkimStatusSuccess,
				},
			},
			expectedSelector: "e2a202607",
			haveKeyMaterial:  true,
			want:             false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canAdoptIdentity(tt.out, tt.expectedSelector, tt.haveKeyMaterial); got != tt.want {
				t.Fatalf("canAdoptIdentity = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSESProvider_ProvisionRefusesForeignConfiguration covers the
// Provision-level negative for a configuration set / identity policy: even
// with a matching BYODKIM selector, either one refuses adoption exactly like
// an unowned identity — no tag, no mutation.
func TestSESProvider_ProvisionRefusesForeignConfiguration(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)

	t.Run("configuration set", func(t *testing.T) {
		stub := &stubSESAPI{
			createErr: &ststypes.AlreadyExistsException{},
			unmanaged: true,
			getOut: &sesv2.GetEmailIdentityOutput{
				ConfigurationSetName: awsString("someone-elses-config-set"),
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
				},
			},
		}
		p := NewSESProvider(stub, "us-east-1", testAccountID)
		if _, err := p.Provision(context.Background(), "shared-config.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
			t.Fatalf("Provision error = %v, want ErrIdentityNotOwned", err)
		}
		if len(stub.tagInputs) != 0 {
			t.Fatalf("identity with a foreign configuration set must never be tagged, got %+v", stub.tagInputs)
		}
		if stub.dkimInput != nil || stub.mailFromInput != nil {
			t.Fatalf("unowned identity was mutated: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
		}
	})

	t.Run("identity policy", func(t *testing.T) {
		stub := &stubSESAPI{
			createErr: &ststypes.AlreadyExistsException{},
			unmanaged: true,
			getOut: &sesv2.GetEmailIdentityOutput{
				Policies: map[string]string{"cross-account": `{"Effect":"Allow"}`},
				DkimAttributes: &ststypes.DkimAttributes{
					SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
					Tokens:                  []string{"e2a202607"},
				},
			},
		}
		p := NewSESProvider(stub, "us-east-1", testAccountID)
		if _, err := p.Provision(context.Background(), "shared-policy.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
			t.Fatalf("Provision error = %v, want ErrIdentityNotOwned", err)
		}
		if len(stub.tagInputs) != 0 {
			t.Fatalf("identity with a foreign policy must never be tagged, got %+v", stub.tagInputs)
		}
	})
}

// TestSESProvider_ProvisionAdoptsProvablyOwnIdentity covers adoption case 1:
// an untagged identity whose installed BYODKIM selector matches e2a's stored
// selector for that exact domain is provably e2a's own (created before
// v1.7.8 introduced the ownership tag). Provision must tag it and then
// proceed exactly as it would for an already-owned identity — no
// ErrIdentityNotOwned, no stranded domain.
func TestSESProvider_ProvisionAdoptsProvablyOwnIdentity(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	stub := &stubSESAPI{
		createErr: &ststypes.AlreadyExistsException{},
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	res, err := p.Provision(context.Background(), "legacy.example", "e2a202607", pkcs1)
	if err != nil {
		t.Fatalf("Provision error = %v, want adoption to succeed", err)
	}
	if res.Status != StatusPending {
		t.Fatalf("want pending after adopted provision, got %q", res.Status)
	}
	if len(stub.tagInputs) != 1 {
		t.Fatalf("want exactly one TagResource call, got %d: %+v", len(stub.tagInputs), stub.tagInputs)
	}
	tagIn := stub.tagInputs[0]
	wantARN := "arn:aws:ses:us-east-1:" + testAccountID + ":identity/legacy.example"
	if tagIn.ResourceArn == nil || *tagIn.ResourceArn != wantARN {
		t.Fatalf("TagResource ARN = %v, want %q", tagIn.ResourceArn, wantARN)
	}
	if len(tagIn.Tags) != 1 || tagIn.Tags[0].Key == nil || *tagIn.Tags[0].Key != managedIdentityTagKey ||
		tagIn.Tags[0].Value == nil || *tagIn.Tags[0].Value != managedIdentityTagValue {
		t.Fatalf("TagResource applied the wrong tag: %+v", tagIn.Tags)
	}
	// Adoption proceeds normally: the BYODKIM selector/key and MAIL FROM are
	// still (re)installed exactly as for an already-owned identity.
	if stub.dkimInput == nil || stub.mailFromInput == nil {
		t.Fatalf("adopted identity was not provisioned normally: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}
}

// TestSESProvider_ProvisionRefusesMismatchedSelector covers adoption case 2,
// the security-critical negative: an untagged BYODKIM identity whose
// installed selector does NOT match e2a's stored selector must be refused
// exactly like today — no tag, no mutation. A too-loose rule here would let
// e2a hijack a different application's SES identity in a shared AWS account.
func TestSESProvider_ProvisionRefusesMismatchedSelector(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	stub := &stubSESAPI{
		createErr: &ststypes.AlreadyExistsException{},
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"someone-elses-selector"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "shared.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Provision error = %v, want ErrIdentityNotOwned", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("mismatched selector must never be tagged, got %+v", stub.tagInputs)
	}
	if stub.dkimInput != nil || stub.mailFromInput != nil {
		t.Fatalf("unowned identity was mutated: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}
}

// TestSESProvider_ProvisionRefusesAWSManagedDkimOrigin covers adoption case
// 3: an untagged identity configured with SES-generated (Easy DKIM) keys —
// AWS_SES origin, not EXTERNAL — can never have been e2a's own Provision
// call (e2a only ever supplies BYODKIM key material), so it must be refused
// even though it happens to expose a matching token.
func TestSESProvider_ProvisionRefusesAWSManagedDkimOrigin(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	stub := &stubSESAPI{
		createErr: &ststypes.AlreadyExistsException{},
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginAwsSes,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "easy-dkim.example", "e2a202607", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Provision error = %v, want ErrIdentityNotOwned", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("AWS-managed DKIM origin must never be tagged, got %+v", stub.tagInputs)
	}
}

// TestSESProvider_ProvisionAlreadyTaggedDoesNotRetag covers adoption case 4:
// an identity that already carries the ownership tag must behave exactly as
// before — no redundant TagResource call.
func TestSESProvider_ProvisionAlreadyTaggedDoesNotRetag(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	// unmanaged defaults to false: stubSESAPI.GetEmailIdentity auto-appends the
	// ownership tag, modeling an already-adopted/always-owned identity.
	stub := &stubSESAPI{createErr: &ststypes.AlreadyExistsException{}}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "already-owned.example", "e2a202607", pkcs1); err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("an already-tagged identity must not be retagged, got %+v", stub.tagInputs)
	}
	if stub.dkimInput == nil || stub.mailFromInput == nil {
		t.Fatalf("already-owned identity was not provisioned: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}
}

// TestSESProvider_ProvisionAdoptionTagFailurePropagates: a transient/permission
// error on the TagResource call itself must propagate (so River retries),
// not silently fall back to ErrIdentityNotOwned.
func TestSESProvider_ProvisionAdoptionTagFailurePropagates(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	boom := errors.New("tag-resource throttled")
	stub := &stubSESAPI{
		createErr: &ststypes.AlreadyExistsException{},
		unmanaged: true,
		tagErr:    boom,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "legacy.example", "e2a202607", pkcs1); !errors.Is(err, boom) {
		t.Fatalf("Provision error = %v, want the TagResource error to propagate", err)
	}
	if stub.dkimInput != nil || stub.mailFromInput != nil {
		t.Fatalf("must not proceed to mutate before adoption is confirmed: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}
}

// TestSESProvider_StatusAdoptsProvablyOwnIdentity proves Status() makes the
// identical adoption judgement as Provision() — the interface widening this
// fix relies on — and that it self-heals an ownership tag removed
// out-of-band (not just the one-time pre-release migration case).
func TestSESProvider_StatusAdoptsProvablyOwnIdentity(t *testing.T) {
	stub := &stubSESAPI{
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			VerifiedForSendingStatus: true,
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
			MailFromAttributes: &ststypes.MailFromAttributes{MailFromDomainStatus: ststypes.MailFromDomainStatusSuccess},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	res, err := p.Status(context.Background(), "legacy.example", "e2a202607", true)
	if err != nil {
		t.Fatalf("Status error = %v, want adoption to succeed", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("adopted identity should report its real status, got %q", res.Status)
	}
	if len(stub.tagInputs) != 1 {
		t.Fatalf("want exactly one TagResource call, got %d", len(stub.tagInputs))
	}
}

// TestSESProvider_StatusRefusesMismatchedSelector mirrors the Provision
// security-critical negative for the Status() path.
func TestSESProvider_StatusRefusesMismatchedSelector(t *testing.T) {
	stub := &stubSESAPI{
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"someone-elses-selector"},
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Status(context.Background(), "shared.example", "e2a202607", true); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Status error = %v, want ErrIdentityNotOwned", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("mismatched selector must never be tagged, got %+v", stub.tagInputs)
	}
}

// TestSESProvider_StatusRefusesAdoptionWithoutKeyMaterial pins finding 2: a
// caller that passes haveKeyMaterial=false must never adopt even when the
// selector token matches AND DKIM is SUCCESS — everything canAdoptIdentity
// can observe from the provider alone says "adopt", but e2a has no private
// key on file for this domain (state.PrivateKey nil/empty, e.g. mid a domain
// reclaim) and cannot actually sign for it. Before this fix, Status only
// checked expectedSelector != "" and would tag it — and the caller's own
// no-key branch (worker.go) would then find it tagged and let Deprovision
// actually delete the identity.
func TestSESProvider_StatusRefusesAdoptionWithoutKeyMaterial(t *testing.T) {
	stub := &stubSESAPI{
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusSuccess,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Status(context.Background(), "no-key.example", "e2a202607", false); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Status error = %v, want ErrIdentityNotOwned when e2a has no key material on file", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("must never tag an identity e2a cannot sign for, got %+v", stub.tagInputs)
	}
}

// TestSESProvider_StatusRefusesAdoptionWhileDkimPending pins finding 3: an
// otherwise-adoptable identity (matching selector, EXTERNAL origin, real key
// material) whose DKIM verification has not yet reached SUCCESS at the
// provider must stay refused — no PENDING fallback.
func TestSESProvider_StatusRefusesAdoptionWhileDkimPending(t *testing.T) {
	stub := &stubSESAPI{
		unmanaged: true,
		getOut: &sesv2.GetEmailIdentityOutput{
			DkimAttributes: &ststypes.DkimAttributes{
				SigningAttributesOrigin: ststypes.DkimSigningAttributesOriginExternal,
				Tokens:                  []string{"e2a202607"},
				Status:                  ststypes.DkimStatusPending,
			},
		},
	}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Status(context.Background(), "still-pending.example", "e2a202607", true); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Status error = %v, want ErrIdentityNotOwned while DKIM is still pending", err)
	}
	if len(stub.tagInputs) != 0 {
		t.Fatalf("must never tag while DKIM has not reached SUCCESS, got %+v", stub.tagInputs)
	}
}

func TestSESProvider_RefusesUnmanagedExistingIdentity(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	stub := &stubSESAPI{createErr: &ststypes.AlreadyExistsException{}, unmanaged: true}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "shared.example", "sel", pkcs1); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Provision error = %v, want ErrIdentityNotOwned", err)
	}
	if stub.dkimInput != nil || stub.mailFromInput != nil {
		t.Fatalf("unmanaged identity was mutated: dkim=%+v mailFrom=%+v", stub.dkimInput, stub.mailFromInput)
	}
	if err := p.Deprovision(context.Background(), "shared.example"); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("Deprovision error = %v, want ErrIdentityNotOwned", err)
	}
}

func TestSESProvider_ProvisionPropagatesMailFromError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	// CreateEmailIdentity ok, but the MAIL FROM call fails (transient) → Provision
	// must surface the error so River retries (not silently return pending).
	stub := &stubSESAPI{putErr: errors.New("throttled")}
	p := NewSESProvider(stub, "us-east-1", testAccountID)
	if _, err := p.Provision(context.Background(), "acme.com", "sel", pkcs1); err == nil {
		t.Fatal("expected PutEmailIdentityMailFromAttributes error to propagate")
	}
}

func TestSESProvider_ProvisionPropagatesReplacementDkimError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	boom := errors.New("dkim update denied")
	stub := &stubSESAPI{createErr: &ststypes.AlreadyExistsException{}, putDkimErr: boom}
	p := NewSESProvider(stub, "us-east-1", testAccountID)

	if _, err := p.Provision(context.Background(), "acme.com", "sel", pkcs1); !errors.Is(err, boom) {
		t.Fatalf("expected DKIM update error to propagate, got %v", err)
	}
	if stub.mailFromInput != nil {
		t.Fatal("MAIL FROM must not be reported configured after the required DKIM replacement failed")
	}
}

func TestSESProvider_StatusReturnsMailFromRecords(t *testing.T) {
	// Status re-emits the MAIL FROM records so the verify/failed transition
	// preserves them (records aren't wiped when a domain goes verified).
	p := NewSESProvider(&stubSESAPI{}, "eu-west-1", testAccountID)
	res, err := p.Status(context.Background(), "acme.com", "", false)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(res.DNSRecords) != 2 {
		t.Fatalf("want 2 records from Status, got %d", len(res.DNSRecords))
	}
	for _, r := range res.DNSRecords {
		if r.Name != "bounce.acme.com" {
			t.Errorf("record name = %q, want bounce.acme.com", r.Name)
		}
	}
}

func TestSESProvider_NotFoundMapping(t *testing.T) {
	t.Run("Status maps NotFoundException to ErrIdentityNotFound", func(t *testing.T) {
		p := NewSESProvider(&stubSESAPI{getErr: &ststypes.NotFoundException{}}, "us-east-1", testAccountID)
		_, err := p.Status(context.Background(), "example.com", "", false)
		if !errors.Is(err, ErrIdentityNotFound) {
			t.Fatalf("expected ErrIdentityNotFound, got %v", err)
		}
	})

	t.Run("Deprovision treats NotFoundException as success", func(t *testing.T) {
		p := NewSESProvider(&stubSESAPI{delErr: &ststypes.NotFoundException{}}, "us-east-1", testAccountID)
		if err := p.Deprovision(context.Background(), "example.com"); err != nil {
			t.Fatalf("expected nil for missing identity, got %v", err)
		}
	})

	t.Run("Status propagates other errors", func(t *testing.T) {
		boom := errors.New("throttled")
		p := NewSESProvider(&stubSESAPI{getErr: boom}, "us-east-1", testAccountID)
		if _, err := p.Status(context.Background(), "example.com", "", false); !errors.Is(err, boom) {
			t.Fatalf("expected boom to propagate, got %v", err)
		}
	})

	t.Run("Deprovision waits for confirmed absence", func(t *testing.T) {
		p := NewSESProvider(&stubSESAPI{deleteLags: true}, "us-east-1", testAccountID)
		p.deleteConfirmDelay = func(int) time.Duration { return 0 }
		if err := p.Deprovision(context.Background(), "example.com"); err == nil {
			t.Fatal("expected a retry while GetEmailIdentity still reports the identity")
		}
	})

	t.Run("Deprovision absorbs brief eventual consistency", func(t *testing.T) {
		stub := &stubSESAPI{deleteLagReads: 2}
		p := NewSESProvider(stub, "us-east-1", testAccountID)
		p.deleteConfirmDelay = func(int) time.Duration { return 0 }
		if err := p.Deprovision(context.Background(), "example.com"); err != nil {
			t.Fatalf("expected bounded confirmation to observe absence, got %v", err)
		}
		if stub.deleteLagReads != 0 || !stub.deleted {
			t.Fatalf("delete confirmation did not exhaust simulated lag: %+v", stub)
		}
	})
}
