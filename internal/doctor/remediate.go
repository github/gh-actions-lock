package doctor

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/lockfile"
	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
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
	// doctor.EnrichImposterFindings — the most recent stable release whose
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

	// AutoFixedImposters records impostor refs that were silently rewritten
	// to the latest reachable release tag during pinning. The end-of-run
	// summary surfaces these as a "✓ auto-pinned to a safer release —
	// review for sanity" section so the user can verify the substitution
	// (which may cross a major-version boundary) wasn't disruptive.
	// Insertion order preserved; deduplicated by (Workflow, NWO).
	AutoFixedImposters []AutoFixedImposter
}

// AutoFixedImposter records a single auto-substitution made when an
// unreachable pinned ref had a sane-release suggestion available.
type AutoFixedImposter struct {
	Workflow string // workflow path the rewrite was applied to
	NWO      string // owner/repo (no path)
	OldRef   string // ref as written before the rewrite (tag or SHA)
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

// recordAutoFixedImposter notes that an impostor ref was silently rewritten
// to a sane-release tag during pinning. Deduplicated by (Workflow, NWO);
// first write wins so a workflow with multiple findings against the same
// action is only surfaced once.
func (rem *Remediator) recordAutoFixedImposter(workflow, nwo, oldRef, newTag, newSHA string) {
	if workflow == "" || nwo == "" || newTag == "" {
		return
	}
	rem.mu.Lock()
	defer rem.mu.Unlock()
	for _, fix := range rem.AutoFixedImposters {
		if fix.Workflow == workflow && fix.NWO == nwo {
			return
		}
	}
	rem.AutoFixedImposters = append(rem.AutoFixedImposters, AutoFixedImposter{
		Workflow: workflow,
		NWO:      nwo,
		OldRef:   oldRef,
		NewTag:   newTag,
		NewSHA:   newSHA,
	})
}

// tryAutoFixImposters rewrites a workflow's uses: lines for any impostor
// findings that have an enriched sane-release suggestion AND appear as a
// direct dep (matching one of wr.ActionRefs). Mutates wr.Findings in place
// to drop the auto-fixed impostors so the per-finding loop won't alert them
// again, and refreshes wr.ActionRefs from the rewritten file. Returns true
// when at least one rewrite was applied — caller should then run applyPin
// to resolve and pin against the new tag.
//
// Auto-fix only applies when SaneSuggestionTag is set (EnrichImposterFindings
// already ran) and the dep is direct: transitive composite-action edges
// can't be fixed by editing the consumer's workflow file.
func (rem *Remediator) tryAutoFixImposters(wr *WorkflowReport) bool {
	if wr == nil {
		return false
	}
	type pendingFix struct {
		nwo    string
		oldRef string
		newTag string
		newSHA string
	}
	rewrites := map[string]string{}
	var pending []pendingFix
	keep := wr.Findings[:0:0]
	for _, f := range wr.Findings {
		if f.Category != CategoryImposterCommit || f.Dependency == nil || f.SaneSuggestionTag == "" {
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
			newTag: f.SaneSuggestionTag,
			newSHA: f.SaneSuggestionSHA,
		})
	}
	if len(rewrites) == 0 {
		return false
	}
	wf, err := lockfile.Load(wr.Path)
	if err != nil {
		return false
	}
	content, _, err := wf.RewriteActionRefs(rewrites)
	if err != nil {
		return false
	}
	if err := writeWorkflowFile(wr.Path, content); err != nil {
		return false
	}
	wf2, err := lockfile.Load(wr.Path)
	if err != nil {
		return false
	}
	refs2, _, _ := wf2.ExtractActionRefs()
	wr.ActionRefs = refs2
	wr.Findings = keep
	for _, fix := range pending {
		rem.recordAutoFixedImposter(wr.Path, fix.nwo, fix.oldRef, fix.newTag, fix.newSHA)
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

// alertImposter wraps alertWorkflow with a sane-release lookup against the
// action repo so the end-of-run summary can suggest a re-pin target (or
// signal that no recent release is reachable). Call sites that already
// produce a Finding go through addAlertedDep + recordAlertSuggestion; this
// path covers the pin-time refusal in apply.go where only the dep coords
// are in hand.
func (rem *Remediator) alertImposter(workflowPath, owner, repo, ref, detail string) {
	depKey := owner + "/" + repo + "@" + ref
	rem.alertWorkflow(workflowPath, depKey, reasonForCategory(CategoryImposterCommit), detail)
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
const pinPoolSize = 8

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
	// interactive UX is preserved. NotPinned workflows go to applyPin
	// indirectly via submitPin, which appends them to pendingPins for
	// Pass B to drain in parallel.
	rem.deferPins = true
	for i, wr := range actionable {
		rem.curWorkflow = i + 1
		rem.totalWorkflows = len(actionable)
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
// set, otherwise applies the pin synchronously. NotPinned handlers call this
// instead of applyPin directly so Remediate can batch the network-heavy work.
func (rem *Remediator) submitPin(wr WorkflowReport) error {
	if rem.deferPins {
		rem.mu.Lock()
		rem.pendingPins = append(rem.pendingPins, wr)
		rem.mu.Unlock()
		return nil
	}
	return rem.applyPin(wr)
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

	workers := rem.pinWorkers
	if workers <= 0 {
		workers = pinPoolSize
	}
	if workers > len(pins) {
		workers = len(pins)
	}

	// Hand the spinner over to per-worker rows: the top label tracks
	// progress, the resolver's own callback is hushed (workers own the
	// detail area now), and slot 0's "Pinning dependencies" detail is wiped
	// so the first worker row doesn't appear to be paired with stale text.
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

	total := int64(len(pins))
	var done atomic.Int64
	updateLabel := func() {
		// Match the parent "Pinning dependencies" wording so the headless
		// label-stem dedup collapses parent + per-worker counter into one
		// phase line instead of emitting a second "Pinning workflows" header.
		rem.output.UpdateLabel(fmt.Sprintf("[%d/%d] Pinning dependencies", done.Load(), total))
	}
	updateLabel()

	jobs := make(chan WorkflowReport, len(pins))
	for _, wr := range pins {
		jobs <- wr
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)
	for slot := 0; slot < workers; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for wr := range jobs {
				rem.output.SetWorkerStatus(slot, "→ "+wr.Path)
				err := rem.applyPin(wr)
				done.Add(1)
				updateLabel()
				if err != nil && !errors.Is(err, errWorkflowAlerted) {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					rem.output.SetWorkerStatus(slot, "")
					continue
				}
				rem.output.SetWorkerStatus(slot, "")
			}
		}(slot)
	}
	wg.Wait()

	return firstErr
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
	case CategoryImposterCommit:
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

func (rem *Remediator) remediateWorkflow(wr WorkflowReport) error {
	// Auto-fix impostor findings that have a sane-release suggestion before
	// any per-finding remediation: rewrite the workflow's uses: lines, then
	// re-pin from the rewritten file. This converts what would otherwise be
	// a "needs investigation" alert into a successful pin against the next
	// reachable release. The substitution may cross a major version (e.g.
	// v1.25.0 → v3.0.3), so the end-of-run summary surfaces it as
	// "auto-pinned — review for sanity". When *any* impostor finding for the
	// workflow lacks a suggestion (or is transitive) we don't auto-fix any
	// of them: the consumer needs the alert anyway, and a half-rewritten
	// workflow would obscure the unfixable cases.
	if rem.tryAutoFixImposters(&wr) {
		// All impostor findings handled — return so we don't re-process
		// stale CategoryImposterCommit entries from before the rewrite.
		return rem.applyPin(wr)
	}

	headerPrinted := false
	ensureHeader := func() {
		if !headerPrinted {
			rem.output.Header("%s", wr.Path)
			headerPrinted = true
		}
	}

	// In interactive mode, always show the header.
	if rem.prompter.IsInteractive() {
		ensureHeader()
	}

	first := true
	for _, finding := range wr.Findings {
		if finding.Category == CategoryValid || finding.Category == CategoryRunOnly || finding.Category == CategoryRefMoved {
			continue
		}

		// Suppress stale alerts on deps that were already auto-converted from
		// a SHA pin to a canonical tag earlier in this run. This is the
		// common case for actions like actions/github-script where a single
		// uses: line trips both sha_as_ref (which auto-pins to a tag) and
		// misleading_sha (which alerts because the ref was an annotated tag
		// SHA, not a commit). Once the ref is rewritten the alert no longer
		// describes the file on disk.
		if finding.Dependency != nil && rem.shaConvertedForNWO(finding.Dependency.NWO) {
			switch finding.Category {
			case CategoryMisleadingSHA, CategoryImposterCommit, CategoryLockfileForgery:
				continue
			}
		}

		// For non-interactive SHA_AS_REF, check if this dep was already printed.
		// If so, skip silently (no header, no blank line).
		if !rem.prompter.IsInteractive() && finding.Category == CategorySHAAsRef {
			if finding.Dependency != nil {
				if _, seen := rem.state.choices[finding.Dependency.Key()]; seen {
					rem.Skipped++
					continue
				}
			}
		}

		// Alerted-only categories (Imposter/Forgery/Misleading) were already
		// fully presented by presentCheckResults in non-interactive mode.
		// Just register the alert here without re-printing the per-workflow
		// header or finding details.
		if !rem.prompter.IsInteractive() {
			switch finding.Category {
			case CategoryImposterCommit, CategoryLockfileForgery, CategoryMisleadingSHA:
				rem.Alerted++
				rem.addAlertedDep(finding)
				continue
			}
		}

		ensureHeader()
		if !first {
			rem.output.Blank()
		}
		first = false

		switch finding.Category {
		case CategoryNotPinned:
			// Re-read workflow from disk — earlier SHA→tag conversions may have
			// changed refs since diagnosis time.
			if wf, err := lockfile.Load(wr.Path); err == nil {
				if freshRefs, _, _ := wf.ExtractActionRefs(); len(freshRefs) > 0 {
					wr.ActionRefs = freshRefs
				}
			}
			if err := rem.handleNotPinned(wr); err != nil {
				return err
			}
			return nil // NotPinned is workflow-level, one pass is enough.

		case CategorySHAAsRef:
			if err := rem.handleSHAAsRef(wr, finding); err != nil {
				return err
			}

		case CategoryStale:
			if err := rem.handleStale(wr, finding); err != nil {
				return err
			}

		case CategoryRefChanged:
			if err := rem.handleRefChanged(wr, finding); err != nil {
				return err
			}

		case CategoryImposterCommit:
			rem.output.Error("%s", finding.Detail)
			rem.output.Hint("This may indicate a fork-network injection attack. Do not auto-fix.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryLockfileForgery:
			rem.output.Error("LOCKFILE_FORGERY %s: %s", rem.depKey(finding), finding.Detail)
			rem.output.Hint("The pinned SHA was never in this ref's lineage — possible lockfile tampering.")
			rem.Alerted++
			rem.addAlertedDep(finding)

		case CategoryMisleadingSHA:
			rem.output.Error("MISLEADING_SHA %s: %s", rem.depKey(finding), finding.Detail)
			rem.output.Hint("This ref may be a deceptive branch or tag name masquerading as a commit hash.")
			rem.Alerted++
			rem.addAlertedDep(finding)
		}
	}

	if headerPrinted {
		rem.output.Blank()
	}
	return nil
}

func (rem *Remediator) handleNotPinned(wr WorkflowReport) error {
	rem.output.Warning("%d %s found but not pinned", len(wr.ActionRefs), ui.Pluralize(len(wr.ActionRefs), "action", "actions"))
	if docURL := DocURLFor(CategoryNotPinned); docURL != "" {
		rem.output.Detail("  see: %s", rem.output.DocLink(docURL))
	}

	if !rem.prompter.IsInteractive() {
		// Non-interactive: auto-pin all refs (ref→SHA is deterministic).
		rem.state.markRefsApproved(wr.ActionRefs)
		return rem.submitPin(wr)
	}

	// For internal repos, offer the default branch as an alternative ref.
	wr = rem.offerDefaultBranch(wr)

	// If all refs in this workflow were already approved in a prior workflow, auto-apply.
	if rem.state.allRefsApproved(wr.ActionRefs) {
		rem.output.Detail("  ↳ all actions already approved — auto-pinning")
		return rem.submitPin(wr)
	}

	// Resolve all refs to show the SHAs they'll pin to.
	rem.startWork(rem.workLabel(fmt.Sprintf("Resolving %s", wr.Path)))
	resolved, _, _ := rem.resolver.ResolveAllRecursive(wr.ActionRefs)
	rem.stopWork()
	shaByKey := make(map[string]string)
	for _, dep := range resolved {
		shaByKey[dep.Key()] = dep.SHA
	}

	// Review each action one at a time. Auto-apply prior choices and internal
	// actions silently; prompt for each external action.
	var approved []parserlock.ActionRef
	for _, ref := range wr.ActionRefs {
		key := ref.FullName() + "@" + ref.Ref // display key (preserves sub-action path)
		depKey := ref.NWO() + "@" + ref.Ref   // dep.Key() form (NWO@Ref, runner-flat)

		// Prior choice — auto-apply without prompting.
		if rem.state.approvedRefs[refKey(ref)] {
			sha, ok := shaByKey[depKey]
			if !ok || sha == "" {
				rem.output.Detail("  %s  (could not resolve)", key)
				rem.markUnresolved(key)
				continue
			}
			rem.output.Detail("  %s → %s  %s", key, sha[:12], rem.output.Dim("↩ prior choice"))
			approved = append(approved, ref)
			continue
		}

		// Internal (same-owner) action — auto-apply without prompting.
		if rem.isSameOwner(ref.Owner) {
			sha, ok := shaByKey[depKey]
			if !ok || sha == "" {
				rem.output.Detail("  %s  (could not resolve)", key)
				rem.markUnresolved(key)
				continue
			}
			label := ""
			if info, err := rem.tagLister.GetRepoInfo(ref.Owner, ref.Repo); err == nil {
				label = info.VisibilityLabel()
				if ref.Ref == info.DefaultBranch {
					label += " · default branch"
				}
				if age := FormatTagAge(info.PushedAt); age != "" {
					label += " · last push " + age
				}
			}
			rem.output.Detail("  %s → %s  %s", key, sha[:12], rem.output.Dim(label))
			approved = append(approved, ref)
			continue
		}

		// External action — auto-pin when there's a clear default, prompt otherwise.
		sha, ok := shaByKey[depKey]
		if !ok {
			rem.output.Detail("  %s  (could not resolve)", key)
			rem.markUnresolved(key)
			continue
		}

		// Already a full SHA — immutable and pinned by construction. Record it
		// without prompting; surface the matching release tag if we can find one.
		if parserlock.IsFullSha(ref.Ref) {
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
			shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
			if tag, err := rem.tagLister.BestPatchTagForSHA(ref.Owner, ref.Repo, sha); err == nil && tag != "" {
				tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, tag)
				rem.output.Detail("  %s → %s → %s  %s", key, tag, shaLabel, rem.output.Dim(rem.output.Hyperlink("release", tagURL)))
			} else {
				rem.output.Detail("  %s → %s  %s", key, shaLabel, rem.output.Dim("already pinned"))
			}
			rem.state.approvedRefs[refKey(ref)] = true
			approved = append(approved, ref)
			continue
		}

		displayTag := ref.Ref
		autoPin := false

		// Case 1: Already a full semver tag (v4.3.1) — good default, verify it's a real tag.
		if sv, svOK := lockfile.ParseSemver(ref.Ref); svOK && sv.IsFullSemver() {
			if rem.tagLister.LookupTag(ref.Owner, ref.Repo, ref.Ref) != nil {
				autoPin = true
			}
		}

		// Case 2: Mutable tag (v4, v4.2) — auto-pin if there's exactly one matching patch tag.
		if !autoPin && IsMutableVersionTag(ref.Ref) {
			if uniqueTag, err := rem.tagLister.UniquePatchTagForRef(ref.Owner, ref.Repo, sha, ref.Ref); err == nil && uniqueTag != "" {
				displayTag = uniqueTag
				autoPin = true
			}
		}

		if autoPin {
			tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, displayTag)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if ti := rem.tagLister.LookupTag(ref.Owner, ref.Repo, displayTag); ti != nil && ti.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			// Show verifiable SHA match: tag resolves to the same commit.
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
			shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
			if displayTag != ref.Ref {
				rem.output.Detail("  %s → %s → %s  %s", key, displayTag, shaLabel, tagLink)
			} else {
				rem.output.Detail("  %s → %s  %s", key, shaLabel, tagLink)
			}
			// Record both original and narrowed ref for cascade.
			rem.state.approvedRefs[refKey(ref)] = true
			if displayTag != ref.Ref {
				narrowedRef := ref
				narrowedRef.Ref = displayTag
				rem.state.approvedRefs[refKey(narrowedRef)] = true
			}
			approved = append(approved, ref)
			continue
		}

		// Fall-through case — typically a branch ref (e.g. `main`). Auto-pin
		// to the resolved SHA without prompting; if we can find a release tag
		// pointing at the same SHA, surface it as a narrowing hint.
		commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)
		shaLabel := rem.output.Hyperlink(sha[:12], commitURL)
		if tag, err := rem.tagLister.BestPatchTagForSHA(ref.Owner, ref.Repo, sha); err == nil && tag != "" {
			tagURL := fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", ref.Owner, ref.Repo, tag)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if ti := rem.tagLister.LookupTag(ref.Owner, ref.Repo, tag); ti != nil && ti.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			rem.output.Detail("  %s → %s → %s  %s", key, tag, shaLabel, tagLink)
		} else {
			rem.output.Detail("  %s → %s", key, shaLabel)
		}
		rem.state.approvedRefs[refKey(ref)] = true
		approved = append(approved, ref)
	}

	if len(approved) == 0 {
		rem.Skipped++
		return nil
	}

	wr.ActionRefs = approved
	rem.state.markRefsApproved(approved)
	return rem.submitPin(wr)
}

// offerDefaultBranch checks each action ref for same-owner repos (internal
// actions) and switches bare SHA refs to the default branch. Named refs
// (tags, branches, versions) are preserved as-is.
// Returns a (possibly modified) copy of the WorkflowReport with updated refs.
func (rem *Remediator) offerDefaultBranch(wr WorkflowReport) WorkflowReport {
	updated := make([]parserlock.ActionRef, 0, len(wr.ActionRefs))
	for _, ref := range wr.ActionRefs {
		if !rem.isSameOwner(ref.Owner) {
			updated = append(updated, ref)
			continue
		}

		info, err := rem.tagLister.GetRepoInfo(ref.Owner, ref.Repo)
		if err != nil {
			updated = append(updated, ref)
			continue
		}

		// Already targeting the default branch — nothing to offer.
		if ref.Ref == info.DefaultBranch {
			updated = append(updated, ref)
			continue
		}

		// Bare SHA → swap to default branch. Named refs stay as-is.
		if parserlock.IsFullSha(ref.Ref) {
			rem.output.Detail("  %s: using %s (default branch) instead of %s",
				ref.FullName(), info.DefaultBranch, ref.Ref)
			ref.Ref = info.DefaultBranch
			updated = append(updated, ref)
			continue
		}

		// Named ref (tag, branch, version) — preserve what the user wrote.
		updated = append(updated, ref)
	}

	wr.ActionRefs = updated
	return wr
}

func (rem *Remediator) handleSHAAsRef(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency

	owner, repo := dep.OwnerRepo()

	// Make the SHA a clickable link to the commit on GitHub.
	commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
	depLabel := dep.NWO + "@" + rem.output.Hyperlink(dep.SHA[:12], commitURL)

	if !rem.prompter.IsInteractive() && owner == "" {
		rem.output.Warning("%s: %s", depLabel, finding.Detail)
		rem.skipDep(dep)
		return nil
	}

	rem.output.Warning("%s: %s", depLabel, finding.Detail)

	if owner == "" {
		rem.Skipped++
		return nil
	}

	// Session memory: reuse prior internal ref choice for same-owner repos (any SHA).
	if rem.isSameOwner(owner) {
		nwo := owner + "/" + repo
		if priorRef, ok := rem.state.internalRefChoices[nwo]; ok {
			rem.output.Detail("  ↳ reusing prior choice for %s: %s", nwo, priorRef)
			return rem.applySHAToTag(wr, dep, owner, repo, priorRef)
		}
	}

	// Session memory: if we already chose a tag for this exact dep, auto-apply.
	if priorTag, ok := rem.state.recallChoice(dep); ok {
		rem.output.Detail("  ↳ reusing prior choice: %s", priorTag)
		return rem.applySHAToTag(wr, dep, owner, repo, priorTag)
	}

	// Try to find which tags this SHA already belongs to.
	suggestions, err := rem.tagLister.SuggestTagsForSHA(owner, repo, dep.SHA)
	if err != nil {
		rem.output.Warning("could not fetch tags: %s", err)
		rem.Skipped++
		return nil
	}

	// Smart default for internal (same-owner) repos: if the SHA already
	// belongs to a tag, auto-pick it — no need to prompt. If no tag match,
	// fall back to the default branch.
	if rem.isSameOwner(owner) {
		// Prefer a tag that directly points at this SHA.
		for _, s := range suggestions {
			if s.Preferred {
				tag := s.Tag
				tagURL := TagURL(owner, repo, tag.Name)
				tagLink := rem.output.Dim(rem.output.Hyperlink("tag", tagURL))
				commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
				shaLabel := rem.output.Hyperlink(dep.SHA[:12], commitURL)
				rem.output.Detail("  ↳ already installed to %s (%s)  %s", tag.Name, shaLabel, tagLink)
				nwo := owner + "/" + repo
				rem.state.internalRefChoices[nwo] = tag.Name
				rem.state.recordChoice(dep, tag.Name)
				return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
			}
		}
		// No tag match — use default branch.
		if info, err := rem.tagLister.GetRepoInfo(owner, repo); err == nil {
			rem.output.Detail("  ↳ using %s (default branch) for %s/%s", info.DefaultBranch, owner, repo)
			nwo := owner + "/" + repo
			rem.state.internalRefChoices[nwo] = info.DefaultBranch
			return rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch)
		}
	}

	// Smart default: for external repos, if exactly one full-semver tag points
	// at this SHA, auto-pick it.
	if len(suggestions) > 0 && !rem.isSameOwner(owner) {
		var fullSemverTags []TagSuggestion
		for _, s := range suggestions {
			sv, ok := lockfile.ParseSemver(s.Tag.Name)
			if ok && sv.IsFullSemver() {
				fullSemverTags = append(fullSemverTags, s)
			}
		}
		if len(fullSemverTags) == 1 {
			tag := fullSemverTags[0].Tag
			tagURL := TagURL(owner, repo, tag.Name)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if tag.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			// Show verifiable SHA match: tag points at the same commit.
			commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
			shaLabel := rem.output.Hyperlink(dep.SHA[:12], commitURL)
			rem.output.Detail("  ↳ auto-pinning to %s (%s)  %s", tag.Name, shaLabel, tagLink)
			rem.state.recordChoice(dep, tag.Name)
			return rem.applySHAToTag(wr, dep, owner, repo, tag.Name)
		}
	}

	// If we found tags for this SHA, present smart suggestions.
	if len(suggestions) > 0 {
		if !rem.prompter.IsInteractive() {
			// Multiple tags match — can't auto-pick, need human choice.
			rem.skipDep(dep)
			return nil
		}
		return rem.handleSHAWithSuggestions(wr, finding, suggestions, owner, repo)
	}

	// No tag matches this SHA — this is an unreleased commit. Be loud.
	noTagCommitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, dep.SHA)
	shaLink := rem.output.Hyperlink(dep.SHA[:12], noTagCommitURL)
	releasesURL := fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)
	releasesLink := rem.output.Hyperlink("releases", releasesURL)
	rem.output.Error("  commit %s does not belong to any release — you are running unreleased code", shaLink)
	rem.output.Detail("  ↳ pin to a tagged release instead: %s", releasesLink)
	if !rem.prompter.IsInteractive() {
		rem.skipDep(dep)
		return nil
	}
	return rem.handleSHATagPicker(wr, finding, owner, repo)
}

