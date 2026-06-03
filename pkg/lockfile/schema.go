package lockfile

import _ "embed"

// schemaV001 is the JSON Schema (draft 2020-12) describing the v0.0.1
// dependency lockfile format. It is the published contract for the file the
// CLI writes, mirroring how actions-workflow-parser embeds its workflow
// schema. Parse enforces the structural subset of this schema (see
// validateKnownFields) directly against the YAML node tree so positions and
// error messages stay consistent with the rest of this package.
//
//go:embed lockfile-v0.0.1.json
var schemaV001 string

// Schema returns the embedded JSON Schema document for the supported lockfile
// version. Callers can surface it for editor integration or external
// validation; Parse already enforces it.
func Schema() string {
	return schemaV001
}
