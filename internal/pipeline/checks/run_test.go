package checks

import (
	"context"
	"sort"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/resolve"
)

// Typed map keys for the test stub: a small struct per lookup tuple so
// callers can't drift on delimiter choice.
type (
	stubRefKey       struct{ owner, repo, ref string }
	stubAncestryKey  struct{ owner, repo, cand, head string }
	stubReachKey     struct{ owner, repo, sha, ref string }
	stubTagObjectKey struct{ owner, repo, sha string }
)

// stubCheckResolver scripts every CheckResolver call from test fixtures.
// Missing entries return *Unknown values (fail-open).
type stubCheckResolver struct {
	refs            map[stubRefKey]string                       // resolved ref → sha; absence = unknown
	ancestry        map[stubAncestryKey]resolve.AncestryStatus  // (cand, head) ancestry decision
	ancestryDetails map[stubAncestryKey]string                  // optional per-key detail string; absence = ""
	reach           map[stubReachKey]resolve.ReachabilityStatus // sha-reachable-from-ref decision
	tagObjects      map[stubTagObjectKey]string                 // sha → peeled commit
}

func (s *stubCheckResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	if s == nil {
		return "", false
	}
	sha, ok := s.refs[stubRefKey{owner, repo, ref}]
	return sha, ok
}

func (s *stubCheckResolver) CheckAncestry(_ context.Context, owner, repo, cand, head string) (resolve.AncestryStatus, string) {
	if s == nil {
		return resolve.AncestryUnknown, ""
	}
	key := stubAncestryKey{owner, repo, cand, head}
	v, ok := s.ancestry[key]
	if !ok {
		return resolve.AncestryUnknown, s.ancestryDetails[key]
	}
	return v, s.ancestryDetails[key]
}

func (s *stubCheckResolver) CheckReachability(owner, repo, sha, ref string) resolve.ReachabilityStatus {
	if s == nil {
		return resolve.ReachabilityUnknown
	}
	v, ok := s.reach[stubReachKey{owner, repo, sha, ref}]
	if !ok {
		return resolve.ReachabilityUnknown
	}
	return v
}

