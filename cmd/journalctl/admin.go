package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/userpaths"
)

// runAdminCommand handles verbs that don't print log lines. It returns the
// exit code, or -1 when the invocation is a normal query.
func runAdminCommand(opts journalOpts) int {
	switch {
	case opts.dmesg:
		fmt.Fprintln(os.Stderr, "--dmesg is not supported: initd keeps unit logs only, not the kernel ring")
		return 1
	case opts.verify:
		return runVerify(opts)
	case opts.listFields:
		for _, f := range journalFieldNames() {
			fmt.Println(f)
		}
		return 0
	case opts.field != "":
		return runField(opts)
	case opts.listBoots:
		return runListBoots(opts)
	case opts.listInvocations:
		return runListInvocations(opts)
	case opts.listNamespaces:
		// initd keeps a single journal with no namespaces.
		fmt.Println("No namespaces found.")
		return 0
	case opts.diskUsage:
		return runDiskUsage(opts)
	case opts.vacuumSize != "" || opts.vacuumFiles != "" || opts.vacuumTime != "":
		return runVacuum(opts)
	case opts.sync || opts.flush:
		return runSyncFlush(opts)
	case opts.rotate:
		return runRotate(opts)
	case opts.header:
		return runHeader(opts)
	}
	return -1
}

// journalScope resolves which daemon(s) and dirs a query touches. --user /
// --system pick one; --merge or the default reads system then user.
func journalScope(opts journalOpts) (sockets []string, dirs []string) {
	sysSock := opts.socket
	if sysSock == "" {
		sysSock = userpaths.SystemSocketPath()
	}
	userSock := opts.socket
	if userSock == "" {
		userSock = userpaths.UserSocketPath()
	}
	sysDir := userpaths.SystemJournalDir()
	userDir := userpaths.UserJournalDir()
	root := strings.TrimSuffix(opts.root, "/")
	if root != "" {
		sysDir = filepath.Join(root, strings.TrimPrefix(sysDir, "/"))
		userDir = filepath.Join(root, strings.TrimPrefix(userDir, "/"))
	}
	if opts.directory != "" {
		return []string{""}, []string{opts.directory}
	}
	if opts.file != "" {
		return []string{""}, []string{filepath.Dir(opts.file), "file:" + opts.file}
	}
	switch {
	case opts.user:
		return []string{userSock}, []string{userDir}
	case opts.system:
		return []string{sysSock}, []string{sysDir}
	default:
		// Non-root system and user journals resolve to the same state
		// dir, and both sockets are served with identical data. Querying
		// both would double every count, header and listing.
		if sysDir == userDir {
			// Prefer the user socket; either answers the same.
			return []string{userSock}, []string{userDir}
		}
		return []string{sysSock, userSock}, []string{sysDir, userDir}
	}
}

// fetchScopedEntries queries each daemon in scope (or reads files directly
// when a daemon is down) and merges in time order.
func fetchScopedEntries(opts journalOpts, req ipc.Request) []logging.StoredEntry {
	sockets, dirs := journalScope(opts)
	var scopes [][]logging.StoredEntry
	for i, sock := range sockets {
		var files []string
		if i < len(dirs) {
			files = expandDirEntry(dirs[i], opts)
		}
		if sock == "" {
			entries, _ := logging.ReadAll(files)
			scopes = append(scopes, filterLocal(entries, req))
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(req)
		if err != nil {
			entries, _ := logging.ReadAll(files)
			scopes = append(scopes, filterLocal(entries, req))
			continue
		}
		if !resp.Success {
			continue
		}
		var entries []logging.StoredEntry
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &entries)
		scopes = append(scopes, entries)
	}
	nonEmpty := scopes[:0]
	for _, sc := range scopes {
		if len(sc) > 0 {
			nonEmpty = append(nonEmpty, sc)
		}
	}
	if len(nonEmpty) == 1 {
		// Common case (one daemon, -D, --file): hand the slice back
		// directly instead of copying everything into a merged array.
		// Sources arrive in time order, so no re-sort is needed.
		return nonEmpty[0]
	}
	var all []logging.StoredEntry
	for _, sc := range nonEmpty {
		all = append(all, sc...)
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].RealtimeUsec == all[b].RealtimeUsec {
			return all[a].Seq < all[b].Seq
		}
		return all[a].RealtimeUsec < all[b].RealtimeUsec
	})
	return all
}

