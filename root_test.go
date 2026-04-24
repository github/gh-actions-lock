package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
)

func TestPreviewMessageForNewPins(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "setup-go", Ref: "v6"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{NWO: "actions/setup-go", Ref: "v6", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{NWO: "actions/cache", Ref: "v4", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}

	msg := previewMessage(
		".github/workflows/test.yml",
		refs,
		nil,
		newDeps,
		"gh actions-pin --diff .github/workflows/test.yml",
		"gh actions-pin --write .github/workflows/test.yml",
	)

	for _, want := range []string{
		"Preview summary for .github/workflows/test.yml",
		"direct: 2 added",
		"transitive: 1 added",
		"Review with: gh actions-pin --diff .github/workflows/test.yml",
		"Apply with:  gh actions-pin --write .github/workflows/test.yml",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("preview message missing %q:\n%s", want, msg)
		}
	}
}

func TestPreviewMessageForUnchangedPins(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
	}
	oldDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	msg := previewMessage(
		".github/workflows/test.yml",
		refs,
		oldDeps,
		newDeps,
		"gh actions-pin --diff .github/workflows/test.yml",
		"gh actions-pin --write .github/workflows/test.yml",
	)

	if !strings.Contains(msg, "Preview: no dependency changes for .github/workflows/test.yml") {
		t.Fatalf("expected no-change preview message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "unchanged: 1") {
		t.Fatalf("expected unchanged count, got:\n%s", msg)
	}
}

func TestPreviewMessageOmitsReviewHintWhenDiffAlreadyShown(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	msg := previewMessage(".github/workflows/test.yml", refs, nil, newDeps, "", "gh actions-pin --write .github/workflows/test.yml")

	if strings.Contains(msg, "Review with:") {
		t.Fatalf("expected review hint to be omitted when diff is already shown:\n%s", msg)
	}
}

func TestPreviewMessageTreatsManualRefEditsAsDirectChanges(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v5"},
	}
	oldDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v5", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}

	msg := previewMessage(
		".github/workflows/test.yml",
		refs,
		oldDeps,
		newDeps,
		"gh actions-pin --diff .github/workflows/test.yml",
		"gh actions-pin --write .github/workflows/test.yml",
	)

	if !strings.Contains(msg, "direct: 1 changed") {
		t.Fatalf("expected manual ref edit to count as a direct change:\n%s", msg)
	}
	if !strings.Contains(msg, "Direct ref changes need `gh actions-pin upgrade --action <action>`.") {
		t.Fatalf("expected preview guidance for direct ref changes:\n%s", msg)
	}
}

func TestShowDiffIncludesCommitLinks(t *testing.T) {
	oldDeps := []lockfile.Dependency{
		{NWO: "actions/setup-go", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	newDeps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{NWO: "actions/setup-go", Ref: "v6", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	oldOutput := output
	output = ui.NewPlain(w)
	showDiff("github.com", oldDeps, newDeps)
	_ = w.Close()
	os.Stderr = oldStderr
	output = oldOutput

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := string(out)

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

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	oldOutput := output
	output = ui.NewPlain(w)
	showDiff("github.com", oldDeps, newDeps)
	_ = w.Close()
	os.Stderr = oldStderr
	output = oldOutput

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := string(out)

	for _, want := range []string{
		"~ actions/checkout",
		"- github.com/actions/checkout@v6:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"+ github.com/actions/checkout@v5:sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"compare: https://github.com/actions/checkout/compare/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa...bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("showDiff output missing %q:\n%s", want, got)
		}
	}
}

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := newRootCmd()
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Fatalf("expected root command to suppress Cobra usage/errors for handled failures")
	}
}

func TestBuildCommandHintIncludesUpgradeFlags(t *testing.T) {
	got := buildCommandHint(
		"gh actions-pin upgrade",
		".github/workflows/test.yml",
		[]string{"actions/checkout"},
		"v5",
		"v6",
		true,
	)

	for _, want := range []string{
		"gh actions-pin upgrade",
		"--action actions/checkout",
		"--from v5",
		"--version v6",
		"--write",
		".github/workflows/test.yml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected command hint to contain %q, got %q", want, got)
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
