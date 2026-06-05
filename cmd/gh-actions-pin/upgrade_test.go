package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
)

func TestShowDiffIncludesCommitLinks(t *testing.T) {
	oldDeps := []lockfile.Dependency{
		{NWO: "actions/setup-go", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{NWO: "actions/setup-go", Ref: "v6", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}

	r, w, _ := os.Pipe()
	testUI := ui.NewPlain(w)
	showDiff(testUI, "github.com", oldDeps, newDeps)
	_ = w.Close()

	rawOut, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := string(rawOut)

	for _, want := range []string{
		"permalink: https://github.com/actions/checkout/commit/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"compare: https://github.com/actions/setup-go/compare/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa...cccccccccccccccccccccccccccccccccccccccc",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("showDiff output missing %q:\n%s", want, got)
		}
	}
}

func TestShowDiffIncludesCompareLinkForRefReplacement(t *testing.T) {
	oldDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v5", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}

	r, w, _ := os.Pipe()
	testUI := ui.NewPlain(w)
	showDiff(testUI, "github.com", oldDeps, newDeps)
	_ = w.Close()

	rawOut, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := string(rawOut)

	for _, want := range []string{
		"~ actions/checkout",
		"- actions/checkout@v6:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"+ actions/checkout@v5:sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"compare: https://github.com/actions/checkout/compare/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa...bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("showDiff output missing %q:\n%s", want, got)
		}
	}
}

func TestParseUpgradeTargets(t *testing.T) {
	targets, err := parseUpgradeTargets([]string{"actions/checkout@v6", "github.com/actions/setup-go"}, "v5", "")
	if err != nil {
		t.Fatalf("parseUpgradeTargets returned error: %v", err)
	}

	if len(targets) != 2 {
		t.Fatalf("expected two targets, got %d", len(targets))
	}
	if targets[0].Match != "actions/checkout" || targets[0].TargetRef != "v6" || targets[0].CurrentRef != "v5" {
		t.Fatalf("unexpected first target: %+v", targets[0])
	}
	if targets[1].Match != "actions/setup-go" || targets[1].TargetRef != "" || targets[1].CurrentRef != "v5" {
		t.Fatalf("unexpected second target: %+v", targets[1])
	}
}

func TestParseUpgradeTargetsRejectsMixedVersionSources(t *testing.T) {
	_, err := parseUpgradeTargets([]string{"actions/checkout@v6"}, "", "v5")
	if err == nil {
		t.Fatal("expected parseUpgradeTargets to reject mixed --version and inline @ref")
	}
}

// Dependabot passes --no-interactive to both `check` and `upgrade`; if upgrade
// rejects it the relock breaks at the flag parser.
func TestUpgradeAcceptsNoInteractiveFlag(t *testing.T) {
	cmd := newUpgradeCmd(&pinFactory{})
	if cmd.Flags().Lookup("no-interactive") == nil {
		t.Fatal("upgrade command must accept --no-interactive for symmetry with check")
	}
}

func TestMatchingUpgradeTargetHonorsCurrentRefSelector(t *testing.T) {
	targets, err := parseUpgradeTargets([]string{"actions/checkout"}, "v5", "v6")
	if err != nil {
		t.Fatalf("parseUpgradeTargets returned error: %v", err)
	}

	if _, ok := matchingUpgradeTarget(lockfile.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"}, targets); ok {
		t.Fatal("expected v4 ref not to match v5 selector")
	}
	target, ok := matchingUpgradeTarget(lockfile.ActionRef{Owner: "actions", Repo: "checkout", Ref: "v5"}, targets)
	if !ok {
		t.Fatal("expected v5 ref to match selector")
	}
	if target.TargetRef != "v6" {
		t.Fatalf("expected target ref v6, got %+v", target)
	}
}