func (s *stubCheckResolver) PeelTagObject(_ context.Context, owner, repo, sha string) (string, bool) {
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

// checkPinKey returns a decorated pin string "owner/repo@ref#sha1-hex" that
// checkNewLockfile parses to build both the workflow key (before #) and the
// Dependencies entry (Commit from the suffix).
func checkPinKey(owner, repo, ref, sha string) string {
	return owner + "/" + repo + "@" + ref + "#sha1-" + sha
}

// checkNewLockfile builds a File from decorated pin keys.
func checkNewLockfile(workflows map[string][]string, deps ...map[string]parserlock.Action) parserlock.File {
	d := make(map[string]parserlock.Action)
	wf := make(map[string][]string, len(workflows))
	if len(deps) > 0 && deps[0] != nil {
		// Explicit deps override, pass through workflows as-is.
		d = deps[0]
		wf = workflows
	} else {
		for path, pins := range workflows {
			keys := make([]string, 0, len(pins))
			for _, decorated := range pins {
				key, commit := splitDecoratedPin(decorated)
				keys = append(keys, key)
				if commit != "" {
					// Extract ref from key (after @)
					ref := key[strings.LastIndex(key, "@")+1:]
					d[key] = parserlock.Action{Ref: ref, Commit: commit}
				}
			}
			wf[path] = keys
		}
	}
	return parserlock.File{
		Version:      parserlock.Version,
		Workflows:    wf,
		Dependencies: d,
	}
}

// splitDecoratedPin splits "owner/repo@ref#sha1-hex" into key and commit.
func splitDecoratedPin(s string) (key, commit string) {
	if idx := strings.Index(s, "#"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
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

// TestRunChecks groups per-category RunChecks integration cases that share
// the same lockfile + parsed-workflow + resolver setup. Cases that need to
// assert non-category aspects (Severity, ObservedSHA, Dependency.SHA,
// Confidence) hang those off the optional `extra` hook.
//
// TestRunChecks_AllFindingsCarryConfidence stays separate: it is a
// structural fail-fast guard that exercises every check path in one
// fixture.
func TestRunChecks(t *testing.T) {
	const wfPath = ".github/workflows/ci.yml"

	cases := []struct {
		name           string
		lockfile       map[string][]string
		workflowRefs   []parserlock.ActionRef
		resolver       *stubCheckResolver
		noResolver     bool // when true, RunChecks gets nil instead of resolver
		wantCategories []Category
		// extra runs after category assertions for case-specific checks
		// (Severity, ObservedSHA, Dependency.SHA, Confidence, etc).
		extra func(t *testing.T, got []Finding)
	}{
		{
			name:           "not-pinned: ref used but absent from lockfile",
			lockfile:       map[string][]string{},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			wantCategories: []Category{NotPinned},
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
			wantCategories: []Category{ShaAsRef},
		},
		{
			name: "ref-changed + stale: lockfile pins v4, workflow uses v3",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
			},
			workflowRefs:   []parserlock.ActionRef{checkRef("actions", "checkout", "v3")},
			wantCategories: []Category{RefChanged, Stale},
			extra: func(t *testing.T, got []Finding) {
				var refChanged *Finding
				for i := range got {
					if got[i].Category == RefChanged {
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
			wantCategories: []Category{Stale},
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
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV4, "v4"}: resolve.Reachable,
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
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolve.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
				},
			},
			wantCategories: []Category{RefMoved},
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
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaImpostor, shaCheckoutV4}: resolve.AncestryNotAncestor,
				},
			},
			extra: func(t *testing.T, got []Finding) {
				hasForgery := false
				for _, f := range got {
					if f.Category == LockfileForgery {
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
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaImpostor, "v4"}: resolve.Unreachable,
				},
			},
			wantCategories: []Category{ImpostorCommit},
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
					if f.Category == MisleadingSHA {
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
					if f.Category == MisleadingSHA {
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
			// Compare API rate-limit fallback: ancestry is unknown so
			// the SHA mismatch can't be classified as ref-moved or
			// lockfile-forgery. Emit AncestryUnknown so
			// consumers don't conflate "scan inconclusive" with valid.
			name: "ancestry unknown emits ancestry-unknown, not ref-moved",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				// No ancestry entry → stub returns AncestryUnknown.
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
				},
			},
			wantCategories: []Category{AncestryUnknown},
			extra: func(t *testing.T, got []Finding) {
				if got[0].Category == Valid {
					t.Fatalf("Category: ancestry-unknown must not regress to valid (Dependabot FindingMapper treats valid as clean)")
				}
				if got[0].Confidence != ConfidenceMedium {
					t.Errorf("Confidence: got %q, want %q (AncestryUnknown is the rate-limit fallback path)", got[0].Confidence, ConfidenceMedium)
				}
				if got[0].Severity != SeverityWarning {
					t.Errorf("Severity: got %q, want %q (inconclusive findings stay warnings)", got[0].Severity, SeverityWarning)
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
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolve.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
				},
			},
			wantCategories: []Category{RefMoved},
			extra: func(t *testing.T, got []Finding) {
				if got[0].Confidence != ConfidenceHigh {
					t.Errorf("Confidence: got %q, want %q (AncestryConfirmed is authoritative)", got[0].Confidence, ConfidenceHigh)
				}
			},
		},
		{
			// Detail plumbing: when CheckAncestry returns a non-empty
			// rate-limit detail (e.g. "rate limited (HTTP 429); resets
			// at 1717552800"), the AncestryUnknown finding must surface
			// it inside the parenthetical so operators don't see a
			// generic "ancestry check inconclusive".
			name: "ancestry unknown surfaces resolver detail",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestryDetails: map[stubAncestryKey]string{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: "rate limited (HTTP 429); resets at 1717552800; retry budget exhausted after 3 attempts",
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
				},
			},
			wantCategories: []Category{AncestryUnknown},
			extra: func(t *testing.T, got []Finding) {
				if !strings.Contains(got[0].Detail, "rate limited (HTTP 429)") {
					t.Errorf("Detail: expected resolver rate-limit detail, got %q", got[0].Detail)
				}
				if !strings.Contains(got[0].Detail, "resets at 1717552800") {
					t.Errorf("Detail: expected reset timestamp in finding, got %q", got[0].Detail)
				}
			},
		},
		{
			// Tag-hijacked-to-fork-network: locked SHA stays legit
			// (still on a branch); live tag has been moved to a
			// fork-network commit not on any upstream branch.
			// AncestryConfirmed means CompareCommits returns
			// "ahead": the live SHA descends from the lockfile
			// commit (its parent is a real descendant), so ref-moved
			// would otherwise be the only finding. The new
			// liveRefImpostorFinding catches the live-SHA branch
			// unreachability and escalates with a parallel
			// impostor-commit error.
			name: "ref-moved + impostor-commit: tag hijacked to fork-network commit",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaImpostor,
				},
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaImpostor}: resolve.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
					{"actions", "checkout", shaImpostor, "v4"}:   resolve.Unreachable,
				},
			},
			wantCategories: []Category{ImpostorCommit, RefMoved},
			extra: func(t *testing.T, got []Finding) {
				var impostor, refMoved *Finding
				for i := range got {
					switch got[i].Category {
					case ImpostorCommit:
						impostor = &got[i]
					case RefMoved:
						refMoved = &got[i]
					}
				}
				if impostor == nil || refMoved == nil {
					t.Fatalf("expected both impostor-commit and ref-moved, got %v", findingCategories(got))
				}
				if impostor.Severity != SeverityError {
					t.Errorf("impostor severity: got %s, want error", impostor.Severity)
				}
				if impostor.ObservedSHA != shaImpostor {
					t.Errorf("impostor ObservedSHA: got %q, want %q (live SHA, the actual impostor)", impostor.ObservedSHA, shaImpostor)
				}
				if impostor.Dependency == nil || impostor.Dependency.SHA != shaImpostor {
					t.Errorf("impostor Dependency.SHA: want live %q, got %#v (must differ from ref-moved finding so consumers can tell them apart)", shaImpostor, impostor.Dependency)
				}
				if refMoved.Dependency == nil || refMoved.Dependency.SHA != shaCheckoutV3 {
					t.Errorf("ref-moved Dependency.SHA: want locked %q, got %#v", shaCheckoutV3, refMoved.Dependency)
				}
				if !strings.Contains(impostor.Detail, "fork-network injection") {
					t.Errorf("impostor Detail: want fork-network wording, got %q", impostor.Detail)
				}
			},
		},
		{
			// Negative: when the live SHA *is* reachable from a
			// branch, the move is benign (release-train style).
			// Only ref-moved should fire — no parallel impostor.
			name: "ref-moved only: live SHA reachable means benign move",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolve.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
					{"actions", "checkout", shaCheckoutV4, "v4"}: resolve.Reachable,
				},
			},
			wantCategories: []Category{RefMoved},
		},
		{
			// Fail-open: reach result Unknown for live SHA (cache miss,
			// rate limit) must not escalate to impostor-commit. Same
			// fallback policy as the locked-SHA path.
			name: "ref-moved only: live-SHA reachability unknown stays benign",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolve.AncestryConfirmed,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
					// no live-SHA entry → ReachabilityUnknown
				},
			},
			wantCategories: []Category{RefMoved},
		},
		{
			// Forgery suppression: when ancestry says
			// AncestryNotAncestor the lockfile is forged. Do NOT
			// emit a parallel impostor-commit even if the live SHA
			// is also unreachable — forgery is the stronger claim
			// and double-flagging clutters without adding action.
			name: "forgery suppresses live impostor",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaImpostor)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaCheckoutV4,
				},
				ancestry: map[stubAncestryKey]resolve.AncestryStatus{
					{"actions", "checkout", shaImpostor, shaCheckoutV4}: resolve.AncestryNotAncestor,
				},
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV4, "v4"}: resolve.Unreachable,
				},
			},
			extra: func(t *testing.T, got []Finding) {
				cats := findingCategories(got)
				for _, c := range cats {
					if c == string(ImpostorCommit) {
						t.Fatalf("forgery branch must not emit parallel impostor-commit, got %v", cats)
					}
				}
				hasForgery := false
				for _, c := range cats {
					if c == string(LockfileForgery) {
						hasForgery = true
					}
				}
				if !hasForgery {
					t.Fatalf("expected lockfile-forgery, got %v", cats)
				}
			},
		},
		{
			// AncestryUnknown + live SHA unreachable: both signals
			// must surface. ancestry-unknown says "we can't tell if
			// this is a release move or a forgery", impostor-commit
			// says "live SHA is on no branch — investigate". They
			// answer different questions, so emit both.
			name: "ancestry-unknown + impostor: independent signals coexist",
			lockfile: map[string][]string{
				wfPath: {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
			},
			workflowRefs: []parserlock.ActionRef{checkRef("actions", "checkout", "v4")},
			resolver: &stubCheckResolver{
				refs: map[stubRefKey]string{
					{"actions", "checkout", "v4"}: shaImpostor,
				},
				// No ancestry entry → AncestryUnknown.
				reach: map[stubReachKey]resolve.ReachabilityStatus{
					{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
					{"actions", "checkout", shaImpostor, "v4"}:   resolve.Unreachable,
				},
			},
			wantCategories: []Category{AncestryUnknown, ImpostorCommit},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := checkNewLockfile(tc.lockfile)
			pw := checkParsedWF(wfPath, tc.workflowRefs...)
			var r CheckResolver
			if !tc.noResolver && tc.resolver != nil {
				r = tc.resolver
			}
			got := RunChecks(context.Background(), pw, lf, r)

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
	// Cover every check path RunChecks dispatches to.
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
		ancestry: map[stubAncestryKey]resolve.AncestryStatus{
			{"actions", "checkout", shaCheckoutV3, shaCheckoutV4}: resolve.AncestryConfirmed,
		},
		reach: map[stubReachKey]resolve.ReachabilityStatus{
			{"actions", "checkout", shaCheckoutV3, "v4"}: resolve.Reachable,
		},
	}
	got := RunChecks(context.Background(), pw, lf, r)
	if len(got) == 0 {
		t.Fatal("expected findings to exercise the confidence guard")
	}
	for i, f := range got {
		if f.Confidence == "" {
			t.Errorf("finding[%d] category=%s has empty Confidence — every construction site must set it", i, f.Category)
		}
	}
}

// --- impostor-commit fail-open guards -------------------------------------
//
// The locked-SHA impostor check (checkImpostorCommit) must only fire on an
// authoritative Unreachable verdict. When reachability is Unknown (rate
// limit / transient API failure) it must fail open: a scanner that cried
// "injected lockfile entry" every time the GitHub API hiccupped would be
// worse than useless. These tests pin that contract.

func impostorFixture(reach resolve.ReachabilityStatus) (ParsedWorkflow, map[string]lockedPin, *stubCheckResolver) {
	ref := checkRef("actions", "checkout", "v4")
	pw := checkParsedWF(".github/workflows/ci.yml", ref)
	depIndex := map[string]lockedPin{
		parserlock.IndexKey("actions", "checkout", "v4"): {
			Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: "v4"},
			Commit: "sha1-" + shaImpostor,
		},
	}
	r := &stubCheckResolver{
		reach: map[stubReachKey]resolve.ReachabilityStatus{
			{"actions", "checkout", shaImpostor, "v4"}: reach,
		},
	}
	return pw, depIndex, r
}

