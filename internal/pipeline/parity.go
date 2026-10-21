package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// checkParity re-checks every locked pin in each workflow's recorded closure
// the way the runner does at job start: same repository identity, commit
// still present, and the lock entry's ref (a tag, else a branch) still
// vouching for it: the commit must be in the ref's history, since a
// fork-network commit resolves under the upstream name, so existence alone
// proves nothing (#174). A floating ref (v4, main) that has moved ahead is
// fine: the lock is sticky until --relock. An exact release tag names one
// commit, so an entry keyed by one that locks an older commit in the tag's
// history blocks in every mode (tag-commit-mismatch); with verify, a
// full-SHA key whose recorded tag moved ahead only warns, since its commit
// is visible in the workflow. A ref that is gone, or a history check that
// can't finish, blocks.
//
// A pinned commit fixes the dependencies it declares, so the flattened
// closure in the lockfile is checked in one batch with no recursion.
// Pins resolved in this run (refs absent from recordedKeys, and their
// transitive closure from this run's resolve) join the same batch. Any
// blocking finding keeps its workflow out of the write.
//
// Identity follows the runner: only the repo ID counts. A rename or
// transfer redirect to the same repo ID is a warning, also inside a
// remote composite; a different ID or a name that doesn't resolve blocks. Fresh pins fail closed: an
// inconclusive check keeps them out of the write.
//
// Under the Dependabot proxy any inconclusive check is an error: the run
// aborts so Dependabot retries rather than shipping an unverified lock.
func checkParity(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report, recordedKeys map[string]bool, resolved []dep.Dependency, parents map[string][]string, mode parityMode) error {
	gh := r.GHClient()
	if gh == nil || store == nil {
		return nil
	}
	reportIdx := make(map[string]int, len(report.Workflows))
	for i, wr := range report.Workflows {
		reportIdx[wr.Path] = i
	}
	var pins []ghapi.PinCheck
	pinIdx := map[string]int{}
	owners := map[int][]parityOwner{}
	owned := map[[2]int]int{}
	add := func(o parityOwner) {
		if o.fresh && o.lp.Action.RepoID == 0 {
			o.lp.Action.RepoID = store.RecordedRepoID(o.lp.Pin.Owner, o.lp.Pin.Repo)
			if o.lp.Action.RepoID == 0 {
				o.lp.Action.RepoID = store.RecordedRepoID(o.ref.Owner, o.ref.Repo)
			}
		}
		// Pin.String folds owner/repo only: refs are case-sensitive, as
		// the runner keys them.
		// A recorded and a fresh owner of one pin can have different targets
		// (the recorded ref, and what this run will write), so the target is
		// part of the key. The lockfile parser has already rejected an entry
		// ref the Actions backend resolver would not accept.
		// An entry that records its own SHA has no ref to look up: only the
		// repository and commit are checked here, and the entry is reported
		// or re-recorded elsewhere (see lockfile.RecordsOwnSHA).
		head := o.lp.Action.Ref
		if lockfile.RecordsOwnSHA(o.lp.Pin, head) {
			head = ""
		}
		key := o.lp.Pin.String() + ":" + strings.ToLower(lockedSHA(o.lp.Action.Commit)) + ":" + head
		i, seen := pinIdx[key]
		if !seen {
			i = len(pins)
			pinIdx[key] = i
			pins = append(pins, ghapi.PinCheck{Owner: o.lp.Pin.Owner, Repo: o.lp.Pin.Repo, SHA: lockedSHA(o.lp.Action.Commit), Head: head})
		}
		if j, dup := owned[[2]int{o.report, i}]; dup {
			owners[i][j].fresh = owners[i][j].fresh || o.fresh
			return
		}
		owned[[2]int{o.report, i}] = len(owners[i])
		owners[i] = append(owners[i], o)
	}
	fresh := freshClosure(resolved, parents)
	// Recorded entries skip resolution, so nothing else reads their
	// manifests. A run that rewrites an entry reads its replacement live.
	roots := map[int][]manifestNode{}
	byKey := map[string]string{}
	for k := range store.File().Dependencies {
		if p, ok := parserlock.ParsePin(k); ok {
			byKey[pinKey(p.NWO, p.Ref)] = k
		}
	}
	for _, pw := range parsed {
		ri, ok := reportIdx[pw.Path]
		if !ok || isBlocked(pw) {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		// rootRef is the workflow ref that still uses a recorded root, by
		// the same rule as the stale check (lockfile.RootUse): the use
		// naming its key, else one naming its commit.
		use := lockfile.RootUse(recorded)
		rootRef := func(p parserlock.Pin, a parserlock.Action) (parserlock.ActionRef, bool) {
			return use(p, a.Commit)
		}
		live := func(p parserlock.Pin, a parserlock.Action) bool {
			_, ok := rootRef(p, a)
			return ok
		}
		// Planning writes a recorded entry back verbatim unless one of
		// these findings prunes it.
		effects := map[string]checks.PruneEffect{}
		for _, f := range report.Workflows[ri].Findings {
			if checks.PrunesDependency(f, mode.acceptMoved, mode.relock) {
				k := checks.PruneKey(f.Dependency.NWO, f.Dependency.Ref, f.Dependency.SHA)
				// A dropped use leaves the key behind: it wins.
				if e := checks.PruneEffectOf(f, mode.acceptMoved, mode.relock); effects[k] != checks.Dropped {
					effects[k] = e
				}
			}
		}
		closure := store.Closure(workflowfile.KeyFromPath(pw.Path), live)
		edges := compositeEdges(closure)
		for _, lp := range closure {
			ref, _ := rootRef(lp.Pin, lp.Action)
			if lp.Parent != "" {
				ref = parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: lp.Pin.Ref}
			}
			k := lp.Pin.String()
			o := parityOwner{report: ri, pw: pw, lp: lp, ref: ref,
				away:   plannedAway(lp, edges[k], effects[checks.PruneKey(lp.Pin.NWO, lp.Pin.Ref, lockedSHA(lp.Action.Commit))]),
				heldBy: heldBy(lp, edges[k]),
			}
			add(o)
			if key := byKey[pinKey(lp.Pin.NWO, lp.Pin.Ref)]; key != "" && lp.Parent == "" && (mode.readOnly || !mode.relock && !o.away) {
				for _, path := range rootPaths(recorded, lp) {
					roots[ri] = append(roots[ri], manifestNode{key: key, path: path})
				}
			}
		}
		if pw.Resolved {
			continue
		}
		for _, o := range fresh(pw, recordedKeys) {
			o.report = ri
			add(o)
			// A seeded pin, a composite's child included, was copied from
			// the lockfile with its recorded children, not read.
			if k := byKey[pinKey(o.lp.Pin.NWO, o.lp.Pin.Ref)]; k != "" && r.Seeded(o.ref) {
				roots[ri] = append(roots[ri], manifestNode{key: k, path: o.ref.Path})
			}
			// A fresh pin the lockfile already records at the same commit,
			// such as a composite only an unselected workflow locks, keeps
			// its recorded descendants when the write reuses its recorded
			// `uses:`, so they are checked as recorded pins. A pin read live
			// is checked by what it read; any recorded child kept only for
			// an unwritten workflow isn't this write's (see
			// lockfile.State.admitUses). Entry repair proves an own-SHA one.
			if !o.reuses {
				continue
			}
			key := lockfile.EntryKey(o.lp.Pin)
			if recorded, ok := store.Entry(key); !ok || lockedSHA(recorded.Commit) != strings.ToLower(lockedSHA(o.lp.Action.Commit)) {
				continue
			}
			for _, lp := range store.Descendants(key) {
				if lockfile.RecordsOwnSHA(lp.Pin, lp.Action.Ref) {
					continue
				}
				add(parityOwner{report: ri, pw: pw, lp: lp,
					ref:    parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: lp.Pin.Ref},
					heldBy: lockParent(lp),
				})
			}
		}
	}
	if len(pins) == 0 {
		return nil
	}

	states := gh.CheckPins(ctx, pins)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// tagMoved marks an exact-version tag that moved ahead of its commit. A
	// branch named like one is a floating ref.
	tagMoved := make([]bool, len(states))
	// answered holds GitHub's answer to each comparison it answered, by
	// compareKey.
	answered := map[string]resolve.AncestryStatus{}
	defer func() {
		for i := range report.Workflows {
			dropSuperseded(&report.Workflows[i], answered)
		}
	}()
	for i := range states {
		st, pc := &states[i], pins[i]
		if pc.Head == "" || st.Err != nil || !st.CommitFound || st.HeadOID == "" || strings.EqualFold(st.HeadOID, pc.SHA) {
			continue
		}
		ancestry, detail, cause := r.CheckAncestryCause(ctx, pc.Owner, pc.Repo, pc.SHA, st.HeadOID)
		if cause == nil && ancestry != resolve.AncestryUnknown {
			answered[compareKey(pc.Owner+"/"+pc.Repo, pc.SHA, st.HeadOID)] = ancestry
		}
		switch {
		case ancestry == resolve.AncestryNotAncestor && cause == nil:
			st.CommitFound = false
		case ancestry == resolve.AncestryConfirmed:
			tagMoved[i] = st.HeadIsTag && exactTag(pc.Head)
		default:
			// GitHub didn't answer, or refused: the commit is unproven.
			brief := fmt.Sprintf("could not check %s history", pc.Head)
			err := fmt.Errorf("checking %s history: %s", pc.Head, detail)
			if cause != nil {
				brief += ": " + ghapi.Brief(cause)
				err = fmt.Errorf("checking %s history: %w", pc.Head, cause)
			}
			st.Err = &ghapi.BriefError{Msg: brief, Err: err}
		}
	}
	for _, st := range states {
		if err := ghapi.ConfigError(st.Err); err != nil {
			return err
		}
	}
	if ghapi.DependabotProxy() {
		for i, st := range states {
			if st.Err != nil {
				msg := fmt.Sprintf("could not verify locked %s/%s@%s", pins[i].Owner, pins[i].Repo, parserlock.ShortSHA(pins[i].SHA))
				return &ghapi.BriefError{Msg: msg + ": " + briefCause(st.Err), Err: fmt.Errorf("%s: %s", msg, verbose(st.Err))}
			}
		}
	}
	unhealthy := map[string]bool{}
	verified := map[string]bool{}
	defer func() {
		if report.Verified == nil {
			report.Verified = map[string]bool{}
		}
		for k := range verified {
			report.Verified[k] = true
		}
	}()
	for i, st := range states {
		for _, o := range owners[i] {
			f, ok := parityFinding(o.pw, o.ref, o.lp, pins[i], st)
			// A pin parity blocks or can't check is not worth reading.
			if ok && (f.Severity == checks.SeverityError || st.Err != nil) {
				unhealthy[pinKey(pins[i].Owner+"/"+pins[i].Repo, pins[i].SHA)] = true
			}
			var found []checks.Finding
			if ok {
				found = append(found, f)
			}
			// A same-ID rename doesn't vouch for the commit, so the tag is
			// still checked alongside it.
			renamed := ok && f.Category == checks.RepoRenamed && f.Severity == checks.SeverityWarning
			if tagMoved[i] && (!ok || renamed) {
				switch {
				case exactTag(o.lp.Pin.Ref):
					found = append(found, tagCommitMismatchFinding(o.pw, o.ref, o.lp, pins[i], st))
				case mode.verify:
					found = append(found, movedTagFinding(o.pw, o.ref, o.lp, pins[i], st))
				}
			}
			wr := &report.Workflows[o.report]
			if len(found) == 0 {
				if o.fresh {
					verified[checks.VerifiedKey(lockfile.EntryKey(o.lp.Pin), pins[i].SHA)] = true
				}
				continue
			}
			clean := true
			for _, f := range found {
				if o.fresh {
					f.FreshRef = true
				} else {
					f.RecordedRef = o.lp.Action.Ref
				}
				verdict, blocking := disposeParity(mode, &f, paritySubject{pin: o.lp.Pin, fresh: o.fresh, ownSHA: !o.fresh && lockfile.RecordsOwnSHA(o.lp.Pin, o.lp.Action.Ref), away: o.away, heldBy: o.heldBy, head: pins[i].Head, inconclusive: unanswered(st.Err)})
				if verdict != verdictKeep {
					wr.Deferred = append(wr.Deferred, checks.Deferred{
						Key: lockfile.EntryKey(o.lp.Pin), Ref: o.lp.Action.Ref, Commit: pins[i].SHA, Finding: blocking,
					})
				}
				if verdict == verdictExcuse {
					clean = false
					continue
				}
				// Dependabot reads back only actions.lock, so a rewritten `uses:`
				// would never ship; keep the pin under the name the workflow uses.
				if f.Category == checks.RepoRenamed && f.Severity == checks.SeverityWarning && o.lp.Parent == "" && !ghapi.DependabotProxy() {
					r.Redirect(o.lp.Pin.Owner, o.lp.Pin.Repo, o.ref, st.NameWithOwner)
				}
				if f.Severity == checks.SeverityError {
					clean = false
					wr.Findings = dropValid(wr.Findings)
					wr.SkipCommit = true
				}
				wr.Findings = append(wr.Findings, f)
			}
			if o.fresh && clean {
				verified[checks.VerifiedKey(lockfile.EntryKey(o.lp.Pin), pins[i].SHA)] = true
			}
		}
	}
	return checkManifests(ctx, r, store.File(), byKey, report, roots, unhealthy)
}

