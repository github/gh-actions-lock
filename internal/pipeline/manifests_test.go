package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadManifest(t *testing.T) {
	const sha = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	file := parserlock.File{Dependencies: map[string]parserlock.Action{
		"Acme/Comp@v1":  {Commit: "sha1-1111111111111111111111111111111111111111", Uses: []string{"acme/child@v2", "acme/gone@v1", "acme/pinned@" + sha}},
		"acme/child@v2": {Commit: "sha1-2222222222222222222222222222222222222222"},
		"acme/pinned@abcdef0123456789abcdef0123456789abcdef01": {Commit: "sha1-abcdef0123456789abcdef0123456789abcdef01"},
	}}
	byKey := map[string]string{}
	for k := range file.Dependencies {
		p, _ := parserlock.ParsePin(k)
		byKey[pinKey(p.NWO, p.Ref)] = k
	}
	n := manifestNode{key: "Acme/Comp@v1"}
	ok := ghapi.ActionFileResult{CommitOID: "1111111111111111111111111111111111111111"}

	t.Run("a missing commit parity didn't check is unverified", func(t *testing.T) {
		assert.Error(t, readManifest(n, file, byKey, ghapi.ActionFileResult{Err: &ghapi.RefNotFoundError{Err: errors.New("gone")}}).err)
	})
	t.Run("no result is unverified", func(t *testing.T) {
		assert.Error(t, readManifest(n, file, byKey, ghapi.ActionFileResult{}).err)
	})
	t.Run("a Dockerfile is a leaf", func(t *testing.T) {
		res := ok
		res.Dockerfile = true
		rd := readManifest(n, file, byKey, res)
		assert.False(t, rd.missing)
		assert.Empty(t, rd.edges)
	})
	t.Run("edges", func(t *testing.T) {
		res := ok
		res.ActionYML = "runs:\n  using: composite\n  steps:\n" +
			"    - uses: ACME/child@v2\n" +
			"    - uses: acme/gone@v1\n" +
			"    - uses: acme/pinned/sub@" + sha + "\n" +
			"    - uses: acme/comp/other@v1\n" +
			"    - uses: $/third\n" +
			"    - uses: acme/new@v1\n"
		rd := readManifest(n, file, byKey, res)
		require.NotNil(t, rd)
		require.NoError(t, rd.err)
		want := []manifestEdge{
			{to: manifestNode{key: "acme/child@v2"}},
			{}, // recorded but dangling: the structural check reports it
			{to: manifestNode{key: "acme/pinned@abcdef0123456789abcdef0123456789abcdef01", path: "sub"}},
			{to: manifestNode{key: "Acme/Comp@v1", path: "other"}},
			{to: manifestNode{key: "Acme/Comp@v1", path: "third"}},
			{omitted: true},
		}
		require.Len(t, rd.edges, len(want))
		for i, w := range want {
			assert.Equal(t, w.to, rd.edges[i].to, rd.edges[i].use)
			assert.Equal(t, w.omitted, rd.edges[i].omitted, rd.edges[i].use)
		}
	})
}

// fakeManifests answers reads from yml by repo; a missing repo fails.
type fakeManifests struct {
	yml   map[string]string
	calls [][]string
}

func (f *fakeManifests) ReadManifests(_ context.Context, reqs []ghapi.ActionFileRequest) []ghapi.ActionFileResult {
	var repos []string
	out := make([]ghapi.ActionFileResult, len(reqs))
	for i, r := range reqs {
		repos = append(repos, r.Repo)
		y, ok := f.yml[r.Repo]
		if !ok {
			out[i] = ghapi.ActionFileResult{Err: errors.New("boom")}
			continue
		}
		out[i] = ghapi.ActionFileResult{Owner: r.Owner, Repo: r.Repo, CommitOID: r.Ref, ActionYML: y}
	}
	f.calls = append(f.calls, repos)
	return out
}

const compositeUsing = "runs:\n  using: composite\n  steps:\n"

func manifestFile(entries map[string][]string) (parserlock.File, map[string]string) {
	file := parserlock.File{Dependencies: map[string]parserlock.Action{}}
	byKey := map[string]string{}
	i := 0
	for k, uses := range entries {
		i++
		file.Dependencies[k] = parserlock.Action{Commit: fmt.Sprintf("sha1-%040d", i), Uses: uses}
		p, _ := parserlock.ParsePin(k)
		byKey[pinKey(p.NWO, p.Ref)] = k
	}
	return file, byKey
}

func TestReadManifestGraph_ProxyStopsAtFailedRead(t *testing.T) {
	tests := []struct {
		name  string
		proxy bool
		calls [][]string
	}{
		{"reads every level", false, [][]string{{"a", "c"}, {"b"}}},
		{"stops after a failed read behind the proxy", true, [][]string{{"a", "c"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			}
			file, byKey := manifestFile(map[string][]string{
				"acme/a@v1": {"acme/b@v1"},
				"acme/b@v1": nil,
				"acme/c@v1": nil,
			})
			f := &fakeManifests{yml: map[string]string{
				"a": compositeUsing + "    - uses: acme/b@v1\n",
				"b": "runs:\n  using: node20\n",
			}}
			roots := map[int][]manifestNode{0: {{key: "acme/a@v1"}}, 1: {{key: "acme/c@v1"}}}

			reads := readManifestGraph(context.Background(), f, file, byKey, roots, nil)

			assert.Equal(t, tt.calls, f.calls)
			assert.Error(t, firstManifestErr(roots, reads))
		})
	}
}

