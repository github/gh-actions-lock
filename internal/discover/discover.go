// Package discover implements read-only version discovery for pinned actions:
// given the lockfile inventory, it offers the highest newer tag for each pin,
// matching the current ref's precision form. It never writes.
//
// The picker mirrors dependabot-core's github_actions UpdateChecker so the
// CLI's offer equals the ref an eventual Dependabot PR would land:
// precision-preserving (a @v5 major float offers the highest major-form tag,
// a @v5.1.0 full pin offers the highest full tag), cross-major allowed,
// strictly greater than current, prereleases dropped unless the current ref
// is itself a prerelease.
//
// Parity is best-effort and rides on the same parserlock.ParseSemVer used by
// check and update — so an offered ref is guaranteed to be interpreted
// identically when fed back to `update --action nwo@<ref>`. Edges where the
// shared parser diverges from core (ref path prefixes like releases/v1, bare
// non-v majors, GitHub-Release prerelease flags vs. semver-text prereleases)
// are inherited CLI-wide, not introduced here.
package discover

import (
	"context"
	"fmt"
	"sort"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
)

// Offer is a single available upgrade for a pinned action, precision-matched
// to the form of the current ref.
type Offer struct {
	NWO          string
	CurrentRef   string
	CurrentSHA   string
	AvailableRef string
	AvailableSHA string
	Precision    string // "major" | "minor" | "full"
}

// TagLister lists a repo's tags with the commit SHA each points at. Satisfied
// by *ghapi.Client; kept as an interface so the picker is unit-testable
// without network access.
type TagLister interface {
	RepoTags(ctx context.Context, owner, repo string) ([]ghapi.RepoTag, error)
}

// precision is the granularity of a version ref: major (v5), minor (v5.1), or
// full (v5.1.0). Ordered so a smaller value is a coarser form, which the
// lower-precision fallback walks downward.
type precision int

const (
	precNone precision = iota
	precMajor
	precMinor
	precFull
)

func (p precision) String() string {
	switch p {
	case precMajor:
		return "major"
	case precMinor:
		return "minor"
	case precFull:
		return "full"
	default:
		return ""
	}
}

// formOf classifies a parsed version into its precision form. A prerelease is
// treated as full so it competes in the full-precision pass.
func formOf(sv parserlock.SemVer) precision {
	if !sv.IsStable() {
		return precFull
	}
	if sv.IsMajorOnly() {
		return precMajor
	}
	if sv.Raw == sv.MinorTag() {
		return precMinor
	}
	return precFull
}

// strictGreater reports whether cand is strictly newer than cur at the given
// precision granularity (major compares Major only, minor compares Major.Minor,
// full compares the full tuple).
func strictGreater(cand, cur parserlock.SemVer, p precision) bool {
	switch p {
	case precMajor:
		return cand.Major > cur.Major
	case precMinor:
		if cand.Major != cur.Major {
			return cand.Major > cur.Major
		}
		return cand.Minor > cur.Minor
	case precFull:
		if cand.Major != cur.Major {
			return cand.Major > cur.Major
		}
		if cand.Minor != cur.Minor {
			return cand.Minor > cur.Minor
		}
		if cand.Patch != cur.Patch {
			return cand.Patch > cur.Patch
		}
		// Same major.minor.patch: only a prerelease progression (beta.1 →
		// beta.2) or a prerelease → stable promotion is an upgrade. Defer to
		// SemVer.Greater for that ordering, but never offer the same tag.
		if cand.Raw == cur.Raw {
			return false
		}
		return cand.Greater(cur)
	default:
		return false
	}
}

type candidate struct {
	tag  ghapi.RepoTag
	sv   parserlock.SemVer
	form precision
}

// pickUpgrade returns the precision-preserving best upgrade for cur among
// candidates, along with the precision form it was offered at. It tries the
// current ref's own precision first, then falls back to coarser forms.
func pickUpgrade(cur parserlock.SemVer, tags []ghapi.RepoTag) (ghapi.RepoTag, precision, bool) {
	curPrerelease := !cur.IsStable()
	parsed := make([]candidate, 0, len(tags))
	for _, t := range tags {
		sv, ok := parserlock.ParseSemVer(t.Name)
		if !ok {
			continue
		}
		if !sv.IsStable() && !curPrerelease {
			continue // drop prereleases unless the current ref is one
		}
		parsed = append(parsed, candidate{tag: t, sv: sv, form: formOf(sv)})
	}

	curPrec := precisionOf(cur)
	// Same precision first, then descend to coarser forms (the
	// lower-precision fallback). full → minor → major.
	for p := curPrec; p >= precMajor; p-- {
		if tag, ok := pickAt(parsed, cur, p); ok {
			return tag, p, true
		}
	}
	return ghapi.RepoTag{}, precNone, false
}

// precisionOf classifies the current ref's precision form.
func precisionOf(cur parserlock.SemVer) precision {
	return formOf(cur)
}

// pickAt returns the highest candidate whose form is exactly p and which is
// strictly greater than cur at that granularity.
func pickAt(parsed []candidate, cur parserlock.SemVer, p precision) (ghapi.RepoTag, bool) {
	var best candidate
	found := false
	for _, c := range parsed {
		if c.form != p || !strictGreater(c.sv, cur, p) {
			continue
		}
		if !found || c.sv.Greater(best.sv) {
			best = c
			found = true
		}
	}
	return best.tag, found
}

// Discover returns the available upgrade for each distinct pinned (NWO, ref)
// in deps. Pins whose ref is not a parseable version (branch or bare-SHA pins)
// are skipped — there is no precision anchor to preserve. Offers are sorted
// deterministically by (NWO, current ref, current SHA).
func Discover(ctx context.Context, deps []dep.Dependency, tags TagLister) ([]Offer, error) {
	seen := make(map[string]bool, len(deps))
	var offers []Offer
	for _, d := range deps {
		key := d.NWO + "@" + d.Ref
		if seen[key] {
			continue
		}
		seen[key] = true

		cur, ok := parserlock.ParseSemVer(d.Ref)
		if !ok {
			continue
		}
		owner, repo, ok := splitNWO(d.NWO)
		if !ok {
			continue
		}
		repoTags, err := tags.RepoTags(ctx, owner, repo)
		if err != nil {
			return nil, fmt.Errorf("listing tags for %s: %w", d.NWO, err)
		}
		tag, prec, ok := pickUpgrade(cur, repoTags)
		if !ok {
			continue
		}
		offers = append(offers, Offer{
			NWO:          d.NWO,
			CurrentRef:   d.Ref,
			CurrentSHA:   d.SHA,
			AvailableRef: tag.Name,
			AvailableSHA: tag.SHA,
			Precision:    prec.String(),
		})
	}
	sort.Slice(offers, func(i, j int) bool {
		a, b := offers[i], offers[j]
		if a.NWO != b.NWO {
			return a.NWO < b.NWO
		}
		if a.CurrentRef != b.CurrentRef {
			return a.CurrentRef < b.CurrentRef
		}
		return a.CurrentSHA < b.CurrentSHA
	})
	return offers, nil
}

// splitNWO splits owner/repo. NWO carries no sub-path (dep.Dependency.NWO is
// owner/repo only), so a single slash split suffices.
func splitNWO(nwo string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(nwo, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}
