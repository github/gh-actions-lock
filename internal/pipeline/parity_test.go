package pipeline

import (
	"errors"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		wantDetail  string
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
		{
			name: "replaced transitive dependency points at its parent", ref: "v2", parent: "octo/composite@v1",
			state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", RepoID: 99, CommitFound: true}, want: checks.RepoReplaced,
			wantDetail: "krzema12/github-actions-typing (used by octo/composite@v1) now resolves to a different repository than the one locked (repo ID 99, locked 2)",
			wantRemedy: "do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Stop using octo/composite@v1 or upgrade it to a version that doesn't use krzema12/github-actions-typing",
		},
		{name: "redirect to a different repo ID blocks", ref: "v2", state: ghapi.PinState{NameWithOwner: "mallory/github-actions-typing", RepoID: 99, CommitFound: true}, want: checks.RepoReplaced},
		{
			name: "missing repository leads with relock", ref: "v2", state: ghapi.PinState{RepoMissing: true}, want: checks.RepoUnavailable,
			wantDetail: "krzema12/github-actions-typing is missing or not visible to this token",
			wantRemedy: "if the repository moved or was deleted, update `uses:` and run `gh actions-lock`; if it's private or internal, use a token that can read it or authorize SSO for krzema12",
		},
		{
			name: "missing repository on fallback leads with access", ref: "v2", state: ghapi.PinState{RepoMissing: true, ViaFallback: true}, want: checks.RepoUnavailable,
			wantRemedy: "if it's private or internal, use a token that can read it or authorize SSO for krzema12; if the repository moved or was deleted, update `uses:` and run `gh actions-lock`",
		},
		{name: "missing ID is not a change", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}},
		{name: "commit gone", ref: "v2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing"}, want: checks.UnreachablePin},
		{
			name: "commit gone names both causes and the tag's target", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", TagOID: other}, want: checks.UnreachablePin,
			wantDetail: "locked commit aaaaaaaaaaaa is not in krzema12/github-actions-typing's history: it was force-pushed away, or the lockfile was edited; v2.2.2 currently points at bbbbbbbbbbbb",
			wantRemedy: "verify it, then run `gh actions-lock --accept-moved`",
		},
		{
			name: "exact tag moved points at accept-moved", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true, TagOID: other}, want: checks.UnreachablePin, wantObserve: other,
			wantRemedy: "release tags should not move; verify it, then run `gh actions-lock --accept-moved`",
		},
		{
			name: "deleted tag asks for a new ref", ref: "v2.2.2", state: ghapi.PinState{NameWithOwner: "krzema12/github-actions-typing", CommitFound: true}, want: checks.UnreachablePin,
			wantRemedy: "point `uses:` at a tag that exists, then run `gh actions-lock`",
		},
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
			assert.Equal(t, tt.wantWarn, f.Severity == checks.SeverityWarning)
			if tt.wantDetail != "" {
				assert.Equal(t, tt.wantDetail, f.Detail)
			}
			if tt.wantRemedy != "" {
				assert.Equal(t, tt.wantRemedy, f.Remediation)
			}
			assert.Equal(t, tt.wantObserve, f.ObservedSHA)
		})
	}
}

// TestParityTarget is #174: a full-SHA key names no ref, so the lock
// entry's recorded ref is what the commit must be reachable from, in every
// mode. Existence alone passes for any fork-network commit.
func TestParityTarget(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	tests := []struct {
		name     string
		key, ref string
		wantTag  string
		wantHead string
	}{
		{name: "exact tag key must still peel to the commit", key: "v6.1.0", ref: "v6.1.0", wantTag: "v6.1.0"},
		{name: "mutable key checks history", key: "v6", ref: "v6", wantHead: "v6"},
		{name: "branch key checks history", key: "main", ref: "main", wantHead: "main"},
		{name: "SHA key checks recorded tag", key: sha, ref: "v6.1.0", wantHead: "v6.1.0"},
		{name: "SHA key checks recorded branch", key: sha, ref: "releases/v4", wantHead: "releases/v4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: tt.key},
				Action: parserlock.Action{Ref: tt.ref, Commit: "sha1-" + sha},
			}
			tag, head := parityTarget(lp)
			assert.Equal(t, tt.wantTag, tag, "tag")
			assert.Equal(t, tt.wantHead, head, "head")
		})
	}
}

