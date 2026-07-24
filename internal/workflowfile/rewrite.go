package workflowfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SentinelComment is prepended to workflow files managed by gh actions-lock
// so users can tell at a glance that the file's action refs are locked.
const SentinelComment = "# This workflow is managed by gh actions-lock."

// EnsureSentinel prepends the sentinel comment to the workflow content if it
// is not already present at the top of the file. The comment is placed before
// any existing content with a blank line separating it from the YAML body.
func EnsureSentinel(content []byte) []byte {
	if bytes.HasPrefix(content, []byte(SentinelComment)) {
		return content
	}

	var buf bytes.Buffer
	buf.WriteString(SentinelComment)
	buf.WriteByte('\n')

	// If the file doesn't start with a comment or blank line, add a
	// separator so the sentinel stands apart from the YAML body.
	if len(content) > 0 && content[0] != '#' && content[0] != '\n' {
		buf.WriteByte('\n')
	}
	buf.Write(content)
	return buf.Bytes()
}

// RewriteActionRefs rewrites targeted uses: refs in the original workflow
// content while preserving the surrounding formatting and comments.
func (f *File) RewriteActionRefs(replacements map[string]string) ([]byte, int, error) {
	if len(replacements) == 0 {
		return append([]byte(nil), f.Content...), 0, nil
	}

	lines := strings.Split(string(f.Content), "\n")
	changed := 0

	walkYAMLNodes(&f.root, func(keyNode, valueNode *yaml.Node) {
		if keyNode == nil || valueNode == nil || keyNode.Value != "uses" || valueNode.Kind != yaml.ScalarNode {
			return
		}
		// Skip aliases / nodes that came from anchors. Replacing one
		// anchor reference would silently change every other use site.
		if valueNode.Alias != nil || valueNode.Anchor != "" {
			return
		}

		oldValue := strings.TrimSpace(valueNode.Value)
		newValue, ok := replacements[oldValue]
		if !ok {
			// Sub-path actions: uses: owner/repo/path@ref should match
			// a rewrite keyed on owner/repo@ref (NWO-level granularity).
			newValue, ok = subpathRewriteLookup(oldValue, replacements)
		}
		if !ok || newValue == "" || newValue == oldValue {
			return
		}

		// Anchor the rewrite at the YAML node's reported (line, column)
		// rather than scanning the line for the first occurrence of
		// oldValue. The previous strings.Index(...) approach would
		// happily substitute matching text inside a YAML comment that
		// preceded the value.
		lineIndex := valueNode.Line - 1
		colIndex := valueNode.Column - 1
		if lineIndex < 0 || lineIndex >= len(lines) || colIndex < 0 {
			return
		}
		line := lines[lineIndex]
		if colIndex+len(oldValue) > len(line) {
			return
		}
		// Quoted scalars report Column at the opening quote; the actual
		// value sits one byte further in.
		if valueNode.Style == yaml.SingleQuotedStyle || valueNode.Style == yaml.DoubleQuotedStyle {
			if colIndex+1+len(oldValue) > len(line) {
				return
			}
			if line[colIndex+1:colIndex+1+len(oldValue)] != oldValue {
				return
			}
			lines[lineIndex] = line[:colIndex+1] + newValue + stripTrailingComment(line[colIndex+1+len(oldValue):])
			changed++
			return
		}
		if line[colIndex:colIndex+len(oldValue)] != oldValue {
			return
		}
		lines[lineIndex] = line[:colIndex] + newValue + stripTrailingComment(line[colIndex+len(oldValue):])
		changed++
	})

	return []byte(strings.Join(lines, "\n")), changed, nil
}

// MigrateLocalActionsToSelfRepository rewrites same-repo `./…` composite action
// references to the inherently-pinned `$/…` form. Only local paths that
// resolve to an in-repo action file are rewritten — that in-repo existence is
// the same-repo equivalence guard that makes `$/` a safe replacement for
// `./`. Local reusable workflows are never candidates (ExtractActionRefs
// excludes them). Returns the new content and the number of `uses:` lines
// changed.
func (f *File) MigrateLocalActionsToSelfRepository() ([]byte, int, error) {
	scan := f.ExtractActionRefs()
	if err := f.validateSelfRepositoryMigration(scan); err != nil {
		return append([]byte(nil), f.Content...), 0, err
	}
	if len(scan.LocalPaths) == 0 {
		return append([]byte(nil), f.Content...), 0, nil
	}

	repoRoot := findRepoRoot(f.Path)
	if repoRoot == "" {
		return append([]byte(nil), f.Content...), 0, nil
	}
	replacements := make(map[string]string, len(scan.LocalPaths))
	for _, localPath := range scan.LocalPaths {
		if !localActionExists(repoRoot, localPath) {
			continue
		}
		replacements[localPath] = selfRepositoryPrefix + strings.TrimPrefix(localPath, "./")
	}
	if len(replacements) == 0 {
		return append([]byte(nil), f.Content...), 0, nil
	}

	return f.RewriteActionRefs(replacements)
}

func (f *File) validateSelfRepositoryMigration(scan RefScan) error {
	if len(scan.SelfRepositoryRefErrs) > 0 {
		return fmt.Errorf("invalid self repository reference %q", scan.SelfRepositoryRefErrs[0])
	}
	selfScan := ScanSelfRepositoryDependencies(
		f.Path,
		scan.SelfRepositoryActionRefs,
		scan.SelfRepositoryWorkflowRefs,
	)
	if len(selfScan.SelfRepositoryRefErrs) > 0 {
		return fmt.Errorf("invalid self repository reference %q", selfScan.SelfRepositoryRefErrs[0])
	}
	if len(selfScan.Errors) > 0 {
		return fmt.Errorf("invalid self repository dependency: %s", selfScan.Errors[0])
	}
	return nil
}

// localActionExists reports whether a `./…` path resolves to an action file
// (action.yml or action.yaml) within the repo root.
func localActionExists(repoRoot, localPath string) bool {
	relPath := strings.TrimPrefix(localPath, "./")
	actionDir := filepath.Join(repoRoot, relPath)
	if !isWithinRoot(repoRoot, actionDir) {
		return false
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		actionPath := filepath.Join(actionDir, name)
		if err := ValidatePathWithinRoot(repoRoot, actionPath); err != nil {
			continue
		}
		info, err := os.Stat(actionPath)
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// subpathRewriteLookup handles sub-path actions like actions/cache/restore@ref.
func subpathRewriteLookup(usesValue string, replacements map[string]string) (string, bool) {
	atIdx := strings.LastIndex(usesValue, "@")
	if atIdx < 0 {
		return "", false
	}
	fullName := usesValue[:atIdx]
	ref := usesValue[atIdx+1:]

	parts := strings.SplitN(fullName, "/", 3)
	if len(parts) < 3 {
		return "", false // no sub-path
	}
	nwo := parts[0] + "/" + parts[1]
	subPath := parts[2]

	nwoKey := nwo + "@" + ref
	newNWOValue, ok := replacements[nwoKey]
	if !ok {
		return "", false
	}

	newAtIdx := strings.LastIndex(newNWOValue, "@")
	if newAtIdx < 0 {
		return "", false
	}
	newRef := newNWOValue[newAtIdx+1:]
	return nwo + "/" + subPath + "@" + newRef, true
}

// stripTrailingComment removes a trailing YAML comment (# ...) from the
// remainder of a line after the uses: value has been replaced.
func stripTrailingComment(tail string) string {
	idx := strings.Index(tail, "#")
	if idx < 0 {
		return tail
	}
	return strings.TrimRight(tail[:idx], " \t")
}
