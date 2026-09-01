package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyVerifyFlags(t *testing.T) {
	tests := []struct {
		name      string
		opts      checkOptions
		wantNoFix bool
	}{
		{
			name:      "verify sets noFix",
			opts:      checkOptions{verify: true},
			wantNoFix: true,
		},
		{
			name:      "no verify leaves flags alone",
			opts:      checkOptions{},
			wantNoFix: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyVerifyFlags(&tt.opts)
			assert.Equal(t, tt.wantNoFix, tt.opts.noFix)
		})
	}
}

func TestValidateOutputFlags_VerifyConflicts(t *testing.T) {
	tests := []struct {
		name    string
		opts    checkOptions
		wantErr string
	}{
		{
			name:    "verify and verify-local are mutually exclusive",
			opts:    checkOptions{verify: true, verifyLocal: true},
			wantErr: "mutually exclusive",
		},
		{
			name:    "verify-local and accept-moved conflict",
			opts:    checkOptions{verifyLocal: true, acceptMoved: true},
			wantErr: "offline",
		},
		{
			name:    "verify-local and relock conflict",
			opts:    checkOptions{verifyLocal: true, relock: true},
			wantErr: "offline",
		},
		{
			name: "verify alone is valid",
			opts: checkOptions{verify: true},
		},
		{
			name: "verify-local alone is valid",
			opts: checkOptions{verifyLocal: true},
		},
		{
			name: "neither is valid",
			opts: checkOptions{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validateOutputFlags()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
