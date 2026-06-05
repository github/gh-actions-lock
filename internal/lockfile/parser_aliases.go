package lockfile

import (
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
)

// This file owns the boundary between internal/lockfile orchestration and the
// parser-level schema package.
//
// Type aliases avoid conversion churn while keeping call sites on the internal
// package path.

// ActionRef is the parsed form of a workflow `uses:` string.
type ActionRef = parserlock.ActionRef

// Pin is a fully-resolved lockfile pin (NWO@Ref:Algo-Hex).
type Pin = parserlock.Pin

// File is the on-disk lockfile shape.
type File = parserlock.File

// Action is a single per-action entry inside File.Actions.
type Action = parserlock.Action

// ActionMeta is the resolver-relevant subset of action.yml.
type ActionMeta = parserlock.ActionMeta

// ExecutionType labels how an action runs (node, docker, composite, …).
type ExecutionType = parserlock.ExecutionType

// ExecComposite is the ExecutionType for composite actions, i.e. the only
// execution kind whose nested `uses:` need recursive resolution.
const ExecComposite = parserlock.ExecComposite

// SchemaVersion is the schema version string written into freshly created
// lockfiles. Named distinctly from the in-memory Version struct in this
// package.
const SchemaVersion = parserlock.Version

// ErrFutureVersion is the sentinel returned (via errors.Is) when the parser
// refuses to read a lockfile whose schema version is newer than this binary
// supports. Re-exported so callers outside the lockfile boundary can match
// it without importing pkg/lockfile directly.
var ErrFutureVersion = parserlock.ErrFutureVersion

// IsFullSha reports whether s is a full 40-hex-char SHA-1 or 64-hex-char
// SHA-256.
func IsFullSha(s string) bool { return parserlock.IsFullSha(s) }

// ParseActionRef parses a `uses:` string into an ActionRef. Returns nil on
// malformed input.
func ParseActionRef(uses string) *ActionRef { return parserlock.ParseActionRef(uses) }

// ParseActionMeta parses an action.yml byte stream into ActionMeta.
func ParseActionMeta(content string) (*ActionMeta, error) {
	return parserlock.ParseActionMeta(content)
}

// ParsePin parses a canonical lockfile pin string (NWO@Ref:Algo-Hex).
func ParsePin(s string) (Pin, bool) { return parserlock.ParsePin(s) }

// IndexKey returns the lockfile index key for an (owner, repo, ref) tuple.
func IndexKey(owner, repo, ref string) string {
	return parserlock.IndexKey(owner, repo, ref)
}

// SplitNWO splits "owner/repo[/path]" into (owner, repo, ok). Multi-slash
// inputs collapse to (owner, repo) at repo granularity. See pkg/lockfile.SplitNWO.
func SplitNWO(nwo string) (owner, repo string, ok bool) {
	return parserlock.SplitNWO(nwo)
}
