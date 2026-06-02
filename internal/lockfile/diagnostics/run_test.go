package diagnostics

import (
	"context"
	"sort"
	"strings"
	"testing"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// stubResolver scripts every Resolver call from test fixtures. Missing
// entries return *Unknown values (engine fail-open semantics).
type stubResolver struct {
	refs         map[string]RefResult          // key: "owner/repo@ref"
	ancestry     map[string]AncestryStatus     // key: "owner/repo:candidate:head"
	reachability map[string]ReachabilityStatus // key: "owner/repo:sha:ref"
}

func (s *stubResolver) ResolveRef(_ context.Context, owner, repo, ref string) RefResult {
	if s == nil {
		return RefResult{Status: RefStatusUnknown}
	}
	return s.refs[owner+"/"+repo+"@"+ref]
}

func (s *stubResolver) CheckAncestry(_ context.Context, owner, repo, cand, head string) AncestryStatus {
	if s == nil {
		return AncestryUnknown
	}
	return s.ancestry[owner+"/"+repo+":"+cand+":"+head]
}

func (s *stubResolver) CheckReachability(_ context.Context, owner, repo, sha, ref string) ReachabilityStatus {
	if s == nil {
		return ReachabilityUnknown
	}
	return s.reachability[owner+"/"+repo+":"+sha+":"+ref]
}

const (
	shaCheckoutV4 = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	shaCheckoutV3 = "11bd71901bbe5b1630ceea73d27597364c9af683"
	shaSetupGoV5  = "0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaImposter   = "ffffffffffffffffffffffffffffffffffffffff"
)

func pinKey(owner, repo, ref, sha string) string {
	return owner + "/" + repo + "@" + ref + ":sha1-" + sha
}

func wfWithPins(path string, pins ...string) []string {
	return pins
}

func newLockfile(workflows map[string][]string) parserlock.File {
	return parserlock.File{
		Version:   parserlock.Version,
		Workflows: workflows,
	}
}

func findingCodes(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f.Code))
	}
	sort.Strings(out)
	return out
}

func TestRun_NotPinned(t *testing.T) {
	lf := newLockfile(map[string][]string{})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{})
	if len(got) != 1 || got[0].Code != CodeNotPinned {
		t.Fatalf("expected single not_pinned finding, got %#v", got)
	}
	if got[0].Severity != SeverityError {
		t.Fatalf("expected error severity, got %s", got[0].Severity)
	}
}

func TestRun_ShaAsRef(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", shaCheckoutV4, shaCheckoutV4)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: shaCheckoutV4}},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{})
	if len(got) != 1 || got[0].Code != CodeShaAsRef {
		t.Fatalf("expected single sha_as_ref finding, got %#v", got)
	}
}

func TestRun_RefChanged(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaCheckoutV4)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v3"}},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{})
	codes := findingCodes(got)
	// ref_changed because uses v3 ≠ pinned v4; stale because pinned v4 is
	// no longer referenced. Both are correct.
	if len(codes) != 2 || codes[0] != string(CodeRefChanged) || codes[1] != string(CodeStale) {
		t.Fatalf("expected ref_changed + stale, got %v (%#v)", codes, got)
	}
	var refChanged *Finding
	for i := range got {
		if got[i].Code == CodeRefChanged {
			refChanged = &got[i]
		}
	}
	if refChanged == nil || refChanged.LockedSha != shaCheckoutV4 {
		t.Fatalf("expected ref_changed with locked sha %s, got %#v", shaCheckoutV4, refChanged)
	}
}

func TestRun_Stale(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaCheckoutV4)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: nil,
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{})
	if len(got) != 1 || got[0].Code != CodeStale {
		t.Fatalf("expected single stale finding, got %#v", got)
	}
}

func TestRun_PinnedAndCurrent_NoFindings(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaCheckoutV4)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	r := &stubResolver{
		refs: map[string]RefResult{
			"actions/checkout@v4": {Status: RefStatusResolved, Sha: shaCheckoutV4, RefType: "tag"},
		},
		reachability: map[string]ReachabilityStatus{
			"actions/checkout:" + shaCheckoutV4 + ":v4": ReachabilityReachable,
		},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{Resolver: r})
	if len(got) != 0 {
		t.Fatalf("expected zero findings, got %#v", got)
	}
}