// rootPaths are the sub-action paths through which a workflow's recorded
// uses reach the root entry lp, matched as lockfile.RootUse matches them.
func rootPaths(recorded []parserlock.ActionRef, lp lockfile.LockedPin) []string {
	var out []string
	sha := lockedSHA(lp.Action.Commit)
	for _, ref := range recorded {
		if !strings.EqualFold(ref.NWO(), lp.Pin.NWO) || slices.Contains(out, ref.Path) {
			continue
		}
		if ref.Ref == lp.Pin.Ref || parserlock.IsFullSha(ref.Ref) && strings.EqualFold(ref.Ref, sha) {
			out = append(out, ref.Path)
		}
	}
	return out
}

// unanswered reports a check GitHub didn't answer, which a later run may
// complete. An SSO block whose anonymous fallback was rate-limited is
// answered: authorizing the token is the fix.
func unanswered(err error) bool {
	return ghapi.IsTransient(err) && !errors.As(err, new(*ghapi.SSORateLimitedError))
}

// dropSuperseded fails closed: it settles a finding only where another
// check answers the same question. A ref-moved check that couldn't compare
// a pin's commit with the ref's takes parity's answer to that comparison
// (in answered, by compareKey): confirmed makes it the ref-moved warning,
// so --relock still re-pins it; anything else leaves parity's own finding.
// It also goes once parity reports its own error on the pin. A workflow's
// resolver outage goes once a pin of the workflow blocks, now or should its
// deferred finding be restored.
func dropSuperseded(wr *checks.WorkflowReport, answered map[string]resolve.AncestryStatus) {
	pinBlocks := func(f checks.Finding) bool {
		return f.Severity == checks.SeverityError && f.DepKey() != ""
	}
	parityBlocks := map[string]bool{}
	for _, f := range wr.Findings {
		if pinBlocks(f) && (f.RecordedRef != "" || f.FreshRef) {
			parityBlocks[findingPinKey(f)] = true
		}
	}
	for _, d := range wr.Deferred {
		if pinBlocks(d.Finding) {
			parityBlocks[findingPinKey(d.Finding)] = true
		}
	}
	blocked := slices.ContainsFunc(wr.Findings, pinBlocks) || len(parityBlocks) > 0
	wr.Findings = slices.DeleteFunc(wr.Findings, func(f checks.Finding) bool {
		switch {
		case refMovedUnproven(f) && f.Dependency != nil:
			if parityBlocks[findingPinKey(f)] {
				return true
			}
			a, ok := answered[compareKey(f.Dependency.NWO, f.Dependency.SHA, f.ObservedSHA)]
			return ok && a != resolve.AncestryConfirmed
		case f.Category == checks.ReachabilityUnknown && f.Retryable && f.DepKey() == "":
			return blocked
		}
		return false
	})
	for i, f := range wr.Findings {
		if !refMovedUnproven(f) || f.Dependency == nil {
			continue
		}
		if a, ok := answered[compareKey(f.Dependency.NWO, f.Dependency.SHA, f.ObservedSHA)]; ok && a == resolve.AncestryConfirmed {
			wr.Findings[i].MarkRefMoved()
		}
	}
}

