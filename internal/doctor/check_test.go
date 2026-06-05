package doctor

import (
	"sort"
	"testing"

	"github.com/github/gh-actions-pin/internal/resolver"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// Typed map keys for the test stub. These mirror the shape used by
// internal/cachekey: a small struct per lookup tuple so callers can't
// silently drift on delimiter choice or normalization. Test-internal,
// so we don't need cachekey's full normalization machinery — fields
// are passed lowercase by callers (or naturally lowercase, like SHAs).
type (
	stubRefKey       struct{ owner, repo, ref string }
	stubAncestryKey  struct{ owner, repo, cand, head string }
	stubReachKey     struct{ owner, repo, sha, ref string }
	stubTagObjectKey struct{ owner, repo, sha string }
)

// stubCheckResolver scripts every checkResolver call from test fixtures.
// Missing entries return *Unknown values (fail-open semantics).
type stubCheckResolver struct {
	refs       map[stubRefKey]string                        // resolved ref → sha; absence = unknown
	ancestry   map[stubAncestryKey]resolver.AncestryStatus  // (cand, head) ancestry decision
	reach      map[stubReachKey]resolver.ReachabilityStatus // sha-reachable-from-ref decision
	tagObjects map[stubTagObjectKey]string                  // sha → peeled commit
}

func (s *stubCheckResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	if s == nil {
		return "", false
	}
	sha, ok := s.refs[stubRefKey{owner, repo, ref}]
	return sha, ok
}

func (s *stubCheckResolver) CheckAncestry(owner, repo, cand, head string) resolver.AncestryStatus {
	if s == nil {
		return resolver.AncestryUnknown
	}
	v, ok := s.ancestry[stubAncestryKey{owner, repo, cand, head}]
	if !ok {
		return resolver.AncestryUnknown
	}
	return v
}

func (s *stubCheckResolver) CheckReachability(owner, repo, sha, ref string) resolver.ReachabilityStatus {
	if s == nil {
		return resolver.ReachabilityUnknown
	}
	v, ok := s.reach[stubReachKey{owner, repo, sha, ref}]
	if !ok {
		return resolver.ReachabilityUnknown
	}
	return v
}

func (s *stubCheckResolver) PeelTagObject(owner, repo, sha string) (string, bool) {
	if s == nil {
		return "", false
	}
	commit, ok := s.tagObjects[stubTagObjectKey{owner, repo, sha}]
	return commit, ok
}

const (
	shaCheckoutV4 = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	shaCheckoutV3 = "11bd71901bbe5b1630ceea73d27597364c9af683"
	shaSetupGoV5  = "0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaImpostor   = "ffffffffffffffffffffffffffffffffffffffff"
)

func checkPinKey(owner, repo, ref, sha string) string {
	return owner + "/" + repo + "@" + ref + ":sha1-" + sha
}

func checkNewLockfile(workflows map[string][]string) parserlock.File {
	return parserlock.File{
		Version:   parserlock.Version,
		Workflows: workflows,
	}
}

func checkParsedWF(path string, uses ...parserlock.ActionRef) ParsedWorkflow {
	return ParsedWorkflow{Path: path, Refs: uses}
}

func checkRef(owner, repo, ref string) parserlock.ActionRef {
	return parserlock.ActionRef{Owner: owner, Repo: repo, Ref: ref}
}

func findingCategories(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f.Category))
	}
	sort.Strings(out)
	return out
}

