package doctor

import (
	"sort"
	"testing"

	"github.com/github/gh-actions-pin/internal/resolver"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// stubCheckResolver scripts every checkResolver call from test fixtures.
// Missing entries return *Unknown values (fail-open semantics).
type stubCheckResolver struct {
	refs       map[string]string                      // "owner/repo@ref" -> sha; absence = unknown
	ancestry   map[string]resolver.AncestryStatus     // "owner/repo:cand:head"
	reach      map[string]resolver.ReachabilityStatus // "owner/repo:sha:ref"
	tagObjects map[string]string                      // "owner/repo@sha" -> peeled commit
}

func (s *stubCheckResolver) ResolveRef(owner, repo, ref string) (string, bool) {
	if s == nil {
		return "", false
	}
	sha, ok := s.refs[owner+"/"+repo+"@"+ref]
	return sha, ok
}

func (s *stubCheckResolver) CheckAncestry(owner, repo, cand, head string) resolver.AncestryStatus {
	if s == nil {
		return resolver.AncestryUnknown
	}
	v, ok := s.ancestry[owner+"/"+repo+":"+cand+":"+head]
	if !ok {
		return resolver.AncestryUnknown
	}
	return v
}

func (s *stubCheckResolver) CheckReachability(owner, repo, sha, ref string) resolver.ReachabilityStatus {
	if s == nil {
		return resolver.ReachabilityUnknown
	}
	v, ok := s.reach[owner+"/"+repo+":"+sha+":"+ref]
	if !ok {
		return resolver.ReachabilityUnknown
	}
	return v
}

func (s *stubCheckResolver) PeelTagObject(owner, repo, sha string) (string, bool) {
	if s == nil {
		return "", false
	}
	commit, ok := s.tagObjects[owner+"/"+repo+"@"+sha]
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

func TestRunChecks_NotPinned(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	got := runChecks(pw, lf, nil)
	if len(got) != 1 || got[0].Category != CategoryNotPinned {
		t.Fatalf("expected single not_pinned finding, got %#v", got)
	}
	if got[0].Severity != SeverityError {
		t.Fatalf("expected error severity, got %s", got[0].Severity)
	}
}

func TestRunChecks_ShaAsRef(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", shaCheckoutV4, shaCheckoutV4)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", shaCheckoutV4))
	got := runChecks(pw, lf, nil)
	if len(got) != 1 || got[0].Category != CategorySHAAsRef {
		t.Fatalf("expected single sha_as_ref finding, got %#v", got)
	}
}

func TestRunChecks_RefChanged(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v3"))
	got := runChecks(pw, lf, nil)
	cats := findingCategories(got)
	if len(cats) != 2 || cats[0] != string(CategoryRefChanged) || cats[1] != string(CategoryStale) {
		t.Fatalf("expected ref_changed + stale, got %v (%#v)", cats, got)
	}
	var refChanged *Finding
	for i := range got {
		if got[i].Category == CategoryRefChanged {
			refChanged = &got[i]
		}
	}
	if refChanged == nil || refChanged.Dependency == nil || refChanged.Dependency.SHA != shaCheckoutV4 {
		t.Fatalf("expected ref_changed with locked sha %s, got %#v", shaCheckoutV4, refChanged)
	}
}

func TestRunChecks_Stale(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml")
	got := runChecks(pw, lf, nil)
	if len(got) != 1 || got[0].Category != CategoryStale {
		t.Fatalf("expected single stale finding, got %#v", got)
	}
}

func TestRunChecks_PinnedAndCurrent_NoFindings(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	r := &stubCheckResolver{
		refs: map[string]string{
			"actions/checkout@v4": shaCheckoutV4,
		},
		reach: map[string]resolver.ReachabilityStatus{
			"actions/checkout:" + shaCheckoutV4 + ":v4": resolver.Reachable,
		},
	}
	got := runChecks(pw, lf, r)
	if len(got) != 0 {
		t.Fatalf("expected zero findings, got %#v", got)
	}
}

