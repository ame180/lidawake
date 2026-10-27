// Package daemon runs the root launchd service: poll loop plus socket server.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ame180/lidawake/internal/power"
	"github.com/ame180/lidawake/internal/rpc"
	"github.com/ame180/lidawake/internal/session"
)

type Config struct {
	SocketPath   string
	AllowUID     int // socket owner; -1 = root only
	PollInterval time.Duration
	MaxAllowed   time.Duration
}

type Daemon struct {
	cfg   Config
	mgr   *session.Manager
	sleep power.SleepControl
	log   *slog.Logger
}

func New(cfg Config, mgr *session.Manager, sleep power.SleepControl, log *slog.Logger) *Daemon {
	return &Daemon{cfg: cfg, mgr: mgr, sleep: sleep, log: log}
}

// Run blocks until ctx is cancelled. Exiting does not touch pmset:
// launchd KeepAlive restarts the daemon and Recover resumes the session.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.mgr.Recover(ctx); err != nil {
		d.log.Error("recover", "err", err)
	}

	ln, err := d.listen()
	if err != nil {
		return err
	}
	defer os.Remove(d.cfg.SocketPath)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.pollLoop(ctx)
	}()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	d.log.Info("listening", "socket", d.cfg.SocketPath, "allowUID", d.cfg.AllowUID)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			d.log.Warn("accept", "err", err)
			continue
		}
		go d.handle(ctx, conn)
	}
	wg.Wait()
	return nil
}

func (d *Daemon) listen() (net.Listener, error) {
	if err := os.Remove(d.cfg.SocketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", d.cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	// macOS checks write permission on the socket file for connect().
	// 0600 + chown to the allowed user = only that user and root can talk to us.
	if err := os.Chmod(d.cfg.SocketPath, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	if d.cfg.AllowUID >= 0 {
		if err := os.Chown(d.cfg.SocketPath, d.cfg.AllowUID, -1); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chown socket: %w", err)
		}
	}
	return ln, nil
}

func (d *Daemon) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.mgr.Tick(ctx)
		}
	}
}

func (d *Daemon) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var req rpc.Request
	// Requests are a few dozen bytes; cap to avoid unbounded reads.
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, 4096))).Decode(&req); err != nil {
		d.reply(conn, rpc.Response{Error: "bad request: " + err.Error()})
		return
	}

	var err error
	switch req.Cmd {
	case rpc.CmdStart:
		maxDuration := time.Duration(req.MaxSeconds) * time.Second
		if maxDuration <= 0 || maxDuration > d.cfg.MaxAllowed {
			err = fmt.Errorf("max duration must be between 1s and %s", d.cfg.MaxAllowed)
			break
		}
		_, err = d.mgr.Start(ctx, maxDuration)
	case rpc.CmdStop:
		_, err = d.mgr.Stop(ctx)
	case rpc.CmdStatus:
	default:
		err = fmt.Errorf("unknown command %q", req.Cmd)
	}

	resp := rpc.Response{OK: err == nil, Status: d.status(ctx)}
	if err != nil {
		resp.Error = err.Error()
	}
	d.reply(conn, resp)
}

func (d *Daemon) status(ctx context.Context) rpc.Status {
	st := d.mgr.Status()
	disabled, err := d.sleep.SleepDisabled(ctx)
	if err != nil {
		d.log.Warn("read pmset", "err", err)
	}
	return rpc.Status{
		Active:        st.Active,
		SeenClosed:    st.SeenClosed,
		StartedAt:     st.StartedAt,
		Deadline:      st.Deadline,
		SleepDisabled: disabled,
		LastEndReason: st.LastEndReason,
		LastEndedAt:   st.LastEndedAt,
	}
}

func (d *Daemon) reply(conn net.Conn, resp rpc.Response) {
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		d.log.Warn("write response", "err", err)
	}
}
