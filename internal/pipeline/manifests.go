package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/uses"
)

// manifestNode is one action a recorded entry provides: the entry's key and
// the sub-action path within its tarball.
type manifestNode struct {
	key  string
	path string
}

type manifestEdge struct {
	use parserlock.ActionRef
	// to is the recorded entry the edge leads to; zero when the lockfile
	// omits the edge or has no entry for it.
	to      manifestNode
	omitted bool
}

type manifestRead struct {
	err     error
	missing bool
	edges   []manifestEdge
	images  []uses.Image
	local   []*resolve.CompositeLocalPathError
}

// checkManifests reads each root's action.yml at its locked commit, then
// every recorded child it reaches, and reports any composite whose manifest
// uses an action its entry's `uses:` omits. An entry's `uses:` is the union
// over every path the lockfile reaches, so an edge no reached manifest
// declares is not reported. Reads are batched per level across workflows.
// roots holds, per report index, where its workflow enters the recorded
// graph. unhealthy holds the pins parity already reported (pinKey of NWO and
// commit); they are not read. byKey maps pinKey to each entry's key.
func checkManifests(ctx context.Context, r *resolve.Resolver, file parserlock.File, byKey map[string]string, report *checks.Report, roots map[int][]manifestNode, unhealthy map[string]bool) error {
	reads := map[manifestNode]*manifestRead{}
	var level []manifestNode
	for _, ri := range slices.Sorted(maps.Keys(roots)) {
		for _, n := range roots[ri] {
			if _, ok := reads[n]; !ok {
				reads[n] = nil
				level = append(level, n)
			}
		}
	}
	for len(level) > 0 {
		// Entries at one commit share its manifests: read each once.
		var reqs []ghapi.ActionFileRequest
		var nodes []manifestNode
		var nodeReq []int
		reqIdx := map[string]int{}
		for _, n := range level {
			p, _ := parserlock.ParsePin(n.key)
			sha := lockedSHA(file.Dependencies[n.key].Commit)
			if unhealthy[pinKey(p.NWO, sha)] {
				continue
			}
			id := pinKey(p.NWO, sha) + ":" + n.path
			i, ok := reqIdx[id]
			if !ok {
				i = len(reqs)
				reqIdx[id] = i
				reqs = append(reqs, ghapi.ActionFileRequest{Owner: p.Owner, Repo: p.Repo, Path: n.path, Ref: sha, Dockerfile: true})
			}
			nodes = append(nodes, n)
			nodeReq = append(nodeReq, i)
		}
		results := r.ReadManifests(ctx, reqs)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		level = nil
		for i, n := range nodes {
			rd := readManifest(n, file, byKey, results[nodeReq[i]])
			if rd.err != nil && ghapi.DependabotProxy() {
				msg := fmt.Sprintf("could not read locked manifest %s", nodeName(n))
				return &ghapi.BriefError{Msg: msg + ": " + briefCause(rd.err), Err: fmt.Errorf("%s: %s", msg, verbose(rd.err))}
			}
			reads[n] = rd
			for _, e := range rd.edges {
				if _, seen := reads[e.to]; e.to.key != "" && !seen {
					reads[e.to] = nil
					level = append(level, e.to)
				}
			}
		}
	}

	for ri, nodes := range roots {
		wr := &report.Workflows[ri]
		seen := map[manifestNode]bool{}
		for len(nodes) > 0 {
			n := nodes[0]
			nodes = nodes[1:]
			if seen[n] {
				continue
			}
			seen[n] = true
			rd := reads[n]
			if rd == nil {
				continue
			}
			for _, f := range nestedLocalActionFindings(wr.Path, rd.local) {
				if !slices.ContainsFunc(wr.Findings, func(g checks.Finding) bool { return g.Detail == f.Detail }) {
					wr.Findings = append(wr.Findings, f)
				}
			}
			for _, f := range manifestFindings(wr.Path, n, file.Dependencies[n.key], rd) {
				if f.Severity == checks.SeverityError {
					wr.Findings = dropValid(wr.Findings)
					wr.SkipCommit = true
				}
				wr.Findings = append(wr.Findings, f)
			}
			for _, img := range rd.images {
				checks.AddContainerImage(wr, img)
			}
			for _, e := range rd.edges {
				if e.to.key != "" {
					nodes = append(nodes, e.to)
				}
			}
		}
	}
	return nil
}

