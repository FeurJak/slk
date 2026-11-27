// This file holds the time handling of the agent commands: reading a
// --since/--until value into a Slack ts, and ordering Slack ts strings.

package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// agentDurationRe matches a "how long ago" value: a Go duration
// ("36h", "90m") or a whole number of days or weeks ("7d", "2w").
var agentDurationRe = regexp.MustCompile(`^(\d+)([dw])$`)

// agentTSRe matches a Slack ts or bare unix seconds.
var agentTSRe = regexp.MustCompile(`^\d+(\.\d+)?$`)

// agentNow is the clock relative times ("7d") are read against; a var
// so tests can pin it.
var agentNow = time.Now

// parseAgentTS reads a --since/--until value as a Slack ts. It takes
// a YYYY-MM-DD date (local midnight), an RFC 3339 time, a Slack ts or
// unix seconds (a ts is returned as given), or a duration back from
// now: a Go duration ("36h") or days/weeks ("7d", "2w").
func parseAgentTS(s string, now time.Time) (string, error) {
	switch {
	case s == "":
		return "", fmt.Errorf("empty time")
	case agentTSRe.MatchString(s):
		if !strings.Contains(s, ".") {
			return s + ".000000", nil
		}
		return s, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return agentTS(t), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return agentTS(t), nil
	}
	if m := agentDurationRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		if m[2] == "w" {
			n *= 7
		}
		return agentTS(now.AddDate(0, 0, -n)), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return agentTS(now.Add(-d)), nil
	}
	return "", fmt.Errorf("%q is not a date (YYYY-MM-DD), an RFC 3339 time, a Slack ts or a duration like 36h or 7d", s)
}

// agentTS renders t as a Slack ts with a zero sequence number.
func agentTS(t time.Time) string {
	return fmt.Sprintf("%d.000000", t.Unix())
}

// compareSlackTS orders two Slack timestamps ("1795634655.404329") by
// value, returning -1, 0 or +1. Seconds compare as integers and the
// fraction as six digits, so a short fraction ("…655.4") and a missing
// one ("…655") read as Slack means them. A plain string compare is
// wrong once the seconds differ in length; float64 loses the last
// digit of a 16-digit ts.
func compareSlackTS(a, b string) int {
	as, af := splitSlackTS(a)
	bs, bf := splitSlackTS(b)
	switch {
	case as != bs:
		if as < bs {
			return -1
		}
		return 1
	case af != bf:
		if af < bf {
			return -1
		}
		return 1
	}
	return 0
}

func splitSlackTS(ts string) (int64, int64) {
	secs, frac, _ := strings.Cut(ts, ".")
	sec, _ := strconv.ParseInt(secs, 10, 64)
	frac = (frac + "000000")[:6]
	f, _ := strconv.ParseInt(frac, 10, 64)
	return sec, f
}
