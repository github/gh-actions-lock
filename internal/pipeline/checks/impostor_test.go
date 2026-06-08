package checks

import (
	"context"
	"testing"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/tag"
)

type fakeReachabilityChecker struct {
	results map[string]resolve.ReachabilityStatus
}

func (f *fakeReachabilityChecker) CheckReachability(_ context.Context, owner, repo, sha, ref string) resolve.ReachabilityResult {
	status := f.results[ref]
	if status == "" {
		status = resolve.Unreachable
	}
	return resolve.ReachabilityResult{Owner: owner, Repo: repo, SHA: sha, Ref: ref, Status: status}
}

// registerTagWalk wires the three endpoints Lister hits during a
// publisher walk: GET /tags, GET /git/matching-refs/tags, GET /releases.
// Tests parameterize only the /tags payload; matching-refs and releases
// are registered empty so the walk completes deterministically.
func registerTagWalk(reg *httpmock.Registry, owner, repo string, tags []map[string]any) {
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/tags`),
		httpmock.JSONResponse(tags),
	)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)
}

// TestFindRecommendedRelease_PicksFirstReachable walks tags newest-first and stops at
// the first stable tag whose commit is reachable from a branch.
func TestFindRecommendedRelease_PicksFirstReachable(t *testing.T) {
	reg := &httpmock.Registry{}
	registerTagWalk(reg, "acme", "widget", []map[string]any{
		{"name": "v1.5.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
		{"name": "v1.4.0", "commit": map[string]any{"sha": "bbbbbbb2222222222222222222222222222222bb"}},
		{"name": "v1.3.0", "commit": map[string]any{"sha": "ccccccc3333333333333333333333333333333cc"}},
	})

	tl := tag.NewListerForTest(t, reg)
	rc := &fakeReachabilityChecker{results: map[string]resolve.ReachabilityStatus{
		"v1.5.0": resolve.Unreachable,
		"v1.4.0": resolve.Reachable,
	}}

	tag, sha := FindRecommendedRelease(context.Background(), tl, rc, pinpool.New(0, nil), "acme", "widget")
	if tag != "v1.4.0" {
		t.Fatalf("expected v1.4.0, got %q", tag)
	}
	if sha != "bbbbbbb2222222222222222222222222222222bb" {
		t.Fatalf("expected bbbb…bb, got %q", sha)
	}
}

// TestFindRecommendedRelease_NoneReachable returns empty when every recent tag is
// detached from a branch — signal for the caller to escalate to the publisher.
func TestFindRecommendedRelease_NoneReachable(t *testing.T) {
	reg := &httpmock.Registry{}
	registerTagWalk(reg, "acme", "widget", []map[string]any{
		{"name": "v1.2.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
		{"name": "v1.1.0", "commit": map[string]any{"sha": "bbbbbbb2222222222222222222222222222222bb"}},
	})

	tl := tag.NewListerForTest(t, reg)
	rc := &fakeReachabilityChecker{} // all Unreachable

	tag, sha := FindRecommendedRelease(context.Background(), tl, rc, pinpool.New(0, nil), "acme", "widget")
	if tag != "" || sha != "" {
		t.Fatalf("expected empty suggestion, got tag=%q sha=%q", tag, sha)
	}
}

// TestEnrichImpostorFindings_MarksSearched flags impostor findings with the
// search outcome even when no suggestion is found so renderers can surface
// the "escalate to publisher" hint.
func TestEnrichImpostorFindings_MarksSearched(t *testing.T) {
	reg := &httpmock.Registry{}
	registerTagWalk(reg, "acme", "widget", []map[string]any{
		{"name": "v1.0.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
	})

	tl := tag.NewListerForTest(t, reg)
	rc := &fakeReachabilityChecker{} // none reachable

	report := &Report{
		Workflows: []WorkflowReport{{
			Path: ".github/workflows/test.yml",
			Findings: []Finding{{
				Category:   ImpostorCommit,
				Confidence: ConfidenceHigh,
				Dependency: &dep.Dependency{NWO: "acme/widget", Ref: "v1"},
			}},
		}},
	}

	EnrichImpostorFindings(context.Background(), report, tl, rc, pinpool.New(0, nil))

	f := report.Workflows[0].Findings[0]
	if !f.RecommendedSearched {
		t.Error("expected RecommendedSearched=true after walk")
	}
	if f.RecommendedTag != "" {
		t.Errorf("expected no suggestion when nothing reachable, got %q", f.RecommendedTag)
	}
}

// TestEnrichImpostorFindings_PopulatesSuggestion attaches the discovered tag
// to the finding so downstream renderers (presentCheckResults, summary) can
// surface a concrete re-pin target.
func TestEnrichImpostorFindings_PopulatesSuggestion(t *testing.T) {
	reg := &httpmock.Registry{}
	registerTagWalk(reg, "acme", "widget", []map[string]any{
		{"name": "v1.0.0", "commit": map[string]any{"sha": "aaaaaaa1111111111111111111111111111111aa"}},
	})

	tl := tag.NewListerForTest(t, reg)
	rc := &fakeReachabilityChecker{results: map[string]resolve.ReachabilityStatus{
		"v1.0.0": resolve.Reachable,
	}}

	report := &Report{
		Workflows: []WorkflowReport{{
			Path: ".github/workflows/test.yml",
			Findings: []Finding{{
				Category:   ImpostorCommit,
				Confidence: ConfidenceHigh,
				Dependency: &dep.Dependency{NWO: "acme/widget", Ref: "v1"},
			}},
		}},
	}

	EnrichImpostorFindings(context.Background(), report, tl, rc, pinpool.New(0, nil))

	f := report.Workflows[0].Findings[0]
	if f.RecommendedTag != "v1.0.0" {
		t.Errorf("expected v1.0.0, got %q", f.RecommendedTag)
	}
	if f.RecommendedSHA != "aaaaaaa1111111111111111111111111111111aa" {
		t.Errorf("unexpected sha %q", f.RecommendedSHA)
	}
}
