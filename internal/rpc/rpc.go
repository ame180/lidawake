// Package rpc is the JSON-over-Unix-socket protocol between CLI and daemon.
// One request line, one response line, then the connection closes.
package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const (
	CmdStart  = "start"
	CmdStop   = "stop"
	CmdStatus = "status"
)

type Request struct {
	Cmd        string `json:"cmd"`
	MaxSeconds int64  `json:"maxSeconds,omitempty"`
}

type Status struct {
	Active        bool      `json:"active"`
	SeenClosed    bool      `json:"seenClosed"`
	StartedAt     time.Time `json:"startedAt,omitzero"`
	Deadline      time.Time `json:"deadline,omitzero"`
	SleepDisabled bool      `json:"sleepDisabled"`
	LastEndReason string    `json:"lastEndReason,omitempty"`
	LastEndedAt   time.Time `json:"lastEndedAt,omitzero"`
}

type Response struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Status Status `json:"status"`
}

// Call sends one request and waits for the response.
func Call(socketPath string, req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return Response{}, fmt.Errorf("connect to daemon at %s: %w (installed? run: sudo lidawake install)", socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("send: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}
