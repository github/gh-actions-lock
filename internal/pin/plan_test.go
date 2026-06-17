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
	"github.com/github/gh-actions-lock/internal/tag"
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

// TestPlanWorkflow_DoesNotNarrowTransitiveDeps verifies that a transitive
// dependency discovered from a composite action's action.yml keeps the ref the
// composite author declared, even when a narrower full-semver tag exists at the
// same commit. We only own (and may narrow) refs that literally appear in a
// workflow `uses:` line; rewriting a composite's internal refs is churn and can
// invent refs the composite never declared.
func TestPlanWorkflow_DoesNotNarrowTransitiveDeps(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compSHA := "1111111111111111111111111111111111111111"
	transSHA := "2222222222222222222222222222222222222222"

	// Depth 0: the direct composite. Its action.yml uses trans/dep@v2.
	reg.Register(
		httpmock.GraphQLForRepo("comp", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "comp/action",
					"object": map[string]any{
						"oid": compSHA,
						"file": map[string]any{"object": map[string]any{
							"text": "name: Comp\nruns:\n  using: composite\n  steps:\n    - uses: trans/dep@v2\n",
						}},
					},
				},
			},
		}),
	)
	// Depth 1: the transitive leaf.
	reg.Register(
		httpmock.GraphQLForRepo("trans", "dep"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "trans/dep",
					"object": map[string]any{
						"oid":  transSHA,
						"file": map[string]any{"object": map[string]any{"text": "name: Trans\nruns:\n  using: node20\n"}},
					},
				},
			},
		}),
	)

	// Reverse-lookup stubs for both repos: repo metadata (default branch),
	// branch listing, and tag listing. Stubbing the repo-metadata call keeps
	// DiscoverContaining on its representative path instead of the
	// default-branch-unknown fallback.
	reg.Register(
		httpmock.REST("GET", `repos/comp/action$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/comp/action/branches`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": compSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/comp/action/tags`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "v1.0.0", "commit": map[string]any{"sha": compSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/trans/dep$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/trans/dep/branches`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": transSHA}},
		}),
	)
	// trans/dep publishes a narrower full-semver tag at the same commit. If
	// narrowing were (wrongly) applied to this transitive dep, the Tagger would
	// rewrite v2 -> v2.3.4. The gate must prevent that.
	reg.Register(
		httpmock.REST("GET", `repos/trans/dep/tags`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "v2", "commit": map[string]any{"sha": transSHA}},
			map[string]any{"name": "v2.3.4", "commit": map[string]any{"sha": transSHA}},
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
				ActionRef:  &parserlock.ActionRef{Owner: "comp", Repo: "action", Ref: "v1.0.0"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
		},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "comp", Repo: "action", Ref: "v1.0.0"},
		},
	}

	opts := PlanOptions{
		Resolver: resolver,
		Pool:     pool,
		// A real Tagger makes the narrowing path live; the gate, not the
		// absence of a Tagger, is what must spare the transitive dep.
		Tagger: tag.NewListerForTest(t, reg),
	}

	result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
	require.NoError(t, err)

	byNWO := make(map[string]Entry, len(result.entries))
	for _, e := range result.entries {
		byNWO[e.NWO] = e
	}

	trans, ok := byNWO["trans/dep"]
	require.True(t, ok, "transitive dep should be pinned")
	assert.Equal(t, "v2", trans.Ref, "transitive ref must stay as the composite declared it")
	assert.NotEqual(t, "v2.3.4", trans.Ref, "transitive dep must not be narrowed")
	assert.Equal(t, transSHA, trans.SHA)
	assert.False(t, trans.Direct, "transitive dep must not be marked Direct")
	assert.Contains(t, trans.RequiredBy, "comp/action@v1.0.0")

	comp, ok := byNWO["comp/action"]
	require.True(t, ok, "direct composite should be pinned")
	assert.Equal(t, "v1.0.0", comp.Ref)
	assert.True(t, comp.Direct, "composite is a direct workflow use")
}