// TestFreshClosure_EntryRef: a fresh pin is checked against the ref the
// lockfile will record, so a full-SHA key is checked against its tag or
// branch, never the SHA.
func TestFreshClosure_EntryRef(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	tests := []struct {
		name string
		d    dep.Dependency
		want string
	}{
		{"tag key", dep.Dependency{NWO: "actions/checkout", Ref: "v6", SHA: sha}, "v6"},
		{"SHA key on a tag", dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha, Tag: "v6.1.0", Branch: "main"}, "v6.1.0"},
		{"SHA key on a branch", dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha, Branch: "main"}, "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw := checks.ParsedWorkflow{Path: ".github/workflows/ci.yml", Refs: []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: tt.d.Ref}}}
			got := freshClosure([]dep.Dependency{tt.d}, nil)(pw, nil)
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].lp.Action.Ref)
		})
	}
}

// TestParityFinding_FullSHAKey covers the verdicts for a full-SHA key
// checked against its recorded ref.
func TestParityFinding_FullSHAKey(t *testing.T) {
	const (
		sha  = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
		live = "d23441a49e5d9e4ba0dd8e0f0b5b86ff8ad2e2d5"
	)
	tests := []struct {
		name       string
		head       string
		state      ghapi.PinState
		want       checks.Category
		wantDetail string
	}{
		{name: "commit in recorded ref", head: "v6.1.0", state: ghapi.PinState{NameWithOwner: "actions/checkout", CommitFound: true, HeadOID: live}},
		{
			name: "commit outside recorded ref", head: "v6.1.0",
			state: ghapi.PinState{NameWithOwner: "actions/checkout", HeadOID: live}, want: checks.UnreachablePin,
			wantDetail: "locked commit 373d742f768e is not in actions/checkout's history: it was force-pushed away, or the lockfile was edited; v6.1.0 currently points at d23441a49e5d",
		},
		{
			name: "recorded ref missing", head: "does-not-exist",
			state: ghapi.PinState{NameWithOwner: "actions/checkout", CommitFound: true}, want: checks.UnreachablePin,
			wantDetail: "ref does-not-exist is not a tag or branch of actions/checkout; the runner will reject this pin",
		},
		{name: "inconclusive blocks", head: "v6.1.0", state: ghapi.PinState{Err: errors.New("timeout")}, want: checks.ReachabilityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lp := lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: "actions/checkout", Owner: "actions", Repo: "checkout", Ref: sha},
				Action: parserlock.Action{Ref: tt.head, Commit: "sha1-" + sha, OwnerID: 1, RepoID: 2},
			}
			ref := parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: sha}
			pc := ghapi.PinCheck{Owner: "actions", Repo: "checkout", SHA: sha, Head: tt.head}
			f, ok := parityFinding(checks.ParsedWorkflow{Path: ".github/workflows/ci.yml"}, ref, lp, pc, tt.state)
			if tt.want == "" {
				assert.False(t, ok, "unexpected finding: %+v", f)
				return
			}
			assert.True(t, ok)
			assert.Equal(t, tt.want, f.Category)
			assert.Equal(t, checks.SeverityError, f.Severity)
			if tt.wantDetail != "" {
				assert.Equal(t, tt.wantDetail, f.Detail)
			}
			assert.False(t, f.RefMissing, "the workflow ref is a SHA; only the recorded ref is gone")
			assert.NotEmpty(t, f.Remediation)
		})
	}
}