func TestCheckManifests_RejectedPins(t *testing.T) {
	const (
		omits = compositeUsing + "    - uses: acme/extra@v1\n"
		leaf  = "runs:\n  using: node20\n"
	)
	uses := func(to string) string { return compositeUsing + "    - uses: " + to + "\n" }
	type want map[int][]string // report index -> finding categories
	tests := []struct {
		name     string
		entries  map[string][]string
		yml      map[string]string // by repo; others are leaves
		fail     []string          // repos whose reads fail
		roots    map[int][]string
		rejected []string
		want     want
	}{
		{
			name:    "a healthy root's failed read and omitted edge are reported",
			entries: map[string][]string{"acme/a@v1": nil, "acme/b@v1": nil},
			yml:     map[string]string{"b": omits},
			fail:    []string{"a"},
			roots:   map[int][]string{0: {"acme/a@v1"}, 1: {"acme/b@v1"}},
			want:    want{0: {"reachability-unknown"}, 1: {"incomplete-lockfile"}},
		},
		{
			name:     "a rejected root's failed read and omitted edge are not",
			entries:  map[string][]string{"acme/a@v1": nil, "acme/b@v1": nil},
			yml:      map[string]string{"b": omits},
			fail:     []string{"a"},
			roots:    map[int][]string{0: {"acme/a@v1"}, 1: {"acme/b@v1"}},
			rejected: []string{"acme/a@v1", "acme/b@v1"},
			want:     want{},
		},
		{
			name:     "a rejected intermediate node's failed read is not reported",
			entries:  map[string][]string{"acme/a@v1": {"acme/b@v1"}, "acme/b@v1": nil},
			yml:      map[string]string{"a": uses("acme/b@v1")},
			fail:     []string{"b"},
			roots:    map[int][]string{0: {"acme/a@v1"}},
			rejected: []string{"acme/b@v1"},
			want:     want{},
		},
		{
			name:     "a rejected intermediate node's omitted edge is not reported",
			entries:  map[string][]string{"acme/a@v1": {"acme/b@v1"}, "acme/b@v1": nil},
			yml:      map[string]string{"a": uses("acme/b@v1"), "b": omits},
			roots:    map[int][]string{0: {"acme/a@v1"}},
			rejected: []string{"acme/b@v1"},
			want:     want{},
		},
		{
			name:     "a descendant reachable only through a rejected node is not checked",
			entries:  map[string][]string{"acme/a@v1": {"acme/b@v1"}, "acme/b@v1": {"acme/d@v1"}, "acme/d@v1": nil},
			yml:      map[string]string{"a": uses("acme/b@v1"), "b": uses("acme/d@v1"), "d": omits},
			roots:    map[int][]string{0: {"acme/a@v1"}},
			rejected: []string{"acme/b@v1"},
			want:     want{},
		},
		{
			name: "a shared descendant is still checked through a healthy workflow",
			entries: map[string][]string{
				"acme/r@v1": {"acme/d@v1"}, "acme/h@v1": {"acme/d@v1"}, "acme/d@v1": nil,
			},
			yml:      map[string]string{"r": uses("acme/d@v1"), "h": uses("acme/d@v1"), "d": omits},
			roots:    map[int][]string{0: {"acme/r@v1"}, 1: {"acme/h@v1"}},
			rejected: []string{"acme/r@v1"},
			want:     want{1: {"incomplete-lockfile"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, byKey := manifestFile(tt.entries)
			roots := map[int][]manifestNode{}
			report := &checks.Report{}
			for ri := range len(tt.roots) {
				report.Workflows = append(report.Workflows, checks.WorkflowReport{Path: fmt.Sprintf(".github/workflows/w%d.yml", ri)})
				for _, k := range tt.roots[ri] {
					roots[ri] = append(roots[ri], manifestNode{key: k})
				}
			}
			unhealthy := map[string]bool{}
			for _, k := range tt.rejected {
				p, _ := parserlock.ParsePin(k)
				unhealthy[pinKey(p.NWO, lockedSHA(file.Dependencies[k].Commit))] = true
			}
			yml := map[string]string{}
			for k := range tt.entries {
				p, _ := parserlock.ParsePin(k)
				yml[p.Repo] = leaf
			}
			maps.Copy(yml, tt.yml)
			for _, repo := range tt.fail {
				delete(yml, repo)
			}
			reads := readManifestGraph(context.Background(), &fakeManifests{yml: yml}, file, byKey, roots, nil)

			require.NoError(t, checkManifests(file, report, roots, unhealthy, reads, nil))

			for ri, wr := range report.Workflows {
				var got []string
				for _, f := range wr.Findings {
					got = append(got, string(f.Category))
				}
				assert.Equal(t, tt.want[ri], got, wr.Path)
			}
		})
	}
}
