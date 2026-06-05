// Package parserlock is the source of truth for the workflow dependency
// lockfile format and pin grammar. It was originally developed as
// actions-workflow-parser/go/lockfile and was extracted into this
// repository so the format definition can be open-sourced alongside the
// CLI that owns it.
package lockfile

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Pin holds the parsed components of a dependency pin key.
//
//	"OWNER/REPO@REF:ALGO-HEX"
//
// The pin identifies a downloaded action tarball at repo+SHA granularity —
// matching the runner, which downloads `owner/repo@sha` once per ref and
// reuses the tree for any sub-action path. Sub-action paths (e.g. the
// `save` in `actions/cache/save@v4`) are graph traversal details, not pin
// identity, and do not appear in this serialized form.
type Pin struct {
	NWO   string // "actions/checkout"
	Owner string // "actions"
	Repo  string // "checkout"
	Ref   string // "v4"
	Algo  string // "sha1"
	Hex   string // "34e114876b0b11c390a56381ad16ebd13914f8d5"
}

// Canonical returns a copy of p with all case-insensitive components
// (owner, repo, algo, hex) normalized to lowercase. Ref preserves source
// casing — git refs are case-sensitive.
//
// This is the single normalization point for the lockfile pin grammar:
// String, IndexKey, and ParsePin all funnel through it, so callers never
// need their own ToLower bookkeeping when handing pins through this package.
func (p Pin) Canonical() Pin {
	p.Owner = strings.ToLower(p.Owner)
	p.Repo = strings.ToLower(p.Repo)
	p.Algo = strings.ToLower(p.Algo)
	p.Hex = strings.ToLower(p.Hex)
	p.NWO = p.Owner + "/" + p.Repo
	return p
}

// String returns the canonical pin form: "OWNER/REPO@REF:ALGO-HEX".
// This doubles as the actions-map key in the lockfile.
func (p Pin) String() string {
	c := p.Canonical()
	return c.NWO + "@" + c.Ref + ":" + c.Algo + "-" + c.Hex
}

// IndexKey returns the normalized lookup key for this pin without the digest:
// "OWNER/REPO@REF".
func (p Pin) IndexKey() string {
	c := p.Canonical()
	return c.NWO + "@" + c.Ref
}

// IndexKey builds the normalized lookup key for a dependency entry without
// the digest: "OWNER/REPO@REF".
func IndexKey(owner, repo, ref string) string {
	return Pin{Owner: owner, Repo: repo, Ref: ref}.IndexKey()
}

// ParsePin parses a pin string of the canonical form:
//
//	"OWNER/REPO@REF:ALGO-HEX"
//
// Returns ok=false if the string doesn't match the expected format,
// including any sub-action path component (e.g. "owner/repo/sub@ref:...")
// — the lockfile grammar is strictly repo-scoped, matching the runner's
// tarball download identity.
func ParsePin(s string) (Pin, bool) {
	atIdx := strings.IndexByte(s, '@')
	if atIdx <= 0 || atIdx == len(s)-1 {
		return Pin{}, false
	}
	repoPath := s[:atIdx]
	refHash := s[atIdx+1:]

	slashIdx := strings.IndexByte(repoPath, '/')
	if slashIdx <= 0 || slashIdx == len(repoPath)-1 {
		return Pin{}, false
	}
	owner := repoPath[:slashIdx]
	repo := repoPath[slashIdx+1:]
	// Sub-action paths are not part of the lockfile pin grammar: the runner
	// downloads at repo+sha granularity. Reject any extra slashes here so
	// hand-edited lockfiles don't drift into a path-bearing format.
	if strings.ContainsRune(repo, '/') {
		return Pin{}, false
	}

	colonIdx := strings.LastIndexByte(refHash, ':')
	if colonIdx <= 0 || colonIdx == len(refHash)-1 {
		return Pin{}, false
	}
	ref := refHash[:colonIdx]
	if strings.ContainsRune(ref, ':') {
		return Pin{}, false
	}
	hashSpec := refHash[colonIdx+1:]

	dashIdx := strings.IndexByte(hashSpec, '-')
	if dashIdx <= 0 || dashIdx == len(hashSpec)-1 {
		return Pin{}, false
	}
	algo := strings.ToLower(hashSpec[:dashIdx])
	hexDigest := strings.ToLower(hashSpec[dashIdx+1:])
	if !isValidDigest(algo, hexDigest) {
		return Pin{}, false
	}

	return Pin{
		Owner: owner,
		Repo:  repo,
		Ref:   ref,
		Algo:  algo,
		Hex:   hexDigest,
	}.Canonical(), true
}

// isValidPin checks that a pin string matches the canonical
// "OWNER/REPO@REF:ALGO-HEX" form.
func isValidPin(s string) bool {
	_, ok := ParsePin(s)
	return ok
}

// IsFullSha reports whether s looks like a full commit hash (SHA-1 or
// SHA-256). Callers use this to distinguish bare-SHA `uses:` refs from
// symbolic refs.
func IsFullSha(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// buildPinIndex parses a slice of plain pin strings (canonical
// "OWNER/REPO@REF:ALGO-HEX" form) into a lookup map keyed by IndexKey
// ("OWNER/REPO@REF"). Any duplicate IndexKey is an error: callers receive
// already-deduplicated lists from the parser, so a duplicate here indicates
// corruption upstream.
func buildPinIndex(deps []string) (map[string]Pin, error) {
	index := make(map[string]Pin, len(deps))
	for _, dep := range deps {
		pin, ok := ParsePin(dep)
		if !ok {
			return nil, fmt.Errorf("invalid dependency pin: %q", dep)
		}
		key := pin.IndexKey()
		if _, dup := index[key]; dup {
			return nil, fmt.Errorf("duplicate dependency key: %q", key)
		}
		index[key] = pin
	}
	return index, nil
}

func digestLength(algo string) (int, bool) {
	switch algo {
	case "sha1":
		return sha1.Size * 2, true
	case "sha256":
		return sha256.Size * 2, true
	default:
		return 0, false
	}
}

func isValidDigest(algo, digest string) bool {
	expectedDigestLength, ok := digestLength(algo)
	if !ok || len(digest) != expectedDigestLength {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
