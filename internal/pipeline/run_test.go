package pipeline

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func ref(owner, repo, path, ref string) parserlock.ActionRef {
	return parserlock.ActionRef{Owner: owner, Repo: repo, Path: path, Ref: ref}
}

func mkDep(nwo, ref, sha string) dep.Dependency {
	return dep.Dependency{NWO: nwo, Ref: ref, SHA: sha}
}

func TestPlanFastPath(t *testing.T) {
	tests := []struct {
		name         string
		pw           checks.ParsedWorkflow
		wantResolved bool
		wantMutable  int
	}{
		{
			name: "all mutable recorded → trusted, no live resolution",
			pw: checks.ParsedWorkflow{
				Refs:         []parserlock.ActionRef{ref("actions", "checkout", "", "v4")},
				ExistingDeps: []dep.Dependency{mkDep("actions/checkout", "v4", "aaa")},
			},
			wantResolved: true,
			wantMutable:  1,
		},
		{
			name: "immutable recorded pin → forces live resolution (the #819 fix)",
			pw: checks.ParsedWorkflow{
				Refs:         []parserlock.ActionRef{ref("actions", "checkout", "", "v4.2.1")},
				ExistingDeps: []dep.Dependency{mkDep("actions/checkout", "v4.2.1", "aaa")},
			},
			wantResolved: false,
			wantMutable:  0,
		},
		{
			name: "mixed immutable + mutable → resolve live, trust the mutable one",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "v4.2.1"),
					ref("actions", "setup-go", "", "v5"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/checkout", "v4.2.1", "aaa"),
					mkDep("actions/setup-go", "v5", "bbb"),
				},
			},
			wantResolved: false,
			wantMutable:  1,
		},
		{
			name: "unrecorded ref → resolve live, nothing trusted",
			pw: checks.ParsedWorkflow{
				Refs:         []parserlock.ActionRef{ref("actions", "checkout", "", "v4")},
				ExistingDeps: nil,
			},
			wantResolved: false,
			wantMutable:  0,
		},
		{
			name:         "no refs → resolved, nothing to do",
			pw:           checks.ParsedWorkflow{},
			wantResolved: true,
			wantMutable:  0,
		},
		{
			name: "local-path workflow → resolved, refs untouched",
			pw: checks.ParsedWorkflow{
				LocalPaths: []string{"./my-local-action"},
				Refs:       []parserlock.ActionRef{ref("actions", "checkout", "", "v4.2.1")},
			},
			wantResolved: true,
			wantMutable:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planFastPath(tt.pw)
			assert.Equal(t, tt.wantResolved, plan.resolved, "resolved")
			assert.Len(t, plan.mutableRefs, tt.wantMutable, "mutableRefs")
		})
	}
}

func TestPartitionRefs(t *testing.T) {
	tests := []struct {
		name             string
		pw               checks.ParsedWorkflow
		wantRecordedLen  int
		wantUnrecordLen  int
		wantRecordedNWOs []string
	}{
		{
			name: "all recorded",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "v4"),
					ref("actions", "setup-go", "", "v5"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/checkout", "v4", "aaa"),
					mkDep("actions/setup-go", "v5", "bbb"),
				},
			},
			wantRecordedLen: 2,
			wantUnrecordLen: 0,
		},
		{
			name: "none recorded",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "v4"),
				},
				ExistingDeps: nil,
			},
			wantRecordedLen: 0,
			wantUnrecordLen: 1,
		},
		{
			name: "mixed: one recorded one not",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "v4"),
					ref("mmastrac", "mmm-matrix", "", "v1"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/checkout", "v4", "aaa"),
				},
			},
			wantRecordedLen:  1,
			wantUnrecordLen:  1,
			wantRecordedNWOs: []string{"actions/checkout"},
		},
		{
			name: "load error makes everything unrecorded",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "v4"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/checkout", "v4", "aaa"),
				},
				LoadErr: assert.AnError,
			},
			wantRecordedLen: 0,
			wantUnrecordLen: 1,
		},
		{
			name:            "empty refs",
			pw:              checks.ParsedWorkflow{},
			wantRecordedLen: 0,
			wantUnrecordLen: 0,
		},
		{
			name: "sub-action path collapses to NWO level",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "cache", "save", "v4"),
					ref("actions", "cache", "restore", "v4"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/cache", "v4", "ccc"),
				},
			},
			wantRecordedLen: 2, // both sub-actions match the dep
			wantUnrecordLen: 0,
		},
		{
			name: "bare SHA ref matches by SHA",
			pw: checks.ParsedWorkflow{
				Refs: []parserlock.ActionRef{
					ref("actions", "checkout", "", "de0fac2e4500dabe0009e67214ff5f5447ce83dd"),
				},
				ExistingDeps: []dep.Dependency{
					mkDep("actions/checkout", "v6.0.2", "de0fac2e4500dabe0009e67214ff5f5447ce83dd"),
				},
			},
			wantRecordedLen: 1,
			wantUnrecordLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded, unrecorded := tt.pw.PartitionRefs()
			assert.Len(t, recorded, tt.wantRecordedLen)
			assert.Len(t, unrecorded, tt.wantUnrecordLen)

			if len(tt.wantRecordedNWOs) > 0 {
				var gotNWOs []string
				for _, r := range recorded {
					gotNWOs = append(gotNWOs, r.Owner+"/"+r.Repo)
				}
				assert.Equal(t, tt.wantRecordedNWOs, gotNWOs)
			}
		})
	}
}

