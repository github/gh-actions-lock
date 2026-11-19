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
	// RenamedFrom lists the lockfile keys this pin had before a
	// same-repository rename, so their recorded `uses:` carry over.
	RenamedFrom []string
	FullScan    bool
	// ReRecorded marks a full-SHA pin whose entry recorded the SHA itself
	// as its ref; Commit records the tag or branch found for it instead.
	ReRecorded bool
	// FromLock marks an entry taken from the lockfile without reading its
	// manifest (see dep.Dependency.FromLock).
	FromLock bool
}

// WorkflowPlan records what Commit must write for one workflow file.
type WorkflowPlan struct {
	Path     string
	Rewrites map[string]string
	// RequiredRewrites move redirected `uses:` to the canonical repository;
	// Commit fails rather than skip one.
	RequiredRewrites map[string]string
}

// Record is the complete output of Plan: everything Commit needs to
// write all changes atomically.
type Record struct {
	Entries   []Entry
	Workflows []WorkflowPlan
	// EntryRepairs re-record entries the workflows reach only through
	// another entry's `uses:`; see planEntryRepairs.
	EntryRepairs []EntryRepair
}

// Pinned returns entries with Resolution == Pinned.
func (r *Record) Pinned() []Entry {
	return r.byResolution(Pinned)
}

// Verified returns entries with Resolution == Verified.
func (r *Record) Verified() []Entry {
	return r.byResolution(Verified)
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
