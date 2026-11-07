package ghapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
)

// ActionFileRequest identifies a GitHub Action ref to resolve via GraphQL.
type ActionFileRequest struct {
	Owner string
	Repo  string
	Path  string // sub-action path within the repo, may be empty
	Ref   string // tag, branch, or SHA
	// Dockerfile also looks for the Dockerfile the runner falls back to
	// when there is no action.yml or action.yaml.
	Dockerfile bool
}

// NWO returns "Owner/Repo".
func (r ActionFileRequest) NWO() string { return r.Owner + "/" + r.Repo }

// ActionFileResult holds the resolved commit OID and action.yml content
// for one ActionFileRequest. Err is non-nil when this specific ref could
// not be resolved (e.g. not found, SSO required).
//
// Owner and Repo are the repository's canonical name. OriginalNWO is set
// when the request followed a rename or transfer redirect.
type ActionFileResult struct {
	Hostname    string
	Owner       string
	Repo        string
	OriginalNWO string
	Path        string
	Ref         string
	CommitOID   string
	ActionYML   string
	// Dockerfile reports a Dockerfile at Path, when the request asked.
	Dockerfile bool
	Err        error
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
		Dockerfile      *struct{} `json:"dockerfile"`
		DockerfileLower *struct{} `json:"dockerfileLower"`
	} `json:"object"`
}

// ResolveActionFiles resolves action refs to commit OIDs and fetches
// their action.yml/yaml content in a single batched GraphQL round-trip.
// Results are returned in the same order as inputs; per-ref failures are
// recorded in each result's Err field rather than aborting the batch.
//
// If GraphQL rejects the whole query — query cost/complexity, an unattributed
// SAML block, or an organization IP allow list 403 — the batch is split in
// half and each half retried, recursively down to a single ref, so a few
// oversized or blocked refs can't fail their batch-mates. An outage or rate
// limit is never split: it rejects every half the same way, and the transport
// already retried it. Splitting is skipped once ctx is canceled so a canceled
// scan doesn't fan out into a flurry of doomed retries.
func (c *Client) ResolveActionFiles(ctx context.Context, refs []ActionFileRequest) []ActionFileResult {
	if len(refs) == 0 {
		return nil
	}
	if c.local != nil {
		results := make([]ActionFileResult, len(refs))
		groups := make(map[*Client][]ActionFileRequest)
		indices := make(map[*Client][]int)
		for i, ref := range refs {
			client, err := c.ForRepo(ctx, ref.Owner, ref.Repo)
			if err != nil {
				results[i] = ActionFileResult{Owner: ref.Owner, Repo: ref.Repo, Path: ref.Path, Ref: ref.Ref, Err: err}
				continue
			}
			groups[client] = append(groups[client], ref)
			indices[client] = append(indices[client], i)
		}
		for client, batch := range groups {
			for j, result := range client.ResolveActionFiles(ctx, batch) {
				results[indices[client][j]] = result
			}
		}
		return results
	}
	if c.restOnly {
		results := make([]ActionFileResult, len(refs))
		for i, ref := range refs {
			results[i] = c.resolveAnonymous(ctx, ref)
		}
		return results
	}
	results, batchErr := c.resolveActionFilesOnce(ctx, refs)
	if !splittable(batchErr) || len(refs) == 1 || ctx.Err() != nil {
		return c.retryWithAnonymous(ctx, refs, results)
	}
	mid := len(refs) / 2
	left := c.ResolveActionFiles(ctx, refs[:mid])
	// A cancellation during the left half must not fan out the right half into
	// doomed retries. Synthesize the right results so length and order are
	// preserved without another round-trip.
	if ctx.Err() != nil {
		right := make([]ActionFileResult, len(refs)-mid)
		for i, r := range refs[mid:] {
			right[i] = ActionFileResult{
				Owner: r.Owner, Repo: r.Repo, Path: r.Path, Ref: r.Ref,
				Err: ctx.Err(),
			}
		}
		return append(left, right...)
	}
	right := c.ResolveActionFiles(ctx, refs[mid:])
	return append(left, right...)
}

