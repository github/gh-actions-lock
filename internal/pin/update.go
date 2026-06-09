package pin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/workflowfile"
)

// UpdateOptions configures a targeted relock of a single action.
type UpdateOptions struct {
	Resolver *resolve.Resolver
	Store    *lockfile.State

	// TargetNWO is the owner/repo of the action to relock (e.g.
	// "actions/checkout").
	TargetNWO string
	// TargetRef is the new ref to relock to (e.g. "v6"). It is written
	// verbatim into the workflow uses: line and the lockfile pin — `update`
	// never narrows it (v6 stays v6, not v6.1.2).
	TargetRef string

	// WorkflowPaths is the set of workflow files to consider. Only files that
	// already reference TargetNWO and already have a lockfile entry are
	// eligible; everything else is ignored or flagged onboarding-required.
	WorkflowPaths []string
}

// ActionChange records one action ref bump computed for the relock. One entry
// is emitted per distinct (NWO, OldRef, OldSHA, NewRef, NewSHA) tuple across
// all touched workflows — workflows sharing the same bump collapse to one.
type ActionChange struct {
	NWO    string
	OldRef string
	NewRef string
	OldSHA string
	NewSHA string
}

// UpdatePlan is the precomputed outcome of a relock: the action changes, any
// diagnostic findings, and the per-workflow edits to apply. Edits are private;
// UpdateCommit consumes them.
type UpdatePlan struct {
	Changes  []ActionChange
	Findings []checks.Finding

	// Warnings are human-only diagnostics (e.g. expression-templated uses: refs
	// that update can't match). They never enter the JSON contract; runUpdate
	// surfaces them on stderr in non-JSON mode so a silent no-op is explainable.
	Warnings []string

	edits []workflowEdit
}

// workflowEdit is a fully precomputed change to a single eligible workflow:
// the spliced lockfile closure plus the uses: rewrite map. Everything here is
// computed before any disk mutation so a failure leaves the tree untouched.
type workflowEdit struct {
	path       string
	wfKey      string
	rewrites   map[string]string
	closure    []dep.Dependency
	parentMap  map[string][]string
	directKeys map[string]bool
}

