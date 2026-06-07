package pipeline

import (
	"sort"
	"testing"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
)

func reachResult(d dep.Dependency, status resolve.ReachabilityStatus, detail string) resolve.ReachabilityResult {
	owner, repo := d.OwnerRepo()
	return resolve.ReachabilityResult{
		Owner:  owner,
		Repo:   repo,
		Ref:    d.Ref,
		SHA:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		DepKey: d.Key(),
		Status: status,
		Detail: detail,
	}
}

func reachCategories(fs []checks.Finding) []checks.Category {
	out := make([]checks.Category, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Category)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestReachabilityComplementFindings locks the division of labor between the
// check engine (checkImpostorCommit) and the pipeline-level complement sweep.
// The engine owns the authoritative direct-Unreachable -> ImpostorCommit
// emission and stays SILENT on Unknown; the complement path must therefore
// (a) suppress direct Unreachable to avoid double-emitting impostor, and
// (b) own the Unknown warning for every dep so a rate-limit hiccup is still
// surfaced as inconclusive rather than swallowed.
func TestReachabilityComplementFindings(t *testing.T) {
	d := dep.Dependency{NWO: "actions/checkout", Ref: "v4"}
	directNWOs := map[ghapi.Repo]bool{ghapi.ForRepo("actions", "checkout"): true}
	transitiveNWOs := map[ghapi.Repo]bool{}

	cases := []struct {
		name    string
		direct  bool
		status  resolve.ReachabilityStatus
		forgery bool
		want    []checks.Category
	}{
		{
			name:   "direct unknown fails open to a warning",
			direct: true, status: resolve.ReachabilityUnknown,
			want: []checks.Category{checks.ReachabilityUnknown},
		},
		{
			name:   "direct unreachable is silent (engine owns impostor)",
			direct: true, status: resolve.Unreachable,
			want: nil,
		},
		{
			name:   "direct reachable emits nothing",
			direct: true, status: resolve.Reachable,
			want: nil,
		},
		{
			name:   "transitive unreachable emits impostor",
			direct: false, status: resolve.Unreachable,
			want: []checks.Category{checks.ImpostorCommit},
		},
		{
			name:   "transitive unreachable under forgery is suppressed",
			direct: false, status: resolve.Unreachable, forgery: true,
			want: nil,
		},
		{
			name:   "transitive unknown fails open to a warning",
			direct: false, status: resolve.ReachabilityUnknown,
			want: []checks.Category{checks.ReachabilityUnknown},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nwos := transitiveNWOs
			if tc.direct {
				nwos = directNWOs
			}
			var existing []checks.Finding
			if tc.forgery {
				dc := d
				existing = []checks.Finding{{
					Category:   checks.LockfileForgery,
					Dependency: &dc,
				}}
			}

			reach := []resolve.ReachabilityResult{reachResult(d, tc.status, "compare status: diverged")}
			got := reachabilityComplementFindings(
				".github/workflows/ci.yml",
				reach,
				[]dep.Dependency{d},
				nwos,
				nil,
				existing,
			)

			gotCats := reachCategories(got)
			if len(gotCats) != len(tc.want) {
				t.Fatalf("categories = %v, want %v", gotCats, tc.want)
			}
			for i := range gotCats {
				if gotCats[i] != tc.want[i] {
					t.Fatalf("categories = %v, want %v", gotCats, tc.want)
				}
			}

			// Every emitted finding must carry a confidence (schema invariant).
			for _, f := range got {
				if f.Confidence == "" {
					t.Errorf("finding %s missing confidence", f.Category)
				}
			}
		})
	}
}
