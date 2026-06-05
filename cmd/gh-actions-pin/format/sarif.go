package format

// SARIF 2.1.0 emitter for `gh actions-pin check --format=sarif`.
//
// Wire-format contract (spec-locked, do not re-litigate):
//   - SARIF schema: 2.1.0
//     (https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html)
//   - Rule IDs match zizmor where the audit definition overlaps
//     (`impostor-commit`, `unpinned-uses`) so consumers can correlate.
//     Non-overlapping rules use our kebab-case category IDs.
//   - Severity → SARIF level: error→error, warning→warning, info→note,
//     ok→note. An unrecognized severity is a hard error here, not a
//     silent downgrade.
//   - Confidence rides in `properties.confidence`. SARIF `level` maps
//     from Severity only.
//   - partialFingerprints uses `primaryLocationLineHash`: SHA-256 of
//     the trimmed `uses:` line text. Stable across runs so
//     code-scanning can dedupe.
//   - Positions are 1-based per the SARIF spec.
//   - SARIF carries the diff against the lockfile; the lockfile
//     itself is never embedded.
//
// Caveats worth knowing when consuming the emitted file:
//   - GitHub code-scanning caps uploads at 25 MB / 5000 results per run.
//   - Private repos without GitHub Advanced Security will 403 on
//     `gh code-scanning upload-sarif`, but we still emit the file.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/pkg/findings"
)

// sarifVersion is the SARIF spec version we conform to.
const sarifVersion = "2.1.0"

// sarifSchemaURI is the JSON-schema URL for the SARIF 2.1.0 schema.
const sarifSchemaURI = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"

// toolName is the tool name advertised in run.tool.driver.name. Stable
// across versions so code-scanning groups runs correctly.
const toolName = "gh-actions-pin"

// toolInfoURI points GitHub code-scanning at the project home.
const toolInfoURI = "https://github.com/github/gh-actions-pin"

// sarifLog is the top-level SARIF document.
type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
	// ColumnKind tells consumers our column numbers are unicode code
	// points (not utf-16 code units). We don't currently emit columns,
	// but stating the convention is harmless and forward-compatible.
	ColumnKind string `json:"columnKind,omitempty"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name,omitempty"`
	ShortDescription sarifMessage           `json:"shortDescription"`
	FullDescription  sarifMessage           `json:"fullDescription,omitempty"`
	HelpURI          string                 `json:"helpUri,omitempty"`
	Properties       map[string]interface{} `json:"properties,omitempty"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID              string                 `json:"ruleId"`
	Level               string                 `json:"level"`
	Message             sarifMessage           `json:"message"`
	Locations           []sarifLocation        `json:"locations"`
	PartialFingerprints map[string]string      `json:"partialFingerprints"`
	Properties          map[string]interface{} `json:"properties,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine   int           `json:"startLine"`
	StartColumn int           `json:"startColumn,omitempty"`
	EndColumn   int           `json:"endColumn,omitempty"`
	Snippet     *sarifMessage `json:"snippet,omitempty"`
}

// categoryRuleID maps an internal doctor.Category to the SARIF ruleId we
// emit. Overlaps with zizmor's audit IDs use zizmor's name so consumers
// can correlate; everything else uses our kebab-case category ID
// verbatim. CategoryValid and CategoryRunOnly never produce SARIF
// results.
func categoryRuleID(c doctor.Category) string {
	switch c {
	case findings.NotPinned:
		return "unpinned-uses"
	case findings.ImpostorCommit:
		return "impostor-commit"
	default:
		return string(c)
	}
}

