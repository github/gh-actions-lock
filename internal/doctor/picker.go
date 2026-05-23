package doctor

import (
	"errors"
	"fmt"
)

// pickerTag is a tag option in a tag picker, with display metadata.
type pickerTag struct {
	Name        string
	IsInstalled bool // currently pinned SHA points to this tag
	IsImmutable bool
	IsRelease   bool
	IsMajor     bool
}

// pickerAction is the result of a tag picker selection.
type pickerAction int

const (
	pickerApply         pickerAction = iota // user selected a tag
	pickerSkip                              // user chose "Skip"
	pickerShowAll                           // user chose "Show all tags"
	pickerOpenReleases                      // user chose "Open releases"
	pickerDefaultBranch                     // user chose the default branch
)

// pickerResult holds the outcome of a picker prompt.
type pickerResult struct {
	Action   pickerAction
	Tag      string // populated for pickerDefaultBranch
	TagIndex int    // index into the tag slice for pickerApply
}

// tagLabel renders a single tag option with hyperlinks and decorators.
func (rem *Remediator) tagLabel(owner, repo string, tag pickerTag, recommend bool) string {
	tagURL := TagURL(owner, repo, tag.Name)
	label := rem.output.Hyperlink(tag.Name, tagURL)
	if tag.IsInstalled {
		label += "  📌 current"
	}
	if !rem.isSameOwner(owner) {
		if tag.IsImmutable {
			label += "  🔒 immutable"
		} else if tag.IsRelease {
			label += "  (release)"
		}
	}
	if recommend {
		label += "  (recommended)"
	}
	if age := FormatTagAge(rem.tagLister.ReleaseDate(owner, repo, tag.Name)); age != "" {
		label += "  " + age
	}
	return label
}

// defaultBranchOption appends a default branch entry if this is a same-owner repo.
// Returns the index of the default branch option (-1 if not added) and the updated options slice.
func (rem *Remediator) defaultBranchOption(options []string, owner, repo string) ([]string, int) {
	if !rem.isSameOwner(owner) {
		return options, -1
	}
	info, err := rem.tagLister.GetRepoInfo(owner, repo)
	if err != nil {
		return options, -1
	}
	branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, info.DefaultBranch)
	label := rem.output.Hyperlink(info.DefaultBranch, branchURL) + "  (default branch)"
	if age := FormatTagAge(info.PushedAt); age != "" {
		label += "  last push " + age
	}
	options = append(options, label)
	return options, len(options) - 1
}

// sentinel options appended after tags + default branch.
type pickerSentinels struct {
	ShowAll      bool   // "Show all tags"
	OpenReleases string // "Open releases → URL" (empty = disabled)
}

// runPicker shows a tag selection prompt and returns the user's choice.
// tagCount is the number of tag options at the front of the options slice
// (before default branch and sentinel options).
func (rem *Remediator) runPicker(title string, options []string, tagCount int, defaultBranchIdx int, sentinels pickerSentinels) (pickerResult, error) {
	// Append sentinel options.
	if sentinels.ShowAll {
		options = append(options, "Show all tags")
	}
	if sentinels.OpenReleases != "" {
		options = append(options, fmt.Sprintf("Open releases → %s", sentinels.OpenReleases))
	}
	options = append(options, "Skip this action")

	idx, err := rem.prompter.Select(title, options)
	if err != nil {
		if errors.Is(err, ErrAborted) {
			return pickerResult{}, ErrAborted
		}
		return pickerResult{Action: pickerSkip}, nil
	}
	if idx < 0 || idx >= len(options) {
		return pickerResult{Action: pickerSkip}, nil
	}

	// Skip is always last.
	if idx == len(options)-1 {
		return pickerResult{Action: pickerSkip}, nil
	}

	// Second-to-last sentinel.
	if idx == len(options)-2 {
		if sentinels.OpenReleases != "" {
			return pickerResult{Action: pickerOpenReleases}, nil
		}
		if sentinels.ShowAll {
			return pickerResult{Action: pickerShowAll}, nil
		}
	}

	// Third-to-last when both sentinels are present.
	if sentinels.ShowAll && sentinels.OpenReleases != "" && idx == len(options)-3 {
		return pickerResult{Action: pickerShowAll}, nil
	}

	// Default branch.
	if idx == defaultBranchIdx {
		return pickerResult{Action: pickerDefaultBranch}, nil
	}

	return pickerResult{Action: pickerApply, TagIndex: idx}, nil
}