func (rem *Remediator) handleSHAWithSuggestions(wr WorkflowReport, finding Finding, suggestions []TagSuggestion, owner, repo string) error {
	dep := finding.Dependency

	// Build picker — full semver first (recommended), then major tags.
	reordered := reorderSuggestions(suggestions)
	if !rem.isSameOwner(owner) {
		var filtered []TagSuggestion
		for _, s := range reordered {
			if !s.Tag.IsMajor {
				filtered = append(filtered, s)
			}
		}
		reordered = filtered
	}

	options := make([]string, 0, len(reordered)+3)
	for i, s := range reordered {
		recommend := i == 0 && !s.Tag.IsMajor && !rem.isSameOwner(owner)
		options = append(options, rem.tagLabel(owner, repo, pickerTag{
			Name:        s.Tag.Name,
			IsInstalled: s.Preferred,
			IsImmutable: s.Tag.IsImmutable,
			IsRelease:   s.Tag.IsRelease,
			IsMajor:     s.Tag.IsMajor,
		}, recommend))
	}

	var defaultBranchIdx int
	options, defaultBranchIdx = rem.defaultBranchOption(options, owner, repo)
	tagCount := len(reordered)

	// Exactly one suggested tag and no default-branch alternative: no real
	// choice to make, so auto-pin it instead of prompting.
	if tagCount == 1 && defaultBranchIdx == -1 {
		selectedTag := reordered[0].Tag
		tagURL := TagURL(owner, repo, selectedTag.Name)
		tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
		if selectedTag.IsImmutable {
			tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
		}
		rem.output.Detail("  ↳ only one tag available — pinning to %s  %s", selectedTag.Name, tagLink)
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}

	result, err := rem.runPicker(
		rem.pinPromptTitle(dep.NWO, owner, repo),
		options, tagCount, defaultBranchIdx,
		pickerSentinels{ShowAll: true},
	)
	if err != nil {
		return err
	}

	switch result.Action {
	case pickerSkip:
		rem.Skipped++
		return nil
	case pickerShowAll:
		return rem.handleSHATagPicker(wr, finding, owner, repo)
	case pickerDefaultBranch:
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		rem.state.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := reordered[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}
}

func (rem *Remediator) handleSHATagPicker(wr WorkflowReport, finding Finding, owner, repo string) error {
	dep := finding.Dependency

	curated, err := rem.tagLister.CuratePickerTags(owner, repo, dep.SHA)
	if err != nil {
		rem.output.Warning("could not fetch tags: %s", err)
		rem.Skipped++
		return nil
	}

	if len(curated) == 0 {
		rem.output.Warning("no tags found for %s/%s", owner, repo)
		rem.Skipped++
		return nil
	}

	options := make([]string, 0, len(curated)+3)
	for _, pt := range curated {
		options = append(options, rem.tagLabel(owner, repo, pickerTag{
			Name:        pt.Tag.Name,
			IsInstalled: pt.Installed,
			IsImmutable: pt.Tag.IsImmutable,
			IsRelease:   pt.Tag.IsRelease,
			IsMajor:     pt.Tag.IsMajor,
		}, false))
	}

	var defaultBranchIdx int
	options, defaultBranchIdx = rem.defaultBranchOption(options, owner, repo)
	tagCount := len(curated)

	// Exactly one tag and no default-branch alternative: there's no real
	// choice for the user to make (the only other options are "open releases"
	// and "skip"). Auto-pin it and narrate, rather than forcing a prompt.
	if tagCount == 1 && defaultBranchIdx == -1 {
		selectedTag := curated[0].Tag
		tagURL := TagURL(owner, repo, selectedTag.Name)
		tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
		if selectedTag.IsImmutable {
			tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
		}
		rem.output.Detail("  ↳ only one tag available — pinning to %s  %s", selectedTag.Name, tagLink)
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}

	// External repo (no default-branch alternative) with a full-semver
	// release at the top of the curated list: pick it. The user's SHA
	// doesn't map to any tag; the latest stable release is the safe default
	// and asking them to pick from a list of N versions is pure friction.
	// `gh actions-pin upgrade` lets them shift to a different version later.
	if defaultBranchIdx == -1 && len(curated) > 0 {
		top := curated[0].Tag
		if sv, ok := lockfile.ParseSemver(top.Name); ok && sv.IsFullSemver() && top.IsRelease {
			tagURL := TagURL(owner, repo, top.Name)
			tagLink := rem.output.Dim(rem.output.Hyperlink("release", tagURL))
			if top.IsImmutable {
				tagLink = rem.output.Dim("🔒 " + rem.output.Hyperlink("immutable release", tagURL))
			}
			rem.output.Detail("  ↳ pinning to latest release %s  %s", top.Name, tagLink)
			if err := rem.applySHAToTag(wr, dep, owner, repo, top.Name); err != nil {
				return err
			}
			rem.offerApplyAll(dep, top.Name)
			return nil
		}
	}

	releasesURL := fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo)
	result, err := rem.runPicker(
		rem.pinPromptTitle(owner+"/"+repo, owner, repo),
		options, tagCount, defaultBranchIdx,
		pickerSentinels{OpenReleases: releasesURL},
	)
	if err != nil {
		return err
	}

	switch result.Action {
	case pickerSkip:
		rem.Skipped++
		return nil
	case pickerOpenReleases:
		rem.output.Info("Opening releases page...")
		openBrowser(releasesURL)
		rem.Skipped++
		return nil
	case pickerDefaultBranch:
		info, _ := rem.tagLister.GetRepoInfo(owner, repo)
		rem.state.internalRefChoices[owner+"/"+repo] = info.DefaultBranch
		if err := rem.applySHAToTag(wr, dep, owner, repo, info.DefaultBranch); err != nil {
			return err
		}
		rem.offerApplyAll(dep, info.DefaultBranch)
		return nil
	default:
		selectedTag := curated[result.TagIndex].Tag
		if rem.isSameOwner(owner) {
			rem.state.internalRefChoices[owner+"/"+repo] = selectedTag.Name
		}
		if err := rem.applySHAToTag(wr, dep, owner, repo, selectedTag.Name); err != nil {
			return err
		}
		rem.offerApplyAll(dep, selectedTag.Name)
		return nil
	}
}

