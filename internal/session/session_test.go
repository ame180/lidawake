package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ame180/lidawake/internal/power"
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

// flakyStore wraps FileStore and can be told to fail Save.
type flakyStore struct {
	FileStore
	failSave bool
}

func (s *flakyStore) Save(st State) error {
	if s.failSave {
		return errors.New("disk full")
	}
	return s.FileStore.Save(st)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	m     *Manager
	lid   *fakeLid
	sleep *fakeSleep
	clk   *clock
	store *flakyStore
}

func newHarness(t *testing.T, boot string) *harness {
	t.Helper()
	h := &harness{
		lid:   &fakeLid{},
		sleep: &fakeSleep{},
		clk:   &clock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)},
		store: &flakyStore{FileStore: FileStore{Path: filepath.Join(t.TempDir(), "state.json")}},
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

func (h *harness) start(t *testing.T, maxDuration time.Duration) {
	t.Helper()
	if _, err := h.m.Start(t.Context(), maxDuration); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestFullCycle(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, 8*time.Hour)
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
		t.Fatalf("reason = %q, want %q", st.LastEndReason, EndLidOpened)
	}
}

func TestDeadline(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, time.Hour)
	h.lid.closed = true
	h.m.Tick(ctx)
	h.clk.advance(time.Hour)
	h.m.Tick(ctx)
	st := h.m.Status()
	if st.Active || h.sleep.disabled {
		t.Fatal("deadline should end session")
	}
	if st.LastEndReason != EndDeadline {
		t.Fatalf("reason = %q, want %q", st.LastEndReason, EndDeadline)
	}
}

func TestStop(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	if _, err := h.m.Stop(ctx); !errors.Is(err, ErrNotActive) {
		t.Fatalf("stop without session: %v", err)
	}
	h.start(t, time.Hour)
	if _, err := h.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if h.sleep.disabled {
		t.Fatal("stop should re-enable sleep")
	}
}

func TestStartReplacesActiveSession(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, time.Hour)
	h.lid.closed = true
	h.m.Tick(ctx)

	h.clk.advance(10 * time.Minute)
	h.start(t, 2*time.Hour)
	st := h.m.Status()
	if !st.Active || st.SeenClosed {
		t.Fatalf("restart should reset seenClosed: %+v", st)
	}
	if want := h.clk.t.Add(2 * time.Hour); !st.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", st.Deadline, want)
	}
}

func TestStartRollsBackWhenPmsetFails(t *testing.T) {
	h := newHarness(t, "boot-1")
	h.sleep.failSet = true
	if _, err := h.m.Start(t.Context(), time.Hour); err == nil {
		t.Fatal("expected pmset error")
	}
	st := h.m.Status()
	if st.Active || st.LastEndReason != EndStartFailed {
		t.Fatalf("failed start should roll back: %+v", st)
	}
}

func TestStartRejectsNonLaptop(t *testing.T) {
	h := newHarness(t, "boot-1")
	h.lid.err = power.ErrNoClamshell
	if _, err := h.m.Start(t.Context(), time.Hour); !errors.Is(err, power.ErrNoClamshell) {
		t.Fatalf("err = %v, want ErrNoClamshell", err)
	}
	if h.m.Status().Active || h.sleep.disabled {
		t.Fatal("non-laptop start should not disable sleep")
	}
}

// TestRecover covers the crash/restart table in README.md.
func TestRecover(t *testing.T) {
	cases := []struct {
		name         string
		startFor     time.Duration // 0 = no session before the restart
		staleFlag    bool          // sleep left disabled with no session
		advance      time.Duration
		bootBefore   string
		bootAfter    string
		wantActive   bool
		wantReason   EndReason
		wantDisabled bool
	}{
		{name: "no session clears stale flag", staleFlag: true, bootBefore: "boot-1", bootAfter: "boot-1"},
		{name: "different boot ends", startFor: 8 * time.Hour, bootBefore: "boot-1", bootAfter: "boot-2", wantReason: EndReboot},
		{name: "unknown boot ends", startFor: 8 * time.Hour, bootBefore: "boot-1", bootAfter: "", wantReason: EndReboot},
		{name: "deadline passed ends", startFor: time.Hour, advance: 2 * time.Hour, bootBefore: "boot-1", bootAfter: "boot-1", wantReason: EndDeadline},
		{name: "same boot resumes", startFor: 8 * time.Hour, bootBefore: "boot-1", bootAfter: "boot-1", wantActive: true, wantDisabled: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.bootBefore)
			if c.startFor > 0 {
				h.start(t, c.startFor)
			}
			// Daemon goes down; pmset may be changed meanwhile.
			h.sleep.disabled = c.staleFlag
			h.clk.advance(c.advance)

			m2 := h.newManager(c.bootAfter)
			if err := m2.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			st := m2.Status()
			if st.Active != c.wantActive || st.LastEndReason != c.wantReason || h.sleep.disabled != c.wantDisabled {
				t.Fatalf("got active=%v reason=%q disabled=%v, want active=%v reason=%q disabled=%v",
					st.Active, st.LastEndReason, h.sleep.disabled, c.wantActive, c.wantReason, c.wantDisabled)
			}
		})
	}
}

func TestRecoverKeepsSeenClosed(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, 8*time.Hour)
	h.lid.closed = true
	h.m.Tick(ctx)

	m2 := h.newManager("boot-1")
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !m2.Status().SeenClosed {
		t.Fatal("resume should keep seenClosed")
	}
	h.lid.closed = false
	m2.Tick(ctx)
	if m2.Status().Active {
		t.Fatal("resumed session should end on lid open")
	}
}

func TestRecoverCorruptState(t *testing.T) {
	h := newHarness(t, "boot-1")
	if err := os.WriteFile(h.store.Path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.sleep.disabled = true
	if err := h.m.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.m.Status().Active || h.sleep.disabled {
		t.Fatal("corrupt state should reset and re-enable sleep")
	}
}

func TestClearRetriedOnTick(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, time.Hour)
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

func TestEndSaveRetriedOnTick(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, "boot-1")
	h.start(t, time.Hour)
	h.store.failSave = true
	if _, err := h.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	h.store.failSave = false
	h.m.Tick(ctx)

	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Active || st.LastEndReason != EndStopped {
		t.Fatalf("tick should persist the end: %+v", st)
	}
}

func TestLidReadErrorKeepsSession(t *testing.T) {
	h := newHarness(t, "boot-1")
	h.start(t, time.Hour)
	h.lid.err = errors.New("ioreg broke")
	h.m.Tick(t.Context())
	if !h.m.Status().Active {
		t.Fatal("transient lid error should not end session")
	}
}
