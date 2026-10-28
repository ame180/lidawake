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
  lidawake start [-max 12h] [-min-battery 20] [-ignore-lid]
                                  disable sleep until lid closes and reopens
                                  (or max elapses, or battery drops below min;
                                  -ignore-lid: only max, battery, or stop)
  lidawake stop                   end session now, re-enable sleep
  lidawake status [-json]         show session and pmset state
  sudo lidawake install [-uid N]  install binary + launchd daemon
  sudo lidawake uninstall         remove everything, re-enable sleep
  lidawake version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "start":
		err = runStart(args)
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
	case "version":
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

func runStart(args []string) error {
	fs := newFlagSet("start [-max 12h] [-min-battery 20] [-ignore-lid]")
	maxDuration := fs.Duration("max", 12*time.Hour, "safety deadline; sleep re-enabled after this even if lid never reopens")
	minBattery := fs.Int("min-battery", 20, "end session when on battery below this percent; 0 disables")
	ignoreLid := fs.Bool("ignore-lid", false, "keep running when the lid reopens; only max, battery, or stop end the session")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	resp, err := rpc.Call(context.Background(), install.SocketPath, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: int64(maxDuration.Seconds()), MinBattery: *minBattery, IgnoreLid: *ignoreLid})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if *ignoreLid {
		fmt.Println("Sleep disabled. Lid ignored; runs until the deadline or stop.")
	} else {
		fmt.Println("Sleep disabled. Close the lid; reopening it re-enables sleep.")
	}
	fmt.Printf("Deadline: %s (%s)\n", resp.Status.Deadline.Local().Format("Mon 15:04"), *maxDuration)
	fmt.Printf("Battery cutoff: %s\n", batteryCutoff(resp.Status.MinBattery))
	return nil
}

func runStop(args []string) error {
	if err := parseFlags(newFlagSet("stop"), args); err != nil {
		return err
	}
	resp, err := rpc.Call(context.Background(), install.SocketPath, rpc.Request{Cmd: rpc.CmdStop})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	fmt.Println("Session stopped. Sleep re-enabled.")
	return nil
}

func runStatus(args []string) error {
	fs := newFlagSet("status [-json]")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	resp, err := rpc.Call(context.Background(), install.SocketPath, rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Status)
	}
	printStatus(resp.Status)
	return nil
}

func printStatus(s rpc.Status) {
	if s.Active {
		phase := "waiting for lid to close"
		switch {
		case s.IgnoreLid:
			phase = "lid ignored, waiting for deadline"
		case s.SeenClosed:
			phase = "lid closed, waiting for reopen"
		}
		fmt.Printf("Session:   active (%s)\n", phase)
		fmt.Printf("Started:   %s\n", s.StartedAt.Local().Format("Mon 15:04"))
		fmt.Printf("Deadline:  %s (in %s)\n", s.Deadline.Local().Format("Mon 15:04"),
			time.Until(s.Deadline).Round(time.Minute))
		fmt.Printf("Battery:   cutoff %s\n", batteryCutoff(s.MinBattery))
	} else {
		fmt.Println("Session:   none")
		if s.LastEndReason != "" {
			fmt.Printf("Last end:  %s at %s\n", s.LastEndReason, s.LastEndedAt.Local().Format("Mon 15:04"))
		}
	}
	fmt.Printf("pmset:     SleepDisabled=%d\n", boolInt(s.SleepDisabled))
	if !s.Active && s.SleepDisabled {
		fmt.Println("WARNING: sleep disabled outside a session. Fix: sudo pmset -a disablesleep 0")
	}
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
// e.g. "start [-max 12h]"; the first word is the subcommand name.
func newFlagSet(synopsis string) *flag.FlagSet {
	name, _, _ := strings.Cut(synopsis, " ")
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: lidawake %s\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
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
