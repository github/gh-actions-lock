package parserlock

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Entry is the decoded form of a single workflow dependency. Workflow
// templates carry these entries on the wire as base64-encoded JSON strings
// (see EncodeEntry / DecodeEntry) so the pin and its lockfile metadata
// travel together as a single opaque []string element.
type Entry struct {
	Pin     string `json:"pin"`
	OwnerID int64  `json:"owner_id,omitempty"`
	RepoID  int64  `json:"repo_id,omitempty"`
}

// EncodeEntry encodes the entry as base64(JSON) suitable for embedding in
// a workflow template's dependencies list.
func EncodeEntry(e Entry) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeEntry decodes a string produced by EncodeEntry back into its Entry
// form.
func DecodeEntry(s string) (Entry, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Entry{}, fmt.Errorf("decode dependency entry: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, fmt.Errorf("decode dependency entry: %w", err)
	}
	return e, nil
}

// IndexKey is a convenience that parses e.Pin and returns its IndexKey
// ("OWNER/REPO@REF"). Returns "" if e.Pin is not parseable.
func (e Entry) IndexKey() string {
	pin, ok := ParsePin(e.Pin)
	if !ok {
		return ""
	}
	return pin.IndexKey()
}

// BuildIndex decodes a slice of base64(JSON) Entry strings (the on-wire
// form carried in WorkflowTemplate.ActionsDependencies) into a lookup map
// keyed by IndexKey ("OWNER/REPO@REF"). Any duplicate IndexKey is an
// error: callers receive already-deduplicated lists from the parser, so a
// duplicate here indicates corruption upstream.
func BuildIndex(deps []string) (map[string]Entry, error) {
	index := make(map[string]Entry, len(deps))
	for _, dep := range deps {
		entry, err := DecodeEntry(dep)
		if err != nil {
			return nil, err
		}
		key := entry.IndexKey()
		if key == "" {
			return nil, fmt.Errorf("invalid dependency pin in entry: %q", entry.Pin)
		}
		if _, dup := index[key]; dup {
			return nil, fmt.Errorf("duplicate dependency key: %q", key)
		}
		index[key] = entry
	}
	return index, nil
}
