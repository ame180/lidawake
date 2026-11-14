package main

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		12 * time.Hour:              "12h",
		90 * time.Minute:            "1h30m",
		10 * time.Minute:            "10m",
		45 * time.Second:            "45s",
		time.Hour + 30*time.Second:  "1h0m30s",
		0:                           "0s",
		-5 * time.Minute:            "-5m",
		2*time.Hour + 5*time.Minute: "2h5m",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
