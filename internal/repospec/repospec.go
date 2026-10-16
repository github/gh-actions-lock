// Package repospec parses remote repository arguments: the value of
// -R/--repo and the positional form of `--verify`.
package repospec

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/repository"
)

// Spec identifies a repository and, optionally, a ref to read.
type Spec struct {
	Host  string
	Owner string
	Repo  string
	Ref   string
}

// String renders the spec as [HOST/]OWNER/REPO[@REF].
func (s Spec) String() string {
	out := s.Owner + "/" + s.Repo
	if s.Host != "" {
		out = s.Host + "/" + out
	}
	if s.Ref != "" {
		out += "@" + s.Ref
	}
	return out
}

// Parse accepts OWNER/REPO[@REF], HOST/OWNER/REPO[@REF], and repository URLs
// (https://HOST/OWNER/REPO, optionally with /tree/<ref>, /releases/tag/<tag>
// or /commit/<sha>). A trailing .git is dropped. A ref in the URL form comes
// only from those paths; blob URLs are rejected because they name a file,
// not a repository.
func Parse(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, errors.New("empty repository")
	}
	if isURL(s) {
		return parseURL(s)
	}

	nwo, ref, hasRef := strings.Cut(s, "@")
	if hasRef && ref == "" {
		return Spec{}, fmt.Errorf("invalid repository %q: empty ref after @", s)
	}
	nwo = strings.TrimSuffix(nwo, ".git")
	// An empty fallback host keeps an unnamed host empty, so callers can
	// apply --hostname / GH_HOST precedence themselves.
	r, err := repository.ParseWithHost(nwo, "")
	if err != nil {
		return Spec{}, fmt.Errorf("invalid repository %q: %w", s, err)
	}
	return Spec{Host: strings.ToLower(r.Host), Owner: r.Owner, Repo: r.Name, Ref: ref}, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

func parseURL(s string) (Spec, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Spec{}, fmt.Errorf("invalid repository URL %q", s)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return Spec{}, fmt.Errorf("invalid repository URL %q: expected https://HOST/OWNER/REPO", s)
	}
	spec := Spec{
		Host:  strings.ToLower(u.Hostname()),
		Owner: segs[0],
		Repo:  strings.TrimSuffix(segs[1], ".git"),
	}
	rest := segs[2:]
	switch {
	case len(rest) == 0:
	case rest[0] == "tree" && len(rest) > 1:
		spec.Ref = strings.Join(rest[1:], "/")
	case len(rest) > 2 && rest[0] == "releases" && rest[1] == "tag":
		spec.Ref = strings.Join(rest[2:], "/")
	case rest[0] == "commit" && len(rest) == 2:
		spec.Ref = rest[1]
	default:
		return Spec{}, fmt.Errorf("invalid repository URL %q: only repository root, /tree/<ref>, /releases/tag/<tag> and /commit/<sha> URLs are supported", s)
	}
	return spec, nil
}

// Equal reports whether two specs name the same repository and ref.
// Host, owner and repo compare case-insensitively; refs are case-sensitive.
func (s Spec) Equal(o Spec) bool {
	return strings.EqualFold(s.Host, o.Host) &&
		strings.EqualFold(s.Owner, o.Owner) &&
		strings.EqualFold(s.Repo, o.Repo) &&
		s.Ref == o.Ref
}

// LooksLikeRepo reports whether a positional argument has the shape of a
// remote repository: a URL, or [HOST/]OWNER/REPO[@REF] with no .yml/.yaml
// extension. Callers must still check that the argument is not an existing
// path on disk.
func LooksLikeRepo(arg string) bool {
	if isURL(arg) {
		_, err := Parse(arg)
		return err == nil
	}
	nwo, _, _ := strings.Cut(arg, "@")
	lower := strings.ToLower(nwo)
	if strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") {
		return false
	}
	if strings.HasPrefix(nwo, ".") || strings.HasPrefix(nwo, "/") || strings.Contains(nwo, `\`) {
		return false
	}
	_, err := Parse(arg)
	return err == nil
}
