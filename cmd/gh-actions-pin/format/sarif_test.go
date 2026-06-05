package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// writeWorkflow drops a workflow fixture into a temp dir and returns
// its path. The path is what we hand to doctor.Finding.WorkflowPath so
// the SARIF locator can re-read the file at emit time.
func writeWorkflow(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// decode parses the SARIF bytes back into a typed document so tests
// assert on structure rather than fragile string substrings.
func decode(t *testing.T, b []byte) sarifLog {
	t.Helper()
	var doc sarifLog
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid SARIF JSON: %v\n%s", err, b)
	}
	if doc.Version != sarifVersion {
		t.Errorf("version = %q, want %q", doc.Version, sarifVersion)
	}
	if len(doc.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(doc.Runs))
	}
	return doc
}

// findResult returns the first result with the given ruleId, or fails.
func findResult(t *testing.T, doc sarifLog, ruleID string) sarifResult {
	t.Helper()
	for _, r := range doc.Runs[0].Results {
		if r.RuleID == ruleID {
			return r
		}
	}
	t.Fatalf("no result with ruleId %q in %+v", ruleID, doc.Runs[0].Results)
	return sarifResult{}
}

// TestWriteSARIF_PerRuleID covers one finding per rule ID we emit, so
// the rule-ID mapping and severity-level translation are pinned by
// table. zizmor-overlap rules (impostor-commit, unpinned-uses) stay on
// the zizmor names; everything else keeps our kebab-case category ID.
func TestWriteSARIF_PerRuleID(t *testing.T) {
	wfBody := `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@abc1234567890abcdef1234567890abcdef12345
      - uses: actions/cache@v3
      - uses: actions/upload-artifact@deadbeef
      - uses: docker/build-push-action@v5
      - uses: actions/download-artifact@v4
      - uses: hashicorp/setup-terraform@v2
`
	wf := writeWorkflow(t, "ci.yml", wfBody)

	ref := func(owner, repo, version string) *lockfile.ActionRef {
		return &lockfile.ActionRef{Owner: owner, Repo: repo, Ref: version, Raw: owner + "/" + repo + "@" + version}
	}

	cases := []struct {
		name      string
		finding   doctor.Finding
		wantRule  string
		wantLevel string
	}{
		{
			name: "unpinned-uses (NotPinned)",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryNotPinned,
				Severity:     doctor.SeverityError,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("actions", "checkout", "v4"),
				Detail:       "checkout is not pinned",
			},
			wantRule:  "unpinned-uses",
			wantLevel: "error",
		},
		{
			name: "sha-as-ref",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategorySHAAsRef,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("actions", "setup-node", "abc1234567890abcdef1234567890abcdef12345"),
				Detail:       "bare SHA without companion ref",
			},
			wantRule:  "sha-as-ref",
			wantLevel: "warning",
		},
		{
			name: "ref-changed",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryRefChanged,
				Severity:     doctor.SeverityError,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("actions", "cache", "v3"),
				Detail:       "ref was edited from v3.1.0 to v3",
			},
			wantRule:  "ref-changed",
			wantLevel: "error",
		},
		{
			name: "impostor-commit",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryImpostorCommit,
				Severity:     doctor.SeverityError,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("actions", "upload-artifact", "deadbeef"),
				Detail:       "commit not reachable from any branch",
			},
			wantRule:  "impostor-commit",
			wantLevel: "error",
		},
		{
			name: "misleading-sha",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryMisleadingSHA,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceMedium,
				ActionRef:    ref("docker", "build-push-action", "v5"),
				Detail:       "ref looks SHA-shaped but resolves elsewhere",
			},
			wantRule:  "misleading-sha",
			wantLevel: "warning",
		},
		{
			name: "lockfile-forgery",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryLockfileForgery,
				Severity:     doctor.SeverityError,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("actions", "download-artifact", "v4"),
				Detail:       "pinned SHA not in ref ancestry",
			},
			wantRule:  "lockfile-forgery",
			wantLevel: "error",
		},
		{
			name: "ref-moved",
			finding: doctor.Finding{
				WorkflowPath: wf,
				Category:     doctor.CategoryRefMoved,
				Severity:     doctor.SeverityInfo,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    ref("hashicorp", "setup-terraform", "v2"),
				Detail:       "upstream ref moved",
			},
			wantRule:  "ref-moved",
			wantLevel: "note",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := &doctor.Report{
				Workflows: []doctor.WorkflowReport{{
					Path:     wf,
					Findings: []doctor.Finding{tc.finding},
				}},
			}
			var buf bytes.Buffer
			if err := WriteSARIF(&buf, report, "v0.0.0-test"); err != nil {
				t.Fatalf("WriteSARIF: %v", err)
			}
			doc := decode(t, buf.Bytes())
			r := findResult(t, doc, tc.wantRule)
			if r.Level != tc.wantLevel {
				t.Errorf("level = %q, want %q", r.Level, tc.wantLevel)
			}
			// Confidence rides in properties.confidence, not in level.
			if got, _ := r.Properties["confidence"].(string); got != string(tc.finding.Confidence) {
				t.Errorf("properties.confidence = %q, want %q", got, tc.finding.Confidence)
			}
			// Location must point at a real workflow line (>1) and the
			// snippet must be the trimmed `uses:` line.
			loc := r.Locations[0].PhysicalLocation
			if loc.ArtifactLocation.URI != wf {
				t.Errorf("uri = %q, want %q", loc.ArtifactLocation.URI, wf)
			}
			if loc.Region == nil || loc.Region.StartLine < 2 {
				t.Errorf("startLine = %v, want >= 2 (found the uses line)", loc.Region)
			}
			if loc.Region.Snippet == nil || !strings.HasPrefix(loc.Region.Snippet.Text, "- uses:") {
				t.Errorf("snippet = %+v, want trimmed `- uses:` line", loc.Region.Snippet)
			}
			// partialFingerprints required + matches sha256 of snippet.
			want := sha256.Sum256([]byte(loc.Region.Snippet.Text))
			if got := r.PartialFingerprints["primaryLocationLineHash"]; got != hex.EncodeToString(want[:]) {
				t.Errorf("primaryLocationLineHash mismatch:\n got %s\nwant %s", got, hex.EncodeToString(want[:]))
			}
		})
	}
}

