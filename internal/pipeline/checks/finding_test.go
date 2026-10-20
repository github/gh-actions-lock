package checks

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
)

func TestFindingIsValidFollowsSeverity(t *testing.T) {
	categories := []Category{Stale, NotPinned, RefChanged, UnreachablePin, ShaAsRef, RefMoved, ReachabilityUnknown, StaleWorkflow, Valid}
	for _, sev := range []Severity{SeverityOK, SeverityInfo, SeverityWarning, SeverityError} {
		for _, cat := range categories {
			t.Run(string(cat)+"/"+string(sev), func(t *testing.T) {
				f := Finding{Category: cat, Severity: sev, ActionRef: &parserlock.ActionRef{Owner: "o", Repo: "r", Ref: "v1"}}
				want := sev != SeverityError
				assert.Equal(t, want, f.IsValid())
				r := Report{Workflows: []WorkflowReport{{Findings: []Finding{f}}}}
				assert.Equal(t, want, r.IsValid())
			})
		}

	}
}

func TestInconclusiveFindingPresentationFollowsSeverity(t *testing.T) {
	for _, category := range []Category{ReachabilityUnknown, AncestryUnknown} {
		for _, severity := range []Severity{SeverityWarning, SeverityError} {
			t.Run(string(category)+"/"+string(severity), func(t *testing.T) {
				finding := Finding{Category: category, Severity: severity}
				assert.Equal(t, severity == SeverityWarning, finding.IsWarning())
				report := WorkflowReport{Findings: []Finding{finding}}
				assert.Equal(t, severity == SeverityError, report.NeedsAttention())
			})
		}
	}
}
