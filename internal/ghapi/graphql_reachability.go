package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
)

// batchReachabilitySize is the maximum number of branches per GraphQL query.
const batchReachabilitySize = 50

// BatchBranchContains checks whether sha is reachable from any of the given
// branches using the GraphQL Ref.compare API. It batches all branches into
// one or a few GraphQL queries (batchReachabilitySize per query) and returns
// the first matching branch name in the order provided.
//
// Returns:
//   - matchedBranch: name of the first branch containing sha, or "" if none
//   - anyChecked: true if at least one branch was successfully checked
//   - err: non-nil only on transport/auth failures (not per-branch misses)
func (c *Client) BatchBranchContains(ctx context.Context, owner, repo, sha string, branches []BranchHead) (matchedBranch string, anyChecked bool, err error) {
	if len(branches) == 0 {
		return "", false, nil
	}

	for start := 0; start < len(branches); start += batchReachabilitySize {
		end := start + batchReachabilitySize
		if end > len(branches) {
			end = len(branches)
		}
		chunk := branches[start:end]

		matched, checked, qerr := c.batchBranchContainsChunk(ctx, owner, repo, sha, chunk)
		if checked {
			anyChecked = true
		}
		if matched != "" {
			return matched, true, nil
		}
		if qerr != nil {
			err = qerr
		}
	}
	return "", anyChecked, err
}

func (c *Client) batchBranchContainsChunk(ctx context.Context, owner, repo, sha string, branches []BranchHead) (matched string, anyChecked bool, err error) {
	query, vars := buildBranchCompareQuery(owner, repo, sha, branches)

	var data map[string]json.RawMessage
	gqlErr := c.graphql.DoWithContext(profile.WithGraphQLLabel(ctx, "reachability"), query, vars, &data)
	if gqlErr != nil {
		var httpErr *api.HTTPError
		if errors.As(gqlErr, &httpErr) {
			// SAML-blocked GraphQL → fall back to REST compare for eligible orgs.
			if IsSAMLEnforcement(gqlErr) && c.SSOFallbackEligible(ctx, owner) {
				return c.anonBatchBranchContains(ctx, owner, repo, sha, branches)
			}
			return "", false, gqlErr
		}
		if data == nil {
			return "", false, gqlErr
		}
	}

	repoRaw, ok := data["repo"]
	if !ok {
		return "", false, gqlErr
	}

	var repoData map[string]json.RawMessage
	if err := json.Unmarshal(repoRaw, &repoData); err != nil {
		return "", false, fmt.Errorf("parsing batch reachability response: %w", err)
	}

	for i, b := range branches {
		alias := fmt.Sprintf("b%d", i)
		raw, ok := repoData[alias]
		if !ok || string(raw) == "null" {
			continue
		}

		var refData struct {
			Compare *struct {
				Status string `json:"status"`
			} `json:"compare"`
		}
		if err := json.Unmarshal(raw, &refData); err != nil || refData.Compare == nil {
			continue
		}

		anyChecked = true
		status := strings.ToUpper(refData.Compare.Status)
		if status == "BEHIND" || status == "IDENTICAL" {
			return b.Name, true, nil
		}
	}

	return "", anyChecked, nil
}

// anonBatchBranchContains falls back to per-branch REST compare calls when
// the GraphQL reachability query is SAML-blocked.
func (c *Client) anonBatchBranchContains(ctx context.Context, owner, repo, sha string, branches []BranchHead) (string, bool, error) {
	var anyChecked bool
	for _, b := range branches {
		if ctx.Err() != nil {
			return "", anyChecked, ctx.Err()
		}
		contains, err := c.anonCompareCommits(ctx, owner, repo, sha, b.SHA)
		if err != nil {
			continue
		}
		anyChecked = true
		if contains {
			return b.Name, true, nil
		}
	}
	return "", anyChecked, nil
}

func buildBranchCompareQuery(owner, repo, sha string, branches []BranchHead) (string, map[string]any) {
	vars := make(map[string]any, len(branches)+3)
	vars["owner"] = owner
	vars["name"] = repo
	vars["sha"] = sha

	var decl strings.Builder
	var body strings.Builder

	decl.WriteString("query($owner: String!, $name: String!, $sha: String!")
	body.WriteString(") { repo: repository(owner: $owner, name: $name) {")

	for i, b := range branches {
		refVar := fmt.Sprintf("ref%d", i)
		vars[refVar] = "refs/heads/" + b.Name

		fmt.Fprintf(&decl, ", $%s: String!", refVar)
		fmt.Fprintf(&body, " b%d: ref(qualifiedName: $%s) { compare(headRef: $sha) { status } }", i, refVar)
	}

	body.WriteString(" } }")
	return decl.String() + body.String(), vars
}