func (rem *Remediator) handleRefChanged(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency
	newRef := ""
	if finding.ActionRef != nil {
		newRef = finding.ActionRef.Ref
	}
	rem.output.Warning("%s: %s", dep.Key(), finding.Detail)

	// If the workflow's new ref no longer exists upstream (e.g. someone
	// typo'd or pointed at a deleted tag), don't silently re-resolve to a
	// missing ref. Divert into the full tag picker so the user can choose
	// a real tag, open the releases page, or skip.
	owner, repo := dep.OwnerRepo()
	if rem.prompter.IsInteractive() && owner != "" && newRef != "" && !parserlock.IsFullSha(newRef) {
		if rem.tagLister.LookupTag(owner, repo, newRef) == nil {
			rem.output.Detail("  ref %q no longer exists upstream — pick a valid tag instead", newRef)
			return rem.handleSHATagPicker(wr, finding, owner, repo)
		}
	}

	// The workflow is the source of truth: just pin what it asks for,
	// without prompting the user to confirm a "more specific" version.
	return rem.applyReResolve(wr, dep)
}

func (rem *Remediator) handleStale(wr WorkflowReport, finding Finding) error {
	dep := finding.Dependency
	rem.output.Detail("%s: no longer in workflow — cleaning up", dep.Key())

	// Auto-clean: re-resolve rewrites the lockfile without orphaned deps.
	return rem.applyReResolve(wr, dep)
}

// openBrowser attempts to open a URL in the user's browser. The URL is
// validated to ensure it's a well-formed https:// link before being
// passed to the platform launcher; this matters because the URL is
// constructed from action owner/repo strings that originate in workflow
// YAML (i.e. user input). We also pin the launcher to a hardcoded literal
// so the command is never shell-interpreted.
func openBrowser(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return
	}
	cmd := "open"
	if _, err := os.Stat("/usr/bin/xdg-open"); err == nil {
		cmd = "xdg-open"
	}
	// #nosec G204 — cmd is a hardcoded literal (open / xdg-open); the URL
	// is parsed and required to be https with a non-empty host above.
	proc := exec.Command(cmd, u.String())
	_ = proc.Start()
}
