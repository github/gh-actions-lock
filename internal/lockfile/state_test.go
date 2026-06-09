package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/workflowfile"
)

type fakeMetadataResolver struct{}

func (fakeMetadataResolver) RepoIDs(_ context.Context, owner, repo string) (int64, int64, error) {
	return 1, 2, nil
}

func TestState_PersistsTagAndBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []dep.Dependency{
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

	if err := store.Set(context.Background(), workflowfile.KeyFromPath(filepath.Join(dir, ".github", "workflows", "ci.yml")), deps, nil, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
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
	store2, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	checkoutKey := "actions/checkout@v4.2.1:sha1-abc123abc123abc123abc123abc123abc123abc1"
	a, ok := store2.file.Dependencies[checkoutKey]
	if !ok {
		t.Fatalf("expected %s in reloaded lockfile, keys=%v", checkoutKey, actionKeys(store2.file.Dependencies))
	}
	if a.Tag != "v4.2.1" {
		t.Errorf("expected Tag=v4.2.1, got %q", a.Tag)
	}
	if a.Branch != "main" {
		t.Errorf("expected Branch=main, got %q", a.Branch)
	}
	branchOnlyKey := "internal/branch-only@main:sha1-def456def456def456def456def456def456def4"
	b, ok := store2.file.Dependencies[branchOnlyKey]
	if !ok {
		t.Fatalf("expected %s, keys=%v", branchOnlyKey, actionKeys(store2.file.Dependencies))
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

func TestState_SetRejectsEmptyBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []dep.Dependency{
		{
			NWO:      "actions/checkout",
			Ref:      "v4",
			SHA:      "abc123abc123abc123abc123abc123abc123abc1",
			HashAlgo: "sha1",
			// Branch intentionally empty — should be rejected.
		},
	}

	err = store.Set(context.Background(), ".github/workflows/ci.yml", deps, nil, nil)
	if err == nil {
		t.Fatal("expected error for dep with empty Branch, got nil")
	}
	if !strings.Contains(err.Error(), "branch is required") {
		t.Errorf("expected 'branch is required' in error, got: %v", err)
	}
}

// TestState_DiamondTransitiveDepEmittedCorrectly verifies that when two direct
// actions share a transitive dependency (diamond pattern: A→C, B→C), the
// lockfile correctly records `uses: [C]` on both A and B, and the shared dep
// C appears in the dependencies section exactly once.
func TestState_DiamondTransitiveDepEmittedCorrectly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []dep.Dependency{
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

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
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
	store2, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	aAction, ok := store2.file.Dependencies[aPin]
	if !ok {
		t.Fatalf("expected %s in lockfile, keys=%v", aPin, actionKeys(store2.file.Dependencies))
	}
	if len(aAction.Uses) != 1 || aAction.Uses[0] != sharedPin {
		t.Errorf("expected A.Uses = [%s], got %v", sharedPin, aAction.Uses)
	}
	bAction, ok := store2.file.Dependencies[bPin]
	if !ok {
		t.Fatalf("expected %s in lockfile, keys=%v", bPin, actionKeys(store2.file.Dependencies))
	}
	if len(bAction.Uses) != 1 || bAction.Uses[0] != sharedPin {
		t.Errorf("expected B.Uses = [%s], got %v", sharedPin, bAction.Uses)
	}
	// Shared dep exists exactly once.
	if _, ok := store2.file.Dependencies[sharedPin]; !ok {
		t.Errorf("expected shared dep %s in dependencies, keys=%v", sharedPin, actionKeys(store2.file.Dependencies))
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

// TestState_SaveGCHandlesCyclicUses verifies that Save()'s garbage collection
// walk (which follows uses: edges) terminates when the uses: graph contains a
// cycle (A uses B, B uses A). Both entries should be retained.
func TestState_SaveGCHandlesCyclicUses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	deps := []dep.Dependency{
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

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify both entries survived GC (cycle didn't cause infinite
	// loop or premature GC).
	store2, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}

	aPin := "owner/a@v1:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bPin := "owner/b@v1:sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if _, ok := store2.file.Dependencies[aPin]; !ok {
		t.Errorf("expected %s to survive GC, keys=%v", aPin, actionKeys(store2.file.Dependencies))
	}
	if _, ok := store2.file.Dependencies[bPin]; !ok {
		t.Errorf("expected %s to survive GC (reachable via cyclic uses:), keys=%v", bPin, actionKeys(store2.file.Dependencies))
	}
}

// TestState_SaveIsByteDeterministic guarantees Save produces byte-identical
// output for the same logical content across two independent runs.
// Dependabot diffs fetched-vs-written content and drops unchanged files, so
// any nondeterminism (map iteration order, embedded timestamps, unstable
// list ordering) either produces phantom diffs on every run or causes real
// pin changes to be silently dropped during grouped updates.
//
// If this test fails, the writer in marshalDeterministic has reintroduced
// nondeterminism. Sort all map iterations, do not embed timestamps, and
// keep list ordering stable.
func TestState_SaveIsByteDeterministic(t *testing.T) {
	// Build a non-trivial store: two workflows, multiple actions, a diamond
	// transitive dep so we exercise the parent-map / uses-list sort paths.
	deps := []dep.Dependency{
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
		store, err := LoadState(dir, fakeMetadataResolver{})
		if err != nil {
			t.Fatalf("opening store: %v", err)
		}
		if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
			t.Fatalf("Set ci.yml: %v", err)
		}
		if err := store.Set(context.Background(), ".github/workflows/release.yml", deps, parentMap, directKeys); err != nil {
			t.Fatalf("Set release.yml: %v", err)
		}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
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

func TestState_RefusesFutureVersionLockfile(t *testing.T) {
	// An old binary encountering a lockfile written by a newer binary must
	// surface the version error to the user, not silently treat the lockfile
	// as corrupt and overwrite it — that would destroy pins this binary
	// cannot interpret.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, parserlock.Path)
	if err := os.WriteFile(lockPath, []byte("version: v9.0.0\ndependencies: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadState(dir, fakeMetadataResolver{})
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

func TestState_CorruptLockfileSurfacesError(t *testing.T) {
	// A lockfile that exists but can't be parsed (here: a dependency entry
	// missing the required owner_id/repo_id keys) must surface as
	// ErrCorruptLockfile, not be silently treated as empty and overwritten.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, parserlock.Path)
	// branch + commit present, but owner_id/repo_id absent → whole-file reject.
	body := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v4:sha1-1111111111111111111111111111111111111111':\n" +
		"    branch: 'main'\n" +
		"    commit: 'sha1-1111111111111111111111111111111111111111'\n"
	if err := os.WriteFile(lockPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadState(dir, fakeMetadataResolver{})
	if err == nil {
		t.Fatal("expected error opening corrupt lockfile, got nil")
	}
	if !errors.Is(err, ErrCorruptLockfile) {
		t.Errorf("error does not match ErrCorruptLockfile sentinel: %v", err)
	}
	if errors.Is(err, parserlock.ErrFutureVersion) {
		t.Errorf("corrupt lockfile must not be classified as future-version: %v", err)
	}

	// LoadState must not delete or rewrite the file; recovery is the caller's job.
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lockfile must remain on disk after a corrupt-load error: %v", err)
	}
}
func setupClosure(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	deps := []dep.Dependency{
		{NWO: "actions/setup-go", Ref: "v6", Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1"},
		{NWO: "actions/cache", Ref: "v4", Branch: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1"},
	}
	parentMap := map[string][]string{"actions/cache@v4": {"actions/setup-go@v6"}}
	directKeys := map[string]bool{"actions/setup-go@v6": true}
	wfKey := workflowfile.KeyFromPath(filepath.Join(dir, ".github", "workflows", "ci.yml"))
	if err := store.Set(context.Background(), wfKey, deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestState_WorkflowClosure roundtrips a saved closure back through the helper
// the update engine uses to read the existing pin graph.
func TestState_WorkflowClosure(t *testing.T) {
	dir := t.TempDir()
	setupClosure(t, dir)

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	wfKey := workflowfile.KeyFromPath(filepath.Join(dir, ".github", "workflows", "ci.yml"))
	deps, parentMap, directKeys, err := store.WorkflowClosure(wfKey)
	if err != nil {
		t.Fatalf("WorkflowClosure: %v", err)
	}

	keys := make(map[string]bool, len(deps))
	for _, d := range deps {
		keys[d.Key()] = true
	}
	if !keys["actions/setup-go@v6"] || !keys["actions/cache@v4"] {
		t.Fatalf("closure missing deps, got keys=%v", keys)
	}
	if !directKeys["actions/setup-go@v6"] {
		t.Errorf("expected setup-go to be a direct pin, got %v", directKeys)
	}
	if directKeys["actions/cache@v4"] {
		t.Errorf("cache is transitive, must not be direct")
	}
	if got := parentMap["actions/cache@v4"]; len(got) != 1 || got[0] != "actions/setup-go@v6" {
		t.Errorf("expected cache parent setup-go, got %v", got)
	}
}

// resaveBumped reloads the store from disk (as `update` does), replaces the
// given workflow's closure, and saves — returning the new on-disk bytes.
func resaveBumped(t *testing.T, dir, wfKey string, deps []dep.Dependency, pm map[string][]string, direct map[string]bool) []byte {
	t.Helper()
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	if err := store.Set(context.Background(), wfKey, deps, pm, direct); err != nil {
		t.Fatalf("Set %s: %v", wfKey, err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("reading lockfile: %v", err)
	}
	return raw
}

func lineSet(b []byte) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		m[l] = true
	}
	return m
}

// TestState_BumpYieldsMinimalDiff is the writer-side guarantee behind the
// consumer's diff-hygiene requirement: bumping ONE dependency in an
// already-canonical lockfile must leave every untouched entry byte-identical.
// Only the changed dep's lines (and its workflow direct-list line) may move.
func TestState_BumpYieldsMinimalDiff(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := []dep.Dependency{
		{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/b", Ref: "v2", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Branch: "main"},
		{NWO: "owner/c", Ref: "v3", SHA: "cccccccccccccccccccccccccccccccccccccccc", HashAlgo: "sha1", Branch: "main"},
	}
	direct := map[string]bool{"owner/a@v1": true, "owner/b@v2": true, "owner/c@v3": true}

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := store.Set(context.Background(), ".github/workflows/ci.yml", base, nil, direct); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	bumped := []dep.Dependency{
		base[0],
		{NWO: "owner/b", Ref: "v6", SHA: "9999999999999999999999999999999999999999", HashAlgo: "sha1", Branch: "main"},
		base[2],
	}
	bumpedDirect := map[string]bool{"owner/a@v1": true, "owner/b@v6": true, "owner/c@v3": true}
	after := resaveBumped(t, dir, ".github/workflows/ci.yml", bumped, nil, bumpedDirect)

	beforeLines := lineSet(before)
	for _, l := range strings.Split(string(after), "\n") {
		if strings.Contains(l, "owner/a") || strings.Contains(l, "owner/c") ||
			strings.Contains(l, "aaaa") || strings.Contains(l, "cccc") {
			if !beforeLines[l] {
				t.Fatalf("untouched entry line changed: %q\n--- before ---\n%s\n--- after ---\n%s", l, before, after)
			}
		}
	}
	// Sanity: the bump actually landed.
	if strings.Contains(string(after), "owner/b@v2") {
		t.Fatalf("expected owner/b@v2 to be gone after bump, got:\n%s", after)
	}
	if !strings.Contains(string(after), "owner/b@v6:sha1-9999999999999999999999999999999999999999") {
		t.Fatalf("expected bumped owner/b@v6 pin, got:\n%s", after)
	}
}

// TestState_BumpTransitiveRemoval covers the GC side of a relock: when a bump
// drops a transitive, its lock entry is reclaimed unless another workflow still
// reaches it.
func TestState_BumpTransitiveRemoval(t *testing.T) {
	t.Run("orphaned transitive is removed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, _ := LoadState(dir, fakeMetadataResolver{})
		deps := []dep.Dependency{
			{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
			{NWO: "trans/x", Ref: "v1", SHA: "1111111111111111111111111111111111111111", HashAlgo: "sha1", Branch: "main"},
		}
		pm := map[string][]string{"trans/x@v1": {"owner/a@v1"}}
		direct := map[string]bool{"owner/a@v1": true}
		if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, pm, direct); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}

		// Bump owner/a -> v2, no longer using trans/x.
		bumped := []dep.Dependency{
			{NWO: "owner/a", Ref: "v2", SHA: "2222222222222222222222222222222222222222", HashAlgo: "sha1", Branch: "main"},
		}
		after := resaveBumped(t, dir, ".github/workflows/ci.yml", bumped, nil, map[string]bool{"owner/a@v2": true})
		if strings.Contains(string(after), "trans/x@v1") {
			t.Fatalf("expected orphaned transitive trans/x@v1 to be GC'd, got:\n%s", after)
		}
		if strings.Contains(string(after), "owner/a@v1:") {
			t.Fatalf("expected old direct owner/a@v1 to be gone, got:\n%s", after)
		}
	})

	t.Run("transitive still reachable from another workflow is preserved", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, _ := LoadState(dir, fakeMetadataResolver{})
		shared := dep.Dependency{NWO: "shared/s", Ref: "v1", SHA: "5555555555555555555555555555555555555555", HashAlgo: "sha1", Branch: "main"}
		ci := []dep.Dependency{
			{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
			shared,
		}
		rel := []dep.Dependency{
			{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Branch: "main"},
			shared,
		}
		if err := store.Set(context.Background(), ".github/workflows/ci.yml", ci, map[string][]string{"shared/s@v1": {"owner/a@v1"}}, map[string]bool{"owner/a@v1": true}); err != nil {
			t.Fatal(err)
		}
		if err := store.Set(context.Background(), ".github/workflows/release.yml", rel, map[string][]string{"shared/s@v1": {"owner/b@v1"}}, map[string]bool{"owner/b@v1": true}); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(dir, parserlock.Path))
		sharedPin := "shared/s@v1:sha1-5555555555555555555555555555555555555555"
		if !strings.Contains(string(before), sharedPin) {
			t.Fatalf("setup: expected shared pin present, got:\n%s", before)
		}

		// Bump ONLY ci.yml's owner/a -> v2, dropping the shared transitive there.
		bumped := []dep.Dependency{
			{NWO: "owner/a", Ref: "v2", SHA: "2222222222222222222222222222222222222222", HashAlgo: "sha1", Branch: "main"},
		}
		after := resaveBumped(t, dir, ".github/workflows/ci.yml", bumped, nil, map[string]bool{"owner/a@v2": true})

		if !strings.Contains(string(after), sharedPin) {
			t.Fatalf("shared transitive still used by release.yml must be preserved, got:\n%s", after)
		}
		// And preserved byte-identically (the shared entry's lines didn't move).
		beforeLines := lineSet(before)
		for _, l := range strings.Split(string(after), "\n") {
			if strings.Contains(l, "shared/s") || strings.Contains(l, "5555") || strings.Contains(l, "owner/b") || strings.Contains(l, "bbbb") {
				if !beforeLines[l] {
					t.Fatalf("untouched entry line changed: %q\n--- before ---\n%s\n--- after ---\n%s", l, before, after)
				}
			}
		}
	})

	t.Run("dropped transitive subgraph is fully removed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, _ := LoadState(dir, fakeMetadataResolver{})
		deps := []dep.Dependency{
			{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Branch: "main"},
			{NWO: "mid/m", Ref: "v1", SHA: "3333333333333333333333333333333333333333", HashAlgo: "sha1", Branch: "main"},
			{NWO: "leaf/l", Ref: "v1", SHA: "4444444444444444444444444444444444444444", HashAlgo: "sha1", Branch: "main"},
		}
		pm := map[string][]string{
			"mid/m@v1":  {"owner/a@v1"},
			"leaf/l@v1": {"mid/m@v1"},
		}
		if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, pm, map[string]bool{"owner/a@v1": true}); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}

		bumped := []dep.Dependency{
			{NWO: "owner/a", Ref: "v2", SHA: "2222222222222222222222222222222222222222", HashAlgo: "sha1", Branch: "main"},
		}
		after := resaveBumped(t, dir, ".github/workflows/ci.yml", bumped, nil, map[string]bool{"owner/a@v2": true})
		if strings.Contains(string(after), "mid/m@v1") || strings.Contains(string(after), "leaf/l@v1") {
			t.Fatalf("expected whole orphaned subgraph (mid/m, leaf/l) to be GC'd, got:\n%s", after)
		}
	})
}

// TestState_SaveFormatIsStable pins the exact canonical byte format of the
// writer. It guards the "heal" decision for `update`: diff-minimality relies on
// the on-disk lockfile already being canonical under THIS writer, so any change
// to the serialized format (sort order, quoting, field order, indentation,
// header) is a breaking change that would reformat every existing lockfile on
// its next write. If this fails intentionally, regenerate the golden and treat
// it as a format migration.
func TestState_SaveFormatIsStable(t *testing.T) {
	const golden = "# This file is machine-generated by `gh actions-pin`.\n" +
		"# Do not edit by hand; run `gh actions-pin` to update.\n" +
		"# Docs: https://gh.io/actions-lockfile\n" +
		"version: 'v0.0.1'\n" +
		"workflows:\n" +
		"    '.github/workflows/ci.yml':\n" +
		"        - 'actions/checkout@v4:sha1-11111111111111111111111111111111111111aa'\n" +
		"        - 'actions/setup-go@v5:sha1-22222222222222222222222222222222222222bb'\n" +
		"dependencies:\n" +
		"    'actions/checkout@v4:sha1-11111111111111111111111111111111111111aa':\n" +
		"        tag: 'v4'\n" +
		"        branch: 'main'\n" +
		"        commit: 'sha1-11111111111111111111111111111111111111aa'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"        uses:\n" +
		"            - 'shared/dep@v1:sha1-33333333333333333333333333333333333333cc'\n" +
		"    'actions/setup-go@v5:sha1-22222222222222222222222222222222222222bb':\n" +
		"        branch: 'main'\n" +
		"        commit: 'sha1-22222222222222222222222222222222222222bb'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"    'shared/dep@v1:sha1-33333333333333333333333333333333333333cc':\n" +
		"        branch: 'main'\n" +
		"        commit: 'sha1-33333333333333333333333333333333333333cc'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n"

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "11111111111111111111111111111111111111aa", HashAlgo: "sha1", Tag: "v4", Branch: "main"},
		{NWO: "actions/setup-go", Ref: "v5", SHA: "22222222222222222222222222222222222222bb", HashAlgo: "sha1", Branch: "main"},
		{NWO: "shared/dep", Ref: "v1", SHA: "33333333333333333333333333333333333333cc", HashAlgo: "sha1", Branch: "main"},
	}
	pm := map[string][]string{"shared/dep@v1": {"actions/checkout@v4"}}
	direct := map[string]bool{"actions/checkout@v4": true, "actions/setup-go@v5": true}
	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, pm, direct); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != golden {
		t.Fatalf("serialized lockfile format drifted from golden.\n--- got ---\n%s\n--- want ---\n%s", raw, golden)
	}
}
