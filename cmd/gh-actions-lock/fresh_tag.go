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
	"github.com/github/gh-actions-lock/internal/ui"
)

// ResolveCooldown picks the cooldown policy by precedence: Dependabot config
// (only when default-days > 0, so a repo can't downgrade a stricter file policy),
// then the user's config file, then none.
func ResolveCooldown(repoRoot string) (cfg tag.CooldownConfig, configured bool, warnings []string) {
	return fallbackCooldown(config.DependabotCooldown(repoRoot))
}

func fallbackCooldown(cfg tag.CooldownConfig, ok bool, warnings []string) (tag.CooldownConfig, bool, []string) {
	if ok {
		return cfg, true, warnings
	}
	if fileCfg, ok := config.FileCooldown(); ok {
		return fileCfg, true, warnings
	}
	return tag.CooldownConfig{}, false, warnings
}

// freshTagWindow is how recent a pinned tag must be to get flagged when the
// action has no positive cooldown configured.
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

// injectFreshTagFindings flags tags pinned this run that are younger than the
// action's freshness window: its configured cooldown when positive, otherwise
// freshTagWindow. Cooldown never filters what gets pinned, so this warning is
// the only place it takes effect. Tags without a GitHub Release have no date
// and aren't flagged.
func injectFreshTagFindings(ctx context.Context, report *checks.Report, record *pin.Record, tagger *tag.Lister, cooldownCfg tag.CooldownConfig) {
	seen := map[string]bool{} // NWO@tag → already flagged
	now := time.Now()
	for _, e := range record.Pinned() {
		owner, repo, ok := strings.Cut(e.NWO, "/")
		if !ok {
			continue
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
		finding, ok := freshTagFinding(e.NWO, tagName, iso, cooldownCfg.CooldownDays(owner, repo), now)
		if !ok {
			continue
		}
		seen[key] = true
		report.RepoFindings = append(report.RepoFindings, finding)
	}
}

// freshTagFinding builds a fresh-tag finding when iso is a release younger
// than cooldownDays (or freshTagWindow when cooldownDays <= 0). ok=false for
// an empty, unparseable, or old date.
func freshTagFinding(nwo, tagName, iso string, cooldownDays int, now time.Time) (checks.Finding, bool) {
	if iso == "" {
		return checks.Finding{}, false
	}
	released, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return checks.Finding{}, false
	}
	window := freshTagWindow
	if cooldownDays > 0 {
		window = time.Duration(cooldownDays) * 24 * time.Hour
	}
	// One age drives both the decision and the message so they can't disagree.
	age := now.Sub(released)
	if age >= window {
		return checks.Finding{}, false
	}
	detail := fmt.Sprintf(
		"%s@%s was released %s — pinned to a very recent tag with no Dependabot cooldown configured; add a github-actions `cooldown` block if you'd rather let fresh releases settle",
		nwo, tagName, tag.FormatAge(age),
	)
	if cooldownDays > 0 {
		detail = fmt.Sprintf(
			"pinned tag %s@%s was released %s, newer than your configured cooldown (%d %s); cooldown only warns and does not hold back fresh releases, so review this release before merging",
			nwo, tagName, tag.FormatAge(age), cooldownDays, ui.Pluralize(cooldownDays, "day", "days"),
		)
	}
	return checks.Finding{
		Category:   checks.FreshTag,
		Severity:   checks.SeverityInfo,
		Confidence: checks.ConfidenceHigh,
		Detail:     detail,
	}, true
}