// PlanUpdate computes a targeted relock without writing anything to disk. It
// resolves TargetRef once, then splices the new target subtree into each
// eligible workflow's existing closure, preserving every untargeted dependency
// verbatim. Onboarding is refused unconditionally: a workflow that uses the
// target but has no lockfile entry yields an onboarding-required finding and is
// skipped.
//
// A returned error is a tool failure (exit 2): bad target, resolve failure,
// transient discovery error. Blocking conditions that still leave clean output
// (onboarding-required, impostor target) are returned as Findings, not errors.
func PlanUpdate(ctx context.Context, opts UpdateOptions) (*UpdatePlan, error) {
	plan := &UpdatePlan{}

	eligible, err := selectEligible(opts, plan)
	if err != nil {
		return nil, err
	}
	if len(eligible) == 0 {
		return plan, nil
	}

	// Relock invariant (spec #9): the whole point of update is to swap a
	// human-readable ref for a freshly-pinned SHA. A bare-SHA TargetRef would
	// write `uses: nwo@<sha>` (sha-as-ref) and pin it to itself — a silent
	// non-relock. Refuse it structurally, before any network resolution, with a
	// blocking finding per eligible workflow. (Un-onboarded workflows already
	// have their onboarding-required findings appended by selectEligible.)
	if looksLikeSHA(opts.TargetRef) {
		for _, e := range eligible {
			plan.Findings = append(plan.Findings, shaAsRefTargetFinding(e.path, opts.TargetNWO, opts.TargetRef))
		}
		return plan, nil
	}

	targetDeps, targetPM, err := resolveTarget(ctx, opts)
	if err != nil {
		return nil, err
	}
	targetDep, ok := findTargetDep(targetDeps, opts.TargetNWO, opts.TargetRef)
	if !ok {
		return nil, fmt.Errorf("resolving %s@%s: target ref did not resolve to a commit", opts.TargetNWO, opts.TargetRef)
	}
	// Use the resolver's canonical NWO for every lockfile key so casing in the
	// --action argument can't desync from stored pins.
	canonNWO := targetDep.NWO
	newSHA := targetDep.SHA

	// Reuse Tag/Branch from already-locked deps so unchanged subtree members
	// aren't re-discovered over the network. Branch membership and tags are
	// properties of the commit, so a stored pin with the same (NWO, SHA) is a
	// safe source — keyed on NWO@SHA, not the ref. The target dep on a genuine
	// bump has a new SHA, so it (and any genuinely-new transitive) still gets a
	// fresh discovery pass that also gates impostors.
	existingBySHA := make(map[string]dep.Dependency)
	for _, d := range opts.Store.AllDeps() {
		if d.Branch != "" {
			existingBySHA[d.NWO+"@"+d.SHA] = d
		}
	}
	if imp, err := enrichBranches(ctx, opts.Resolver, targetDeps, existingBySHA); err != nil {
		return nil, err
	} else if imp != nil {
		for _, e := range eligible {
			plan.Findings = append(plan.Findings, impostorFinding(e.path, imp))
		}
		return plan, nil
	}

	seen := make(map[ActionChange]bool)
	for _, e := range eligible {
		existing, parentMap, directKeys, err := opts.Store.WorkflowClosure(e.wfKey)
		if err != nil {
			return nil, err
		}
		existingSHA := make(map[string]string, len(existing))
		for _, d := range existing {
			existingSHA[strings.ToLower(d.Key())] = d.SHA
		}

		rewrites := make(map[string]string)
		changed := false
		var localChanges []ActionChange
		for _, t := range e.targets {
			oldRef := t.Ref
			oldSHA := existingSHA[strings.ToLower(canonNWO+"@"+oldRef)]
			if oldRef == opts.TargetRef && oldSHA == newSHA {
				continue // already current — no-op
			}
			changed = true
			if oldRef != opts.TargetRef {
				// Key the rewrite on the exact value written in the YAML
				// (preserving owner/repo casing and any subpath) so the
				// anchored substitution matches.
				rewrites[t.FullName()+"@"+oldRef] = t.FullName() + "@" + opts.TargetRef
			}
			localChanges = append(localChanges, ActionChange{
				NWO:    canonNWO,
				OldRef: oldRef,
				NewRef: opts.TargetRef,
				OldSHA: oldSHA,
				NewSHA: newSHA,
			})
		}
		if !changed {
			continue
		}

		closure, mergedPM, mergedDirect := spliceClosure(
			existing, parentMap, directKeys,
			targetDeps, targetPM,
			canonNWO, opts.TargetRef,
		)
		edit := workflowEdit{
			path:       e.path,
			wfKey:      e.wfKey,
			rewrites:   rewrites,
			closure:    closure,
			parentMap:  mergedPM,
			directKeys: mergedDirect,
		}
		// Verify the spliced closure actually upholds the relock postcondition
		// for this workflow before we commit to writing it. In the current
		// engine this holds by construction, but a future change to
		// spliceClosure that silently dropped the target pin would otherwise
		// ship a no-op `update` as success. A violation is blocking, not fatal.
		// We record this workflow's changes only after it passes, so updated[]
		// never advertises a bump for an edit we end up skipping.
		if f, bad := verifyTargetPin(edit, canonNWO, opts.TargetRef, newSHA); bad {
			plan.Findings = append(plan.Findings, f)
			continue
		}
		plan.edits = append(plan.edits, edit)
		for _, ch := range localChanges {
			if !seen[ch] {
				seen[ch] = true
				plan.Changes = append(plan.Changes, ch)
			}
		}
	}

	sortChanges(plan.Changes)
	return plan, nil
}

// sortChanges orders the updated[] array deterministically. The engine appends
// changes while iterating workflows and their matched target refs, so the raw
// order depends on traversal; this imposes a stable total order on the tuple.
func sortChanges(changes []ActionChange) {
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.NWO != b.NWO {
			return a.NWO < b.NWO
		}
		if a.OldRef != b.OldRef {
			return a.OldRef < b.OldRef
		}
		if a.NewRef != b.NewRef {
			return a.NewRef < b.NewRef
		}
		if a.OldSHA != b.OldSHA {
			return a.OldSHA < b.OldSHA
		}
		return a.NewSHA < b.NewSHA
	})
}

// eligibleWorkflow is a workflow that uses the target and already has a
// lockfile entry, so it can be relocked.
type eligibleWorkflow struct {
	path  string
	wfKey string
	// targets are the matched uses: refs (exact casing/subpath as written),
	// used to key YAML rewrites precisely.
	targets []parserlock.ActionRef
}

