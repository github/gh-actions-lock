package checks

import (
	"context"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/tag"
)

// ReachabilityChecker is the subset of resolve.Resolver needed to verify
// that a tag's commit is reachable from a branch in the action repo.
// Defined as an interface so tests can stub without a real resolver.
type ReachabilityChecker interface {
	CheckReachability(ctx context.Context, owner, repo, sha, ref string) resolve.ReachabilityResult
}

// maxRecommendedTagsChecked bounds the per-finding tag walk so a repo with a
// long tail of unreachable tags doesn't trigger an unbounded reachability
// fan-out.
const maxRecommendedTagsChecked = 10

// FindRecommendedRelease walks the action repo's tags newest-first and returns the
// first stable release whose commit is reachable from a branch. It's the
// remediation half of the ImpostorCommit detection: when we flag a
// pinned SHA as orphaned, this answers "what should the user re-pin to?"
//
// Returns ("", "") when no qualifying tag is found within the bounded walk
// (e.g. the action has never tagged a reachable release, or all recent
// releases are also orphaned and the user should escalate to the publisher).
func FindRecommendedRelease(ctx context.Context, tl *tag.Lister, r ReachabilityChecker, pool *pinpool.Pool, owner, repo string) (recTag, sha string) {
	if tl == nil || r == nil {
		return "", ""
	}
	tags, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return "", ""
	}

	// Collect up to maxRecommendedTagsChecked candidate tags.
	type candidate struct{ tag, sha string }
	var candidates []candidate
	for _, t := range tags {
		if t.IsMajor {
			continue
		}
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok || sv.Rest != "" {
			continue
		}
		if t.SHA == "" {
			continue
		}
		candidates = append(candidates, candidate{tag: t.Name, sha: t.SHA})
		if len(candidates) >= maxRecommendedTagsChecked {
			break
		}
	}
	if len(candidates) == 0 {
		return "", ""
	}

	// Check all candidates in parallel via the shared worker pool.
	// Branch listings and singleflight inside CheckReachability coalesce
	// across workers sharing the same NWO, so the marginal cost per extra
	// SHA is roughly one GraphQL compare (~300 ms) divided by pool width.
	type indexedCandidate struct {
		idx int
		candidate
	}
	indexed := make([]indexedCandidate, len(candidates))
	for i, c := range candidates {
		indexed[i] = indexedCandidate{idx: i, candidate: c}
	}
	results := make([]resolve.ReachabilityResult, len(candidates))
	_ = pinpool.RunTyped(pool, ctx, "Checking recommended releases",
		indexed,
		func(ic indexedCandidate) string { return owner + "/" + repo + "@" + ic.tag },
		func(ctx context.Context, _ int, ic indexedCandidate) error {
			results[ic.idx] = r.CheckReachability(ctx, owner, repo, ic.sha, ic.tag)
			return nil
		},
	)

	// Return the first reachable tag in newest-first order.
	for i, rr := range results {
		if rr.Status == resolve.Reachable {
			return candidates[i].tag, candidates[i].sha
		}
	}
	return "", ""
}

// EnrichImpostorFindings walks the report and attaches a recommended release
// to every ImpostorCommit finding when one is available. Mutates
// findings in place. Safe to call when tl or r is nil — becomes a no-op so
// non-network code paths (tests, --offline) don't trigger lookups.
//
// Findings that have been walked are also marked via RecommendedSearched
// so renderers can distinguish "didn't look" from "looked and found nothing"
// — the latter is itself useful signal (e.g. an action whose entire release
// flow detaches tag commits from any branch, warranting harder escalation
// to the publisher).
func EnrichImpostorFindings(ctx context.Context, report *Report, tl *tag.Lister, r ReachabilityChecker, pool *pinpool.Pool) {
	if report == nil || tl == nil || r == nil {
		return
	}
	// Cache per owner/repo so multiple impostor findings against the same
	// action share a single tag walk + reachability sweep.
	type suggestion struct{ tag, sha string }
	cache := make(map[ghapi.Repo]suggestion)
	for i := range report.Workflows {
		wf := &report.Workflows[i]
		for j := range wf.Findings {
			f := &wf.Findings[j]
			if f.Category != ImpostorCommit || f.Dependency == nil {
				continue
			}
			owner, repo := f.Dependency.OwnerRepo()
			if owner == "" || repo == "" {
				continue
			}
			key := ghapi.ForRepo(owner, repo)
			s, ok := cache[key]
			if !ok {
				t, sha := FindRecommendedRelease(ctx, tl, r, pool, owner, repo)
				s = suggestion{tag: t, sha: sha}
				cache[key] = s
			}
			f.RecommendedSearched = true
			if s.tag != "" {
				f.RecommendedTag = s.tag
				f.RecommendedSHA = s.sha
			}
		}
	}
}