// compareKey names a comparison of a pinned commit with a ref's commit.
func compareKey(nwo, pinned, head string) string {
	return strings.ToLower(nwo + "@" + pinned + "..." + head)
}

func findingPinKey(f checks.Finding) string {
	if f.Dependency == nil {
		return ""
	}
	return checks.PruneKey(f.Dependency.NWO, f.Dependency.Ref, f.Dependency.SHA)
}

// refMovedUnproven reports a ref-moved check whose comparison GitHub didn't
// answer or refused (see checks.checkOneRefMoved).
func refMovedUnproven(f checks.Finding) bool {
	return f.Category == checks.AncestryUnknown || f.Category == checks.RepoUnavailable && f.ObservedSHA != ""
}

type parityOwner struct {
	report int
	pw     checks.ParsedWorkflow
	lp     lockfile.LockedPin
	ref    parserlock.ActionRef
	fresh  bool
	// reuses marks a fresh pin whose write keeps the lockfile's recorded
	// `uses:`: one taken from the lockfile, or a composite not read whole.
	reuses bool
	// away marks a recorded entry planning replaces or removes: see
	// plannedAway.
	away bool
	// heldBy is the composite that keeps this entry as recorded when
	// --accept-moved re-resolves the workflow's use: see heldBy.
	heldBy string
}

