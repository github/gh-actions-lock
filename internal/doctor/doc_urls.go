package doctor

// Documentation URLs for each finding Category.
//
// Parity twin of the TypeScript engine's doc URL table at
// languageservices/workflow-parser/src/lockfile/diagnostics/doc-urls.ts.
// Strings here MUST stay in sync with that table — the CLI and editor
// present the same link to users so the experience is identical.
//
// Categories that don't yet have a dedicated docs anchor fall back to
// the canonical "using third-party actions" page so users always land
// somewhere actionable.

const securityHardeningBase = "https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions"

// PublisherTagReleasesDocURL points to GitHub's guidance for action publishers
// on tagging releases from a branch. It's surfaced alongside impostor-commit
// findings to help users escalate to the action's maintainer when the pinned
// SHA is orphaned (off any branch) — a publisher behavior the consumer can't
// fix locally beyond re-pinning to a sane release.
const PublisherTagReleasesDocURL = "https://docs.github.com/en/actions/how-tos/create-and-publish-actions/manage-custom-actions#using-tags-for-release-management"

// docURLs maps every Category that can appear on a Finding to its
// documentation URL. Categories representing "no issue" (Valid, RunOnly)
// have no URL — they aren't rendered as findings.
var docURLs = map[Category]string{
	CategoryNotPinned:       securityHardeningBase + "#using-third-party-actions",
	CategorySHAAsRef:        securityHardeningBase + "#using-third-party-actions",
	CategoryRefChanged:      securityHardeningBase + "#using-third-party-actions",
	CategoryStale:           securityHardeningBase + "#using-third-party-actions",
	CategoryMisleadingSHA:   securityHardeningBase + "#using-third-party-actions",
	CategoryRefMoved:        securityHardeningBase + "#using-third-party-actions",
	CategoryLockfileForgery: securityHardeningBase + "#using-third-party-actions",
	CategoryImpostorCommit:  securityHardeningBase + "#using-third-party-actions",
}

// DocURLFor returns the documentation URL for a finding category, or ""
// when the category has no associated URL (e.g. CategoryValid).
func DocURLFor(c Category) string {
	return docURLs[c]
}

// ReleasesURL returns the GitHub releases URL for an action. When ref
// looks like a tag, links to the specific release; otherwise links to
// the releases index so users can pick one.
func ReleasesURL(owner, repo, ref string) string {
	base := "https://github.com/" + owner + "/" + repo + "/releases"
	if isLikelyTag(ref) {
		return base + "/tag/" + ref
	}
	return base
}

// isLikelyTag mirrors the heuristic in the TS doc-urls module: anything
// that isn't a full SHA and isn't a well-known branch name is treated as
// a tag. Worst case the user gets a 404 and falls back to /releases.
func isLikelyTag(ref string) bool {
	if ref == "" || ref == "main" || ref == "master" || ref == "trunk" {
		return false
	}
	if len(ref) == 40 && isHex(ref) {
		return false
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