// readManifest classifies one read. Parity's failures are not read, so any
// failure here, a missing commit included, leaves the node unverified.
func readManifest(n manifestNode, file parserlock.File, byKey map[string]string, res ghapi.ActionFileResult) *manifestRead {
	switch {
	case res.Err != nil:
		return &manifestRead{err: res.Err}
	case res.CommitOID == "":
		return &manifestRead{err: errors.New("no result")}
	case res.ActionYML == "" && !res.Dockerfile:
		return &manifestRead{missing: true}
	}
	entry := file.Dependencies[n.key]
	p, _ := parserlock.ParsePin(n.key)
	recorded := map[string]bool{}
	for _, u := range entry.Uses {
		if c, ok := parserlock.ParsePin(u); ok {
			recorded[pinKey(c.NWO, c.Ref)] = true
		}
	}
	self := pinKey(p.NWO, p.Ref)
	// Malformed uses are diagnosed when the composite is first resolved; the
	// edges that remain are what the lockfile must record. Local uses are
	// kept so a run that keeps the pin still discloses them.
	uses, err := resolve.CompositeUses(dep.Dependency{NWO: p.NWO, Ref: p.Ref, SHA: lockedSHA(entry.Commit)}, res.ActionYML)
	rd := &manifestRead{images: resolve.ManifestImages(res.ActionYML, nodeName(n))}
	rd.local, _ = resolve.SplitLocalPaths(err)
	for _, u := range uses {
		k := pinKey(u.Ref.NWO(), u.Ref.Ref)
		e := manifestEdge{use: u.Ref}
		switch {
		case k == self:
			// Another path in the same tarball: no edge to record.
			e.to = manifestNode{key: n.key, path: u.Ref.Path}
		case !recorded[k]:
			e.omitted = true
		case byKey[k] != "":
			e.to = manifestNode{key: byKey[k], path: u.Ref.Path}
		}
		rd.edges = append(rd.edges, e)
	}
	return rd
}

func manifestFindings(path string, n manifestNode, entry parserlock.Action, rd *manifestRead) []checks.Finding {
	p, _ := parserlock.ParsePin(n.key)
	sha := lockedSHA(entry.Commit)
	short := parserlock.ShortSHA(sha)
	f := checks.Finding{
		WorkflowPath: path,
		Category:     checks.IncompleteLockfile,
		Severity:     checks.SeverityError,
		Confidence:   checks.ConfidenceHigh,
		Dependency:   &dep.Dependency{NWO: p.NWO, Path: n.path, Ref: p.Ref, SHA: sha},
	}
	switch {
	case rd.err != nil:
		// Unverified dependencies block until a re-run reads the manifest.
		f.Category, f.Confidence = checks.ReachabilityUnknown, checks.ConfidenceLow
		f.MarkRetryable(rd.err)
		f.Detail = fmt.Sprintf("could not read locked manifest %s at %s: %s", nodeName(n), short, verbose(rd.err))
		f.TermDetail = fmt.Sprintf("could not read locked manifest %s at %s: %s", nodeName(n), short, briefCause(rd.err))
		return []checks.Finding{f}
	case rd.missing:
		f.Detail = fmt.Sprintf("%s has no action.yml, action.yaml or Dockerfile at locked commit %s", nodeName(n), short)
		f.Remediation = "point `uses:` at an action that exists at that commit, then run `gh actions-lock --relock`"
		return []checks.Finding{f}
	}
	var out []checks.Finding
	for _, e := range rd.edges {
		if e.omitted {
			f.Dependency = &dep.Dependency{NWO: e.use.NWO(), Path: e.use.Path, Ref: e.use.Ref}
			f.ParentNWO = p.NWO + "@" + p.Ref
			f.Detail = fmt.Sprintf("%s at locked commit %s uses %s@%s, but its lockfile entry %s does not record it", nodeName(n), short, e.use.FullName(), e.use.Ref, n.key)
			f.Remediation = "restore the edge from version control, or run `gh actions-lock --relock`"
			out = append(out, f)
		}
	}
	return out
}

// pinKey is the identity a lockfile key, a `uses:` and a commit share:
// owner/repo folded, refs exact except full SHAs, which are lowercase.
func pinKey(nwo, ref string) string {
	if parserlock.IsFullSha(ref) {
		ref = strings.ToLower(ref)
	}
	return strings.ToLower(nwo) + "@" + ref
}

func nodeName(n manifestNode) string {
	p, _ := parserlock.ParsePin(n.key)
	name := p.NWO
	if n.path != "" {
		name += "/" + n.path
	}
	return name + "@" + p.Ref
}
