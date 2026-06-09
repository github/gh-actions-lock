package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// UpdatedAction is one action ref bump in the `update --json=updated` output.
// One entry per distinct (nwo, old_ref, old_sha, new_ref, new_sha) across all
// saved workflows.
type UpdatedAction struct {
	NWO    string `json:"nwo"`
	OldRef string `json:"old_ref"`
	NewRef string `json:"new_ref"`
	OldSHA string `json:"old_sha,omitempty"`
	NewSHA string `json:"new_sha"`
}

// UpdatedWorkflow is one workflow file that was saved during the relock.
type UpdatedWorkflow struct {
	Path string `json:"path"`
}

// UpdateResult is the serialized outcome of an `update` run: the three
// independent diagnostic arrays plus the validity verdict.
type UpdateResult struct {
	Updated   []UpdatedAction
	Workflows []UpdatedWorkflow
	Findings  []Finding
	Valid     bool
}

// validUpdateField reports whether name is a recognized `update --json` field.
func validUpdateField(name string) bool {
	switch name {
	case "updated", "findings", "workflows", "valid":
		return true
	default:
		return false
	}
}

// ValidateUpdateJSONFields rejects an unknown --json selector before any work
// runs. An empty selection is valid (no JSON requested).
func ValidateUpdateJSONFields(fieldsCSV string) error {
	if fieldsCSV == "" {
		return nil
	}
	for _, field := range strings.Split(fieldsCSV, ",") {
		field = strings.TrimSpace(field)
		if !validUpdateField(field) {
			return fmt.Errorf("unknown JSON field %q (expected updated, findings, workflows, valid)", field)
		}
	}
	return nil
}

// WriteUpdateJSON writes the `update` JSON output. cli_version,
// lockfile_version, valid, and all three arrays (updated, findings, workflows)
// are always emitted: the three arrays are always-on diagnostics per the
// contract. The --json selector is accepted/validated but does not gate any
// array; the consumer always reads `updated`.
func WriteUpdateJSON(w io.Writer, res UpdateResult, fieldsCSV, cliVersion, lockfileVersion string) error {
	findings := res.Findings
	if findings == nil {
		findings = []Finding{}
	}
	workflows := res.Workflows
	if workflows == nil {
		workflows = []UpdatedWorkflow{}
	}
	updated := res.Updated
	if updated == nil {
		updated = []UpdatedAction{}
	}

	payload := map[string]interface{}{
		"cli_version":      cliVersion,
		"lockfile_version": lockfileVersion,
		"valid":            res.Valid,
		"updated":          updated,
		"findings":         findings,
		"workflows":        workflows,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
