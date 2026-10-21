package uses

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		use  string
		pos  Position
		want Kind
	}{
		{"actions/checkout@v4", Step, Action},
		{"actions/checkout/sub@v4", Step, Action},
		{"  actions/checkout@v4  ", Step, Action},
		{"./local", Step, Local},
		{"./.github/workflows/reusable.yml", Job, Local},
		{"docker://alpine:3", Step, Docker},
		{"${{ matrix.action }}", Step, Expression},
		{"$/actions/thing", Step, SelfRepository},
		{"", Step, Empty},
		{"octo/workflows/.github/workflows/release.yml@v1", Job, ReusableWorkflow},
		{"octo/workflows/.github/workflows/release.yml@v1", Step, Invalid},
		{"actions/checkout@main:action.yml", Step, Invalid},
		{"actions/checkout@main:action.yml", Job, Invalid},
		{"actions/checkout", Step, Invalid},
		{"actions/check out@v4", Step, Invalid},
		{"actions/checkout/../x@v4", Step, Invalid},
		{"octo/workflows/.github/workflows/nested/release.yml@v1", Job, Invalid},
		{"octo/action@rel@1", Step, Invalid},
		{"octo/workflows/.github/workflows/release.yml@rel@1", Job, Invalid},
	}
	for _, tt := range tests {
		r := Classify(tt.use, tt.pos)
		assert.Equal(t, tt.want, r.Kind, "%q at %v", tt.use, tt.pos)
		assert.Equal(t, tt.want == Invalid, r.Err() != nil, "%q", tt.use)
	}
}

func TestError(t *testing.T) {
	assert.EqualError(t, Classify("actions/checkout@main:action.yml", Step).Err(),
		"uses: actions/checkout@main:action.yml is not an action reference the Actions backend accepts")
	assert.EqualError(t, Classify("actions/checkout/.github/workflows/ci.yml@v4", Step).Err(),
		"uses: actions/checkout/.github/workflows/ci.yml@v4 is a reusable workflow; reusable workflows should be referenced at the top-level `jobs.<job_id>.uses` key, not within steps")
	assert.EqualError(t, Classify("octo/action@rel@1", Step).Err(),
		"uses: octo/action@rel@1 has more than one @; the Actions backend expects {owner}/{repo}[/path]@ref")
}
