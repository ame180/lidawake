// Package install places the binary and launchd plist, and removes them.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"
	"time"

	"github.com/ame180/lidawake/internal/power"
)

// Paths shared with the daemon and CLI.
const (
	SocketPath = "/var/run/lidawake.sock"
	StatePath  = stateDir + "/state.json"
)

const (
	label     = "dev.lidawake.daemon"
	binPath   = "/usr/local/bin/lidawake"
	plistPath = "/Library/LaunchDaemons/" + label + ".plist"
	stateDir  = "/var/db/lidawake"
	logPath   = "/var/log/lidawake.log"
)

// ErrNotRoot is returned when install or uninstall runs without root.
var ErrNotRoot = errors.New("must run as root (use sudo)")

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Bin}}</string>
		<string>daemon</string>
		<string>-allow-uid</string>
		<string>{{.UID}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{.Log}}</string>
</dict>
</plist>
`))

// Install copies the running binary into place and (re)loads the daemon.
// allowUID is the user allowed to use the CLI without sudo.
//
// Files are written while the old daemon keeps running, so a failure there
// leaves the existing install untouched.
func Install(ctx context.Context, allowUID int, out io.Writer) error {
	if err := requireRoot(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	if self != binPath {
		if err := copyFile(self, binPath, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
		fmt.Fprintf(out, "binary  -> %s\n", binPath)
	}

	plist, err := renderPlist(allowUID)
	if err != nil {
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if err := os.Chown(plistPath, 0, 0); err != nil {
		return fmt.Errorf("chown plist: %w", err)
	}
	fmt.Fprintf(out, "plist   -> %s\n", plistPath)

	// Unload old version; ignore "not loaded".
	_ = launchctl(ctx, "bootout", "system/"+label)
	if err := launchctl(ctx, "bootstrap", "system", plistPath); err != nil {
		// No daemon is running now; do not leave sleep disabled.
		if clearErr := (power.MacOS{}).SetSleepDisabled(ctx, false); clearErr != nil {
			return errors.Join(err, fmt.Errorf("re-enable sleep: %w", clearErr))
		}
		return err
	}
	fmt.Fprintf(out, "daemon loaded (%s), CLI allowed for uid %d\n", label, allowUID)
	return nil
}

func renderPlist(allowUID int) ([]byte, error) {
	var buf bytes.Buffer
	err := plistTmpl.Execute(&buf, map[string]string{
		"Label": label, "Bin": binPath, "UID": strconv.Itoa(allowUID), "Log": logPath,
	})
	return buf.Bytes(), err
}

// Uninstall unloads the daemon, removes files, and re-enables sleep.
func Uninstall(ctx context.Context, out io.Writer) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if err := launchctl(ctx, "bootout", "system/"+label); err != nil {
		fmt.Fprintf(out, "bootout: %v (continuing)\n", err)
	}
	// Daemon gone; make sure sleep is not left disabled.
	sleepErr := (power.MacOS{}).SetSleepDisabled(ctx, false)
	for _, p := range []string{plistPath, SocketPath, binPath} {
		if err := os.Remove(p); err == nil {
			fmt.Fprintf(out, "removed %s\n", p)
		}
	}
	if err := os.RemoveAll(stateDir); err == nil {
		fmt.Fprintf(out, "removed %s\n", stateDir)
	}
	fmt.Fprintf(out, "log left at %s\n", logPath)
	if sleepErr != nil {
		return fmt.Errorf("re-enable sleep: %w (run: sudo pmset -a disablesleep 0)", sleepErr)
	}
	return nil
}

// TargetUID picks the user who ran `sudo lidawake install`.
func TargetUID() (int, error) {
	if s := os.Getenv("SUDO_UID"); s != "" {
		return strconv.Atoi(s)
	}
	return -1, errors.New("SUDO_UID not set; run via sudo or pass -uid")
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}
	return nil
}

func launchctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmp, 0, 0); err != nil {
		return err
	}
	// Rename replaces atomically; safe even if old binary is running.
	return os.Rename(tmp, dst)
}
