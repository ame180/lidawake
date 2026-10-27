// Package session holds the one-shot "stay awake until lid reopens" state machine.
//
// Lifecycle: Start -> (lid closed: SeenClosed=true) -> lid opened -> End.
// Deadline, Stop, or a reboot also end the session. Every transition is
// persisted before the matching pmset change, so a crash at any point
// recovers to a consistent state on the next daemon start.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ame180/lidawake/internal/power"
)

const (
	EndLidOpened = "lid-opened"
	EndDeadline  = "deadline"
	EndStopped   = "stopped"
	EndReboot    = "reboot"
)

type State struct {
	Active      bool      `json:"active"`
	SeenClosed  bool      `json:"seenClosed"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	Deadline    time.Time `json:"deadline,omitzero"`
	BootSession string    `json:"bootSession,omitempty"`

	LastEndReason string    `json:"lastEndReason,omitempty"`
	LastEndedAt   time.Time `json:"lastEndedAt,omitzero"`
}

var ErrNotActive = errors.New("no active session")

type Manager struct {
	mu          sync.Mutex
	state       State
	store       Store
	lid         power.Lid
	sleep       power.SleepControl
	now         func() time.Time
	bootSession string
	log         *slog.Logger

	// needClear is set when pmset failed to re-enable sleep; Tick retries.
	needClear bool
}

type Config struct {
	Store       Store
	Lid         power.Lid
	Sleep       power.SleepControl
	Now         func() time.Time
	BootSession string
	Log         *slog.Logger
}

func NewManager(c Config) *Manager {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return &Manager{
		store:       c.Store,
		lid:         c.Lid,
		sleep:       c.Sleep,
		now:         c.Now,
		bootSession: c.BootSession,
		log:         c.Log,
	}
}

// Recover loads persisted state and makes pmset match it.
// Called once on daemon start, before serving requests.
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, err := m.store.Load()
	if err != nil {
		// Corrupt state: fail safe, sleep back on.
		m.log.Error("state unreadable, resetting", "err", err)
		st = State{}
	}
	m.state = st

	switch {
	case !st.Active:
		m.log.Info("no active session, ensuring sleep enabled")
		return m.clearSleepLocked(ctx)
	case st.BootSession != "" && m.bootSession != "" && st.BootSession != m.bootSession:
		m.log.Info("session from previous boot, ending")
		return m.endLocked(ctx, EndReboot)
	case !m.now().Before(st.Deadline):
		m.log.Info("session deadline passed while daemon was down, ending")
		return m.endLocked(ctx, EndDeadline)
	default:
		m.log.Info("resuming active session", "seenClosed", st.SeenClosed, "deadline", st.Deadline)
		return m.sleep.SetSleepDisabled(ctx, true)
	}
}

// Start begins a new session, replacing any active one.
func (m *Manager) Start(ctx context.Context, maxDuration time.Duration) (State, error) {
	if maxDuration <= 0 {
		return State{}, errors.New("max duration must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	next := m.state
	next.Active = true
	next.SeenClosed = false
	next.StartedAt = now
	next.Deadline = now.Add(maxDuration)
	next.BootSession = m.bootSession

	if err := m.store.Save(next); err != nil {
		return m.state, err
	}
	m.state = next
	m.needClear = false
	if err := m.sleep.SetSleepDisabled(ctx, true); err != nil {
		// Roll back so state does not claim sleep is disabled.
		_ = m.endLocked(ctx, EndStopped)
		return m.state, err
	}
	m.log.Info("session started", "deadline", next.Deadline)
	return m.state, nil
}

// Stop ends the active session.
func (m *Manager) Stop(ctx context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Active {
		return m.state, ErrNotActive
	}
	err := m.endLocked(ctx, EndStopped)
	return m.state, err
}

// Tick runs one poll step. Call periodically.
func (m *Manager) Tick(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.needClear {
		if err := m.clearSleepLocked(ctx); err != nil {
			m.log.Error("retry re-enable sleep failed", "err", err)
		}
	}
	if !m.state.Active {
		return
	}
	if !m.now().Before(m.state.Deadline) {
		m.logEnd(m.endLocked(ctx, EndDeadline), EndDeadline)
		return
	}

	closed, err := m.lid.IsClosed(ctx)
	if err != nil {
		m.log.Warn("lid state read failed", "err", err)
		return
	}
	switch {
	case closed && !m.state.SeenClosed:
		next := m.state
		next.SeenClosed = true
		if err := m.store.Save(next); err != nil {
			m.log.Error("persist seenClosed failed", "err", err)
			return
		}
		m.state = next
		m.log.Info("lid closed")
	case !closed && m.state.SeenClosed:
		m.logEnd(m.endLocked(ctx, EndLidOpened), EndLidOpened)
	}
}

func (m *Manager) Status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// endLocked persists the end first, then re-enables sleep.
// Crash between the two: Recover sees inactive state and clears sleep.
func (m *Manager) endLocked(ctx context.Context, reason string) error {
	next := m.state
	next.Active = false
	next.SeenClosed = false
	next.LastEndReason = reason
	next.LastEndedAt = m.now()
	if err := m.store.Save(next); err != nil {
		m.log.Error("persist end failed", "err", err)
		// Still re-enable sleep; safety beats bookkeeping.
	}
	m.state = next
	return m.clearSleepLocked(ctx)
}

func (m *Manager) clearSleepLocked(ctx context.Context) error {
	if err := m.sleep.SetSleepDisabled(ctx, false); err != nil {
		m.needClear = true
		return err
	}
	m.needClear = false
	return nil
}

func (m *Manager) logEnd(err error, reason string) {
	if err != nil {
		m.log.Error("session ended but re-enabling sleep failed, will retry", "reason", reason, "err", err)
		return
	}
	m.log.Info("session ended", "reason", reason)
}
