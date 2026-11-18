package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyLocalCoverage_SelfActionRequiresExactLockKey(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name   string
		use    string
		key    string
		direct string
		valid  bool
	}{
		{name: "SHA use with only tag key", use: sha, key: "v4", valid: false},
		{name: "SHA use with SHA key", use: sha, key: sha, valid: true},
		{name: "tag use with tag key", use: "v4", key: "v4", valid: true},
		{name: "workflow SHA with tag key and different nested ref", use: "v4", key: "v4", direct: sha, valid: true},
		{name: "uppercase workflow SHA and lowercase nested SHA", use: sha, key: "v4", direct: strings.ToUpper(sha), valid: false},
		{name: "lowercase workflow SHA and uppercase nested SHA", use: strings.ToUpper(sha), key: "v4", direct: sha, valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
			actionPath := filepath.Join(dir, "setup", "action.yml")
			require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
			action := fmt.Sprintf("name: Setup\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@%s\n", tc.use)
			require.NoError(t, os.WriteFile(actionPath, []byte(action), 0o600))
			store := writeTestLockfile(t, dir, fmt.Sprintf(`version: 'v0.0.2'
dependencies:
  'actions/checkout@%s':
    ref: 'v4'
    commit: 'sha1-%s'
    owner_id: 1
    repo_id: 1
workflows:
  '.github/workflows/ci.yml':
    - 'actions/checkout@%s'
`, tc.key, sha, tc.key))
			path := filepath.Join(dir, ".github", "workflows", "ci.yml")
			workflow := "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n"
			if tc.direct != "" {
				workflow += "      - uses: actions/checkout@" + tc.direct + "\n"
			}
			workflow += "      - uses: $/setup\n"
			require.NoError(t, os.WriteFile(path, []byte(workflow), 0o600))
			t.Chdir(dir)
			parsed := ParseAll([]string{".github/workflows/ci.yml"}, store)
			require.NoError(t, parsed[0].LoadErr)
			require.Empty(t, parsed[0].SelfRepositoryResolutionErrs)
			require.Len(t, parsed[0].SelfActionRefs, 1)
			report := VerifyLocalCoverage(parsed, store)
			assert.Equal(t, tc.valid, report.IsValid())
			var missing int
			for _, finding := range report.Workflows[0].Findings {
				if finding.Category == checks.NotPinned {
					missing++
					assert.Equal(t, checks.SeverityError, finding.Severity)
					require.NotNil(t, finding.ActionRef)
					assert.Equal(t, strings.ToLower(tc.use), finding.ActionRef.Ref)
				}
			}
			if tc.valid {
				assert.Zero(t, missing)
			} else {
				assert.Equal(t, 1, missing)
			}
			got, err := os.ReadFile(actionPath)
			require.NoError(t, err)
			assert.Equal(t, action, string(got))
		})
	}
}
