package outbound

import (
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// Pure unit tests over the adapter's two rewriting steps. The DB-backed
// tests live in the external test package, because the shared test database
// helper transitively imports this package.

// SmuggledMIME is exported for the external test package.
var SmuggledMIME = smuggledMIME

// smuggledMIME is customer-shaped wire bytes that try every spelling of the
// provider-owned headers, including a folded one and a body decoy.
func smuggledMIME() []byte {
	return []byte("From: agent@agents.e2a.dev\r\n" +
		"x-ses-tenant: smuggled-lower\r\n" +
		"X-SES-TENANT: smuggled-upper\r\n" +
		"X-Ses-Tenant: smuggled-\r\n folded\r\n" +
		"X-E2A-Provider-Attempt: cor_forged\r\n" +
		"x-e2a-provider-attempt: cor_forged_two\r\n" +
		"X-SES-CONFIGURATION-SET: attacker-set\r\n" +
		"x-ses-configuration-set:\r\n\tattacker-folded\r\n" +
		"X-E2A-Message-ID: msg_forged\r\n" +
		"Subject: hello\r\n" +
		"\r\n" +
		"X-SES-TENANT: body-decoy\r\n" +
		"body line\r\n")
}

func TestProviderHeaderLinesFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		h       sendingpolicy.ProviderHeaders
		set, id string
		wantErr error
		want    string
	}{
		"tenant required but empty": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1", TenantRequired: true}, wantErr: ErrTenantNameMissing,
		},
		"tenant required but blank": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1", TenantRequired: true, TenantName: " \t"}, wantErr: ErrTenantNameMissing,
		},
		"line break in tenant": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1", TenantRequired: true, TenantName: "t\r\nBcc: x"}, wantErr: ErrProviderHeaderValue,
		},
		"line break in config set": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1"}, set: "a\nb", wantErr: ErrProviderHeaderValue,
		},
		"line break in message id": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1"}, id: "m\r", wantErr: ErrProviderHeaderValue,
		},
		"no tenant, no set, no id": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1"}, want: "X-E2A-Provider-Attempt: cor_1\r\n",
		},
		"everything, in the legacy path's order": {
			h: sendingpolicy.ProviderHeaders{AttemptCorrelationID: "cor_1", TenantRequired: true, TenantName: "tenant_a"}, set: "cs", id: "msg_1",
			want: "X-SES-CONFIGURATION-SET: cs\r\nX-E2A-Message-ID: msg_1\r\nX-E2A-Provider-Attempt: cor_1\r\nX-SES-TENANT: tenant_a\r\n",
		},
	} {
		got, err := providerHeaderLines(tc.h, tc.set, tc.id)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
		}
		if string(got) != tc.want {
			t.Errorf("%s: headers = %q, want %q", name, got, tc.want)
		}
	}
}

func TestStripProviderHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want string
		wantErr  error
	}{
		"mixed case, duplicates, folded": {
			in:   string(smuggledMIME()),
			want: "From: agent@agents.e2a.dev\r\nSubject: hello\r\n\r\nX-SES-TENANT: body-decoy\r\nbody line\r\n",
		},
		"lf-only line endings": {
			in:   "X-SES-TENANT: a\nSubject: s\nx-ses-configuration-set: b\n c\n\nbody\n",
			want: "Subject: s\n\nbody\n",
		},
		"headers only, no body separator": {
			in:   "Subject: s\r\nX-E2A-Provider-Attempt: cor\r\n\tfolded\r\n",
			want: "Subject: s\r\n",
		},
		"nothing to strip": {
			in:   "Subject: s\r\nTo: a@example.test\r\n\r\nbody\r\n",
			want: "Subject: s\r\nTo: a@example.test\r\n\r\nbody\r\n",
		},
		"name prefix is not a match": {
			in:   "X-SES-TENANT-EXTRA: keep\r\nX-SES-TENANTS: keep\r\n\r\n",
			want: "X-SES-TENANT-EXTRA: keep\r\nX-SES-TENANTS: keep\r\n\r\n",
		},
		"whitespace before colon still matches": {
			in:   "X-SES-TENANT : a\r\nSubject: s\r\n\r\n",
			want: "Subject: s\r\n\r\n",
		},
		"continuation after a kept header is kept": {
			in:   "Subject: long\r\n subject\r\nX-SES-TENANT: a\r\n b\r\nTo: x@example.test\r\n\r\n",
			want: "Subject: long\r\n subject\r\nTo: x@example.test\r\n\r\n",
		},
		"body may contain bare CR": {
			in:   "Subject: s\r\n\r\nbinary\rbody\r\n",
			want: "Subject: s\r\n\r\nbinary\rbody\r\n",
		},
		"bare CR hiding a header is refused": {
			in:      "Subject: a\rX-SES-TENANT: evil\r\n\r\nbody\r\n",
			wantErr: ErrMalformedHeaderSection,
		},
		"CR CR LF pseudo-separator is refused": {
			in:      "Subject: s\r\n\r\r\nX-SES-TENANT: evil\r\n\r\nbody",
			wantErr: ErrMalformedHeaderSection,
		},
		"empty": {in: "", want: ""},
	} {
		got, err := stripProviderHeaders([]byte(tc.in))
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}
