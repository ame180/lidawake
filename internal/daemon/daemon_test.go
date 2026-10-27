package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lidawake/internal/rpc"
	"lidawake/internal/session"
)

type fakeLid struct{ closed bool }

func (f *fakeLid) IsClosed(context.Context) (bool, error) { return f.closed, nil }

type fakeSleep struct{ disabled bool }

func (f *fakeSleep) SetSleepDisabled(_ context.Context, v bool) error { f.disabled = v; return nil }
func (f *fakeSleep) SleepDisabled(context.Context) (bool, error)      { return f.disabled, nil }

func TestSocketRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lid, sl := &fakeLid{}, &fakeSleep{}

	mgr := session.NewManager(session.Config{
		Store: session.FileStore{Path: filepath.Join(dir, "state.json")},
		Lid:   lid, Sleep: sl, Log: log,
	})
	d := New(Config{SocketPath: sock, AllowUID: -1, PollInterval: 10 * time.Millisecond, MaxAllowed: time.Hour}, mgr, sl, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	info, _ := os.Stat(sock)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket perm = %v", info.Mode().Perm())
	}

	resp, err := rpc.Call(sock, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: 7200})
	if err != nil || resp.OK {
		t.Fatalf("start over MaxAllowed should fail: %+v %v", resp, err)
	}

	resp, err = rpc.Call(sock, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: 60})
	if err != nil || !resp.OK || !resp.Status.Active || !resp.Status.SleepDisabled {
		t.Fatalf("start: %+v %v", resp, err)
	}

	lid.closed = true
	waitFor(t, func() bool { return mgr.Status().SeenClosed })
	lid.closed = false
	waitFor(t, func() bool { return !mgr.Status().Active })

	resp, _ = rpc.Call(sock, rpc.Request{Cmd: rpc.CmdStatus})
	if resp.Status.SleepDisabled || resp.Status.LastEndReason != session.EndLidOpened {
		t.Fatalf("status after reopen: %+v", resp.Status)
	}

	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
