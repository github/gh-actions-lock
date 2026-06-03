package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeMetadataResolver struct{}

func (fakeMetadataResolver) RepoIDs(owner, repo string) (int64, int64, error) {
	return 1, 2, nil
}

func TestStore_PersistsTagAndBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []Dependency{
		{
			NWO:      "actions/checkout",
			Ref:      "v4.2.1",
			Tag:      "v4.2.1",
			Branch:   "main",
			SHA:      "abc123abc123abc123abc123abc123abc123abc1",
			HashAlgo: "sha1",
		},
		{
			NWO:      "internal/branch-only",
			Ref:      "main",
			Tag:      "",
			Branch:   "main",
			SHA:      "def456def456def456def456def456def456def4",
			HashAlgo: "sha1",
		},
	}

	if err := store.Set(WorkflowKeyFromPath(filepath.Join(dir, ".github", "workflows", "ci.yml")), deps, nil, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, Path))
	if err != nil {
		t.Fatalf("reading lockfile: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"'actions/checkout@v4.2.1:sha1-",
		"tag: 'v4.2.1'",
		"branch: 'main'",
		"'internal/branch-only@main:sha1-",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lockfile missing %q\n--- contents ---\n%s", want, got)
		}
	}

	// Branch-only entry should NOT emit tag:, only branch:.
	branchOnlyIdx := strings.Index(got, "internal/branch-only@main:sha1-")
	if branchOnlyIdx < 0 {
		t.Fatalf("expected branch-only entry in lockfile")
	}
	branchSection := got[branchOnlyIdx:]
	nextEntryIdx := strings.Index(branchSection[1:], "  'actions/")
	if nextEntryIdx >= 0 {
		branchSection = branchSection[:nextEntryIdx+1]
	}
	if strings.Contains(branchSection, "tag:") {
		t.Errorf("branch-only entry should not emit tag:\n%s", branchSection)
	}

	// Reload and verify roundtrip.
	store2, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	checkoutKey := "actions/checkout@v4.2.1:sha1-abc123abc123abc123abc123abc123abc123abc1"
	a, ok := store2.file.Actions[checkoutKey]
	if !ok {
		t.Fatalf("expected %s in reloaded lockfile, keys=%v", checkoutKey, actionKeys(store2.file.Actions))
	}
	if a.Tag != "v4.2.1" {
		t.Errorf("expected Tag=v4.2.1, got %q", a.Tag)
	}
	if a.Branch != "main" {
		t.Errorf("expected Branch=main, got %q", a.Branch)
	}
	branchOnlyKey := "internal/branch-only@main:sha1-def456def456def456def456def456def456def4"
	b, ok := store2.file.Actions[branchOnlyKey]
	if !ok {
		t.Fatalf("expected %s, keys=%v", branchOnlyKey, actionKeys(store2.file.Actions))
	}
	if b.Tag != "" {
		t.Errorf("expected empty Tag, got %q", b.Tag)
	}
	if b.Branch != "main" {
		t.Errorf("expected Branch=main, got %q", b.Branch)
	}
}

func actionKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestStore_SetRejectsEmptyBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []Dependency{
		{
			NWO:      "actions/checkout",
			Ref:      "v4",
			SHA:      "abc123abc123abc123abc123abc123abc123abc1",
			HashAlgo: "sha1",
			// Branch intentionally empty — should be rejected.
		},
	}

	err = store.Set(".github/workflows/ci.yml", deps, nil, nil)
	if err == nil {
		t.Fatal("expected error for dep with empty Branch, got nil")
	}
	if !strings.Contains(err.Error(), "branch is required") {
		t.Errorf("expected 'branch is required' in error, got: %v", err)
	}
}
