package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
)

// PinCheck is a locked dependency to re-check against the host, mirroring
// what the runner verifies at job start.
type PinCheck struct {
	Owner string
	Repo  string
	SHA   string
	// Tag, when set, must still peel to SHA.
	Tag string
}

// PinState is the host's view of a PinCheck. Err marks an inconclusive check.
type PinState struct {
	NameWithOwner string
	OwnerID       int64
	RepoID        int64
	// RepoMissing means the locked owner/repo did not resolve. A 404 can't
	// tell a deleted repository from one this token can't see.
	RepoMissing bool
	// ViaFallback marks a verdict from the REST-only or SSO fallback path,
	// where a missing token or SSO grant is the likelier cause.
	ViaFallback bool
	CommitFound bool
	// TagOID is the commit Tag peels to; empty when the tag is gone.
	TagOID string
	Err    error
}

// pinCheckBatchSize is larger than the action-file batch because each alias
// fetches no blobs.
const pinCheckBatchSize = 50

// CheckPins reports, per pin, the repository's canonical name and IDs,
// whether the pinned commit exists, and where Tag points. It does not walk
// action.yml: a pinned commit fixes the dependencies it declares.
func (c *Client) CheckPins(ctx context.Context, pins []PinCheck) []PinState {
	if len(pins) == 0 {
		return nil
	}
	if c.local != nil {
		states := make([]PinState, len(pins))
		groups := make(map[*Client][]PinCheck)
		indices := make(map[*Client][]int)
		for i, p := range pins {
			client, err := c.ForRepo(ctx, p.Owner, p.Repo)
			if err != nil {
				states[i].Err = err
				continue
			}
			groups[client] = append(groups[client], p)
			indices[client] = append(indices[client], i)
		}
		for client, batch := range groups {
			for j, s := range client.CheckPins(ctx, batch) {
				states[indices[client][j]] = s
			}
		}
		return states
	}
	if c.restOnly {
		return c.checkPinsREST(ctx, pins)
	}
	states := make([]PinState, 0, len(pins))
	for start := 0; start < len(pins); start += pinCheckBatchSize {
		end := min(start+pinCheckBatchSize, len(pins))
		states = append(states, c.checkPinBatch(ctx, pins[start:end])...)
	}
	return states
}

// checkPinBatch splits on batch-level failures like ResolveActionFiles.
func (c *Client) checkPinBatch(ctx context.Context, pins []PinCheck) []PinState {
	states, batchErr := c.checkPinsOnce(ctx, pins)
	code, _ := StatusCode(batchErr)
	if batchErr == nil || len(pins) == 1 || ctx.Err() != nil || code == http.StatusUnauthorized {
		for i, s := range states {
			if s.Err != nil && ctx.Err() == nil && c.repoFallbackEligible(ctx, pins[i].Owner, pins[i].Repo, s.Err) {
				states[i] = c.checkPinREST(ctx, pins[i])
			}
		}
		return states
	}
	mid := len(pins) / 2
	left := c.checkPinBatch(ctx, pins[:mid])
	if ctx.Err() != nil {
		right := make([]PinState, len(pins)-mid)
		for i := range right {
			right[i].Err = ctx.Err()
		}
		return append(left, right...)
	}
	return append(left, c.checkPinBatch(ctx, pins[mid:])...)
}

func (c *Client) checkPinsOnce(ctx context.Context, pins []PinCheck) ([]PinState, error) {
	query, vars := buildPinCheckQuery(pins)
	var data map[string]json.RawMessage
	err := c.graphql.DoWithContext(profile.WithGraphQLLabel(ctx, "parity"), query, vars, &data)
	var gqlErr *api.GraphQLError
	if err != nil && !errors.As(err, &gqlErr) {
		states := make([]PinState, len(pins))
		for i := range states {
			states[i].Err = err
		}
		return states, err
	}
	return parsePinCheckResponse(data, pins, gqlErr, c.Hostname), batchLevelGraphQLErr(gqlErr)
}

func buildPinCheckQuery(pins []PinCheck) (string, map[string]any) {
	vars := make(map[string]any, len(pins)*4)
	var decl, body strings.Builder
	decl.WriteString("query(")
	body.WriteString(") {")
	for i, p := range pins {
		if i > 0 {
			decl.WriteString(", ")
		}
		vars[fmt.Sprintf("owner%d", i)] = p.Owner
		vars[fmt.Sprintf("name%d", i)] = p.Repo
		vars[fmt.Sprintf("oid%d", i)] = p.SHA
		fmt.Fprintf(&decl, "$owner%d: String!, $name%d: String!, $oid%d: GitObjectID!", i, i, i)
		fmt.Fprintf(&body, " a%d: repository(owner: $owner%d, name: $name%d) {", i, i, i)
		body.WriteString(" nameWithOwner databaseId owner { ... on Organization { databaseId } ... on User { databaseId } }")
		fmt.Fprintf(&body, " commit: object(oid: $oid%d) { oid }", i)
		if p.Tag != "" {
			vars[fmt.Sprintf("tag%d", i)] = p.Tag + "^{commit}"
			fmt.Fprintf(&decl, ", $tag%d: String!", i)
			fmt.Fprintf(&body, " tag: object(expression: $tag%d) { oid }", i)
		}
		body.WriteString(" }")
	}
	body.WriteString(" }")
	return decl.String() + body.String(), vars
}

