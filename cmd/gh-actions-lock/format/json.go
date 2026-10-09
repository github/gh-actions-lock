// Package format renders check reports for the `check` command.
package format

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// skipFindingInJSON reports whether a finding should be omitted from JSON
// output. Run-only and valid-OK workflows are noise; local-action
// warnings are informational skips (errors are kept because they mean
// an already-tracked workflow became ineligible).
func skipFindingInJSON(f checks.Finding) bool {
	if f.Category == checks.RunOnly {
		return true
	}
	if f.Category == checks.Valid && f.Severity == checks.SeverityOK {
		return true
	}
	if f.Category == checks.LocalAction && f.Severity != checks.SeverityError {
		return true
	}
	if f.Category == checks.SelfRepositoryAction {
		return true
	}
	return false
}

// validJSONField reports whether name is a recognized --json output field.
func validJSONField(name string) bool {
	switch name {
	case "valid", "findings", "workflows", "dependencies", "pins":
		return true
	default:
		return false
	}
}

// ValidateJSONFields checks that every comma-separated entry in fieldsCSV is a
// recognized --json output field. An empty selection is valid (no JSON output
// requested). Wire this into a command's PreRunE so a bad --json list is
// rejected before any workflow or lockfile mutation runs, instead of failing
// late in WriteJSON after side effects.
func ValidateJSONFields(fieldsCSV string) error {
	if fieldsCSV == "" {
		return nil
	}
	for _, field := range strings.Split(fieldsCSV, ",") {
		field = strings.TrimSpace(field)
		if !validJSONField(field) {
			return fmt.Errorf("unknown JSON field %q (expected valid, findings, workflows, dependencies, pins)", field)
		}
	}
	return nil
}

// Finding is the JSON-safe view of a checks.Finding.
type Finding struct {
	Workflow    string `json:"workflow"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Confidence  string `json:"confidence,omitempty"`
	Dependency  string `json:"dependency,omitempty"`
	RequiredBy  string `json:"required_by,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
	DocURL      string `json:"doc_url,omitempty"`
}

// Dependency is the JSON-safe view of a resolved dependency, deduplicated
// across workflows in the JSON output.
type Dependency struct {
	Hostname   string   `json:"hostname,omitempty"`
	NWO        string   `json:"nwo"`
	Ref        string   `json:"ref"`
	SHA        string   `json:"sha"`
	HashAlgo   string   `json:"hash_algo,omitempty"`
	Direct     bool     `json:"direct"`
	RequiredBy []string `json:"required_by,omitempty"`
}

