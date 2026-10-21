package checks

// troubleshootingDocs has one section per finding category, anchored by the
// category string.
//
// The editor's TypeScript table (actions/languageservices,
// workflow-parser/src/lockfile/diagnostics/doc-urls.ts) presents the same
// links and needs the same per-category anchors.
const troubleshootingDocs = "https://docs.github.com/actions/reference/security/workflow-dependency-lockfile-troubleshooting"

// documented lists the categories with a troubleshooting section. Valid,
// RunOnly and SelfRepositoryAction need no action, so they have none.
var documented = map[Category]bool{
	NotPinned:                true,
	ShaAsRef:                 true,
	RefChanged:               true,
	RefMoved:                 true,
	TagCommitMismatch:        true,
	Stale:                    true,
	MisleadingSHA:            true,
	UnreachablePin:           true,
	RepoRenamed:              true,
	RepoReplaced:             true,
	RepoUnavailable:          true,
	AncestryUnknown:          true,
	ReachabilityUnknown:      true,
	OnboardingRequired:       true,
	VersionRef:               true,
	LocalAction:              true,
	StaleWorkflow:            true,
	InvalidSelfRepositoryRef: true,
	FreshTag:                 true,
	CooldownConfigIgnored:    true,
	IncompleteLockfile:       true,
	ContainerImage:           true,
}

// DocURL returns the troubleshooting docs section for c, or "" when c has
// none.
func (c Category) DocURL() string {
	if !documented[c] {
		return ""
	}
	return troubleshootingDocs + "#" + string(c)
}

// DocURL returns the docs section for f's category. It's derived rather than
// stored so a finding whose category is rewritten later (e.g. by the
// --no-onboard gate) still links its own section.
func (f Finding) DocURL() string {
	return f.Category.DocURL()
}