// selectEligible partitions WorkflowPaths into the relockable set, appending an
// onboarding-required finding for any targeted-but-unonboarded workflow.
func selectEligible(opts UpdateOptions, plan *UpdatePlan) ([]eligibleWorkflow, error) {
	var eligible []eligibleWorkflow
	seenWarn := make(map[string]bool)
	for _, path := range opts.WorkflowPaths {
		wf, err := workflowfile.Load(path)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
		refs, warnings, _ := wf.ExtractActionRefs()
		for _, w := range warnings {
			if !seenWarn[w] {
				seenWarn[w] = true
				plan.Warnings = append(plan.Warnings, w)
			}
		}
		var targets []parserlock.ActionRef
		for _, r := range refs {
			if strings.EqualFold(r.NWO(), opts.TargetNWO) {
				targets = append(targets, r)
			}
		}
		if len(targets) == 0 {
			continue // doesn't reference the target
		}
		wfKey := workflowfile.KeyFromPath(path)
		if !opts.Store.HasWorkflow(wfKey) {
			plan.Findings = append(plan.Findings, onboardingFinding(path, opts.TargetNWO, opts.TargetRef))
			continue
		}
		eligible = append(eligible, eligibleWorkflow{path: path, wfKey: wfKey, targets: targets})
	}
	return eligible, nil
}

// resolveTarget resolves the target ref and its transitive subtree once. The
// result is shared across every eligible workflow.
func resolveTarget(ctx context.Context, opts UpdateOptions) ([]dep.Dependency, dep.ParentMap, error) {
	ar := parserlock.ParseActionRef(opts.TargetNWO + "@" + opts.TargetRef)
	if ar == nil {
		return nil, nil, fmt.Errorf("invalid target %q", opts.TargetNWO+"@"+opts.TargetRef)
	}
	deps, pm, err := opts.Resolver.ResolveAllRecursive(ctx, []parserlock.ActionRef{*ar})
	if err != nil {
		return nil, nil, fmt.Errorf("resolving %s@%s: %w", opts.TargetNWO, opts.TargetRef, err)
	}
	return deps, pm, nil
}

func findTargetDep(deps []dep.Dependency, nwo, ref string) (dep.Dependency, bool) {
	for _, d := range deps {
		if strings.EqualFold(d.NWO, nwo) && d.Ref == ref {
			return d, true
		}
	}
	return dep.Dependency{}, false
}

// enrichBranches populates Tag/Branch on the resolved target subtree. A dep
// whose (NWO, SHA) is already locked in the store reuses that metadata without
// a network call: branch membership and tags are properties of the commit, so
// the ref it was reached by is irrelevant. Only commits not already pinned —
// the bumped target and any genuinely-new transitive — are discovered. It
// returns a non-nil ImpostorError when a newly discovered SHA is on no branch
// (a blocking finding for the caller), or a plain error for transient
// discovery failures (a tool failure).
func enrichBranches(ctx context.Context, r *resolve.Resolver, deps []dep.Dependency, existingBySHA map[string]dep.Dependency) (*resolve.ImpostorError, error) {
	for i := range deps {
		if m, ok := existingBySHA[deps[i].NWO+"@"+deps[i].SHA]; ok {
			deps[i].Tag = m.Tag
			deps[i].Branch = m.Branch
			continue
		}
		owner, repo := deps[i].OwnerRepo()
		if owner == "" {
			continue
		}
		tag, branch, err := r.DiscoverContaining(ctx, owner, repo, deps[i].SHA, deps[i].Ref)
		if err != nil {
			var imp *resolve.ImpostorError
			if errors.As(err, &imp) {
				return imp, nil
			}
			return nil, fmt.Errorf("discovering branch for %s@%s: %w", deps[i].NWO, deps[i].Ref, err)
		}
		deps[i].Tag = tag
		deps[i].Branch = branch
	}
	return nil, nil
}

// spliceClosure merges the target's freshly-resolved subtree into a workflow's
// existing closure. Untargeted deps (and shared transitives) are preserved
// verbatim from existing; only the target dep and its genuinely-new transitives
// come from the resolved subtree. Every existing direct pin of the target NWO
// (other than newRef) is dropped from the direct set; Save's GC reclaims any
// now-orphaned old transitives.
func spliceClosure(
	existing []dep.Dependency, existingPM map[string][]string, existingDirect map[string]bool,
	targetDeps []dep.Dependency, targetPM dep.ParentMap,
	nwo, newRef string,
) ([]dep.Dependency, map[string][]string, map[string]bool) {
	newKey := nwo + "@" + newRef
	// The old target keys are whatever the lockfile currently records as a
	// direct pin for this NWO — robust against YAML/lockfile ref divergence.
	oldKeys := make(map[string]bool)
	for k := range existingDirect {
		if k != newKey && strings.EqualFold(keyNWO(k), nwo) {
			oldKeys[k] = true
		}
	}

	merged := make(map[string]dep.Dependency)
	var order []string
	put := func(d dep.Dependency) {
		if _, ok := merged[d.Key()]; !ok {
			order = append(order, d.Key())
		}
		merged[d.Key()] = d
	}
	for _, d := range existing {
		if oldKeys[d.Key()] {
			continue
		}
		put(d)
	}
	for _, d := range targetDeps {
		k := d.Key()
		if k == newKey {
			put(d) // target dep always wins (carries the new SHA)
			continue
		}
		if _, ok := merged[k]; !ok {
			put(d) // genuinely new transitive
		}
	}
	closure := make([]dep.Dependency, 0, len(order))
	for _, k := range order {
		closure = append(closure, merged[k])
	}

	direct := make(map[string]bool, len(existingDirect))
	for k := range existingDirect {
		if !oldKeys[k] {
			direct[k] = true
		}
	}
	direct[newKey] = true

	parentMap := make(map[string][]string)
	for child, parents := range existingPM {
		if oldKeys[child] {
			continue
		}
		parentMap[child] = append(parentMap[child], parents...)
	}
	for child, parents := range targetPM {
		parentMap[child] = unionStrings(parentMap[child], parents)
	}

	return closure, parentMap, direct
}