func TestIsFullyRecorded(t *testing.T) {
	t.Run("fully recorded", func(t *testing.T) {
		pw := checks.ParsedWorkflow{
			Refs: []parserlock.ActionRef{
				ref("actions", "checkout", "", "v4"),
			},
			ExistingDeps: []dep.Dependency{
				mkDep("actions/checkout", "v4", "aaa"),
			},
		}
		assert.True(t, pw.IsFullyRecorded())
	})

	t.Run("partially recorded", func(t *testing.T) {
		pw := checks.ParsedWorkflow{
			Refs: []parserlock.ActionRef{
				ref("actions", "checkout", "", "v4"),
				ref("actions", "setup-go", "", "v5"),
			},
			ExistingDeps: []dep.Dependency{
				mkDep("actions/checkout", "v4", "aaa"),
			},
		}
		assert.False(t, pw.IsFullyRecorded())
	})

	t.Run("no refs", func(t *testing.T) {
		pw := checks.ParsedWorkflow{}
		assert.True(t, pw.IsFullyRecorded())
	})
}

func TestRecordedDeps(t *testing.T) {
	pw := checks.ParsedWorkflow{
		ExistingDeps: []dep.Dependency{
			mkDep("actions/checkout", "v4", "aaa"),
			mkDep("actions/setup-go", "v5", "bbb"),
			mkDep("third-party/tool", "v1", "ccc"),
		},
	}
	recorded := []parserlock.ActionRef{
		ref("actions", "checkout", "", "v4"),
		ref("third-party", "tool", "", "v1"),
	}

	got := pw.RecordedDeps(recorded)
	assert.Len(t, got, 2)

	keys := make(map[string]bool)
	for _, d := range got {
		keys[d.Key()] = true
	}
	assert.True(t, keys["actions/checkout@v4"])
	assert.True(t, keys["third-party/tool@v1"])
	assert.False(t, keys["actions/setup-go@v5"])
}

func TestCollectUnrecordedResolvable(t *testing.T) {
	parsed := []checks.ParsedWorkflow{
		{
			Refs: []parserlock.ActionRef{
				ref("actions", "checkout", "", "v4"),
				ref("actions", "setup-go", "", "v5"),
				ref("mmastrac", "mmm-matrix", "", "v1"),
			},
			ExistingDeps: []dep.Dependency{
				mkDep("actions/checkout", "v4", "aaa"),
				mkDep("actions/setup-go", "v5", "bbb"),
				mkDep("mmastrac/mmm-matrix", "v1", "ccc"),
			},
		},
	}

	recordedKeys := map[string]bool{
		"actions/checkout@v4": true,
		"actions/setup-go@v5": true,
	}

	refs, deps := CollectUnrecordedResolvable(parsed, recordedKeys)

	// Only the unrecorded ref should be collected.
	assert.Len(t, refs, 1)
	assert.Equal(t, "mmastrac", refs[0].Owner)
	assert.Equal(t, "mmm-matrix", refs[0].Repo)

	// Only the unrecorded dep should be collected.
	assert.Len(t, deps, 1)
	assert.Equal(t, "mmastrac/mmm-matrix", deps[0].NWO)
}

func TestCollectUnrecordedResolvable_NilExclude(t *testing.T) {
	parsed := []checks.ParsedWorkflow{
		{
			Refs: []parserlock.ActionRef{
				ref("actions", "checkout", "", "v4"),
				ref("actions", "setup-go", "", "v5"),
			},
			ExistingDeps: []dep.Dependency{
				mkDep("actions/checkout", "v4", "aaa"),
			},
		},
	}

	// With nil recordedKeys, CollectResolvable and CollectUnrecordedResolvable
	// should return the same results.
	refsAll, depsAll := CollectResolvable(parsed)
	refsFiltered, depsFiltered := CollectUnrecordedResolvable(parsed, nil)

	assert.Equal(t, refsAll, refsFiltered)
	assert.Equal(t, depsAll, depsFiltered)
}
