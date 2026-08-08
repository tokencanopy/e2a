package filterquery

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStdlibOnlyDeps guards design decision D4: the package must stay
// dependency-free (stdlib only) so any service can embed it without pulling
// the module's internal tree. Test files are checked too (-test): a
// third-party assertion library would be just as much of a leak.
func TestStdlibOnlyDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-test", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps -test: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		first, _, _ := strings.Cut(dep, "/")
		if !strings.Contains(first, ".") {
			continue // stdlib: no dot in the first path segment
		}
		// The package itself, its external test package, and the generated
		// test main (listed in brackets) all carry the module prefix.
		if strings.HasPrefix(strings.Trim(dep, "[]"), "github.com/tokencanopy/e2a/internal/filterquery") {
			continue
		}
		t.Errorf("non-stdlib dependency %q: filterquery must stay stdlib-only (D4)", dep)
	}
}
