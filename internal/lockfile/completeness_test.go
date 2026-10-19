package lockfile

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectClosure(t *testing.T) {
	const workflow = ".github/workflows/ci.yml"
	for _, tt := range []struct {
		name     string
		roots    []string
		actions  map[string]parserlock.Action
		live     func(parserlock.Pin) bool
		wantPins []string
		missing  []MissingPin
	}{
		{
			name:    "missing root",
			roots:   []string{"o/root@Main"},
			missing: []MissingPin{{Key: "o/root@Main"}},
		},
		{
			name:  "missing grandchild",
			roots: []string{"o/root@Main"},
			actions: map[string]parserlock.Action{
				"o/root@Main": {Uses: []string{"o/child@v2"}},
				"o/child@v2":  {Uses: []string{"o/leaf@v3"}},
			},
			wantPins: []string{"o/root@Main", "o/child@v2"},
			missing:  []MissingPin{{Key: "o/leaf@v3", Parent: "o/child@v2"}},
		},
		{
			name:  "shared missing child is reported once",
			roots: []string{"o/a@v1", "o/b@v1"},
			actions: map[string]parserlock.Action{
				"o/a@v1": {Uses: []string{"o/child@v2"}},
				"o/b@v1": {Uses: []string{"o/child@v2"}},
			},
			wantPins: []string{"o/a@v1", "o/b@v1"},
			missing:  []MissingPin{{Key: "o/child@v2", Parent: "o/a@v1"}},
		},
		{
			name:  "shared recorded child is visited once",
			roots: []string{"o/a@v1", "o/b@v1"},
			actions: map[string]parserlock.Action{
				"o/a@v1":     {Uses: []string{"o/child@v2"}},
				"o/b@v1":     {Uses: []string{"o/child@v2"}},
				"o/child@v2": {},
			},
			wantPins: []string{"o/a@v1", "o/b@v1", "o/child@v2"},
		},
		{
			name:  "defensive traversal terminates on cycles",
			roots: []string{"o/a@v1"},
			actions: map[string]parserlock.Action{
				"o/a@v1": {Uses: []string{"o/b@v1"}},
				"o/b@v1": {Uses: []string{"o/a@v1"}},
			},
			wantPins: []string{"o/a@v1", "o/b@v1"},
		},
		{
			name:  "unused roots are excluded",
			roots: []string{"o/old@v1", "o/current@v1"},
			actions: map[string]parserlock.Action{
				"o/old@v1":     {Uses: []string{"o/missing@v1"}},
				"o/current@v1": {},
			},
			live:     func(p parserlock.Pin) bool { return p.NWO == "o/current" },
			wantPins: []string{"o/current@v1"},
		},
		{
			name:    "ref casing is significant",
			roots:   []string{"o/root@Main"},
			actions: map[string]parserlock.Action{"o/root@main": {}},
			missing: []MissingPin{{Key: "o/root@Main"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			live := tt.live
			if live == nil {
				live = func(parserlock.Pin) bool { return true }
			}
			file := parserlock.File{
				Dependencies: tt.actions,
				Workflows: map[string][]string{
					workflow:                      tt.roots,
					".github/workflows/other.yml": {"o/unrelated-missing@v1"},
				},
			}
			pins, missing := InspectClosure(file, workflow, live)
			var keys []string
			for _, pin := range pins {
				keys = append(keys, pin.Pin.String())
			}
			assert.ElementsMatch(t, tt.wantPins, keys)
			assert.Equal(t, tt.missing, missing)
		})
	}
}

func TestPrepareRefreshPreservesUnselectedWorkflows(t *testing.T) {
	const (
		selected = ".github/workflows/selected.yml"
		other    = ".github/workflows/other.yml"
		sha      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, tt := range []struct {
		name          string
		shared        bool
		changedCommit bool
		changedEdges  bool
		missingChild  bool
		wantErr       bool
	}{
		{name: "exclusive edges can be replaced", changedEdges: true},
		{name: "unchanged shared graph stays intact", shared: true},
		{name: "shared commit cannot advance", shared: true, changedCommit: true, wantErr: true},
		{name: "shared edges from other paths are retained", shared: true, changedEdges: true},
		{name: "shared dangling edge cannot be dropped", shared: true, changedEdges: true, missingChild: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := LoadState(t.TempDir(), fakeMetadataResolver{})
			require.NoError(t, err)
			root := dep.Dependency{NWO: "o/root", Ref: "Main", SHA: sha}
			child := dep.Dependency{NWO: "o/child", Ref: "v2", SHA: sha}
			pm := dep.ParentMap{child.Key(): {root.Key()}}
			require.NoError(t, store.Set(context.Background(), selected, []dep.Dependency{root, child}, pm, map[string]bool{root.Key(): true}))
			otherDep := dep.Dependency{NWO: "o/unrelated", Ref: "v1", SHA: sha}
			if tt.shared {
				otherDep = root
			}

			require.NoError(t, store.Set(context.Background(), other, []dep.Dependency{otherDep}, nil, map[string]bool{otherDep.Key(): true}))
			if tt.missingChild {
				delete(store.File().Dependencies, child.Key())
			}
			before := store.File().Dependencies[root.Key()]
			if tt.changedCommit {
				root.SHA = newSHA
			}
			if tt.changedEdges {
				pm = nil
			}
			err = store.PrepareRefresh([]string{selected}, []dep.Dependency{root, child}, pm)
			if tt.wantErr {
				require.ErrorContains(t, err, "shared")
				assert.Equal(t, before, store.File().Dependencies[root.Key()])
			} else {
				require.NoError(t, err)
				if !tt.shared {
					assert.Empty(t, store.File().Dependencies[root.Key()].Uses)
				} else {
					assert.Equal(t, before, store.File().Dependencies[root.Key()])
				}
			}
			assert.Equal(t, []string{otherDep.Key()}, store.File().Workflows[other])
		})
	}
}

func TestPrepareRefreshRejectsIncompleteOrConflictingPlans(t *testing.T) {
	for _, tt := range []struct {
		name    string
		deps    []dep.Dependency
		parents dep.ParentMap
		wantErr string
	}{
		{
			name:    "missing child",
			deps:    []dep.Dependency{{NWO: "o/root", Ref: "Main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			parents: dep.ParentMap{"o/child@v2": {"o/root@Main"}},
			wantErr: "missing child",
		},
		{
			name:    "missing parent",
			deps:    []dep.Dependency{{NWO: "o/child", Ref: "v2", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			parents: dep.ParentMap{"o/child@v2": {"o/root@Main"}},
			wantErr: "missing parent",
		},
		{
			name: "conflicting commits for the same tarball",
			deps: []dep.Dependency{
				{NWO: "o/root", Ref: "Main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				{NWO: "O/Root", Ref: "Main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			},
			wantErr: "conflicting commits",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := LoadState(t.TempDir(), fakeMetadataResolver{})
			require.NoError(t, err)
			root := dep.Dependency{NWO: "o/root", Ref: "Main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			child := dep.Dependency{NWO: "o/child", Ref: "v2", SHA: root.SHA}
			require.NoError(t, store.Set(context.Background(), "ci.yml", []dep.Dependency{root, child},
				dep.ParentMap{child.Key(): {root.Key()}}, map[string]bool{root.Key(): true}))
			err = store.PrepareRefresh([]string{"ci.yml"}, tt.deps, tt.parents)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, "sha1-"+root.SHA, store.File().Dependencies[root.Key()].Commit)
			assert.Equal(t, []string{child.Key()}, store.File().Dependencies[root.Key()].Uses,
				"invalid plans must be rejected before clearing recorded edges")
		})
	}
}
