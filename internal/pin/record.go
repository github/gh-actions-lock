// Package pin implements the two-phase pin lifecycle: Plan builds a
// complete Record of what to pin (pure computation + network reads),
// and Commit writes the Record to disk (workflow files + lockfile).
package pin

// Entry records the plan decision for one action dependency.
type Entry struct {
	Hostname     string
	NWO          string
	Ref          string
	SHA          string
	ObservedSHA  string
	Resolution   Resolution
	Issue        string
	Reason       string
	Suggestion   string
	AutoFixedRef string // original ref before sane-release rewrite
	OnBranch     string
	Tag          string
	Workflows    []string
	RequiredBy   []string
	Direct       bool
	// RenamedFrom is the lockfile key this pin had before a same-repository
	// rename, so its recorded `uses:` carry over.
	RenamedFrom string
	FullScan    bool
}

// WorkflowPlan records what Commit must write for one workflow file.
type WorkflowPlan struct {
	Path     string
	Rewrites map[string]string
	// RequiredRewrites move redirected `uses:` to the canonical repository;
	// Commit fails rather than skip one.
	RequiredRewrites map[string]string
	// SelfActionFiles are in-repo action definition files reached from this
	// workflow through `$/…`. The same rewrites apply to their `uses:` lines.
	SelfActionFiles []string
}

// Record is the complete output of Plan: everything Commit needs to
// write all changes atomically.
type Record struct {
	Entries   []Entry
	Workflows []WorkflowPlan
}

// Pinned returns entries with Resolution == Pinned.
func (r *Record) Pinned() []Entry {
	return r.byResolution(Pinned)
}

// Investigated returns entries with Resolution == Investigate.
func (r *Record) Investigated() []Entry {
	return r.byResolution(Investigate)
}

// Unresolved returns entries with Resolution == Unresolved.
func (r *Record) Unresolved() []Entry {
	return r.byResolution(Unresolved)
}

// Narrowed returns verified entries whose refs were upgraded (AutoFixedRef set).
func (r *Record) Narrowed() []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Resolution == Verified && e.AutoFixedRef != "" {
			out = append(out, e)
		}
	}
	return out
}

// Valid reports whether the record contains no investigate or unresolved entries.
func (r *Record) Valid() bool {
	for _, e := range r.Entries {
		if e.Resolution == Investigate || e.Resolution == Unresolved {
			return false
		}
	}
	return true
}

func (r *Record) byResolution(res Resolution) []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Resolution == res {
			out = append(out, e)
		}
	}
	return out
}
