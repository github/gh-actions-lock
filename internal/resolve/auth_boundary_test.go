// This test locks the tokenless+proxy auth contract: no CLI code may
// set its own `Authorization` header on outbound HTTP requests. Hosted
// Dependabot routes egress through an auth-injecting proxy; any
// CLI-owned `Authorization` header would collide with the proxy's and
// silently break the contract. See docs/auth-tokenless-mode.md.
//
// The check is an AST walk for string literals equal to "Authorization"
// in non-test Go source. That catches the common regression vectors:
//
//   - req.Header.Set("Authorization", ...)
//   - headers["Authorization"] = ...
//   - const authHeader = "Authorization"
//
// If this test fails, you have two options:
//
//  1. Route the new HTTP call through internal/resolver/httpclient.go
//     so go-gh owns auth (preferred — see docs/auth-tokenless-mode.md
//     "How to extend without breaking it").
//  2. If there is a genuine, documented reason to set an
//     `Authorization` header from CLI code, gate it behind an opt-in
//     flag, document the proxy-mode incompatibility, and add the file
//     to allowedFiles below with a comment explaining why.
package resolve_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestNoCLIOwnedAuthorizationHeader(t *testing.T) {
	// Files that are allowed to mention "Authorization" as a string
	// literal. Keep this list short and justified.
	allowedFiles := map[string]string{
		// The boundary test itself contains the literal in its
		// detection logic and documentation.
		"internal/resolver/auth_boundary_test.go": "this test",
	}

	repoRoot := findAuthBoundaryRepoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// _test.go files are exempt — they may legitimately stub or
		// assert on auth headers (e.g. asserting that a proxy-injected
		// header is honored, or building an httptest fixture).
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if _, ok := allowedFiles[relSlash]; ok {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			// Don't fail the boundary test on unrelated parse errors;
			// `go build` is the right gate for syntax issues.
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if val == "Authorization" {
				pos := fset.Position(lit.Pos())
				violations = append(violations,
					relSlash+":"+itoa(pos.Line)+": string literal \"Authorization\"")
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking repo: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Fatalf("CLI code must not set its own Authorization header "+
			"(see docs/auth-tokenless-mode.md). Found:\n  %s\n\n"+
			"Route the request through internal/resolver/httpclient.go "+
			"so go-gh owns auth, or — if you have a documented reason — "+
			"extend allowedFiles in internal/resolver/auth_boundary_test.go "+
			"with a justification.",
			strings.Join(violations, "\n  "))
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// findAuthBoundaryRepoRoot walks up from this test file until it finds
// the directory containing the top-level go.mod
// (module github.com/github/gh-actions-pin). Mirrors the helper in
// internal/lockfile/import_boundary_test.go.
func findAuthBoundaryRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate test file")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 20; i++ {
		candidate := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(candidate); err == nil {
			if strings.Contains(string(data), "module github.com/github/gh-actions-pin\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (top-level go.mod for github.com/github/gh-actions-pin)")
	return ""
}