func TestRunChecks_RefMoved(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	r := &stubCheckResolver{
		refs: map[string]string{
			"actions/checkout@v4": shaCheckoutV4,
		},
		ancestry: map[string]resolver.AncestryStatus{
			"actions/checkout:" + shaCheckoutV3 + ":" + shaCheckoutV4: resolver.AncestryConfirmed,
		},
		reach: map[string]resolver.ReachabilityStatus{
			"actions/checkout:" + shaCheckoutV3 + ":v4": resolver.Reachable,
		},
	}
	got := runChecks(pw, lf, r)
	if len(got) != 1 || got[0].Category != CategoryRefMoved {
		t.Fatalf("expected single ref_moved finding, got %#v", got)
	}
	if got[0].ObservedSHA != shaCheckoutV4 || got[0].Dependency == nil || got[0].Dependency.SHA != shaCheckoutV3 {
		t.Fatalf("unexpected sha pair on finding: %#v", got[0])
	}
}

func TestRunChecks_LockfileForgery(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaImpostor)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	r := &stubCheckResolver{
		refs: map[string]string{
			"actions/checkout@v4": shaCheckoutV4,
		},
		ancestry: map[string]resolver.AncestryStatus{
			"actions/checkout:" + shaImpostor + ":" + shaCheckoutV4: resolver.AncestryNotAncestor,
		},
	}
	got := runChecks(pw, lf, r)
	if len(got) == 0 {
		t.Fatalf("expected at least one finding")
	}
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
		t.Fatalf("expected a lockfile_forgery finding, got %v", findingCategories(got))
	}
}

func TestRunChecks_ImpostorCommit(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaImpostor)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	r := &stubCheckResolver{
		// Resolver doesn't know the ref → no ref_moved / forgery path.
		reach: map[string]resolver.ReachabilityStatus{
			"actions/checkout:" + shaImpostor + ":v4": resolver.Unreachable,
		},
	}
	got := runChecks(pw, lf, r)
	if len(got) != 1 || got[0].Category != CategoryImpostorCommit {
		t.Fatalf("expected single impostor_commit finding, got %#v", got)
	}
}

func TestRunChecks_MisleadingSha(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", shaCheckoutV4))
	// Resolver maps the SHA-shaped ref to a *different* commit.
	r := &stubCheckResolver{
		refs: map[string]string{
			"actions/checkout@" + shaCheckoutV4: shaSetupGoV5,
		},
	}
	got := runChecks(pw, lf, r)
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
		t.Fatalf("expected misleading_sha finding, got %v", findingCategories(got))
	}
}

// TestRunChecks_MisleadingSha_TagObjectSHA covers the legitimate
// annotated-tag-object pin pattern (e.g. actions/github-script@<v9.0.0
// tag-object sha>): the resolver peels via ^{commit} so res.Sha is the
// underlying commit, not the pinned ref, but the pin is still immutable
// and must not trip MISLEADING_SHA.
func TestRunChecks_MisleadingSha_TagObjectSHA(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{})
	const tagObjectSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	const peeledCommit = shaSetupGoV5
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "github-script", tagObjectSHA))
	r := &stubCheckResolver{
		refs: map[string]string{
			"actions/github-script@" + tagObjectSHA: peeledCommit,
		},
		tagObjects: map[string]string{
			"actions/github-script@" + tagObjectSHA: peeledCommit,
		},
	}
	got := runChecks(pw, lf, r)
	for _, f := range got {
		if f.Category == CategoryMisleadingSHA {
			t.Fatalf("did not expect misleading_sha for tag-object SHA pin, got %v", findingCategories(got))
		}
	}
}

