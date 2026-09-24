package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// StoredEntry is the on-disk form of one log line. Field names mirror the
// journal so -o export/json falls out naturally later. Seq orders lines
// written in the same microsecond; Cursor is the opaque resume token handed
// to clients (initd-<seq>-<boot>-<offset>).
type StoredEntry struct {
	Seq          uint64 `json:"seq"`
	Cursor       string `json:"cursor"`
	RealtimeUsec int64  `json:"__REALTIME_TIMESTAMP"`
	MonotonicUsec int64 `json:"_MONOTONIC_USEC,omitempty"`
	BootID       string `json:"_BOOT_ID,omitempty"`
	Unit         string `json:"_SYSTEMD_UNIT,omitempty"`
	PID          int    `json:"_PID,omitempty"`
	Priority     int    `json:"PRIORITY,omitempty"`
	Identifier   string `json:"SYSLOG_IDENTIFIER,omitempty"`
	Hostname     string `json:"_HOSTNAME,omitempty"`
	InvocationID string `json:"_SYSTEMD_INVOCATION_ID,omitempty"`
	Message      string `json:"MESSAGE"`
}

// FileWriter appends entries as JSONL to one file per boot. It owns rotation
// by size. One daemon process = one writer; the daemon singleton lock
// (system/user lock file) guarantees no two writers share a dir.
type FileWriter struct {
	mu       sync.Mutex
	dir      string
	bootID   string
	hostname string
	maxBytes int64
	seq      uint64
	offset   uint64
	file     *os.File
	buf      *bufio.Writer
	lines    uint64
	syncedAt time.Time
}

// DefaultMaxFileBytes caps a single journal file before rotation.
const DefaultMaxFileBytes = 10 << 20

// NewFileWriter opens dir/<bootID>.jsonl for append, creating the dir. A
// zero maxBytes uses DefaultMaxFileBytes. The seq start is derived from the
// existing tail so cursors stay ordered across daemon restarts.
func NewFileWriter(dir, bootID, hostname string, maxBytes int64) (*FileWriter, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("journal dir is empty")
	}
	if strings.TrimSpace(bootID) == "" {
		bootID = "unknown-boot"
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &FileWriter{dir: dir, bootID: bootID, hostname: hostname, maxBytes: maxBytes}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *FileWriter) activePath() string { return filepath.Join(w.dir, w.bootID+".jsonl") }

func (w *FileWriter) openLocked() error {
	path := w.activePath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Seq must stay monotonic across rotates and restarts: the active
	// file alone is not enough (after a rotate it is empty and would
	// reset seq to 0, duplicating cursors). Take the max across the dir,
	// keep offset from the active tail for the cursor suffix.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		lastSeq, lines := tailSeqOffset(path, st.Size())
		w.offset = lines
		w.seq = lastSeq
	} else {
		w.offset = 0
	}
	if max := dirMaxSeq(w.dir); max > w.seq {
		w.seq = max
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64*1024)
	w.lines = 0
	return nil
}

// dirMaxSeq returns the highest Seq seen in any *.jsonl in dir, using the
// cheap 64k tail scan per file. Torn lines are skipped by tailSeqOffset.
func dirMaxSeq(dir string) uint64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var max uint64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		st, err := os.Stat(full)
		if err != nil || st.Size() == 0 {
			continue
		}
		if last, _ := tailSeqOffset(full, st.Size()); last > max {
			max = last
		}
	}
	return max
}

// tailSeqOffset recovers the last seq and line count from the file tail so a
// restarted daemon keeps cursors ordered. Torn final lines are ignored.
func tailSeqOffset(path string, size int64) (uint64, uint64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	const window = 64 * 1024
	start := int64(0)
	if size > window {
		start = size - window
	}
	if _, err := f.Seek(start, 0); err != nil {
		return 0, 0
	}
	var lastSeq uint64
	var lines uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	skipped := start > 0 // first line may be a fragment
	for sc.Scan() {
		if skipped {
			skipped = false
			continue
		}
		var e StoredEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
		lines++
	}
	return lastSeq, lines
}

// Append stores one entry, rotating first when the active file is over the
// size cap. It never fails the caller: a disk error is reported but the
// in-memory ring already kept the line.
func (w *FileWriter) Append(e StoredEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return fmt.Errorf("journal writer closed")
	}
	// Check the buffered size too: many rapid Appends sit in buf before
	// hitting the fd, and Stat alone would let the file grow unbounded.
	pending := int64(w.buf.Buffered())
	if st, err := w.file.Stat(); err == nil && st.Size()+pending >= w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return err
		}
	}
	w.seq++
	w.offset++
	e.Seq = w.seq
	e.Cursor = fmt.Sprintf("initd-%d-%s-%d", w.seq, w.bootID, w.offset)
	if e.RealtimeUsec == 0 {
		e.RealtimeUsec = time.Now().UnixMicro()
	}
	if e.BootID == "" {
		e.BootID = w.bootID
	}
	if e.Hostname == "" {
		e.Hostname = w.hostname
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := w.buf.Write(raw); err != nil {
		return err
	}
	w.lines++
	if w.lines%64 == 0 || time.Since(w.syncedAt) > time.Second {
		if err := w.buf.Flush(); err != nil {
			return err
		}
		w.syncedAt = time.Now()
	}
	return nil
}

// Sync flushes buffered lines to disk. journalctl --sync maps here.
func (w *FileWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf == nil {
		return nil
	}
	if err := w.buf.Flush(); err != nil {
		return err
	}
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// Rotate closes the active file and opens a fresh generation for the same
// boot (<boot>.jsonl -> <boot>-<nanos>.jsonl via rename, then new active).
// journalctl --rotate maps here.
func (w *FileWriter) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotateLocked()
}

func (w *FileWriter) rotateLocked() error {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Sync()
		_ = w.file.Close()
	}
	stamp := time.Now().UnixNano()
	_ = os.Rename(w.activePath(), filepath.Join(w.dir, fmt.Sprintf("%s-%d.jsonl", w.bootID, stamp)))
	w.file = nil
	w.buf = nil
	w.offset = 0
	return w.openLocked()
}

// Close flushes and releases the writer.
func (w *FileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		w.buf = nil
		return err
	}
	return nil
}

// ListFiles returns journal files oldest-first for reads and vacuum.
func ListFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// ReadAll streams every well-formed entry from files in order. Torn tail
// lines from a crash are skipped, never fatal.
func ReadAll(files []string) ([]StoredEntry, error) {
	var out []StoredEntry
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e StoredEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			out = append(out, e)
		}
		_ = f.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].Cursor < out[j].Cursor
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// BootID reads the kernel boot id, falling back to unknown-boot.
func BootID() string {
	if raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	return "unknown-boot"
}
