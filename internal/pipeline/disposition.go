package pipeline

import (
	"fmt"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// parityMode is the run's flags as they bear on parity findings.
type parityMode struct {
	verify bool
	// relock re-resolves every ref.
	relock bool
	// acceptMoved implies relock.
	acceptMoved bool
	// readOnly runs write nothing, so every recorded entry stays as is.
	readOnly bool
}

// paritySubject is the lock entry a parity finding is about.
type paritySubject struct {
	pin parserlock.Pin
	// fresh marks a pin resolved in this run: it is what the run would write.
	fresh bool
	// ownSHA marks a recorded entry that records its own SHA as its ref.
	// Its check confirms only repository and commit, which the run then
	// repairs on, so like a fresh pin it needs that check to finish.
	ownSHA bool
	// away marks a recorded entry planning provably replaces or removes
	// (see plannedAway in parity.go), so what the run writes for it is not
	// the recorded commit.
	away bool
	// heldBy is the composite that keeps the entry as recorded even when
	// --accept-moved re-resolves the workflow's use (see heldBy in
	// parity.go); "" when that repairs it.
	heldBy string
	// head is the ref the commit was traced to; "" for an entry without one.
	head string
	// inconclusive marks a check GitHub didn't answer (see unanswered).
	inconclusive bool
}

// parityVerdict is what disposeParity decides for a finding.
type parityVerdict int

const (
	// verdictKeep reports the finding as it stands.
	verdictKeep parityVerdict = iota
	// verdictExcuse drops the finding: the run should replace or remove its
	// recorded entry.
	verdictExcuse
	// verdictAccept reports the finding as an accepted warning: the run
	// should repair its recorded entry.
	verdictAccept
)

// disposeParity decides whether a parity finding reaches the report, and
// sets its severity and remediation for the run's mode. It is the one place
// that weighs a finding against the mode, so every mode agrees on what a
// pin's state means. It returns the verdict, and the finding as it would
// block were its entry left as recorded; for verdictKeep that is f itself.
//
// --relock excuses a recorded entry's findings only where re-resolving
// fixes them, and pin.Audit then holds the run to writing that fix. It never
// excuses repository identity, a full-SHA key's inconclusive check, or an
// unreachable pin: a commit gone from its ref's history stays an error
// unless --accept-moved accepts it. An exact tag locked behind its commit
// (tag-commit-mismatch) is excused like any finding re-resolving fixes.
//
// --accept-moved accepts an unreachable pin as a warning that itself makes
// planning re-resolve the entry: a mutable ref is locked at its current
// commit, and a full-SHA key gets a tag or branch that contains its commit.
// pin.Audit holds that to the write too. A full-SHA entry a composite holds
// (see heldBy) can't be repaired that way, so it stays an error, and advice
// names the holder.
func disposeParity(m parityMode, f *checks.Finding, s paritySubject) (parityVerdict, checks.Finding) {
	// A check GitHub didn't answer fails closed: the pin is unverified.
	if s.inconclusive {
		f.Severity = checks.SeverityError
	}
	if s.fresh || s.ownSHA && s.inconclusive {
		return verdictKeep, *f
	}
	fullSHA := parserlock.IsFullSha(s.pin.Ref)
	c := f.Category
	if c == checks.UnreachablePin && !f.RefMissing && f.Dependency != nil {
		f.Remediation = checks.UnreachableRemediation(s.heldBy, f.Dependency.NWO)
	}
	// pin.Audit holds an accepted or excused finding to the write the run
	// plans, so a mutable entry is deferred to it whoever holds it. A
	// composite's full-SHA entry stays blocking: its repair re-keys the
	// workflow's use and leaves the composite's entry as recorded.
	if m.acceptMoved && c == checks.UnreachablePin && !f.RefMissing && (!fullSHA || s.heldBy == "") {
		blocking := *f
		f.Severity = checks.SeverityWarning
		f.Remediation = "accepted: --accept-moved records a tag or branch that contains this commit"
		if !fullSHA {
			f.Remediation = fmt.Sprintf("accepted: --accept-moved locks %s at its current commit", s.pin.Ref)
		}
		return verdictAccept, blocking
	}
	excused := m.relock && (s.away || !fullSHA)
	// An unfinished history check can't be excused for a full-SHA key: the
	// commit is otherwise unproven.
	if s.inconclusive && s.head != "" && (!excused || fullSHA) {
		return verdictKeep, *f
	}
	if excused && !(c == checks.RepoRenamed || c == checks.RepoReplaced || c == checks.OwnerIDChanged || c == checks.RepoUnavailable ||
		c == checks.UnreachablePin) {
		return verdictExcuse, *f
	}
	return verdictKeep, *f
}