// ruleCatalog defines every rule we may emit. Keeping it static (rather
// than synthesizing from observed findings) means an empty-findings run
// still advertises the tool's capabilities — useful for consumers
// inspecting tool.driver.rules to know what to expect.
var ruleCatalog = []sarifRule{
	{
		ID:               "unpinned-uses",
		Name:             "UnpinnedUses",
		ShortDescription: sarifMessage{Text: "Action dependency is not pinned in actions.lock"},
		FullDescription:  sarifMessage{Text: "The workflow references an action but no matching dependency is recorded in .github/workflows/actions.lock. Run `gh actions-pin` to add the entry."},
		HelpURI:          docURLOr(findings.NotPinned),
		Properties:       map[string]interface{}{"category": string(findings.NotPinned)},
	},
	{
		ID:               "sha-as-ref",
		Name:             "ShaAsRef",
		ShortDescription: sarifMessage{Text: "Dependency is pinned to a bare SHA with no tag ref"},
		FullDescription:  sarifMessage{Text: "The lockfile entry uses a bare commit SHA as the `uses:` ref. A human-readable tag alongside the SHA helps reviewers verify intent."},
		HelpURI:          docURLOr(findings.ShaAsRef),
		Properties:       map[string]interface{}{"category": string(findings.ShaAsRef)},
	},
	{
		ID:               "stale",
		Name:             "Stale",
		ShortDescription: sarifMessage{Text: "Lockfile entry references an action no longer present in any workflow"},
		FullDescription:  sarifMessage{Text: "The lockfile retains a dependency that no workflow `uses:` anymore. Stale entries pollute audits and may mask removed actions."},
		HelpURI:          docURLOr(findings.Stale),
		Properties:       map[string]interface{}{"category": string(findings.Stale)},
	},
	{
		ID:               "ref-changed",
		Name:             "RefChanged",
		ShortDescription: sarifMessage{Text: "Workflow `uses:` ref was edited; lockfile no longer matches"},
		FullDescription:  sarifMessage{Text: "Someone changed the `uses:` ref in the workflow (e.g. v4.1.0 → v4) without updating the lockfile entry. Re-pin so the recorded SHA reflects the requested ref."},
		HelpURI:          docURLOr(findings.RefChanged),
		Properties:       map[string]interface{}{"category": string(findings.RefChanged)},
	},
	{
		ID:               "impostor-commit",
		Name:             "ImpostorCommit",
		ShortDescription: sarifMessage{Text: "Pinned SHA is not reachable from any branch in the action repository"},
		FullDescription:  sarifMessage{Text: "The locked commit cannot be reached from any branch or release tag in the upstream repository — most commonly a fork-network commit injected via PR. Re-pin to a sane release."},
		HelpURI:          docURLOr(findings.ImpostorCommit),
		Properties:       map[string]interface{}{"category": string(findings.ImpostorCommit)},
	},
	{
		ID:               "misleading-sha",
		Name:             "MisleadingSha",
		ShortDescription: sarifMessage{Text: "Ref looks like a SHA but resolves to a different commit"},
		FullDescription:  sarifMessage{Text: "The `uses:` ref is shaped like a commit SHA but the upstream API resolves it to a different commit. The ref is likely a tag or branch that shadows a SHA prefix."},
		HelpURI:          docURLOr(findings.MisleadingSHA),
		Properties:       map[string]interface{}{"category": string(findings.MisleadingSHA)},
	},
	{
		ID:               "lockfile-forgery",
		Name:             "LockfileForgery",
		ShortDescription: sarifMessage{Text: "Lockfile SHA is not an ancestor of the current ref"},
		FullDescription:  sarifMessage{Text: "The pinned commit is not in the ancestry of the upstream ref the workflow asks for. The lockfile entry was likely injected or tampered with."},
		HelpURI:          docURLOr(findings.LockfileForgery),
		Properties:       map[string]interface{}{"category": string(findings.LockfileForgery)},
	},
	{
		ID:               "ref-moved",
		Name:             "RefMoved",
		ShortDescription: sarifMessage{Text: "Upstream ref now resolves to a different SHA than the lockfile records"},
		FullDescription:  sarifMessage{Text: "Expected for mutable tags like `v4`. Re-pin to record the new SHA after verifying the upstream change is intentional."},
		HelpURI:          docURLOr(findings.RefMoved),
		Properties:       map[string]interface{}{"category": string(findings.RefMoved)},
	},
	{
		ID:               "onboarding-required",
		Name:             "OnboardingRequired",
		ShortDescription: sarifMessage{Text: "Workflow is not tracked in the lockfile; `upgrade --no-onboard` refuses to add it"},
		FullDescription: sarifMessage{Text: "An `upgrade --no-onboard` run targeted a workflow that has no entry in `lockfile.workflows{}`. " +
			"Strict mode (used by Dependabot) refuses to silently onboard new workflows during a dependency-update run. " +
			"Run `gh actions-pin` without `--no-onboard` (or without `--workflow`) on this repository to onboard the workflow first."},
		HelpURI:    docURLOr(doctor.CategoryOnboardingRequired),
		Properties: map[string]interface{}{"category": string(doctor.CategoryOnboardingRequired)},
	},
	{
		ID:               "ancestry-unknown",
		Name:             "AncestryUnknown",
		ShortDescription: sarifMessage{Text: "Compare API could not classify the pinned SHA against the upstream ref"},
		FullDescription: sarifMessage{Text: "The pinned SHA differs from the live ref but the Compare API was rate-limited or returned an error, " +
			"so the move could not be classified as benign (ref-moved) or tampered (lockfile-forgery). " +
			"Diagnostic only — retry when the API is available to resolve the ambiguity."},
		HelpURI:    docURLOr(doctor.CategoryAncestryUnknown),
		Properties: map[string]interface{}{"category": string(doctor.CategoryAncestryUnknown)},
	},
	{
		ID:               "reachability-unknown",
		Name:             "ReachabilityUnknown",
		ShortDescription: sarifMessage{Text: "Could not verify the pinned SHA is reachable upstream"},
		FullDescription: sarifMessage{Text: "branch_commits did not return an authoritative reachability answer (resolver failure, GraphQL rate limit, network error). " +
			"Diagnostic only — the dependency was neither confirmed verified nor flagged as an impostor."},
		HelpURI:    docURLOr(doctor.CategoryReachabilityUnknown),
		Properties: map[string]interface{}{"category": string(doctor.CategoryReachabilityUnknown)},
	},
}

