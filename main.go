// Lidawake keeps a Mac awake with the lid closed, once.
// Sleep is re-enabled automatically when the lid is reopened.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ame180/lidawake/internal/daemon"
	"github.com/ame180/lidawake/internal/install"
	"github.com/ame180/lidawake/internal/power"
	"github.com/ame180/lidawake/internal/rpc"
	"github.com/ame180/lidawake/internal/session"
)

// version is set by -ldflags "-X main.version=..."; see buildVersion for the fallback.
var version = "dev"

const usage = `lidawake - stay awake with lid closed, until lid reopens

Usage:
  lidawake                        start with defaults, or show status if active
  lidawake start [-min-battery 20] [-ignore-lid] [-max 12h | duration]
                                  disable sleep until lid closes and reopens
                                  (or duration elapses, default 12h, or battery
                                  drops below min; -ignore-lid: only duration,
                                  battery, or stop end it)
  lidawake extend <duration>      push the deadline out, keeping other settings
  lidawake stop                   end session now, re-enable sleep
  lidawake status [-json]         show session and pmset state
  sudo lidawake install [-uid N]  install binary + launchd daemon
  sudo lidawake uninstall         remove everything, re-enable sleep
  lidawake version                show version (also -v, --version)
`

// Session defaults shared by start and bare lidawake.
const (
	defaultMax        = 12 * time.Hour
	defaultMinBattery = 20
)

func main() {
	var cmd string
	var args []string
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}

	var err error
	switch cmd {
	case "":
		err = runDefault()
	case "start":
		err = runStart(args)
	case "extend":
		err = runExtend(args)
	case "stop":
		err = runStop(args)
	case "status":
		err = runStatus(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = runUninstall(args)
	case "daemon":
		err = runDaemon(args)
	case "version", "-v", "--version":
		err = runVersion(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runDefault starts a session with default settings, or shows status if one
// is already running, so bare lidawake never replaces a session.
func runDefault() error {
	s, err := call(rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
	}
	if s.Active {
		fmt.Println("Already active.")
		printStatus(s)
		return nil
	}
	return startSession(startOptions{maxDuration: defaultMax, minBattery: defaultMinBattery}, false)
}

func runStart(args []string) error {
	opts, err := parseStartArgs(args)
	if err != nil {
		return err
	}
	before, err := call(rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
	}
	return startSession(opts, before.Active)
}

type startOptions struct {
	maxDuration time.Duration
	minBattery  int
	ignoreLid   bool
}

// parseStartArgs reads start's flags and its optional duration, which must
// come after the flags.
func parseStartArgs(args []string) (startOptions, error) {
	var opts startOptions
	fs := newFlagSet("start [-min-battery 20] [-ignore-lid] [-max 12h | duration]")
	fs.DurationVar(&opts.maxDuration, "max", defaultMax, "safety deadline, same as the duration argument; sleep re-enabled after this even if lid never reopens")
	fs.IntVar(&opts.minBattery, "min-battery", defaultMinBattery, "end session when on battery below this percent; 0 disables")
	fs.BoolVar(&opts.ignoreLid, "ignore-lid", false, "keep running when the lid reopens; only the deadline, battery, or stop end the session")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		return opts, nil
	}
	if flagGiven(fs, "max") {
		return opts, errors.New("start: give the duration once, as -max or as an argument")
	}
	d, err := time.ParseDuration(fs.Arg(0))
	if err != nil {
		return opts, fmt.Errorf("start: %w", err)
	}
	opts.maxDuration = d
	if fs.NArg() > 1 {
		if strings.HasPrefix(fs.Arg(1), "-") {
			return opts, fmt.Errorf("start: flags go before the duration, got %q after it", fs.Arg(1))
		}
		return opts, fmt.Errorf("start: unexpected argument %q", fs.Arg(1))
	}
	return opts, nil
}

// startSession asks the daemon for a new session and prints its summary.
// replacing notes that it took over an active session.
func startSession(opts startOptions, replacing bool) error {
	s, err := call(rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: int64(opts.maxDuration.Seconds()), MinBattery: opts.minBattery, IgnoreLid: opts.ignoreLid})
	if err != nil {
		return err
	}
	if replacing {
		fmt.Println("Replaced the active session.")
	}
	printSessionSummary("Sleep disabled.", "Close the lid; reopening it re-enables sleep.", s, formatDuration(opts.maxDuration))
	return nil
}

func runExtend(args []string) error {
	fs := newFlagSet("extend <duration>")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("extend: want exactly one duration, e.g. 2h")
	}
	by, err := time.ParseDuration(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("extend: %w", err)
	}

	secs := int64(by.Seconds())
	s, err := call(rpc.Request{Cmd: rpc.CmdExtend, MaxSeconds: secs})
	if err != nil {
		return err
	}
	printSessionSummary("Extended by "+formatDuration(time.Duration(secs)*time.Second)+".", "Reopening the lid re-enables sleep.", s,
		"in "+formatDuration(time.Until(s.Deadline).Round(time.Minute)))
	return nil
}

// printSessionSummary reminds the user what session start or extend left
// running.
func printSessionSummary(headline, watchedLidHint string, s rpc.Status, deadlineNote string) {
	lidHint := watchedLidHint
	if s.IgnoreLid {
		lidHint = "Lid ignored; runs until the deadline, battery cutoff, or stop."
	}
	fmt.Println(headline + " " + lidHint)
	fmt.Printf("Deadline: %s (%s)\n", s.Deadline.Local().Format("Mon 15:04"), deadlineNote)
	fmt.Printf("Battery cutoff: %s\n", batteryCutoff(s.MinBattery))
}

