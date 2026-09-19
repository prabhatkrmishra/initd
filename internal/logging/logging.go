package logging

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Level string

const (
	LevelInfo  Level = "INFO"
	LevelError Level = "ERROR"
)

type Entry struct {
	Timestamp time.Duration
	// WallTime is the wall-clock instant the line was logged. Zero for
	// entries created before disk persistence existed; readers fall back
	// to file order then.
	WallTime time.Time
	Unit     string
	PID      int
	Level    Level
	Message  string
}

type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	max     int
	// file, when non-nil, receives every Add as JSONL. The ring stays a
	// bounded hot cache; the file is the durable source of truth.
	file *FileWriter
}

func NewBuffer(maxEntries int) *Buffer {
	return &Buffer{
		entries: make([]Entry, 0, maxEntries),
		max:     maxEntries,
	}
}

// AttachFile wires durable storage. Subsequent Adds go to both ring and
// file; a disk error never drops the ring copy. Nil detaches.
func (b *Buffer) AttachFile(w *FileWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.file = w
}

// Priority maps our two levels onto syslog priorities so -p filtering
// works: INFO->6 (info), ERROR->3 (err).
func (e Entry) Priority() int {
	if e.Level == LevelError {
		return 3
	}
	return 6
}

// Identifier defaults to the unit basename (foo.service -> foo), the same
// value -t matches on.
func (e Entry) Identifier() string {
	base := e.Unit
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".service")
}

func (b *Buffer) Add(entry Entry) {
	b.mu.Lock()
	if b.max <= 0 {
		b.mu.Unlock()
		return
	}
	if len(b.entries) >= b.max {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:b.max-1]
	}
	b.entries = append(b.entries, entry)
	w := b.file
	b.mu.Unlock()
	if w != nil {
		// Zero WallTime predates disk persistence (old tests, replayed
		// fixtures): stamp now so readers can sort and filter by time.
		wall := entry.WallTime
		if wall.IsZero() {
			wall = time.Now()
		}
		_ = w.Append(StoredEntry{
			MonotonicUsec: int64(entry.Timestamp / time.Microsecond),
			Unit:          entry.Unit,
			PID:           entry.PID,
			Priority:      entry.Priority(),
			Identifier:    entry.Identifier(),
			Message:       entry.Message,
			RealtimeUsec:  wall.UnixMicro(),
		})
	}
}

func (b *Buffer) Entries() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries := make([]Entry, len(b.entries))
	copy(entries, b.entries)
	return entries
}

type LineLogger struct {
	Unit   string
	PID    int
	Level  Level
	Buffer *Buffer
	Output io.Writer
}

func (l *LineLogger) Write(p []byte) (int, error) {
	if l.Buffer == nil {
		return len(p), nil
	}
	reader := bufio.NewScanner(strings.NewReader(string(p)))
	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		entry := Entry{
			Timestamp: MonotonicNow(),
			WallTime:  time.Now(),
			Unit:      l.Unit,
			PID:       l.PID,
			Level:     l.Level,
			Message:   line,
		}
		l.Buffer.Add(entry)
		if l.Output != nil {
			_, _ = fmt.Fprintf(l.Output, "%s\n", FormatEntry(entry))
		}
	}
	return len(p), nil
}

func FormatEntry(entry Entry) string {
	return fmt.Sprintf("[%s] %s[%d]: %s", formatMonotonic(entry.Timestamp), entry.Unit, entry.PID, entry.Message)
}