// TestRunChecks groups the per-category runChecks integration cases that
// share the same lockfile + parsed-workflow + resolver setup pattern.
// Cases that need to assert non-category aspects (Severity, ObservedSHA,
// Dependency.SHA, Confidence) hang those off the optional `extra` hook.
//
// Tests that are NOT table-driven below intentionally:
//   - TestRunChecks_AllFindingsCarryConfidence is a structural fail-fast
//     guard that exercises every check path in one fixture; collapsing
//     it into a row would dilute its purpose.
func TestRunChecks(t *testing.T) {
	const wfPath = ".github/workflows/ci.yml"

	cases := []struct {
		name           string
		lockfile       map[string][]string
		workflowRefs   []parserlock.ActionRef
		resolver       *stubCheckResolver
		noResolver     bool // when true, runChecks gets nil instead of resolver
		wantCategories []Category
		// extra runs after category assertions for case-specific checks
		// (Severity, ObservedSHA, Dependency.SHA, Confidence, etc).
		extra func(t *testing.T, got []Finding)
	}{
		{
			name:           "not-pinned: ref used but absent from lockfile",
			lockfile:       map[string][]string{},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			wantCategories: []Category{CategoryNotPinned},
			extra: func(t *testing.T, got []Finding) {
				if got[0].Severity != SeverityError {
					t.Fatalf("expected error severity, got %s", got[0].Severity)
				}
			},
		},
		{
			name: "sha-as-ref: workflow ref is a bare SHA",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", shaCheckoutV4, shaCheckoutV4)},
			},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", shaCheckoutV4)},
			wantCategories: []Category{CategorySHAAsRef},
		},
		{
			name: "ref-changed + stale: lockfile pins v4, workflow uses v3",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
			},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", "v3")},
			wantCategories: []Category{CategoryRefChanged, CategoryStale},
			extra: func(t *testing.T, got []Finding) {
				var refChanged *Finding
				for i := range got {
					if got[i].Category == CategoryRefChanged {
						refChanged = &got[i]
					}
				}
				if refChanged == nil || refChanged.Dependency == nil || refChanged.Dependency.SHA != shaCheckoutV4 {
					t.Fatalf("expected ref-changed with locked sha %s, got %#v", shaCheckoutV4, refChanged)
				}
			},
		},
		{
			name: "stale: lockfile pins ref no workflow uses",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
			},
			workflowRefs:   nil,
			wantCategories: []Category{CategoryStale},
		},
		{
			name: "no findings: pinned and current with reachable sha",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				reach: map[stubReachKey]resolver.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV4, "v4"}: resolver.Reachable,
				},
			},
			wantCategories: nil,
		},
		{
			name: "ref-moved: pinned sha drifted but ancestry confirms",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolver.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolver.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolver.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolver.Reachable,
				},
			},
			wantCategories: []Category{CategoryRefMoved},
			extra: func(t *testing.T, got []Finding) {
				if got[0].ObservedSHA != shaCheckoutV4 || got[0].Dependency == nil || got[0].Dependency.SHA != shaCheckoutV3 {
					t.Fatalf("unexpected sha pair on finding: %#v", got[0])
				}
			},
		},
		{
			name: "lockfile-forgery: pinned sha is not an ancestor of upstream",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaImpostor)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolver.AncestryStatus{
					{"actions", "checkout", shaImpostor, shaCheckoutV4}: resolver.AncestryNotAncestor,
				},
			},
			extra: func(t *testing.T, got []Finding) {
				hasForgery := false
				for _, f := range got {
					if f.Category == CategoryLockfileForgery {
						hasForgery = true
						if f.Severity != SeverityError {
							t.Fatalf("expected error severity, got %s", f.Severity)
						}
						if f.ObservedSHA != shaCheckoutV4 {
							t.Fatalf("ObservedSHA: got %q, want %q (resolver output, makes claim falsifiable)", f.ObservedSHA, shaCheckoutV4)
						}
						if f.Dependency == nil || f.Dependency.SHA != shaImpostor {
							t.Fatalf("Dependency.SHA: want pinned %s, got %#v", shaImpostor, f.Dependency)
						}
					}
				}
				if !hasForgery {
					t.Fatalf("expected a lockfile-forgery finding, got %v", findingCategories(got))
				}
			},
		},
		{
			name: "impostor-commit: sha unreachable from ref and resolver doesn't know ref",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaImpostor)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				// Resolver doesn't know the ref → no ref-moved / forgery path.
				reach: map[stubReachKey]resolver.ReachabilityStatus{
					{"actions", "checkout", shaImpostor, "v4"}: resolver.Unreachable,
				},
			},
			wantCategories: []Category{CategoryImpostorCommit},
		},
		{
			name:         "misleading-sha: sha-shaped ref resolves to different commit",
			lockfile:     map[string][]string{},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", shaCheckoutV4)},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", shaCheckoutV4}: shaSetupGoV5,
				},
			},
			extra: func(t *testing.T, got []Finding) {
				hasMisleading := false
				for _, f := range got {
					if f.Category == CategoryMisleadingSHA {
						hasMisleading = true
						if f.ObservedSHA != shaSetupGoV5 {
							t.Fatalf("ObservedSHA: got %q, want %q (resolver output, makes claim falsifiable)", f.ObservedSHA, shaSetupGoV5)
						}
						if f.Dependency == nil || f.Dependency.SHA != shaCheckoutV4 {
							t.Fatalf("Dependency.SHA: want pinned %s (the SHA-shaped ref), got %#v", shaCheckoutV4, f.Dependency)
						}
					}
				}
				if !hasMisleading {
					t.Fatalf("expected misleading-sha finding, got %v", findingCategories(got))
				}
			},
		},
		{
			// Covers the legitimate annotated-tag-object pin pattern (e.g.
			// actions/github-script@<v9.0.0 tag-object sha>): the resolver
			// peels via ^{commit} so res.Sha is the underlying commit, not
			// the pinned ref, but the pin is still immutable and must not
			// trip misleading-sha.
			name:         "misleading-sha negative: tag-object SHA pin must not false-positive",
			lockfile:     map[string][]string{},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "github-script", "d746ffe35508b1917358783b479e04febd2b8f71")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "github-script", "d746ffe35508b1917358783b479e04febd2b8f71"}: shaSetupGoV5,
				},
				tagObjects: map[stubTagObjectKey]string{
					{"actions", "github-script", "d746ffe35508b1917358783b479e04febd2b8f71"}: shaSetupGoV5,
				},
			},
			extra: func(t *testing.T, got []Finding) {
				for _, f := range got {
					if f.Category == CategoryMisleadingSHA {
						t.Fatalf("did not expect misleading-sha for tag-object SHA pin, got %v", findingCategories(got))
					}
				}
			},
		},
		{
			name: "no resolver: resolver-dependent checks are skipped",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
			},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			noResolver:     true,
			wantCategories: nil,
		},
		{
			// Pins the rate-limit-fallback contract from the
			// confidence-axis card: when the Compare API can't give an
			// authoritative ancestry answer (AncestryUnknown — rate
			// limit, transient API error, see resolver.CheckAncestry),
			// the resulting ref-moved finding downgrades from High to
			// Medium so consumers know we inferred from the SHA mismatch
			// alone.
			name: "ref-moved confidence: AncestryUnknown is medium",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				// No ancestry entry → stub returns AncestryUnknown.
				reach: map[stubReachKey]resolver.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolver.Reachable,
				},
			},
			wantCategories: []Category{CategoryRefMoved},
			extra: func(t *testing.T, got []Finding) {
				if got[0].Confidence != ConfidenceMedium {
					t.Errorf("Confidence: got %q, want %q (AncestryUnknown is the rate-limit fallback path)", got[0].Confidence, ConfidenceMedium)
				}
			},
		},
		{
			// Positive counterpart to the AncestryUnknown→medium case:
			// when the Compare API gives us AncestryConfirmed the
			// ref-moved finding is High-confidence because we have
			// authoritative upstream data.
			name: "ref-moved confidence: AncestryConfirmed is high",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolver.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolver.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolver.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolver.Reachable,
				},
			},
			wantCategories: []Category{CategoryRefMoved},
			extra: func(t *testing.T, got []Finding) {
				if got[0].Confidence != ConfidenceHigh {
					t.Errorf("Confidence: got %q, want %q (AncestryConfirmed is authoritative)", got[0].Confidence, ConfidenceHigh)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := checkNewLockfile(tc.lockfile)
			pw := checkParsedWF(wfPath, tc.workflowRefs...)
			var r checkResolver
			if !tc.noResolver && tc.resolver != nil {
				r = tc.resolver
			}
			got := runChecks(pw, lf, r)

			if tc.wantCategories != nil {
				wantStrs := make([]string, len(tc.wantCategories))
				for i, c := range tc.wantCategories {
					wantStrs[i] = string(c)
				}
				sort.Strings(wantStrs)
				gotStrs := findingCategories(got)
				if len(gotStrs) != len(wantStrs) {
					t.Fatalf("findings: got %v, want %v (full: %#v)", gotStrs, wantStrs, got)
				}
				for i := range wantStrs {
					if gotStrs[i] != wantStrs[i] {
						t.Fatalf("findings: got %v, want %v (full: %#v)", gotStrs, wantStrs, got)
					}
				}
			}

			if tc.extra != nil {
				tc.extra(t, got)
			}
		})
	}
}