func docURLOr(c doctor.Category) string {
	if u := doctor.DocURLFor(c); u != "" {
		return u
	}
	return toolInfoURI
}

// severityToLevel translates our Severity to SARIF level. Unknown
// severities return an error rather than defaulting — silently
// downgrading an error-class finding to "note" would hide real
// problems. The mapping must be widened explicitly when a new Severity
// lands.
func severityToLevel(s doctor.Severity) (string, error) {
	switch s {
	case findings.SeverityError:
		return "error", nil
	case findings.SeverityWarning:
		return "warning", nil
	case findings.SeverityInfo, findings.SeverityOK:
		return "note", nil
	default:
		return "", fmt.Errorf("sarif: unmapped severity %q", string(s))
	}
}

// fileLineLookup loads workflow files on demand and locates the first
// `uses: <raw>` occurrence per (file, raw value). Same raw inside the
// same file appears at most once in the finding stream (doctor dedupes
// by NWO@ref), so a single first-match lookup is enough. Cached per
// path because a single report typically produces several findings
// against the same workflow.
type fileLineLookup struct {
	cache map[string][]string // path → 1-based-indexed lines (index 0 unused)
}

func newFileLineLookup() *fileLineLookup {
	return &fileLineLookup{cache: map[string][]string{}}
}

// load reads the file and splits into lines. Errors are swallowed —
// SARIF emission must not fail because a workflow disappeared between
// scan and emit. A nil slice means "we couldn't read it, fall back to
// line 1 with no snippet".
func (l *fileLineLookup) load(path string) []string {
	if lines, ok := l.cache[path]; ok {
		return lines
	}
	data, err := os.ReadFile(path)
	if err != nil {
		l.cache[path] = nil
		return nil
	}
	raw := strings.Split(string(data), "\n")
	// Insert a sentinel at index 0 so callers can use 1-based indexing
	// without translating at every call site.
	lines := make([]string, len(raw)+1)
	copy(lines[1:], raw)
	l.cache[path] = lines
	return lines
}

