package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"initd/internal/ipc"
	"initd/internal/logging"
)

// runOffline serves queries straight from the files when no daemon answers.
// Follow is impossible offline (nothing new will arrive), everything else
// behaves identically to the live path.
func runOffline(opts journalOpts) int {
	if opts.dmesg {
		fmt.Fprintln(os.Stderr, "--dmesg is not supported: initd keeps unit logs only, not the kernel ring")
		return 1
	}
	boot, ok := offlineBoot(opts)
	if !ok {
		return 1
	}
	req := ipc.Request{
		Action: "journal", Units: allRequestedUnits(opts), Boot: boot,
		Priority: opts.priority, PrioritySet: opts.prioritySet,
		Grep: opts.grep, CaseSensitive: opts.caseSensitive,
		Identifier: opts.identifier, Cursor: opts.cursor,
		Invocation: opts.invocation, ExcludeIdentifier: opts.excludeIdentifier,
		LatestInvocation: opts.latestInvocation,
		CursorAfter:      opts.afterCursor != "",
		Lines:            opts.lines, LinesPlus: opts.linesPlus, Reverse: opts.reverse,
	}
	if opts.afterCursor != "" && opts.cursor == "" {
		req.Cursor = opts.afterCursor
	}
	if opts.since != "" || opts.until != "" {
		now := time.Now()
		since, err := parseSinceUntil("--since", opts.since, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			return 1
		}
		until, err := parseSinceUntil("--until", opts.until, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			return 1
		}
		req.Since, req.Until = since, until
	}
	if opts.cursorFile != "" {
		if raw, err := readCursorFile(opts.cursorFile); err == nil && raw != "" {
			req.Cursor, req.CursorAfter = raw, true
		}
	}
	// Single-source offline queries stream in cursor pages like the live
	// path (flat memory); merged multi-source queries keep the old
	// materializing path since one cursor cannot page several scopes.
	if req.Lines == 0 && !req.Reverse && !req.LatestInvocation {
		if _, dirs := journalScope(opts); len(dirs) == 1 {
			files := expandDirEntry(dirs[0], opts)
			return streamUnbounded(req, opts, func(q ipc.Request) []logging.StoredEntry {
				out, _ := logging.QueryJournalStream(files, requestFilter(q))
				return out
			})
		}
	}
	entries := applyDisplayFilters(fetchScopedEntries(opts, req), opts)
	trailer := ""
	if opts.showCursor && len(entries) > 0 {
		trailer = "-- cursor: " + entries[len(entries)-1].Cursor
	}
	if opts.cursorFile != "" && len(entries) > 0 {
		_ = writeCursorFile(opts.cursorFile, entries[len(entries)-1].Cursor)
	}
	emitEntries(entries, trailer, opts)
	return 0
}

// offlineBoot resolves -b without a daemon: empty means current boot from
// the kernel, an id matches prefix-style, -N walks the boot list.
func offlineBoot(opts journalOpts) (string, bool) {
	if !opts.bootSet {
		return "", true
	}
	if opts.boot == "" {
		return logging.BootID(), true
	}
	_, dirs := journalScope(opts)
	var all []logging.StoredEntry
	for i := range dirs {
		entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
		all = append(all, entries...)
	}
	boots := logging.BootList(all)
	ids := make([]string, 0, len(boots))
	for _, b := range boots {
		ids = append(ids, b[0])
	}
	resolved, ok := resolveBootID(opts.boot, ids)
	if !ok {
		fmt.Fprintf(os.Stderr, "boot %q not found\n", opts.boot)
		return "", false
	}
	return resolved, true
}

func allRequestedUnits(opts journalOpts) []string {
	out := append([]string{}, opts.units...)
	out = append(out, opts.userUnits...)
	return out
}

// resolveBoot maps -b values onto boot ids: "" is current, "-N" counts back
// from newest, a full id or unambiguous prefix matches directly.
func resolveBoot(client *ipc.Client, socketPath string, opts journalOpts) string {
	if !opts.bootSet {
		return ""
	}
	if opts.boot == "" {
		return logging.BootID()
	}
	c := &ipc.Client{SocketPath: socketPath}
	resp, err := c.Do(ipc.Request{Action: "journal-boots"})
	var ids []string
	if err == nil && resp.Success {
		var boots [][2]string
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &boots)
		for _, b := range boots {
			ids = append(ids, b[0])
		}
	}
	if len(ids) == 0 {
		_, dirs := journalScope(opts)
		var all []logging.StoredEntry
		for i := range dirs {
			entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
			all = append(all, entries...)
		}
		for _, b := range logging.BootList(all) {
			ids = append(ids, b[0])
		}
	}
	resolved, ok := resolveBootID(opts.boot, ids)
	if !ok {
		fmt.Fprintf(os.Stderr, "boot %q not found\n", opts.boot)
		opts.bootFailed = true
		return ""
	}
	return resolved
}

func resolveBootID(want string, ids []string) (string, bool) {
	if len(ids) == 0 {
		return "", false
	}
	if n, ok := parseBootOffset(want); ok {
		idx := len(ids) + n // -0 newest, -1 previous
		if idx < 0 || idx >= len(ids) {
			return "", false
		}
		return ids[idx], true
	}
	var match string
	matches := 0
	for _, id := range ids {
		if id == want || (len(want) >= 8 && len(id) >= len(want) && id[:len(want)] == want) {
			match, matches = id, matches+1
		}
	}
	return match, matches == 1
}

func parseBootOffset(want string) (int, bool) {
	if want == "" || want == "0" {
		return -1 + 1, true // callers pass "" separately; "0" is newest
	}
	neg := false
	s := want
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	n := 0
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		return -n, true
	}
	// Positive offsets count from oldest like the real tool does for +N;
	// bare numbers other than 0 are ids, not offsets.
	return 0, false
}

// isBootValue reports whether a token following -b is a boot value rather
// than the next option. Offsets (-1, 0) and ids all start with alphanumerics
// or dashes; real flags are longer alphabetic words.
func isBootValue(tok string) bool {
	if tok == "" {
		return false
	}
	if _, ok := parseBootOffset(tok); ok {
		return true
	}
	// Hex-ish boot ids and prefixes: allow [0-9a-f-] of length >= 2.
	if len(tok) >= 2 {
		hexish := true
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-') {
				hexish = false
				break
			}
		}
		if hexish {
			return true
		}
	}
	return false
}
