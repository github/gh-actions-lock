package pin

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindFinding(t *testing.T) {
	ref := func(owner, repo, ref string) *parserlock.ActionRef {
		return &parserlock.ActionRef{Owner: owner, Repo: repo, Ref: ref}
	}

	ff := []checks.Finding{
		{ActionRef: ref("actions", "checkout", "v4"), Category: "unpinned"},
		{ActionRef: ref("actions", "cache", "v3"), Category: "impostor", RecommendedTag: "v3.4.0", RecommendedSHA: "abc"},
		{ActionRef: ref("actions", "cache", "v3"), Category: "unpinned"},
		{Dependency: &dep.Dependency{NWO: "other/dep", Ref: "v2"}, Category: "ref-moved"},
		{Dependency: &dep.Dependency{NWO: "other/dep", Ref: "v2"}, Category: "sane", RecommendedTag: "v2.1.0"},
	}

	t.Run("matches by ActionRef NWO and ref", func(t *testing.T) {
		f := findFinding(ff, "actions/checkout", "v4")
		require.NotNil(t, f)
		assert.Equal(t, checks.Category("unpinned"), f.Category)
	})

	t.Run("prefers finding with RecommendedTag", func(t *testing.T) {
		f := findFinding(ff, "actions/cache", "v3")
		require.NotNil(t, f)
		assert.Equal(t, "v3.4.0", f.RecommendedTag)
		assert.Equal(t, "abc", f.RecommendedSHA)
	})

	t.Run("matches by Dependency NWO and ref", func(t *testing.T) {
		f := findFinding(ff, "other/dep", "v2")
		require.NotNil(t, f)
		assert.Equal(t, "v2.1.0", f.RecommendedTag)
	})

	t.Run("returns nil for no match", func(t *testing.T) {
		f := findFinding(ff, "nonexistent/action", "v1")
		assert.Nil(t, f)
	})

	t.Run("returns best match without RecommendedTag", func(t *testing.T) {
		onlyBasic := []checks.Finding{
			{ActionRef: ref("a", "b", "v1"), Category: "first"},
			{ActionRef: ref("a", "b", "v1"), Category: "second"},
		}
		f := findFinding(onlyBasic, "a/b", "v1")
		require.NotNil(t, f)
		assert.Equal(t, checks.Category("first"), f.Category, "should return the first match")
	})

	t.Run("empty findings", func(t *testing.T) {
		assert.Nil(t, findFinding(nil, "a/b", "v1"))
	})
}

func TestDropDeps(t *testing.T) {
	deps := []dep.Dependency{
		{NWO: "a/b", Ref: "v1"},
		{NWO: "c/d", Ref: "v2"},
		{NWO: "e/f", Ref: "v3"},
	}
	pm := dep.ParentMap{
		"a/b@v1": {"root"},
		"c/d@v2": {"a/b@v1"},
		"e/f@v3": {"c/d@v2"},
	}
	bad := map[string]bool{
		"c/d@v2": true,
	}

	gotDeps, gotPM := dropDeps(deps, pm, bad)

	require.Len(t, gotDeps, 2)
	assert.Equal(t, "a/b", gotDeps[0].NWO)
	assert.Equal(t, "e/f", gotDeps[1].NWO)

	assert.Contains(t, gotPM, "a/b@v1")
	assert.NotContains(t, gotPM, "c/d@v2")
	assert.Contains(t, gotPM, "e/f@v3")
}

func TestDropDeps_all_bad(t *testing.T) {
	deps := []dep.Dependency{
		{NWO: "a/b", Ref: "v1"},
	}
	pm := dep.ParentMap{
		"a/b@v1": {"root"},
	}
	bad := map[string]bool{
		"a/b@v1": true,
	}

	gotDeps, gotPM := dropDeps(deps, pm, bad)
	assert.Empty(t, gotDeps)
	assert.Empty(t, gotPM)
}

func TestDropDeps_none_bad(t *testing.T) {
	deps := []dep.Dependency{
		{NWO: "a/b", Ref: "v1"},
		{NWO: "c/d", Ref: "v2"},
	}
	pm := dep.ParentMap{
		"a/b@v1": {"root"},
		"c/d@v2": {"a/b@v1"},
	}

	gotDeps, gotPM := dropDeps(deps, pm, map[string]bool{})
	assert.Len(t, gotDeps, 2)
	assert.Len(t, gotPM, 2)
}

