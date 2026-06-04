package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"golang.org/x/term"
)

// ErrAborted is returned when the user presses Ctrl+C to abort.
var ErrAborted = errors.New("aborted by user")

// ProgressController lets a prompter pause an active progress spinner while a
// prompt owns the terminal, then resume it afterwards. The UI satisfies this.
type ProgressController interface {
	PauseProgress()
	ResumeProgress()
}

// Prompter abstracts interactive user prompts for testing and non-TTY fallback.
type Prompter interface {
	// Confirm asks a yes/no question.
	Confirm(message string, defaultVal bool) (bool, error)
	// Select presents a single-choice menu. Returns the selected index.
	Select(message string, options []string) (int, error)
	// MultiSelect presents a multi-choice menu. Returns selected indices.
	MultiSelect(message string, options []string) ([]int, error)
	// IsInteractive returns true if this prompter can ask questions.
	IsInteractive() bool
}

// HuhPrompter implements Prompter using the huh library (same as gh CLI).
type HuhPrompter struct {
	out        io.Writer
	isTerminal func() bool
	progress   ProgressController
}

// NewHuhPrompter creates an interactive prompter that writes to stderr.
func NewHuhPrompter() *HuhPrompter {
	return &HuhPrompter{
		out:        os.Stderr,
		isTerminal: func() bool { return term.IsTerminal(int(os.Stderr.Fd())) },
	}
}

// NewHuhPrompterWithWriter creates a prompter that writes to the given writer
// and uses the provided function for TTY detection.
func NewHuhPrompterWithWriter(w io.Writer, isTerminal func() bool) *HuhPrompter {
	return &HuhPrompter{out: w, isTerminal: isTerminal}
}

func (p *HuhPrompter) IsInteractive() bool {
	return p.isTerminal()
}

// SetProgress attaches a progress controller that is paused while a prompt is
// on screen and resumed once it closes, so a continuous spinner and prompts
// don't fight over the terminal.
func (p *HuhPrompter) SetProgress(pc ProgressController) {
	p.progress = pc
}

// pauseProgress pauses the attached spinner (if any) and returns a function
// that resumes it. Always defer the returned function.
func (p *HuhPrompter) pauseProgress() func() {
	if p.progress == nil {
		return func() {}
	}
	p.progress.PauseProgress()
	return p.progress.ResumeProgress
}

func (p *HuhPrompter) Confirm(message string, defaultVal bool) (bool, error) {
	defer p.pauseProgress()()
	result := defaultVal
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(message).
				Value(&result).
				Affirmative("Yes").
				Negative("No"),
		),
	).WithOutput(p.out).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, ErrAborted
		}
		return false, err
	}
	return result, nil
}

func (p *HuhPrompter) Select(message string, options []string) (int, error) {
	if len(options) == 0 {
		return -1, fmt.Errorf("no options provided")
	}
	defer p.pauseProgress()()
	var selected int
	huhOptions := make([]huh.Option[int], len(options))
	for i, opt := range options {
		huhOptions[i] = huh.NewOption(opt, i)
	}

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[int]().
				Title(message).
				Options(huhOptions...).
				Value(&selected),
		),
	).WithOutput(p.out).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return -1, ErrAborted
		}
		return -1, err
	}
	return selected, nil
}

func (p *HuhPrompter) MultiSelect(message string, options []string) ([]int, error) {
	if len(options) == 0 {
		return nil, fmt.Errorf("no options provided")
	}
	defer p.pauseProgress()()
	var selected []int
	huhOptions := make([]huh.Option[int], len(options))
	for i, opt := range options {
		huhOptions[i] = huh.NewOption(opt, i)
	}

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[int]().
				Title(message).
				Options(huhOptions...).
				Value(&selected),
		),
	).WithOutput(p.out).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, ErrAborted
		}
		return nil, err
	}
	return selected, nil
}

// TestPrompter is a non-interactive prompter for tests.
// It plays back pre-configured responses in order.
type TestPrompter struct {
	confirmResponses     []bool
	selectResponses      []int
	multiSelectResponses [][]int
	confirmIdx           int
	selectIdx            int
	multiSelectIdx       int
}

// NewTestPrompter creates a test prompter with canned responses.
func NewTestPrompter(confirms []bool, selects []int) *TestPrompter {
	return &TestPrompter{
		confirmResponses: confirms,
		selectResponses:  selects,
	}
}

// NewTestPrompterFull creates a test prompter with all response types.
func NewTestPrompterFull(confirms []bool, selects []int, multiSelects [][]int) *TestPrompter {
	return &TestPrompter{
		confirmResponses:     confirms,
		selectResponses:      selects,
		multiSelectResponses: multiSelects,
	}
}

func (p *TestPrompter) IsInteractive() bool { return true }

func (p *TestPrompter) Confirm(message string, defaultVal bool) (bool, error) {
	if p.confirmIdx >= len(p.confirmResponses) {
		return defaultVal, nil
	}
	result := p.confirmResponses[p.confirmIdx]
	p.confirmIdx++
	return result, nil
}

func (p *TestPrompter) Select(message string, options []string) (int, error) {
	if p.selectIdx >= len(p.selectResponses) {
		return 0, nil
	}
	result := p.selectResponses[p.selectIdx]
	p.selectIdx++
	return result, nil
}

func (p *TestPrompter) MultiSelect(message string, options []string) ([]int, error) {
	if p.multiSelectIdx >= len(p.multiSelectResponses) {
		return nil, nil
	}
	result := p.multiSelectResponses[p.multiSelectIdx]
	p.multiSelectIdx++
	return result, nil
}

// NoopPrompter always returns defaults — used in non-interactive mode.
type NoopPrompter struct{}

func (p *NoopPrompter) IsInteractive() bool                                   { return false }
func (p *NoopPrompter) Confirm(message string, defaultVal bool) (bool, error) { return defaultVal, nil }
func (p *NoopPrompter) Select(message string, options []string) (int, error) {
	return -1, fmt.Errorf("non-interactive")
}
func (p *NoopPrompter) MultiSelect(message string, options []string) ([]int, error) {
	return nil, fmt.Errorf("non-interactive")
}
