package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
)

type fakeMetadataResolver struct{}

func (fakeMetadataResolver) RepoIDs(_ context.Context, owner, repo string) (int64, int64, error) {
	return 1, 2, nil
}

func TestState_PersistsRef(t *testing.T) {
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

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, nil, nil); err != nil {
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
	// Tag dep should serialize ref as the tag value (tag > branch).
	for _, want := range []string{
		"'actions/checkout@v4.2.1':",
		"ref: 'v4.2.1'",
		"'internal/branch-only@main':",
		"ref: 'main'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lockfile missing %q\n--- contents ---\n%s", want, got)
		}
	}

	// The lockfile should NOT contain separate tag:/branch: fields.
	if strings.Contains(got, "tag:") {
		t.Errorf("lockfile should not contain tag: field\n%s", got)
	}
	if strings.Contains(got, "branch:") {
		t.Errorf("lockfile should not contain branch: field\n%s", got)
	}

	// Reload and verify roundtrip.
	store2, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	checkoutKey := "actions/checkout@v4.2.1"
	a, ok := store2.file.Dependencies[checkoutKey]
	if !ok {
		t.Fatalf("expected %s in reloaded lockfile, keys=%v", checkoutKey, actionKeys(store2.file.Dependencies))
	}
	if a.Ref != "v4.2.1" {
		t.Errorf("expected Ref=v4.2.1, got %q", a.Ref)
	}
	branchOnlyKey := "internal/branch-only@main"
	b, ok := store2.file.Dependencies[branchOnlyKey]
	if !ok {
		t.Fatalf("expected %s, keys=%v", branchOnlyKey, actionKeys(store2.file.Dependencies))
	}
	if b.Ref != "main" {
		t.Errorf("expected Ref=main, got %q", b.Ref)
	}
}

func actionKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestState_SetAcceptsEmptyBranch verifies that deps with no discovered
// branch are accepted — the new schema makes ref optional.
func TestState_SetAcceptsEmptyBranch(t *testing.T) {
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
		},
	}

	err = store.Set(context.Background(), ".github/workflows/ci.yml", deps, nil, nil)
	if err != nil {
		t.Fatalf("Set should accept dep with empty Branch, got: %v", err)
	}
}

