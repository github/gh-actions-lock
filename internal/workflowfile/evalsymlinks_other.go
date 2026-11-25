//go:build !windows

package workflowfile

import "path/filepath"

func evalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