func TestRun_RefMoved(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaCheckoutV3)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	r := &stubResolver{
		refs: map[string]RefResult{
			"actions/checkout@v4": {Status: RefStatusResolved, Sha: shaCheckoutV4, RefType: "tag"},
		},
		ancestry: map[string]AncestryStatus{
			"actions/checkout:" + shaCheckoutV3 + ":" + shaCheckoutV4: AncestryConfirmed,
		},
		reachability: map[string]ReachabilityStatus{
			"actions/checkout:" + shaCheckoutV3 + ":v4": ReachabilityReachable,
		},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{Resolver: r})
	if len(got) != 1 || got[0].Code != CodeRefMoved {
		t.Fatalf("expected single ref_moved finding, got %#v", got)
	}
	if got[0].LiveSha != shaCheckoutV4 || got[0].LockedSha != shaCheckoutV3 {
		t.Fatalf("unexpected sha pair on finding: %#v", got[0])
	}
}

func TestRun_LockfileForgery(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaImposter)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	r := &stubResolver{
		refs: map[string]RefResult{
			"actions/checkout@v4": {Status: RefStatusResolved, Sha: shaCheckoutV4, RefType: "tag"},
		},
		ancestry: map[string]AncestryStatus{
			"actions/checkout:" + shaImposter + ":" + shaCheckoutV4: AncestryNotAncestor,
		},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{Resolver: r})
	if len(got) == 0 {
		t.Fatalf("expected at least one finding")
	}
	hasForgery := false
	for _, f := range got {
		if f.Code == CodeLockfileForgery {
			hasForgery = true
			if f.Severity != SeverityError {
				t.Fatalf("expected error severity, got %s", f.Severity)
			}
		}
	}
	if !hasForgery {
		t.Fatalf("expected a lockfile_forgery finding, got %v", findingCodes(got))
	}
}

func TestRun_ImposterCommit(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaImposter)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	r := &stubResolver{
		// Resolver doesn't know the ref → no ref_moved / forgery path.
		refs: map[string]RefResult{
			"actions/checkout@v4": {Status: RefStatusUnknown},
		},
		reachability: map[string]ReachabilityStatus{
			"actions/checkout:" + shaImposter + ":v4": ReachabilityUnreachable,
		},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{Resolver: r})
	if len(got) != 1 || got[0].Code != CodeImposterCommit {
		t.Fatalf("expected single imposter_commit finding, got %#v", got)
	}
}

func TestRun_MisleadingSha(t *testing.T) {
	lf := newLockfile(map[string][]string{})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: shaCheckoutV4}},
	}
	// Resolver maps the SHA-shaped ref to a *different* commit.
	r := &stubResolver{
		refs: map[string]RefResult{
			"actions/checkout@" + shaCheckoutV4: {Status: RefStatusResolved, Sha: shaSetupGoV5},
		},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{Resolver: r})
	hasMisleading := false
	for _, f := range got {
		if f.Code == CodeMisleadingSha {
			hasMisleading = true
		}
	}
	if !hasMisleading {
		t.Fatalf("expected misleading_sha finding, got %v", findingCodes(got))
	}
}

func TestRun_NoResolver_SkipsResolverChecks(t *testing.T) {
	lf := newLockfile(map[string][]string{
		".github/workflows/ci.yml": wfWithPins(".github/workflows/ci.yml",
			pinKey("actions", "checkout", "v4", shaCheckoutV4)),
	})
	wf := WorkflowInput{
		Path: ".github/workflows/ci.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wf}, Options{})
	if len(got) != 0 {
		t.Fatalf("expected zero findings with no resolver, got %#v", got)
	}
}

func TestRun_MultipleWorkflowsStableOrder(t *testing.T) {
	lf := newLockfile(map[string][]string{})
	wfA := WorkflowInput{
		Path: ".github/workflows/a.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
	}
	wfB := WorkflowInput{
		Path: ".github/workflows/b.yml",
		Uses: []UsesRef{{Owner: "actions", Repo: "setup-go", Ref: "v5"}},
	}
	got := Run(context.Background(), lf, []WorkflowInput{wfA, wfB}, Options{})
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d (%#v)", len(got), got)
	}
	if !strings.HasSuffix(got[0].WorkflowPath, "/a.yml") || !strings.HasSuffix(got[1].WorkflowPath, "/b.yml") {
		t.Fatalf("findings out of order: %#v", got)
	}
}
