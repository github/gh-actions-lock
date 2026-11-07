package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
)

// PinCheck is a locked dependency to re-check against the host, mirroring
// what the runner verifies at job start.
type PinCheck struct {
	Owner string
	Repo  string
	SHA   string
	// Head, when set, is a ref whose current commit is reported as HeadOID
	// so the caller can check SHA is still in its history. It is looked up
	// only as an existing refs/tags/<name>, then refs/heads/<name>, of
	// exactly that name, as the Actions backend resolver does: rev-parse
	// rules would also accept a pull request head (pull/1/head).
	Head string
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
	// HeadOID is the commit Head peels to; empty when the ref is gone.
	HeadOID string
	// HeadIsTag marks a Head that resolved as a tag rather than a branch.
	HeadIsTag bool
	Err       error
}

// GraphQL latency grows with the aliases in one query, so pins are checked in
// small batches, a few at a time. Behind the Dependabot proxy, where retries
// are patient and nobody waits on the run, they go in one large batch at a
// time as before.
const (
	pinCheckBatchSize      = 6
	pinCheckConcurrency    = 4
	proxyPinCheckBatchSize = 50
)

func pinCheckBatching() (size, workers int) {
	if DependabotProxy() {
		return proxyPinCheckBatchSize, 1
	}
	return pinCheckBatchSize, pinCheckConcurrency
}

// CheckPins reports, per pin, the repository's canonical name and IDs,
// whether the pinned commit exists, and where Head points. It does not walk
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
				if IsNotFound(err) {
					states[i] = PinState{RepoMissing: true}
				}
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
	size, workers := pinCheckBatching()
	states := make([]PinState, len(pins))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for start := 0; start < len(pins); start += size {
		end := min(start+size, len(pins))
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			copy(states[start:end], c.checkPinBatch(ctx, pins[start:end]))
		})
	}
	wg.Wait()
	return states
}

// checkPinBatch splits on batch-level failures like ResolveActionFiles.
func (c *Client) checkPinBatch(ctx context.Context, pins []PinCheck) []PinState {
	states, batchErr := c.checkPinsOnce(ctx, pins)
	if !splittable(batchErr) || len(pins) == 1 || ctx.Err() != nil {
		for i, s := range states {
			if s.Err == nil || ctx.Err() != nil {
				continue
			}
			if c.repoFallbackEligible(ctx, pins[i].Owner, pins[i].Repo, s.Err) {
				states[i] = c.checkPinREST(ctx, pins[i])
			} else {
				states[i].Err = c.ssoErr(pins[i].Owner, s.Err)
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
		if refs := p.refs(); refs != nil {
			vars[fmt.Sprintf("tag%d", i)] = refs[0] + "^{commit}"
			vars[fmt.Sprintf("head%d", i)] = refs[1] + "^{commit}"
			vars[fmt.Sprintf("tagRef%d", i)] = refs[0]
			vars[fmt.Sprintf("headRef%d", i)] = refs[1]
			fmt.Fprintf(&decl, ", $tag%d: String!, $head%d: String!, $tagRef%d: String!, $headRef%d: String!", i, i, i, i)
			fmt.Fprintf(&body, " tag: object(expression: $tag%d) { oid } head: object(expression: $head%d) { oid }", i, i)
			fmt.Fprintf(&body, " tagRef: ref(qualifiedName: $tagRef%d) { name } headRef: ref(qualifiedName: $headRef%d) { name }", i, i)
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
		if policyErr := ipAllowListAliasError(gqlErr, alias); policyErr != nil {
			states[i].Err = policyErr
			continue
		}
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
			Head *struct {
				OID string `json:"oid"`
			} `json:"head"`
			TagRef  *struct{} `json:"tagRef"`
			HeadRef *struct{} `json:"headRef"`
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
		// An expression peels the ref, but it would also evaluate a name that
		// is no ref at all; only a ref that exists by that exact name counts.
		if repo.TagRef == nil {
			repo.Tag = nil
		}
		if repo.HeadRef == nil {
			repo.Head = nil
		}
		// Tag precedence keys off the tag existing, not peeling: a tag that
		// targets a tree or blob still hides a same-named branch.
		var oid string
		switch {
		case repo.TagRef != nil:
			states[i].HeadIsTag = true
			if repo.Tag != nil {
				oid = repo.Tag.OID
			}
		case repo.Head != nil:
			oid = repo.Head.OID
		}
		states[i].HeadOID = oid
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
		if item.Type == "NOT_FOUND" && len(item.Path) >= 2 && (item.Path[1] == "commit" || item.Path[1] == "tag" || item.Path[1] == "head" || item.Path[1] == "tagRef" || item.Path[1] == "headRef") {
			continue
		}
		return errors.New(item.Message)
	}
	return nil
}

// checkPinsREST costs one repos/{owner}/{repo} call per unique repo plus one
// commits call per intact pin: the Head when one is set (a match proves the
// commit exists), otherwise the SHA. A Head that moved costs one more
// commits call, and a branch rather than a tag one more again.
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
	if p.Head != "" {
		t, err := c.LookupRefName(ctx, p.Owner, p.Repo, p.Head)
		if err != nil {
			return PinState{Err: err}
		}
		s.HeadOID, s.HeadIsTag = t.Commit, t.Kind == TagRef
		if strings.EqualFold(s.HeadOID, p.SHA) {
			s.CommitFound = true
			return s
		}
	}
	// No ref, or the ref moved: look the commit up on its own so a moved
	// ref isn't misreported as a missing commit.
	_, s.CommitFound, err = c.restCommit(ctx, p.Owner, p.Repo, p.SHA)
	if err != nil {
		return PinState{Err: err}
	}
	return s
}