// keyNWO returns the NWO portion of a dep key (NWO@Ref).
func keyNWO(key string) string {
	if i := strings.LastIndex(key, "@"); i >= 0 {
		return key[:i]
	}
	return key
}

func unionStrings(dst, add []string) []string {
	have := make(map[string]bool, len(dst))
	for _, s := range dst {
		have[s] = true
	}
	for _, s := range add {
		if !have[s] {
			have[s] = true
			dst = append(dst, s)
		}
	}
	return dst
}

func onboardingFinding(path, nwo, ref string) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.OnboardingRequired,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail: fmt.Sprintf(
			"%s has no lockfile entry; update refuses to onboard new workflows (target %s@%s)",
			path, nwo, ref),
		Remediation: "pin this workflow first (run `gh actions-pin check`), then re-run update",
	}
}

func impostorFinding(path string, imp *resolve.ImpostorError) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.ImpostorCommit,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail:       imp.Error(),
		Remediation:  "the resolved commit is on no branch of the upstream repo; pin to a tagged release instead, or escalate to the action maintainer if you expected this ref to be reachable",
		ObservedSHA:  imp.SHA,
	}
}

// looksLikeSHA reports whether ref is shaped like a full git commit SHA (40 hex
// digits). update refuses a SHA-shaped target ref because relocking to a bare
// SHA would write a sha-as-ref instead of a freshly-pinned human-readable ref.
func looksLikeSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// verifyTargetPin re-checks one workflow's spliced closure against the relock
// postcondition: the target is a direct dep pinned at NewRef (a human-readable
// ref, not a bare SHA) whose recorded SHA equals the freshly-resolved newSHA. It
// returns a blocking finding and true when the postcondition is violated. The
// current engine satisfies this by construction; the guard converts any future
// regression in spliceClosure from a silent no-op-success into a loud blocking
// finding.
func verifyTargetPin(e workflowEdit, nwo, newRef, newSHA string) (checks.Finding, bool) {
	newKey := nwo + "@" + newRef
	if !e.directKeys[newKey] {
		return notPinnedTargetFinding(e.path, nwo, newRef), true
	}
	for _, d := range e.closure {
		if d.Key() == newKey {
			if d.SHA != newSHA {
				return staleTargetFinding(e.path, nwo, newRef, newSHA, d.SHA), true
			}
			return checks.Finding{}, false
		}
	}
	return notPinnedTargetFinding(e.path, nwo, newRef), true
}

func shaAsRefTargetFinding(path, nwo, ref string) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.ShaAsRef,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail: fmt.Sprintf(
			"target ref %q for %s is a bare commit SHA; update needs a tag or branch ref so it can keep a human-readable ref while pinning the SHA",
			ref, nwo),
		Remediation: fmt.Sprintf("pass --action %s@<tag|branch> (e.g. a release tag); update records the SHA for you", nwo),
	}
}

func notPinnedTargetFinding(path, nwo, ref string) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.NotPinned,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail: fmt.Sprintf(
			"after relock, %s@%s is not pinned in %s (lockfile entry missing)",
			nwo, ref, path),
		Remediation: "re-run update; if it persists, run `gh actions-pin check` to inspect the lockfile",
	}
}

func staleTargetFinding(path, nwo, ref, want, got string) checks.Finding {
	return checks.Finding{
		WorkflowPath: path,
		Category:     checks.Stale,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Detail: fmt.Sprintf(
			"after relock, %s@%s is pinned to %s but the ref resolves to %s",
			nwo, ref, got, want),
		Remediation: "re-run update to re-pin the target",
	}
}
