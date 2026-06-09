package pipeline

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func ref(owner, repo, path, ref string) parserlock.ActionRef {
	return parserlock.ActionRef{Owner: owner, Repo: repo, Path: path, Ref: ref}
}

func mkDep(nwo, ref, sha string) dep.Dependency {
	return dep.Dependency{NWO: nwo, Ref: ref, SHA: sha}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded, unrecorded := partitionRefs(tt.pw)
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
		assert.True(t, isFullyRecorded(pw))
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
		assert.False(t, isFullyRecorded(pw))
	})

	t.Run("no refs", func(t *testing.T) {
		pw := checks.ParsedWorkflow{}
		assert.True(t, isFullyRecorded(pw))
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

	got := recordedDeps(pw, recorded)
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
