package main

import (
	"encoding/json"
	"fmt"
	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/userpaths"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/shlex"
)

const journalctlVersion = "1.1.0"

func main() {
	if wantsHelp(os.Args[1:]) {
		printHelp()
		return
	}
	if wantsVersion(os.Args[1:]) {
		fmt.Printf("journalctl (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n", journalctlVersion)
		return
	}

	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		usage()
		os.Exit(1)
	}

	if code := runAdminCommand(opts); code >= 0 {
		os.Exit(code)
	}

	socketPath := opts.socket
	if socketPath == "" {
		if opts.user {
			socketPath = userpaths.UserSocketPath()
		} else {
			socketPath = userpaths.SystemSocketPath()
		}
	}

	// Offline fallback: daemon down means read the files directly like the
	// real journalctl does. Everything except follow works offline.
	client := &ipc.Client{SocketPath: socketPath}
	if _, err := client.Do(ipc.Request{Action: "is-system-running"}); err != nil {
		if opts.follow {
			fmt.Fprintf(os.Stderr, "daemon not running: follow mode needs a live daemon\n")
			os.Exit(1)
		}
		os.Exit(runOffline(opts))
	}

	boot := resolveBoot(client, socketPath, opts)
	if opts.bootFailed {
		os.Exit(1)
	}
	req := ipc.Request{
		Action:      "journal",
		Units:       append(append([]string{}, opts.units...), opts.userUnits...),
		Boot:        boot,
		Priority:    opts.priority,
		PrioritySet: opts.prioritySet,
		Grep:        opts.grep,
		CaseSensitive: opts.caseSensitive,
		Identifier:    opts.identifier,
		Invocation:    opts.invocation,
		ExcludeIdentifier: opts.excludeIdentifier,
		LatestInvocation: opts.latestInvocation,
		Cursor:        opts.cursor,
		Lines:         opts.lines,
		LinesPlus:     opts.linesPlus,
		Reverse:       opts.reverse,
	}
	if opts.afterCursor != "" {
		req.Cursor, req.CursorAfter = opts.afterCursor, true
	}
	if opts.since != "" || opts.until != "" {
		now := time.Now()
		since, err := parseSinceUntil("--since", opts.since, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}
		until, err := parseSinceUntil("--until", opts.until, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}
		req.Since = since
		req.Until = until
	}

	if opts.cursorFile != "" {
		if raw, err := readCursorFile(opts.cursorFile); err == nil && raw != "" {
			req.Cursor = raw
			req.CursorAfter = true
		}
	}

	if opts.follow {
		os.Exit(runFollow(client, req, opts))
	}

	entries := applyDisplayFilters(fetchEntries(client, req), opts)
	trailer := ""
	if opts.showCursor && len(entries) > 0 {
		trailer = "-- cursor: " + entries[len(entries)-1].Cursor
	}
	if opts.cursorFile != "" && len(entries) > 0 {
		_ = writeCursorFile(opts.cursorFile, entries[len(entries)-1].Cursor)
	}
	emitEntries(entries, trailer, opts)
}

// applyDisplayFilters implements the output-only flags the daemon never
// sees: -q drops info-and-above noise, --truncate-newline cuts embedded
// newlines. -a/--no-full are already no-ops (all fields always print in
// full), so they need no handling here.
func applyDisplayFilters(entries []logging.StoredEntry, opts journalOpts) []logging.StoredEntry {
	if !opts.quiet && !opts.truncateNewline {
		return entries
	}
	out := make([]logging.StoredEntry, 0, len(entries))
	for _, e := range entries {
		if opts.quiet && e.Priority > 4 {
			continue
		}
		if opts.truncateNewline {
			if i := strings.IndexByte(e.Message, '\n'); i >= 0 {
				e.Message = e.Message[:i]
			}
		}
		out = append(out, e)
	}
	return out
}

func fetchEntries(client *ipc.Client, req ipc.Request) []logging.StoredEntry {
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}
	var entries []logging.StoredEntry
	raw, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(raw, &entries)
	return entries
}

