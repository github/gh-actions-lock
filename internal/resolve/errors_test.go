package resolve

import (
	"strings"
	"testing"
)

func TestImpostorError(t *testing.T) {
	e := &ImpostorError{
		NWO: "evil/fork",
		Ref: "v1",
		SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	msg := e.Error()
	if !strings.Contains(msg, "evil/fork") {
		t.Fatalf("error should mention NWO, got %q", msg)
	}
	if !strings.Contains(msg, "not on any branch") {
		t.Fatalf("error should mention fork signal, got %q", msg)
	}
	// SHA should be shortened.
	if strings.Contains(msg, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatalf("error should use short SHA, got %q", msg)
	}
}
