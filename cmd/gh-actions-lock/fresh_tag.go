package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/config"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/tag"
)

// ResolveCooldown picks the cooldown policy by precedence: Dependabot config
// (only when default-days > 0, so a repo can't downgrade a stricter file policy),
// then the user's config file, then none.
func ResolveCooldown(repoRoot string) (cfg tag.CooldownConfig, configured bool, warnings []string) {
	cfg, ok, warnings := config.DependabotCooldown(repoRoot)
	if ok {
		return cfg, true, warnings
	}
	if fileCfg, ok := config.FileCooldown(); ok {
		return fileCfg, true, warnings
	}
	return tag.CooldownConfig{}, false, warnings
}

// freshTagWindow is how recent a pinned tag must be to get flagged when no
// cooldown is configured. Same 3 days the old silent cooldown used.
const freshTagWindow = 3 * 24 * time.Hour

// appendCooldownConfigFindings surfaces Dependabot cooldown keys we don't honor
// as non-blocking findings, so ignored config never goes silent.
func appendCooldownConfigFindings(report *checks.Report, warnings []string) {
	for _, msg := range warnings {
		report.RepoFindings = append(report.RepoFindings, checks.Finding{
			Category:   checks.CooldownConfigIgnored,
			Severity:   checks.SeverityWarning,
			Confidence: checks.ConfidenceHigh,
			Detail:     msg,
		})
	}
}

// injectFreshTagFindings flags tags pinned this run that were released within
// freshTagWindow, for actions whose effective cooldown is 0 (so the resolver's
// cooldown filter didn't already gate them). An action with a positive cooldown
// is skipped: fresh releases were filtered during narrowing. Tags without a
// GitHub Release have no date and aren't flagged.
func injectFreshTagFindings(ctx context.Context, report *checks.Report, record *pin.Record, tagger *tag.Lister, cooldownCfg tag.CooldownConfig) {
	seen := map[string]bool{} // NWO@tag → already flagged
	for _, e := range record.Pinned() {
		owner, repo, ok := strings.Cut(e.NWO, "/")
		if !ok {
			continue
		}
		if cooldownCfg.CooldownDays(owner, repo) > 0 {
			continue // resolver already filtered fresh tags for this action
		}
		tagName := e.Tag
		if tagName == "" {
			tagName = e.Ref
		}
		if _, isSemver := parserlock.ParseSemVer(tagName); !isSemver {
			continue // branch or bare SHA — not a release tag
		}
		key := e.NWO + "@" + tagName
		if seen[key] {
			continue
		}

		iso := tagger.ReleaseDate(owner, repo, tagName)
		if iso == "" {
			// Populate the release-date cache once (cached + singleflighted).
			_, _ = tagger.ListTags(ctx, owner, repo)
			iso = tagger.ReleaseDate(owner, repo, tagName)
		}
		finding, ok := freshTagFinding(e.NWO, tagName, iso, time.Now())
		if !ok {
			continue
		}
		seen[key] = true
		report.RepoFindings = append(report.RepoFindings, finding)
	}
}

// freshTagFinding builds a fresh-tag finding when iso is a release within
// freshTagWindow of now. ok=false for an empty, unparseable, or old date.
func freshTagFinding(nwo, tagName, iso string, now time.Time) (checks.Finding, bool) {
	if iso == "" {
		return checks.Finding{}, false
	}
	released, err := time.Parse(time.RFC3339, iso)
	if err != nil || now.Sub(released) >= freshTagWindow {
		return checks.Finding{}, false
	}
	return checks.Finding{
		Category:   checks.FreshTag,
		Severity:   checks.SeverityInfo,
		Confidence: checks.ConfidenceHigh,
		Detail: fmt.Sprintf(
			"%s@%s was released %s — pinned to a very recent tag with no Dependabot cooldown configured; add a github-actions `cooldown` block if you'd rather let fresh releases settle",
			nwo, tagName, tag.FormatTagAge(iso),
		),
	}, true
}