// runFollow prints the current tail, then polls with after-cursor until
// interrupted. Polling fits our request/response IPC: no streams needed,
// and at supervisor log rates a 250ms cadence is instant to a human. Follow
// output never goes through the pager (like the real tool): it must stay
// live instead of blocking on a pager quit.
func runFollow(client *ipc.Client, req ipc.Request, opts journalOpts) int {
	entries := applyDisplayFilters(fetchEntries(client, req), opts)
	streamEntries(os.Stdout, entries, opts.output, opts.utc, opts.noHostname, opts.outputFields)
	last := ""
	if len(entries) > 0 {
		last = entries[len(entries)-1].Cursor
	}
	if opts.showCursor && last != "" {
		fmt.Println("-- cursor: " + last)
	}
	if opts.cursorFile != "" && last != "" {
		_ = writeCursorFile(opts.cursorFile, last)
	}
	for {
		time.Sleep(250 * time.Millisecond)
		poll := req
		poll.Cursor = last
		poll.CursorAfter = true
		poll.Lines = 0
		next := applyDisplayFilters(fetchEntries(client, poll), opts)
		if len(next) == 0 {
			continue
		}
		streamEntries(os.Stdout, next, opts.output, opts.utc, opts.noHostname, opts.outputFields)
		last = next[len(next)-1].Cursor
		if opts.cursorFile != "" {
			_ = writeCursorFile(opts.cursorFile, last)
		}
	}
}

// emitEntries streams entries to stdout, paging only on a tty without
// --no-pager. The trailer (e.g. --show-cursor) travels through the pager
// with the entries so output is never printed twice.
func emitEntries(entries []logging.StoredEntry, trailer string, opts journalOpts) {
	if opts.noPager || !isTerminal() || len(entries) == 0 && trailer == "" {
		streamEntries(os.Stdout, entries, opts.output, opts.utc, opts.noHostname, opts.outputFields)
		if trailer != "" {
			fmt.Println(trailer)
		}
		return
	}
	runPager(entries, trailer, opts)
}

// resolvePager picks the pager like systemd does: $SYSTEMD_PAGER wins over
// $PAGER, an explicitly empty value disables paging, otherwise less. The
// value is shell-split so `PAGER="less -R"` works instead of looking for a
// binary with a space in its name.
func resolvePager() (string, []string, bool) {
	if v, set := os.LookupEnv("SYSTEMD_PAGER"); set {
		if strings.TrimSpace(v) == "" {
			return "", nil, false
		}
		return splitPager(v)
	}
	if v, set := os.LookupEnv("PAGER"); set {
		if strings.TrimSpace(v) == "" {
			return "", nil, false
		}
		return splitPager(v)
	}
	return "less", nil, true
}

func splitPager(v string) (string, []string, bool) {
	parts, err := shlex.Split(v)
	if err != nil || len(parts) == 0 {
		return v, nil, true
	}
	return parts[0], parts[1:], true
}

// runPager streams entries into the pager chunk by chunk instead of joining
// them into one giant string, so large journals don't blow up memory. The
// pager runs in its own process group and SIGINT is held while it owns the
// screen: Ctrl-C then talks to the pager (less quits on it with -K) instead
// of killing us mid-frame and wedging the terminal in raw mode. less opens
// /dev/tty itself for keyboard input when its stdin is a pipe.
func runPager(entries []logging.StoredEntry, trailer string, opts journalOpts) {
	name, pargs, ok := resolvePager()
	if !ok {
		streamEntries(os.Stdout, entries, opts.output, opts.utc, opts.noHostname, opts.outputFields)
		if trailer != "" {
			fmt.Println(trailer)
		}
		return
	}
	if opts.pagerEnd && filepath.Base(name) == "less" {
		pargs = append(pargs, "+G")
	}
	pr, pw := io.Pipe()
	cmd := exec.Command(name, pargs...)
	cmd.Stdin = pr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if filepath.Base(name) == "less" && os.Getenv("LESS") == "" {
		// Upstream defaults: quit at once when everything fits one
		// screen (short output never hangs), raw control chars, chop
		// long lines, no alternate screen (nothing left behind).
		cmd.Env = append(os.Environ(), "LESS=FRSXMK")
	}
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		streamEntries(os.Stdout, entries, opts.output, opts.utc, opts.noHostname, opts.outputFields)
		if trailer != "" {
			fmt.Println(trailer)
		}
		return
	}
	signal.Ignore(syscall.SIGINT)
	streamEntries(pw, entries, opts.output, opts.utc, opts.noHostname, opts.outputFields)
	if trailer != "" {
		fmt.Fprintln(pw, trailer)
	}
	_ = pw.Close()
	_ = cmd.Wait()
	_ = pr.Close()
	signal.Reset(syscall.SIGINT)
}

