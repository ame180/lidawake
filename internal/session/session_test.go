package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

type fakeLid struct {
	closed bool
	err    error
}

func (f *fakeLid) IsClosed(context.Context) (bool, error) { return f.closed, f.err }

type fakeSleep struct {
	disabled bool
	failSet  bool
}

func (f *fakeSleep) SetSleepDisabled(_ context.Context, v bool) error {
	if f.failSet {
		return errors.New("pmset failed")
	}
	f.disabled = v
	return nil
}
func (f *fakeSleep) SleepDisabled(context.Context) (bool, error) { return f.disabled, nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	m     *Manager
	lid   *fakeLid
	sleep *fakeSleep
	clk   *clock
	store FileStore
}

func newHarness(t *testing.T, boot string) *harness {
	t.Helper()
	h := &harness{
		lid:   &fakeLid{},
		sleep: &fakeSleep{},
		clk:   &clock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)},
		store: FileStore{Path: filepath.Join(t.TempDir(), "state.json")},
	}
	h.m = h.newManager(boot)
	return h
}

func (h *harness) newManager(boot string) *Manager {
	return NewManager(Config{
		Store:       h.store,
		Lid:         h.lid,
		Sleep:       h.sleep,
		Now:         h.clk.now,
		BootSession: boot,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

var ctx = context.Background()

func TestFullCycle(t *testing.T) {
	h := newHarness(t, "boot-1")
	if _, err := h.m.Start(ctx, 8*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !h.sleep.disabled {
		t.Fatal("sleep should be disabled after start")
	}

	h.m.Tick(ctx) // lid open, never closed yet: keep going
	if !h.m.Status().Active {
		t.Fatal("should stay active before lid closes")
	}

	h.lid.closed = true
	h.m.Tick(ctx)
	if !h.m.Status().SeenClosed {
		t.Fatal("should record lid closed")
	}

	h.lid.closed = false
	h.m.Tick(ctx)
	st := h.m.Status()
	if st.Active || h.sleep.disabled {
		t.Fatal("lid reopen should end session and re-enable sleep")
	}
	if st.LastEndReason != EndLidOpened {
		t.Fatalf("reason = %q", st.LastEndReason)
	}
}

func TestDeadline(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, time.Hour)
	h.lid.closed = true
	h.m.Tick(ctx)
	h.clk.advance(time.Hour)
	h.m.Tick(ctx)
	if h.m.Status().Active || h.sleep.disabled {
		t.Fatal("deadline should end session")
	}
	if h.m.Status().LastEndReason != EndDeadline {
		t.Fatal("wrong reason")
	}
}

func TestStop(t *testing.T) {
	h := newHarness(t, "boot-1")
	if _, err := h.m.Stop(ctx); !errors.Is(err, ErrNotActive) {
		t.Fatalf("stop without session: %v", err)
	}
	_, _ = h.m.Start(ctx, time.Hour)
	if _, err := h.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if h.sleep.disabled {
		t.Fatal("stop should re-enable sleep")
	}
}

func TestRecoverResumesAfterCrash(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, 8*time.Hour)
	h.lid.closed = true
	h.m.Tick(ctx)

	// Daemon crashes; something resets pmset meanwhile.
	h.sleep.disabled = false
	m2 := h.newManager("boot-1")
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	st := m2.Status()
	if !st.Active || !st.SeenClosed || !h.sleep.disabled {
		t.Fatalf("should resume with seenClosed kept: %+v", st)
	}

	h.lid.closed = false
	m2.Tick(ctx)
	if m2.Status().Active {
		t.Fatal("resumed session should end on lid open")
	}
}

func TestRecoverAfterReboot(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, 8*time.Hour)

	m2 := h.newManager("boot-2")
	_ = m2.Recover(ctx)
	if m2.Status().Active || h.sleep.disabled {
		t.Fatal("reboot should end session")
	}
	if m2.Status().LastEndReason != EndReboot {
		t.Fatal("wrong reason")
	}
}

func TestRecoverPastDeadline(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, time.Hour)
	h.clk.advance(2 * time.Hour)
	m2 := h.newManager("boot-1")
	_ = m2.Recover(ctx)
	if m2.Status().Active || h.sleep.disabled {
		t.Fatal("expired session should end on recover")
	}
}

func TestRecoverClearsStaleFlag(t *testing.T) {
	h := newHarness(t, "boot-1")
	h.sleep.disabled = true // left over, no session
	_ = h.m.Recover(ctx)
	if h.sleep.disabled {
		t.Fatal("recover without session should re-enable sleep")
	}
}

func TestClearRetriedOnTick(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, time.Hour)
	h.sleep.failSet = true
	if _, err := h.m.Stop(ctx); err == nil {
		t.Fatal("expected pmset error")
	}
	if !h.sleep.disabled {
		t.Fatal("fake should still be disabled")
	}
	h.sleep.failSet = false
	h.m.Tick(ctx)
	if h.sleep.disabled {
		t.Fatal("tick should retry re-enabling sleep")
	}
}

func TestLidReadErrorKeepsSession(t *testing.T) {
	h := newHarness(t, "boot-1")
	_, _ = h.m.Start(ctx, time.Hour)
	h.lid.err = errors.New("ioreg broke")
	h.m.Tick(ctx)
	if !h.m.Status().Active {
		t.Fatal("transient lid error should not end session")
	}
}