// expandDirEntry turns a scope dir into file lists; "file:<path>" markers
// restrict to one file.
func expandDirEntry(dir string, opts journalOpts) []string {
	if strings.HasPrefix(dir, "file:") {
		return []string{strings.TrimPrefix(dir, "file:")}
	}
	return logging.ListFiles(dir)
}

// filterLocal applies the request filter to offline-read entries.
func filterLocal(entries []logging.StoredEntry, req ipc.Request) []logging.StoredEntry {
	return logging.QueryJournal(entries, logging.JournalFilter{
		Units: req.Units, BootID: req.Boot, SinceUsec: req.Since, UntilUsec: req.Until,
		PriorityMax: req.Priority, PrioritySet: req.PrioritySet, Grep: req.Grep,
		CaseSensitive: req.CaseSensitive, Identifier: req.Identifier,
		Invocation: req.Invocation, ExcludeIdentifier: req.ExcludeIdentifier,
		LatestInvocation: req.LatestInvocation,
		Cursor:           req.Cursor, CursorAfter: req.CursorAfter,
		Lines: req.Lines, LinesPlus: req.LinesPlus, Reverse: req.Reverse,
	})
}

func runListBoots(opts journalOpts) int {
	sockets, dirs := journalScope(opts)
	seen := map[string]int{}
	var order []string
	for i, sock := range sockets {
		var entries []logging.StoredEntry
		if sock == "" {
			entries, _ = logging.ReadAll(expandDirEntry(dirs[i], opts))
		} else {
			client := &ipc.Client{SocketPath: sock}
			if resp, err := client.Do(ipc.Request{Action: "journal-boots"}); err == nil && resp.Success {
				var boots [][2]string
				raw, _ := json.Marshal(resp.Data)
				_ = json.Unmarshal(raw, &boots)
				for _, b := range boots {
					n := 0
					fmt.Sscanf(b[1], "%d", &n)
					if _, ok := seen[b[0]]; !ok {
						order = append(order, b[0])
					}
					seen[b[0]] += n
				}
				continue
			}
			entries, _ = logging.ReadAll(expandDirEntry(dirs[i], opts))
		}
		for _, b := range logging.BootList(entries) {
			n := 0
			fmt.Sscanf(b[1], "%d", &n)
			if _, ok := seen[b[0]]; !ok {
				order = append(order, b[0])
			}
			seen[b[0]] += n
		}
	}
	for i, id := range order {
		fmt.Printf("%d %s (%d entries)\n", i, id, seen[id])
	}
	return 0
}

func runListInvocations(opts journalOpts) int {
	sockets, dirs := journalScope(opts)
	var all []logging.StoredEntry
	for i, sock := range sockets {
		if sock == "" {
			entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
			all = append(all, entries...)
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(ipc.Request{Action: "journal", Units: allRequestedUnits(opts)})
		if err != nil || !resp.Success {
			entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
			all = append(all, entries...)
			continue
		}
		var entries []logging.StoredEntry
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &entries)
		all = append(all, entries...)
	}
	units := map[string]bool{}
	for _, u := range allRequestedUnits(opts) {
		units[u] = true
		units[strings.TrimSuffix(u, ".service")] = true
	}
	n := 0
	for _, inv := range logging.Invocations(all) {
		if len(units) > 0 && !units[inv.Unit] && !units[strings.TrimSuffix(inv.Unit, ".service")] {
			continue
		}
		n++
		fmt.Printf("%d %s %s\n", n, inv.ID, formatUnixShort(inv.FirstUsec))
	}
	return 0
}

// formatUnixShort renders microsecond timestamps for --list-invocations.
func formatUnixShort(usec int64) string {
	if usec <= 0 {
		return "-"
	}
	return time.UnixMicro(usec).Local().Format("Mon 2006-01-02 15:04:05 MST")
}