// TestWriteSARIF_MultipleFindingsPerWorkflow ensures a single workflow
// with several findings produces several results, each anchored to its
// own `uses:` line, and that the document remains deterministic across
// runs (sorted by uri, line, ruleId).
func TestWriteSARIF_MultipleFindingsPerWorkflow(t *testing.T) {
	wfBody := `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - uses: actions/cache@v3
`
	wf := writeWorkflow(t, "ci.yml", wfBody)

	mk := func(name, version string, cat doctor.Category, sev doctor.Severity) doctor.Finding {
		return doctor.Finding{
			WorkflowPath: wf,
			Category:     cat,
			Severity:     sev,
			Confidence:   doctor.ConfidenceHigh,
			ActionRef:    &lockfile.ActionRef{Owner: "actions", Repo: name, Ref: version, Raw: "actions/" + name + "@" + version},
			Detail:       name + " issue",
		}
	}

	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path: wf,
			Findings: []doctor.Finding{
				mk("checkout", "v4", doctor.CategoryRefMoved, doctor.SeverityInfo),
				mk("setup-go", "v5", doctor.CategoryRefChanged, doctor.SeverityError),
				mk("cache", "v3", doctor.CategoryNotPinned, doctor.SeverityError),
			},
		}},
	}

	var buf bytes.Buffer
	if err := WriteSARIF(&buf, report, "v0.0.0-test"); err != nil {
		t.Fatalf("WriteSARIF: %v", err)
	}
	doc := decode(t, buf.Bytes())
	if got := len(doc.Runs[0].Results); got != 3 {
		t.Fatalf("results = %d, want 3", got)
	}

	// Sort contract: by (uri, line, ruleId). Same uri, so order should
	// follow startLine ascending (checkout=7, setup-go=8, cache=9).
	lines := []int{
		doc.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.StartLine,
		doc.Runs[0].Results[1].Locations[0].PhysicalLocation.Region.StartLine,
		doc.Runs[0].Results[2].Locations[0].PhysicalLocation.Region.StartLine,
	}
	if !(lines[0] < lines[1] && lines[1] < lines[2]) {
		t.Errorf("results not sorted by startLine: %v", lines)
	}

	// Run again — bytes must match byte-for-byte. Deterministic emission
	// is required for snapshot-friendly CI artifacts and for
	// code-scanning dedup to work.
	var buf2 bytes.Buffer
	if err := WriteSARIF(&buf2, report, "v0.0.0-test"); err != nil {
		t.Fatalf("WriteSARIF (second run): %v", err)
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Error("SARIF output is not deterministic across two calls")
	}
}