// TestPlanWorkflow_PartialResolutionFailure verifies that when one ref in a
// workflow fails resolution (e.g. repo not found), only the failed ref is
// marked Unresolved. The successful ref proceeds through reachability and
// pinning. This is the cascade-failure regression test.
func TestPlanWorkflow_PartialResolutionFailure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	goodSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Both refs fold into one batched query: a0=good/action resolves,
	// a1=bad/private is null (repo not found).
	reg.Register(
		httpmock.GraphQLForRepo("good", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "good/action",
					"object": map[string]any{
						"oid":  goodSHA,
						"file": map[string]any{"object": map[string]any{"text": "name: Good\nruns:\n  using: node20\n"}},
					},
				},
				"a1": nil,
			},
		}),
	)

	// Reverse lookup stubs for good/action: branch listing and tags.
	reg.Register(
		httpmock.REST("GET", `repos/good/action/branches`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": goodSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/good/action/tags`),
		httpmock.JSONResponse([]any{
			map[string]any{
				"name":   "v1",
				"commit": map[string]any{"sha": goodSHA},
			},
		}),
	)

	pool := pinpool.New(2, nil)
	reachFn := func(_ context.Context, _, _, _, _ string) (resolve.ReachabilityStatus, string) {
		return resolve.Reachable, "test stub"
	}
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg),
		resolve.WithCheckReachabilityFunc(reachFn))
	require.NoError(t, err)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/test.yml",
		Findings: []checks.Finding{
			{
				ActionRef:  &parserlock.ActionRef{Owner: "good", Repo: "action", Ref: "v1"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
			{
				ActionRef:  &parserlock.ActionRef{Owner: "bad", Repo: "private", Ref: "main"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
		},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "good", Repo: "action", Ref: "v1"},
			{Owner: "bad", Repo: "private", Ref: "main"},
		},
	}

	opts := PlanOptions{
		Resolver: resolver,
		Pool:     pool,
	}

	result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
	require.NoError(t, err)

	// Classify entries.
	var unresolved, pinned []Entry
	for _, e := range result.entries {
		switch e.Resolution {
		case Unresolved:
			unresolved = append(unresolved, e)
		case Pinned:
			pinned = append(pinned, e)
		}
	}

	// bad/private must be unresolved.
	require.Len(t, unresolved, 1, "expected exactly one unresolved entry")
	assert.Equal(t, "bad/private", unresolved[0].NWO)
	assert.Equal(t, "main", unresolved[0].Ref)
	assert.Contains(t, unresolved[0].Reason, "not found")

	// good/action must be pinned (not poisoned by the bad ref).
	require.Len(t, pinned, 1, "expected exactly one pinned entry")
	assert.Equal(t, "good/action", pinned[0].NWO)
	assert.Equal(t, goodSHA, pinned[0].SHA)
}

// TestPlanWorkflow_AllResolutionsFail verifies that when ALL refs in a
// workflow fail resolution, every finding is marked Unresolved and no
// reachability is attempted.
func TestPlanWorkflow_AllResolutionsFail(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Both refs fold into one batched query; a0=bad/one and a1=bad/two are
	// both null (repos not found).
	reg.Register(
		httpmock.GraphQLForRepo("bad", "one"),
		httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": nil, "a1": nil}}),
	)

	pool := pinpool.New(2, nil)
	reachTrap := func(_ context.Context, _, _, _, _ string) (resolve.ReachabilityStatus, string) {
		t.Fatal("reachability should not be called when all resolutions fail")
		return resolve.Unreachable, ""
	}
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg),
		resolve.WithCheckReachabilityFunc(reachTrap))
	require.NoError(t, err)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/test.yml",
		Findings: []checks.Finding{
			{
				ActionRef:  &parserlock.ActionRef{Owner: "bad", Repo: "one", Ref: "v1"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
			{
				ActionRef:  &parserlock.ActionRef{Owner: "bad", Repo: "two", Ref: "main"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
		},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "bad", Repo: "one", Ref: "v1"},
			{Owner: "bad", Repo: "two", Ref: "main"},
		},
	}

	result, err := planWorkflow(context.Background(), wr, PlanOptions{
		Resolver: resolver,
		Pool:     pool,
	}, func(string) {})
	require.NoError(t, err)

	require.Len(t, result.entries, 2)
	for _, e := range result.entries {
		assert.Equal(t, Unresolved, e.Resolution, "expected %s to be Unresolved", e.NWO)
		assert.Contains(t, e.Reason, "not found")
	}
}
