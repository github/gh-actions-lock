package pin

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
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
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
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

func newTransitivePlanFixture(t *testing.T, compSHA, transSHA string) (*resolve.Resolver, *pinpool.Pool, *tag.Lister) {
	t.Helper()
	reg := &httpmock.Registry{}
	t.Cleanup(func() { reg.Verify(t) })

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
	reg.Register(
		httpmock.REST("GET", `repos/trans/dep/tags`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "v2", "commit": map[string]any{"sha": transSHA}},
			map[string]any{"name": "v2.3.4", "commit": map[string]any{"sha": transSHA}},
		}),
	)

	pool := pinpool.New(2, nil)
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
	require.NoError(t, err)
	return resolver, pool, tag.NewListerForTest(t, reg)
}

// TestPlanWorkflow_TransitiveDepUsesDiscoveredRef verifies that a transitive
// dependency keeps the composite's declared ref when it's a valid symbolic ref
// (tag/branch). Only bare-SHA refs are replaced by ReverseLookup's discovery.
func TestPlanWorkflow_TransitiveDepUsesDiscoveredRef(t *testing.T) {
	compSHA := "1111111111111111111111111111111111111111"
	transSHA := "2222222222222222222222222222222222222222"
	resolver, pool, tagger := newTransitivePlanFixture(t, compSHA, transSHA)

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
		Tagger:   tagger,
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
	assert.Equal(t, transSHA, trans.SHA)
	assert.False(t, trans.Direct, "transitive dep must not be marked Direct")
	assert.Contains(t, trans.RequiredBy, "comp/action@v1.0.0")

	comp, ok := byNWO["comp/action"]
	require.True(t, ok, "direct composite should be pinned")
	assert.Equal(t, "v1.0.0", comp.Ref)
	assert.True(t, comp.Direct, "composite is a direct workflow use")
}

