package workflowfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsHostedRunnerLabel(t *testing.T) {
	hosted := []string{
		"ubuntu-latest", "ubuntu-22.04", "ubuntu-24.04", "ubuntu-26.04",
		"ubuntu-22.04-arm", "ubuntu-24.04-arm", "ubuntu-26.04-arm",
		"ubuntu-slim",
		"ubuntu-24.04-firewall", "ubuntu-latest-firewall",
		"windows-latest", "windows-2022", "windows-2025", "windows-2025-vs2026",
		"windows-11-arm", "windows-11-vs2026-arm",
		"macos-latest", "macos-14", "macos-15", "macos-26",
		"macos-15-intel", "macos-26-intel",
		"macos-14-large", "macos-15-large", "macos-26-large", "macos-latest-large",
		"macos-14-xlarge", "macos-15-xlarge", "macos-26-xlarge", "macos-latest-xlarge",
		"codespaces-prebuild",
		"Ubuntu-Latest", // case-insensitive
	}
	for _, l := range hosted {
		assert.True(t, IsHostedRunnerLabel(l), "expected hosted: %s", l)
	}

	notHosted := []string{
		"self-hosted", "linux", "my-custom-runner",
		"gpu", "ARM64",
		"ubuntu-20.04",  // EOL, not in map
		"macos-13",      // removed
		"macos-12",      // removed
		"macos-11",      // removed
		"windows-2019",  // removed
		"ubuntu-18.04",  // removed
	}
	for _, l := range notHosted {
		assert.False(t, IsHostedRunnerLabel(l), "expected non-hosted: %s", l)
	}
}

func TestExtractRunsOnLabels_Scalar(t *testing.T) {
	wf, err := Parse("ci.yml", []byte(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`))
	require.NoError(t, err)
	labels := wf.ExtractRunsOnLabels()
	assert.Equal(t, []string{"ubuntu-latest"}, labels)
}

func TestExtractRunsOnLabels_Sequence(t *testing.T) {
	wf, err := Parse("ci.yml", []byte(`
name: ci
on: push
jobs:
  test:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo hi
`))
	require.NoError(t, err)
	labels := wf.ExtractRunsOnLabels()
	assert.Equal(t, []string{"self-hosted", "linux"}, labels)
}

func TestExtractRunsOnLabels_Group(t *testing.T) {
	wf, err := Parse("ci.yml", []byte(`
name: ci
on: push
jobs:
  test:
    runs-on:
      group: my-org-runners
      labels: [linux, x64]
    steps:
      - run: echo hi
`))
	require.NoError(t, err)
	labels := wf.ExtractRunsOnLabels()
	assert.Contains(t, labels, "my-org-runners")
	assert.Contains(t, labels, "linux")
	assert.Contains(t, labels, "x64")
}

func TestExtractRunsOnLabels_MultipleJobs(t *testing.T) {
	wf, err := Parse("ci.yml", []byte(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
  deploy:
    runs-on: macos-latest
    steps:
      - run: echo deploy
`))
	require.NoError(t, err)
	labels := wf.ExtractRunsOnLabels()
	assert.Len(t, labels, 2)
	assert.Contains(t, labels, "ubuntu-latest")
	assert.Contains(t, labels, "macos-latest")
}

func TestExtractRunsOnLabels_Deduplicates(t *testing.T) {
	wf, err := Parse("ci.yml", []byte(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo lint
`))
	require.NoError(t, err)
	labels := wf.ExtractRunsOnLabels()
	assert.Equal(t, []string{"ubuntu-latest"}, labels)
}

func TestHasNonHostedRunnerLabels(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantNon bool
	}{
		{
			name: "all hosted",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  build:
    runs-on: macos-14
    steps:
      - run: echo hi
`,
			wantNon: false,
		},
		{
			name: "self-hosted label",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo hi
`,
			wantNon: true,
		},
		{
			name: "custom label",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on: my-custom-runner
    steps:
      - run: echo hi
`,
			wantNon: true,
		},
		{
			name: "runner group",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on:
      group: org-runners
    steps:
      - run: echo hi
`,
			wantNon: true,
		},
		{
			name: "expression label",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on: ${{ matrix.os }}
    steps:
      - run: echo hi
`,
			wantNon: true,
		},
		{
			name: "mixed hosted and self-hosted jobs",
			yaml: `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
  deploy:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo deploy
`,
			wantNon: true,
		},
		{
			name: "no jobs",
			yaml: `
name: ci
on: push
`,
			wantNon: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf, err := Parse("ci.yml", []byte(tt.yaml))
			require.NoError(t, err)
			assert.Equal(t, tt.wantNon, wf.HasNonHostedRunnerLabels())
		})
	}
}