func TestCheckImpostorCommit_UnreachableEmitsFinding(t *testing.T) {
	pw, depIndex, r := impostorFixture(resolve.Unreachable)

	out := checkImpostorCommit(pw, depIndex, r, nil)
	if len(out) != 1 {
		t.Fatalf("Unreachable: got %d findings, want 1 (%v)", len(out), findingCategories(out))
	}
	if out[0].Category != ImpostorCommit {
		t.Fatalf("category = %v, want ImpostorCommit", out[0].Category)
	}
}

func TestCheckImpostorCommit_ReachabilityUnknownFailsOpen(t *testing.T) {
	pw, depIndex, r := impostorFixture(resolve.ReachabilityUnknown)

	out := checkImpostorCommit(pw, depIndex, r, nil)
	for _, f := range out {
		if f.Category == ImpostorCommit {
			t.Fatalf("Unknown reachability must not emit ImpostorCommit (got %v)", findingCategories(out))
		}
	}
}

func TestLiveRefImpostorFinding_ReachabilityUnknownFailsOpen(t *testing.T) {
	ref := checkRef("actions", "checkout", "v4")
	pw := checkParsedWF(".github/workflows/ci.yml", ref)
	r := &stubCheckResolver{
		reach: map[stubReachKey]resolve.ReachabilityStatus{
			{"actions", "checkout", shaImpostor, "v4"}: resolve.ReachabilityUnknown,
		},
	}

	if _, ok := liveRefImpostorFinding(pw, ref, shaImpostor, r); ok {
		t.Fatal("Unknown reachability must not produce a live-ref impostor finding")
	}
}
