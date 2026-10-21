package pipeline

// PublisherTagReleasesDocURL points to GitHub's guidance for action publishers
// on tagging releases from a branch. It's surfaced alongside unreachable-pin
// findings to help users escalate to the action's maintainer when the pinned
// SHA is orphaned (off any branch) — a publisher behavior the consumer can't
// fix locally beyond re-pinning to a sane release.
const PublisherTagReleasesDocURL = "https://docs.github.com/en/actions/how-tos/create-and-publish-actions/manage-custom-actions#using-tags-for-release-management"

// PublisherEscalationCopy is the standardized one-liner shown in any block
// where a SHA fell off-branch on the publisher side. Phrased as direct
// guidance so users know what to do next: ask the maintainer to tag from
// a branch.
const PublisherEscalationCopy = "Ask the action maintainer to tag releases from a branch"

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
