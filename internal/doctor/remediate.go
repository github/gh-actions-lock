package doctor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
)

// RemediateOptions controls the remediation flow.
type RemediateOptions struct {
	Interactive bool   // true when stderr is a TTY
	RepoOwner   string // owner of the repo being scanned (for same-owner detection)
	RepoName    string // name of the repo being scanned (used for repo-level remediations)
}

// Remediator walks through findings and applies fixes interactively.
type Remediator struct {
	prompter  Prompter
	resolver  *resolver.Resolver
	tagLister *TagLister
	client    *api.RESTClient
	store     *lockfile.Store
	output    *ui.UI
	opts      RemediateOptions

	state sessionState

	// mu guards the mutable counters and slices below so the parallel pin
	// worker pool (Pass B of Remediate) can safely update them concurrently.
	mu sync.Mutex

	// pinWorkers controls the size of the parallel pin pool. Defaults to
	// pinPoolSize but tests can override it.
	pinWorkers int

	// deferPins is set during Pass A of Remediate: handleNotPinned appends
	// to pendingPins instead of calling applyPin immediately, so Pass B can
	// run the actual pins in parallel.
	deferPins   bool
	pendingPins []WorkflowReport

	// parallel is true while Pass B's worker pool is draining pendingPins.
	// While set, startWork/stopWork are no-ops so workers do not race on
	// resolver.ProgressFn or the single-spinner detail line.
	parallel bool

	// sessionProgress is true while a single continuous spinner spans the whole
	// Remediate pass (non-interactive bulk pinning). While set, the per-workflow
	// startWork/stopWork calls only update the spinner detail instead of
	// stopping and restarting it, so there are no blank gaps between workflows.
	sessionProgress bool

	// curWorkflow / totalWorkflows track position within the pinning pass so
	// every spinner label can show an [i/N] counter. Set at the top of each
	// Remediate iteration and used by workLabel for both interactive (per-file
	// spinner) and bulk (session spinner) modes.
	curWorkflow    int
	totalWorkflows int

	// How many remaining occurrences of each choiceKey across all workflows.
	remaining map[string]int

	// Counters for summary.
	Fixed          int
	Skipped        int
	Alerted        int
	Unresolved     int
	SkippedDeps    []string // unique dep keys that were skipped (for summary)
	AlertedDeps    []string // dep keys that triggered security alerts (deduplicated)
	UnresolvedDeps []string // dep keys whose ref could not be resolved (e.g. bad tag)

	// AlertedWorkflows maps an alerted dep key to the workflow paths it blocked,
	// so the end-of-run summary can name every impacted workflow even when the
	// offending action is a transitive (non-direct) dependency.
	AlertedWorkflows map[string][]string

	// AlertedReasons maps an alerted dep key to concise, user-facing copy
	// explaining why it needs manual investigation (first write wins).
	AlertedReasons map[string]string

	// AlertedSuggestions maps an alerted dep key to a sane-release
	// suggestion (e.g. "v1.4.0 a1b2c3d") computed by
	// doctor.EnrichImpostorFindings — the most recent stable release whose
	// commit is still reachable from a branch. Empty for deps with no
	// available suggestion. Rendered alongside the reason in the summary.
	AlertedSuggestions map[string]string

	// AlertedSearched tracks dep keys whose sane-release walk ran (whether
	// or not it found a suggestion). Lets the summary distinguish "we
	// looked and found nothing" from "we never looked".
	AlertedSearched map[string]bool

	// FullScanDeps lists dep keys (owner/repo@ref) that were pinned but whose
	// commit was not on a canonical branch (default, protected, release/v*,
	// literal ref, lockfile hint) — reachability had to fall back to scanning
	// every branch. The commit is valid, but a full-scan fallback is a notable
	// signal the end-of-run summary surfaces in red. Deduplicated, insertion
	// order preserved.
	FullScanDeps []string

	// AutoFixedImpostors records impostor refs that were silently rewritten
	// to the latest reachable release tag during pinning. The end-of-run
	// summary surfaces these as a "✓ auto-pinned to a safer release —
	// review for sanity" section so the user can verify the substitution
	// (which may cross a major-version boundary) wasn't disruptive.
	// Insertion order preserved; deduplicated by (Workflow, NWO).
	AutoFixedImpostors []AutoFixedImpostor
}

