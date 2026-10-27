// lidawake: keep a Mac awake with the lid closed, once.
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
  lidawake start [-max 12h]   disable sleep until lid closes and reopens (or max elapses)
  lidawake stop               end session now, re-enable sleep
  lidawake status [-json]     show session and pmset state
  sudo lidawake install       install binary + launchd daemon
  sudo lidawake uninstall     remove everything, re-enable sleep
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
		err = runStop()
	case "status":
		err = runStatus(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = install.Uninstall(context.Background(), os.Stdout)
	case "daemon":
		err = runDaemon(args)
	case "version":
		fmt.Println(buildVersion())
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
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	max := fs.Duration("max", 12*time.Hour, "safety deadline; sleep re-enabled after this even if lid never reopens")
	_ = fs.Parse(args)

	resp, err := rpc.Call(install.SocketPath, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: int64(max.Seconds())})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	fmt.Printf("Sleep disabled. Close the lid; reopening it re-enables sleep.\nDeadline: %s (%s)\n",
		resp.Status.Deadline.Local().Format("Mon 15:04"), max)
	return nil
}

func runStop() error {
	resp, err := rpc.Call(install.SocketPath, rpc.Request{Cmd: rpc.CmdStop})
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
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)

	resp, err := rpc.Call(install.SocketPath, rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		return err
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
		if s.SeenClosed {
			phase = "lid closed, waiting for reopen"
		}
		fmt.Printf("Session:   active (%s)\n", phase)
		fmt.Printf("Started:   %s\n", s.StartedAt.Local().Format("Mon 15:04"))
		fmt.Printf("Deadline:  %s (in %s)\n", s.Deadline.Local().Format("Mon 15:04"),
			time.Until(s.Deadline).Round(time.Minute))
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

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	uid := fs.Int("uid", -1, "user allowed to run start/stop/status without sudo (default: $SUDO_UID)")
	_ = fs.Parse(args)

	if *uid < 0 {
		u, err := install.TargetUID()
		if err != nil {
			return err
		}
		*uid = u
	}
	return install.Install(context.Background(), *uid, os.Stdout)
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	allowUID := fs.Int("allow-uid", -1, "uid that owns the control socket")
	socket := fs.String("socket", install.SocketPath, "control socket path")
	statePath := fs.String("state", install.StatePath, "state file path")
	poll := fs.Duration("poll", 5*time.Second, "lid poll interval")
	_ = fs.Parse(args)

	if os.Geteuid() != 0 {
		return errors.New("daemon must run as root")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mac := power.MacOS{}
	boot, err := power.BootSessionID(ctx)
	if err != nil {
		log.Warn("boot session id unavailable, reboot detection off", "err", err)
	}

	mgr := session.NewManager(session.Config{
		Store:       session.FileStore{Path: *statePath},
		Lid:         mac,
		Sleep:       mac,
		BootSession: boot,
		Log:         log,
	})
	d := daemon.New(daemon.Config{
		SocketPath:   *socket,
		AllowUID:     *allowUID,
		PollInterval: *poll,
		MaxAllowed:   72 * time.Hour,
	}, mgr, mac, log)

	log.Info("lidawake daemon starting", "version", buildVersion())
	return d.Run(ctx)
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
