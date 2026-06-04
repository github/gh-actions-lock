package doctor

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

type fakeReachabilityChecker struct {
	results map[string]resolver.ReachabilityStatus
}

func (f *fakeReachabilityChecker) CheckReachability(owner, repo, sha, ref string) resolver.ReachabilityResult {
	status := f.results[ref]
	if status == "" {
		status = resolver.Unreachable
	}
	return resolver.ReachabilityResult{Owner: owner, Repo: repo, SHA: sha, Ref: ref, Status: status}
}

// TestFindSaneRelease_PicksFirstReachable walks tags newest-first and stops at
// the first stable tag whose commit is reachable from a branch.
func TestFindSaneRelease_PicksFirstReachable(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.5.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
			{"name": "v1.4.0", "commit": map[string]any{"sha": "bbbbbbb2222222222222222222222222222222bb"}},
			{"name": "v1.3.0", "commit": map[string]any{"sha": "ccccccc3333333333333333333333333333333cc"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := newTagListerWithRegistry(t, reg)
	rc := &fakeReachabilityChecker{results: map[string]resolver.ReachabilityStatus{
		"v1.5.0": resolver.Unreachable,
		"v1.4.0": resolver.Reachable,
	}}

	tag, sha := FindSaneRelease(tl, rc, "acme", "widget")
	if tag != "v1.4.0" {
		t.Fatalf("expected v1.4.0, got %q", tag)
	}
	if sha != "bbbbbbb2222222222222222222222222222222bb" {
		t.Fatalf("expected bbbb…bb, got %q", sha)
	}
}

// TestFindSaneRelease_NoneReachable returns empty when every recent tag is
// detached from a branch — signal for the caller to escalate to the publisher.
func TestFindSaneRelease_NoneReachable(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.2.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
			{"name": "v1.1.0", "commit": map[string]any{"sha": "bbbbbbb2222222222222222222222222222222bb"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := newTagListerWithRegistry(t, reg)
	rc := &fakeReachabilityChecker{} // all Unreachable

	tag, sha := FindSaneRelease(tl, rc, "acme", "widget")
	if tag != "" || sha != "" {
		t.Fatalf("expected empty suggestion, got tag=%q sha=%q", tag, sha)
	}
}

// TestEnrichImposterFindings_MarksSearched flags imposter findings with the
// search outcome even when no suggestion is found so renderers can surface
// the "escalate to publisher" hint.
func TestEnrichImposterFindings_MarksSearched(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := newTagListerWithRegistry(t, reg)
	rc := &fakeReachabilityChecker{} // none reachable

	report := &Report{
		Workflows: []WorkflowReport{{
			Path: ".github/workflows/test.yml",
			Findings: []Finding{{
				Category:   CategoryImposterCommit,
				Dependency: &lockfile.Dependency{NWO: "acme/widget", Ref: "v1"},
			}},
		}},
	}

	EnrichImposterFindings(report, tl, rc)

	f := report.Workflows[0].Findings[0]
	if !f.SaneSuggestionSearched {
		t.Error("expected SaneSuggestionSearched=true after walk")
	}
	if f.SaneSuggestionTag != "" {
		t.Errorf("expected no suggestion when nothing reachable, got %q", f.SaneSuggestionTag)
	}
}

// TestEnrichImposterFindings_PopulatesSuggestion attaches the discovered tag
// to the finding so downstream renderers (presentCheckResults, summary) can
// surface a concrete re-pin target.
func TestEnrichImposterFindings_PopulatesSuggestion(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := newTagListerWithRegistry(t, reg)
	rc := &fakeReachabilityChecker{results: map[string]resolver.ReachabilityStatus{
		"v1.0.0": resolver.Reachable,
	}}

	report := &Report{
		Workflows: []WorkflowReport{{
			Path: ".github/workflows/test.yml",
			Findings: []Finding{{
				Category:   CategoryImposterCommit,
				Dependency: &lockfile.Dependency{NWO: "acme/widget", Ref: "v1"},
			}},
		}},
	}

	EnrichImposterFindings(report, tl, rc)

	f := report.Workflows[0].Findings[0]
	if f.SaneSuggestionTag != "v1.0.0" {
		t.Errorf("expected v1.0.0, got %q", f.SaneSuggestionTag)
	}
	if f.SaneSuggestionSHA != "aaaaaaa1111111111111111111111111111111aa" {
		t.Errorf("unexpected sha %q", f.SaneSuggestionSHA)
	}
}
