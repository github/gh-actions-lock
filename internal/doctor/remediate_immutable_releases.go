package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
)

// handleNonImmutableReleases offers, in interactive mode only, to enable the
// repo-level "release immutability" setting and republish each existing
// non-immutable release as immutable.
//
// Non-interactive runs print a one-line hint pointing at the Settings page
// and otherwise leave the warning untouched — there is nothing safe to do
// silently for an irreversible toggle on a repo-wide setting.
func (rem *Remediator) handleNonImmutableReleases(f Finding) error {
	data := f.ImmutableReleasesData
	if data == nil || rem.client == nil {
		return nil
	}
	if !rem.prompter.IsInteractive() {
		if url := data.SettingsURL(); url != "" {
			rem.output.Detail("  enable immutability: %s", rem.output.Hyperlink(url, url))
		}
		return nil
	}

	rem.output.Blank()
	rem.output.Header("%s/%s (release immutability)", data.Owner, data.Repo)

	if !data.SettingEnabled {
		ok, err := rem.prompter.Confirm(
			fmt.Sprintf("Enable release immutability for %s/%s? (irreversible per release)", data.Owner, data.Repo),
			true,
		)
		if err != nil {
			return err
		}
		if !ok {
			rem.output.Skip("setting not enabled — skipping release republish")
			return nil
		}
		if err := rem.enableImmutableReleases(data.Owner, data.Repo); err != nil {
			rem.output.Warning("could not enable immutability via API: %s", err)
			if url := data.SettingsURL(); url != "" {
				rem.output.Detail("  enable manually: %s", rem.output.Hyperlink(url, url))
			}
			return nil
		}
		rem.output.Success("release immutability enabled for %s/%s", data.Owner, data.Repo)
	}

	for _, rel := range data.NonImmutableReleases {
		label := rel.TagName
		if rel.HTMLURL != "" {
			label = rem.output.Hyperlink(rel.TagName, rel.HTMLURL)
		}
		ok, err := rem.prompter.Confirm(
			fmt.Sprintf("Republish %s as immutable? (cannot be undone)", rel.TagName),
			true,
		)
		if err != nil {
			return err
		}
		if !ok {
			rem.output.Skip("%s skipped", label)
			continue
		}
		if err := rem.makeReleaseImmutable(data.Owner, data.Repo, rel.ID); err != nil {
			rem.output.Warning("%s: %s", rel.TagName, err)
			continue
		}
		rem.output.Success("%s republished as immutable", label)
		rem.Fixed++
	}
	return nil
}

// enableImmutableReleases attempts to flip the repo-level immutability
// setting via PATCH /repos/{owner}/{repo}. The exact field name is not
// officially documented; send both candidates so we degrade gracefully if
// the API picks one.
func (rem *Remediator) enableImmutableReleases(owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	body := map[string]any{
		"release_immutability_enabled": true,
		"immutable_releases":           map[string]any{"enabled": true},
	}
	return rem.patchJSON(path, body)
}

// makeReleaseImmutable republishes a single release as immutable.
// PATCH /repos/{owner}/{repo}/releases/{id} with {"make_immutable": true}.
func (rem *Remediator) makeReleaseImmutable(owner, repo string, id int64) error {
	path := fmt.Sprintf("repos/%s/%s/releases/%d", url.PathEscape(owner), url.PathEscape(repo), id)
	return rem.patchJSON(path, map[string]any{"make_immutable": true})
}

func (rem *Remediator) patchJSON(path string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return rem.client.Patch(path, bytes.NewReader(raw), nil)
}
