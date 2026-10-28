// Package session holds the one-shot "stay awake until lid reopens" state machine.
//
// Lifecycle: Start -> (lid closed: SeenClosed=true) -> lid opened -> End.
// Deadline, low battery, Stop, or a reboot also end the session. With
// IgnoreLid, the lid is not watched and only those end it. Every transition is
// persisted before the matching pmset change, so a crash at any point
// recovers to a consistent state on the next daemon start.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ame180/lidawake/internal/power"
)

// EndReason records why a session ended.
type EndReason string

// End reasons stored in State.LastEndReason.
const (
	EndLidOpened   EndReason = "lid-opened"
	EndDeadline    EndReason = "deadline"
	EndLowBattery  EndReason = "low-battery"
	EndStopped     EndReason = "stopped"
	EndReboot      EndReason = "reboot"
	EndStartFailed EndReason = "start-failed"
)

// Lid reports whether the laptop lid is closed.
type Lid interface {
	IsClosed(ctx context.Context) (bool, error)
}

// SleepSetter sets the system-wide "disablesleep" flag.
type SleepSetter interface {
	SetSleepDisabled(ctx context.Context, disabled bool) error
}

// Battery reports the battery charge and whether the machine runs on it.
type Battery interface {
	Battery(ctx context.Context) (percent int, onBattery bool, err error)
}

