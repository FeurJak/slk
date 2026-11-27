package main

import (
	"testing"
	"time"
)

func TestParseAgentTS(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 11, 26, 0, 0, 0, 0, time.Local)
	cases := []struct {
		in   string
		want string
	}{
		{"2026-11-26", agentTS(midnight)},
		{"2026-11-26T06:24:00Z", "1795674240.000000"},
		{"1795634655.404329", "1795634655.404329"},
		{"1795634655", "1795634655.000000"},
		{"36h", agentTS(now.Add(-36 * time.Hour))},
		{"90m", agentTS(now.Add(-90 * time.Minute))},
		{"7d", agentTS(now.AddDate(0, 0, -7))},
		{"2w", agentTS(now.AddDate(0, 0, -14))},
	}
	for _, c := range cases {
		got, err := parseAgentTS(c.in, now)
		if err != nil {
			t.Errorf("parseAgentTS(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseAgentTS(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "2026-13-01", "-3d", "1.2.3", "12.ab"} {
		if got, err := parseAgentTS(bad, now); err == nil {
			t.Errorf("parseAgentTS(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCompareSlackTS(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1795634655.404329", "1795634655.404329", 0},
		{"1795634655.404329", "1795634655.404330", -1},
		{"1795634656.000001", "1795634655.999999", 1},
		{"999999999.9", "1000000000.0", -1},
		{"1795634655.4", "1795634655.400000", 0},
		{"1795634655", "1795634655.000000", 0},
	}
	for _, c := range cases {
		if got := compareSlackTS(c.a, c.b); got != c.want {
			t.Errorf("compareSlackTS(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
