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

// ResolveCooldown picks the release-cooldown policy by precedence: the repo's
// Dependabot config is gospel when it sets a positive default-days; else the
// user's ~/.config/gh-actions-lock config file; else no policy. A Dependabot
// cooldown of default-days <= 0 does not count as configured, so a repo can't
// use it to silently override a stricter policy from the user's file. configured
// is false only when no source sets a policy, so the caller applies the global
// fresh-tag warn instead of a silent filter. warnings carries any Dependabot
// keys we don't honor (surfaced non-blocking).
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

// freshTagWindow is the age below which a newly pinned tag is flagged when no
// Dependabot cooldown is configured. Matches the CLI's historical default so
// the nudge fires exactly where the old silent 3-day cooldown used to filter.
const freshTagWindow = 3 * 24 * time.Hour

// appendCooldownConfigFindings surfaces Dependabot cooldown keys the tool does
// not honor as non-blocking repo-level findings, so ignored config is never
// silent.
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

// injectFreshTagFindings warns about newly pinned tags released within
// freshTagWindow when no Dependabot cooldown is configured. It only inspects
// entries pinned this run (Resolution == Pinned) and relies on release dates
// already fetched during resolution, falling back to a single cached tag
// listing per repo. Tags without a GitHub Release have no published_at and are
// not flagged — the same limitation the cooldown filter has always had.
func injectFreshTagFindings(ctx context.Context, report *checks.Report, record *pin.Record, tagger *tag.Lister) {
	seen := map[string]bool{} // NWO@tag → already flagged
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
		finding, ok := freshTagFinding(e.NWO, tagName, iso, time.Now())
		if !ok {
			continue
		}
		seen[key] = true
		report.RepoFindings = append(report.RepoFindings, finding)
	}
}

// freshTagFinding builds a fresh-tag finding when iso parses to a release
// within freshTagWindow of now. It reports ok=false for an empty, unparseable,
// or old date so the caller skips it.
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
