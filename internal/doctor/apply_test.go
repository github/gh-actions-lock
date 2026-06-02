package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteWorkflowFile_PreservesMode confirms that rewriting a workflow file
// keeps its existing permission bits rather than widening to 0o644.
func TestWriteWorkflowFile_PreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	if err := os.WriteFile(path, []byte("name: ci\n"), 0o600); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	if err := writeWorkflowFile(path, []byte("name: ci updated\n")); err != nil {
		t.Fatalf("writeWorkflowFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected mode 0600 preserved, got %o", got)
	}
}

// TestWriteWorkflowFile_DefaultsWhenMissing confirms that writing a file that
// does not yet exist falls back to 0o644.
func TestWriteWorkflowFile_DefaultsWhenMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "new.yml")

	if err := writeWorkflowFile(path, []byte("name: ci\n")); err != nil {
		t.Fatalf("writeWorkflowFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("expected default mode 0644, got %o", got)
	}
}
