package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

func checkoutDep() dep.Dependency {
	return dep.Dependency{
		NWO:      "actions/checkout",
		Ref:      "v4.2.1",
		Tag:      "v4.2.1",
		SHA:      "abc123abc123abc123abc123abc123abc123abc1",
		HashAlgo: "sha1",
	}
}

func setupGoDep() dep.Dependency {
	return dep.Dependency{
		NWO:      "actions/setup-go",
		Ref:      "v5.0.0",
		Tag:      "v5.0.0",
		SHA:      "def456def456def456def456def456def456def4",
		HashAlgo: "sha1",
	}
}

func newStoreWithWorkflows(t *testing.T, dir string) *State {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	return store
}

func TestPruneWorkflows_RemovesStaleEntryAndOrphanedDeps(t *testing.T) {
	dir := t.TempDir()
	store := newStoreWithWorkflows(t, dir)

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{checkoutDep()}, nil, nil); err != nil {
		t.Fatalf("Set ci: %v", err)
	}
	if err := store.Set(context.Background(), ".github/workflows/release.yml", []dep.Dependency{setupGoDep()}, nil, nil); err != nil {
		t.Fatalf("Set release: %v", err)
	}

	removed := store.PruneWorkflows(map[string]bool{".github/workflows/ci.yml": true})
	if len(removed) != 1 || removed[0] != ".github/workflows/release.yml" {
		t.Fatalf("removed = %v, want [.github/workflows/release.yml]", removed)
	}

	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("reading lockfile: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "ci.yml") || !strings.Contains(got, "actions/checkout") {
		t.Errorf("live workflow/dep missing after prune:\n%s", got)
	}
	if strings.Contains(got, "release.yml") {
		t.Errorf("stale workflow still present after prune:\n%s", got)
	}
	if strings.Contains(got, "actions/setup-go") {
		t.Errorf("orphaned dep not garbage-collected after prune:\n%s", got)
	}
}

func TestPruneWorkflows_KeepsDepStillReferenced(t *testing.T) {
	dir := t.TempDir()
	store := newStoreWithWorkflows(t, dir)

	// Both workflows use actions/checkout.
	if err := store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{checkoutDep()}, nil, nil); err != nil {
		t.Fatalf("Set ci: %v", err)
	}
	if err := store.Set(context.Background(), ".github/workflows/release.yml", []dep.Dependency{checkoutDep()}, nil, nil); err != nil {
		t.Fatalf("Set release: %v", err)
	}

	store.PruneWorkflows(map[string]bool{".github/workflows/ci.yml": true})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("reading lockfile: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "actions/checkout") {
		t.Errorf("shared dep dropped even though a live workflow still uses it:\n%s", got)
	}
	if strings.Contains(got, "release.yml") {
		t.Errorf("stale workflow still present after prune:\n%s", got)
	}
}

func TestPruneWorkflows_NoStaleIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := newStoreWithWorkflows(t, dir)

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{checkoutDep()}, nil, nil); err != nil {
		t.Fatalf("Set ci: %v", err)
	}

	removed := store.PruneWorkflows(map[string]bool{".github/workflows/ci.yml": true})
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if keys := store.WorkflowKeys(); len(keys) != 1 || keys[0] != ".github/workflows/ci.yml" {
		t.Fatalf("WorkflowKeys = %v, want [.github/workflows/ci.yml]", keys)
	}
}
