package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
)

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := NewRootCmd(NewDefaultFactory())
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Fatalf("expected root command to suppress Cobra usage/errors for handled failures")
	}
}

func TestExitCodeFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil → 0", nil, 0},
		{"errSilent → 1", errSilent, 1},
		{"wrapped errSilent → 1", fmt.Errorf("findings: %w", errSilent), 1},
		{"generic error → 2", errors.New("boom"), 2},
		{"ErrFutureVersion → 2", lockfile.ErrFutureVersion, 2},
		{"wrapped ErrFutureVersion → 2", fmt.Errorf("parsing lockfile: %w", lockfile.ErrFutureVersion), 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
