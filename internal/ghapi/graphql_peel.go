package ghapi

import (
	"context"

	"github.com/github/gh-actions-lock/internal/profile"
)

// tagObjectPeelQuery resolves both the type of the object at $oid and the
// commit it peels to in a single round trip. The `^{commit}` suffix follows
// tag-of-tag chains server-side so depth costs nothing.
const tagObjectPeelQuery = `query($owner: String!, $name: String!, $oid: GitObjectID!, $expr: String!) {
  repository(owner: $owner, name: $name) {
    head: object(oid: $oid) { __typename }
    peeled: object(expression: $expr) { ... on Commit { oid } }
  }
}`

// PeelTagObjectResult holds the outcome of a tag-object peel query.
type PeelTagObjectResult struct {
	// Typename is the __typename of the object at the given OID
	// ("Tag", "Commit", "Blob", "Tree"), or empty if the OID/repo
	// was not found.
	Typename string

	// CommitOID is the commit SHA the tag peels to (via ^{commit}).
	// Empty when the peel fails or the object is not a tag.
	CommitOID string
}

// PeelTagObject queries whether sha is an annotated tag object in owner/repo
// and, if so, returns the commit it dereferences to. Returns a zero result
// (not an error) when the OID or repo is not accessible — callers decide
// how to interpret the negative.
func (c *Client) PeelTagObject(ctx context.Context, owner, repo, sha string) (PeelTagObjectResult, error) {
	var resp struct {
		Repository *struct {
			Head *struct {
				Typename string `json:"__typename"`
			} `json:"head"`
			Peeled *struct {
				OID string `json:"oid"`
			} `json:"peeled"`
		} `json:"repository"`
	}
	vars := map[string]any{
		"owner": owner,
		"name":  repo,
		"oid":   sha,
		"expr":  sha + "^{commit}",
	}
	if err := c.graphql.DoWithContext(profile.WithGraphQLLabel(ctx, "peel"), tagObjectPeelQuery, vars, &resp); err != nil {
		// SAML-blocked: return zero result (callers treat as "unknown type").
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			return PeelTagObjectResult{}, nil
		}
		return PeelTagObjectResult{}, err
	}
	if resp.Repository == nil || resp.Repository.Head == nil {
		return PeelTagObjectResult{}, nil
	}
	result := PeelTagObjectResult{Typename: resp.Repository.Head.Typename}
	if resp.Repository.Peeled != nil {
		result.CommitOID = resp.Repository.Peeled.OID
	}
	return result, nil
}
