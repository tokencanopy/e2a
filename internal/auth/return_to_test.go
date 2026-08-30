package auth

import "testing"

func TestValidateReturnToPathAllowsGetStartedStep(t *testing.T) {
	t.Parallel()

	if err := validateReturnToPath("/get-started?step=address"); err != nil {
		t.Fatalf("validateReturnToPath() error = %v, want nil", err)
	}
}

func TestValidateReturnToPathKeepsGetStartedExact(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"/get-started/other",
		"/get-started/../dashboard",
	} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if err := validateReturnToPath(raw); err == nil {
				t.Fatal("validateReturnToPath() error = nil, want rejection")
			}
		})
	}
}
