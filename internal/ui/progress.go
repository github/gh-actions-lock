package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// progressTrace caches the result of GH_ACTIONS_LOCK_DEBUG_PROGRESS once. When
// set, every UpdateLabel and SetWorkerStatus call writes a timestamped JSONL
// line to a dedicated trace file so we can audit phase transitions and verify
// the worker pool is actually fanning out without depending on visual
// inspection of the spinner. The path is resolved from the env var: "1" or
// "true" maps to $TMPDIR/gh-actions-lock-progress.log, anything else is treated
// as a literal path.
var (
	progressTraceMu   sync.Mutex
	progressTraceFile *os.File
	progressTracePath = resolveProgressTracePath()
)

func resolveProgressTracePath() string {
	v := os.Getenv("GH_ACTIONS_LOCK_DEBUG_PROGRESS")
	switch v {
	case "":
		return ""
	case "1", "true", "yes":
		dir := os.TempDir()
		return dir + "/gh-actions-lock-progress.log"
	default:
		return v
	}
}

// CloseProgressTrace closes the progress trace file if one was opened. It is
// safe to call unconditionally; it no-ops when tracing is off or already closed.
func CloseProgressTrace() {
	progressTraceMu.Lock()
	defer progressTraceMu.Unlock()
	if progressTraceFile != nil {
		progressTraceFile.Close()
		progressTraceFile = nil
	}
}

// traceProgress emits a structured trace event to the progress trace file when
// progress tracing is enabled. kind is "label" or "slot[N]"; payload is the new
// value. No-op when tracing is off.
func (u *UI) traceProgress(kind, payload string) {
	if progressTracePath == "" {
		return
	}
	progressTraceMu.Lock()
	defer progressTraceMu.Unlock()
	if progressTraceFile == nil {
		f, err := os.OpenFile(progressTracePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		progressTraceFile = f
		fmt.Fprintf(f, "# gh-actions-lock progress trace %s\n", time.Now().Format(time.RFC3339))
	}
	rec := struct {
		Time    string `json:"time"`
		Kind    string `json:"kind"`
		Payload string `json:"payload"`
	}{
		Time:    time.Now().Format(time.RFC3339Nano),
		Kind:    kind,
		Payload: payload,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Fprintln(progressTraceFile, string(b))
}

// UpdateProgress sets a detail string in worker slot 0 (backward-compat
// single-detail shim). No-op when no spinner is active.
func (u *UI) UpdateProgress(detail string) {
	if u.spinner == nil {
		return
	}
	u.progDetail = detail
	u.renderProgress()
}

// SetWorkerStatus sets or clears one worker slot's status line, shown as a
// subdued line below the spinner. slot indexes from 0. No-op when no spinner
// is active.
func (u *UI) SetWorkerStatus(slot int, status string) {
	u.traceProgress(fmt.Sprintf("slot[%d]", slot), status)
	if u.spinWriter == nil {
		return
	}
	if !u.noColor {
		width := u.termWidth()
		if width > 4 {
			status = truncateBytes(status, width-4)
		}
	}
	u.spinWriter.setWorkerStatus(slot, status)
}

// SetWorkerHint sets or clears a dim suffix appended after the worker slot's
// status text (e.g. "→ workflow.yml (still working…)"). Used by pinpool's
// stall watcher to surface that a worker has been on the same job for longer
// than the stall threshold without clobbering the slot's main status. No-op
// when no spinner is active.
func (u *UI) SetWorkerHint(slot int, hint string) {
	u.traceProgress(fmt.Sprintf("hint[%d]", slot), hint)
	if u.spinWriter == nil {
		return
	}
	u.spinWriter.setWorkerHint(slot, hint)
}

// ClearWorkerStatuses wipes every worker slot so stale "✓ NWO" rows from a
// completed phase don't carry into the next one. No-op when no spinner is
// active.
func (u *UI) ClearWorkerStatuses() {
	if u.spinWriter == nil {
		return
	}
	u.spinWriter.mu.Lock()
	for i := range u.spinWriter.workers {
		u.spinWriter.workers[i] = ""
	}
	for i := range u.spinWriter.hints {
		u.spinWriter.hints[i] = ""
	}
	// Immediately erase the old rows from the terminal rather than
	// waiting for the next spinner tick (~120ms). Without this, the
	// stale rows sit on screen for one tick and then all vanish at once,
	// which looks like a jump at phase transitions.
	u.spinWriter.renderWorkersLocked()
	u.spinWriter.mu.Unlock()
}

// UpdateLabel is a no-op on TTY — the spinner label is static for the
// lifetime of the spinner. In headless mode it logs a plain-text phase
// boundary when the label changes.
func (u *UI) UpdateLabel(label string) {
	u.traceProgress("label", label)
	if !u.headless {
		return
	}
	stem := labelStem(label)
	if stem != "" && stem != u.headlessLabelStem {
		u.headlessEmit(stem)
		u.headlessLabelStem = stem
	}
}

// renderProgress updates worker slot 0 with the current detail string.
// The label (top-line prefix) is managed separately by UpdateLabel.
func (u *UI) renderProgress() {
	if u.spinner == nil {
		return
	}

	detail := u.progDetail
	if detail == "" {
		if u.progHasDetail {
			if u.spinWriter != nil {
				u.spinWriter.setDetail("")
			}
			u.progHasDetail = false
		}
		return
	}

	// Worker rows animate only when text starts with "→ "; UpdateProgress
	// callers (resolver hooks) pass plain strings. Prepend the arrow so
	// the slot pulses instead of looking frozen.
	if !strings.HasPrefix(detail, "→ ") {
		detail = "→ " + detail
	}
	width := u.termWidth()
	if width > 4 {
		budget := width - 4
		if !u.noColor {
			budget -= 7
		}
		detail = truncateBytes(detail, budget)
	}

	if u.spinWriter != nil {
		u.spinWriter.setDetail(detail)
	}
	u.progHasDetail = true
}
