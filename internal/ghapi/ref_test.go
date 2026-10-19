package ghapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveActionRefSelection(t *testing.T) {
	for _, tt := range []struct {
		name, tag, branch, want string
		collision               bool
	}{
		{name: "tag only", tag: paritySHA, want: paritySHA},
		{name: "branch only", branch: parityMoved, want: parityMoved},
		{name: "same target", tag: paritySHA, branch: paritySHA, want: paritySHA},
		{name: "different targets", tag: paritySHA, branch: parityMoved, collision: true},
		{name: "missing"},
	} {
		for _, proxy := range []bool{false, true} {
			transport := "graphql"
			if proxy {
				transport = "rest"
			}
			t.Run(tt.name+"/"+transport, func(t *testing.T) {
				var got string
				var err error
				if proxy {
					reg := &httpmock.Registry{}
					defer reg.Verify(t)
					for namespace, sha := range map[string]string{"tags": tt.tag, "heads": tt.branch} {
						response := httpmock.StatusResponse(http.StatusNotFound)
						if sha != "" {
							response = httpmock.JSONResponse(map[string]any{"sha": sha})
						}
						reg.Register(httpmock.REST("GET", "^/repos/o/r/commits/refs/"+namespace+"/v1$"), response)
					}
					c := newTestClient(t, reg)
					c.anonHTTP = &http.Client{Transport: reg}
					got, err = c.anonResolveRef(context.Background(), "o", "r", "v1")
				} else {
					repo := map[string]any{"nameWithOwner": "o/r"}
					for field, sha := range map[string]string{"object": tt.tag, "branch": tt.branch} {
						if sha != "" {
							repo[field] = map[string]any{"oid": sha}
						}
					}
					raw, marshalErr := json.Marshal(repo)
					require.NoError(t, marshalErr)
					results := parseActionFileResponse(map[string]json.RawMessage{"a0": raw},
						[]ActionFileRequest{{Owner: "o", Repo: "r", Ref: "v1"}},
						map[string]int{"a0": 0}, nil, "github.com")
					got, err = results[0].CommitOID, results[0].Err
				}
				switch {
				case tt.collision:
					var ambiguous *AmbiguousRefError
					require.ErrorAs(t, err, &ambiguous)
					assert.Equal(t, "v1", ambiguous.Ref)
				case tt.want == "":
					var missing *RefNotFoundError
					require.ErrorAs(t, err, &missing)
				default:
					require.NoError(t, err)
					assert.Equal(t, tt.want, got)
				}
			})
		}
	}
}

func TestActionQueryPeelsBothNamespacesButNotFullSHAs(t *testing.T) {
	query, vars, _ := buildActionFileQuery([]ActionFileRequest{
		{Owner: "o", Repo: "r", Ref: "v1"},
		{Owner: "o", Repo: "r", Ref: paritySHA},
	})
	assert.Equal(t, "refs/tags/v1^{commit}", vars["expr0"], "annotated and lightweight tags must resolve to commits")
	assert.Equal(t, "refs/heads/v1^{commit}", vars["branch0"])
	assert.Equal(t, paritySHA+"^{commit}", vars["expr1"])
	assert.NotContains(t, vars, "branch1")
	assert.Contains(t, query, "branch: object(expression: $branch0)")
}

func TestMissingTagDoesNotHideBranchOrDenial(t *testing.T) {
	raw := json.RawMessage(`{"nameWithOwner":"o/r","branch":{"oid":"` + paritySHA + `"}}`)
	for _, kind := range []string{"NOT_FOUND", "FORBIDDEN"} {
		t.Run(kind, func(t *testing.T) {
			results := parseActionFileResponse(map[string]json.RawMessage{"a0": raw},
				[]ActionFileRequest{{Owner: "o", Repo: "r", Ref: "main"}},
				map[string]int{"a0": 0},
				&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: kind, Path: []any{"a0", "object"}, Message: "lookup denied"}}},
				"github.com")
			if kind == "NOT_FOUND" {
				require.NoError(t, results[0].Err)
				assert.Equal(t, paritySHA, results[0].CommitOID)
			} else {
				require.ErrorContains(t, results[0].Err, "lookup denied")
			}
		})
	}
}
