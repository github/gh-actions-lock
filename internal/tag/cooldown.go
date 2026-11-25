// Package tag lists tags and classifies versions.
package tag

import (
	"context"
	"regexp"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// CooldownConfig is the configured minimum release age per action.
// DefaultDays applies to every action; RepoOverrides sets a per-repo
// (owner/name) value that wins over the default. Include and Exclude are
// Dependabot's glob filters on owner/name: when Include is set only matching
// actions get a cooldown, and Exclude always wins.
type CooldownConfig struct {
	DefaultDays   int
	RepoOverrides map[string]int
	Include       []string
	Exclude       []string
}

// CooldownDays returns the cooldown period for a repo, falling back to the default.
func (c CooldownConfig) CooldownDays(owner, repo string) int {
	nwo := owner + "/" + repo
	if matchesAny(c.Exclude, nwo) || len(c.Include) > 0 && !matchesAny(c.Include, nwo) {
		return 0
	}
	if days, ok := c.RepoOverrides[nwo]; ok {
		return days
	}
	// Override keys may be written with different owner/repo casing than the
	// canonicalized lookup; fall back to a case-insensitive match.
	want := strings.ToLower(nwo)
	for k, days := range c.RepoOverrides {
		if strings.ToLower(k) == want {
			return days
		}
	}
	return c.DefaultDays
}

// matchesAny reports whether name matches one of Dependabot's fnmatch-style
// patterns, where `*` also matches `/`. Names compare case-insensitively
// because GitHub owner and repository names do.
func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		expr := "(?i)^" + strings.ReplaceAll(strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, ".*"), `\?`, ".") + "$"
		if ok, _ := regexp.MatchString(expr, name); ok {
			return true
		}
	}
	return false
}

// CooledTag holds back a floating version ref (v4, v4.2) whose current
// release is younger than days. It returns the newest stable release in the
// ref's version family that is older than the current one and at least days
// old. ok is false when ref is not a floating version tag, its current
// release clears the cooldown or has no release date, or no older release
// qualifies; the caller then keeps the ref's current target.
func (tl *Lister) CooledTag(ctx context.Context, owner, repo, ref string, days int, now time.Time) (string, bool) {
	refSV, ok := ParseVersion(ref)
	if days <= 0 || !ok || refSV.IsFull() {
		return "", false
	}
	all, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return "", false
	}
	sha := ""
	for _, t := range all {
		if t.Name == ref {
			sha = t.SHA
			break
		}
	}
	if sha == "" {
		return "", false
	}
	current, err := tl.BestPatchTagForSHA(ctx, owner, repo, sha, ref)
	if err != nil || current == "" {
		return "", false
	}
	window := time.Duration(days) * 24 * time.Hour
	released := func(name string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339, tl.ReleaseDate(owner, repo, name))
		return t, err == nil
	}
	if at, ok := released(current); !ok || now.Sub(at) >= window {
		return "", false
	}
	currentSV, _ := ParseVersion(current)
	var best parserlock.SemVer
	found := false
	for _, t := range all {
		sv, ok := ParseVersion(t.Name)
		if !ok || !sv.IsFull() || !sv.IsStable() || !sameVersionFamily(sv, refSV) || !currentSV.Greater(sv) {
			continue
		}
		if at, ok := released(t.Name); !ok || now.Sub(at) < window {
			continue
		}
		if !found || sv.Greater(best) {
			best, found = sv, true
		}
	}
	return best.Raw, found
}