func TestRunChecks_NoResolver_SkipsResolverChecks(t *testing.T) {
	lf := checkNewLockfile(map[string][]string{
		".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV4)},
	})
	pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
	got := runChecks(pw, lf, nil)
	if len(got) != 0 {
		t.Fatalf("expected zero findings with no resolver, got %#v", got)
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
checkPinKey("actions", "checkout", "v4", shaCheckoutV3), // ref_moved/forgery seed
checkPinKey("actions", "unused", "v1", shaSetupGoV5),    // stale seed
},
})
pw := checkParsedWF(".github/workflows/ci.yml",
checkRef("actions", "checkout", "v4"),       // ref_moved or forgery
checkRef("actions", "setup-node", "v3"),     // not_pinned
checkRef("actions", "bare-sha", shaImpostor), // sha_as_ref + misleading
)
r := &stubCheckResolver{
refs: map[string]string{
"actions/checkout@v4":         shaCheckoutV4,
"actions/bare-sha@" + shaImpostor: shaSetupGoV5,
},
ancestry: map[string]resolver.AncestryStatus{
"actions/checkout:" + shaCheckoutV3 + ":" + shaCheckoutV4: resolver.AncestryConfirmed,
},
reach: map[string]resolver.ReachabilityStatus{
"actions/checkout:" + shaCheckoutV3 + ":v4": resolver.Reachable,
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

// TestRunChecks_RefMoved_AncestryUnknown_IsMedium pins the
// rate-limit-fallback contract from the confidence-axis card: when the
// Compare API can't give us an authoritative ancestry answer
// (AncestryUnknown — rate limit, transient API error, see
// resolver/resolver.go CheckAncestry), the resulting ref_moved finding
// downgrades from High to Medium so consumers know we inferred from the
// SHA mismatch alone.
func TestRunChecks_RefMoved_AncestryUnknown_IsMedium(t *testing.T) {
lf := checkNewLockfile(map[string][]string{
".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
})
pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
r := &stubCheckResolver{
refs: map[string]string{
"actions/checkout@v4": shaCheckoutV4,
},
// No ancestry entry → stub returns AncestryUnknown.
reach: map[string]resolver.ReachabilityStatus{
"actions/checkout:" + shaCheckoutV3 + ":v4": resolver.Reachable,
},
}
got := runChecks(pw, lf, r)
var rm *Finding
for i := range got {
if got[i].Category == CategoryRefMoved {
rm = &got[i]
break
}
}
if rm == nil {
t.Fatalf("expected a ref_moved finding, got %v", findingCategories(got))
}
if rm.Confidence != ConfidenceMedium {
t.Errorf("Confidence: got %q, want %q (AncestryUnknown is the rate-limit fallback path)", rm.Confidence, ConfidenceMedium)
}
}

// TestRunChecks_RefMoved_AncestryConfirmed_IsHigh is the positive counterpart:
// when the Compare API gives us AncestryConfirmed the ref_moved finding is
// High-confidence because we have authoritative upstream data.
func TestRunChecks_RefMoved_AncestryConfirmed_IsHigh(t *testing.T) {
lf := checkNewLockfile(map[string][]string{
".github/workflows/ci.yml": {checkPinKey("actions", "checkout", "v4", shaCheckoutV3)},
})
pw := checkParsedWF(".github/workflows/ci.yml", checkRef("actions", "checkout", "v4"))
r := &stubCheckResolver{
refs: map[string]string{
"actions/checkout@v4": shaCheckoutV4,
},
ancestry: map[string]resolver.AncestryStatus{
"actions/checkout:" + shaCheckoutV3 + ":" + shaCheckoutV4: resolver.AncestryConfirmed,
},
reach: map[string]resolver.ReachabilityStatus{
"actions/checkout:" + shaCheckoutV3 + ":v4": resolver.Reachable,
},
}
got := runChecks(pw, lf, r)
var rm *Finding
for i := range got {
if got[i].Category == CategoryRefMoved {
rm = &got[i]
break
}
}
if rm == nil {
t.Fatalf("expected a ref_moved finding, got %v", findingCategories(got))
}
if rm.Confidence != ConfidenceHigh {
t.Errorf("Confidence: got %q, want %q (AncestryConfirmed is authoritative)", rm.Confidence, ConfidenceHigh)
}
}
