package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// ActionFileRequest identifies a GitHub Action ref to resolve via GraphQL.
type ActionFileRequest struct {
	Owner string
	Repo  string
	Path  string // sub-action path within the repo, may be empty
	Ref   string // tag, branch, or SHA
}

// NWO returns "Owner/Repo".
func (r ActionFileRequest) NWO() string { return r.Owner + "/" + r.Repo }

// ActionFileResult holds the resolved commit OID and action.yml content
// for one ActionFileRequest. Err is non-nil when this specific ref could
// not be resolved (e.g. not found, SSO required).
type ActionFileResult struct {
	Owner     string
	Repo      string
	Path      string
	Ref       string
	CommitOID string
	ActionYML string
	Err       error
}

// repoResponse is the raw GraphQL response shape for a single repository alias.
type repoResponse struct {
	NameWithOwner string `json:"nameWithOwner"`
	Object        *struct {
		OID  string `json:"oid"`
		File *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"file"`
		FileYAML *struct {
			Object *struct {
				Text string `json:"text"`
			} `json:"object"`
		} `json:"fileYaml"`
	} `json:"object"`
}

// ResolveActionFiles resolves action refs to commit OIDs and fetches
// their action.yml/yaml content in a single batched GraphQL round-trip.
// Results are returned in the same order as inputs; per-ref failures are
// recorded in each result's Err field rather than aborting the batch.
func (c *Client) ResolveActionFiles(ctx context.Context, refs []ActionFileRequest) []ActionFileResult {
	if len(refs) == 0 {
		return nil
	}
	query, vars, aliasMap := buildActionFileQuery(refs)

	var data map[string]json.RawMessage
	err := c.graphql.DoWithContext(ctx, query, vars, &data)
	var gqlErr *api.GraphQLError
	if err != nil {
		if !errors.As(err, &gqlErr) {
			// Total transport failure — every result gets the error.
			results := make([]ActionFileResult, len(refs))
			for i, r := range refs {
				results[i] = ActionFileResult{
					Owner: r.Owner, Repo: r.Repo, Path: r.Path, Ref: r.Ref,
					Err: err,
				}
			}
			return results
		}
	}

	return parseActionFileResponse(data, refs, aliasMap, gqlErr, c.Hostname)
}

func buildActionFileQuery(refs []ActionFileRequest) (string, map[string]any, map[string]int) {
	aliasMap := make(map[string]int, len(refs))
	vars := make(map[string]any, len(refs)*5)

	var decl strings.Builder
	var body strings.Builder
	decl.WriteString("query(")
	body.WriteString(") {")

	for i, ref := range refs {
		alias := fmt.Sprintf("a%d", i)
		aliasMap[alias] = i

		ownerVar := fmt.Sprintf("owner%d", i)
		nameVar := fmt.Sprintf("name%d", i)
		exprVar := fmt.Sprintf("expr%d", i)
		ymlVar := fmt.Sprintf("yml%d", i)
		yamlVar := fmt.Sprintf("yaml%d", i)

		ymlPath := "action.yml"
		yamlPath := "action.yaml"
		if ref.Path != "" {
			ymlPath = ref.Path + "/action.yml"
			yamlPath = ref.Path + "/action.yaml"
		}

		vars[ownerVar] = ref.Owner
		vars[nameVar] = ref.Repo
		// Peel through annotated tags with `^{commit}` so the
		// `... on Commit` fragment matches for annotated tag refs.
		vars[exprVar] = ref.Ref + "^{commit}"
		vars[ymlVar] = ymlPath
		vars[yamlVar] = yamlPath

		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$%s: String!, $%s: String!, $%s: String!, $%s: String!, $%s: String!",
			ownerVar, nameVar, exprVar, ymlVar, yamlVar)

		fmt.Fprintf(&body, " %s: repository(owner: $%s, name: $%s) {", alias, ownerVar, nameVar)
		body.WriteString(" nameWithOwner")
		fmt.Fprintf(&body, " object(expression: $%s) {", exprVar)
		body.WriteString(" ... on Commit { oid")
		fmt.Fprintf(&body, " file: file(path: $%s) { object { ... on Blob { text } } }", ymlVar)
		fmt.Fprintf(&body, " fileYaml: file(path: $%s) { object { ... on Blob { text } } }", yamlVar)
		body.WriteString(" }")
		body.WriteString(" }")
		body.WriteString(" }")
	}

	body.WriteString(" }")
	return decl.String() + body.String(), vars, aliasMap
}

func parseActionFileResponse(data map[string]json.RawMessage, refs []ActionFileRequest, aliasMap map[string]int, gqlErr *api.GraphQLError, hostname string) []ActionFileResult {
	results := make([]ActionFileResult, len(refs))
	for i, r := range refs {
		results[i] = ActionFileResult{Owner: r.Owner, Repo: r.Repo, Path: r.Path, Ref: r.Ref}
	}

	samlOwners := samlBlockedOwners(gqlErr, refs, aliasMap)

	for alias, idx := range aliasMap {
		ref := refs[idx]
		raw, ok := data[alias]
		if !ok {
			if samlOwners[ref.Owner] {
				results[idx].Err = fmt.Errorf("%s", ssoRequiredMessage(hostname, ref.Owner))
			} else {
				results[idx].Err = fmt.Errorf("not found in response")
			}
			continue
		}
		if string(raw) == "null" {
			if samlOwners[ref.Owner] {
				results[idx].Err = fmt.Errorf("%s", ssoRequiredMessage(hostname, ref.Owner))
			} else {
				results[idx].Err = fmt.Errorf("repository not found or not accessible")
			}
			continue
		}

		var repo repoResponse
		if err := json.Unmarshal(raw, &repo); err != nil {
			results[idx].Err = fmt.Errorf("failed to parse: %w", err)
			continue
		}

		if repo.Object == nil || repo.Object.OID == "" {
			results[idx].Err = fmt.Errorf("ref %q does not exist", ref.Ref)
			continue
		}

		results[idx].CommitOID = repo.Object.OID

		if repo.Object.File != nil && repo.Object.File.Object != nil {
			results[idx].ActionYML = repo.Object.File.Object.Text
		} else if repo.Object.FileYAML != nil && repo.Object.FileYAML.Object != nil {
			results[idx].ActionYML = repo.Object.FileYAML.Object.Text
		}
	}

	return results
}

// samlBlockedOwners returns the set of repository owners whose resolution
// failed an organization SAML SSO enforcement check.
func samlBlockedOwners(gqlErr *api.GraphQLError, refs []ActionFileRequest, aliasMap map[string]int) map[string]bool {
	if gqlErr == nil {
		return nil
	}
	owners := make(map[string]bool)
	for _, e := range gqlErr.Errors {
		if blocked, _ := e.Extensions["saml_failure"].(bool); !blocked {
			continue
		}
		if len(e.Path) == 0 {
			continue
		}
		alias, ok := e.Path[0].(string)
		if !ok {
			continue
		}
		idx, ok := aliasMap[alias]
		if !ok || idx < 0 || idx >= len(refs) {
			continue
		}
		owners[refs[idx].Owner] = true
	}
	return owners
}

// ssoRequiredMessage builds an actionable error directing the user to
// authorize their token for an SSO-protected organization.
func ssoRequiredMessage(hostname, owner string) string {
	host := hostname
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("SSO authorization required: your token is not authorized for the %q organization (SAML enforcement). Authorize it at https://%s/orgs/%s/sso and retry", owner, host, owner)
}
