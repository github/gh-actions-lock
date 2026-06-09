// Package discover lists the upgrade targets available for a pinned action.
// Given an action's NWO and current ref, it returns the repo's tags that are
// candidate next versions, ordered newest-first, for an interactive picker on
// `update` to present. It never writes.
//
// Ordering uses semver as the heuristic: tags that parse as semver
// (parserlock.ParseSemVer, the same parser check and update use) rank by semver
// descending; tags that are not semver are appended in the order the tag
// listing returned them (release order). When the current ref is itself a
// version, only strictly-greater semver tags are offered; when it is not
// version-like (a branch or bare SHA) there is no ordering anchor, so every tag
// is offered and the human chooses.
package discover

import (
	"context"
	"fmt"
	"sort"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/ghapi"
)

// Candidate is one selectable upgrade target: a tag ref and the commit SHA it
// points at. The picker relocks to Ref; SHA is shown for context.
type Candidate struct {
	Ref string
	SHA string
}

// TagLister lists a repo's tags with the commit SHA each points at. Satisfied
// by *ghapi.Client; an interface so the picker is unit-testable without
// network access.
type TagLister interface {
	RepoTags(ctx context.Context, owner, repo string) ([]ghapi.RepoTag, error)
}

// Candidates returns the upgrade targets for nwo from currentRef, ordered
// newest-first. Semver tags rank by semver descending and come first;
// non-semver tags follow in release (listing) order. When currentRef parses as
// a version, only strictly-greater semver tags are kept (prereleases dropped
// unless currentRef is itself a prerelease). When currentRef is not a version,
// no strict-greater filter applies — every tag is a candidate. The current ref
// is never offered as its own upgrade.
func Candidates(ctx context.Context, nwo, currentRef string, tags TagLister) ([]Candidate, error) {
	owner, repo, ok := splitNWO(nwo)
	if !ok {
		return nil, fmt.Errorf("invalid action %q: expected owner/repo", nwo)
	}
	repoTags, err := tags.RepoTags(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("listing tags for %s: %w", nwo, err)
	}

	cur, curIsVersion := parserlock.ParseSemVer(currentRef)
	curPrerelease := curIsVersion && !cur.IsStable()

	type semverTag struct {
		tag ghapi.RepoTag
		sv  parserlock.SemVer
	}
	var versioned []semverTag
	var others []Candidate // non-semver tags, in listing order

	for _, t := range repoTags {
		if t.Name == currentRef {
			continue // never offer the current ref as its own upgrade
		}
		sv, isVer := parserlock.ParseSemVer(t.Name)
		if !isVer {
			others = append(others, Candidate{Ref: t.Name, SHA: t.SHA})
			continue
		}
		if !sv.IsStable() && !curPrerelease {
			continue // drop prereleases unless the current ref is one
		}
		if curIsVersion && !strictlyGreater(sv, cur) {
			continue // only strictly-newer versions when anchored
		}
		versioned = append(versioned, semverTag{tag: t, sv: sv})
	}

	sort.SliceStable(versioned, func(i, j int) bool {
		return versioned[i].sv.Greater(versioned[j].sv)
	})

	out := make([]Candidate, 0, len(versioned)+len(others))
	for _, v := range versioned {
		out = append(out, Candidate{Ref: v.tag.Name, SHA: v.tag.SHA})
	}
	out = append(out, others...)
	return out, nil
}

// strictlyGreater reports whether cand is a newer version than cur, using the
// shared parser's ordering (major.minor.patch then prerelease), and never
// treating an identical raw ref as an upgrade.
func strictlyGreater(cand, cur parserlock.SemVer) bool {
	if cand.Raw == cur.Raw {
		return false
	}
	return cand.Greater(cur)
}

// splitNWO splits owner/repo. NWO carries no sub-path, so a single slash split
// suffices.
func splitNWO(nwo string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(nwo, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}
