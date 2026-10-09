package pipeline

import (
	"context"
	"fmt"
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
// still present, exact-version tag still at the commit. Mutable refs that
// moved are not reported; the lock is sticky until --relock.
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
func checkParity(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report, recordedKeys map[string]bool, resolved []dep.Dependency, parents map[string][]string, keep func(checks.Category) bool) {
	gh := r.GHClient()
	if gh == nil || store == nil {
		return
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
		key := strings.ToLower(o.lp.Pin.String() + ":" + lockedSHA(o.lp.Action.Commit))
		i, seen := pinIdx[key]
		if !seen {
			i = len(pins)
			pinIdx[key] = i
			pins = append(pins, ghapi.PinCheck{Owner: o.lp.Pin.Owner, Repo: o.lp.Pin.Repo, SHA: lockedSHA(o.lp.Action.Commit)})
		}
		if !o.fresh && checks.IsImmutableRef(o.lp.Pin.Ref) {
			pins[i].Tag = o.lp.Pin.Ref
		}
		if j, dup := owned[[2]int{o.report, i}]; dup {
			owners[i][j].fresh = owners[i][j].fresh || o.fresh
			return
		}
		owned[[2]int{o.report, i}] = len(owners[i])
		owners[i] = append(owners[i], o)
	}
	fresh := freshClosure(resolved, parents)
	for _, pw := range parsed {
		ri, ok := reportIdx[pw.Path]
		if !ok || isBlocked(pw) {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		direct := make(map[string]parserlock.ActionRef, len(recorded))
		for _, r := range recorded {
			direct[strings.ToLower(r.Owner+"/"+r.Repo)+"@"+r.Ref] = r
		}
		live := func(p parserlock.Pin) bool {
			_, ok := direct[strings.ToLower(p.Owner+"/"+p.Repo)+"@"+p.Ref]
			return ok
		}
		for _, lp := range store.Closure(workflowfile.KeyFromPath(pw.Path), live) {
			ref := direct[strings.ToLower(lp.Pin.Owner+"/"+lp.Pin.Repo)+"@"+lp.Pin.Ref]
			if lp.Parent != "" {
				ref = parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: lp.Pin.Ref}
			}
			add(parityOwner{report: ri, pw: pw, lp: lp, ref: ref})
		}
		if pw.Resolved {
			continue
		}
		for _, o := range fresh(pw, recordedKeys) {
			o.report = ri
			add(o)
		}
	}
	if len(pins) == 0 {
		return
	}

	states := gh.CheckPins(ctx, pins)
	if ctx.Err() != nil {
		return
	}
	for i, st := range states {
		for _, o := range owners[i] {
			f, ok := parityFinding(o.pw, o.ref, o.lp, pins[i], st)
			switch {
			case ok && o.fresh && st.Err != nil:
				f.Severity = checks.SeverityError
				f.Remediation = "retry; nothing is written until the repository identity is confirmed"
			case !ok || !keep(f.Category):
				continue
			}
			wr := &report.Workflows[o.report]
			if f.Category == checks.RepoRenamed && f.Severity == checks.SeverityWarning && o.lp.Parent == "" {
				r.Redirect(o.lp.Pin.Owner, o.lp.Pin.Repo, o.ref, st.NameWithOwner)
			}
			if f.Severity == checks.SeverityError {
				wr.Findings = dropValid(wr.Findings)
				wr.SkipCommit = true
			}
			wr.Findings = append(wr.Findings, f)
		}
	}
}

type parityOwner struct {
	report int
	pw     checks.ParsedWorkflow
	lp     lockfile.LockedPin
	ref    parserlock.ActionRef
	fresh  bool
}

// freshClosure returns a function yielding the closure of a workflow's
// unrecorded refs from this run's single resolve, so no further requests
// are made per workflow.
func freshClosure(resolved []dep.Dependency, parents map[string][]string) func(checks.ParsedWorkflow, map[string]bool) []parityOwner {
	children := map[string][]dep.Dependency{}
	byKey := map[string][]dep.Dependency{}
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
			o := parityOwner{pw: pw, fresh: true, ref: ref, lp: lockfile.LockedPin{
				Pin:    parserlock.Pin{NWO: d.NWO, Owner: owner, Repo: repo, Ref: d.Ref},
				Action: parserlock.Action{Commit: d.SHA},
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

func parityFinding(pw checks.ParsedWorkflow, ref parserlock.ActionRef, lp lockfile.LockedPin, pc ghapi.PinCheck, st ghapi.PinState) (checks.Finding, bool) {
	nwo := pc.Owner + "/" + pc.Repo
	f := checks.Finding{
		WorkflowPath: pw.Path,
		ActionRef:    &ref,
		Dependency:   &dep.Dependency{NWO: nwo, Path: ref.Path, Ref: lp.Pin.Ref, SHA: pc.SHA},
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
	}
	if lp.Parent != "" {
		if parent, ok := parserlock.ParsePin(lp.Parent); ok {
			f.ParentNWO = parent.NWO + "@" + parent.Ref
		}
	}
	via := ""
	if f.ParentNWO != "" {
		via = fmt.Sprintf(" (used by %s)", f.ParentNWO)
	}
	short := parserlock.ShortSHA(pc.SHA)
	switch {
	case st.Err != nil:
		f.Category = checks.ReachabilityUnknown
		f.Severity = checks.SeverityWarning
		f.Confidence = checks.ConfidenceLow
		f.Detail = fmt.Sprintf("could not verify locked %s@%s: %s", nwo, short, st.Err)
		f.Remediation = "retry; the runner will re-check this pin at job start"
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
		f.Category = checks.RepoHijacked
		f.Detail = fmt.Sprintf("%s now resolves to a different repository than the one locked (repo ID %d, locked %d)%s", nwo, st.RepoID, lp.Action.RepoID, via)
		f.Remediation = "do not trust it: the locked repository was deleted and its name taken over, possibly by an attacker. Point `uses:` at a repository you have verified"
	case !st.CommitFound:
		f.Category = checks.UnreachablePin
		f.Detail = fmt.Sprintf("locked commit %s no longer exists in %s%s", short, nwo, via)
		f.Remediation = "investigate immediately (a force-push or deleted branch can drop a commit), then run `gh actions-lock --accept-moved` to re-pin"
	case pc.Tag != "" && !strings.EqualFold(st.TagOID, pc.SHA):
		f.Category = checks.UnreachablePin
		f.ObservedSHA = st.TagOID
		if st.TagOID == "" {
			f.Detail = fmt.Sprintf("tag %s no longer exists in %s; lockfile pins %s%s", pc.Tag, nwo, short, via)
		} else {
			f.Detail = fmt.Sprintf("tag %s now points at %s, lockfile pins %s%s", pc.Tag, parserlock.ShortSHA(st.TagOID), short, via)
		}
		f.Remediation = "investigate immediately — release tags should not move; run `gh actions-lock --relock` once verified"
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
	f.DocURL = DocURLFor(f.Category)
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
	return pw.LoadErr != nil || pw.DepsErr != nil || len(pw.LocalPaths) > 0 ||
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