// AutoFixedImpostor records a single auto-substitution made when an
// unreachable pinned ref had a sane-release suggestion available.
type AutoFixedImpostor struct {
	Workflow string // workflow path the rewrite was applied to
	NWO      string // owner/repo (no path)
	OldRef   string // ref as written before the rewrite (tag or SHA)
	OldSHA   string // commit SHA the impostor ref resolved to (full); empty when unknown
	NewTag   string // tag the workflow was rewritten to
	NewSHA   string // commit SHA the new tag points to (full)
}

// recordFullScanDep notes that depKey was reachable only via a full branch
// scan (not on a canonical branch). Deduplicated; first write wins.
func (rem *Remediator) recordFullScanDep(depKey string) {
	if depKey == "" {
		return
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	for _, d := range rem.FullScanDeps {
		if d == depKey {
			return
		}
	}
	rem.FullScanDeps = append(rem.FullScanDeps, depKey)
}

// recordAutoFixedImpostor notes that an impostor ref was silently rewritten
// to a sane-release tag during pinning. Deduplicated by (Workflow, NWO);
// first write wins so a workflow with multiple findings against the same
// action is only surfaced once.
func (rem *Remediator) recordAutoFixedImpostor(workflow, nwo, oldRef, oldSHA, newTag, newSHA string) {
	if workflow == "" || nwo == "" || newTag == "" {
		return
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	for _, fix := range rem.AutoFixedImpostors {
		if fix.Workflow == workflow && fix.NWO == nwo {
			return
		}
	}
	rem.AutoFixedImpostors = append(rem.AutoFixedImpostors, AutoFixedImpostor{
		Workflow: workflow,
		NWO:      nwo,
		OldRef:   oldRef,
		OldSHA:   oldSHA,
		NewTag:   newTag,
		NewSHA:   newSHA,
	})
}

// tryAutoFixImpostors rewrites a workflow's uses: lines for any impostor
// findings that have an enriched sane-release suggestion AND appear as a
// direct dep (matching one of wr.ActionRefs). Mutates wr.Findings in place
// to drop the auto-fixed impostors so the per-finding loop won't alert them
// again, and refreshes wr.ActionRefs from the rewritten file. Returns true
// when at least one rewrite was applied — caller should then run applyPin
// to resolve and pin against the new tag.
//
// Auto-fix only applies when SaneSuggestionTag is set (EnrichImpostorFindings
// already ran) and the dep is direct: transitive composite-action edges
// can't be fixed by editing the consumer's workflow file.
func (rem *Remediator) tryAutoFixImpostors(wr *WorkflowReport) bool {
	if wr == nil {
		return false
	}
	type pendingFix struct {
		nwo    string
		oldRef string
		oldSHA string
		newTag string
		newSHA string
	}
	rewrites := map[string]string{}
	var pending []pendingFix
	keep := wr.Findings[:0:0]
	for _, f := range wr.Findings {
		if f.Category != CategoryImpostorCommit || f.Dependency == nil || f.SaneSuggestionTag == "" {
			keep = append(keep, f)
			continue
		}
		owner, repo := f.Dependency.OwnerRepo()
		if owner == "" || repo == "" {
			keep = append(keep, f)
			continue
		}
		directRaw := directUsesFor(wr.ActionRefs, owner, repo, f.Dependency.Ref)
		if directRaw == "" {
			keep = append(keep, f)
			continue
		}
		rewrites[directRaw] = owner + "/" + repo + "@" + f.SaneSuggestionTag
		pending = append(pending, pendingFix{
			nwo:    owner + "/" + repo,
			oldRef: f.Dependency.Ref,
			oldSHA: f.Dependency.SHA,
			newTag: f.SaneSuggestionTag,
			newSHA: f.SaneSuggestionSHA,
		})
	}
	if len(rewrites) == 0 {
		return false
	}
	refs, applied, err := rem.applyImpostorRewrites(wr.Path, rewrites)
	if err != nil || !applied {
		return false
	}
	wr.ActionRefs = refs
	wr.Findings = keep
	for _, fix := range pending {
		rem.recordAutoFixedImpostor(wr.Path, fix.nwo, fix.oldRef, fix.oldSHA, fix.newTag, fix.newSHA)
	}
	return true
}

// errWorkflowAlerted signals that a workflow was skipped because one of its
// actions tripped a security gate (impostor commit or unverifiable
// reachability). The offending action is recorded via alertWorkflow; the
// Remediate loop treats this as non-fatal and continues with the next
// workflow so all impacted workflows are surfaced and the safe ones still pin.
var errWorkflowAlerted = errors.New("workflow alerted")

// alertWorkflow records a security refusal for depKey affecting workflowPath.
// The action is flagged for investigation but the run continues. Alerted
// counts unique offending actions; AlertedDeps and AlertedWorkflows drive the
// end-of-run summary. reason is concise copy shown beneath the dep.
func (rem *Remediator) alertWorkflow(workflowPath, depKey, reason, detail string) {
	rem.output.Error("%s: %s", workflowPath, detail)
	rem.recordAlertReason(depKey, reason)

	rem.mu.Lock()
	defer rem.mu.Unlock()
	seen := false
	for _, d := range rem.AlertedDeps {
		if d == depKey {
			seen = true
			break
		}
	}
	if !seen {
		rem.AlertedDeps = append(rem.AlertedDeps, depKey)
		rem.Alerted++
	}

	if rem.AlertedWorkflows == nil {
		rem.AlertedWorkflows = map[string][]string{}
	}
	for _, p := range rem.AlertedWorkflows[depKey] {
		if p == workflowPath {
			return
		}
	}
	rem.AlertedWorkflows[depKey] = append(rem.AlertedWorkflows[depKey], workflowPath)
}

// alertImpostor wraps alertWorkflow with a sane-release lookup against the
// action repo so the end-of-run summary can suggest a re-pin target (or
// signal that no recent release is reachable). Call sites that already
// produce a Finding go through addAlertedDep + recordAlertSuggestion; this
// path covers the pin-time refusal in apply.go where only the dep coords
// are in hand.
func (rem *Remediator) alertImpostor(workflowPath, owner, repo, ref, detail string) {
	depKey := owner + "/" + repo + "@" + ref
	rem.alertWorkflow(workflowPath, depKey, reasonForCategory(CategoryImpostorCommit), detail)
	tag, sha := FindSaneRelease(rem.tagLister, rem.resolver, owner, repo)
	rem.mu.Lock()
	defer rem.mu.Unlock()
	if rem.AlertedSearched == nil {
		rem.AlertedSearched = map[string]bool{}
	}
	rem.AlertedSearched[depKey] = true
	if tag == "" {
		return
	}
	if rem.AlertedSuggestions == nil {
		rem.AlertedSuggestions = map[string]string{}
	}
	if _, ok := rem.AlertedSuggestions[depKey]; ok {
		return
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	rem.AlertedSuggestions[depKey] = tag + " " + short
}

func (rem *Remediator) markUnresolved(key string) {
	rem.mu.Lock()
	defer rem.mu.Unlock()
	for _, k := range rem.UnresolvedDeps {
		if k == key {
			return
		}
	}
	rem.UnresolvedDeps = append(rem.UnresolvedDeps, key)
	rem.Unresolved++
}

// pinPoolSize is the default size of the parallel pin worker pool.
// Matches resolver.reachabilityConcurrency so the two phases consume the
// same rough number of in-flight HTTP requests per pinning session.
const pinPoolSize = pinpool.DefaultWorkers

// incFixed / incSkipped / incAlerted increment summary counters under the
// Remediator mutex. They exist so apply.go callers (which may run from the
// parallel pin pool) can update counters without racing.
func (rem *Remediator) incFixed() {
	rem.mu.Lock()
	rem.Fixed++
	rem.mu.Unlock()
}

func (rem *Remediator) incSkipped() {
	rem.mu.Lock()
	rem.Skipped++
	rem.mu.Unlock()
}

// NewRemediator creates a new Remediator.
func NewRemediator(p Prompter, r *resolver.Resolver, client *api.RESTClient, store *lockfile.Store, out *ui.UI, opts RemediateOptions) *Remediator {
	return &Remediator{
		prompter:   p,
		resolver:   r,
		tagLister:  NewTagLister(client),
		client:     client,
		store:      store,
		output:     out,
		opts:       opts,
		state:      newSessionState(),
		pinWorkers: pinPoolSize,
	}
}

// isSameOwner returns true if the action's owner matches the repo being scanned,
// meaning it's an internal/first-party action where default-branch pinning is sensible.
func (rem *Remediator) isSameOwner(actionOwner string) bool {
	return rem.opts.RepoOwner != "" && strings.EqualFold(rem.opts.RepoOwner, actionOwner)
}

// offerApplyAll checks if this dep appears in more workflows and auto-applies
// the same choice everywhere. No prompt needed — same dep, same tag, just do it.
func (rem *Remediator) offerApplyAll(dep *lockfile.Dependency, tag string) {
	key := choiceKey(dep)
	rem.remaining[key]--
	others := rem.remaining[key]
	if others <= 0 {
		return
	}

	rem.output.Detail("  ↳ applying %s to %d remaining %s", tag, others, ui.Pluralize(others, "file", "files"))
	rem.state.recordChoice(dep, tag)
}

// Remediate walks through a report and handles each workflow that needs attention.
func (rem *Remediator) Remediate(report *Report) error {
	actionable := report.WorkflowsNeedingAttention()

	// Pre-scan: count how many times each dep appears so we can offer "apply to all".
	rem.remaining = make(map[string]int)
	for _, wr := range actionable {
		for _, f := range wr.Findings {
			if f.Category == CategorySHAAsRef && f.Dependency != nil {
				rem.remaining[choiceKey(f.Dependency)]++
			}
		}
	}

	// Run a single continuous spinner across every workflow rather than
	// starting and stopping one per file — the latter clears the line between
	// workflows and looks jumpy. The label is updated at the top of each
	// iteration so forward progress stays visible. In interactive mode the
	// prompter pauses this spinner while a prompt is on screen and resumes it
	// afterwards, so prompts and the spinner don't fight over the terminal.
	//
	// If a spinner is already running (the check command keeps the "Checking"
	// spinner alive across the phase transition so there's no gap before
	// pinning starts), adopt it instead of restarting: relabel it here and
	// leave it for the caller to stop.
	if rem.output.IsTTY() {
		adopted := rem.output.ProgressActive()
		if !adopted {
			rem.output.StartProgress("Pinning dependencies")
		}
		rem.output.UpdateLabel("Pinning dependencies")
		rem.output.UpdateProgress("")
		// Wipe stale `→` / `✓` worker rows left over from the resolve phase
		// so they don't masquerade as in-flight work under the new label.
		rem.output.ClearWorkerStatuses()
		rem.resolver.ProgressFn = rem.output.UpdateProgress
		rem.sessionProgress = true
		defer func() {
			rem.sessionProgress = false
			rem.resolver.ProgressFn = nil
			if !adopted {
				rem.output.StopProgress()
			}
		}()
	}

	// Pass A: serial walk through findings. Prompts run in order so
	// interactive UX is preserved. Workflows whose pin can be deferred —
	// NotPinned, SHA-as-ref (auto-pick converts the file then enqueues),
	// and impostor auto-fix (file rewrite in tryAutoFixImpostors then
	// enqueue) — all route to submitPin and land in pendingPins for Pass B
	// to drain through the worker pool. Last-wins replacement in submitPin
	// keeps multiple SHA-as-ref findings on the same workflow at one entry
	// with the post-rewrite refs.
	rem.deferPins = true
	for i, wr := range actionable {
		rem.curWorkflow = i + 1
		rem.totalWorkflows = len(actionable)
		// Keep the spinner header informative during Pass A: long
		// sub-operations (SuggestTagsForSHA, GetRepoInfo, etc.) run
		// without firing the resolver's progress callback, which used
		// to leave the user looking at a bare "Pinning dependencies"
		// for seconds at a time. Anchoring the label to [i/N] and
		// pinning the current target to worker slot 0 guarantees the
		// spinner always reflects forward motion and a current file.
		if rem.output.IsTTY() {
			rem.output.UpdateLabel(fmt.Sprintf("[%d/%d] Pinning dependencies", i+1, len(actionable)))
			rem.output.SetWorkerStatus(0, "→ "+wr.Path)
		}
		if err := rem.remediateWorkflow(wr); err != nil {
			if errors.Is(err, errWorkflowAlerted) {
				// Security gate tripped for this workflow: it's already been
				// narrated and recorded. Skip it but keep pinning the rest.
				continue
			}
			rem.deferPins = false
			return err
		}
	}
	rem.deferPins = false

	// Pass B: drain pendingPins through a bounded worker pool. Each worker
	// owns a slot index that drives a stable "→ path" / "✓ path" row in the
	// UI's worker area; the top spinner label tracks done/total.
	if err := rem.runPinWorkers(); err != nil {
		return err
	}

	return nil
}

// submitPin enqueues a workflow for the parallel pin pass when deferPins is
// set, otherwise applies the pin synchronously. NotPinned and SHA-as-ref
// handlers both call this instead of applyPin directly so Remediate can
// batch the network-heavy work.
//
// When an entry for the same Path is already pending (the common case for
// SHA-as-ref where each finding rewrites the workflow and re-submits with
// the post-rewrite ActionRefs), the new wr replaces the old one. Last-wins
// is the right semantics because the latest submission carries refs that
// reflect every rewrite Pass A has done so far — earlier entries are stale
// and would make applyPin's resolver call run against pre-rewrite refs.
// NotPinned submits exactly once per workflow so the replacement path is a
// no-op for it; the virtual ActionRefs produced by offerDefaultBranch are
// preserved by construction.
func (rem *Remediator) submitPin(wr WorkflowReport) error {
	if rem.deferPins {
		rem.mu.Lock()
		defer rem.mu.Unlock()
		for i, p := range rem.pendingPins {
			if p.Path == wr.Path {
				rem.pendingPins[i] = wr
				return nil
			}
		}
		rem.pendingPins = append(rem.pendingPins, wr)
		return nil
	}
	return rem.applyPin(wr)
}

// dedupePinsByPath is retained as a defense-in-depth safety net. submitPin's
// last-wins replacement already prevents duplicates from being enqueued, so
// this should be a no-op in production. Keeping it (and its test) means a
// future code path that bypasses submitPin can't quietly reintroduce the
// stacked-workers-on-one-file behavior the original UX fix prevented.
func dedupePinsByPath(pins []WorkflowReport) []WorkflowReport {
	seen := make(map[string]struct{}, len(pins))
	out := make([]WorkflowReport, 0, len(pins))
	for _, p := range pins {
		if _, ok := seen[p.Path]; ok {
			continue
		}
		seen[p.Path] = struct{}{}
		out = append(out, p)
	}
	return out
}

// runPinWorkers drains rem.pendingPins through a bounded pool, surfacing each
// worker's current workflow as a subdued row beneath the top spinner. Errors
// other than errWorkflowAlerted are aggregated; the first one is returned
// once every worker has finished so a single failure doesn't strand siblings.
func (rem *Remediator) runPinWorkers() error {
	pins := rem.pendingPins
	rem.pendingPins = nil
	if len(pins) == 0 {
		return nil
	}

	// Dedupe by workflow path. With submitPin's last-wins replacement
	// this should already be a no-op (each path appears at most once in
	// pendingPins). Kept as defense-in-depth: a future code path that
	// appends directly to pendingPins without going through submitPin
	// would otherwise reintroduce stacked workers on the same file.
	pins = dedupePinsByPath(pins)

	if dbg := os.Getenv("GH_ACTIONS_PIN_DEBUG_PINS"); dbg != "" {
		seen := map[string]int{}
		for _, p := range pins {
			seen[p.Path]++
		}
		var dups []string
		for k, v := range seen {
			if v > 1 {
				dups = append(dups, fmt.Sprintf("%s=%d", k, v))
			}
		}
		fmt.Fprintf(os.Stderr, "\n[debug] pendingPins total=%d unique=%d dups=%v\n", len(pins), len(seen), dups)
	}

	// Hand the spinner over to per-worker rows: the resolver's own callback
	// is hushed (workers own the detail area now) and slot 0's "Pinning
	// dependencies" detail is wiped so the first worker row doesn't appear
	// to be paired with stale text.
	if rem.output.IsTTY() {
		rem.output.UpdateProgress("")
		rem.output.ClearWorkerStatuses()
		rem.resolver.ProgressFn = nil
	}
	rem.parallel = true
	defer func() {
		rem.parallel = false
		if rem.output.IsTTY() {
			rem.output.ClearWorkerStatuses()
			rem.resolver.ProgressFn = rem.output.UpdateProgress
		}
	}()

	// Match the parent "Pinning dependencies" wording so the headless
	// label-stem dedup collapses parent + per-worker counter into one
	// phase line instead of emitting a second "Pinning workflows" header.
	return pinpool.Run(
		rem.pinWorkers,
		rem.output,
		"Pinning dependencies",
		pins,
		func(wr WorkflowReport) string { return wr.Path },
		func(_ int, wr WorkflowReport) error {
			err := rem.applyPin(wr)
			// Security gate trips are non-fatal: the workflow is already
			// recorded in alertedWorkflows, so keep pinning siblings.
			if errors.Is(err, errWorkflowAlerted) {
				return nil
			}
			return err
		},
	)
}

// workLabel prefixes a spinner label with the current [i/N] workflow counter
// when one is set, so the pinning phase shows per-workflow progress in both
// interactive (one spinner per file) and bulk (single session spinner) modes.
func (rem *Remediator) workLabel(label string) string {
	if rem.totalWorkflows > 0 {
		return fmt.Sprintf("[%d/%d] %s", rem.curWorkflow, rem.totalWorkflows, label)
	}
	return label
}

func (rem *Remediator) depKey(f Finding) string {
	if f.Dependency != nil {
		return f.Dependency.Key()
	}
	if f.ActionRef != nil {
		return f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	return ""
}

func (rem *Remediator) addAlertedDep(f Finding) {
	key := rem.depKey(f)
	rem.debugf("alert dep category=%s key=%s workflow=%s nwo=%s choices=%v", f.Category, key, f.WorkflowPath, rem.repoNWO(f), rem.debugChoiceKeys())
	rem.recordAlertReason(key, reasonForCategory(f.Category))
	rem.recordAlertSuggestion(key, f)
	rem.mu.Lock()
	defer rem.mu.Unlock()
	for _, k := range rem.AlertedDeps {
		if k == key {
			return
		}
	}
	rem.AlertedDeps = append(rem.AlertedDeps, key)
}

// recordAlertReason stores concise investigation copy for a dep key. The first
// reason recorded wins so the earliest (most specific) signal is kept.
func (rem *Remediator) recordAlertReason(depKey, reason string) {
	if reason == "" {
		return
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	if rem.AlertedReasons == nil {
		rem.AlertedReasons = map[string]string{}
	}
	if _, ok := rem.AlertedReasons[depKey]; !ok {
		rem.AlertedReasons[depKey] = reason
	}
}

// recordAlertSuggestion stores a sane-release suggestion for a dep key when
// the finding carries one. First write wins so the earliest enrichment
// (typically the only one) is kept across multiple findings against the same
// dep.
func (rem *Remediator) recordAlertSuggestion(depKey string, f Finding) {
	if depKey == "" {
		return
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	if f.SaneSuggestionSearched {
		if rem.AlertedSearched == nil {
			rem.AlertedSearched = map[string]bool{}
		}
		rem.AlertedSearched[depKey] = true
	}
	if f.SaneSuggestionTag == "" {
		return
	}
	sha := f.SaneSuggestionSHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if rem.AlertedSuggestions == nil {
		rem.AlertedSuggestions = map[string]string{}
	}
	if _, ok := rem.AlertedSuggestions[depKey]; !ok {
		rem.AlertedSuggestions[depKey] = f.SaneSuggestionTag + " " + sha
	}
}

// reasonForCategory maps an investigation category to concise, user-facing copy.
func reasonForCategory(c Category) string {
	switch c {
	case CategoryImpostorCommit:
		return "pinned SHA isn't reachable from any branch — likely orphaned and benign, but could be an impostor commit; action publishers should tag releases from a branch"
	case CategoryLockfileForgery:
		return "pinned SHA was never in this ref's history — possible lockfile tampering"
	case CategoryMisleadingSHA:
		return "ref looks like a commit SHA but resolves to a different commit — possible deceptive ref"
	default:
		return "fails an integrity check — review before pinning"
	}
}

// skipDep records a dependency as skipped (needs interactive resolution).
func (rem *Remediator) skipDep(dep *lockfile.Dependency) {
	key := dep.Key()
	rem.output.Skip("%s: requires interactive tag selection", key)
	rem.mu.Lock()
	defer rem.mu.Unlock()
	rem.state.choices[key] = "skipped"
	rem.SkippedDeps = append(rem.SkippedDeps, key)
	rem.Skipped++
}

// shaConvertedForNWO returns true if any prior step in this run recorded a
// real (non-skipped) tag choice for the given owner/repo. We use this to
// suppress stale impostor/misleading/forgery alerts on deps whose SHA pin
// was already rewritten to a canonical tag — typically by handleSHAAsRef
// or by the reach loop in applyPin spreading a fix across workflow files.
func (rem *Remediator) shaConvertedForNWO(nwo string) bool {
	if nwo == "" {
		return false
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	prefix := nwo + "@"
	for k, v := range rem.state.choices {
		if v == "" || v == "skipped" {
			continue
		}
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (rem *Remediator) debugf(format string, args ...any) {
	if os.Getenv("GH_ACTIONS_PIN_DEBUG_ALERTS") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "\n[debug] "+format+"\n", args...)
}

func (rem *Remediator) debugChoiceKeys() []string {
	rem.mu.Lock()
	defer rem.mu.Unlock()
	keys := make([]string, 0, len(rem.state.choices))
	for k, v := range rem.state.choices {
		keys = append(keys, k+"="+v)
	}
	return keys
}

func (rem *Remediator) repoNWO(f Finding) string {
	if f.Dependency != nil {
		owner, repo := f.Dependency.OwnerRepo()
		if owner != "" {
			return owner + "/" + repo
		}
	}
	if f.ActionRef != nil {
		return f.ActionRef.Owner + "/" + f.ActionRef.Repo
	}
	return ""
}

// pinPromptTitle returns the Select prompt title annotated with repo visibility.
func (rem *Remediator) pinPromptTitle(nwo, owner, repo string) string {
	title := fmt.Sprintf("Pin %s to which tag?", nwo)
	if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
		title += fmt.Sprintf("  (%s)", info.VisibilityLabel())
	}
	return title
}
