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
// transitive closure from the resolver cache) join the same batch; a
// blocking finding on one keeps its workflow out of the write.
func checkParity(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report, recordedKeys map[string]bool, keep func(checks.Category) bool) {
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
	owned := map[[2]int]bool{}
	add := func(o parityOwner) {
		key := o.lp.Pin.String() + ":" + o.lp.Action.Commit
		i, seen := pinIdx[key]
		if !seen {
			i = len(pins)
			pinIdx[key] = i
			pc := ghapi.PinCheck{Owner: o.lp.Pin.Owner, Repo: o.lp.Pin.Repo, SHA: lockedSHA(o.lp.Action.Commit)}
			if !o.fresh && checks.IsImmutableRef(o.lp.Pin.Ref) {
				pc.Tag = o.lp.Pin.Ref
			}
			pins = append(pins, pc)
		}
		if owned[[2]int{o.report, i}] {
			return
		}
		owned[[2]int{o.report, i}] = true
		owners[i] = append(owners[i], o)
	}
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
		for _, lp := range store.Closure(workflowfile.KeyFromPath(pw.Path)) {
			ref, isDirect := direct[strings.ToLower(lp.Pin.Owner+"/"+lp.Pin.Repo)+"@"+lp.Pin.Ref]
			if lp.Parent == "" && !isDirect {
				continue // stale workflow entry; pruned on write
			}
			if !isDirect {
				ref = parserlock.ActionRef{Owner: lp.Pin.Owner, Repo: lp.Pin.Repo, Ref: lp.Pin.Ref}
			}
			add(parityOwner{report: ri, pw: pw, lp: lp, ref: ref})
		}
		if pw.Resolved {
			continue
		}
		for _, o := range freshPins(ctx, r, pw, recordedKeys) {
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
			if !ok || !keep(f.Category) {
				continue
			}
			wr := &report.Workflows[o.report]
			if f.Severity == checks.SeverityError {
				wr.Findings = dropValid(wr.Findings)
				wr.SkipCommit = wr.SkipCommit || o.fresh
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

// freshPins returns the closure of pw's unrecorded refs as resolved earlier
// in this run. The resolver serves it from cache.
func freshPins(ctx context.Context, r *resolve.Resolver, pw checks.ParsedWorkflow, recordedKeys map[string]bool) []parityOwner {
	var roots []parserlock.ActionRef
	direct := map[string]parserlock.ActionRef{}
	for _, ref := range pw.Refs {
		key := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
		if !recordedKeys[key] {
			roots = append(roots, ref)
			direct[key] = ref
		}
	}
	if len(roots) == 0 {
		return nil
	}
	deps, parents, _ := r.ResolveAllRecursive(ctx, roots)
	var out []parityOwner
	for _, d := range deps {
		if d.SHA == "" {
			continue
		}
		owner, repo := d.OwnerRepo()
		o := parityOwner{pw: pw, fresh: true, lp: lockfile.LockedPin{
			Pin:    parserlock.Pin{NWO: d.NWO, Owner: owner, Repo: repo, Ref: d.Ref},
			Action: parserlock.Action{Commit: d.SHA},
		}}
		ref, isDirect := direct[strings.ToLower(d.NWO)+"@"+d.Ref]
		if !isDirect {
			ref = parserlock.ActionRef{Owner: owner, Repo: repo, Path: d.Path, Ref: d.Ref}
			if ps := parents[d.Key()]; len(ps) > 0 {
				o.lp.Parent = ps[0]
			}
		}
		o.ref = ref
		out = append(out, o)
	}
	return out
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
	case st.NameWithOwner != "" && !strings.EqualFold(st.NameWithOwner, nwo):
		f.Category = checks.RepoMoved
		f.Detail = fmt.Sprintf("%s now resolves to %s%s; the runner rejects pins to renamed or transferred repositories", nwo, st.NameWithOwner, via)
		f.Remediation = fmt.Sprintf("update `uses:` to %s@%s and run `gh actions-lock`", st.NameWithOwner, lp.Pin.Ref)
		if f.ParentNWO != "" {
			f.Remediation = fmt.Sprintf("upgrade %s to a version that uses %s, then run `gh actions-lock --relock`", f.ParentNWO, st.NameWithOwner)
		}
	case idChanged(lp.Action.OwnerID, st.OwnerID) || idChanged(lp.Action.RepoID, st.RepoID):
		f.Category = checks.RepoMoved
		f.Detail = fmt.Sprintf("%s is a different repository than the one locked (owner or repo ID changed)%s", nwo, via)
		f.Remediation = "investigate immediately — the original repository was deleted or its name was taken over"
	case !st.CommitFound:
		f.Category = checks.UnreachablePin
		f.Detail = fmt.Sprintf("locked commit %s no longer exists in %s%s", short, nwo, via)
		f.Remediation = "investigate immediately, then run `gh actions-lock --relock` to re-resolve"
	case pc.Tag != "" && !strings.EqualFold(st.TagOID, pc.SHA):
		f.Category = checks.UnreachablePin
		f.ObservedSHA = st.TagOID
		if st.TagOID == "" {
			f.Detail = fmt.Sprintf("tag %s no longer exists in %s; lockfile pins %s%s", pc.Tag, nwo, short, via)
		} else {
			f.Detail = fmt.Sprintf("tag %s now points at %s, lockfile pins %s%s", pc.Tag, parserlock.ShortSHA(st.TagOID), short, via)
		}
		f.Remediation = "investigate immediately — release tags should not move; run `gh actions-lock --relock` once verified"
	default:
		return checks.Finding{}, false
	}
	f.DocURL = DocURLFor(f.Category)
	return f, true
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