func runDiskUsage(opts journalOpts) int {
	bytes, files := diskUsageTotals(opts)
	fmt.Printf("Journals take up %s in %d files.\n", humanBytes(bytes), files)
	return 0
}

// diskUsageTotals shares the counting between --disk-usage output and the
// --vacuum confirmation so both agree.
func diskUsageTotals(opts journalOpts) (int64, int64) {
	sockets, dirs := journalScope(opts)
	var bytes, files int64
	var daemonDirs []string
	seenDaemonDirs := map[string]bool{}
	for _, sock := range sockets {
		if sock == "" {
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(ipc.Request{Action: "journal-usage"})
		if err != nil || !resp.Success {
			continue
		}
		var data struct {
			Bytes int64  `json:"bytes"`
			Files int64  `json:"files"`
			Dir   string `json:"dir"`
		}
		raw, _ := json.Marshal(resp.Data)
		if err := json.Unmarshal(raw, &data); err != nil {
			continue
		}
		// Both sockets can report the same dir (non-root system ==
		// user). Count each unique dir once.
		if data.Dir != "" {
			if seenDaemonDirs[data.Dir] {
				continue
			}
			seenDaemonDirs[data.Dir] = true
		}
		bytes += data.Bytes
		files += data.Files
		if data.Dir != "" {
			daemonDirs = append(daemonDirs, data.Dir)
		}
	}
	// Any scope the daemon didn't answer (down, --directory, --file) falls
	// back to local files. Daemon-covered dirs are skipped so nothing is
	// counted twice when client and daemon envs disagree.
	for _, d := range dirs {
		skip := false
		for _, covered := range daemonDirs {
			if d == covered {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if strings.HasPrefix(d, "file:") {
			if st, err := os.Stat(strings.TrimPrefix(d, "file:")); err == nil {
				bytes += st.Size()
				files++
			}
			continue
		}
		for _, f := range logging.ListFiles(d) {
			if st, err := os.Stat(f); err == nil {
				bytes += st.Size()
				files++
			}
		}
	}
	return bytes, files
}

func runVacuum(opts journalOpts) int {
	maxBytes, err := parseVacuumSize(opts.vacuumSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	maxFiles := 0
	if opts.vacuumFiles != "" {
		n, err := strconv.Atoi(opts.vacuumFiles)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "invalid --vacuum-files %q\n", opts.vacuumFiles)
			return 1
		}
		maxFiles = n
	}
	maxDays := 0
	if opts.vacuumTime != "" {
		d, err := parseVacuumTime(opts.vacuumTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		maxDays = d
	}
	// Vacuum runs through the daemon so the open file is never removed.
	// Offline dirs vacuum directly with the same never-newest rule.
	sockList, dirList := journalScope(opts)
	beforeBytes, _ := diskUsageTotals(opts)
	code := 0
	for i, sock := range sockList {
		if sock != "" {
			client := &ipc.Client{SocketPath: sock}
			resp, err := client.Do(ipc.Request{
				Action: "journal-vacuum", MaxBytes: maxBytes,
				MaxFiles: maxFiles, MaxDays: maxDays,
			})
			if err == nil && resp.Success {
				continue
			}
		}
		if i < len(dirList) && !strings.HasPrefix(dirList[i], "file:") {
			if err := vacuumDir(dirList[i], maxBytes, maxFiles, maxDays); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				code = 1
			}
		}
	}
	if code == 0 {
		afterBytes, afterFiles := diskUsageTotals(opts)
		freed := beforeBytes - afterBytes
		if freed < 0 {
			freed = 0
		}
		fmt.Printf("Vacuuming done, freed %s (now %s in %d files).\n", humanBytes(freed), humanBytes(afterBytes), afterFiles)
	}
	return code
}

// vacuumDir mirrors Manager.VacuumJournal for offline dirs.
func vacuumDir(dir string, maxBytes int64, maxFiles, maxDays int) error {
	files := logging.ListFiles(dir)
	if len(files) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	if maxFiles <= 0 {
		maxFiles = 10
	}
	if maxDays <= 0 {
		maxDays = 14
	}
	keepNewest := files[len(files)-1]
	// Mirror Manager.VacuumJournal: after a rotate the newest file is
	// empty while the previous holds history. Keep the newest non-empty
	// file too so a small --vacuum-size leaves data instead of 0B.
	kept := map[string]bool{keepNewest: true}
	if st, err := os.Stat(keepNewest); err == nil && st.Size() < 4096 && len(files) >= 2 {
		for i := len(files) - 2; i >= 0; i-- {
			if st, err := os.Stat(files[i]); err == nil && st.Size() >= 4096 {
				kept[files[i]] = true
				break
			}
		}
		if len(kept) == 1 {
			kept[files[len(files)-2]] = true
		}
	}
	cutoffNow := nowFunc().AddDate(0, 0, -maxDays)
	for _, path := range files {
		if kept[path] {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.ModTime().Before(cutoffNow) {
			_ = os.Remove(path)
		}
	}
	for {
		files = logging.ListFiles(dir)
		if len(files) <= maxFiles {
			break
		}
		oldest := ""
		for _, path := range files {
			if !kept[path] {
				oldest = path
				break
			}
		}
		if oldest == "" {
			break
		}
		_ = os.Remove(oldest)
	}
	for {
		var total int64
		for _, path := range logging.ListFiles(dir) {
			if st, err := os.Stat(path); err == nil {
				total += st.Size()
			}
		}
		if total <= maxBytes {
			return nil
		}
		oldest := ""
		for _, path := range logging.ListFiles(dir) {
			if !kept[path] {
				oldest = path
				break
			}
		}
		if oldest == "" {
			return nil
		}
		_ = os.Remove(oldest)
	}
}

func parseVacuumSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	up := strings.ToUpper(s)
	for _, suf := range []struct {
		suffix string
		mult   int64
	}{
		{"G", 1 << 30}, {"GB", 1 << 30}, {"GIB", 1 << 30},
		{"M", 1 << 20}, {"MB", 1 << 20}, {"MIB", 1 << 20},
		{"K", 1 << 10}, {"KB", 1 << 10}, {"KIB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(up, suf.suffix) {
			mult = suf.mult
			s = s[:len(s)-len(suf.suffix)]
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --vacuum-size %q", raw)
	}
	return int64(n * float64(mult)), nil
}

func parseVacuumTime(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	// Plain number = seconds like the real tool; convert up to days.
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("invalid --vacuum-time %q", raw)
		}
		days := n / 86400
		if n > 0 && days == 0 {
			days = 1
		}
		return days, nil
	}
	lower := strings.ToLower(s)
	for _, suf := range []string{"days", "day", "d"} {
		if strings.HasSuffix(lower, suf) {
			n, err := strconv.Atoi(strings.TrimSpace(s[:len(s)-len(suf)]))
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid --vacuum-time %q", raw)
			}
			return n, nil
		}
	}
	// Duration forms: 1w2d, 36h.
	if d, err := parseDurationDays(s); err == nil {
		return d, nil
	}
	return 0, fmt.Errorf("invalid --vacuum-time %q", raw)
}

