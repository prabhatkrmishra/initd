package main

import (
	"os"
	"strings"
	"time"
)

// nowFunc lets tests pin "now" for vacuum age math without touching the
// wall clock. Production always returns time.Now.
var nowFunc = defaultNow

func defaultNow() time.Time { return time.Now() }

func readCursorFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func writeCursorFile(path, cursor string) error {
	return os.WriteFile(path, []byte(cursor+"\n"), 0o644)
}
