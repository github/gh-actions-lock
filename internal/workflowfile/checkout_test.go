package workflowfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateLocalActionsToSelfRepository_CheckoutContext(t *testing.T) {
	for _, tt := range []struct {
		name     string
		workflow string
		outer    string
		blocked  bool
	}{
		{
			name:     "custom ref reaches an already migrated composite",
			workflow: "      - uses: actions/checkout@v4\n        with: {ref: other}\n      - uses: $/outer\n",
			outer:    "    - uses: ./leaf\n",
			blocked:  true,
		},
		{
			name:     "custom ref reaches a nested composite",
			workflow: "      - uses: actions/checkout@v4\n        with: {ref: other}\n      - uses: $/outer\n",
			outer:    "    - uses: $/middle\n",
			blocked:  true,
		},
		{
			name:     "a safe call does not hide a later unsafe call",
			workflow: "      - uses: $/outer\n      - uses: actions/checkout@v4\n        with: {ref: other}\n      - uses: $/outer\n",
			outer:    "    - uses: ./leaf\n",
			blocked:  true,
		},
		{
			name:     "checkout in a composite reaches subsequent caller steps",
			workflow: "      - uses: $/outer\n      - uses: ./leaf\n",
			outer:    "    - uses: actions/checkout@v4\n      with: {ref: other}\n",
			blocked:  true,
		},
		{
			name:     "checkout in a local composite reaches subsequent caller steps",
			workflow: "      - uses: ./outer\n      - uses: ./leaf\n",
			outer:    "    - uses: actions/checkout@v4\n      with: {ref: other}\n",
			blocked:  true,
		},
		{
			name:     "checkout in a composite reaches a sibling composite",
			workflow: "      - uses: $/outer\n      - uses: $/middle\n",
			outer:    "    - uses: actions/checkout@v4\n      with: {ref: other}\n",
			blocked:  true,
		},
		{
			name:     "conditional composite checkout remains blocking",
			workflow: "      - uses: $/outer\n        if: github.event_name == 'push'\n      - uses: ./leaf\n",
			outer:    "    - uses: actions/checkout@v4\n      with: {ref: other}\n",
			blocked:  true,
		},
		{
			name:     "composite checkout does not affect another job",
			workflow: "      - uses: $/outer\n  second:\n    steps:\n      - uses: $/middle\n",
			outer:    "    - uses: actions/checkout@v4\n      with: {ref: other}\n",
		},
		{
			name:     "checkout after a nested local action allows migration",
			workflow: "      - uses: $/outer\n",
			outer:    "    - uses: ./leaf\n    - uses: actions/checkout@v4\n      with: {ref: other}\n",
		},
		{
			name:     "running commit checkout allows nested migration",
			workflow: "      - uses: actions/checkout@v4\n        with: {ref: '${{ github.sha }}'}\n      - uses: $/outer\n",
			outer:    "    - uses: $/middle\n",
		},
		{
			name:     "custom checkout with only self repository references needs no migration",
			workflow: "      - uses: actions/checkout@v4\n        with: {ref: other}\n      - uses: $/outer\n",
			outer:    "    - uses: $/leaf\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
			workflow := "jobs:\n  build:\n    steps:\n" + tt.workflow
			for path, content := range map[string]string{
				".github/workflows/ci.yml": workflow,
				"outer/action.yml":         "runs:\n  using: composite\n  steps:\n" + tt.outer,
				"middle/action.yaml":       "runs:\n  using: composite\n  steps:\n    - uses: ./leaf\n",
				"leaf/action.yml":          "runs:\n  using: node24\n  main: index.js\n",
			} {
				full := filepath.Join(dir, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
			}
			f, err := Load(filepath.Join(dir, ".github/workflows/ci.yml"))
			require.NoError(t, err)
			content, changed, err := f.MigrateLocalActionsToSelfRepository()
			if tt.blocked {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "preceding actions/checkout")
				assert.Contains(t, err.Error(), `ref "other"`)
				assert.Contains(t, err.Error(), ".yml:")
				assert.Zero(t, changed)
				assert.Equal(t, workflow, string(content))
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateLocalActionMigrations_ScopedRun(t *testing.T) {
	for _, tt := range []struct {
		name    string
		uses    string
		blocked bool
	}{
		{name: "unchanged workflow reference does not block a scoped run", uses: "./leaf"},
		{name: "shared composite reference blocks a scoped run", uses: "$/outer", blocked: true},
		{name: "symlinked composite reference blocks a scoped run", uses: "$/alias", blocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
			for path, content := range map[string]string{
				".github/workflows/outside.yml": "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n        with: {ref: other}\n      - uses: " + tt.uses + "\n",
				"outer/action.yml":              "runs:\n  using: composite\n  steps:\n    - uses: ./leaf\n",
				"leaf/action.yml":               "runs:\n  using: node24\n  main: index.js\n",
			} {
				full := filepath.Join(dir, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
			}
			if tt.uses == "$/alias" {
				if err := os.Symlink(filepath.Join(dir, "outer"), filepath.Join(dir, "alias")); err != nil {
					t.Skipf("creating symlink: %v", err)
				}
			}
			err := ValidateLocalActionMigrations(
				[]string{filepath.Join(dir, ".github/workflows/outside.yml")},
				[]string{filepath.Join(dir, "outer/action.yml")},
			)
			if tt.blocked {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "outer/action.yml")
				assert.Contains(t, err.Error(), "outside.yml:5")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestMigrateLocalActionsToSelfRepository_CompositeCycle(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	for path, content := range map[string]string{
		"ci.yml":           "jobs:\n  build:\n    steps:\n      - uses: $/outer\n",
		"outer/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: $/inner\n",
		"inner/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: $/outer\n",
	} {
		full := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	f, err := Load(filepath.Join(dir, "ci.yml"))
	require.NoError(t, err)
	_, changed, err := f.MigrateLocalActionsToSelfRepository()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "composite action cycle")
	assert.Zero(t, changed)
}
