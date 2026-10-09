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

func TestPinsFromRecord_NeverCallsABlockedPinVerified(t *testing.T) {
	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "new-owner/tool", Ref: "v1", SHA: "aaa", Resolution: pin.Pinned, RenamedFrom: "old-owner/tool@v1", Workflows: []string{"ok.yml"}},
		{NWO: "acme/replaced", Ref: "v2", SHA: "bbb", Resolution: pin.Verified, Workflows: []string{"blocked.yml"}},
		{NWO: "acme/bystander", Ref: "v3", SHA: "ccc", Resolution: pin.Verified, Workflows: []string{"blocked.yml"}},
		{NWO: "acme/shared", Ref: "v4", SHA: "ddd", Resolution: pin.Verified, Workflows: []string{"blocked.yml"}},
		{NWO: "acme/shared", Ref: "v4", SHA: "ddd", Resolution: pin.Verified, Workflows: []string{"ok.yml"}},
	}}
	report := &checks.Report{Workflows: []checks.WorkflowReport{
		{Path: "ok.yml"},
		{Path: "blocked.yml", SkipCommit: true, Findings: []checks.Finding{
			{Category: checks.RepoReplaced, Severity: checks.SeverityError, Detail: "replaced", Dependency: &dep.Dependency{NWO: "acme/replaced", Ref: "v2", SHA: "bbb"}},
			{Category: checks.RepoUnavailable, Severity: checks.SeverityError, Detail: "gone", Dependency: &dep.Dependency{NWO: "acme/fresh", Ref: "v5", SHA: "eee"}},
		}},
	}}

	got := map[string]Pin{}
	for _, p := range pinsFromRecord(record, report) {
		got[p.NWO+"@"+p.Ref] = p
	}
	require.Len(t, got, 5)
	assert.Equal(t, "pinned", got["new-owner/tool@v1"].Outcome)
	assert.Equal(t, "old-owner/tool@v1", got["new-owner/tool@v1"].RenamedFrom)
	assert.Equal(t, "blocked", got["acme/replaced@v2"].Outcome)
	assert.Equal(t, "replaced", got["acme/replaced@v2"].Reason)
	assert.Equal(t, "skipped", got["acme/bystander@v3"].Outcome)
	assert.Equal(t, "verified", got["acme/shared@v4"].Outcome, "also verified by a healthy workflow")
	assert.Equal(t, "blocked", got["acme/fresh@v5"].Outcome)
	assert.Equal(t, "eee", got["acme/fresh@v5"].ObservedSHA)
}
