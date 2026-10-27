// Package rpc is the JSON-over-Unix-socket protocol between CLI and daemon.
// One request line, one response line, then the connection closes.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"time"
)

// Command names a daemon operation.
type Command string

// Commands understood by the daemon.
const (
	CmdStart  Command = "start"
	CmdStop   Command = "stop"
	CmdStatus Command = "status"
)

// Request is sent by the CLI. MaxSeconds applies to CmdStart only.
type Request struct {
	Cmd        Command `json:"cmd"`
	MaxSeconds int64   `json:"maxSeconds,omitempty"`
}

// Status describes the session and the live pmset flag.
type Status struct {
	Active        bool      `json:"active"`
	SeenClosed    bool      `json:"seenClosed"`
	StartedAt     time.Time `json:"startedAt,omitzero"`
	Deadline      time.Time `json:"deadline,omitzero"`
	SleepDisabled bool      `json:"sleepDisabled"`
	LastEndReason string    `json:"lastEndReason,omitempty"`
	LastEndedAt   time.Time `json:"lastEndedAt,omitzero"`
}

// Response is sent by the daemon. Status is filled even when OK is false.
type Response struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Status Status `json:"status"`
}

// Call sends one request and waits for the response.
func Call(ctx context.Context, socketPath string, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if errors.Is(err, fs.ErrPermission) {
		return Response{}, fmt.Errorf("connect to daemon at %s: %w (only the user who ran install may use lidawake)", socketPath, err)
	}
	if err != nil {
		return Response{}, fmt.Errorf("connect to daemon at %s: %w (installed? run: sudo lidawake install)", socketPath, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("send: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}
