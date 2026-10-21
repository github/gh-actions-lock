// Package uses classifies `uses:` values, so every caller decides what a
// value is the same way and an unparseable reference can't be mistaken for
// one that is deliberately not a repository action.
package uses

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// Kind is what a `uses:` value refers to.
type Kind int

const (
	// Empty is a blank value.
	Empty Kind = iota
	// Action is a remote repository action, in Ref.Action.
	Action
	// Local is a `./…` path in the same checkout.
	Local
	// Docker is a `docker://` image.
	Docker
	// Expression contains `${{ … }}` and is only known at run time.
	Expression
	// SelfRepository is a `$/…` reference to the defining repository.
	SelfRepository
	// ReusableWorkflow is a remote owner/repo/.github/workflows/x.yml@ref,
	// valid only at jobs.<job_id>.uses.
	ReusableWorkflow
	// Invalid is a value the Actions backend rejects; Ref.Problem says why.
	Invalid
)

// Position is where a `uses:` value appears.
type Position int

const (
	// Step is a workflow step or a composite action step.
	Step Position = iota
	// Job is jobs.<job_id>.uses.
	Job
)

// Ref is a classified `uses:` value.
type Ref struct {
	Kind Kind
	// Raw is the trimmed value.
	Raw string
	// Action is set when Kind is Action.
	Action parserlock.ActionRef
	// Problem, set when Kind is Invalid, completes "uses: <Raw> …".
	Problem string
}

const (
	problemMalformed = "is not an action reference the Actions backend accepts"
	problemExtraAt   = "has more than one @; the Actions backend expects {owner}/{repo}[/path]@ref"
	problemStepWF    = "is a reusable workflow; reusable workflows should be referenced at the top-level `jobs.<job_id>.uses` key, not within steps"
)

// Classify classifies a `uses:` value found at pos.
func Classify(raw string, pos Position) Ref {
	v := strings.TrimSpace(raw)
	r := Ref{Raw: v}
	switch {
	case v == "":
		r.Kind = Empty
	case strings.HasPrefix(v, "./"):
		r.Kind = Local
	case strings.HasPrefix(v, "docker://"):
		r.Kind = Docker
	case strings.Contains(v, "${"):
		r.Kind = Expression
	case strings.HasPrefix(v, "$/"):
		r.Kind = SelfRepository
	case strings.Count(v, "@") > 1:
		// The lockfile grammar accepts a ref containing @, but the Actions
		// backend rejects the workflow, with or without a lockfile.
		r.Kind, r.Problem = Invalid, problemExtraAt
	default:
		if a := parserlock.ParseActionRef(v); a != nil {
			r.Kind, r.Action = Action, *a
		} else if parserlock.ParseReusableWorkflowRef(v) == nil {
			r.Kind, r.Problem = Invalid, problemMalformed
		} else if pos == Step {
			r.Kind, r.Problem = Invalid, problemStepWF
		} else {
			r.Kind = ReusableWorkflow
		}
	}
	return r
}

// Err is the error for an Invalid ref, nil otherwise.
func (r Ref) Err() error {
	if r.Kind != Invalid {
		return nil
	}
	return &Error{Raw: r.Raw, Problem: r.Problem}
}

// Error reports a `uses:` value the Actions backend rejects.
type Error struct {
	Raw     string
	Problem string
}

func (e *Error) Error() string { return "uses: " + e.Raw + " " + e.Problem }