type journalOpts struct {
	units           []string
	userUnits       []string
	lines           int
	linesPlus       bool
	user            bool
	system          bool
	merge           bool
	socket          string
	directory       string
	file            string
	root            string
	since           string
	until           string
	cursor          string
	afterCursor     string
	cursorFile      string
	showCursor      bool
	boot            string
	bootSet         bool
	bootFailed      bool
	identifier      string
	excludeIdentifier string
	invocation      string
	latestInvocation bool
	priority        int
	prioritySet     bool
	priorityRaw     string
	grep            string
	caseSensitive   bool
	caseSet         bool
	output          string
	outputFields    string
	reverse         bool
	utc             bool
	noHostname      bool
	noFull          bool
	all             bool
	catalog         bool
	quiet           bool
	noPager         bool
	pagerEnd        bool
	follow          bool
	noTail          bool
	truncateNewline bool
	dmesg           bool
	listBoots       bool
	diskUsage       bool
	vacuumSize      string
	vacuumFiles     string
	vacuumTime      string
	verify          bool
	sync            bool
	flush           bool
	rotate          bool
	header          bool
	listFields      bool
	field           string
	listInvocations bool
	listNamespaces  bool
}

// parseArgs handles the journalctl surface initd supports. Anything outside
// it (machines, namespaces, images, FSS) fails loudly so scripts never get
// silently partial answers.
func parseArgs(args []string) (journalOpts, error) {
	var opts journalOpts
	opts.output = "short"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unit":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a unit name", a)
			}
			opts.units = append(opts.units, args[i])
		case a == "--user-unit":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a unit name", a)
			}
			opts.userUnits = append(opts.userUnits, args[i])
		case strings.HasPrefix(a, "-u"):
			opts.units = append(opts.units, strings.TrimPrefix(a, "-u"))
		case strings.HasPrefix(a, "--unit="):
			opts.units = append(opts.units, strings.TrimPrefix(a, "--unit="))
		case strings.HasPrefix(a, "--user-unit="):
			opts.userUnits = append(opts.userUnits, strings.TrimPrefix(a, "--user-unit="))
		case a == "-n" || a == "--lines":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a number", a)
			}
			n, plus, err := parseLinesValue(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid line count %q", args[i])
			}
			opts.lines, opts.linesPlus = n, plus
		case strings.HasPrefix(a, "-n"):
			if !isCompactN(a) {
				return opts, fmt.Errorf("unknown option %q", a)
			}
			n, _ := strconv.Atoi(strings.TrimPrefix(a, "-n"))
			if n < 0 {
				return opts, fmt.Errorf("invalid line count %q", a)
			}
			opts.lines = n
		case strings.HasPrefix(a, "--lines="):
			n, plus, err := parseLinesValue(strings.TrimPrefix(a, "--lines="))
			if err != nil {
				return opts, fmt.Errorf("invalid line count %q", a)
			}
			opts.lines, opts.linesPlus = n, plus
		case a == "-S" || a == "--since":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a date", a)
			}
			opts.since = args[i]
		case strings.HasPrefix(a, "--since="):
			opts.since = strings.TrimPrefix(a, "--since=")
		case a == "-U" || a == "--until":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a date", a)
			}
			opts.until = args[i]
		case strings.HasPrefix(a, "--until="):
			opts.until = strings.TrimPrefix(a, "--until=")
		case a == "-c" || a == "--cursor":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a cursor", a)
			}
			opts.cursor = args[i]
		case strings.HasPrefix(a, "--cursor="):
			opts.cursor = strings.TrimPrefix(a, "--cursor=")
		case strings.HasPrefix(a, "--after-cursor="):
			opts.afterCursor = strings.TrimPrefix(a, "--after-cursor=")
		case a == "--after-cursor":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a cursor", a)
			}
			opts.afterCursor = args[i]
		case a == "--cursor-file":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a path", a)
			}
			opts.cursorFile = args[i]
		case strings.HasPrefix(a, "--cursor-file="):
			opts.cursorFile = strings.TrimPrefix(a, "--cursor-file=")
		case a == "--show-cursor":
			opts.showCursor = true
		case a == "-b" || a == "--boot":
			opts.bootSet = true
			// Boot values look like flags: -1, -0, ids. Only skip when
			// the next token is a known option, not merely dash-led.
			if i+1 < len(args) && isBootValue(args[i+1]) {
				i++
				opts.boot = args[i]
			}
		case strings.HasPrefix(a, "--boot="):
			opts.boot = strings.TrimPrefix(a, "--boot=")
			opts.bootSet = true
		case a == "-t" || a == "--identifier":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a string", a)
			}
			opts.identifier = args[i]
		case strings.HasPrefix(a, "--identifier="):
			opts.identifier = strings.TrimPrefix(a, "--identifier=")
		case strings.HasPrefix(a, "-t"):
			opts.identifier = strings.TrimPrefix(a, "-t")
		case a == "-T" || a == "--exclude-identifier":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a string", a)
			}
			opts.excludeIdentifier = args[i]
		case strings.HasPrefix(a, "--exclude-identifier="):
			opts.excludeIdentifier = strings.TrimPrefix(a, "--exclude-identifier=")
		case strings.HasPrefix(a, "-T"):
			opts.excludeIdentifier = strings.TrimPrefix(a, "-T")
		case a == "-I":
			opts.latestInvocation = true
		case a == "--invocation":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires an id", a)
			}
			opts.invocation = args[i]
		case strings.HasPrefix(a, "--invocation="):
			opts.invocation = strings.TrimPrefix(a, "--invocation=")
		case a == "-p" || a == "--priority":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a priority", a)
			}
			opts.priorityRaw = args[i]
		case strings.HasPrefix(a, "--priority="):
			opts.priorityRaw = strings.TrimPrefix(a, "--priority=")
		case strings.HasPrefix(a, "--facility="):
			// No facility field stored; accept and ignore so scripts
			// passing --facility don't break (unfiltered results).
		case a == "--facility":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a facility", a)
			}
			// Ignored (see above).
		case a == "-g" || a == "--grep":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a pattern", a)
			}
			opts.grep = args[i]
		case strings.HasPrefix(a, "--grep="):
			opts.grep = strings.TrimPrefix(a, "--grep=")
		case strings.HasPrefix(a, "-g"):
			opts.grep = strings.TrimPrefix(a, "-g")
		case a == "--case-sensitive":
			opts.caseSensitive, opts.caseSet = true, true
		case strings.HasPrefix(a, "--case-sensitive="):
			v := strings.TrimPrefix(a, "--case-sensitive=")
			on, err := strconv.ParseBool(v)
			if err != nil {
				return opts, fmt.Errorf("invalid --case-sensitive value %q", v)
			}
			opts.caseSensitive, opts.caseSet = on, true
		case a == "-k" || a == "--dmesg":
			opts.dmesg = true
		case a == "-o" || a == "--output":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a format", a)
			}
			opts.output = strings.ToLower(args[i])
		case strings.HasPrefix(a, "-o"):
			opts.output = strings.ToLower(strings.TrimPrefix(a, "-o"))
		case strings.HasPrefix(a, "--output="):
			opts.output = strings.ToLower(strings.TrimPrefix(a, "--output="))
		case a == "--output-fields":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a list", a)
			}
			opts.outputFields = args[i]
		case strings.HasPrefix(a, "--output-fields="):
			opts.outputFields = strings.TrimPrefix(a, "--output-fields=")
		case a == "-r" || a == "--reverse":
			opts.reverse = true
		case a == "--utc":
			opts.utc = true
		case a == "--no-hostname" || a == "-W":
			opts.noHostname = true
		case a == "--no-full":
			opts.noFull = true
		case a == "-a" || a == "--all":
			opts.all = true
		case a == "-x" || a == "--catalog":
			opts.catalog = true
		case a == "-q" || a == "--quiet":
			opts.quiet = true
		case a == "--truncate-newline":
			opts.truncateNewline = true
		case a == "-f" || a == "--follow":
			opts.follow = true
		case a == "--no-tail":
			opts.noTail = true
		case a == "-e" || a == "--pager-end":
			opts.pagerEnd = true
		case a == "--no-pager":
			opts.noPager = true
		case a == "-m" || a == "--merge":
			opts.merge = true
		case a == "-D" || a == "--directory":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a path", a)
			}
			opts.directory = args[i]
		case strings.HasPrefix(a, "--directory="):
			opts.directory = strings.TrimPrefix(a, "--directory=")
		case strings.HasPrefix(a, "--file="):
			opts.file = strings.TrimPrefix(a, "--file=")
		case a == "--file" || a == "-i":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a path", a)
			}
			opts.file = args[i]
		case strings.HasPrefix(a, "-i"):
			opts.file = strings.TrimPrefix(a, "-i")
		case strings.HasPrefix(a, "--root="):
			opts.root = strings.TrimPrefix(a, "--root=")
		case a == "--root":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a path", a)
			}
			opts.root = args[i]
		case a == "--list-boots":
			opts.listBoots = true
		case a == "--list-invocations":
			opts.listInvocations = true
		case a == "--list-namespaces":
			opts.listNamespaces = true
		case a == "--synchronize-on-exit" || strings.HasPrefix(a, "--synchronize-on-exit="):
			// Accepted: the daemon flushes on its own cadence, so there is
			// nothing extra to wait for on exit.
		case a == "--disk-usage":
			opts.diskUsage = true
		case strings.HasPrefix(a, "--vacuum-size="):
			opts.vacuumSize = strings.TrimPrefix(a, "--vacuum-size=")
		case a == "--vacuum-size":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a size", a)
			}
			opts.vacuumSize = args[i]
		case strings.HasPrefix(a, "--vacuum-files="):
			opts.vacuumFiles = strings.TrimPrefix(a, "--vacuum-files=")
		case a == "--vacuum-files":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a count", a)
			}
			opts.vacuumFiles = args[i]
		case strings.HasPrefix(a, "--vacuum-time="):
			opts.vacuumTime = strings.TrimPrefix(a, "--vacuum-time=")
		case a == "--vacuum-time":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a duration", a)
			}
			opts.vacuumTime = args[i]
		case a == "--verify":
			opts.verify = true
		case a == "--sync":
			opts.sync = true
		case a == "--flush":
			opts.flush = true
		case a == "--rotate":
			opts.rotate = true
		case a == "--header":
			opts.header = true
		case a == "-N" || a == "--fields":
			opts.listFields = true
		case a == "-F" || a == "--field":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a field", a)
			}
			opts.field = args[i]
		case strings.HasPrefix(a, "--field="):
			opts.field = strings.TrimPrefix(a, "--field=")
		case a == "--relinquish-var" || a == "--smart-relinquish-var":
			return opts, fmt.Errorf("%s is not supported: initd always logs to disk", a)
		case a == "--list-catalog" || a == "--dump-catalog" || a == "--update-catalog" ||
			a == "--setup-keys" || strings.HasPrefix(a, "--interval=") ||
			strings.HasPrefix(a, "--verify-key=") || a == "--force":
			return opts, fmt.Errorf("%s is not supported: no message catalog or sealed-hash chain", a)
		case a == "-M" || a == "--machine" || strings.HasPrefix(a, "--machine=") ||
			a == "--image" || strings.HasPrefix(a, "--image=") ||
			a == "--image-policy" || strings.HasPrefix(a, "--image-policy=") ||
			a == "--namespace" || strings.HasPrefix(a, "--namespace="):
			return opts, fmt.Errorf("%s is not supported: single-host journal only", a)
		case a == "--user":
			opts.user = true
		case a == "--system":
			opts.system = true
		case strings.HasPrefix(a, "--socket="):
			opts.socket = strings.TrimPrefix(a, "--socket=")
		case a == "--socket":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a path", a)
			}
			opts.socket = args[i]
		case strings.HasPrefix(a, "-"):
			return opts, fmt.Errorf("unknown option %q", a)
		default:
			return opts, fmt.Errorf("unexpected argument %q (did you mean -u %s?)", a, a)
		}
	}
	if opts.priorityRaw != "" {
		p, set, err := parsePriority(opts.priorityRaw)
		if err != nil {
			return opts, err
		}
		opts.priority, opts.prioritySet = p, set
	}
	if opts.caseSet && opts.grep == "" {
		return opts, fmt.Errorf("--case-sensitive needs --grep")
	}
	if opts.dmesg && (len(opts.units) > 0 || len(opts.userUnits) > 0 || opts.grep != "" || opts.identifier != "") {
		return opts, fmt.Errorf("--dmesg cannot combine with unit/identifier/grep filters")
	}
	return opts, nil
}