// compositeEdges maps each pin a composite in the recorded closure lists in
// its `uses:` to those composites. Keys are Pin.String.
func compositeEdges(closure []lockfile.LockedPin) map[string][]lockfile.LockedPin {
	out := map[string][]lockfile.LockedPin{}
	for _, lp := range closure {
		for _, child := range lp.Action.Uses {
			if p, ok := parserlock.ParsePin(child); ok {
				out[p.String()] = append(out[p.String()], lp)
			}
		}
	}
	return out
}

// plannedAway reports whether planning provably replaces or removes a
// recorded entry, given what a finding makes it do with the workflow's use
// (checks.PruneEffect). Planning prunes only the workflow's own pins, and
// writing a workflow keeps every recorded edge, so an entry a composite
// lists survives unless the workflow's use overwrites that very key: a
// mutable ref re-resolved in place. A full-SHA use may be re-keyed by
// narrowing, and a dropped use leaves the key behind. composites are the
// recorded composites that list lp's key.
func plannedAway(lp lockfile.LockedPin, composites []lockfile.LockedPin, effect checks.PruneEffect) bool {
	if lp.Parent != "" {
		return false
	}
	switch effect {
	case checks.Reresolved:
		return len(composites) == 0 || !parserlock.IsFullSha(lp.Pin.Ref)
	case checks.Dropped:
		return len(composites) == 0
	}
	return false
}

