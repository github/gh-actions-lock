package httpmock

// This file mirrors cli/cli's pkg/httpmock/legacy.go: a small set of
// cross-cutting GitHub REST response shapes used by tests across multiple
// packages. Keep the surface narrow — only fixtures that are duplicated in
// two or more test packages belong here. Per-test bespoke shapes should stay
// inline at the call site.

// BranchListResponse builds a REST list-branches response body. pairs are
// (name, sha) alternating, e.g. BranchListResponse("main", "abc", "dev", "def").
// All entries are marked protected:true so tests model trusted-upstream
// branches by default; use BranchListResponseProtected to mix protected and
// unprotected entries.
func BranchListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":      pairs[i],
			"commit":    map[string]any{"sha": pairs[i+1]},
			"protected": true,
		})
	}
	return out
}

// BranchListResponseProtected builds a REST list-branches response body
// where each entry is a (name, sha, protected) triple.
func BranchListResponseProtected(triples ...any) any {
	out := make([]map[string]any, 0, len(triples)/3)
	for i := 0; i+2 < len(triples); i += 3 {
		out = append(out, map[string]any{
			"name":      triples[i],
			"commit":    map[string]any{"sha": triples[i+1]},
			"protected": triples[i+2],
		})
	}
	return out
}

// TagListResponse builds a REST list-tags response body. pairs are
// (name, sha) alternating.
func TagListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":   pairs[i],
			"commit": map[string]any{"sha": pairs[i+1]},
		})
	}
	return out
}

// CompareAncestorResponse builds a REST compare response indicating the
// requested sha is an ancestor of the head, with the given merge-base sha.
func CompareAncestorResponse(mergeBaseSHA string) any {
	return map[string]any{
		"status":            "ahead",
		"merge_base_commit": map[string]any{"sha": mergeBaseSHA},
	}
}
