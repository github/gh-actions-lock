// This test locks the parser_aliases boundary so that the upcoming
// pkg/lockfile → github.com/github/actions-lockfile extraction is a
// single-file change: only internal/lockfile/parser_aliases.go has to
// swap its import path. Every other CLI package consumes the schema
// through the internal/lockfile aliases (ActionRef, Pin, ParseActionRef,
// …) so the blast radius of the extraction stays bounded.
//
// See pkg-library-boundary.md ("Sequencing" item #2) for the rationale.
// If this test fails, you have two options:
//
//  1. Route the offending import through internal/lockfile (preferred —
//     add an alias in parser_aliases.go if one is missing).
//  2. If there is a genuine reason the file must live outside
//     internal/lockfile/ and import parserlock directly, extend
//     allowedPrefixes below with a comment explaining why.
package lockfile_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNoDirectParserlockImports(t *testing.T) {
	forbidden := map[string]bool{
		`"github.com/github/gh-actions-pin/pkg/lockfile"`:                    true,
		`"github.com/github/actions-workflow-parser/go/lockfile/parserlock"`: true,
	}

	// Files under these repo-relative prefixes are allowed to import the
	// forbidden packages directly. internal/lockfile/ owns the alias
	// trampoline; pkg/lockfile/ is the package itself (self-imports and
	// internal tests). pkg/check/ is a public peer library package that
	// shares the same staged-for-extraction module boundary as
	// pkg/lockfile — the schema/grammar are part of its facts shape, so
	// routing through internal/lockfile would invert the dependency.
	// Future pkg/* peers (e.g. pkg/provenance) should be added here for
	// the same reason; CLI and internal/* code still routes through
	// internal/lockfile aliases.
	allowedPrefixes := []string{
		"internal/lockfile/",
		"pkg/lockfile/",
		"pkg/check/",
	}

	repoRoot := findRepoRoot(t)
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

		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if isAllowed(relSlash, allowedPrefixes) {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// Don't fail the boundary test on unrelated parse errors;
			// `go build` is the right gate for syntax issues.
			return nil
		}
		for _, imp := range f.Imports {
			if forbidden[imp.Path.Value] {
				violations = append(violations,
					relSlash+" imports "+strings.Trim(imp.Path.Value, `"`))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking repo: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Fatalf("forbidden direct imports outside internal/lockfile boundary "+
			"(route these through internal/lockfile aliases; see "+
			"internal/lockfile/parser_aliases.go):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

func isAllowed(relSlash string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(relSlash, p) {
			return true
		}
	}
	return false
}

// findRepoRoot walks up from this test file until it finds the directory
// containing the top-level go.mod (module github.com/github/gh-actions-pin).
// pkg/lockfile/go.mod is skipped; it belongs to the soon-to-be-extracted
// submodule.
func findRepoRoot(t *testing.T) string {
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
