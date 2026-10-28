package power

import (
	"errors"
	"testing"
)

func TestParseClamshellState(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		closed  bool
		wantErr error
	}{
		{"closed", `  | "AppleClamshellState" = Yes`, true, nil},
		{"open", `  | "AppleClamshellState" = No`, false, nil},
		{"missing", `+-o Root  <class IORegistryEntry>`, false, ErrNoClamshell},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseClamshellState([]byte(c.in))
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if got != c.closed {
				t.Fatalf("closed = %v, want %v", got, c.closed)
			}
		})
	}
}

func TestParseSleepDisabled(t *testing.T) {
	on := "System-wide power settings:\n SleepDisabled\t\t1\nCurrently in use:\n"
	off := "System-wide power settings:\n SleepDisabled\t\t0\n"
	if !parseSleepDisabled([]byte(on)) {
		t.Fatal("expected disabled")
	}
	if parseSleepDisabled([]byte(off)) {
		t.Fatal("expected enabled")
	}
	if parseSleepDisabled([]byte("Currently in use:\n")) {
		t.Fatal("missing line should mean enabled")
	}
}

func TestParseBattery(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		percent   int
		onBattery bool
		wantErr   bool
	}{
		{"battery", "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=34668643)\t91%; discharging; 20:00 remaining present: true\n", 91, true, false},
		{"ac", "Now drawing from 'AC Power'\n -InternalBattery-0 (id=34668643)\t15%; charging; 1:20 remaining present: true\n", 15, false, false},
		{"ups", "Now drawing from 'UPS Power'\n -InternalBattery-0 (id=1)\t5%; discharging; present: true\n", 5, false, false},
		{"no source", " -InternalBattery-0 (id=1)\t5%; discharging; present: true\n", 0, false, true},
		{"no battery", "Now drawing from 'AC Power'\n", 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			percent, onBattery, err := parseBattery([]byte(c.in))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if percent != c.percent || onBattery != c.onBattery {
				t.Fatalf("got (%d, %v), want (%d, %v)", percent, onBattery, c.percent, c.onBattery)
			}
		})
	}
}
