package main

import (
	"fmt"
	"strconv"
	"strings"
)

// priorityNames maps journal priority names and numbers onto 0-7
// (0=emerg .. 7=debug). Matching is case-insensitive; "err" and "error"
// are synonyms like in the real journal.
var priorityNames = map[string]int{
	"emerg": 0, "0": 0,
	"alert": 1, "1": 1,
	"crit": 2, "2": 2,
	"err": 3, "error": 3, "3": 3,
	"warning": 4, "warn": 4, "4": 4,
	"notice": 5, "5": 5,
	"info": 6, "6": 6,
	"debug": 7, "7": 7,
}

// parsePriority converts -p RANGE into the maximum accepted priority
// (entries with Priority <= max pass). Accepted: single name/number and
// "low..high" ranges; the lower end only validates. Empty means unset.
func parsePriority(raw string) (int, bool, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, false, nil
	}
	if strings.Contains(s, "..") {
		parts := strings.SplitN(s, "..", 2)
		lo, ok1 := priorityNames[strings.TrimSpace(parts[0])]
		hi, ok2 := priorityNames[strings.TrimSpace(parts[1])]
		if !ok1 || !ok2 {
			return 0, false, fmt.Errorf("invalid priority range %q", raw)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		_ = lo
		return hi, true, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 7 {
			return 0, false, fmt.Errorf("priority %q out of range 0..7", raw)
		}
		return n, true, nil
	}
	if p, ok := priorityNames[s]; ok {
		return p, true, nil
	}
	return 0, false, fmt.Errorf("invalid priority %q", raw)
}
