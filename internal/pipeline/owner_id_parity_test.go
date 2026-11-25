package pipeline

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/require"
)

func TestParityFinding_OwnerIDChangeWithSameNameIsBlocking(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lp := lockfile.LockedPin{
		Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: "v4"},
		Action: parserlock.Action{Ref: "v4", Commit: "sha1-" + sha, OwnerID: 1, RepoID: 2},
	}
	ref := parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"}
	state := ghapi.PinState{
		NameWithOwner: "actions/checkout",
		OwnerID:       77,
		RepoID:        2,
		CommitFound:   true,
		HeadOID:       sha,
	}

	finding, ok := parityFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, ghapi.PinCheck{Owner: "actions", Repo: "checkout", SHA: sha, Head: "v4"}, state)
	require.True(t, ok, "an owner ID mismatch under the unchanged repository name must block verification")
	require.Equal(t, checks.Category("owner-id-changed"), finding.Category)
	require.Equal(t, checks.SeverityError, finding.Severity)
	require.Contains(t, finding.Detail, "owner ID changed (77, locked 1)")
}
