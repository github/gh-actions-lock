package pipeline

import (
	"errors"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func TestParityFinding(t *testing.T) {
	const (
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	pin := func(ref string) ghapi.PinCheck {
		pc := ghapi.PinCheck{Owner: "krzema12", Repo: "github-actions-typing", SHA: sha}
		if checks.IsImmutableRef(ref) {
			pc.Tag = ref
		}
		return pc
	}
	intact := ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 1, RepoID: 2, CommitFound: true, TagOID: sha}

	tests := []struct {
		name        string
		ref         string
		parent      string
		state       ghapi.PinState
		want        checks.Category // empty means no finding
		wantWarn    bool
		wantRemedy  string
		wantObserve string
	}{
		{name: "intact exact tag", ref: "v2.2.2", state: intact},
		{name: "intact mutable ref", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}},
		{name: "case-only name difference is not a move", ref: "v2", state: ghapi.PinState{NameWithOwner: "Krzema12/GitHub-Actions-Typing", CommitFound: true}},
		{
			name: "transferred repository names the new location", ref: "v2.2.2",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", CommitFound: true, TagOID: sha},
			want:  checks.RepoRenamed, wantWarn: true,
			wantRemedy: "run `gh actions-lock` to rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`",
		},
		{
			name: "transferred transitive dependency points at its parent", ref: "v2", parent: "octo/composite@v1",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", CommitFound: true},
			want:  checks.RepoRenamed, wantWarn: true,
			wantRemedy: "upgrade octo/composite@v1 to a version that uses typesafegithub/github-actions-typing",
		},
		{
			name: "transfer keeps the repo ID and is a warning", ref: "v2.2.2",
			state: ghapi.PinState{NameWithOwner: "typesafegithub/github-actions-typing", OwnerID: 77, RepoID: 2, CommitFound: true, TagOID: sha},
			want:  checks.RepoRenamed, wantWarn: true,
		},
		{name: "owner ID change alone is not identity", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 77, RepoID: 2, CommitFound: true}},
		{name: "repo ID changed", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", OwnerID: 1, RepoID: 99, CommitFound: true}, want: checks.RepoReplaced},
		{name: "redirect to a different repo ID blocks", ref: "v2", state: ghapi.PinState{NameWithOwner: "mallory/github-actions-typing", RepoID: 99, CommitFound: true}, want: checks.RepoReplaced},
		{name: "repository gone", ref: "v2", state: ghapi.PinState{RepoMissing: true}, want: checks.RepoReplaced},
		{name: "missing ID is not a change", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}},
		{name: "commit gone", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing"}, want: checks.UnreachablePin},
		{name: "exact tag moved", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true, TagOID: other}, want: checks.UnreachablePin, wantObserve: other},
		{name: "exact tag deleted", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}, want: checks.UnreachablePin},
		{name: "network error is inconclusive", ref: "v2", state: ghapi.PinState{Err: errors.New("timeout")}, want: checks.ReachabilityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "krzema12/github-actions-typing", Owner: "krzema12", Repo: "github-actions-typing", Ref: tt.ref},
				Action: parserlock.Action{Ref: tt.ref, Commit: "sha1-" + sha, OwnerID: 1, RepoID: 2},
				Parent: tt.parent,
			}
			ref := parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: tt.ref}
			f, ok := parityFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, pin(tt.ref), tt.state)
			if tt.want == "" {
				assert.False(t, ok, "unexpected finding: %+v", f)
				return
			}
			assert.True(t, ok)
			assert.Equal(t, tt.want, f.Category)
			assert.Equal(t, tt.wantWarn || tt.want == checks.ReachabilityUnknown, f.Severity == checks.SeverityWarning)
			if tt.wantRemedy != "" {
				assert.Equal(t, tt.wantRemedy, f.Remediation)
			}
			assert.Equal(t, tt.wantObserve, f.ObservedSHA)
		})
	}
}