// locate returns the 1-based line number containing the first
// occurrence of `uses: <raw>` in path, plus the trimmed line text used
// for the partial fingerprint. Returns (1, "") when the file cannot be
// read or no match is found — that yields a stable but conservative
// SARIF location rather than skipping the finding.
func (l *fileLineLookup) locate(path, raw string) (int, string) {
	lines := l.load(path)
	if len(lines) <= 1 {
		return 1, ""
	}
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		// Cheap pre-filter; full match below tolerates surrounding
		// whitespace and optional single/double quotes around raw.
		if !strings.Contains(line, "uses:") || !strings.Contains(line, raw) {
			continue
		}
		if matchesUsesLine(line, raw) {
			return i, strings.TrimSpace(line)
		}
	}
	return 1, ""
}

// matchesUsesLine reports whether line is shaped like `uses: <raw>`
// where raw may be wrapped in matching single or double quotes. We
// don't try to handle YAML flow-mapping `{uses: x}`; workflows in the
// wild always use block style. Commented-out lines (`# uses: ...`)
// are rejected because the `#` precedes the `uses:` token.
func matchesUsesLine(line, raw string) bool {
	idx := strings.Index(line, "uses:")
	if idx < 0 {
		return false
	}
	// Reject if the `uses:` is inside a YAML comment.
	if strings.Contains(line[:idx], "#") {
		return false
	}
	after := strings.TrimSpace(line[idx+len("uses:"):])
	after = strings.TrimRight(after, " \t")
	// Strip an inline trailing `# comment` if present.
	if h := strings.Index(after, " #"); h >= 0 {
		after = strings.TrimSpace(after[:h])
	}
	if len(after) >= 2 {
		first, last := after[0], after[len(after)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			after = after[1 : len(after)-1]
		}
	}
	return after == raw
}

