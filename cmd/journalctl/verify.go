package main

import (
	"bufio"
	"encoding/json"
	"os"

	"initd/internal/logging"
)

// verifyFile counts torn lines a reader would skip. Zero means the file is
// clean; the count itself is the report, never a failure.
func verifyFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var e logging.StoredEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			bad++
		}
	}
	return bad
}
