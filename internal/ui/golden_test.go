package ui

// Golden-file characterization tests for the UI output surface.
//
// These lock the exact bytes every styling and narration method emits across
// the three rendering modes (color, plain, headless) so a refactor that only
// moves code between files can prove it changed no output. Each method is
// captured into its own buffer and strconv.Quote'd, so ANSI escapes and
// hyperlinks are visible and diffs point straight at the method that drifted.
//
// To regenerate after an intentional output change:
//
//	UPDATE_GOLDEN=1 go test ./internal/ui/ -run TestUISurfaceGolden
//
// CI runs without the env var, so any output change must be intentional and
// committed alongside the code change.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

type uiMode struct {
	name     string
	noColor  bool
	headless bool
}

var uiModes = []uiMode{
	{name: "color", noColor: false, headless: false},
	{name: "plain", noColor: true, headless: false},
	{name: "headless", noColor: true, headless: true},
}

// renderUISurface drives every deterministic UI method with fixed inputs and
// returns a label-keyed transcript of the exact bytes each one emitted. The
// termenv profile is pinned to ANSI so color escapes don't depend on the host
// terminal. Spinner and progress methods are intentionally excluded: they are
// time-based and covered by the behavioral tests in ui_test.go.
func renderUISurface(mode uiMode) string {
	u := &UI{
		output:   termenv.NewOutput(io.Discard, termenv.WithProfile(termenv.ANSI)),
		noColor:  mode.noColor,
		headless: mode.headless,
	}

	var out bytes.Buffer
	sub := &bytes.Buffer{}
	u.w = sub

	// record captures the bytes a writing method emits to u.w.
	record := func(label string, fn func()) {
		sub.Reset()
		fn()
		fmt.Fprintf(&out, "%-12s %s\n", label, strconv.Quote(sub.String()))
	}
	// recordStr captures the value a string-returning method produces.
	recordStr := func(label, s string) {
		fmt.Fprintf(&out, "%-12s %s\n", label, strconv.Quote(s))
	}

	// Narration methods.
	record("Success", func() { u.Success("pinned %d actions", 3) })
	record("Error", func() { u.Error("could not resolve %s", "actions/checkout") })
	record("Warning", func() { u.Warning("ref moved upstream") })
	record("Skip", func() { u.Skip("already pinned") })
	record("Info", func() { u.Info("scanning %d workflows", 2) })
	record("Infof", func() { u.Infof("no trailing newline") })
	record("Header", func() { u.Header(".github/workflows/ci.yml") })
	record("Hint", func() { u.Hint("run gh actions-lock to fix") })
	record("Detail", func() { u.Detail("actions/checkout@v4") })
	record("Blank", func() { u.Blank() })

	// Term* summary methods (write directly to the terminal writer).
	record("TermSuccess", func() { u.TermSuccess("all dependencies locked") })
	record("TermError", func() { u.TermError("1 finding needs review") })
	record("TermWarn", func() { u.TermWarn("2 refs moved") })
	record("TermCaution", func() { u.TermCaution("pinned after branch scan") })
	record("TermDetail", func() { u.TermDetail("see %s", "actions.lock") })
	record("TermNeutral", func() { u.TermNeutral("resolution recorded") })
	record("TermBlank", func() { u.TermBlank() })

	// Color and style string helpers.
	recordStr("Bold", u.Bold("sample"))
	recordStr("Dim", u.Dim("sample"))
	recordStr("Red", u.Red("sample"))
	recordStr("Green", u.Green("sample"))
	recordStr("Yellow", u.Yellow("sample"))
	recordStr("Cyan", u.Cyan("sample"))
	recordStr("Hyperlink", u.Hyperlink("text", "https://example.com/x"))
	recordStr("DocLink", u.DocLink("https://example.com/docs"))
	recordStr("TermYellow", u.TermYellow("sample"))
	recordStr("TermDim", u.TermDim("sample"))
	recordStr("TermBold", u.TermBold("sample"))
	recordStr("TermLink", u.TermLink("text", "https://example.com/x"))

	// Free functions.
	recordStr("Pluralize1", Pluralize(1, "action", "actions"))
	recordStr("PluralizeN", Pluralize(2, "action", "actions"))

	return out.String()
}

func TestUISurfaceGolden(t *testing.T) {
	for _, mode := range uiModes {
		t.Run(mode.name, func(t *testing.T) {
			got := renderUISurface(mode)
			goldenPath := filepath.Join("testdata", "ui_surface_"+mode.name+".golden")

			if os.Getenv("UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o644))
				return
			}

			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "missing golden; regenerate with UPDATE_GOLDEN=1")
			require.Equal(t, string(want), got,
				"UI output drifted for mode %q; if intentional, regenerate with UPDATE_GOLDEN=1", mode.name)
		})
	}
}