// TestNarrowVerifiedEntries_StickyPrecision locks the fast-path narrowing
// guard: an already-recorded direct dep the user kept at an imprecise semver
// ref (v4) must not be narrowed to a full tag on a no-op re-pin, and a
// non-version ref (main) must not be narrowed either (it's an intentional
// choice). Only imprecise semver refs (v4, v4.2) are narrowing candidates.
func TestNarrowVerifiedEntries_StickyPrecision(t *testing.T) {
	const sha = "abc1230000000000000000000000000000000000"

	// A live Tagger that *would* narrow: actions/checkout publishes a full
	// semver tag at the same commit as the imprecise ref. Only the guard, not
	// the absence of a Tagger, may spare a sticky entry.
	newTagger := func(t *testing.T) (*tag.Lister, *httpmock.Registry) {
		reg := &httpmock.Registry{}
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/tags`),
			httpmock.JSONResponse([]any{
				map[string]any{"name": "v4", "commit": map[string]any{"sha": sha}},
				map[string]any{"name": "v4.2.1", "commit": map[string]any{"sha": sha}},
			}),
		)
		return tag.NewListerForTest(t, reg), reg
	}

	// Empty Findings => NeedsAttention() false => verified fast path.
	fastPathReport := func(ref string) checks.WorkflowReport {
		return checks.WorkflowReport{
			Path: ".github/workflows/ci.yml",
			Inventory: []checks.InventoryEntry{{
				Dep:    dep.Dependency{NWO: "actions/checkout", Ref: ref, SHA: sha},
				File:   ".github/workflows/ci.yml",
				Direct: true,
			}},
		}
	}

	t.Run("imprecise v4 marked sticky is left as v4", func(t *testing.T) {
		tagger, _ := newTagger(t)
		opts := PlanOptions{
			Tagger:           tagger,
			prevImpreciseNWO: map[string]bool{"actions/checkout": true},
		}

		result, err := planWorkflow(context.Background(), fastPathReport("v4"), opts, func(string) {})
		require.NoError(t, err)

		require.Len(t, result.entries, 1)
		assert.Equal(t, "v4", result.entries[0].Ref, "sticky v4 must not be narrowed")
		assert.Empty(t, result.entries[0].AutoFixedRef, "no auto-fix should be recorded")
		require.Len(t, result.wplans, 1)
		assert.Empty(t, result.wplans[0].Rewrites, "no workflow rewrite for a sticky entry")
	})

	t.Run("branch ref main is NOT narrowed", func(t *testing.T) {
		// main is not version-shaped, so narrowing must not touch it.
		// Non-version refs are intentional choices (e.g. vercel/next.js@canary).
		opts := PlanOptions{Tagger: nil, prevImpreciseNWO: map[string]bool{}}

		result, err := planWorkflow(context.Background(), fastPathReport("main"), opts, func(string) {})
		require.NoError(t, err)

		require.Len(t, result.entries, 1)
		assert.Equal(t, "main", result.entries[0].Ref, "branch ref should stay as-is")
		assert.Equal(t, "", result.entries[0].AutoFixedRef)
		require.Len(t, result.wplans, 1)
		assert.Nil(t, result.wplans[0].Rewrites)
	})
}

func TestPlanWorkflow_SelfRepositoryDependencyIsNotRewrittenOnFastPath(t *testing.T) {
	const sha = "abc1230000000000000000000000000000000000"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "v4", "commit": map[string]any{"sha": sha}},
			map[string]any{"name": "v4.2.1", "commit": map[string]any{"sha": sha}},
		}),
	)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/ci.yml",
		Findings: []checks.Finding{{
			Category:   checks.SelfRepositoryAction,
			Severity:   checks.SeverityInfo,
			Confidence: checks.ConfidenceHigh,
		}},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "actions", Repo: "checkout", Ref: "v4"},
		},
		RewriteRefs: []parserlock.ActionRef{},
		Inventory: []checks.InventoryEntry{{
			Dep:    dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: sha},
			File:   ".github/workflows/ci.yml",
			Direct: true,
		}},
	}

	result, err := planWorkflow(context.Background(), wr, PlanOptions{
		Tagger: tag.NewListerForTest(t, reg),
	}, func(string) {})
	require.NoError(t, err)

	require.Len(t, result.entries, 1)
	assert.Equal(t, "v4", result.entries[0].Ref)
	require.Len(t, result.wplans, 1)
	assert.Empty(t, result.wplans[0].Rewrites)
}

func TestPlanWorkflow_InvalidSelfRepositoryRefDoesNotMutateWorkflow(t *testing.T) {
	wr := checks.WorkflowReport{
		Path: ".github/workflows/ci.yml",
		Findings: []checks.Finding{{
			Category:   checks.InvalidSelfRepositoryRef,
			Severity:   checks.SeverityError,
			Confidence: checks.ConfidenceHigh,
		}},
		ActionRefs: []parserlock.ActionRef{
			{Owner: "actions", Repo: "checkout", Ref: "v4"},
		},
		RewriteRefs: []parserlock.ActionRef{
			{Owner: "actions", Repo: "checkout", Ref: "v4"},
		},
	}

	result, err := planWorkflow(context.Background(), wr, PlanOptions{}, func(string) {})
	require.NoError(t, err)
	assert.Empty(t, result.entries)
	assert.Empty(t, result.wplans)
}

// TestNoNarrow_BareSHA exercises bare-SHA narrowing through the full slow path
// (Resolver + ReverseLookup + Tagger), confirming that --no-narrow protects the
// SHA from rewriting and that the default path still narrows it to a tag.
func TestNoNarrow_BareSHA(t *testing.T) {
	const sha = "abc1230000000000000000000000000000000000"

	// newSlowPathFixtures wires up a Resolver (GraphQL resolve + ReverseLookup
	// branch/tag listing) and a Tagger that would narrow the SHA to v4.2.1.
	// The report has a Finding so NeedsAttention() is true and the slow path runs.
	newSlowPathFixtures := func(t *testing.T) (*resolve.Resolver, *tag.Lister, checks.WorkflowReport, *httpmock.Registry) {
		t.Helper()
		reg := &httpmock.Registry{}

		// GraphQL: resolve the bare-SHA ref to itself.
		reg.Register(
			httpmock.GraphQLForRepo("actions", "checkout"),
			httpmock.JSONResponse(map[string]any{
				"data": map[string]any{
					"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"object": map[string]any{
							"oid":  sha,
							"file": map[string]any{"object": map[string]any{"text": "name: Checkout\nruns:\n  using: node20\n"}},
						},
					},
				},
			}),
		)

		// ReverseLookup: branch + tag listing — would rewrite the SHA to v4.2.1.
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse([]any{
				map[string]any{"name": "main", "commit": map[string]any{"sha": sha}},
			}),
		)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/tags`),
			httpmock.JSONResponse([]any{
				map[string]any{"name": "v4", "commit": map[string]any{"sha": sha}},
				map[string]any{"name": "v4.2.1", "commit": map[string]any{"sha": sha}},
			}),
		)

		pool := pinpool.New(2, nil)
		resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
		require.NoError(t, err)

		tagger := tag.NewListerForTest(t, reg)

		wr := checks.WorkflowReport{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				ActionRef:  &parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: sha},
				Category:   "unpinned",
				Severity:   checks.SeverityWarning,
				Confidence: checks.ConfidenceHigh,
			}},
			ActionRefs: []parserlock.ActionRef{
				{Owner: "actions", Repo: "checkout", Ref: sha},
			},
		}

		return resolver, tagger, wr, reg
	}

	t.Run("no-narrow preserves bare SHA through ReverseLookup", func(t *testing.T) {
		resolver, tagger, wr, _ := newSlowPathFixtures(t)

		opts := PlanOptions{
			Resolver: resolver,
			Tagger:   tagger,
			Pool:     pinpool.New(2, nil),
			NoNarrow: true,
		}

		result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
		require.NoError(t, err)

		var pinned []Entry
		for _, e := range result.entries {
			if e.Resolution == Pinned {
				pinned = append(pinned, e)
			}
		}
		require.Len(t, pinned, 1, "expected exactly one pinned entry")
		assert.Equal(t, sha, pinned[0].Ref, "bare SHA must survive both narrowDirectDeps and ReverseLookup")
		assert.Empty(t, pinned[0].AutoFixedRef, "no auto-fix should be recorded")
		require.Len(t, result.wplans, 1)
		assert.Empty(t, result.wplans[0].Rewrites, "no workflow rewrite when --no-narrow")
	})

	t.Run("default narrows bare SHA to tag", func(t *testing.T) {
		resolver, tagger, wr, reg := newSlowPathFixtures(t)

		// ReverseLookup after narrowing resolves the new ref (v4.2.1) and
		// lists tags again — register a second stub for that round-trip.
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/tags`),
			httpmock.JSONResponse([]any{
				map[string]any{"name": "v4", "commit": map[string]any{"sha": sha}},
				map[string]any{"name": "v4.2.1", "commit": map[string]any{"sha": sha}},
			}),
		)

		opts := PlanOptions{
			Resolver: resolver,
			Tagger:   tagger,
			Pool:     pinpool.New(2, nil),
			NoNarrow: false,
		}

		result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
		require.NoError(t, err)

		var pinned []Entry
		for _, e := range result.entries {
			if e.Resolution == Pinned {
				pinned = append(pinned, e)
			}
		}
		require.Len(t, pinned, 1, "expected exactly one pinned entry")
		assert.Equal(t, "v4.2.1", pinned[0].Ref, "bare SHA should be narrowed to full semver tag")
		require.Len(t, result.wplans, 1)
		assert.Contains(t, result.wplans[0].Rewrites,
			"actions/checkout@"+sha,
			"rewrite map should record the original SHA ref")
	})
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
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
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