// State is the persisted session state.
type State struct {
	Active      bool      `json:"active"`
	SeenClosed  bool      `json:"seenClosed"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	Deadline    time.Time `json:"deadline,omitzero"`
	BootSession string    `json:"bootSession,omitempty"`
	// MinBattery ends the session when on battery below this percent; 0 disables.
	MinBattery int `json:"minBattery,omitempty"`
	// IgnoreLid keeps the session running through lid open/close.
	IgnoreLid bool `json:"ignoreLid,omitempty"`

	LastEndReason EndReason `json:"lastEndReason,omitempty"`
	LastEndedAt   time.Time `json:"lastEndedAt,omitzero"`
}

// ErrNotActive is returned by Stop when no session is running.
var ErrNotActive = errors.New("no active session")

// Manager runs the session state machine. It is safe for concurrent use.
type Manager struct {
	mu          sync.Mutex
	state       State
	store       Store
	lid         Lid
	sleep       SleepSetter
	battery     Battery
	now         func() time.Time
	bootSession string
	log         *slog.Logger

	// needClear is set when pmset failed to re-enable sleep; Tick retries.
	needClear bool
	// needSave is set when the in-memory state failed to persist; Tick retries.
	needSave bool
}

// Config holds Manager dependencies. Now and Log are optional.
type Config struct {
	Store   Store
	Lid     Lid
	Sleep   SleepSetter
	Battery Battery
	Now     func() time.Time
	// BootSession identifies the current boot; see power.BootSessionID.
	BootSession string
	Log         *slog.Logger
}

// NewManager returns a Manager with no loaded state; call Recover first.
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
		battery:     c.Battery,
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
	case st.BootSession == "" || m.bootSession == "" || st.BootSession != m.bootSession:
		// Unknown boot ID on either side counts as a reboot: fail safe.
		m.log.Info("session from previous or unknown boot, ending")
		return m.endLocked(ctx, EndReboot)
	case !m.now().Before(st.Deadline):
		m.log.Info("session deadline passed while daemon was down, ending")
		return m.endLocked(ctx, EndDeadline)
	default:
		m.log.Info("resuming active session", "seenClosed", st.SeenClosed, "deadline", st.Deadline)
		return m.sleep.SetSleepDisabled(ctx, true)
	}
}

// Start begins a new session, replacing any active one. minBattery is the
// cutoff percent on battery power; 0 disables it. ignoreLid skips lid
// tracking, so only the deadline, battery, or Stop end the session.
func (m *Manager) Start(ctx context.Context, maxDuration time.Duration, minBattery int, ignoreLid bool) (State, error) {
	if maxDuration <= 0 {
		return State{}, errors.New("max duration must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.lid.IsClosed(ctx); errors.Is(err, power.ErrNoClamshell) {
		return m.state, err
	}
	if percent, low := m.batteryLow(ctx, minBattery); low {
		return m.state, fmt.Errorf("battery at %d%%, below cutoff %d%% (plug in or lower -min-battery)", percent, minBattery)
	}

	now := m.now()
	next := m.state
	next.Active = true
	next.SeenClosed = false
	next.StartedAt = now
	next.Deadline = now.Add(maxDuration)
	next.BootSession = m.bootSession
	next.MinBattery = minBattery
	next.IgnoreLid = ignoreLid

	if err := m.saveLocked(next); err != nil {
		return m.state, err
	}
	m.state = next
	m.needClear = false
	if err := m.sleep.SetSleepDisabled(ctx, true); err != nil {
		// Roll back so state does not claim sleep is disabled.
		_ = m.endLocked(ctx, EndStartFailed)
		return m.state, err
	}
	m.log.Info("session started", "deadline", next.Deadline, "minBattery", minBattery, "ignoreLid", ignoreLid)
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
	if m.needSave {
		if err := m.saveLocked(m.state); err != nil {
			m.log.Error("retry persist state failed", "err", err)
		}
	}
	if !m.state.Active {
		return
	}
	if !m.now().Before(m.state.Deadline) {
		m.logEnd(m.endLocked(ctx, EndDeadline), EndDeadline)
		return
	}
	if _, low := m.batteryLow(ctx, m.state.MinBattery); low {
		m.logEnd(m.endLocked(ctx, EndLowBattery), EndLowBattery)
		return
	}
	if m.state.IgnoreLid {
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
		if err := m.saveLocked(next); err != nil {
			m.log.Error("persist seenClosed failed", "err", err)
			return
		}
		m.state = next
		m.log.Info("lid closed")
	case !closed && m.state.SeenClosed:
		m.logEnd(m.endLocked(ctx, EndLidOpened), EndLidOpened)
	}
}

// Status returns a snapshot of the current state.
func (m *Manager) Status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// batteryLow reports whether the machine is on battery below minBattery
// percent. A read failure is logged and counts as not low; the deadline
// still bounds the session.
func (m *Manager) batteryLow(ctx context.Context, minBattery int) (percent int, low bool) {
	if minBattery <= 0 {
		return 0, false
	}
	percent, onBattery, err := m.battery.Battery(ctx)
	if err != nil {
		m.log.Warn("battery read failed", "err", err)
		return 0, false
	}
	return percent, onBattery && percent < minBattery
}

// endLocked persists the end first, then re-enables sleep.
// Crash between the two: Recover sees inactive state and clears sleep.
func (m *Manager) endLocked(ctx context.Context, reason EndReason) error {
	next := m.state
	next.Active = false
	next.SeenClosed = false
	next.LastEndReason = reason
	next.LastEndedAt = m.now()
	if err := m.saveLocked(next); err != nil {
		m.log.Error("persist end failed, will retry", "err", err)
		// Still re-enable sleep; safety beats bookkeeping.
		m.needSave = true
	}
	m.state = next
	return m.clearSleepLocked(ctx)
}

// saveLocked persists st. On success the store matches st, so any pending
// retry of an earlier failed save is no longer needed.
func (m *Manager) saveLocked(st State) error {
	if err := m.store.Save(st); err != nil {
		return err
	}
	m.needSave = false
	return nil
}

func (m *Manager) clearSleepLocked(ctx context.Context) error {
	if err := m.sleep.SetSleepDisabled(ctx, false); err != nil {
		m.needClear = true
		return err
	}
	m.needClear = false
	return nil
}

func (m *Manager) logEnd(err error, reason EndReason) {
	if err != nil {
		m.log.Error("session ended but re-enabling sleep failed, will retry", "reason", reason, "err", err)
		return
	}
	m.log.Info("session ended", "reason", reason)
}
