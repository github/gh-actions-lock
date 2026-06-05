package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// TestWriteWorkflowFile_PreservesMode confirms that rewriting a workflow file
// keeps its existing permission bits rather than widening to 0o644.
func TestWriteWorkflowFile_PreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	if err := os.WriteFile(path, []byte("name: ci\n"), 0o600); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	if err := writeWorkflowFile(path, []byte("name: ci updated\n")); err != nil {
		t.Fatalf("writeWorkflowFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected mode 0600 preserved, got %o", got)
	}
}

// TestWriteWorkflowFile_DefaultsWhenMissing confirms that writing a file that
// does not yet exist falls back to 0o644.
func TestWriteWorkflowFile_DefaultsWhenMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "new.yml")

	if err := writeWorkflowFile(path, []byte("name: ci\n")); err != nil {
		t.Fatalf("writeWorkflowFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("expected default mode 0644, got %o", got)
	}
}

// TestDropDeps_ContainsBadKeepsSiblings is the unit-level guard for the
// non-convergence fix: an impostor dep mixed into a workflow must be dropped
// from the closure while every benign sibling survives, and the parent map
// must be pruned of the bad key as both a child entry and a parent reference.
func TestDropDeps_ContainsBadKeepsSiblings(t *testing.T) {
	deps := []lockfile.Dependency{
		{NWO: "actions/cache", Ref: "v5.0.5"},
		{NWO: "mmastrac/mmm-matrix", Ref: "v1.0.5"}, // impostor
		{NWO: "actions/checkout", Ref: "v6.0.2"},
	}
	parentMap := resolver.ParentMap{
		"actions/cache@v5.0.5":       {"mmastrac/mmm-matrix@v1.0.5"},
		"mmastrac/mmm-matrix@v1.0.5": {},
		"actions/checkout@v6.0.2":    {},
	}
	drop := map[string]bool{"mmastrac/mmm-matrix@v1.0.5": true}

	gotDeps, gotPM := dropDeps(deps, parentMap, drop)

	wantDeps := []lockfile.Dependency{
		{NWO: "actions/cache", Ref: "v5.0.5"},
		{NWO: "actions/checkout", Ref: "v6.0.2"},
	}
	if !reflect.DeepEqual(gotDeps, wantDeps) {
		t.Fatalf("deps: got %+v, want %+v", gotDeps, wantDeps)
	}

	if _, ok := gotPM["mmastrac/mmm-matrix@v1.0.5"]; ok {
		t.Fatalf("parent map still has dropped child key: %+v", gotPM)
	}
	if parents := gotPM["actions/cache@v5.0.5"]; len(parents) != 0 {
		t.Fatalf("dropped key not pruned from parent list, got %+v", parents)
	}
}

// TestDropDeps_DoesNotMutateInputs confirms dropDeps leaves the caller's deps
// slice and parent map untouched (the apply path reuses neither after, but the
// contract matters for the index-aligned DirectTracker rebuild that follows).
func TestDropDeps_DoesNotMutateInputs(t *testing.T) {
	deps := []lockfile.Dependency{
		{NWO: "actions/cache", Ref: "v5.0.5"},
		{NWO: "bad/actor", Ref: "v9"},
	}
	parentMap := resolver.ParentMap{
		"actions/cache@v5.0.5": {"bad/actor@v9"},
		"bad/actor@v9":         {},
	}
	drop := map[string]bool{"bad/actor@v9": true}

	dropDeps(deps, parentMap, drop)

	if len(deps) != 2 {
		t.Fatalf("input deps mutated, len=%d", len(deps))
	}
	if got := parentMap["actions/cache@v5.0.5"]; !reflect.DeepEqual(got, []string{"bad/actor@v9"}) {
		t.Fatalf("input parent map mutated: %+v", got)
	}
}