// heldBy is the composite (NWO@ref) that keeps a recorded entry as recorded
// even when --accept-moved re-resolves the workflow's use of it, or "" when
// that repairs it. Repair advice and the override both ask this.
func heldBy(lp lockfile.LockedPin, composites []lockfile.LockedPin) string {
	switch {
	case plannedAway(lp, composites, checks.Reresolved):
		return ""
	case lp.Parent != "":
		return lockParent(lp)
	}
	return composites[0].Pin.NWO + "@" + composites[0].Pin.Ref
}

// freshClosure returns a function yielding the closure of a workflow's
// unrecorded refs from this run's single resolve, so no further requests
// are made per workflow.
func freshClosure(resolved []dep.Dependency, parents map[string][]string) func(checks.ParsedWorkflow, map[string]bool) []parityOwner {
	children := map[string][]dep.Dependency{}
	byKey := map[string][]dep.Dependency{}
	// As planning marks it: a parent with an edge to no resolved dep wasn't
	// read whole and keeps its recorded edges.
	depKeys := map[string]bool{}
	for _, d := range resolved {
		depKeys[d.Key()] = true
	}
	partial := map[string]bool{}
	for child, ps := range parents {
		if !depKeys[child] {
			for _, p := range ps {
				partial[p] = true
			}
		}
	}
	for _, d := range resolved {
		if d.SHA == "" {
			continue
		}
		keys := []string{d.Key()}
		for _, ref := range d.OriginalRefs {
			keys = append(keys, ref.NWO()+"@"+ref.Ref)
		}
		for _, k := range keys {
			byKey[foldKey(k)] = append(byKey[foldKey(k)], d)
			for _, p := range parents[k] {
				children[foldKey(p)] = append(children[foldKey(p)], d)
			}
		}
	}
	return func(pw checks.ParsedWorkflow, recordedKeys map[string]bool) []parityOwner {
		var out []parityOwner
		seen := map[string]bool{}
		var walk func(d dep.Dependency, ref parserlock.ActionRef, parent string)
		walk = func(d dep.Dependency, ref parserlock.ActionRef, parent string) {
			id := foldKey(d.Key()) + "/" + d.Path
			if seen[id] {
				return
			}
			seen[id] = true
			owner, repo := d.OwnerRepo()
			o := parityOwner{pw: pw, fresh: true, reuses: d.FromLock || partial[d.Key()], ref: ref, lp: lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: d.NWO, Owner: owner, Repo: repo, Ref: d.Ref},
				Action: parserlock.Action{Ref: freshEntryRef(d), Commit: d.SHA},
				Parent: parent,
			}}
			out = append(out, o)
			for _, c := range children[foldKey(d.Key())] {
				co, cr := c.OwnerRepo()
				walk(c, parserlock.ActionRef{Owner: co, Repo: cr, Path: c.Path, Ref: c.Ref}, d.Key())
			}
		}
		for _, ref := range pw.Refs {
			key := foldKey(ref.Owner + "/" + ref.Repo + "@" + ref.Ref)
			if recordedKeys[key] {
				continue
			}
			for _, d := range byKey[key] {
				walk(d, ref, "")
			}
		}
		return out
	}
}