func runStop(args []string) error {
	if err := parseFlags(newFlagSet("stop"), args); err != nil {
		return err
	}
	before, err := call(rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
	}
	if _, err := call(rpc.Request{Cmd: rpc.CmdStop}); err != nil {
		return err
	}
	switch {
	case before.Active:
		fmt.Println("Session stopped. Sleep re-enabled.")
	case before.SleepDisabled:
		fmt.Println("No active session. Sleep was still disabled; re-enabled.")
	default:
		fmt.Println("No active session. Sleep already enabled.")
	}
	return nil
}

func runStatus(args []string) error {
	fs := newFlagSet("status [-json]")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	s, err := call(rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	printStatus(s)
	return nil
}

// call sends one request to the daemon; a refused request becomes an error.
func call(req rpc.Request) (rpc.Status, error) {
	resp, err := rpc.Call(context.Background(), install.SocketPath, req)
	if err != nil {
		return rpc.Status{}, err
	}
	if !resp.OK {
		return resp.Status, errors.New(resp.Error)
	}
	return resp.Status, nil
}

func printStatus(s rpc.Status) {
	if s.Active {
		phase := "waiting for lid to close"
		switch {
		case s.IgnoreLid:
			phase = "lid ignored"
		case s.SeenClosed:
			phase = "lid closed, waiting for reopen"
		}
		fmt.Printf("Session:   active (%s)\n", phase)
		fmt.Printf("Started:   %s\n", s.StartedAt.Local().Format("Mon 15:04"))
		fmt.Printf("Deadline:  %s (in %s)\n", s.Deadline.Local().Format("Mon 15:04"),
			formatDuration(time.Until(s.Deadline).Round(time.Minute)))
		printBattery(s)
		fmt.Printf("Cutoff:    %s\n", batteryCutoff(s.MinBattery))
	} else {
		fmt.Println("Session:   none")
		if s.LastEndReason != "" {
			fmt.Printf("Last end:  %s at %s\n", s.LastEndReason, s.LastEndedAt.Local().Format("Mon 15:04"))
		}
		printBattery(s)
	}
	fmt.Printf("pmset:     SleepDisabled=%d\n", boolInt(s.SleepDisabled))
	if !s.Active && s.SleepDisabled {
		fmt.Println("WARNING: sleep disabled outside a session. Fix: lidawake stop")
	}
}

// formatDuration prints d without trailing zero units: 12h, 1h30m, 45s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// printBattery prints the charge and power source, if a battery was read.
func printBattery(s rpc.Status) {
	if s.BatteryPercent <= 0 {
		return
	}
	source := "on AC power"
	if s.OnBattery {
		source = "on battery"
	}
	fmt.Printf("Battery:   %d%% (%s)\n", s.BatteryPercent, source)
}

func batteryCutoff(minBattery int) string {
	if minBattery <= 0 {
		return "off"
	}
	return fmt.Sprintf("below %d%% on battery", minBattery)
}

func runInstall(args []string) error {
	fs := newFlagSet("install [-uid N]")
	uid := fs.Int("uid", -1, "user allowed to run start/stop/status without sudo (default: $SUDO_UID)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if *uid < 0 {
		u, err := install.TargetUID()
		if err != nil {
			return err
		}
		*uid = u
	}
	return install.Install(context.Background(), *uid, os.Stdout)
}

func runUninstall(args []string) error {
	if err := parseFlags(newFlagSet("uninstall"), args); err != nil {
		return err
	}
	return install.Uninstall(context.Background(), os.Stdout)
}

func runVersion(args []string) error {
	if err := parseFlags(newFlagSet("version"), args); err != nil {
		return err
	}
	fmt.Println(buildVersion())
	return nil
}

func runDaemon(args []string) error {
	fs := newFlagSet("daemon [flags]")
	allowUID := fs.Int("allow-uid", -1, "uid that owns the control socket")
	socket := fs.String("socket", install.SocketPath, "control socket path")
	statePath := fs.String("state", install.StatePath, "state file path")
	poll := fs.Duration("poll", 5*time.Second, "lid poll interval")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return errors.New("daemon must run as root")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mac := power.MacOS{}
	boot, err := power.BootSessionID(ctx)
	if err != nil {
		logger.Warn("boot session id unavailable; sessions will not survive a daemon restart", "err", err)
	}

	mgr := session.NewManager(session.Config{
		Store:       session.FileStore{Path: *statePath},
		Lid:         mac,
		Sleep:       mac,
		Battery:     mac,
		BootSession: boot,
		Log:         logger,
	})
	d := daemon.New(daemon.Config{
		SocketPath:   *socket,
		AllowUID:     *allowUID,
		PollInterval: *poll,
		MaxAllowed:   72 * time.Hour,
	}, mgr, mac, logger)

	logger.Info("lidawake daemon starting", "version", buildVersion())
	return d.Run(ctx)
}

// newFlagSet returns a flag set whose -h output starts with the given synopsis,
// e.g. "status [-json]"; the first word is the subcommand name.
func newFlagSet(synopsis string) *flag.FlagSet {
	name, _, _ := strings.Cut(synopsis, " ")
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: lidawake %s\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// flagGiven reports whether the named flag was set on the command line.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// parseFlags parses args and rejects leftover positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) error {
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

// buildVersion falls back to the module version recorded by `go install module@version`.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version
	}
	return info.Main.Version
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