func parsePinCheckResponse(data map[string]json.RawMessage, pins []PinCheck, gqlErr *api.GraphQLError, hostname string) []PinState {
	reqs := make([]ActionFileRequest, len(pins))
	aliasMap := make(map[string]int, len(pins))
	for i, p := range pins {
		reqs[i] = ActionFileRequest{Owner: p.Owner, Repo: p.Repo}
		aliasMap[fmt.Sprintf("a%d", i)] = i
	}
	samlOwners := samlBlockedOwners(gqlErr, reqs, aliasMap)
	batchErr := batchLevelGraphQLErr(gqlErr)

	states := make([]PinState, len(pins))
	for alias, i := range aliasMap {
		p := pins[i]
		raw, ok := data[alias]
		switch {
		case samlOwners[p.Owner] && (!ok || string(raw) == "null"):
			states[i].Err = errors.New(ssoRequiredMessage(hostname, p.Owner))
			continue
		case !ok && batchErr != nil:
			states[i].Err = batchErr
			continue
		case !ok:
			states[i].Err = fmt.Errorf("repository %s/%s missing from response", p.Owner, p.Repo)
			continue
		case string(raw) == "null":
			states[i].Err = aliasError(gqlErr, alias)
			states[i].RepoMissing = states[i].Err == nil || repoNotFound(gqlErr, alias)
			if states[i].RepoMissing {
				states[i].Err = nil
			}
			continue
		}
		if err := aliasError(gqlErr, alias); err != nil {
			states[i].Err = err
			continue
		}
		var repo struct {
			NameWithOwner string `json:"nameWithOwner"`
			DatabaseID    int64  `json:"databaseId"`
			Owner         struct {
				DatabaseID int64 `json:"databaseId"`
			} `json:"owner"`
			Commit *struct {
				OID string `json:"oid"`
			} `json:"commit"`
			Tag *struct {
				OID string `json:"oid"`
			} `json:"tag"`
		}
		if err := json.Unmarshal(raw, &repo); err != nil {
			states[i].Err = fmt.Errorf("failed to parse: %w", err)
			continue
		}
		states[i] = PinState{
			NameWithOwner: repo.NameWithOwner,
			OwnerID:       repo.Owner.DatabaseID,
			RepoID:        repo.DatabaseID,
			CommitFound:   repo.Commit != nil && repo.Commit.OID != "",
		}
		if repo.Tag != nil {
			states[i].TagOID = repo.Tag.OID
		}
	}
	return states
}

// aliasError returns the first per-alias error, ignoring NOT_FOUND on the
// commit or tag lookups: those mean "missing", which the caller classifies.
func repoNotFound(gqlErr *api.GraphQLError, alias string) bool {
	for _, item := range gqlErr.Errors {
		if len(item.Path) == 1 && item.Path[0] == alias && item.Type == "NOT_FOUND" {
			return true
		}
	}
	return false
}

func aliasError(gqlErr *api.GraphQLError, alias string) error {
	if gqlErr == nil {
		return nil
	}
	for _, item := range gqlErr.Errors {
		if len(item.Path) == 0 || item.Path[0] != alias {
			continue
		}
		if item.Type == "NOT_FOUND" && len(item.Path) >= 2 && (item.Path[1] == "commit" || item.Path[1] == "tag") {
			continue
		}
		return errors.New(item.Message)
	}
	return nil
}

// checkPinsREST costs one repos/{owner}/{repo} call per unique repo plus one
// commits call per intact pin: the tag when one is set (a match proves the
// commit exists), otherwise the SHA.
func (c *Client) checkPinsREST(ctx context.Context, pins []PinCheck) []PinState {
	states := make([]PinState, len(pins))
	for i, p := range pins {
		states[i] = c.checkPinREST(ctx, p)
	}
	return states
}

func (c *Client) checkPinREST(ctx context.Context, p PinCheck) PinState {
	meta, err := c.repoMetadata(ctx, p.Owner, p.Repo)
	if code, _ := StatusCode(err); code == http.StatusNotFound {
		return PinState{RepoMissing: true, ViaFallback: true}
	}
	if err != nil {
		return PinState{Err: err}
	}
	s := PinState{NameWithOwner: meta.FullName, OwnerID: meta.OwnerID, RepoID: meta.RepoID}
	if p.Tag != "" {
		sha, found, err := c.restCommit(ctx, p.Owner, p.Repo, p.Tag)
		if err != nil {
			return PinState{Err: err}
		}
		if found {
			s.TagOID = sha
		}
		if strings.EqualFold(sha, p.SHA) {
			s.CommitFound = true
			return s
		}
	}
	// No tag, or the tag moved: look the commit up on its own so a moved
	// tag isn't misreported as a missing commit.
	_, s.CommitFound, err = c.restCommit(ctx, p.Owner, p.Repo, p.SHA)
	if err != nil {
		return PinState{Err: err}
	}
	return s
}

func (c *Client) restCommit(ctx context.Context, owner, repo, ref string) (string, bool, error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	path := fmt.Sprintf("repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	var err error
	if c.restOnly {
		err = c.anonGet(ctx, path, &commit)
	} else {
		err = c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &commit)
		if err != nil && IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			err = c.anonGet(ctx, path, &commit)
		}
	}
	if code, _ := StatusCode(err); code == http.StatusNotFound || code == http.StatusUnprocessableEntity {
		return "", false, nil
	}
	if err != nil {
		return "", false, c.ssoErr(owner, err)
	}
	return commit.SHA, true, nil
}
