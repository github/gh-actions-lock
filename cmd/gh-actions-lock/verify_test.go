package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyVerifyFlags(t *testing.T) {
	tests := []struct {
		name       string
		opts       checkOptions
		wantRescan bool
		wantNoFix  bool
	}{
		{
			name:       "verify sets rescan and noFix",
			opts:       checkOptions{verify: true},
			wantRescan: true,
			wantNoFix:  true,
		},
		{
			name:       "no verify leaves flags alone",
			opts:       checkOptions{},
			wantRescan: false,
			wantNoFix:  false,
		},
		{
			name:       "verify with existing rescan keeps both",
			opts:       checkOptions{verify: true, rescan: true},
			wantRescan: true,
			wantNoFix:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyVerifyFlags(&tt.opts)
			assert.Equal(t, tt.wantRescan, tt.opts.rescan)
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
			name: "verify and verify-local are mutually exclusive",
			opts: checkOptions{verify: true, verifyLocal: true},
			wantErr: "mutually exclusive",
		},
		{
			name: "verify-local and rescan conflict",
			opts: checkOptions{verifyLocal: true, rescan: true},
			wantErr: "offline",
		},
		{
			name: "verify-local and accept-moved conflict",
			opts: checkOptions{verifyLocal: true, acceptMoved: true},
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
