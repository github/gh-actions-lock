package ghapi

import (
	"strings"
	"testing"
)

func TestBuildBranchCompareQuery(t *testing.T) {
	branches := []BranchHead{
		{Name: "main", SHA: "aaa"},
		{Name: "release/v4", SHA: "bbb"},
	}

	query, vars := buildBranchCompareQuery("o", "r", "targetsha", branches)

	for _, want := range []string{
		"$owner: String!", "$name: String!", "$sha: String!",
		"$ref0: String!", "$ref1: String!",
		"b0: ref(qualifiedName: $ref0)",
		"b1: ref(qualifiedName: $ref1)",
		"compare(headRef: $sha)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}

	if vars["owner"] != "o" || vars["name"] != "r" || vars["sha"] != "targetsha" {
		t.Fatalf("unexpected base vars: %+v", vars)
	}
	if vars["ref0"] != "refs/heads/main" {
		t.Fatalf("expected refs/heads/main, got %v", vars["ref0"])
	}
	if vars["ref1"] != "refs/heads/release/v4" {
		t.Fatalf("expected refs/heads/release/v4, got %v", vars["ref1"])
	}
}

func TestBatchReachabilitySize(t *testing.T) {
	if batchReachabilitySize != 50 {
		t.Fatalf("expected 50, got %d", batchReachabilitySize)
	}
}