// retryWithAnonymous retries refs that GraphQL could not authenticate using
// REST GETs when the repository is accessible through REST.
func (c *Client) retryWithAnonymous(ctx context.Context, refs []ActionFileRequest, results []ActionFileResult) []ActionFileResult {
	for i, r := range results {
		if r.Err == nil || ctx.Err() != nil {
			continue
		}
		if !c.repoFallbackEligible(ctx, refs[i].Owner, refs[i].Repo, r.Err) {
			results[i].Err = c.ssoErr(refs[i].Owner, r.Err)
			continue
		}
		results[i] = c.resolveAnonymous(ctx, refs[i])
	}
	return results
}

// resolveActionFilesOnce performs one batched round-trip. The returned error is
// non-nil only for batch-level failures — the whole query was rejected — that
// splitting may recover from; ordinary per-ref failures are recorded in each
// result's Err field and do not trigger a split.
func (c *Client) resolveActionFilesOnce(ctx context.Context, refs []ActionFileRequest) ([]ActionFileResult, error) {
	query, vars, aliasMap := buildActionFileQuery(refs)

	var data map[string]json.RawMessage
	err := c.graphql.DoWithContext(profile.WithGraphQLLabel(ctx, cmp.Or(profile.GraphQLLabel(ctx), "resolve")), query, vars, &data)
	var gqlErr *api.GraphQLError
	if err != nil {
		if !errors.As(err, &gqlErr) {
			// Transport or HTTP failure: every result gets the error. Only an
			// organization IP allow list 403 is splittable (see splittable);
			// an ordinary HTTP failure would reject every half the same way.
			results := make([]ActionFileResult, len(refs))
			for i, r := range refs {
				results[i] = ActionFileResult{
					Owner: r.Owner, Repo: r.Repo, Path: r.Path, Ref: r.Ref,
					Err: err,
				}
			}
			return results, err
		}
	}

	results := parseActionFileResponse(data, refs, aliasMap, gqlErr, c.Hostname)
	return results, batchLevelGraphQLErr(gqlErr)
}

// splittable reports whether splitting a batch rejected with err could
// recover some of its refs. A query-level GraphQL error or an organization IP
// allow list 403 can; an outage, an ordinary HTTP failure, or a rate limit
// rejects every half the same way.
func splittable(err error) bool {
	var gqlErr *api.GraphQLError
	if errors.As(err, &gqlErr) {
		return !graphQLRateLimited(err)
	}
	return isIPAllowListDenial(err)
}