// parseLinesValue accepts N and +N (head instead of tail, like real -n).
func parseLinesValue(raw string) (int, bool, error) {
	s := strings.TrimSpace(raw)
	plus := strings.HasPrefix(s, "+")
	// Real journalctl takes -n all for everything; Lines==0 already
	// means unlimited downstream, so map it there.
	if strings.EqualFold(strings.TrimPrefix(s, "+"), "all") {
		return 0, plus, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "+"))
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("invalid line count %q", raw)
	}
	return n, plus, nil
}

// isCompactN reports whether a holds the compact -n<digits> form. A plain
// prefix check would misread unrelated flags, so the digits are validated.
func isCompactN(a string) bool {
	if !strings.HasPrefix(a, "-n") || len(a) <= 2 {
		return false
	}
	for _, r := range a[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func resolveUnit(client *ipc.Client, name string) (string, error) {
	// Try the name as-is, then with .service appended, mirroring systemctl.
	for _, cand := range []string{name, name + ".service"} {
		resp, err := client.Do(ipc.Request{Action: "status", Unit: cand})
		if err == nil && resp.Success {
			return cand, nil
		}
	}
	return "", fmt.Errorf("unit %s not found", name)
}

func allUnits(client *ipc.Client) ([]string, error) {
	resp, err := client.Do(ipc.Request{Action: "list-units"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	var units []ipc.UnitData
	raw, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(raw, &units)
	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Name)
	}
	return names, nil
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

func wantsVersion(args []string) bool {
	for _, a := range args {
		if a == "-V" || a == "--version" {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: journalctl [OPTIONS...] [MATCHES...]")
	fmt.Fprintln(os.Stderr, "Query the journal.")
}

func printHelp() {
	fmt.Println("journalctl [OPTIONS...] [MATCHES...]")
	fmt.Println()
	fmt.Println("Query the journal.")
	fmt.Println()
	fmt.Println("Source Options:")
	fmt.Println("      --system                Show the system journal")
	fmt.Println("      --user                  Show the user journal for the current user")
	fmt.Println("  -m, --merge                 Show entries from all available journals")
	fmt.Println("  -D, --directory=PATH        Show journal files from directory")
	fmt.Println("  -i, --file=PATH             Show journal file")
	fmt.Println("      --root=PATH             Operate on an alternate filesystem root")
	fmt.Println()
	fmt.Println("Filtering Options:")
	fmt.Println("  -S, --since=DATE            Show entries not older than the specified date")
	fmt.Println("  -U, --until=DATE            Show entries not newer than the specified date")
	fmt.Println("  -c, --cursor=CURSOR         Show entries starting at the specified cursor")
	fmt.Println("      --after-cursor=CURSOR   Show entries after the specified cursor")
	fmt.Println("      --cursor-file=FILE      Show entries after cursor in FILE and update FILE")
	fmt.Println("  -b, --boot[=ID]             Show current boot or the specified boot")
	fmt.Println("  -u, --unit=UNIT             Show logs from the specified unit")
	fmt.Println("      --user-unit=UNIT        Show logs from the specified user unit")
	fmt.Println("      --invocation=ID         Show logs from the matching invocation ID")
	fmt.Println("  -I                          Show logs from the latest invocation of unit")
	fmt.Println("  -t, --identifier=STRING     Show entries with the specified syslog identifier")
	fmt.Println("  -T, --exclude-identifier=STRING Hide entries with the specified syslog identifier")
	fmt.Println("  -p, --priority=RANGE        Show entries with the specified priority")
	fmt.Println("  -g, --grep=PATTERN          Show entries with MESSAGE matching PATTERN")
	fmt.Println("      --case-sensitive[=BOOL] Force case sensitive or insensitive matching")
	fmt.Println("  -k, --dmesg                 Show kernel message log (not supported: unit logs only)")
	fmt.Println()
	fmt.Println("Output Control Options:")
	fmt.Println("  -o, --output=STRING         Change output mode (short, short-precise,")
	fmt.Println("                                short-iso, short-full, short-monotonic, short-unix, short-delta,")
	fmt.Println("                                verbose, export, json, json-pretty, json-sse,")
	fmt.Println("                                json-seq, cat, with-unit)")
	fmt.Println("      --output-fields=LIST    Select fields to print in verbose/export/json modes")
	fmt.Println("  -n, --lines[=[+]INTEGER]    Number of journal entries to show")
	fmt.Println("  -r, --reverse               Show the newest entries first")
	fmt.Println("      --show-cursor           Print the cursor after all the entries")
	fmt.Println("      --utc                   Express time in Coordinated Universal Time (UTC)")
	fmt.Println("  -x, --catalog               Accepted (no catalog content to add)")
	fmt.Println("  -W, --no-hostname           Suppress output of hostname field")
	fmt.Println("  -a, --all                   Accepted (all fields always shown)")
	fmt.Println("  -f, --follow                Follow the journal")
	fmt.Println("      --no-tail               Show all lines, even in follow mode")
	fmt.Println("  -q, --quiet                 Do not show info messages")
	fmt.Println("      --synchronize-on-exit=BOOL Accepted (daemon flushes on its own cadence)")
	fmt.Println()
	fmt.Println("Pager Control Options:")
	fmt.Println("      --no-pager              Do not pipe output into a pager")
	fmt.Println("  -e, --pager-end             Immediately jump to the end in the pager")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  -N, --fields                List all field names currently used")
	fmt.Println("  -F, --field=FIELD           List all values that a specified field takes")
	fmt.Println("      --list-boots            Show terse information about recorded boots")
	fmt.Println("      --list-invocations      Show invocation IDs of specified unit")
	fmt.Println("      --list-namespaces       Accepted (no namespaces under initd)")
	fmt.Println("      --disk-usage            Show total disk usage of all journal files")
	fmt.Println("      --vacuum-size=BYTES     Reduce disk usage below specified size")
	fmt.Println("      --vacuum-files=INT      Leave only the specified number of journal files")
	fmt.Println("      --vacuum-time=TIME      Remove journal files older than specified time")
	fmt.Println("      --verify                Verify journal file consistency")
	fmt.Println("      --sync                  Synchronize unwritten journal messages to disk")
	fmt.Println("      --flush                 Flush all journal data to disk")
	fmt.Println("      --rotate                Request immediate rotation of the journal files")
	fmt.Println("      --header                Show journal header information")
	fmt.Println()
	fmt.Println("See the journalctl(1) man page for details.")
}
