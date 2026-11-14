package main

import (
	"strings"
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

func TestParseStartArgs(t *testing.T) {
	defaults := startOptions{maxDuration: defaultMax, minBattery: defaultMinBattery}
	ok := []struct {
		args []string
		want startOptions
	}{
		{nil, defaults},
		{[]string{"3h"}, startOptions{maxDuration: 3 * time.Hour, minBattery: defaultMinBattery}},
		{[]string{"-max", "3h"}, startOptions{maxDuration: 3 * time.Hour, minBattery: defaultMinBattery}},
		{[]string{"-ignore-lid", "-min-battery", "40", "2h"}, startOptions{maxDuration: 2 * time.Hour, minBattery: 40, ignoreLid: true}},
	}
	for _, c := range ok {
		got, err := parseStartArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("parseStartArgs(%q) = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}

	bad := []struct {
		args    []string
		mention string
	}{
		{[]string{"3h", "-ignore-lid"}, "flags go before the duration"},
		{[]string{"-max", "3h", "2h"}, "give the duration once"},
		{[]string{"foo", "2h"}, `"foo"`},
		{[]string{"1h", "2h"}, `unexpected argument "2h"`},
		{[]string{"1"}, "missing unit"},
	}
	for _, c := range bad {
		_, err := parseStartArgs(c.args)
		if err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("parseStartArgs(%q) error = %v, want one mentioning %s", c.args, err, c.mention)
		}
	}
}
