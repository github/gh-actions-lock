package main

import (
	"testing"
)

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := NewRootCmd()
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Fatalf("expected root command to suppress Cobra usage/errors for handled failures")
	}
}