// TestPlanWorkflow_CrossRefTransitiveClosure verifies that a composite at
// ref "updated" whose action.yml references a sibling subpath at ref "main"
// produces the full transitive closure: the sibling (same NWO, different
// ref) and the sibling's cross-repo transitive dep all appear as entries.
func TestPlanWorkflow_CrossRefTransitiveClosure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	nestedSHA := "1111111111111111111111111111111111111111"
	simpleSHA := "2222222222222222222222222222222222222222"
	crossSHA := "3333333333333333333333333333333333333333"

	// Depth 0: nested-composite@updated. Its action.yml uses simple-composite@main.
	reg.Register(
		httpmock.GraphQLForRepo("org", "fixtures"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "org/fixtures",
					"object": map[string]any{
						"oid": nestedSHA,
						"file": map[string]any{"object": map[string]any{
							"text": "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures/simple-composite@main\n",
						}},
					},
				},
			},
		}),
	)
	// Depth 1: simple-composite@main. Its action.yml uses cross/dep@main.
	reg.Register(
		httpmock.GraphQLForRepo("org", "fixtures"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "org/fixtures",
					"object": map[string]any{
						"oid": simpleSHA,
						"file": map[string]any{"object": map[string]any{
							"text": "name: Simple\nruns:\n  using: composite\n  steps:\n    - uses: cross/dep@main\n",
						}},
					},
				},
			},
		}),
	)
	// Depth 2: cross/dep@main — leaf node.
	reg.Register(
		httpmock.GraphQLForRepo("cross", "dep"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "cross/dep",
					"object": map[string]any{
						"oid":  crossSHA,
						"file": map[string]any{"object": map[string]any{"text": "name: Cross\nruns:\n  using: node20\n"}},
					},
				},
			},
		}),
	)

	// Reverse lookup stubs — one set per unique NWO.
	// org/fixtures: branches include both "updated" and "main" tips.
	reg.Register(
		httpmock.REST("GET", `repos/org/fixtures$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/org/fixtures/branches`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": simpleSHA}},
			map[string]any{"name": "updated", "commit": map[string]any{"sha": nestedSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/org/fixtures/tags`),
		httpmock.JSONResponse([]any{}),
	)
	// cross/dep
	reg.Register(
		httpmock.REST("GET", `repos/cross/dep$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/cross/dep/branches`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": crossSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/cross/dep/tags`),
		httpmock.JSONResponse([]any{}),
	)

	pool := pinpool.New(2, nil)
	reachFn := func(_ context.Context, _, _, _, _ string) (resolve.ReachabilityStatus, string) {
		return resolve.Reachable, "test stub"
	}
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg),
		resolve.WithCheckReachabilityFunc(reachFn))
	require.NoError(t, err)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/happy-path.yml",
		Findings: []checks.Finding{
			{
				ActionRef:  &parserlock.ActionRef{Owner: "org", Repo: "fixtures", Path: "nested-composite", Ref: "updated"},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			},
		},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "org", Repo: "fixtures", Path: "nested-composite", Ref: "updated"},
		},
	}

	opts := PlanOptions{
		Resolver: resolver,
		Pool:     pool,
	}

	result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
	require.NoError(t, err)

	// Collect all entries by NWO@Ref.
	byKey := make(map[string]Entry, len(result.entries))
	for _, e := range result.entries {
		byKey[e.NWO+"@"+e.Ref] = e
	}

	// 1. org/fixtures@updated (direct, from nested-composite)
	nested, ok := byKey["org/fixtures@updated"]
	require.True(t, ok, "direct nested-composite should be pinned; got keys: %v", keys(byKey))
	assert.True(t, nested.Direct)
	assert.Equal(t, nestedSHA, nested.SHA)

	// 2. org/fixtures@main (transitive, from simple-composite — same NWO, different ref)
	simple, ok := byKey["org/fixtures@main"]
	require.True(t, ok, "transitive simple-composite@main should be pinned; got keys: %v", keys(byKey))
	assert.False(t, simple.Direct)
	assert.Equal(t, simpleSHA, simple.SHA)

	// 3. cross/dep@main (transitive, 2 levels deep — cross-repo)
	cross, ok := byKey["cross/dep@main"]
	require.True(t, ok, "2-levels-deep transitive cross/dep@main should be pinned; got keys: %v", keys(byKey))
	assert.False(t, cross.Direct)
	assert.Equal(t, crossSHA, cross.SHA)
}

func keys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