// lineHash returns the SHA-256 hex of the trimmed line, used as the
// partial fingerprint per the primaryLocationLineHash strategy. Empty
// line text yields the empty string — we omit the fingerprint in that
// case rather than emit a meaningless constant hash.
func lineHash(line string) string {
	if line == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// findingURI returns the workflow path used in SARIF
// artifactLocation.uri. Forward slashes per SARIF convention; the
// caller already supplies repo-relative paths so we don't normalize.
func findingURI(path string) string {
	return filepathSlash(path)
}

// filepathSlash is filepath.ToSlash without importing path/filepath —
// the file is text-shaped already and we don't want to depend on the
// OS path separator at this boundary.
func filepathSlash(s string) string {
	return strings.ReplaceAll(s, "\\", "/")
}

// usesLineRaw returns the raw `uses:` value to locate in the workflow
// file. Prefers Finding.ActionRef.Raw because that's the exact string
// the user wrote; falls back to the Dependency identity for findings
// that don't carry an ActionRef (e.g. stale lockfile entries).
func usesLineRaw(f doctor.Finding) string {
	if f.ActionRef != nil && f.ActionRef.Raw != "" {
		return f.ActionRef.Raw
	}
	if f.Dependency != nil {
		// Best-effort: NWO@ref is what's most likely on a `uses:`
		// line for a stale-entry finding. Will miss if the workflow
		// uses a sub-path action, but stale findings rarely have
		// matching workflow lines anyway.
		nwo := f.Dependency.NWO
		if f.Dependency.Ref != "" {
			return nwo + "@" + f.Dependency.Ref
		}
		return nwo
	}
	return ""
}

// WriteSARIF emits a SARIF 2.1.0 document covering every actionable
// finding in report. Skips CategoryRunOnly and CategoryValid+OK to
// match the JSON formatter's actionable-only contract. cliVersion is
// recorded as tool.driver.version so consumers can correlate alerts
// with a specific release of the CLI.
func WriteSARIF(w io.Writer, report *doctor.Report, cliVersion string) error {
	lookup := newFileLineLookup()
	var results []sarifResult

	emit := func(f doctor.Finding) error {
		// Drop non-findings — same filter the JSON path uses so the two
		// formats agree on what counts as actionable.
		if f.Category == findings.RunOnly {
			return nil
		}
		if f.Category == findings.Valid && f.Severity == findings.SeverityOK {
			return nil
		}

		level, err := severityToLevel(f.Severity)
		if err != nil {
			return err
		}

		ruleID := categoryRuleID(f.Category)
		raw := usesLineRaw(f)
		line, trimmed := 1, ""
		if f.WorkflowPath != "" && raw != "" {
			line, trimmed = lookup.locate(f.WorkflowPath, raw)
		}

		region := &sarifRegion{StartLine: line}
		if trimmed != "" {
			region.Snippet = &sarifMessage{Text: trimmed}
		}

		props := map[string]interface{}{}
		if f.Confidence != "" {
			props["confidence"] = string(f.Confidence)
		}
		props["category"] = string(f.Category)
		if f.Remediation != "" {
			props["remediation"] = f.Remediation
		}
		if f.ObservedSHA != "" {
			props["observed_sha"] = f.ObservedSHA
		}
		if f.SaneSuggestionTag != "" {
			props["sane_suggestion_tag"] = f.SaneSuggestionTag
		}
		if f.SaneSuggestionSHA != "" {
			props["sane_suggestion_sha"] = f.SaneSuggestionSHA
		}

		fingerprints := map[string]string{}
		if h := lineHash(trimmed); h != "" {
			fingerprints["primaryLocationLineHash"] = h
		}

		res := sarifResult{
			RuleID:              ruleID,
			Level:               level,
			Message:             sarifMessage{Text: messageText(f)},
			Locations:           []sarifLocation{{PhysicalLocation: sarifPhysicalLocation{ArtifactLocation: sarifArtifactLocation{URI: findingURI(f.WorkflowPath)}, Region: region}}},
			PartialFingerprints: fingerprints,
			Properties:          props,
		}
		results = append(results, res)
		return nil
	}

	for _, f := range report.RepoFindings {
		if err := emit(f); err != nil {
			return err
		}
	}
	// Iterate workflows in their report order so SARIF output is stable
	// for diff-friendly snapshot tests.
	for _, wr := range report.Workflows {
		for _, f := range wr.Findings {
			if err := emit(f); err != nil {
				return err
			}
		}
	}

	// Sort results by (uri, startLine, ruleId) for deterministic output
	// independent of map iteration order in upstream callers.
	sort.SliceStable(results, func(i, j int) bool {
		li := results[i].Locations[0].PhysicalLocation
		lj := results[j].Locations[0].PhysicalLocation
		if li.ArtifactLocation.URI != lj.ArtifactLocation.URI {
			return li.ArtifactLocation.URI < lj.ArtifactLocation.URI
		}
		if li.Region.StartLine != lj.Region.StartLine {
			return li.Region.StartLine < lj.Region.StartLine
		}
		return results[i].RuleID < results[j].RuleID
	})

	if results == nil {
		results = []sarifResult{}
	}

	doc := sarifLog{
		Schema:  sarifSchemaURI,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           toolName,
				Version:        cliVersion,
				InformationURI: toolInfoURI,
				Rules:          ruleCatalog,
			}},
			Results:    results,
			ColumnKind: "unicodeCodePoints",
		}},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}

// messageText builds the human-readable result message. Prefers the
// finding's Detail; falls back to the rule's short description so
// SARIF viewers always have something to display.
func messageText(f doctor.Finding) string {
	if f.Detail != "" {
		return f.Detail
	}
	for _, r := range ruleCatalog {
		if r.ID == categoryRuleID(f.Category) {
			return r.ShortDescription.Text
		}
	}
	return string(f.Category)
}