// batchLevelGraphQLErr returns a non-nil error when gqlErr carries a
// query-level failure: any error item with no alias path (e.g. cost limit,
// query too complex, secondary rate limit, or a SAML block that GitHub did not
// pin to a specific alias). Such a failure rejects the whole query, so
// splitting the batch can recover the refs that would have succeeded on their
// own — and, for a no-path SAML block, drives the batch down to single refs so
// the SSO message can be attributed to the one remaining owner. Per-alias
// errors (which carry a path) return nil — they belong to one ref and splitting
// wouldn't help.
func batchLevelGraphQLErr(gqlErr *api.GraphQLError) error {
	if gqlErr == nil {
		return nil
	}
	for _, e := range gqlErr.Errors {
		if len(e.Path) == 0 {
			return gqlErr
		}
	}
	return nil
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
		if ref.Dockerfile {
			dir := strings.TrimSuffix(ymlPath, "action.yml")
			vars[fmt.Sprintf("docker%d", i)] = dir + "Dockerfile"
			vars[fmt.Sprintf("dockerLower%d", i)] = dir + "dockerfile"
			fmt.Fprintf(&decl, ", $docker%d: String!, $dockerLower%d: String!", i, i)
			fmt.Fprintf(&body, " dockerfile: file(path: $docker%d) { oid } dockerfileLower: file(path: $dockerLower%d) { oid }", i, i)
		}
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
		results[i] = ActionFileResult{Hostname: hostname, Owner: r.Owner, Repo: r.Repo, Path: r.Path, Ref: r.Ref}
	}

	samlOwners := samlBlockedOwners(gqlErr, refs, aliasMap)
	batchErr := batchLevelGraphQLErr(gqlErr)

	for alias, idx := range aliasMap {
		ref := refs[idx]
		if policyErr := ipAllowListAliasError(gqlErr, alias); policyErr != nil {
			results[idx].Err = policyErr
			continue
		}
		raw, ok := data[alias]
		if !ok {
			switch {
			case samlOwners[ref.Owner]:
				results[idx].Err = fmt.Errorf("%s", ssoRequiredMessage(hostname, ref.Owner))
			case batchErr != nil:
				// The whole query was rejected (cost/complexity/rate limit), so
				// the alias is missing for a batch-level reason, not because the
				// ref doesn't exist. Surface the real error; the caller splits
				// and retries rather than mislabeling 20 good refs "not found".
				results[idx].Err = batchErr
			default:
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
		if gqlErr != nil {
			for _, item := range gqlErr.Errors {
				if len(item.Path) > 0 && item.Path[0] == alias {
					// Both metadata spellings are queried; either may be absent.
					// Reusable workflows can have neither.
					if item.Type == "NOT_FOUND" && len(item.Path) == 3 && item.Path[1] == "object" &&
						(item.Path[2] == "file" || item.Path[2] == "fileYaml" || item.Path[2] == "dockerfile" || item.Path[2] == "dockerfileLower") {
						continue
					}
					results[idx].Err = fmt.Errorf("%s", item.Message)
					break
				}
			}
			if results[idx].Err != nil {
				continue
			}
		}

		var repo repoResponse
		if err := json.Unmarshal(raw, &repo); err != nil {
			results[idx].Err = fmt.Errorf("failed to parse: %w", err)
			continue
		}
		if err := canonicalize(&results[idx], repo.NameWithOwner); err != nil {
			results[idx].Err = err
			continue
		}

		if repo.Object == nil || repo.Object.OID == "" {
			n := len(ref.Ref)
			var err error
			switch {
			case isHexString(ref.Ref) && (n == 40 || n == 64):
				// Full SHA that doesn't resolve — commit is unreachable/orphaned
				err = fmt.Errorf("commit %s does not exist or is not reachable in %s/%s",
					ref.Ref[:12], ref.Owner, ref.Repo)
			case isHexString(ref.Ref):
				// Short hex — ambiguous, might be a truncated SHA
				err = fmt.Errorf("version %q does not resolve — if this is a commit, use the full 40-character SHA", ref.Ref)
			default:
				err = fmt.Errorf("version %q does not exist", ref.Ref)
			}
			results[idx].Err = &RefNotFoundError{Owner: ref.Owner, Repo: ref.Repo, Ref: ref.Ref, Err: err}
			continue
		}

		results[idx].CommitOID = repo.Object.OID

		if repo.Object.File != nil && repo.Object.File.Object != nil {
			results[idx].ActionYML = repo.Object.File.Object.Text
		} else if repo.Object.FileYAML != nil && repo.Object.FileYAML.Object != nil {
			results[idx].ActionYML = repo.Object.FileYAML.Object.Text
		}
		results[idx].Dockerfile = repo.Object.Dockerfile != nil || repo.Object.DockerfileLower != nil
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
			// A SAML block with no alias path can't be tied to one ref. Mark
			// every owner in the batch. ResolveActionFiles treats a no-path
			// error as batch-level and splits down to single refs, so by the
			// time a result survives the split the batch holds one owner and
			// this attribution is exact.
			for _, r := range refs {
				owners[r.Owner] = true
			}
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

func isHexString(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// canonicalize moves a result to the repository's current name.
func canonicalize(r *ActionFileResult, canonical string) error {
	if canonical == "" {
		return nil
	}
	owner, name, ok := strings.Cut(canonical, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("invalid canonical repository name %q", canonical)
	}
	if strings.EqualFold(canonical, r.Owner+"/"+r.Repo) {
		return nil
	}
	r.OriginalNWO = r.Owner + "/" + r.Repo
	r.Owner, r.Repo = owner, name
	return nil
}