// TestRunChecks_AllFindingsCarryConfidence is the fail-fast guard the
// confidence-axis card requires: every finding emitted by any check
// path must carry a non-empty Confidence. A zero value here would mean
// a new check (or an edit to an existing one) forgot to set the field,
// and the JSON/SARIF surface would leak `""`.
func TestRunChecks_AllFindingsCarryConfidence(t *testing.T) {
	// Cover every check path runChecks dispatches to.
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {
			checkPinKey("actions", "checkout", "v4", shaCheckoutV3), // ref-moved/forgery seed
			checkPinKey("actions", "unused", "v1", shaSetupGoV5),    // stale seed
		},
	})
	pw := checkParsedWF(".github/workflows/ci.yml",
		checkRef("actions", "checkout", "v4"),        // ref-moved or forgery
		checkRef("actions", "setup-node", "v3"),      // not-pinned
		checkRef("actions", "bare-sha", shaImpostor), // sha-as-ref + misleading
	)
	r := &stubCheckResolver{
		refs: map[stubRefKey]string{
			{"actions", "checkout", "v4"}:        shaCheckoutV4,
			{"actions", "bare-sha", shaImpostor}: shaSetupGoV5,
		},
		ancestry: map[stubAncestryKey]resolver.AncestryStatus{
			{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolver.AncestryConfirmed,
		},
		reach: map[stubReachKey]resolver.ReachabilityStatus{
			{"actions", "checkout", shaCheckoutV3, "v4"}: resolver.Reachable,
		},
	}
	got := runChecks(pw, lf, r)
	if len(got) == 0 {
		t.Fatal("expected findings to exercise the confidence guard")
	}
	for i, f := range got {
		if f.Confidence == "" {
			t.Errorf("finding[%d] category=%s has empty Confidence — every construction site must set it", i, f.Category)
		}
	}
}
