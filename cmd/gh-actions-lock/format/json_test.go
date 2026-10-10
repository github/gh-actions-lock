package format

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONHostnameOnlyForProximaDotcomDependencies(t *testing.T) {
	for _, tt := range []struct {
		name, homeHost, depHost, want string
	}{
		{"dotcom", "github.com", "github.com", ""},
		{"tenant local", "tenant.ghe.com", "tenant.ghe.com", ""},
		{"tenant public", "tenant.ghe.com", "github.com", "github.com"},
		{"unresolved", "tenant.ghe.com", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := &checks.Report{Workflows: []checks.WorkflowReport{{
				Path: ".github/workflows/ci.yml",
				Inventory: []checks.InventoryEntry{{
					Dep:    dep.Dependency{Hostname: tt.depHost, NWO: "o/r", Ref: "v1", SHA: "abc"},
					Direct: true,
				}},
			}}}
			var out bytes.Buffer
			require.NoError(t, WriteJSON(&out, report, nil, true, "dependencies,workflows", "dev", "v0.0.3", tt.homeHost))
			var payload struct {
				Dependencies []map[string]any `json:"dependencies"`
				Workflows    []struct {
					Dependencies []map[string]any `json:"dependencies"`
				} `json:"workflows"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
			require.Len(t, payload.Dependencies, 1)
			require.Len(t, payload.Workflows, 1)
			require.Len(t, payload.Workflows[0].Dependencies, 1)
			for _, entry := range []map[string]any{payload.Dependencies[0], payload.Workflows[0].Dependencies[0]} {
				if tt.want == "" {
					assert.NotContains(t, entry, "hostname")
				} else {
					assert.Equal(t, tt.want, entry["hostname"])
				}
			}
			assert.Equal(t, tt.depHost, report.Workflows[0].Inventory[0].Dep.Hostname)
		})
	}
}

func TestValidateJSONFields(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		wantErr bool
	}{
		{name: "empty is allowed", fields: "", wantErr: false},
		{name: "single valid field", fields: "findings", wantErr: false},
		{name: "all valid fields", fields: "valid,findings,workflows,dependencies", wantErr: false},
		{name: "valid fields with surrounding spaces", fields: " valid , findings ", wantErr: false},
		{name: "unknown field is rejected", fields: "foo", wantErr: true},
		{name: "unknown field mixed with valid", fields: "findings,foo", wantErr: true},
		{name: "empty segment from trailing comma is rejected", fields: "findings,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJSONFields(tt.fields)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateJSONFields(%q) = nil, want error", tt.fields)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateJSONFields(%q) = %v, want nil", tt.fields, err)
			}
		})
	}
}

func TestPinsFromRecordBlockedWorkflow(t *testing.T) {
	report := &checks.Report{Workflows: []checks.WorkflowReport{
		{Path: "blocked.yml", SkipCommit: true},
		{Path: "ok.yml"},
	}}
	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "o/only-blocked", Ref: "v1", SHA: "a", Resolution: pin.Verified, Workflows: []string{"blocked.yml"}},
		{NWO: "o/shared", Ref: "v1", SHA: "b", Resolution: pin.Verified, Workflows: []string{"blocked.yml"}},
		{NWO: "o/shared", Ref: "v1", SHA: "b", Resolution: pin.Verified, Workflows: []string{"ok.yml"}},
		{NWO: "o/fresh", Ref: "v1", SHA: "c", Resolution: pin.Pinned, Workflows: []string{"ok.yml"}},
	}}

	got := map[string]string{}
	for _, p := range pinsFromRecord(record, report) {
		got[p.NWO] = p.Outcome
	}

	assert.Equal(t, "skipped", got["o/only-blocked"], "pin used only by a blocked workflow was not checked")
	assert.Equal(t, "verified", got["o/shared"], "pin also used by a healthy workflow was checked")
	assert.Equal(t, "pinned", got["o/fresh"])
}