// TestWriteSARIF_EmptyFindings is the no-issues case. The document must
// still validate as SARIF 2.1.0, advertise our rule catalog, and have
// an explicit empty results array (not null) so code-scanning treats it
// as "scan ran, found nothing" rather than malformed.
func TestWriteSARIF_EmptyFindings(t *testing.T) {
	report := &doctor.Report{}
	var buf bytes.Buffer
	if err := WriteSARIF(&buf, report, "v1.2.3"); err != nil {
		t.Fatalf("WriteSARIF: %v", err)
	}
	doc := decode(t, buf.Bytes())
	if doc.Runs[0].Results == nil {
		t.Error("results must be [] not null when there are no findings")
	}
	if len(doc.Runs[0].Results) != 0 {
		t.Errorf("results = %d, want 0", len(doc.Runs[0].Results))
	}
	if len(doc.Runs[0].Tool.Driver.Rules) == 0 {
		t.Error("driver.rules should still advertise the catalog on an empty run")
	}
	if doc.Runs[0].Tool.Driver.Version != "v1.2.3" {
		t.Errorf("driver.version = %q, want v1.2.3", doc.Runs[0].Tool.Driver.Version)
	}

	// Spot-check that zizmor-overlap rule IDs are in the catalog so
	// upload consumers can pre-register them.
	seen := map[string]bool{}
	for _, r := range doc.Runs[0].Tool.Driver.Rules {
		seen[r.ID] = true
	}
	for _, id := range []string{"unpinned-uses", "impostor-commit", "onboarding-required"} {
		if !seen[id] {
			t.Errorf("catalog missing %q", id)
		}
	}
}

// TestWriteSARIF_SkipsNonFindings asserts CategoryValid+OK and
// CategoryRunOnly do not produce SARIF results — matches the JSON
// formatter's actionable-only contract.
func TestWriteSARIF_SkipsNonFindings(t *testing.T) {
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path: "ok.yml",
			Findings: []doctor.Finding{
				{WorkflowPath: "ok.yml", Category: doctor.CategoryValid, Severity: doctor.SeverityOK, Confidence: doctor.ConfidenceHigh},
				{WorkflowPath: "ok.yml", Category: doctor.CategoryRunOnly, Severity: doctor.SeverityOK, Confidence: doctor.ConfidenceHigh},
			},
		}},
	}
	var buf bytes.Buffer
	if err := WriteSARIF(&buf, report, "v0.0.0"); err != nil {
		t.Fatalf("WriteSARIF: %v", err)
	}
	doc := decode(t, buf.Bytes())
	if len(doc.Runs[0].Results) != 0 {
		t.Errorf("results = %d, want 0 (valid+OK and run-only must not surface)", len(doc.Runs[0].Results))
	}
}

// TestSeverityToLevel pins the mapping so a silent downgrade of an
// unmapped severity is impossible without a test change.
func TestSeverityToLevel(t *testing.T) {
	cases := []struct {
		sev   doctor.Severity
		level string
		err   bool
	}{
		{doctor.SeverityError, "error", false},
		{doctor.SeverityWarning, "warning", false},
		{doctor.SeverityInfo, "note", false},
		{doctor.SeverityOK, "note", false},
		{doctor.Severity("bogus"), "", true},
	}
	for _, tc := range cases {
		t.Run(string(tc.sev), func(t *testing.T) {
			got, err := severityToLevel(tc.sev)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want err? %v", err, tc.err)
			}
			if got != tc.level {
				t.Errorf("level = %q, want %q", got, tc.level)
			}
		})
	}
}

// TestMatchesUsesLine verifies the line locator handles quoted refs and
// trailing comments without false positives. A bug here would put the
// SARIF region on the wrong line and break code-scanning dedup.
func TestMatchesUsesLine(t *testing.T) {
	raw := "actions/checkout@v4"
	cases := []struct {
		line string
		want bool
	}{
		{"      - uses: actions/checkout@v4", true},
		{`      - uses: "actions/checkout@v4"`, true},
		{`      - uses: 'actions/checkout@v4'`, true},
		{"      - uses: actions/checkout@v4 # comment", true},
		{"      - uses: actions/checkout@v4.0.0", false},
		{"      - uses: actions/checkout-foo@v4", false},
		{"      # uses: actions/checkout@v4", false}, // commented out
	}
	for _, tc := range cases {
		if got := matchesUsesLine(tc.line, raw); got != tc.want {
			t.Errorf("matchesUsesLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
