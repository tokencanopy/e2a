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
	// The ONE function that may reference the relay's socket-opening core:
	// the authorized adapter's SubmitOnce. The exception is a symbol, not a
	// file, so a second function added beside it is not exempt.
	socketCallAllowed := map[string]string{
		"internal/outbound/provider_submit.go:SubmitOnce": "the one authorized adapter method",
	}
	// Provider SDKs that can send mail without SMTP, and the one package
	// that may import each: sender-identity provisioning uses SES v2 for
	// identities and tags, never SendEmail. A send through an HTTP provider
	// API is invisible to the socket check, so the import is fenced instead.
	providerSDKAllowed := map[string]map[string]string{
		"github.com/aws/aws-sdk-go-v2/service/sesv2": {
			"internal/senderidentity/ses.go":  "SES identity provisioning",
			"internal/senderidentity/tags.go": "SES identity tagging",
		},
	}
	allowedSocketCalls := 0

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
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "net/smtp" {
				if _, ok := smtpImportAllowed[rel]; !ok {
					t.Errorf("%s imports net/smtp: provider I/O must go through outbound.ProviderSubmitter (or be named in the guard's exception list with its reason)", rel)
				}
			}
			if files, fenced := providerSDKAllowed[path]; fenced {
				if _, ok := files[rel]; !ok {
					t.Errorf("%s imports %s: a provider SDK may only be used where the guard names it, and never to send", rel, path)
				}
			}
		}
		full, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		// Any reference to the socket core counts, not only a direct call:
		// a method value (`f := r.sendOnceContext`) or a method expression
		// (`(*SMTPRelay).sendOnceContext`) is a SelectorExpr too, and either
		// would otherwise let a caller open the socket one hop away from the
		// name this guard looks for.
		for _, decl := range full.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			var enclosing string
			if isFunc {
				enclosing = rel + ":" + fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "sendOnceContext" {
					return true
				}
				if rel == "internal/outbound/smtp_relay.go" && isFunc && fn.Name.Name == "sendOnceContext" {
					return true // the definition's own receiver method is not a reference
				}
				if _, ok := socketCallAllowed[enclosing]; ok {
					allowedSocketCalls++
					return true
				}
				t.Errorf("%s:%s references the relay's socket-opening core outside ProviderSubmitter.SubmitOnce", rel, fset.Position(sel.Pos()))
				return true
			})
		}
	}
	// The sentinel must be real: renaming the socket core would otherwise
	// turn the whole reference check into a no-op that still passes.
	if allowedSocketCalls == 0 {
		t.Fatal("ProviderSubmitter.SubmitOnce no longer references sendOnceContext: the guard's sentinel is stale, update both together")
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

// moduleRoot walks up from the package directory to the module's go.mod.
// It needs no git: a guard that skipped itself wherever git was absent (a
// source tarball, a container without the binary, a prebuilt test binary)
// would report green exactly where nobody was looking.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}

// trackedGoFiles lists the production (non-test) Go files under internal/
// and cmd/. Git's index is the authority when available — it is what ships —
// and a filesystem walk is the fallback so the guard never skips.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	cmd := exec.Command("git", "ls-files", "--", "internal/*.go", "internal/**/*.go", "cmd/*.go", "cmd/**/*.go")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" || strings.HasSuffix(line, "_test.go") {
				continue
			}
			files = append(files, line)
		}
	} else {
		for _, top := range []string{"internal", "cmd"} {
			err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", top, err)
			}
		}
	}
	if len(files) < 50 {
		t.Fatalf("only %d production files found; the guard is scanning the wrong tree", len(files))
	}
	return files
}