func parseDurationDays(s string) (int, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	total := 0
	num := ""
	seen := false
	flush := func(unit byte) error {
		if num == "" {
			return fmt.Errorf("bad duration")
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < 0 {
			return fmt.Errorf("bad duration")
		}
		switch unit {
		case 'w':
			total += n * 7
		case 'd':
			total += n
		case 'h':
			total += n / 24
			if n > 0 && n < 24 {
				total++
			}
		case 'm':
			total++
		case 's':
			total++
		}
		num = ""
		seen = true
		return nil
	}
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= '0' && c <= '9' {
			num += string(c)
			continue
		}
		if c == ' ' {
			continue
		}
		if err := flush(c); err != nil {
			return 0, err
		}
	}
	if num != "" || !seen {
		return 0, fmt.Errorf("bad duration")
	}
	return total, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func runSyncFlush(opts journalOpts) int {
	sockets, _ := journalScope(opts)
	code := 0
	did := false
	for _, sock := range sockets {
		if sock == "" {
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(ipc.Request{Action: "journal-sync"})
		if err != nil || !resp.Success {
			code = 1
		} else {
			did = true
		}
	}
	if code == 0 && did {
		if opts.flush {
			fmt.Println("Flushed journal.")
		} else {
			fmt.Println("Synced journal.")
		}
	}
	return code
}

func runRotate(opts journalOpts) int {
	sockets, _ := journalScope(opts)
	code := 0
	did := false
	for _, sock := range sockets {
		if sock == "" {
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(ipc.Request{Action: "journal-rotate"})
		if err != nil || !resp.Success {
			code = 1
		} else {
			did = true
		}
	}
	if code == 0 && did {
		fmt.Println("Rotated journal.")
	}
	return code
}

func runHeader(opts journalOpts) int {
	_, dirs := journalScope(opts)
	for _, d := range dirs {
		if strings.HasPrefix(d, "file:") {
			d = filepath.Dir(strings.TrimPrefix(d, "file:"))
		}
		files := logging.ListFiles(d)
		fmt.Printf("Directory: %s\nFiles: %d\nFormat: initd JSONL (seq, cursor, __REALTIME_TIMESTAMP, _SYSTEMD_UNIT, PRIORITY, MESSAGE)\n", d, len(files))
	}
	return 0
}

func journalFieldNames() []string {
	return []string{"__CURSOR", "__REALTIME_TIMESTAMP", "_MONOTONIC_USEC", "_BOOT_ID", "_SYSTEMD_UNIT", "_PID", "PRIORITY", "SYSLOG_IDENTIFIER", "_HOSTNAME", "_SYSTEMD_INVOCATION_ID", "MESSAGE"}
}

func runField(opts journalOpts) int {
	name := strings.ToUpper(opts.field)
	sockets, dirs := journalScope(opts)
	seen := map[string]bool{}
	var vals []string
	collect := func(entries []logging.StoredEntry) {
		for _, e := range entries {
			v := fieldValue(e, name)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for i, sock := range sockets {
		if sock == "" {
			entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
			collect(entries)
			continue
		}
		client := &ipc.Client{SocketPath: sock}
		resp, err := client.Do(ipc.Request{Action: "journal"})
		if err != nil || !resp.Success {
			entries, _ := logging.ReadAll(expandDirEntry(dirs[i], opts))
			collect(entries)
			continue
		}
		var entries []logging.StoredEntry
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &entries)
		collect(entries)
	}
	sort.Strings(vals)
	for _, v := range vals {
		fmt.Println(v)
	}
	return 0
}

func fieldValue(e logging.StoredEntry, name string) string {
	switch name {
	case "MESSAGE":
		return e.Message
	case "_SYSTEMD_UNIT", "UNIT", "_SYSTEM_UNIT":
		return e.Unit
	case "PRIORITY":
		return fmt.Sprintf("%d", e.Priority)
	case "SYSLOG_IDENTIFIER", "IDENTIFIER", "SYSLOG_ID":
		return e.Identifier
	case "_PID", "PID":
		return fmt.Sprintf("%d", e.PID)
	case "_HOSTNAME", "HOSTNAME":
		return e.Hostname
	case "_BOOT_ID", "BOOT_ID":
		return e.BootID
	case "__CURSOR", "CURSOR":
		return e.Cursor
	case "_SYSTEMD_INVOCATION_ID", "INVOCATION_ID", "_INVOCATION_ID":
		return e.InvocationID
	case "__REALTIME_TIMESTAMP":
		return fmt.Sprintf("%d", e.RealtimeUsec)
	}
	return ""
}

func runVerify(opts journalOpts) int {
	_, dirs := journalScope(opts)
	bad := 0
	files := 0
	for _, d := range dirs {
		if strings.HasPrefix(d, "file:") {
			d = filepath.Dir(strings.TrimPrefix(d, "file:"))
		}
		for _, f := range logging.ListFiles(d) {
			files++
			if n := verifyFile(f); n > 0 {
				fmt.Printf("%s: %d torn lines skipped\n", f, n)
				bad += n
			}
		}
	}
	fmt.Printf("Verified %d files, %d torn lines.\n", files, bad)
	return 0
}
