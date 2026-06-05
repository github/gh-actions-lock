package doctor

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
)

// TestDedupePinsByPath guards the parallel-pin-pool UX fix: submitPin runs
// once per finding, so a workflow with multiple findings (e.g. three
// SHA-as-ref entries that each take the applySHAToTag → submitPin path) can
// end up enqueued multiple times. Without this dedupe the pool would launch
// concurrent workers on the same file — wasted work, racy file writes, and
// stacked sub-spinner rows for one workflow.
func TestDedupePinsByPath(t *testing.T) {
	pins := []WorkflowReport{
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/secret-rotation.yml"},
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/codeql.yml"},
		{Path: ".github/workflows/release.yml"},
		{Path: ".github/workflows/secret-rotation.yml"},
	}

	got := dedupePinsByPath(pins)

	want := []string{
		".github/workflows/codeql.yml",
		".github/workflows/secret-rotation.yml",
		".github/workflows/release.yml",
	}
	if len(got) != len(want) {
		t.Fatalf("dedupePinsByPath returned %d pins, want %d (%v)", len(got), len(want), got)
	}
	for i, p := range got {
		if p.Path != want[i] {
			t.Fatalf("pin[%d].Path = %q, want %q (full list: %v)", i, p.Path, want[i], got)
		}
	}
}

func TestDedupePinsByPathEmpty(t *testing.T) {
	if got := dedupePinsByPath(nil); len(got) != 0 {
		t.Fatalf("dedupePinsByPath(nil) = %v, want empty", got)
	}
}

// TestSubmitPin_LastWinsReplacement is the load-bearing guarantee for the
// deferred-pin flow: when applySHAToTag rewrites a workflow file multiple
// times in Pass A (one per SHA-as-ref finding), each call re-extracts the
// post-rewrite ActionRefs and re-submits. The pinpool worker must see the
// LATEST submission so its resolver call runs against the fully-rewritten
// refs — earlier submissions hold stale refs (pre-rewrite of subsequent
// findings) and would mis-resolve to the SHA we rewrote away from.
func TestSubmitPin_LastWinsReplacement(t *testing.T) {
	rem := &Remediator{deferPins: true}

	refsV1 := []lockfile.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v1"}}
	refsV2 := []lockfile.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v2"}}
	refsV3 := []lockfile.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v3"}}

	if err := rem.submitPin(WorkflowReport{Path: "a.yml", ActionRefs: refsV1}); err != nil {
		t.Fatalf("submitPin v1: %v", err)
	}
	if err := rem.submitPin(WorkflowReport{Path: "b.yml", ActionRefs: refsV1}); err != nil {
		t.Fatalf("submitPin b.yml: %v", err)
	}
	if err := rem.submitPin(WorkflowReport{Path: "a.yml", ActionRefs: refsV2}); err != nil {
		t.Fatalf("submitPin v2: %v", err)
	}
	if err := rem.submitPin(WorkflowReport{Path: "a.yml", ActionRefs: refsV3}); err != nil {
		t.Fatalf("submitPin v3: %v", err)
	}

	if got, want := len(rem.pendingPins), 2; got != want {
		t.Fatalf("len(pendingPins) = %d, want %d (%v)", got, want, rem.pendingPins)
	}

	// a.yml should hold the LAST submission's refs (v3), not the first.
	// b.yml should be unaffected.
	for _, p := range rem.pendingPins {
		switch p.Path {
		case "a.yml":
			if len(p.ActionRefs) != 1 || p.ActionRefs[0].Ref != "v3" {
				t.Fatalf("a.yml.ActionRefs = %v, want [v3]", p.ActionRefs)
			}
		case "b.yml":
			if len(p.ActionRefs) != 1 || p.ActionRefs[0].Ref != "v1" {
				t.Fatalf("b.yml.ActionRefs = %v, want [v1]", p.ActionRefs)
			}
		default:
			t.Fatalf("unexpected pending path %q", p.Path)
		}
	}

	// First-submitted path wins ordering, even when later submissions
	// replace its refs. Pass B drains in insertion order so this keeps
	// the user-visible worker rows in the order workflows first appeared.
	if rem.pendingPins[0].Path != "a.yml" {
		t.Fatalf("insertion order broken: pendingPins[0].Path = %q, want a.yml", rem.pendingPins[0].Path)
	}
}

