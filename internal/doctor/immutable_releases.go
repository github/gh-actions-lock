package doctor

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// CategoryNonImmutableReleases warns that an action defined in the
// current repo has releases that are not marked immutable. Surfaces on
// the repo, not on workflows, because releases are a repo-level property.
const CategoryNonImmutableReleases Category = "non_immutable_releases"

// ReleaseEntry identifies a release the remediator may republish as immutable.
type ReleaseEntry struct {
	ID      int64
	TagName string
	HTMLURL string
}

// ImmutableReleasesData carries enough state for the remediator to enable
// the repo-level immutability setting (if disabled) and republish each
// non-immutable release. Attached to findings of CategoryNonImmutableReleases.
type ImmutableReleasesData struct {
	Owner                string
	Repo                 string
	SettingEnabled       bool
	NonImmutableReleases []ReleaseEntry
}

// SettingsURL returns a deep link to the repo Settings page where the
// "Enable release immutability" toggle lives.
func (d *ImmutableReleasesData) SettingsURL() string {
	if d == nil || d.Owner == "" || d.Repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/settings", d.Owner, d.Repo)
}

// CheckRepoImmutableReleases scans `root` for action.yml/action.yaml
// files and, if the current repo publishes its actions via releases,
// warns when those releases are not immutable. Returns no findings when:
//   - the repo contains no action definitions
//   - the repo has zero published releases
//   - every published release is immutable
//   - the caller lacks admin access to the repo (no actionable remediation)
//   - the releases endpoint can't be reached (silent — releases are
//     optional metadata, not a blocker for the doctor flow)
//
// owner/repo identify the upstream repository whose releases to fetch.
// Pass "" for either to skip the check entirely.
func CheckRepoImmutableReleases(client *api.RESTClient, root, owner, repo string) []Finding {
	if client == nil || owner == "" || repo == "" {
		return nil
	}
	actionFiles, err := findActionFiles(root)
	if err != nil || len(actionFiles) == 0 {
		return nil
	}

	// Probe repo metadata: we need both permission (skip if not admin) and
	// the current immutability setting (so the remediator knows whether to
	// flip it first).
	var repoMeta struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
		// Field name for the repo-level immutability toggle is best-effort:
		// the API is not officially documented. Probe both candidates.
		ImmutableReleases struct {
			Enabled bool `json:"enabled"`
		} `json:"immutable_releases"`
		ReleaseImmutabilityEnabled bool `json:"release_immutability_enabled"`
	}
	repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	if err := client.Get(repoPath, &repoMeta); err != nil {
		return nil
	}
	if !repoMeta.Permissions.Admin {
		// Without admin, the user can neither enable the setting nor
		// republish releases. Stay quiet rather than nag them.
		return nil
	}
	settingEnabled := repoMeta.ImmutableReleases.Enabled || repoMeta.ReleaseImmutabilityEnabled

	path := fmt.Sprintf("repos/%s/%s/releases?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))

	var releases []struct {
		ID        int64  `json:"id"`
		TagName   string `json:"tag_name"`
		HTMLURL   string `json:"html_url"`
		Immutable bool   `json:"immutable"`
	}
	if err := client.Get(path, &releases); err != nil {
		return nil
	}
	if len(releases) == 0 {
		return nil
	}

	var mutable []ReleaseEntry
	for _, rel := range releases {
		if !rel.Immutable {
			mutable = append(mutable, ReleaseEntry{
				ID:      rel.ID,
				TagName: rel.TagName,
				HTMLURL: rel.HTMLURL,
			})
		}
	}
	if len(mutable) == 0 {
		return nil
	}

	display := strings.Join(actionFiles, ", ")
	previewNames := make([]string, 0, len(mutable))
	for _, r := range mutable {
		previewNames = append(previewNames, r.TagName)
	}
	preview := previewNames
	if len(preview) > 5 {
		preview = preview[:5]
	}
	detail := fmt.Sprintf(
		"%s/%s publishes %d of %d action release(s) without immutable tags (e.g. %s). "+
			"Republish %s as immutable releases so downstream consumers can pin to verifiable, tamper-proof versions.",
		owner, repo, len(mutable), len(releases), strings.Join(preview, ", "), display,
	)
	return []Finding{{
		Category: CategoryNonImmutableReleases,
		Severity: SeverityWarning,
		Detail:   detail,
		DocURL:   DocURLFor(CategoryNonImmutableReleases),
		ImmutableReleasesData: &ImmutableReleasesData{
			Owner:                owner,
			Repo:                 repo,
			SettingEnabled:       settingEnabled,
			NonImmutableReleases: mutable,
		},
	}}
}

// findActionFiles returns repo-relative paths to action.yml/action.yaml
// files under root, skipping common vendor and metadata directories so
// we don't flag node_modules forks.
func findActionFiles(root string) ([]string, error) {
	if root == "" {
		root = "."
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == ".git" || name == "node_modules" || name == "vendor" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name != "action.yml" && name != "action.yaml" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