// TestState_SetPreservesRefForUnchangedPin reproduces the write-path where
// adding a new action to an already-tracked workflow must not lose the ref
// for carried (Verified) deps that arrive without Tag/Branch from the read
// path. Set must fall back to the ref already recorded on disk.
func TestState_SetPreservesRefForUnchangedPin(t *testing.T) {
	dir := t.TempDir()
	wfKey := ".github/workflows/ci.yml"

	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	checkout := dep.Dependency{
		NWO: "actions/checkout", Ref: "v4", Tag: "v4", Branch: "main",
		SHA: "abc123abc123abc123abc123abc123abc123abc1", HashAlgo: "sha1",
	}
	if err := store.Set(context.Background(), wfKey, []dep.Dependency{checkout}, nil, nil); err != nil {
		t.Fatalf("initial Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}

	// Reload from disk and re-Set with the existing checkout dep arriving
	// branchless (the Verified read-path shape) plus a genuinely new dep.
	store2, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	carriedCheckout := dep.Dependency{
		NWO: "actions/checkout", Ref: "v4",
		SHA: "abc123abc123abc123abc123abc123abc123abc1", HashAlgo: "sha1",
		// Branch/Tag intentionally empty — dropped by the read path.
	}
	newSetupGo := dep.Dependency{
		NWO: "actions/setup-go", Ref: "v5", Tag: "v5", Branch: "main",
		SHA: "def456def456def456def456def456def456def4", HashAlgo: "sha1",
	}
	if err := store2.Set(context.Background(), wfKey, []dep.Dependency{carriedCheckout, newSetupGo}, nil, nil); err != nil {
		t.Fatalf("re-Set with carried branchless dep should succeed, got: %v", err)
	}
	if err := store2.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	store3, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	checkoutKey := "actions/checkout@v4"
	a, ok := store3.file.Dependencies[checkoutKey]
	if !ok {
		t.Fatalf("expected %s preserved, keys=%v", checkoutKey, actionKeys(store3.file.Dependencies))
	}
	if a.Ref != "v4" {
		t.Errorf("expected preserved Ref=v4 for unchanged pin, got %q", a.Ref)
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
	sharedPin := "shared/dep@v1"

	// Both A and B should have uses: containing the shared dep.
	aPin := "owner/a@v1"
	bPin := "owner/b@v1"

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
// cycle (A uses B, B uses A). Both entries should be retained in the write.
// The new parser rejects cycles on reload, so we verify Save doesn't hang
// and the cycle is caught on re-parse.
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
	directKeys := map[string]bool{
		"owner/a@v1": true,
	}

	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Save should not hang (GC walk terminates despite cycle).
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The new parser rejects cycles on reload.
	_, err = LoadState(dir, fakeMetadataResolver{})
	if err == nil {
		t.Fatal("expected parser to reject cyclic uses on reload")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
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
	if !strings.Contains(err.Error(), "gh extension upgrade gh-actions-lock") {
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
	if err := store.Set(context.Background(), ".github/workflows/ci.yml", deps, parentMap, directKeys); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
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
	if !strings.Contains(string(after), "owner/b@v6") {
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
		sharedPin := "shared/s@v1"
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
	const golden = "# This file is machine-generated by `gh actions-lock`.\n" +
		"# Do not edit by hand; run `gh actions-lock` to update.\n" +
		"# Docs: https://gh.io/actions-lockfile\n" +
		"version: '" + parserlock.Version + "'\n" +
		"workflows:\n" +
		"    '.github/workflows/ci.yml':\n" +
		"        - 'actions/checkout@v4'\n" +
		"        - 'actions/setup-go@v5'\n" +
		"dependencies:\n" +
		"    'actions/checkout@v4':\n" +
		"        ref: 'v4'\n" +
		"        commit: 'sha1-11111111111111111111111111111111111111aa'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"        uses:\n" +
		"            - 'shared/dep@v1'\n" +
		"    'actions/setup-go@v5':\n" +
		"        ref: 'v5'\n" +
		"        commit: 'sha1-22222222222222222222222222222222222222bb'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"    'shared/dep@v1':\n" +
		"        ref: 'v1'\n" +
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

// TestState_TransitiveClosureGolden is a golden test for the full
// Set→Save→serialize path with a complex multi-workflow transitive graph.
//
// Graph:
//
//	workflow-a.yml                       workflow-b.yml         workflow-c.yml
//	  ├─ checkout@v4                       ├─ checkout@v4         └─ leaf@main
//	  ├─ composite-a@v1                    └─ composite-b@v2
//	  │    ├─ composite-b@v2 ──(diamond)       (direct, no children —
//	  │    │    └─ leaf@main                    resolved via node subpath)
//	  │    └─ external@v1
//	  └─ composite-c@v1
//	       └─ composite-b@v2 ──(diamond)
//	            └─ leaf@main
//
// Exercises: diamond deps, depth-3 transitive, direct+transitive overlap
// (composite-b is transitive-parent in wf-a, direct-leaf in wf-b),
// uses-merge clobber (wf-b's Set must not erase composite-b's uses:[leaf]),
// leaf clobber (leaf@main direct in wf-c), cross-org transitive (external),
// and GC survival of deep transitive deps.
func TestState_TransitiveClosureGolden(t *testing.T) {
	const golden = "# This file is machine-generated by `gh actions-lock`.\n" +
		"# Do not edit by hand; run `gh actions-lock` to update.\n" +
		"# Docs: https://gh.io/actions-lockfile\n" +
		"version: '" + parserlock.Version + "'\n" +
		"workflows:\n" +
		"    '.github/workflows/workflow-a.yml':\n" +
		"        - 'actions/checkout@v4'\n" +
		"        - 'my-org/composite-a@v1'\n" +
		"        - 'my-org/composite-c@v1'\n" +
		"    '.github/workflows/workflow-b.yml':\n" +
		"        - 'actions/checkout@v4'\n" +
		"        - 'my-org/composite-b@v2'\n" +
		"    '.github/workflows/workflow-c.yml':\n" +
		"        - 'my-org/leaf@main'\n" +
		"dependencies:\n" +
		"    'actions/checkout@v4':\n" +
		"        ref: 'v4'\n" +
		"        commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"    'my-org/composite-a@v1':\n" +
		"        ref: 'v1'\n" +
		"        commit: 'sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"        uses:\n" +
		"            - 'my-org/composite-b@v2'\n" +
		"            - 'other-org/external@v1'\n" +
		"    'my-org/composite-b@v2':\n" +
		"        ref: 'v2'\n" +
		"        commit: 'sha1-cccccccccccccccccccccccccccccccccccccccc'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"        uses:\n" +
		"            - 'my-org/leaf@main'\n" +
		"    'my-org/composite-c@v1':\n" +
		"        ref: 'v1'\n" +
		"        commit: 'sha1-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"        uses:\n" +
		"            - 'my-org/composite-b@v2'\n" +
		"    'my-org/leaf@main':\n" +
		"        ref: 'main'\n" +
		"        commit: 'sha1-dddddddddddddddddddddddddddddddddddddddd'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n" +
		"    'other-org/external@v1':\n" +
		"        ref: 'v1'\n" +
		"        commit: 'sha1-ffffffffffffffffffffffffffffffffffffffff'\n" +
		"        owner_id: 1\n" +
		"        repo_id: 2\n"

	checkout := dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1", Tag: "v4", Branch: "main"}
	compositeA := dep.Dependency{NWO: "my-org/composite-a", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1", Tag: "v1", Branch: "main"}
	compositeB := dep.Dependency{NWO: "my-org/composite-b", Ref: "v2", SHA: "cccccccccccccccccccccccccccccccccccccccc", HashAlgo: "sha1", Tag: "v2", Branch: "main"}
	leaf := dep.Dependency{NWO: "my-org/leaf", Ref: "main", SHA: "dddddddddddddddddddddddddddddddddddddddd", HashAlgo: "sha1", Branch: "main"}
	compositeC := dep.Dependency{NWO: "my-org/composite-c", Ref: "v1", SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", HashAlgo: "sha1", Tag: "v1", Branch: "main"}
	external := dep.Dependency{NWO: "other-org/external", Ref: "v1", SHA: "ffffffffffffffffffffffffffffffffffffffff", HashAlgo: "sha1", Tag: "v1", Branch: "main"}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := LoadState(dir, fakeMetadataResolver{})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	ctx := context.Background()

	// workflow-a: checkout (direct), composite-a (direct) → composite-b + external,
	//            composite-c (direct) → composite-b → leaf
	if err := store.Set(ctx, ".github/workflows/workflow-a.yml",
		[]dep.Dependency{checkout, compositeA, compositeB, leaf, compositeC, external},
		map[string][]string{
			"my-org/composite-b@v2": {"my-org/composite-a@v1", "my-org/composite-c@v1"},
			"other-org/external@v1": {"my-org/composite-a@v1"},
			"my-org/leaf@main":      {"my-org/composite-b@v2"},
		},
		map[string]bool{
			"actions/checkout@v4":   true,
			"my-org/composite-a@v1": true,
			"my-org/composite-c@v1": true,
		},
	); err != nil {
		t.Fatalf("Set workflow-a: %v", err)
	}

	// workflow-b: checkout (direct), composite-b (direct, no children).
	// This models the real-world scenario: same NWO@Ref resolved via a
	// node-action subpath that has no composite expansion, so planWorkflow
	// produces no parentMap edges. Set must not clobber composite-b's
	// uses:[leaf] from workflow-a.
	if err := store.Set(ctx, ".github/workflows/workflow-b.yml",
		[]dep.Dependency{checkout, compositeB},
		nil,
		map[string]bool{
			"actions/checkout@v4":   true,
			"my-org/composite-b@v2": true,
		},
	); err != nil {
		t.Fatalf("Set workflow-b: %v", err)
	}

	// workflow-c: leaf (direct, no transitive deps).
	// This is the clobber scenario — leaf has no children here, must not
	// erase composite-b's uses: [leaf] from prior Set calls.
	if err := store.Set(ctx, ".github/workflows/workflow-c.yml",
		[]dep.Dependency{leaf},
		nil,
		map[string]bool{"my-org/leaf@main": true},
	); err != nil {
		t.Fatalf("Set workflow-c: %v", err)
	}

	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != golden {
		t.Fatalf("lockfile content drifted from golden.\n--- got ---\n%s\n--- want ---\n%s", raw, golden)
	}
}
