package pin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanNormalizationPrunesStaleTargetsInOneRun(t *testing.T) {
	const (
		oldSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, tt := range []struct {
		name      string
		tag       string
		recorded  bool
		workflows []string
	}{
		{name: "fresh branch head"},
		{name: "recorded branch head", recorded: true},
		{name: "fresh exact tag", tag: "v1.0.0"},
		{name: "recorded exact tag", tag: "v1.0.0", recorded: true},
		{
			name:      "stale in every workflow",
			workflows: []string{".github/workflows/ci.yml", ".github/workflows/release.yml"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			t.Chdir(dir)
			paths := tt.workflows
			if len(paths) == 0 {
				paths = []string{".github/workflows/ci.yml"}
			}
			store, err := lockfile.LoadState(dir, fakeMeta{})
			require.NoError(t, err)
			wantRef := "main"
			if tt.tag != "" {
				wantRef = tt.tag
			}
			for _, path := range paths {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/action@"+headSHA+"\n"), 0o644))
				locked := []dep.Dependency{{Hostname: "github.com", NWO: "owner/action", Ref: wantRef, SHA: oldSHA, Branch: "main", Tag: tt.tag}}
				if tt.recorded {
					locked = append(locked, dep.Dependency{Hostname: "github.com", NWO: "owner/action", Ref: headSHA, SHA: headSHA, Branch: "main", Tag: tt.tag})
				}
				require.NoError(t, store.Set(ctx, path, locked, nil, nil))
			}
			require.NoError(t, store.Save())

			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if !tt.recorded {
				reg.Register(httpmock.REST("GET", `repos/owner/action$`),
					httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
				tags := httpmock.TagListResponse()
				if tt.tag != "" {
					tags = httpmock.TagListResponse(tt.tag, headSHA)
				}
				reg.Register(httpmock.REST("GET", `repos/owner/action/tags`), httpmock.JSONResponse(tags))
				reg.Register(httpmock.REST("GET", `repos/owner/action/git/ref/heads/main`),
					httpmock.JSONResponse(map[string]any{
						"ref": "refs/heads/main", "object": map[string]any{"sha": headSHA, "type": "commit"},
					}))
			}
			pool := pinpool.New(2, nil)
			resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
			require.NoError(t, err)
			resolver.SeedFromLockfile([]dep.Dependency{{Hostname: "github.com", NWO: "owner/action", Ref: headSHA, SHA: headSHA}})

			firstFiles := make(map[string][]byte)
			files := append([]string{parserlock.Path}, paths...)
			for pass := range 2 {
				store, err := lockfile.LoadState(dir, fakeMeta{})
				require.NoError(t, err)
				report := pipeline.DiagnoseParsed(ctx, pipeline.ParseAll(paths, store), nil, store, pool)
				record, err := Plan(ctx, report, PlanOptions{
					Resolver: resolver, Tagger: tag.NewListerForTest(t, reg), Store: store, Pool: pool,
				})
				require.NoError(t, err)
				require.Len(t, record.Entries, len(paths))
				for _, entry := range record.Entries {
					assert.Equal(t, wantRef, entry.Ref)
					assert.Equal(t, headSHA, entry.SHA)
				}
				require.NoError(t, Commit(ctx, record, store, nil))
				assert.True(t, pipeline.VerifyLocalCoverage(pipeline.ParseAll(paths, store), store).IsValid())
				for _, path := range files {
					content, err := os.ReadFile(path)
					require.NoError(t, err)
					if pass == 0 {
						firstFiles[path] = content
					} else {
						assert.Equal(t, string(firstFiles[path]), string(content), "%s changed on an unchanged run", path)
					}
				}
			}
		})
	}
}

func TestPlanNormalizationRetainsSharedTargets(t *testing.T) {
	const (
		oldSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, tt := range []struct {
		name       string
		unscanned  bool
		transitive bool
		blocked    string
	}{
		{name: "used by another scanned workflow"},
		{name: "used by an unscanned workflow", unscanned: true},
		{name: "used by a scanned composite", transitive: true},
		{name: "used by an unscanned composite", transitive: true, unscanned: true},
		{name: "stale in a skipped workflow", blocked: "skip"},
		{name: "stale in a workflow with a resolver error", blocked: "resolver"},
		{name: "stale in a workflow with an invalid self ref", blocked: "self"},
		{name: "stale in a workflow with a blocked relock", blocked: "relock"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := lockfile.LoadState(t.TempDir(), fakeMeta{})
			require.NoError(t, err)
			locked := dep.Dependency{Hostname: "github.com", NWO: "owner/action", Ref: "main", SHA: oldSHA}
			changed := checks.WorkflowReport{
				Path:       ".github/workflows/changed.yml",
				ActionRefs: []parserlock.ActionRef{{Owner: "owner", Repo: "action", Ref: headSHA}},
				Inventory:  []checks.InventoryEntry{{Dep: locked, Direct: true}},
				Findings: []checks.Finding{
					{Category: checks.Stale, Dependency: &locked},
					{Category: checks.NotPinned, Severity: checks.SeverityError},
				},
			}
			require.NoError(t, store.Set(ctx, changed.Path, []dep.Dependency{locked}, nil, nil))
			protected := checks.WorkflowReport{Path: ".github/workflows/protected.yml"}
			root := locked
			deps := []dep.Dependency{locked}
			var parents dep.ParentMap
			if tt.transitive {
				root = dep.Dependency{Hostname: "github.com", NWO: "owner/composite", Ref: "v1.0.0", SHA: "cccccccccccccccccccccccccccccccccccccccc"}
				deps = append(deps, root)
				parents = dep.ParentMap{locked.Key(): {root.Key()}}
			}
			require.NoError(t, store.Set(ctx, protected.Path, deps, parents, nil))
			protected.Inventory = []checks.InventoryEntry{{Dep: root, Direct: true}}
			if tt.blocked != "" {
				protected.Findings = []checks.Finding{{Category: checks.Stale, Dependency: &locked}}
			}
			switch tt.blocked {
			case "skip":
				protected.SkipCommit = true
			case "resolver":
				protected.BlockingResolverError = true
			case "self":
				protected.Findings = append(protected.Findings, checks.Finding{Category: checks.InvalidSelfRepositoryRef})
			case "relock":
				protected.Findings = append(protected.Findings,
					checks.Finding{Category: checks.RefMoved, Dependency: &locked},
					checks.Finding{Category: checks.UnreachablePin},
				)
			}
			report := &checks.Report{Workflows: []checks.WorkflowReport{changed}}
			if !tt.unscanned {
				report.Workflows = append(report.Workflows, protected)
			}

			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.REST("GET", `repos/owner/action$`),
				httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
			reg.Register(httpmock.REST("GET", `repos/owner/action/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
			reg.Register(httpmock.REST("GET", `repos/owner/action/git/ref/heads/main`),
				httpmock.JSONResponse(map[string]any{
					"ref": "refs/heads/main", "object": map[string]any{"sha": headSHA, "type": "commit"},
				}))
			pool := pinpool.New(2, nil)
			resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
			require.NoError(t, err)
			resolver.SeedFromLockfile([]dep.Dependency{{Hostname: "github.com", NWO: "owner/action", Ref: headSHA, SHA: headSHA}})
			record, err := Plan(ctx, report, PlanOptions{
				Resolver: resolver, Tagger: tag.NewListerForTest(t, reg), Store: store, Pool: pool,
				PartialScan: tt.unscanned, Relock: tt.blocked == "relock",
			})
			require.NoError(t, err)
			require.NotEmpty(t, record.Entries)
			assert.Equal(t, []string{changed.Path}, record.Entries[0].Workflows)
			assert.Equal(t, headSHA, record.Entries[0].Ref)
			assert.Equal(t, headSHA, record.Entries[0].SHA)
			require.NotEmpty(t, record.Workflows)
			assert.Empty(t, record.Workflows[0].Rewrites)
		})
	}
}