// briefCause is err for a terminal line: see ghapi.Brief.
func briefCause(err error) string {
	var be *ghapi.BriefError
	if errors.As(err, &be) {
		return be.Msg
	}
	return ghapi.Brief(err)
}

// verbose is err with any underlying detail a BriefError shortened.
func verbose(err error) string {
	var be *ghapi.BriefError
	if errors.As(err, &be) {
		return be.Verbose()
	}
	return err.Error()
}

// exactTag reports whether ref is a full semver release tag (v4.2.1).
func exactTag(ref string) bool {
	sv, ok := parserlock.ParseSemVer(ref)
	return ok && sv.IsFull()
}

// tagCommitMismatchFinding blocks a lock entry keyed by an exact release tag
// whose locked commit is an older commit in the tag's history. The rollback
// is invisible in the workflow, which names only the tag, so unlike a
// floating ref the lock is not sticky. --relock and --accept-moved excuse it
// (see disposeParity) and re-resolve the entry.
func tagCommitMismatchFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) checks.Finding {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Category:     checks.TagCommitMismatch,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		ObservedSHA:  st.HeadOID,
		ParentNWO:    lockParent(lp),
		Detail:       fmt.Sprintf("tag %s points at %s; the lockfile locks %s, an older commit in its history", pc.Head, parserlock.ShortSHA(st.HeadOID), parserlock.ShortSHA(pc.SHA)),
	}
	if f.ParentNWO != "" {
		f.Detail += fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	f.Remediation = checks.TagCommitMismatchRemediation(f.ParentNWO)
	return f
}

