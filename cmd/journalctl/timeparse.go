package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseSinceUntil converts --since/--until text into Unix micros. Accepted:
// RFC3339 and common datetime forms, bare dates, "today"/"yesterday", and
// relative "-Nh/-Nm/-Ns" (also "1 hour ago", "30 min ago"). Empty input
// means no bound. Errors name the offending flag.
func parseSinceUntil(flag, raw string, now time.Time) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	switch lower {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMicro(), nil
	case "yesterday":
		y, m, d := now.AddDate(0, 0, -1).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMicro(), nil
	}
	if ago, ok := parseRelativeAgo(s, now); ok {
		return ago.UnixMicro(), nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"15:04:05",
		"15:04",
	}
	for _, layout := range layouts {
		t, err := time.ParseInLocation(layout, s, now.Location())
		if err != nil {
			continue
		}
		// Bare clock times refer to today (or yesterday if in the future).
		if layout == "15:04:05" || layout == "15:04" {
			y, m, d := now.Date()
			t = time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), 0, now.Location())
			if t.After(now) {
				t = t.AddDate(0, 0, -1)
			}
		}
		return t.UnixMicro(), nil
	}
	return 0, fmt.Errorf("invalid %s %q", flag, raw)
}

// parseRelativeAgo handles "-2h", "-30m", "2 hours ago", "30 min ago".
func parseRelativeAgo(s string, now time.Time) (time.Time, bool) {
	t := strings.ToLower(strings.TrimSpace(s))
	ago := false
	if strings.HasSuffix(t, "ago") {
		ago = true
		t = strings.TrimSpace(strings.TrimSuffix(t, "ago"))
	}
	t = strings.TrimPrefix(t, "-")
	// Normalize "2 hours" -> "2h".
	repl := []struct{ old, new string }{
		{"hours", "h"}, {"hour", "h"}, {"hrs", "h"}, {"hr", "h"},
		{"minutes", "m"}, {"minute", "m"}, {"mins", "m"}, {"min", "m"},
		{"seconds", "s"}, {"second", "s"}, {"secs", "s"}, {"sec", "s"},
		{"days", "d"}, {"day", "d"},
		{"weeks", "w"}, {"week", "w"},
	}
	for _, r := range repl {
		if strings.HasSuffix(t, " "+r.old) {
			t = strings.TrimSpace(strings.TrimSuffix(t, " "+r.old)) + r.new
		} else if strings.HasSuffix(t, r.old) && len(t) > len(r.old) {
			// "2hours" without space.
			t = t[:len(t)-len(r.old)] + r.new
		}
	}
	t = strings.ReplaceAll(t, " ", "")
	if t == "" {
		return time.Time{}, false
	}
	// Must end in a duration unit now.
	last := t[len(t)-1]
	if last != 'h' && last != 'm' && last != 's' && last != 'd' && last != 'w' {
		return time.Time{}, false
	}
	num := t[:len(t)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return time.Time{}, false
	}
	var d time.Duration
	switch last {
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'm':
		d = time.Duration(n) * time.Minute
	case 's':
		d = time.Duration(n) * time.Second
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'w':
		d = time.Duration(n) * 7 * 24 * time.Hour
	}
	_ = ago // both "-2h" and "2h ago" mean the past
	return now.Add(-d), true
}
