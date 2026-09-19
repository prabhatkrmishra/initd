package logging

import (
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
func QueryJournal(entries []StoredEntry, f JournalFilter) []StoredEntry {
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
	grep := f.Grep
	if !f.CaseSensitive && grep != "" {
		grep = strings.ToLower(grep)
	}
	out := make([]StoredEntry, 0, len(entries))
	cursorIdx := -1
	for i, e := range entries {
		if len(units) > 0 {
			if _, ok := units[e.Unit]; !ok {
				continue
			}
		}
		if f.BootID != "" && e.BootID != "" && e.BootID != f.BootID {
			continue
		}
		if f.SinceUsec > 0 && e.RealtimeUsec < f.SinceUsec {
			continue
		}
		if f.UntilUsec > 0 && e.RealtimeUsec > f.UntilUsec {
			continue
		}
		if f.PrioritySet && e.Priority > f.PriorityMax {
			continue
		}
		if f.Identifier != "" && !strings.EqualFold(e.Identifier, f.Identifier) {
			continue
		}
		if grep != "" {
			hay := e.Message
			if !f.CaseSensitive {
				hay = strings.ToLower(hay)
			}
			if !strings.Contains(hay, grep) {
				continue
			}
		}
		out = append(out, e)
		if f.Cursor != "" && e.Cursor == f.Cursor {
			cursorIdx = len(out) - 1
		}
		_ = i
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
