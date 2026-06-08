package format

import "testing"

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
