package doctor

import "github.com/github/gh-actions-pin/internal/lockfile"

// sessionState tracks user decisions across multiple workflows during
// an interactive remediation session. It allows auto-applying a prior
// choice when the same dependency appears in another workflow file.
type sessionState struct {
	// "owner/repo@SHA" → chosen tag name.
	choices map[string]string

	// "owner/repo" → chosen ref (e.g. "main" or "v2") for same-owner repos.
	internalRefChoices map[string]string

	// "owner/repo@ref" → true for refs the user already approved for pinning.
	approvedRefs map[string]bool
}

func newSessionState() sessionState {
	return sessionState{
		choices:            make(map[string]string),
		internalRefChoices: make(map[string]string),
		approvedRefs:       make(map[string]bool),
	}
}

// choiceKey returns a stable key for session memory: "owner/repo@SHA".
func choiceKey(dep *lockfile.Dependency) string {
	return dep.NWO + "@" + dep.SHA
}

// recordChoice saves a tag choice for a dep so it can be auto-applied later.
func (s *sessionState) recordChoice(dep *lockfile.Dependency, tag string) {
	s.choices[choiceKey(dep)] = tag
}

// recallChoice returns (tag, true) if we already made a choice for this dep.
func (s *sessionState) recallChoice(dep *lockfile.Dependency) (string, bool) {
	tag, ok := s.choices[choiceKey(dep)]
	return tag, ok
}

// refKey returns a session memory key for an unpinned action ref: "owner/repo@ref".
func refKey(ref lockfile.ActionRef) string {
	return ref.FullName() + "@" + ref.Ref
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
