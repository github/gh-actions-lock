package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// AvailableUpdate is one offered upgrade in the `outdated --json` output. SHAs
// are bare (no hash_algo prefix), mirroring update's updated[] entries.
type AvailableUpdate struct {
	NWO          string `json:"nwo"`
	CurrentRef   string `json:"current_ref"`
	CurrentSHA   string `json:"current_sha,omitempty"`
	AvailableRef string `json:"available_ref"`
	AvailableSHA string `json:"available_sha"`
	Precision    string `json:"precision"`
}

// validOutdatedField reports whether name is a recognized `outdated --json`
// field. The selector is cosmetic: available_updates is always emitted.
func validOutdatedField(name string) bool {
	switch name {
	case "available_updates":
		return true
	default:
		return false
	}
}

// ValidateOutdatedJSONFields rejects an unknown --json selector before any
// work runs. An empty selection is valid (no JSON requested).
func ValidateOutdatedJSONFields(fieldsCSV string) error {
	if fieldsCSV == "" {
		return nil
	}
	for _, field := range strings.Split(fieldsCSV, ",") {
		field = strings.TrimSpace(field)
		if !validOutdatedField(field) {
			return fmt.Errorf("unknown JSON field %q (expected available_updates)", field)
		}
	}
	return nil
}

// WriteOutdatedJSON writes the `outdated` JSON output: cli_version,
// lockfile_version, and the always-emitted available_updates array. The
// --json selector is validated at flag-parse time but never gates the array.
func WriteOutdatedJSON(w io.Writer, updates []AvailableUpdate, cliVersion, lockfileVersion string) error {
	if updates == nil {
		updates = []AvailableUpdate{}
	}
	payload := map[string]interface{}{
		"cli_version":       cliVersion,
		"lockfile_version":  lockfileVersion,
		"available_updates": updates,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
