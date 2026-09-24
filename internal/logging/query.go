package logging

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// JournalFilter selects entries from the durable store. Zero values mean no
// constraint. Since/Until are Unix micros (inclusive); PriorityMax keeps
// entries with Priority <= max (0=emerg .. 7=debug); Cursor/CursorAfter
// resume from an opaque cursor token.
type JournalFilter struct {
	Units       []string
	BootID      string
	SinceUsec   int64
	UntilUsec   int64
	PriorityMax int
	PrioritySet bool
	Grep        string
	CaseSensitive bool
	Identifier  string
	// Invocation keeps only one run (_SYSTEMD_INVOCATION_ID exact match).
	// ExcludeIdentifier hides one syslog identifier. LatestInvocation keeps
	// only the newest run per unit (or globally without a unit filter).
	Invocation        string
	ExcludeIdentifier string
	LatestInvocation  bool
	Cursor      string
	CursorAfter bool
	Lines       int
	LinesPlus   bool
	Reverse     bool
}

// QueryJournal filters entries in (realtime, seq) order. Lines>0 keeps the
// last N (or the first N with LinesPlus, like journalctl -n +N) unless
// Reverse flips to first-N-then-reversed like journalctl -r.
// Cursor/CursorAfter locate a resume point by exact token match.
// journalUnits expands unit filters (bare names also match .service).
func journalUnits(f JournalFilter) map[string]struct{} {
	units := map[string]struct{}{}
	for _, u := range f.Units {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		units[u] = struct{}{}
		if !strings.HasSuffix(u, ".service") && !strings.HasSuffix(u, ".socket") {
			units[u+".service"] = struct{}{}
		}
	}
	return units
}

// journalGrepLower pre-lowers the grep pattern for case-insensitive match.
func journalGrepLower(f JournalFilter) string {
	grep := f.Grep
	if !f.CaseSensitive && grep != "" {
		grep = strings.ToLower(grep)
	}
	return grep
}

// matchJournalEntry reports whether e passes every content filter: units,
// boot, time window, priority, identifiers, invocation and grep. Cursor,
// line limits, latest-invocation grouping and reversal are applied by the
// caller, identically for in-memory and streaming queries.
func matchJournalEntry(e StoredEntry, units map[string]struct{}, grep string, f JournalFilter) bool {
	if len(units) > 0 {
		if _, ok := units[e.Unit]; !ok {
			return false
		}
	}
	if f.BootID != "" && e.BootID != "" && e.BootID != f.BootID {
		return false
	}
	if f.SinceUsec > 0 && e.RealtimeUsec < f.SinceUsec {
		return false
	}
	if f.UntilUsec > 0 && e.RealtimeUsec > f.UntilUsec {
		return false
	}
	if f.PrioritySet && e.Priority > f.PriorityMax {
		return false
	}
	if f.Identifier != "" && !strings.EqualFold(e.Identifier, f.Identifier) {
		return false
	}
	if f.ExcludeIdentifier != "" && strings.EqualFold(e.Identifier, f.ExcludeIdentifier) {
		return false
	}
	if f.Invocation != "" && e.InvocationID != f.Invocation {
		return false
	}
	if grep != "" {
		hay := e.Message
		if !f.CaseSensitive {
			hay = strings.ToLower(hay)
		}
		if !strings.Contains(hay, grep) {
			return false
		}
	}
	return true
}

