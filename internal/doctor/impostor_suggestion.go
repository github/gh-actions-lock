package doctor

import (
	"context"

	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// reachabilityChecker is the subset of resolve.Resolver we need to verify
// that a tag's commit is reachable from a branch in the action repo. It's
// defined here so tests can stub it without spinning up a real resolver.
type reachabilityChecker interface {
	CheckReachability(ctx context.Context, owner, repo, sha, ref string) resolve.ReachabilityResult
}

// maxSaneReleaseTagsChecked bounds the per-finding tag walk so a repo with a
// long tail of unreachable tags (e.g. a publisher with months of orphaned
// releases) doesn't trigger an unbounded reachability fan-out.
const maxSaneReleaseTagsChecked = 10

// FindSaneRelease walks the action repo's tags newest-first and returns the
// first stable release whose commit is reachable from a branch. It's the
// remediation half of the CategoryImpostorCommit detection: when we flag a
// pinned SHA as orphaned, this answers "what should the user re-pin to?"
//
// Returns ("", "") when no qualifying tag is found within the bounded walk
// (e.g. the action has never tagged a reachable release, or all recent
// releases are also orphaned and the user should escalate to the publisher).
func FindSaneRelease(ctx context.Context, tl *TagLister, r reachabilityChecker, owner, repo string) (tag, sha string) {
	if tl == nil || r == nil {
		return "", ""
	}
	tags, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return "", ""
	}
	checked := 0
	for _, t := range tags {
		if t.IsMajor {
			continue
		}
		sv, ok := lockfile.ParseVersion(t.Name)
		if !ok || sv.Rest != "" {
			continue
		}
		if t.SHA == "" {
			continue
		}
		rr := r.CheckReachability(ctx, owner, repo, t.SHA, t.Name)
		if rr.Status == resolve.Reachable {
			return t.Name, t.SHA
		}
		checked++
		if checked >= maxSaneReleaseTagsChecked {
			break
		}
	}
	return "", ""
}

// EnrichImpostorFindings walks the report and attaches a sane-release
// suggestion to every CategoryImpostorCommit finding when one is available.
// Mutates findings in place. Safe to call when tl or r is nil — becomes a
// no-op so non-network code paths (tests, --offline) don't trigger lookups.
//
// Findings that have been walked are also marked via SaneSuggestionSearched
// so renderers can distinguish "didn't look" from "looked and found nothing"
// — the latter is itself useful signal (e.g. an action whose entire release
// flow detaches tag commits from any branch, warranting harder escalation
// to the publisher).
func EnrichImpostorFindings(ctx context.Context, report *Report, tl *TagLister, r reachabilityChecker) {
	if report == nil || tl == nil || r == nil {
		return
	}
	// Cache per owner/repo so multiple impostor findings against the same
	// action share a single tag walk + reachability sweep.
	type suggestion struct{ tag, sha string }
	cache := make(map[cachekey.Repo]suggestion)
	for i := range report.Workflows {
		wf := &report.Workflows[i]
		for j := range wf.Findings {
			f := &wf.Findings[j]
			if f.Category != CategoryImpostorCommit || f.Dependency == nil {
				continue
			}
			owner, repo := f.Dependency.OwnerRepo()
			if owner == "" || repo == "" {
				continue
			}
			key := cachekey.ForRepo(owner, repo)
			s, ok := cache[key]
			if !ok {
				t, sha := FindSaneRelease(ctx, tl, r, owner, repo)
				s = suggestion{tag: t, sha: sha}
				cache[key] = s
			}
			f.SaneSuggestionSearched = true
			if s.tag != "" {
				f.SaneSuggestionTag = s.tag
				f.SaneSuggestionSHA = s.sha
			}
		}
	}
}
