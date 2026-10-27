// Package power wraps the macOS tools that read lid state and toggle sleep.
package power

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Lid reports whether the laptop lid is closed.
type Lid interface {
	IsClosed(ctx context.Context) (bool, error)
}

// SleepControl reads and sets the system-wide "disablesleep" flag.
type SleepControl interface {
	SetSleepDisabled(ctx context.Context, disabled bool) error
	SleepDisabled(ctx context.Context) (bool, error)
}

// MacOS implements Lid and SleepControl using ioreg and pmset.
type MacOS struct{}

var (
	clamshellRe     = regexp.MustCompile(`"AppleClamshellState"\s*=\s*(Yes|No)`)
	sleepDisabledRe = regexp.MustCompile(`(?m)^\s*SleepDisabled\s+(\d)`)

	ErrNoClamshell = errors.New("AppleClamshellState not found (not a laptop?)")
)

func (MacOS) IsClosed(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-r", "-k", "AppleClamshellState", "-d", "4").Output()
	if err != nil {
		return false, fmt.Errorf("ioreg: %w", err)
	}
	return ParseClamshellState(out)
}

func (MacOS) SetSleepDisabled(ctx context.Context, disabled bool) error {
	value := "0"
	if disabled {
		value = "1"
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-a", "disablesleep", value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pmset -a disablesleep %s: %w: %s", value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (MacOS) SleepDisabled(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g").Output()
	if err != nil {
		return false, fmt.Errorf("pmset -g: %w", err)
	}
	return ParseSleepDisabled(out), nil
}

// BootSessionID returns an identifier that changes on every boot.
func BootSessionID(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "kern.bootsessionuuid").Output()
	if err != nil {
		return "", fmt.Errorf("sysctl kern.bootsessionuuid: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ParseClamshellState extracts lid state from ioreg output. true = closed.
func ParseClamshellState(out []byte) (bool, error) {
	m := clamshellRe.FindSubmatch(out)
	if m == nil {
		return false, ErrNoClamshell
	}
	return string(m[1]) == "Yes", nil
}

// ParseSleepDisabled extracts SleepDisabled from `pmset -g` output.
// Missing line is treated as not disabled.
func ParseSleepDisabled(out []byte) bool {
	m := sleepDisabledRe.FindSubmatch(out)
	return m != nil && string(m[1]) == "1"
}
