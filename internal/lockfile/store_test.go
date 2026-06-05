package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
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

// TestStore_DiamondTransitiveDepEmittedCorrectly verifies that when two direct
// actions share a transitive dependency (diamond pattern: A→C, B→C), the
// lockfile correctly records `uses: [C]` on both A and B, and the shared dep
// C appears in the actions section exactly once.
func TestStore_DiamondTransitiveDepEmittedCorrectly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []Dependency{
		{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Branch: "main"},
		{NWO: "shared/dep", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc", HashAlgo: "sha1", Branch: "main"},
	}
	parentMap := map[string][]string{
		"shared/dep@v1": {"owner/a@v1", "owner/b@v1"},
	}
	directKeys := map[string]bool{
		"owner/a@v1": true,
		"owner/b@v1": true,
	}

	if err := store.Set(".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
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

	// Shared dep pin that both A and B should reference.
	sharedPin := "shared/dep@v1:sha1-cccccccccccccccccccccccccccccccccccccccc"

	// Both A and B should have uses: containing the shared dep.
	aPin := "owner/a@v1:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bPin := "owner/b@v1:sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Verify structure via reload.
	store2, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	aAction, ok := store2.file.Actions[aPin]
	if !ok {
		t.Fatalf("expected %s in lockfile, keys=%v", aPin, actionKeys(store2.file.Actions))
	}
	if len(aAction.Uses) != 1 || aAction.Uses[0] != sharedPin {
		t.Errorf("expected A.Uses = [%s], got %v", sharedPin, aAction.Uses)
	}
	bAction, ok := store2.file.Actions[bPin]
	if !ok {
		t.Fatalf("expected %s in lockfile, keys=%v", bPin, actionKeys(store2.file.Actions))
	}
	if len(bAction.Uses) != 1 || bAction.Uses[0] != sharedPin {
		t.Errorf("expected B.Uses = [%s], got %v", sharedPin, bAction.Uses)
	}
	// Shared dep exists exactly once.
	if _, ok := store2.file.Actions[sharedPin]; !ok {
		t.Errorf("expected shared dep %s in actions, keys=%v", sharedPin, actionKeys(store2.file.Actions))
	}
	// Workflows section only lists direct refs.
	wfDeps := store2.file.Workflows[".github/workflows/ci.yml"]
	if len(wfDeps) != 2 {
		t.Fatalf("expected 2 direct workflow deps, got %d: %v", len(wfDeps), wfDeps)
	}

	// Verify the raw YAML contains the uses: field (not just in-memory).
	if !strings.Contains(got, "uses:") {
		t.Errorf("expected 'uses:' in serialized lockfile, got:\n%s", got)
	}
}

// TestStore_SaveGCHandlesCyclicUses verifies that Save()'s garbage collection
// walk (which follows uses: edges) terminates when the uses: graph contains a
// cycle (A uses B, B uses A). Both entries should be retained.
func TestStore_SaveGCHandlesCyclicUses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []Dependency{
		{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Branch: "main"},
	}
	// Cyclic parentMap: A is parent of B, B is parent of A.
	parentMap := map[string][]string{
		"owner/b@v1": {"owner/a@v1"},
		"owner/a@v1": {"owner/b@v1"},
	}
	// A is the workflow-direct entry.
	directKeys := map[string]bool{
		"owner/a@v1": true,
	}

	if err := store.Set(".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify both entries survived GC (cycle didn't cause infinite
	// loop or premature GC).
	store2, err := OpenStore(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}

	aPin := "owner/a@v1:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bPin := "owner/b@v1:sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if _, ok := store2.file.Actions[aPin]; !ok {
		t.Errorf("expected %s to survive GC, keys=%v", aPin, actionKeys(store2.file.Actions))
	}
	if _, ok := store2.file.Actions[bPin]; !ok {
		t.Errorf("expected %s to survive GC (reachable via cyclic uses:), keys=%v", bPin, actionKeys(store2.file.Actions))
	}
}

// TestStore_SaveIsByteDeterministic guarantees Save produces byte-identical
// output for the same logical content across two independent runs. This is
// the G8 contract for Dependabot: the platform diffs fetched-vs-written
// content and drops unchanged files, so any nondeterminism (map iteration
// order, embedded timestamps, unstable list ordering) either produces
// phantom diffs on every run or causes real pin changes to be silently
// dropped during grouped updates.
//
// If this test fails, the writer in marshalDeterministic has reintroduced
// nondeterminism. Sort all map iterations, do not embed timestamps, and
// keep list ordering stable.
func TestStore_SaveIsByteDeterministic(t *testing.T) {
	// Build a non-trivial store: two workflows, multiple actions, a diamond
	// transitive dep so we exercise the parent-map / uses-list sort paths.
	deps := []Dependency{
		{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/b", Ref: "v2", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Branch: "main"},
		{NWO: "shared/dep", Ref: "v3", SHA: "cccccccccccccccccccccccccccccccccccccccc", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/extra", Ref: "v4", SHA: "dddddddddddddddddddddddddddddddddddddddd", HashAlgo: "sha1", Branch: "main"},
	}
	parentMap := map[string][]string{
		"shared/dep@v3": {"owner/a@v1", "owner/b@v2"},
	}
	directKeys := map[string]bool{
		"owner/a@v1":     true,
		"owner/b@v2":     true,
		"owner/extra@v4": true,
	}

	build := func() []byte {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(dir, fakeMetadataResolver{})
		if err != nil {
			t.Fatalf("opening store: %v", err)
		}
		if err := store.Set(".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
			t.Fatalf("Set ci.yml: %v", err)
		}
		if err := store.Set(".github/workflows/release.yml", deps, parentMap, directKeys); err != nil {
			t.Fatalf("Set release.yml: %v", err)
		}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, Path))
		if err != nil {
			t.Fatalf("read lockfile: %v", err)
		}
		return raw
	}

	first := build()
	second := build()

	if !bytesEqual(first, second) {
		t.Fatalf("Save is non-deterministic: byte-for-byte mismatch between runs\n--- first run (%d bytes) ---\n%s\n--- second run (%d bytes) ---\n%s",
			len(first), string(first), len(second), string(second))
	}

	// Sanity guard: ensure we actually wrote a non-trivial lockfile (so a
	// future refactor that accidentally short-circuits Save can't make this
	// test pass vacuously).
	if !strings.Contains(string(first), "shared/dep@v3") {
		t.Fatalf("expected lockfile to contain shared/dep@v3, got:\n%s", string(first))
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStore_RefusesFutureVersionLockfile(t *testing.T) {
	// An old binary encountering a lockfile written by a newer binary must
	// surface the version error to the user, not silently treat the lockfile
	// as corrupt and overwrite it — that would destroy pins this binary
	// cannot interpret.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, Path)
	if err := os.WriteFile(lockPath, []byte("version: v9.0.0\ndependencies: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := OpenStore(dir, fakeMetadataResolver{})
	if err == nil {
		t.Fatal("expected error opening future-version lockfile, got nil")
	}
	if !errors.Is(err, parserlock.ErrFutureVersion) {
		t.Errorf("error does not match ErrFutureVersion sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "gh extension upgrade gh-actions-pin") {
		t.Errorf("error should include upgrade-path hint, got: %v", err)
	}

	// Lockfile must remain untouched on disk so the user can recover.
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lockfile after refused open: %v", err)
	}
	if !strings.Contains(string(got), "v9.0.0") {
		t.Errorf("lockfile was overwritten: %s", got)
	}
}
