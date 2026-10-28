// Package power wraps the macOS tools that read lid state and toggle sleep.
package power

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// commandTimeout bounds each ioreg/pmset/sysctl call so a hung tool cannot
// stall the daemon while it holds the session lock.
const commandTimeout = 10 * time.Second

// ErrNoClamshell means the machine reports no lid (not a laptop).
var ErrNoClamshell = errors.New("AppleClamshellState not found (not a laptop?)")

var (
	clamshellRe      = regexp.MustCompile(`"AppleClamshellState"\s*=\s*(Yes|No)`)
	sleepDisabledRe  = regexp.MustCompile(`(?m)^\s*SleepDisabled\s+(\d)`)
	powerSourceRe    = regexp.MustCompile(`Now drawing from '([^']+)'`)
	batteryPercentRe = regexp.MustCompile(`(\d+)%;`)
)

// MacOS reads lid state and controls sleep using ioreg and pmset.
type MacOS struct{}

// IsClosed reports whether the laptop lid is closed.
func (MacOS) IsClosed(ctx context.Context) (bool, error) {
	out, err := output(ctx, "/usr/sbin/ioreg", "-r", "-k", "AppleClamshellState", "-d", "4")
	if err != nil {
		return false, fmt.Errorf("ioreg: %w", err)
	}
	return parseClamshellState(out)
}

// SetSleepDisabled sets the system-wide "disablesleep" flag.
func (MacOS) SetSleepDisabled(ctx context.Context, disabled bool) error {
	value := "0"
	if disabled {
		value = "1"
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-a", "disablesleep", value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pmset -a disablesleep %s: %w: %s", value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SleepDisabled reports the current system-wide "disablesleep" flag.
func (MacOS) SleepDisabled(ctx context.Context) (bool, error) {
	out, err := output(ctx, "/usr/bin/pmset", "-g")
	if err != nil {
		return false, fmt.Errorf("pmset -g: %w", err)
	}
	return parseSleepDisabled(out), nil
}

// Battery reports the battery charge in percent and whether the machine
// is running on battery power.
func (MacOS) Battery(ctx context.Context) (percent int, onBattery bool, err error) {
	out, err := output(ctx, "/usr/bin/pmset", "-g", "batt")
	if err != nil {
		return 0, false, fmt.Errorf("pmset -g batt: %w", err)
	}
	return parseBattery(out)
}

// BootSessionID returns an identifier that changes on every boot.
func BootSessionID(ctx context.Context) (string, error) {
	out, err := output(ctx, "/usr/sbin/sysctl", "-n", "kern.bootsessionuuid")
	if err != nil {
		return "", fmt.Errorf("sysctl kern.bootsessionuuid: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// parseClamshellState extracts lid state from ioreg output. true = closed.
func parseClamshellState(out []byte) (bool, error) {
	m := clamshellRe.FindSubmatch(out)
	if m == nil {
		return false, ErrNoClamshell
	}
	return string(m[1]) == "Yes", nil
}

// parseSleepDisabled extracts SleepDisabled from `pmset -g` output.
// Missing line is treated as not disabled.
func parseSleepDisabled(out []byte) bool {
	m := sleepDisabledRe.FindSubmatch(out)
	return m != nil && string(m[1]) == "1"
}

// parseBattery extracts charge percent and power source from `pmset -g batt`
// output. Only an exact 'Battery Power' source counts as on battery.
func parseBattery(out []byte) (percent int, onBattery bool, err error) {
	source := powerSourceRe.FindSubmatch(out)
	if source == nil {
		return 0, false, errors.New("pmset -g batt: power source not found")
	}
	charge := batteryPercentRe.FindSubmatch(out)
	if charge == nil {
		return 0, false, errors.New("pmset -g batt: battery percent not found")
	}
	percent, err = strconv.Atoi(string(charge[1]))
	if err != nil {
		return 0, false, fmt.Errorf("pmset -g batt: %w", err)
	}
	return percent, string(source[1]) == "Battery Power", nil
}