// Workflow is the JSON-safe view of a single workflow's findings and
// dependencies.
type Workflow struct {
	Path         string       `json:"path"`
	Valid        bool         `json:"valid"`
	Findings     []Finding    `json:"findings"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Pin is the post-fix outcome for one action from the pin plan. The other
// fields describe the pre-fix diagnosis.
type Pin struct {
	Hostname     string `json:"hostname,omitempty"`
	NWO          string `json:"nwo"`
	Ref          string `json:"ref"`
	SHA          string `json:"sha,omitempty"`
	Outcome      string `json:"outcome"`
	NarrowedFrom string `json:"narrowed_from,omitempty"`
	RenamedFrom  string `json:"renamed_from,omitempty"`
	ObservedSHA  string `json:"observed_sha,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// AllJSONFields selects every --json field.
const AllJSONFields = "valid,findings,workflows,dependencies,pins"

// findingFromReport converts an checks.Finding to a JSON-safe Finding.
func findingFromReport(f checks.Finding) Finding {
	jf := Finding{
		Workflow:    f.WorkflowPath,
		Category:    string(f.Category),
		Severity:    string(f.Severity),
		Confidence:  string(f.Confidence),
		Detail:      f.Detail,
		Remediation: f.Remediation,
		DocURL:      f.DocURL,
	}
	if f.Dependency != nil {
		jf.Dependency = f.Dependency.Key()
	} else if f.ActionRef != nil {
		jf.Dependency = f.ActionRef.FullName() + "@" + f.ActionRef.Ref
	}
	if f.ParentNWO != "" {
		jf.RequiredBy = f.ParentNWO
	}
	return jf
}

// WriteJSON writes the unified JSON output for `check --json`. fieldsCSV is
// the comma-separated user selection (e.g. "valid,findings,workflows").
// cliVersion and lockfileVersion are emitted as top-level fields so consumers
// can pin behavior to a known schema. record is nil when no fix was planned,
// which emits an empty pins list.
func WriteJSON(w io.Writer, report *checks.Report, record *pin.Record, valid bool, fieldsCSV, cliVersion, lockfileVersion, homeHost string) error {
	fields := strings.Split(fieldsCSV, ",")
	outputHostname := func(host string) string {
		if ghapi.IsProxima(homeHost) && host == "github.com" {
			return host
		}
		return ""
	}

	// Build all data lazily.
	var allFindings []Finding
	var allDeps []Dependency
	var allWorkflows []Workflow

	buildFindings := func() []Finding {
		if allFindings != nil {
			return allFindings
		}
		allFindings = []Finding{}
		for _, f := range report.RepoFindings {
			allFindings = append(allFindings, findingFromReport(f))
		}
		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if skipFindingInJSON(f) {
					continue
				}
				allFindings = append(allFindings, findingFromReport(f))
			}
		}
		return allFindings
	}

	buildDeps := func() []Dependency {
		if allDeps != nil {
			return allDeps
		}
		allDeps = []Dependency{}
		// Deduplicate across workflows, merging required_by lists.
		seen := make(map[string]*Dependency)
		var order []string
		for _, wr := range report.Workflows {
			for _, inv := range wr.Inventory {
				key := inv.Dep.Key()
				if existing, ok := seen[key]; ok {
					// Merge required_by lists.
					for _, p := range inv.Parents {
						found := false
						for _, ep := range existing.RequiredBy {
							if ep == p {
								found = true
								break
							}
						}
						if !found {
							existing.RequiredBy = append(existing.RequiredBy, p)
						}
					}
					// If direct in any workflow, mark as direct.
					if inv.Direct {
						existing.Direct = true
					}
					continue
				}
				d := Dependency{
					Hostname:   outputHostname(inv.Dep.Hostname),
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				}
				seen[key] = &d
				order = append(order, key)
			}
		}
		for _, key := range order {
			allDeps = append(allDeps, *seen[key])
		}
		return allDeps
	}

	buildWorkflows := func() []Workflow {
		if allWorkflows != nil {
			return allWorkflows
		}
		allWorkflows = []Workflow{}
		for _, wr := range report.Workflows {
			wf := Workflow{
				Path:     wr.Path,
				Valid:    wr.IsValid(),
				Findings: []Finding{},
			}
			for _, f := range wr.Findings {
				if skipFindingInJSON(f) {
					continue
				}
				wf.Findings = append(wf.Findings, findingFromReport(f))
			}
			for _, inv := range wr.Inventory {
				wf.Dependencies = append(wf.Dependencies, Dependency{
					Hostname:   outputHostname(inv.Dep.Hostname),
					NWO:        inv.Dep.NWO,
					Ref:        inv.Dep.Ref,
					SHA:        inv.Dep.SHA,
					HashAlgo:   inv.Dep.HashAlgo,
					Direct:     inv.Direct,
					RequiredBy: inv.Parents,
				})
			}
			allWorkflows = append(allWorkflows, wf)
		}
		return allWorkflows
	}

	payload := map[string]interface{}{
		"cli_version":      cliVersion,
		"lockfile_version": lockfileVersion,
	}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		switch field {
		case "valid":
			payload[field] = valid
		case "findings":
			payload[field] = buildFindings()
		case "dependencies":
			payload[field] = buildDeps()
		case "workflows":
			payload[field] = buildWorkflows()
		case "pins":
			payload[field] = pinsFromRecord(record, report)
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, findings, workflows, dependencies, pins)", field)
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// pinsFromRecord lists one Pin per host/NWO@Ref; the record holds one entry
// per workflow that uses the action. Host and NWO are case-insensitive, refs
// are not (see ghapi.ForNWORef).
//
// Plan carries a blocked workflow's lock entries forward as Verified so Commit
// leaves them alone. Here they are reported as skipped, or blocked when an
// error finding names the pin, so a log never calls an unchecked pin verified.
func pinsFromRecord(record *pin.Record, report *checks.Report) []Pin {
	pins := []Pin{}
	if record == nil {
		return pins
	}
	blockedWF := map[string]bool{}
	blockedDep := map[string]checks.Finding{}
	if report != nil {
		for _, wr := range report.Workflows {
			if !wr.SkipCommit && !wr.BlockingResolverError {
				continue
			}
			blockedWF[wr.Path] = true
			for _, f := range wr.Findings {
				if f.Severity == checks.SeverityError && f.Dependency != nil {
					k := strings.ToLower(f.Dependency.NWO) + "@" + f.Dependency.Ref
					if _, ok := blockedDep[k]; !ok {
						blockedDep[k] = f
					}
				}
			}
		}
	}
	healthy := map[string]bool{}
	for _, e := range record.Entries {
		for _, w := range e.Workflows {
			if !blockedWF[w] {
				healthy[strings.ToLower(e.Hostname+"/"+e.NWO)+"@"+e.Ref] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range record.Entries {
		key := strings.ToLower(e.Hostname+"/"+e.NWO) + "@" + e.Ref
		if seen[key] {
			continue
		}
		seen[key] = true
		p := Pin{
			Hostname:     e.Hostname,
			NWO:          e.NWO,
			Ref:          e.Ref,
			SHA:          e.SHA,
			Outcome:      e.Resolution.String(),
			NarrowedFrom: e.AutoFixedRef,
			RenamedFrom:  e.RenamedFrom,
			ObservedSHA:  e.ObservedSHA,
			Reason:       e.Reason,
		}
		depKey := strings.ToLower(e.NWO) + "@" + e.Ref
		if f, ok := blockedDep[depKey]; ok {
			p.Outcome, p.Reason = "blocked", f.Detail
			delete(blockedDep, depKey)
		} else if !healthy[key] && e.Resolution == pin.Verified {
			p.Outcome, p.Reason = pin.Skipped.String(), "workflow blocked; lock entry left unchanged"
		}
		pins = append(pins, p)
	}
	// Blocked pins the lockfile never had (a first run) have no entry.
	var fresh []Pin
	for _, f := range blockedDep {
		fresh = append(fresh, Pin{
			Hostname:    f.Dependency.Hostname,
			NWO:         f.Dependency.NWO,
			Ref:         f.Dependency.Ref,
			Outcome:     "blocked",
			ObservedSHA: f.Dependency.SHA,
			Reason:      f.Detail,
		})
	}
	sort.Slice(fresh, func(i, j int) bool {
		return strings.ToLower(fresh[i].NWO+"@"+fresh[i].Ref) < strings.ToLower(fresh[j].NWO+"@"+fresh[j].Ref)
	})
	pins = append(pins, fresh...)
	return pins
}
