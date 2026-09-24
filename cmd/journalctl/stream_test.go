package main

import (
	"io"
	"os"
	"testing"

	"initd/internal/ipc"
	"initd/internal/logging"
)

// captureStdout swaps os.Stdout for the duration of fn and returns what was
// written, so streaming output stays assertable instead of polluting logs.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// The unbounded driver must page with bounded head-after-cursor requests,
// advancing the cursor and stopping on a short page.
func TestStreamUnboundedPages(t *testing.T) {
	mk := func(i int) logging.StoredEntry {
		return logging.StoredEntry{Cursor: "c" + itoa(i), Unit: "a.service", PID: 1, Priority: 6, Identifier: "a", Hostname: "h", RealtimeUsec: int64(i), Message: "line"}
	}
	full := make([]logging.StoredEntry, 0, journalPageSize+2)
	for i := 0; i < journalPageSize+2; i++ {
		full = append(full, mk(i))
	}
	var seen []ipc.Request
	fetch := func(q ipc.Request) []logging.StoredEntry {
		seen = append(seen, q)
		if len(seen) == 1 {
			return full[:journalPageSize]
		}
		if len(seen) == 2 {
			return full[journalPageSize:]
		}
		return nil
	}
	opts := journalOpts{noPager: true, output: "cat"}
	var code int
	out := captureStdout(t, func() {
		code = streamUnbounded(ipc.Request{Action: "journal"}, opts, fetch)
	})
	if code != 0 {
		t.Fatalf("streamUnbounded = %d, want 0", code)
	}
	if got, want := countLines(out), journalPageSize+2; got != want {
		t.Fatalf("lines = %d, want %d", got, want)
	}
	if len(seen) != 2 {
		t.Fatalf("fetches = %d, want 2 (short page stops, no empty probe)", len(seen))
	}
	if seen[0].Lines != journalPageSize || !seen[0].LinesPlus {
		t.Fatalf("first page not bounded head request: %+v", seen[0])
	}
	if seen[0].Cursor != "" || seen[0].CursorAfter {
		t.Fatalf("first page should carry no cursor: %+v", seen[0])
	}
	if seen[1].Cursor != full[journalPageSize-1].Cursor || !seen[1].CursorAfter {
		t.Fatalf("second page should continue after full page: %+v", seen[1])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

func TestStreamUnboundedEmpty(t *testing.T) {
	var seen []ipc.Request
	out := captureStdout(t, func() {
		if got := streamUnbounded(ipc.Request{Action: "journal"}, journalOpts{noPager: true, output: "cat"}, func(q ipc.Request) []logging.StoredEntry {
			seen = append(seen, q)
			return nil
		}); got != 0 {
			t.Fatalf("streamUnbounded = %d, want 0", got)
		}
	})
	if out != "" {
		t.Fatalf("output = %q, want empty", out)
	}
	if len(seen) != 1 {
		t.Fatalf("fetches = %d, want 1", len(seen))
	}
}
