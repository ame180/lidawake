package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ame180/lidawake/internal/rpc"
	"github.com/ame180/lidawake/internal/session"
)

// Fakes are shared between the test goroutine and the daemon's goroutines.
type fakeLid struct{ closed atomic.Bool }

func (f *fakeLid) IsClosed(context.Context) (bool, error) { return f.closed.Load(), nil }

type fakeSleep struct{ disabled atomic.Bool }

func (f *fakeSleep) SetSleepDisabled(_ context.Context, v bool) error {
	f.disabled.Store(v)
	return nil
}
func (f *fakeSleep) SleepDisabled(context.Context) (bool, error) { return f.disabled.Load(), nil }

type fakeBattery struct{}

func (fakeBattery) Battery(context.Context) (int, bool, error) { return 100, false, nil }

type testDaemon struct {
	sock string
	lid  *fakeLid
	mgr  *session.Manager
}

// startDaemon runs a daemon on a temp socket until the test ends.
func startDaemon(t *testing.T) *testDaemon {
	t.Helper()
	dir := t.TempDir()
	td := &testDaemon{sock: filepath.Join(dir, "s.sock"), lid: &fakeLid{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sleep := &fakeSleep{}

	td.mgr = session.NewManager(session.Config{
		Store: session.FileStore{Path: filepath.Join(dir, "state.json")},
		Lid:   td.lid, Sleep: sleep, Battery: fakeBattery{}, Log: logger,
	})
	d := New(Config{SocketPath: td.sock, AllowUID: -1, PollInterval: 10 * time.Millisecond, MaxAllowed: time.Hour}, td.mgr, sleep, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	waitFor(t, func() bool { _, err := os.Stat(td.sock); return err == nil })
	return td
}

func TestSocketRoundTrip(t *testing.T) {
	ctx := t.Context()
	td := startDaemon(t)

	info, err := os.Stat(td.sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket perm = %v", info.Mode().Perm())
	}

	resp, err := rpc.Call(ctx, td.sock, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: 7200})
	if err != nil || resp.OK {
		t.Fatalf("start over MaxAllowed should fail: %+v %v", resp, err)
	}

	resp, err = rpc.Call(ctx, td.sock, rpc.Request{Cmd: rpc.CmdStart, MaxSeconds: 60})
	if err != nil || !resp.OK || !resp.Status.Active || !resp.Status.SleepDisabled {
		t.Fatalf("start: %+v %v", resp, err)
	}

	td.lid.closed.Store(true)
	waitFor(t, func() bool { return td.mgr.Status().SeenClosed })
	td.lid.closed.Store(false)
	waitFor(t, func() bool { return !td.mgr.Status().Active })

	resp, err = rpc.Call(ctx, td.sock, rpc.Request{Cmd: rpc.CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status.SleepDisabled || resp.Status.LastEndReason != string(session.EndLidOpened) {
		t.Fatalf("status after reopen: %+v", resp.Status)
	}
}

func TestRejectedRequests(t *testing.T) {
	td := startDaemon(t)
	cases := []struct {
		name string
		body string
	}{
		{"bad json", "{nope\n"},
		{"unknown command", `{"cmd":"reboot"}` + "\n"},
		{"overflowing max", `{"cmd":"start","maxSeconds":9223372036854775807}` + "\n"},
		{"negative min battery", `{"cmd":"start","maxSeconds":60,"minBattery":-1}` + "\n"},
		{"min battery 100", `{"cmd":"start","maxSeconds":60,"minBattery":100}` + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := rawCall(t, td.sock, c.body)
			if resp.OK || resp.Error == "" {
				t.Fatalf("want error response, got %+v", resp)
			}
			if resp.Status.Active {
				t.Fatal("rejected request must not start a session")
			}
		})
	}
}

func rawCall(t *testing.T, sock, body string) rpc.Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, body); err != nil {
		t.Fatal(err)
	}
	var resp rpc.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
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
