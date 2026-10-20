package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordedPartialRefsRemainValidInReadOnlyModes(t *testing.T) {
	for _, ref := range []string{"v4", "v4.3"} {
		for _, args := range [][]string{{"--verify"}, {"--no-fix"}, {"--verify-local"}} {
			t.Run(ref+"/"+args[0], func(t *testing.T) {
				wf := writeTempWorkflow(t, strings.Replace(cleanWorkflow, "@v5.0.0", "@"+ref, 1),
					"actions/setup-go@"+ref+"=sha1-"+parityTestMoved)
				before := snapshotWorkflows(t)
				reg := &httpmock.Registry{}
				registerParity(reg)
				registerSetupGo(reg)
				stdout, stderr, err := runCommandWithHTTP(t, reg,
					append(append([]string{}, args...), "--json=valid,findings", wf)...)
				require.NoError(t, err, stdout, stderr)
				var payload struct {
					Valid bool `json:"valid"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
				assert.True(t, payload.Valid, stdout)
				assert.Equal(t, before, snapshotWorkflows(t), "read-only modes must preserve source and lock bytes")
			})
		}
	}
}
