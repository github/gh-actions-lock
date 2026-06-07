package workflowfile

import (
	"strings"

	"gopkg.in/yaml.v3"
)

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