func QueryJournal(entries []StoredEntry, f JournalFilter) []StoredEntry {
	units := journalUnits(f)
	if f.Units == nil && f.BootID == "" && f.SinceUsec == 0 && f.UntilUsec == 0 &&
		!f.PrioritySet && f.Grep == "" && f.Identifier == "" && f.Cursor == "" &&
		f.Lines == 0 && !f.Reverse && f.Invocation == "" &&
		f.ExcludeIdentifier == "" && !f.LatestInvocation {
		// No constraints: return the input slice untouched instead of
		// copying every entry into a second backing array.
		return entries
	}
	grep := journalGrepLower(f)
	// NOTE: deliberately not preallocated to len(entries): selective queries
	// over huge journals must not reserve the full history up front.
	var out []StoredEntry
	cursorIdx := -1
	for _, e := range entries {
		if !matchJournalEntry(e, units, grep, f) {
			continue
		}
		out = append(out, e)
		if f.Cursor != "" && e.Cursor == f.Cursor {
			cursorIdx = len(out) - 1
		}
	}
	if f.Cursor != "" {
		if cursorIdx < 0 {
			return nil
		}
		if f.CursorAfter {
			out = out[cursorIdx+1:]
		} else {
			out = out[cursorIdx:]
		}
	}
	if f.LatestInvocation {
		out = latestInvocationOnly(out, units)
	}
	if f.Lines > 0 && len(out) > f.Lines {
		if f.LinesPlus {
			out = out[:f.Lines]
		} else {
			out = out[len(out)-f.Lines:]
		}
	}
	if f.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// latestInvocationOnly keeps, per unit (or globally when no unit filter is
// given), only the entries of the newest run. Entries written before
// invocation tracking carry no id: groups that have runs drop their untagged
// lines, while groups with no runs at all pass through untouched.
func latestInvocationOnly(entries []StoredEntry, units map[string]struct{}) []StoredEntry {
	keyOf := func(e StoredEntry) string {
		if len(units) > 0 {
			return e.Unit
		}
		return ""
	}
	target := map[string]string{}
	for _, e := range entries {
		if e.InvocationID == "" {
			continue
		}
		target[keyOf(e)] = e.InvocationID // last in time order wins
	}
	if len(target) == 0 {
		return entries
	}
	out := make([]StoredEntry, 0, len(entries))
	for _, e := range entries {
		want, ok := target[keyOf(e)]
		if !ok {
			out = append(out, e)
			continue
		}
		if e.InvocationID != want {
			continue
		}
		out = append(out, e)
	}
	return out
}

// InvocationInfo describes one unit run for --list-invocations.
type InvocationInfo struct {
	ID        string
	Unit      string
	FirstUsec int64
	LastUsec  int64
	Count     int
}

// Invocations groups entries by run id in first-seen order, skipping lines
// that predate invocation tracking.
func Invocations(entries []StoredEntry) []InvocationInfo {
	idx := map[string]int{}
	var out []InvocationInfo
	for _, e := range entries {
		if e.InvocationID == "" {
			continue
		}
		k := e.Unit + "\x00" + e.InvocationID
		if i, ok := idx[k]; ok {
			out[i].Count++
			if e.RealtimeUsec < out[i].FirstUsec {
				out[i].FirstUsec = e.RealtimeUsec
			}
			if e.RealtimeUsec > out[i].LastUsec {
				out[i].LastUsec = e.RealtimeUsec
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, InvocationInfo{
			ID: e.InvocationID, Unit: e.Unit,
			FirstUsec: e.RealtimeUsec, LastUsec: e.RealtimeUsec, Count: 1,
		})
	}
	return out
}

// BootList returns distinct boot ids in first-seen order with entry counts.
func BootList(entries []StoredEntry) [][2]string {
	type boot struct {
		id    string
		count int
	}
	var order []string
	counts := map[string]int{}
	for _, e := range entries {
		id := e.BootID
		if id == "" {
			id = "unknown-boot"
		}
		if _, ok := counts[id]; !ok {
			order = append(order, id)
		}
		counts[id]++
	}
	out := make([][2]string, 0, len(order))
	for _, id := range order {
		out = append(out, [2]string{id, itoa(counts[id])})
	}
	return out
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

// filesNewerThan drops files whose modification time predates sinceUsec.
// A journal file's mtime tracks its last flush, which always follows the
// entries it contains (buffered appends flush after the lines they carry,
// and rotation renames preserve mtime), so a file older than the window
// cannot hold in-window entries and is skipped without opening. Paths that
// cannot be stated are kept. This turns trailing-window queries over deep
// history from full-archive scans into recent-file reads.
func filesNewerThan(files []string, sinceUsec int64) []string {
	if sinceUsec <= 0 {
		return files
	}
	kept := make([]string, 0, len(files))
	for _, path := range files {
		st, err := os.Stat(path)
		if err != nil {
			kept = append(kept, path)
			continue
		}
		if st.ModTime().UnixMicro() < sinceUsec {
			continue
		}
		kept = append(kept, path)
	}
	return kept
}

// QueryJournalStream answers a first-N-after-cursor query (Lines>0 with
// LinesPlus, no Reverse, no LatestInvocation) by scanning files oldest-first
// and stopping as soon as limit matches are collected, so unbounded journals
// never materialize in RAM. Entries after f.Cursor (exclusive with
// CursorAfter, inclusive otherwise) are considered; an unknown cursor yields
// nil like QueryJournal. Within one boot the order matches QueryJournal
// exactly (per-boot sequence order); across boots it follows file order.
func QueryJournalStream(files []string, f JournalFilter) ([]StoredEntry, error) {
	if f.Lines <= 0 {
		return nil, nil
	}
	files = filesNewerThan(files, f.SinceUsec)
	units := journalUnits(f)
	grep := journalGrepLower(f)
	out := make([]StoredEntry, 0, f.Lines)
	found := f.Cursor == ""
	for _, path := range files {
		fh, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e StoredEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			if !found {
				if e.Cursor == f.Cursor {
					found = true
					if f.CursorAfter {
						continue
					}
				} else {
					continue
				}
			}
			if !matchJournalEntry(e, units, grep, f) {
				continue
			}
			out = append(out, e)
			if len(out) >= f.Lines {
				_ = fh.Close()
				return out, nil
			}
		}
		_ = fh.Close()
	}
	if !found {
		return nil, nil
	}
	return out, nil
}

// QueryJournalTail answers a tail-N query (Lines>0 without LinesPlus, no
// Reverse, no LatestInvocation) by scanning files newest-first and stopping
// once limit matches are banked, so `-n` never loads the whole journal.
// The returned slice is chronological (oldest first), matching QueryJournal.
func QueryJournalTail(files []string, f JournalFilter) ([]StoredEntry, error) {
	if f.Lines <= 0 {
		return nil, nil
	}
	files = filesNewerThan(files, f.SinceUsec)
	units := journalUnits(f)
	grep := journalGrepLower(f)
	var chunks [][]StoredEntry
	total := 0
	for i := len(files) - 1; i >= 0; i-- {
		fh, err := os.Open(files[i])
		if err != nil {
			continue
		}
		var cur []StoredEntry
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e StoredEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			if !matchJournalEntry(e, units, grep, f) {
				continue
			}
			cur = append(cur, e)
		}
		_ = fh.Close()
		if len(cur) > 0 {
			chunks = append(chunks, cur)
			total += len(cur)
			if total >= f.Lines {
				break
			}
		}
	}
	if total == 0 {
		return nil, nil
	}
	// Reassemble oldest-first and keep the tail.
	out := make([]StoredEntry, 0, min(f.Lines, total))
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	if len(out) > f.Lines {
		out = out[len(out)-f.Lines:]
	}
	return out, nil
}