// TestSubmitPin_PreservesVirtualRefs guards risk E from the design review:
// handleNotPinned passes wr.ActionRefs that have been MUTATED by
// offerDefaultBranch (a bare SHA swapped to the default branch ref) without
// touching the workflow file on disk. The deferred-pin path must NOT
// re-read from disk — that would discard the virtual ref and pin the
// original SHA. submitPin trusts caller-owned refs by construction.
func TestSubmitPin_PreservesVirtualRefs(t *testing.T) {
	rem := &Remediator{deferPins: true}

	// Caller-owned refs that don't match what's on disk — the typical
	// shape after offerDefaultBranch rewrites a bare-SHA internal ref
	// to the default branch.
	virtual := []lockfile.ActionRef{{Owner: "myorg", Repo: "actions-lib", Ref: "main", Raw: "myorg/actions-lib@main"}}

	if err := rem.submitPin(WorkflowReport{Path: "ci.yml", ActionRefs: virtual}); err != nil {
		t.Fatalf("submitPin: %v", err)
	}

	if len(rem.pendingPins) != 1 {
		t.Fatalf("len(pendingPins) = %d, want 1", len(rem.pendingPins))
	}
	got := rem.pendingPins[0].ActionRefs
	if len(got) != 1 || got[0].Ref != "main" {
		t.Fatalf("pendingPins[0].ActionRefs = %v, want [{… main}]", got)
	}
}

// TestSubmitPin_SyncFallthrough confirms the non-deferred path still calls
// applyPin synchronously. We can't run applyPin without a resolver so we
// just assert pendingPins stays empty (and applyPin's missing dependencies
// surface as a panic, which the test catches). The point is the gate flips
// correctly on deferPins.
func TestSubmitPin_SyncFallthrough(t *testing.T) {
	rem := &Remediator{deferPins: false}
	defer func() {
		_ = recover() // applyPin will panic on the nil store/resolver/output
	}()
	_ = rem.submitPin(WorkflowReport{Path: "ci.yml"})
	if len(rem.pendingPins) != 0 {
		t.Fatalf("sync path should not enqueue; got %d pending", len(rem.pendingPins))
	}
}

// TestApplySHAToTag_DefersInsteadOfSyncResolve is the end-to-end guard for
// the heavy-phase UX fix: applySHAToTag must perform the cheap file rewrite
// + state recording in Pass A, then hand the resolve/normalize/pin to the
// pinpool worker via submitPin. The pre-refactor implementation ran
// ResolveAllRecursive inline here, which is what froze the spinner on
// SHA-as-ref-heavy repos like vercel/next.js.
//
// We construct a Remediator with deferPins=true, a real workflow on disk, a
// nil resolver, and a SHA-as-ref dep. If the function still tries to resolve
// synchronously it will panic on the nil resolver; the test asserts:
//   - no panic (proves no synchronous resolver call)
//   - the file on disk was rewritten from @SHA to @tag
//   - exactly one entry landed in pendingPins with refs reflecting the
//     post-rewrite state
//   - Fixed counter was NOT incremented in Pass A (the deferred applyPin
//     owns that increment; if both incremented we'd double-count).
func TestApplySHAToTag_DefersInsteadOfSyncResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	const sha = "1111111111111111111111111111111111111111"
	const yamlSrc = "name: ci\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@" + sha + "\n"
	if err := os.WriteFile(path, []byte(yamlSrc), 0o644); err != nil {
		t.Fatalf("seeding workflow: %v", err)
	}

	rem := &Remediator{
		deferPins: true,
		output:    ui.NewPlain(io.Discard),
		state:     newSessionState(),
	}

	dep := &lockfile.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha}
	if err := rem.applySHAToTag(WorkflowReport{Path: path}, dep, "actions", "checkout", "v4"); err != nil {
		t.Fatalf("applySHAToTag: %v", err)
	}

	// File on disk should now reference @v4, not @SHA.
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading workflow: %v", err)
	}
	if want := "actions/checkout@v4"; !contains(string(rewritten), want) {
		t.Fatalf("rewritten file missing %q:\n%s", want, rewritten)
	}
	if contains(string(rewritten), "actions/checkout@"+sha) {
		t.Fatalf("rewritten file still contains old SHA:\n%s", rewritten)
	}

	// Deferred: exactly one pending pin, refs reflect the new tag.
	if len(rem.pendingPins) != 1 {
		t.Fatalf("len(pendingPins) = %d, want 1", len(rem.pendingPins))
	}
	pp := rem.pendingPins[0]
	if pp.Path != path {
		t.Fatalf("pendingPins[0].Path = %q, want %q", pp.Path, path)
	}
	if len(pp.ActionRefs) != 1 || pp.ActionRefs[0].Ref != "v4" {
		t.Fatalf("pendingPins[0].ActionRefs = %v, want [{… v4}]", pp.ActionRefs)
	}

	// Fixed accounting is owned by the deferred applyPin (Pass B) so
	// Pass A's apply step must leave the counter alone. Otherwise a
	// SHA-as-ref workflow with N findings would double-count
	// (N rewrites in Pass A + 1 applyPin in Pass B = N+1 fixed).
	if rem.Fixed != 0 {
		t.Fatalf("Fixed = %d after deferred Pass A, want 0 (incFixed belongs to Pass B applyPin)", rem.Fixed)
	}
}

