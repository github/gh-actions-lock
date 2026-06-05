package runlog

import _ "embed"

// schemaV1 is the JSON Schema (draft 2020-12) describing the v1 provenance
// document shape. It is the published contract for the report WriteReport
// emits, mirroring how pkg/lockfile embeds its lockfile schema.
//
// Provenance versioning is major-only by design — there is no v1.0.1 or
// v1.1.0, only v1, v2, etc. The file is named provenance-v1.json
// (intentionally asymmetric with lockfile's semver-named lockfile-v0.0.1.json):
// any breaking change ships a new major and a new file.
//
//go:embed provenance-v1.json
var schemaV1 []byte

// Schema returns the embedded JSON Schema document for the supported
// provenance version. Callers can surface it for editor integration or
// external validation; WriteReport always emits documents that conform to it.
func Schema() []byte {
	return schemaV1
}
