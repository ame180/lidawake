// Package install places the binary and launchd plist, and removes them.
package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"
	"time"

	"lidawake/internal/power"
)

const (
	Label      = "dev.lidawake.daemon"
	BinPath    = "/usr/local/bin/lidawake"
	PlistPath  = "/Library/LaunchDaemons/" + Label + ".plist"
	SocketPath = "/var/run/lidawake.sock"
	StateDir   = "/var/db/lidawake"
	StatePath  = StateDir + "/state.json"
	LogPath    = "/var/log/lidawake.log"
)

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
func Install(ctx context.Context, allowUID int, out io.Writer) error {
	if err := requireRoot(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)

	// Unload old version first; ignore "not loaded".
	_ = launchctl(ctx, "bootout", "system/"+Label)

	if self != BinPath {
		if err := copyFile(self, BinPath, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
		fmt.Fprintf(out, "binary  -> %s\n", BinPath)
	}

	var buf bytes.Buffer
	err = plistTmpl.Execute(&buf, map[string]string{
		"Label": Label, "Bin": BinPath, "UID": strconv.Itoa(allowUID), "Log": LogPath,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(PlistPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if err := os.Chown(PlistPath, 0, 0); err != nil {
		return fmt.Errorf("chown plist: %w", err)
	}
	fmt.Fprintf(out, "plist   -> %s\n", PlistPath)

	if err := launchctl(ctx, "bootstrap", "system", PlistPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "daemon loaded (%s), CLI allowed for uid %d\n", Label, allowUID)
	return nil
}

// Uninstall unloads the daemon, removes files, and re-enables sleep.
func Uninstall(ctx context.Context, out io.Writer) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if err := launchctl(ctx, "bootout", "system/"+Label); err != nil {
		fmt.Fprintf(out, "bootout: %v (continuing)\n", err)
	}
	// Daemon gone; make sure sleep is not left disabled.
	if err := (power.MacOS{}).SetSleepDisabled(ctx, false); err != nil {
		fmt.Fprintf(out, "WARNING: could not re-enable sleep: %v\nRun: sudo pmset -a disablesleep 0\n", err)
	}
	for _, p := range []string{PlistPath, SocketPath, BinPath} {
		if err := os.Remove(p); err == nil {
			fmt.Fprintf(out, "removed %s\n", p)
		}
	}
	if err := os.RemoveAll(StateDir); err == nil {
		fmt.Fprintf(out, "removed %s\n", StateDir)
	}
	fmt.Fprintf(out, "log left at %s\n", LogPath)
	return nil
}

// TargetUID picks the user who ran `sudo lidawake install`.
func TargetUID() (int, error) {
	if s := os.Getenv("SUDO_UID"); s != "" {
		return strconv.Atoi(s)
	}
	return -1, fmt.Errorf("SUDO_UID not set; run via sudo or pass -uid")
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (use sudo)")
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
