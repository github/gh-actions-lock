// Package format renders doctor reports for the `check` command.
package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/pkg/findings"
)

// Finding is the JSON-safe view of a doctor.Finding.
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

// findingFromDoctor converts a doctor.Finding to a JSON-safe Finding.
func findingFromDoctor(f doctor.Finding) Finding {
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
// can pin behavior to a known schema.
func WriteJSON(w io.Writer, report *doctor.Report, valid bool, fieldsCSV, cliVersion, lockfileVersion string) error {
	fields := strings.Split(fieldsCSV, ",")

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
			allFindings = append(allFindings, findingFromDoctor(f))
		}
		for _, wr := range report.Workflows {
			for _, f := range wr.Findings {
				if f.Category == findings.RunOnly || (f.Category == findings.Valid && f.Severity == findings.SeverityOK) {
					continue
				}
				allFindings = append(allFindings, findingFromDoctor(f))
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
				if f.Category == findings.RunOnly || (f.Category == findings.Valid && f.Severity == findings.SeverityOK) {
					continue
				}
				wf.Findings = append(wf.Findings, findingFromDoctor(f))
			}
			for _, inv := range wr.Inventory {
				wf.Dependencies = append(wf.Dependencies, Dependency{
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
		default:
			return fmt.Errorf("unknown JSON field %q (expected valid, findings, workflows, dependencies)", field)
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
