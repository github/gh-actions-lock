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

// CheckRepoImmutableReleases scans `root` for action.yml/action.yaml
// files and, if the current repo publishes its actions via releases,
// warns when those releases are not immutable. Returns no findings when:
//   - the repo contains no action definitions
//   - the repo has zero published releases
//   - every published release is immutable
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

	path := fmt.Sprintf("repos/%s/%s/releases?per_page=30",
		url.PathEscape(owner), url.PathEscape(repo))

	var releases []struct {
		TagName   string `json:"tag_name"`
		Immutable bool   `json:"immutable"`
	}
	if err := client.Get(path, &releases); err != nil {
		return nil
	}
	if len(releases) == 0 {
		return nil
	}

	var mutableTags []string
	for _, rel := range releases {
		if !rel.Immutable {
			mutableTags = append(mutableTags, rel.TagName)
		}
	}
	if len(mutableTags) == 0 {
		return nil
	}

	display := strings.Join(actionFiles, ", ")
	preview := mutableTags
	if len(preview) > 5 {
		preview = preview[:5]
	}
	detail := fmt.Sprintf(
		"%s/%s defines %s but publishes %d non-immutable release(s) (e.g. %s); enable immutable releases so consumers can pin to verifiable tags",
		owner, repo, display, len(mutableTags), strings.Join(preview, ", "),
	)
	return []Finding{{
		Category: CategoryNonImmutableReleases,
		Severity: SeverityWarning,
		Detail:   detail,
		DocURL:   DocURLFor(CategoryNonImmutableReleases),
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
