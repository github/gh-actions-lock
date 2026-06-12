package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-pin/internal/ui"
)

type fakeConfirmer struct {
	result    bool
	err       error
	called    bool
	gotPrompt string
}

func (f *fakeConfirmer) Confirm(prompt string, _ bool) (bool, error) {
	f.called = true
	f.gotPrompt = prompt
	return f.result, f.err
}

func writeScratchLock(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "actions.lock")
	if err := os.WriteFile(p, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func discardUI() *ui.UI { return ui.NewWithWriter(io.Discard) }

func fileExists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

func TestNewLockRecovery(t *testing.T) {
	parseErr := errors.New("missing required action field \"owner_id\"")

	t.Run("read-only/relock command refuses to delete and fails", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, false)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if fc.called {
			t.Error("confirmer must not be consulted when delete is disallowed")
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("non-interactive flag fails without prompting", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(true, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if fc.called {
			t.Error("confirmer must not be consulted under --no-interactive")
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("headless session (canPrompt false) fails", func(t *testing.T) {
		lock := writeScratchLock(t)
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return nil, false }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place")
		}
	})

	t.Run("interactive confirm yes deletes and recovers", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: true}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if !recovered || err != nil {
			t.Fatalf("want (true, nil), got (%v, %v)", recovered, err)
		}
		if !fc.called {
			t.Error("confirmer should have been consulted")
		}
		if fileExists(t, lock) {
			t.Error("lockfile should have been deleted")
		}
	})

	t.Run("interactive confirm no fails and keeps file", func(t *testing.T) {
		lock := writeScratchLock(t)
		fc := &fakeConfirmer{result: false}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || err == nil {
			t.Fatalf("want (false, error), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place when the user declines")
		}
	})

	t.Run("confirm error propagates and keeps file", func(t *testing.T) {
		lock := writeScratchLock(t)
		boom := errors.New("prompt failed")
		fc := &fakeConfirmer{err: boom}
		rec := newLockRecovery(false, discardUI(), func() (confirmer, bool) { return fc, true }, true)

		recovered, err := rec(lock, parseErr)
		if recovered || !errors.Is(err, boom) {
			t.Fatalf("want (false, boom), got (%v, %v)", recovered, err)
		}
		if !fileExists(t, lock) {
			t.Error("lockfile must be left in place on prompt error")
		}
	})
}
