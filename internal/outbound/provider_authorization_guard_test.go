package outbound

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryProviderCallRequiresAuthorization is the tracked closure guard for
// the provider seam. It parses every tracked production Go file and rejects:
//
//   - any import of net/smtp outside the relay itself and the named exceptions;
//   - any call to the relay's private socket-opening core outside the one
//     authorized adapter;
//   - any exported relay method that could open a socket without a token.
//
// Exceptions are exact file paths, never substrings, and each is named here
// with the reason it may exist. Adding a provider-bound caller anywhere else
// fails this test until it goes through ProviderSubmitter.SubmitOnce.
func TestEveryProviderCallRequiresAuthorization(t *testing.T) {
	root := moduleRoot(t)
	files := trackedGoFiles(t, root)

	// Files that may import net/smtp: the relay (the only SES client) and the
	// self-test scenarios, which drive a local SMTP conversation against
	// e2a's OWN inbound listener to prove delivery end to end — never the
	// provider.
	smtpImportAllowed := map[string]string{
		"internal/outbound/smtp_relay.go": "the provider relay itself",
		"internal/selftest/scenarios.go":  "local inbound self-test client, not provider-bound",
	}
	// Files that may call the relay's socket-opening core.
	socketCallAllowed := map[string]string{
		"internal/outbound/provider_submit.go": "the one authorized adapter",
	}

	fset := token.NewFileSet()
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, src, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "net/smtp" {
				if _, ok := smtpImportAllowed[rel]; !ok {
					t.Errorf("%s imports net/smtp: provider I/O must go through outbound.ProviderSubmitter (or be named in the guard's exception list with its reason)", rel)
				}
			}
		}
		full, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(full, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "sendOnceContext" {
				if _, ok := socketCallAllowed[rel]; !ok {
					t.Errorf("%s:%s calls the relay's socket-opening core outside the authorized adapter", rel, fset.Position(call.Pos()))
				}
			}
			return true
		})
	}

	// The relay's exported surface may not open a socket: Configured is a
	// field read, and everything that dials is unexported. A newly exported
	// Send* method is exactly the bypass this guard exists to refuse.
	relaySrc, err := os.ReadFile(filepath.Join(root, "internal/outbound/smtp_relay.go"))
	if err != nil {
		t.Fatal(err)
	}
	relayFile, err := parser.ParseFile(fset, "smtp_relay.go", relaySrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range relayFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if ident, ok := recv.(*ast.Ident); !ok || ident.Name != "SMTPRelay" {
			continue
		}
		if fn.Name.IsExported() && fn.Name.Name != "Configured" {
			t.Errorf("SMTPRelay exports %s: the relay must expose no socket-opening method", fn.Name.Name)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// trackedGoFiles lists tracked, non-test Go files under internal/ and cmd/
// — production code only, by git's own account of what ships.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--", "internal/*.go", "internal/**/*.go", "cmd/*.go", "cmd/**/*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasSuffix(line, "_test.go") {
			continue
		}
		files = append(files, line)
	}
	if len(files) < 50 {
		t.Fatalf("only %d tracked production files found; the guard is scanning the wrong tree", len(files))
	}
	return files
}