// TestApplySHAToTag_NoMatchSkips covers the warning path where the rewrite
// targets a uses: line that doesn't exist in the workflow (e.g. the
// diagnose-time dep has drifted). Nothing should be enqueued; Skipped goes
// up; no error.
func TestApplySHAToTag_NoMatchSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	if err := os.WriteFile(path, []byte("name: ci\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rem := &Remediator{
		deferPins: true,
		output:    ui.NewPlain(io.Discard),
		state:     newSessionState(),
	}

	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	dep := &lockfile.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha}
	if err := rem.applySHAToTag(WorkflowReport{Path: path}, dep, "actions", "checkout", "v5"); err != nil {
		t.Fatalf("applySHAToTag: %v", err)
	}

	if len(rem.pendingPins) != 0 {
		t.Fatalf("no-match path should not enqueue; got %d pending", len(rem.pendingPins))
	}
	if rem.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", rem.Skipped)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestMergeEnrichmentForAlert_NoSaneRelease covers the impostor-without-
// suggestion shape that was visibly regressing on next.js: a tag-pinned
// uses: line resolves to an unreachable SHA, EnrichImpostorFindings ran
// but found no sane release. Apply.go's reach loop calls alertWorkflow
// (bare path); without mergeEnrichmentForAlert the renderer would skip
// the "→ no recent release was reachable — escalate" line, losing the
// actionable signal users had under the pre-c58919b binary.
func TestMergeEnrichmentForAlert_NoSaneRelease(t *testing.T) {
	rem := &Remediator{}
	findings := []Finding{
		{
			Category:               CategoryImpostorCommit,
			Dependency:             &lockfile.Dependency{NWO: "mmastrac/mmm-matrix", Ref: "v1.0.5", SHA: "3edd85c30addba11887c770740309c979a446aa9"},
			SaneSuggestionSearched: true,
			// SaneSuggestionTag deliberately empty — the "looked, found nothing" shape
		},
	}

	rem.mergeEnrichmentForAlert(findings, "mmastrac/mmm-matrix", "v1.0.5")

	depKey := "mmastrac/mmm-matrix@v1.0.5"
	if !rem.AlertedSearched[depKey] {
		t.Fatalf("AlertedSearched[%q] = false, want true (renderer needs this for the escalate line)", depKey)
	}
	if got, ok := rem.AlertedSuggestions[depKey]; ok {
		t.Fatalf("AlertedSuggestions[%q] = %q, want unset (no sane tag found)", depKey, got)
	}
}

// TestMergeEnrichmentForAlert_SaneRelease covers the impostor-with-suggestion
// shape: an unreachable tag has a sibling sane release that EnrichImpostorFindings
// found. The merged alert must carry the suggestion so the renderer prints
// "→ suggested: re-pin to <tag>" instead of falling back to the escalate line.
func TestMergeEnrichmentForAlert_SaneRelease(t *testing.T) {
	rem := &Remediator{}
	findings := []Finding{
		{
			Category:               CategoryImpostorCommit,
			Dependency:             &lockfile.Dependency{NWO: "foo/bar", Ref: "v1.0.5", SHA: "deadbeef00000000000000000000000000000000"},
			SaneSuggestionSearched: true,
			SaneSuggestionTag:      "v2.0.0",
			SaneSuggestionSHA:      "abc1234defabc1234defabc1234defabc1234def",
		},
	}

	rem.mergeEnrichmentForAlert(findings, "foo/bar", "v1.0.5")

	depKey := "foo/bar@v1.0.5"
	if !rem.AlertedSearched[depKey] {
		t.Fatalf("AlertedSearched[%q] = false, want true", depKey)
	}
	want := "v2.0.0 abc1234"
	if got := rem.AlertedSuggestions[depKey]; got != want {
		t.Fatalf("AlertedSuggestions[%q] = %q, want %q", depKey, got, want)
	}
}

