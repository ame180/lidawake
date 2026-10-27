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
			got, err := ParseClamshellState([]byte(c.in))
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
	if !ParseSleepDisabled([]byte(on)) {
		t.Fatal("expected disabled")
	}
	if ParseSleepDisabled([]byte(off)) {
		t.Fatal("expected enabled")
	}
	if ParseSleepDisabled([]byte("Currently in use:\n")) {
		t.Fatal("missing line should mean enabled")
	}
}
