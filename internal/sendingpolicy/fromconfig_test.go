package sendingpolicy

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
)

func TestPolicySourceFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want PolicySource
	}{
		{name: "absent defaults to config", want: PolicySourceConfig},
		{name: "explicit config", raw: "config", want: PolicySourceConfig},
		{name: "hosted database", raw: "database", want: PolicySourceDatabase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SourceFromConfig(&config.Config{SendingProtect: config.SendingProtectionConfig{
				RuntimePolicySource: tc.raw,
			}})
			if err != nil {
				t.Fatalf("source from config: %v", err)
			}
			if got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := SourceFromConfig(&config.Config{SendingProtect: config.SendingProtectionConfig{
		RuntimePolicySource: "latest",
	}}); err == nil {
		t.Fatal("unknown runtime policy source must fail closed")
	}
}