// TestMergeEnrichmentForAlert_NoMatch confirms the helper is a no-op when no
// matching impostor finding exists (e.g. the alertWorkflow call was for a
// non-impostor reach failure like ReachabilityUnknown). The registry must
// stay empty so the renderer doesn't show a phantom escalate line.
func TestMergeEnrichmentForAlert_NoMatch(t *testing.T) {
	rem := &Remediator{}
	findings := []Finding{
		{
			Category:               CategoryImpostorCommit,
			Dependency:             &lockfile.Dependency{NWO: "different/action", Ref: "v1", SHA: "deadbeef00000000000000000000000000000000"},
			SaneSuggestionSearched: true,
		},
	}

	rem.mergeEnrichmentForAlert(findings, "foo/bar", "v1.0.5")

	if len(rem.AlertedSearched) != 0 {
		t.Fatalf("AlertedSearched = %v, want empty (no matching finding)", rem.AlertedSearched)
	}
	if len(rem.AlertedSuggestions) != 0 {
		t.Fatalf("AlertedSuggestions = %v, want empty", rem.AlertedSuggestions)
	}
}

// TestMergeEnrichmentForAlert_NotSearched proves the helper skips findings
// that haven't been through EnrichImpostorFindings yet — recordAlertSuggestion
// is a no-op for those, but we don't want the helper to record them either,
// because rendering depends on SaneSuggestionSearched semantics ("looked and
// found nothing" vs "didn't look"). A finding that hasn't been enriched
// shouldn't produce an AlertedSearched entry that misleads the renderer.
func TestMergeEnrichmentForAlert_NotSearched(t *testing.T) {
	rem := &Remediator{}
	findings := []Finding{
		{
			Category:               CategoryImpostorCommit,
			Dependency:             &lockfile.Dependency{NWO: "foo/bar", Ref: "v1.0.5", SHA: "deadbeef00000000000000000000000000000000"},
			SaneSuggestionSearched: false,
		},
	}

	rem.mergeEnrichmentForAlert(findings, "foo/bar", "v1.0.5")

	if len(rem.AlertedSearched) != 0 {
		t.Fatalf("AlertedSearched = %v, want empty (finding wasn't enriched)", rem.AlertedSearched)
	}
}

// TestMergeEnrichmentForAlert_NilFindings exercises the
// normalizeAndRewrite-from-applyReResolve case where callers may pass nil
// findings (or an empty slice). The helper must be a clean no-op without
// panicking; the alert just stays bare in that case.
func TestMergeEnrichmentForAlert_NilFindings(t *testing.T) {
	rem := &Remediator{}
	rem.mergeEnrichmentForAlert(nil, "foo/bar", "v1.0.5")
	rem.mergeEnrichmentForAlert([]Finding{}, "foo/bar", "v1.0.5")

	if len(rem.AlertedSearched) != 0 {
		t.Fatalf("AlertedSearched = %v, want empty", rem.AlertedSearched)
	}
}

// TestMergeEnrichmentForAlert_SkipsNonImpostor guards against accidentally
// folding enrichment from a non-impostor finding (e.g. a CategoryRefMoved
// finding that happens to share NWO+Ref with the alerted dep). Only
// CategoryImpostorCommit findings carry the sane-release search semantics.
func TestMergeEnrichmentForAlert_SkipsNonImpostor(t *testing.T) {
	rem := &Remediator{}
	findings := []Finding{
		{
			Category:               CategoryRefMoved,
			Dependency:             &lockfile.Dependency{NWO: "foo/bar", Ref: "v1.0.5"},
			SaneSuggestionSearched: true, // contrived: shouldn't be set on non-impostor anyway
		},
	}

	rem.mergeEnrichmentForAlert(findings, "foo/bar", "v1.0.5")

	if len(rem.AlertedSearched) != 0 {
		t.Fatalf("AlertedSearched = %v, want empty (non-impostor category)", rem.AlertedSearched)
	}
}