// movedTagFinding warns that the exact-version tag a full-SHA key records
// moved ahead of its locked commit. The commit is visible in the workflow
// and the Actions backend accepts the pin, so it never blocks. The advice
// must follow the tag: only changing `uses:` follows it, and only its
// parent changes a composite's pin.
func movedTagFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) checks.Finding {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Category:     checks.RefMoved,
		Severity:     checks.SeverityWarning,
		Confidence:   checks.ConfidenceHigh,
		ObservedSHA:  st.HeadOID,
		TagMoved:     true,
		ParentNWO:    lockParent(lp),
		Detail:       fmt.Sprintf("tag %s now points at %s; locked commit %s is still in its history", pc.Head, parserlock.ShortSHA(st.HeadOID), parserlock.ShortSHA(pc.SHA)),
		Remediation:  "the Actions backend accepts this pin; run `gh actions-lock --relock` to lock the tag's current commit",
	}
	if f.ParentNWO != "" {
		f.Detail += fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	if parserlock.IsFullSha(lp.Pin.Ref) {
		uses := nwo
		if ref.Path != "" {
			uses += "/" + ref.Path
		}
		f.Remediation = fmt.Sprintf("the Actions backend accepts this pin; to follow the tag, change `uses:` to %s@%s and run `gh actions-lock`", uses, pc.Head)
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("the Actions backend accepts this pin; only upgrading %s follows the tag", f.ParentNWO)
		}
	}
	return f
}

// lockParent is the NWO@ref of the composite that pins lp, or "" for a
// workflow's own pin.
func lockParent(lp lockfile.LockedPin) string {
	if parent, ok := parserlock.ParsePin(lp.Parent); ok {
		return parent.NWO + "@" + parent.Ref
	}
	return ""
}

// freshEntryRef is the ref a fresh pin is checked against. A full-SHA key
// has none yet: planning picks its tag or branch by reverse lookup, which
// only accepts a tag at the commit or a branch containing it.
func freshEntryRef(d dep.Dependency) string {
	if parserlock.IsFullSha(d.Ref) {
		return ""
	}
	return d.Ref
}

func parityFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) (checks.Finding, bool) {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
	}
	f.ParentNWO = lockParent(lp)
	via := ""
	if f.ParentNWO != "" {
		via = fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	short := parserlock.ShortSHA(pc.SHA)
	switch {
	case st.Err != nil:
		f.Detail = fmt.Sprintf("could not verify locked %s@%s%s: %s", nwo, short, via, verbose(st.Err))
		f.TermDetail = fmt.Sprintf("could not verify locked %s@%s: %s", nwo, short, briefCause(st.Err))
		var be *ghapi.BriefError
		if errors.As(st.Err, &be) {
			f.TermDetail = be.Msg
		}
		if unanswered(st.Err) {
			f.Category = checks.ReachabilityUnknown
			f.Confidence = checks.ConfidenceLow
			f.MarkRetryable(st.Err)
			break
		}
		// GitHub answered, but not with the pin: a re-run won't change that.
		f.Category = checks.RepoUnavailable
		f.Remediation = fmt.Sprintf("use a token that can read %s or authorize SSO for %s, then re-run", nwo, pc.Owner)
	case st.RepoMissing:
		f.Category = checks.RepoUnavailable
		f.Detail = fmt.Sprintf("%s is missing or not visible to this token%s", nwo, via)
		moved := "if the repository moved or was deleted, update `uses:` and run `gh actions-lock`"
		access := fmt.Sprintf("if it's private or internal, use a token that can read it or authorize SSO for %s", pc.Owner)
		f.Remediation = moved + "; " + access
		if st.ViaFallback {
			f.Remediation = access + "; " + moved
		}
	case idChanged(lp.Action.RepoID, st.RepoID):
		f.Category = checks.RepoReplaced
		f.Detail = fmt.Sprintf("%s%s now resolves to a different repository than the one locked (repo ID %d, locked %d)", nwo, via, st.RepoID, lp.Action.RepoID)
		f.Remediation = "do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Point `uses:` at a repository you have verified"
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Stop using %s or upgrade it to a version that doesn't use %s", f.ParentNWO, nwo)
		}
	case !st.CommitFound:
		f.Category = checks.UnreachablePin
		f.Detail = checks.NotInHistoryDetail(pc.SHA, nwo, cmp.Or(pc.Head, ref.Ref), st.HeadOID) + via
		f.Remediation = checks.UnreachableRemediation(f.ParentNWO, nwo)
	case pc.Head != "" && st.HeadOID == "":
		f.Category = checks.UnreachablePin
		f.Detail = fmt.Sprintf("ref %s is not a tag or branch of %s; the Actions backend will reject this pin%s", pc.Head, nwo, via)
		f.RefGone = true
		// A full-SHA `uses:` names no ref to repoint; the lock entry is wrong.
		f.Remediation = checks.UnreachableRemediation(f.ParentNWO, nwo)
		if !parserlock.IsFullSha(lp.Pin.Ref) {
			f.Remediation = "point `uses:` at a tag or branch that exists, then run `gh actions-lock`"
			f.RefMissing = true
		}
		if ghapi.DescribeName(pc.Head) {
			// Refused by name, not looked up: it may well exist.
			f.Detail = fmt.Sprintf("ref %s looks like `git describe` output, which the Actions backend refuses as a tag or branch name; it will reject this pin%s", pc.Head, via)
			f.RefGone, f.RefRefused = false, true
			if f.RefMissing {
				f.Remediation = "point `uses:` at a tag or branch whose name doesn't end in -g<hex>, then run `gh actions-lock`"
			}
		}
	case st.NameWithOwner != "" && !strings.EqualFold(st.NameWithOwner, nwo):
		f.Category = checks.RepoRenamed
		f.Severity = checks.SeverityWarning
		f.Detail = fmt.Sprintf("%s was renamed or transferred to %s%s; the runner follows the redirect while the repo ID matches", nwo, st.NameWithOwner, via)
		uses := st.NameWithOwner
		if ref.Path != "" {
			uses += "/" + ref.Path
		}
		f.Remediation = fmt.Sprintf("run `gh actions-lock` to rewrite it as `uses: %s@%s`", uses, ref.Ref)
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("upgrade %s to a version that uses %s", f.ParentNWO, st.NameWithOwner)
		}
	default:
		return checks.Finding{}, false
	}
	return f, true
}

// foldKey lowercases the owner/repo of an NWO@ref key. Refs are
// case-sensitive, so folding them would let a fresh ref miss its closure.
func foldKey(k string) string {
	nwo, ref, _ := strings.Cut(k, "@")
	return strings.ToLower(nwo) + "@" + ref
}

func idChanged(locked, live int64) bool {
	return locked != 0 && live != 0 && locked != live
}

func lockedSHA(commit string) string {
	if i := strings.IndexByte(commit, '-'); i >= 0 {
		return commit[i+1:]
	}
	return commit
}

func isBlocked(pw checks.ParsedWorkflow) bool {
	return pw.LoadErr != nil || pw.DepsErr != nil ||
		len(pw.SelfRepositoryRefErrs) > 0 || len(pw.SelfRepositoryResolutionErrs) > 0
}

func dropValid(ff []checks.Finding) []checks.Finding {
	out := ff[:0]
	for _, f := range ff {
		if f.Category != checks.Valid {
			out = append(out, f)
		}
	}
	return out
}
