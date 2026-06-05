package doctor

import (
	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// sessionState tracks user decisions across multiple workflows during
// an interactive remediation session. It allows auto-applying a prior
// choice when the same dependency appears in another workflow file.
//
// The choices map is intentionally string-keyed because it stores entries
// under two different key shapes:
//   - cachekey.NWOSha.String() ("owner/repo|SHA") for tag selections (via
//     choiceKey), where the value is a chosen tag name; and
//   - Dependency.Key() ("owner/repo@Ref") for skip markers, where the
//     value is "skipped".
//
// Both string keys are produced by typed helpers so we still get
// normalized owner/repo casing. shaConvertedForNWO no longer prefix-scans
// the choices map (the two key shapes have different separators, so the
// scan was silently broken); it consults the convertedNWOs set instead.
type sessionState struct {
	choices map[string]string

	// convertedNWOs records owner/repo pairs whose SHA pin was rewritten
	// to a canonical tag earlier in this run (typically by handleSHAAsRef's
	// applySHAToTag path). Stale impostor/misleading/forgery alerts on the
	// same NWO are suppressed once the ref is rewritten — the alert no
	// longer describes the file on disk.
	convertedNWOs map[cachekey.Repo]bool

	// owner/repo → chosen ref (e.g. "main" or "v2") for same-owner repos.
	internalRefChoices map[cachekey.Repo]string

	// (owner/repo[/path], ref) → true for refs the user already approved
	// for pinning.
	approvedRefs map[cachekey.ActionRef]bool
}

func newSessionState() sessionState {
	return sessionState{
		choices:            make(map[string]string),
		convertedNWOs:      make(map[cachekey.Repo]bool),
		internalRefChoices: make(map[cachekey.Repo]string),
		approvedRefs:       make(map[cachekey.ActionRef]bool),
	}
}

// choiceKey returns a stable session-memory key for the dep's (NWO, SHA)
// pair. The owner/repo segment is normalized via cachekey.ForNWOSha so
// case-only NWO drift can't split the cache.
func choiceKey(dep *lockfile.Dependency) string {
	owner, repo := dep.OwnerRepo()
	return cachekey.ForNWOSha(owner, repo, dep.SHA).String()
}

// recordChoice saves a tag choice for a dep so it can be auto-applied later.
// Also marks the NWO as converted so shaConvertedForNWO can suppress stale
// alerts on the same NWO from later findings in this run.
func (s *sessionState) recordChoice(dep *lockfile.Dependency, tag string) {
	s.choices[choiceKey(dep)] = tag
	owner, repo := dep.OwnerRepo()
	if owner != "" {
		s.convertedNWOs[cachekey.ForRepo(owner, repo)] = true
	}
}

// recallChoice returns (tag, true) if we already made a choice for this dep.
func (s *sessionState) recallChoice(dep *lockfile.Dependency) (string, bool) {
	tag, ok := s.choices[choiceKey(dep)]
	return tag, ok
}

// refKey returns a session memory key for an unpinned action ref.
func refKey(ref lockfile.ActionRef) cachekey.ActionRef {
	return cachekey.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
}

// markRefsApproved records all action refs as approved for auto-pinning.
func (s *sessionState) markRefsApproved(refs []lockfile.ActionRef) {
	for _, ref := range refs {
		s.approvedRefs[refKey(ref)] = true
	}
}

// allRefsApproved returns true if every ref was already approved in a prior workflow.
func (s *sessionState) allRefsApproved(refs []lockfile.ActionRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if !s.approvedRefs[refKey(ref)] {
			return false
		}
	}
	return true
}
