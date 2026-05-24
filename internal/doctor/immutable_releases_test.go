package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestFindActionFiles(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "action.yml"), "name: root\n")
	mustWriteFile(t, filepath.Join(tmp, "save", "action.yml"), "name: save\n")
	mustWriteFile(t, filepath.Join(tmp, "restore", "action.yaml"), "name: restore\n")
	// Should be ignored.
	mustWriteFile(t, filepath.Join(tmp, "node_modules", "x", "action.yml"), "name: vendored\n")
	mustWriteFile(t, filepath.Join(tmp, ".git", "hooks", "action.yml"), "name: git\n")
	mustWriteFile(t, filepath.Join(tmp, "vendor", "y", "action.yaml"), "name: vendored2\n")
	mustWriteFile(t, filepath.Join(tmp, "dist", "action.yml"), "name: built\n")
	mustWriteFile(t, filepath.Join(tmp, "README.md"), "")

	got, err := findActionFiles(tmp)
	if err != nil {
		t.Fatalf("findActionFiles: %v", err)
	}
	sort.Strings(got)

	want := []string{"action.yml", "restore/action.yaml", "save/action.yml"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
