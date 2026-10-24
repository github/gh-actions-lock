// Package refresh selects the lock entries a run re-resolves live instead of
// keeping them as recorded. A nil *Set selects nothing.
package refresh

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
)

// Selector names lock entries by action. An empty Ref matches every ref the
// lockfile records for the action.
type Selector struct {
	NWO string
	Ref string
}

func (s Selector) String() string {
	if s.Ref == "" {
		return s.NWO
	}
	return s.NWO + "@" + s.Ref
}

// ParseSelector parses owner/repo or owner/repo@ref.
func ParseSelector(s string) (Selector, error) {
	nwo, ref, hasRef := strings.Cut(s, "@")
	owner, repo, ok := strings.Cut(nwo, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || hasRef && ref == "" {
		return Selector{}, fmt.Errorf("%q is neither a workflow file nor an owner/repo[@ref] action", s)
	}
	return Selector{NWO: nwo, Ref: ref}, nil
}

func (s Selector) matches(p parserlock.Pin) bool {
	if !strings.EqualFold(s.NWO, p.NWO) {
		return false
	}
	return s.Ref == "" || s.Ref == p.Ref || parserlock.IsFullSha(s.Ref) && strings.EqualFold(s.Ref, p.Ref)
}

// Set is the entries a run refreshes, and the workflows whose recorded
// closure holds one of them.
type Set struct {
	keys      map[string]bool
	workflows map[string]bool
}

// Expand selects the entries sels match, with every entry below them so a
// refreshed composite's dependencies are read from its manifest too. A
// selector that matches no entry is an error: there is nothing to update.
func Expand(store *lockfile.State, sels []Selector) (*Set, error) {
	s := &Set{keys: map[string]bool{}, workflows: map[string]bool{}}
	for _, sel := range sels {
		found := false
		for k := range store.File().Dependencies {
			p, ok := parserlock.ParsePin(k)
			if !ok || !sel.matches(p) {
				continue
			}
			found = true
			s.keys[normalize(p.NWO, p.Ref)] = true
			for _, lp := range store.Descendants(k) {
				s.keys[normalize(lp.Pin.NWO, lp.Pin.Ref)] = true
			}
		}
		if !found {
			return nil, fmt.Errorf("no lockfile entry matches %s", sel)
		}
	}
	all := func(parserlock.Pin, parserlock.Action) bool { return true }
	for _, wk := range store.WorkflowKeys() {
		for _, lp := range store.Closure(wk, all) {
			if s.Has(lp.Pin.NWO, lp.Pin.Ref) {
				s.workflows[wk] = true
				break
			}
		}
	}
	return s, nil
}

// Has reports whether the entry keyed nwo@ref is refreshed.
func (s *Set) Has(nwo, ref string) bool {
	return s != nil && s.keys[normalize(nwo, ref)]
}

// Workflow reports whether the workflow keyed wk records a refreshed entry.
func (s *Set) Workflow(wk string) bool {
	return s != nil && s.workflows[wk]
}

// normalize folds owner/repo but keeps a ref's case, since refs are
// case-sensitive; a full-SHA ref names a commit, so it folds too.
func normalize(nwo, ref string) string {
	if parserlock.IsFullSha(ref) {
		ref = strings.ToLower(ref)
	}
	return strings.ToLower(nwo) + "@" + ref
}
