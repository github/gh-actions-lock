package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const graphWorkflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: acme/parent@v1.0.0
`

const (
	graphParentSHA = "1111111111111111111111111111111111111111"
	graphChildSHA  = "2222222222222222222222222222222222222222"
	graphLeafSHA   = "3333333333333333333333333333333333333333"
)

type graphTransport struct {
	failChild        bool
	movedParent      bool
	movedChild       bool
	unreachableChild bool
}

func (g *graphTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if httpmock.GraphQL(`file: file\(path`)(req) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for i := 0; ; i++ {
			name, ok := body.Variables[fmt.Sprintf("name%d", i)]
			if !ok {
				break
			}
			ref := strings.TrimSuffix(body.Variables[fmt.Sprintf("expr%d", i)], "^{commit}")
			sha, yml := graphLeafSHA, nodeActionYAML
			switch strings.ToLower(name) {
			case "parent":
				sha = graphParentSHA
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/child@v2\n"
				if g.movedParent && ref == "v1.0.0" {
					sha, yml = graphLeafSHA, nodeActionYAML
				}
			case "child":
				if g.failChild {
					return httpmock.StatusResponse(http.StatusForbidden)(req)
				}
				sha = graphChildSHA
				if g.movedChild {
					sha = graphLeafSHA
				}
				yml = "runs:\n  using: composite\n  steps:\n    - uses: acme/leaf@v3\n"
			case "leaf":
			default:
				return nil, fmt.Errorf("unexpected fixture repository %s", name)
			}
			data[fmt.Sprintf("a%d", i)] = testRepoResponse("acme/"+name, sha, yml)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	if httpmock.GraphQL(`commit: object\(oid`)(req) {
		if g.unreachableChild {
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			data := map[string]any{}
			for i := 0; ; i++ {
				name, ok := body.Variables[fmt.Sprintf("name%d", i)]
				if !ok {
					break
				}
				sha := body.Variables[fmt.Sprintf("oid%d", i)]
				repo := testRepoResponse("acme/"+name, sha, nodeActionYAML)
				if name == "child" && sha == graphChildSHA {
					repo["commit"] = nil
				}
				data[fmt.Sprintf("a%d", i)] = repo
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		}
		return parityOK(req)
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/acme/") {
		switch req.URL.Path {
		case "/repos/acme/parent", "/repos/acme/child", "/repos/acme/leaf":
			return httpmock.JSONResponse(map[string]any{
				"id": 1, "owner": map[string]any{"id": 1}, "default_branch": "main",
			})(req)
		}
	}
	return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL)
}

func generateGraph(t *testing.T) (string, parserlock.File) {
	t.Helper()
	path := writeTempWorkflow(t, graphWorkflow)
	_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--no-narrow", path)
	require.NoError(t, err, stderr)
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	require.Len(t, file.Dependencies, 3)
	assert.Equal(t, []string{"acme/child@v2"}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.Equal(t, []string{"acme/leaf@v3"}, file.Dependencies["acme/child@v2"].Uses)
	return path, file
}

func writeGraph(t *testing.T, file parserlock.File) {
	t.Helper()
	b, err := yaml.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(parserlock.Path, b, 0o600))
}

func TestCompleteness_DanglingEntriesFailWithoutWrites(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "offline verification", args: []string{"--verify-local"}},
		{name: "online verification", args: []string{"--verify"}},
		{name: "no-fix", args: []string{"--no-fix"}},
		{name: "sticky write with onboarding disabled", args: []string{"--no-onboard"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			var transport http.RoundTripper = &graphTransport{}
			if tt.args[0] == "--verify-local" {
				counter := &requestCountingTransport{}
				transport = counter
				t.Cleanup(func() { assert.Zero(t, counter.calls.Load()) })
			}
			p, err := runParity(t, transport, append(tt.args, "--no-narrow", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "not-pinned", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.Contains(t, p.Findings[0].Detail, "acme/child@v2")
			assert.Equal(t, "acme/parent@v1.0.0", p.Findings[0].RequiredBy)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_RelockRebuildsWithoutRefMovement(t *testing.T) {
	for _, tt := range []struct {
		name     string
		missing  string
		dropEdge bool
		flag     string
	}{
		{name: "missing root", missing: "acme/parent@v1.0.0", flag: "--relock"},
		{name: "missing edge and child", missing: "acme/child@v2", dropEdge: true, flag: "--relock"},
		{name: "accept-moved repairs missing child", missing: "acme/child@v2", flag: "--accept-moved"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			delete(file.Dependencies, tt.missing)
			if tt.dropEdge {
				parent := file.Dependencies["acme/parent@v1.0.0"]
				parent.Uses = nil
				file.Dependencies["acme/parent@v1.0.0"] = parent
			}
			writeGraph(t, file)

			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, tt.flag, "--no-narrow", path)
			require.NoError(t, err, stderr)
			after := readTempLockfilePins(t)
			repaired, err := parserlock.Parse([]byte(after))
			require.NoError(t, err)
			assert.Len(t, repaired.Dependencies, 3)
			assert.Equal(t, "sha1-"+graphParentSHA, repaired.Dependencies["acme/parent@v1.0.0"].Commit)
			assert.Equal(t, "sha1-"+graphChildSHA, repaired.Dependencies["acme/child@v2"].Commit)
			assert.Equal(t, "sha1-"+graphLeafSHA, repaired.Dependencies["acme/leaf@v3"].Commit)
			assert.Equal(t, []string{"acme/child@v2"}, repaired.Dependencies["acme/parent@v1.0.0"].Uses)
			assert.Equal(t, []string{"acme/leaf@v3"}, repaired.Dependencies["acme/child@v2"].Uses)

			if tt.dropEdge {
				before := snapshotWorkflows(t)
				_, stderr, err = runCommandWithHTTP(t, &graphTransport{}, "--relock", "--no-narrow", path)
				require.NoError(t, err, stderr)
				assert.Equal(t, before, snapshotWorkflows(t), "a repaired graph must converge")
			}
		})
	}
}

func TestCompleteness_VerifyInspectsLockedManifests(t *testing.T) {
	for _, keepChild := range []bool{false, true} {
		t.Run(fmt.Sprintf("child recorded=%v", keepChild), func(t *testing.T) {
			path, file := generateGraph(t)
			parent := file.Dependencies["acme/parent@v1.0.0"]
			parent.Uses = nil
			file.Dependencies["acme/parent@v1.0.0"] = parent
			if !keepChild {
				delete(file.Dependencies, "acme/child@v2")
			}
			writeGraph(t, file)
			before := snapshotWorkflows(t)

			_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", path)
			require.NoError(t, err, "offline verification cannot infer an erased remote edge")
			transport := &graphTransport{movedParent: true}
			p, err := runParity(t, transport, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1)
			assert.Contains(t, p.Findings[0].Detail, "lockfile omits that edge")
			assert.Equal(t, "acme/parent@v1.0.0", p.Findings[0].RequiredBy)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_RelockReplacesObsoleteEdges(t *testing.T) {
	path, file := generateGraph(t)
	parent := file.Dependencies["acme/parent@v1.0.0"]
	parent.Uses = append(parent.Uses, "acme/obsolete@v1")
	file.Dependencies["acme/parent@v1.0.0"] = parent
	obsolete := file.Dependencies["acme/leaf@v3"]
	obsolete.Ref = "v1"
	file.Dependencies["acme/obsolete@v1"] = obsolete
	writeGraph(t, file)
	_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, "--relock", "--no-narrow", path)
	require.NoError(t, err, stderr)
	file, err = parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Len(t, file.Dependencies, 3)
	assert.Equal(t, []string{"acme/child@v2"}, file.Dependencies["acme/parent@v1.0.0"].Uses)
	assert.NotContains(t, file.Dependencies, "acme/obsolete@v1")
}

func TestCompleteness_VerifyManifestFailureIsBlocking(t *testing.T) {
	for _, tt := range []struct {
		name       string
		dropEdge   bool
		categories []string
	}{
		{name: "complete recorded graph", categories: []string{"reachability-unknown"}},
		{name: "omitted edge does not hide inspection failure", dropEdge: true, categories: []string{"not-pinned", "reachability-unknown"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file := generateGraph(t)
			if tt.dropEdge {
				parent := file.Dependencies["acme/parent@v1.0.0"]
				parent.Uses = nil
				file.Dependencies["acme/parent@v1.0.0"] = parent
				writeGraph(t, file)
			}
			before := snapshotWorkflows(t)
			p, err := runParity(t, &graphTransport{failChild: true}, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			var categories []string
			for _, f := range p.Findings {
				categories = append(categories, f.Category)
				assert.Equal(t, "error", f.Severity)
			}
			assert.ElementsMatch(t, tt.categories, categories)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_ScopedRunsIgnoreUnrelatedDamage(t *testing.T) {
	path, file := generateGraph(t)
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/missing@v1", 1)), 0o600))
	file.Workflows[other] = []string{"acme/missing@v1"}
	writeGraph(t, file)
	for _, mode := range []string{"--verify-local", "--verify", "--relock"} {
		_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, mode, "--no-narrow", path)
		require.NoError(t, err, stderr)
		result, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
		require.NoError(t, err)
		assert.Equal(t, []string{"acme/missing@v1"}, result.Workflows[other])
		assert.NotContains(t, result.Dependencies, "acme/missing@v1")
	}
}

func TestCompleteness_ScopedRelockCannotAdvanceSharedChild(t *testing.T) {
	path, file := generateGraph(t)
	const other = ".github/workflows/other.yml"
	require.NoError(t, os.WriteFile(other, []byte(graphWorkflow), 0o600))
	file.Workflows[other] = []string{"acme/parent@v1.0.0"}
	writeGraph(t, file)
	before := snapshotWorkflows(t)
	_, _, err := runCommandWithHTTP(t, &graphTransport{movedChild: true}, "--relock", "--no-narrow", path)
	require.ErrorContains(t, err, "cannot refresh shared pin")
	assert.Equal(t, before, snapshotWorkflows(t))
}

func TestCompleteness_RelockCannotAcceptUnreachableTransitivePin(t *testing.T) {
	path, _ := generateGraph(t)
	before := snapshotWorkflows(t)
	transport := &graphTransport{movedChild: true, unreachableChild: true}
	p, err := runParity(t, transport, "--relock", "--no-narrow", path)
	require.ErrorIs(t, err, errSilent)
	assert.False(t, p.Valid)
	found := false
	for _, f := range p.Findings {
		if f.Category == "unreachable-pin" {
			found = true
			assert.Equal(t, "acme/parent@v1.0.0", f.RequiredBy)
		}
	}
	assert.True(t, found, "%+v", p.Findings)
	assert.Equal(t, before, snapshotWorkflows(t))

	_, stderr, err := runCommandWithHTTP(t, transport, "--accept-moved", "--no-narrow", path)
	require.NoError(t, err, stderr)
	file, err := parserlock.Parse([]byte(readTempLockfilePins(t)))
	require.NoError(t, err)
	assert.Equal(t, "sha1-"+graphLeafSHA, file.Dependencies["acme/child@v2"].Commit)
}

func TestCompleteness_SHARootRetainsCoverageChecks(t *testing.T) {
	for _, mode := range []string{"--verify-local", "--verify"} {
		t.Run(mode, func(t *testing.T) {
			path, file := generateGraph(t)
			root := "acme/parent@" + graphParentSHA + " # v1.0.0"
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", root, 1)), 0o600))
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{}, mode, path)
			require.NoError(t, err, stderr)
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			_, stderr, err = runCommandWithHTTP(t, &graphTransport{}, mode, path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_BlockingDamageCannotWriteOtherWorkflows(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		failChild bool
	}{
		{name: "sticky write rejects recorded damage"},
		{name: "relock fails closed on discovery failure", args: []string{"--relock"}, failChild: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, file := generateGraph(t)
			const other = ".github/workflows/new.yml"
			otherContent := []byte(strings.Replace(graphWorkflow, "acme/parent@v1.0.0", "acme/leaf@v3", 1))
			require.NoError(t, os.WriteFile(other, otherContent, 0o600))
			delete(file.Dependencies, "acme/child@v2")
			writeGraph(t, file)
			before := snapshotWorkflows(t)
			args := append(tt.args, "--no-narrow")
			_, stderr, err := runCommandWithHTTP(t, &graphTransport{failChild: tt.failChild}, args...)
			require.Error(t, err, stderr)
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestCompleteness_CancelledManifestInspectionWritesNothing(t *testing.T) {
	path, _ := generateGraph(t)
	before := snapshotWorkflows(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	registerParity(reg)
	reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	})
	_, stderr, err := runCommandWithHTTPContext(t, ctx, reg, "--verify", path)
	require.ErrorIs(t, err, context.Canceled, stderr)
	assert.Equal(t, before, snapshotWorkflows(t))
}
