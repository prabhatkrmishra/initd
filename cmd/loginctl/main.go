package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"initd/internal/userpaths"
)

const loginctlVersion = "1.1.0"

func main() {
	args := os.Args[1:]

	if wantsHelp(args) {
		printHelp()
		return
	}
	if wantsVersion(args) {
		printVersion()
		return
	}

	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		usage()
		os.Exit(1)
	}
	if len(positional) == 0 {
		usage()
		os.Exit(1)
	}

	verb := positional[0]
	rest := positional[1:]

	var code int
	switch verb {
	case "list-sessions":
		code = handleListSessions(opts, rest)
	case "session-status":
		code = handleSessionStatus(opts, rest)
	case "show-session":
		code = handleShowSession(opts, rest)
	case "activate":
		code = handleActivate(rest)
	case "lock-session", "unlock-session":
		code = handleLockSession(verb, rest)
	case "lock-sessions", "unlock-sessions":
		// No sessions are tracked, so there is nothing to lock.
		code = 0
	case "terminate-session", "kill-session":
		code = handleEndSession(verb, opts, rest)
	case "list-users":
		code = handleListUsers(opts, rest)
	case "user-status", "status":
		code = handleUserStatus(opts, rest)
	case "show-user":
		code = handleShowUser(opts, rest)
	case "enable-linger", "disable-linger":
		code = handleLinger(verb, rest)
	case "list-linger":
		code = handleListLinger(opts, rest)
	case "terminate-user", "kill-user":
		code = handleEndUser(verb, opts, rest)
	case "list-seats":
		code = handleListSeats(opts, rest)
	case "seat-status":
		code = handleSeatStatus(opts, rest)
	case "show-seat":
		code = handleShowSeat(opts, rest)
	case "attach", "flush-devices":
		fmt.Fprintf(os.Stderr, "%s is not supported: initd does not manage seats or device attachments\n", verb)
		code = 1
	case "terminate-seat":
		code = handleTerminateSeat(rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command verb '%s'.\n", verb)
		usage()
		code = 1
	}
	os.Exit(code)
}

// loginctlOptions carries the global options that affect output formatting.
type loginctlOptions struct {
	noLegend bool   // --no-legend: suppress header rows
	value    bool   // --value: print only the property value (with -p)
	property string // -p/--property NAME: filter show-* to one property
	all      bool   // -a/--all: show empty properties too
	jsonMode string // --json=pretty|short|off, -j
}

// parseOptions separates global options from positional arguments. Unknown
// flags are an error like real loginctl; only documented compat shims
// (--no-pager, --plain, --quiet, --no-ask-password) are silently accepted.
func parseOptions(args []string) (loginctlOptions, []string, error) {
	var opts loginctlOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-legend":
			opts.noLegend = true
		case strings.HasPrefix(a, "--legend="):
			v := strings.TrimPrefix(a, "--legend=")
			if v == "0" || v == "false" || v == "no" {
				opts.noLegend = true
			} else if v == "1" || v == "true" || v == "yes" {
				opts.noLegend = false
			} else {
				return opts, nil, fmt.Errorf("invalid --legend value %q", v)
			}
		case a == "--value":
			opts.value = true
		case a == "--no-pager" || a == "--plain" || a == "--quiet" || a == "-q" || a == "--no-ask-password":
			// Accepted and ignored.
		case a == "-l" || a == "--full" || a == "-a" || a == "--all":
			if a == "-a" || a == "--all" {
				opts.all = true
			}
			// -l/--full would disable ellipsizing; output is never
			// ellipsized, so there is nothing to do.
		case a == "-j":
			opts.jsonMode = "auto"
		case strings.HasPrefix(a, "--json="):
			v := strings.TrimPrefix(a, "--json=")
			if v != "pretty" && v != "short" && v != "off" {
				return opts, nil, fmt.Errorf("invalid --json value %q (want pretty, short or off)", v)
			}
			opts.jsonMode = v
		case a == "-n" || a == "--lines":
			// Accepted; user-status carries no journal logs to trim.
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--lines="):
			// Accepted (see above).
		case strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			// Compact -n20; accepted (see above).
		case a == "-o" || a == "--output":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--output="):
			// Accepted (see above).
		case a == "-s" || a == "--signal":
			// Accepted; there are no sessions to signal.
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--signal=") || strings.HasPrefix(a, "--kill-whom="):
			// Accepted; there are no sessions to signal.
		case a == "-H" || a == "--host" || strings.HasPrefix(a, "--host=") || strings.HasPrefix(a, "-H"):
			return opts, nil, fmt.Errorf("remote hosts are not supported: no -H/--host in chroot")
		case a == "-M" || a == "--machine" || strings.HasPrefix(a, "--machine="):
			return opts, nil, fmt.Errorf("containers are not supported: no -M/--machine (single-host only)")
		case a == "-p" || a == "--property":
			if i+1 < len(args) {
				opts.property = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--property="):
			opts.property = strings.TrimPrefix(a, "--property=")
		case strings.HasPrefix(a, "-p") && len(a) > 2:
			opts.property = a[2:]
		case a == "-P":
			// -P NAME is --value --property=NAME in real loginctl.
			opts.value = true
			if i+1 < len(args) {
				opts.property = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-P") && len(a) > 2:
			opts.value = true
			opts.property = a[2:]
		case strings.HasPrefix(a, "-"):
			return opts, nil, fmt.Errorf("unknown option %q", a)
		default:
			positional = append(positional, a)
		}
	}
	return opts, positional, nil
}

// jsonPretty reports whether list output should be JSON. "auto" (-j) means
// pretty on a tty, short otherwise; "off"/"" means the human table.
func (o loginctlOptions) jsonPretty() (bool, bool) {
	switch o.jsonMode {
	case "pretty":
		return true, true
	case "short":
		return true, false
	case "auto":
		return true, isTerminal()
	default:
		return false, false
	}
}

// --- linger store ---------------------------------------------------------

// lingerDir mirrors /var/lib/systemd/linger: one empty file per lingering
// user. Root uses the system path, everyone else their own state dir.
func lingerDir() string {
	if os.Getuid() == 0 {
		return "/var/lib/initd/linger"
	}
	return filepath.Join(userpaths.UserStateDir(), "linger")
}

func lingerPath(name string) string {
	return filepath.Join(lingerDir(), name)
}

func isLinger(name string) bool {
	st, err := os.Stat(lingerPath(name))
	return err == nil && !st.IsDir()
}

func setLinger(name string) error {
	if err := os.MkdirAll(lingerDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(lingerPath(name), []byte{}, 0o644)
}

func clearLinger(name string) {
	_ = os.Remove(lingerPath(name))
}

func lingerUsers() []string {
	entries, err := os.ReadDir(lingerDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// resolveUser accepts a name or numeric UID like real loginctl.
func resolveUser(arg string) (*user.User, error) {
	if u, err := user.Lookup(arg); err == nil {
		return u, nil
	}
	if _, err := strconv.Atoi(arg); err == nil {
		if u, err := user.LookupId(arg); err == nil {
			return u, nil
		}
	}
	return nil, fmt.Errorf("unknown user %q", arg)
}

func callerName() (string, error) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "", fmt.Errorf("unable to determine current user")
	}
	return u.Username, nil
}

// --- sessions (untracked) ---------------------------------------------------

func handleListSessions(opts loginctlOptions, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "list-sessions takes no arguments\n")
		return 1
	}
	if asJSON, pretty := opts.jsonPretty(); asJSON {
		if pretty {
			fmt.Println("[]")
		} else {
			fmt.Println("[]")
		}
		return 0
	}
	if !opts.noLegend {
		fmt.Println("SESSION UID USER SEAT LEADER CLASS TTY IDLE SINCE")
	}
	fmt.Println("0 sessions listed.")
	return 0
}

func noSuchSession(id string) int {
	fmt.Fprintf(os.Stderr, "No such session %q: initd does not track login sessions.\n", id)
	return 1
}

func handleSessionStatus(opts loginctlOptions, args []string) int {
	_ = opts
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "No session of caller: initd does not track login sessions.")
		return 1
	}
	code := 0
	for _, id := range args {
		if c := noSuchSession(id); c != 0 {
			code = c
		}
	}
	return code
}

func handleShowSession(opts loginctlOptions, args []string) int {
	if len(args) == 0 {
		props := map[string]string{
			"Type":     "manager",
			"Sessions": "0",
			"Users":    "0",
			"Seats":    "0",
		}
		return printProps(props, opts)
	}
	code := 0
	for _, id := range args {
		if c := noSuchSession(id); c != 0 {
			code = c
		}
	}
	return code
}

func handleActivate(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "No session of caller: initd does not track login sessions.")
		return 1
	}
	return noSuchSession(args[0])
}

func handleLockSession(verb string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s requires at least one session ID.\n", verb)
		return 1
	}
	code := 0
	for _, id := range args {
		if c := noSuchSession(id); c != 0 {
			code = c
		}
	}
	return code
}

func handleEndSession(verb string, opts loginctlOptions, args []string) int {
	_ = opts
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s requires at least one session ID.\n", verb)
		return 1
	}
	code := 0
	for _, id := range args {
		if c := noSuchSession(id); c != 0 {
			code = c
		}
	}
	return code
}

// --- users ------------------------------------------------------------------

type userRow struct {
	UID    string
	Name   string
	Linger string
	State  string
}

func userState(name string) string {
	if isLinger(name) {
		return "active"
	}
	return "closing"
}

func handleListUsers(opts loginctlOptions, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "list-users takes no arguments\n")
		return 1
	}
	var rows []userRow
	for _, name := range lingerUsers() {
		u, err := resolveUser(name)
		if err != nil {
			continue
		}
		rows = append(rows, userRow{UID: u.Uid, Name: u.Username, Linger: "yes", State: userState(name)})
	}
	if asJSON, pretty := opts.jsonPretty(); asJSON {
		type juser struct {
			UID    string `json:"uid"`
			User   string `json:"user"`
			Linger string `json:"linger"`
			State  string `json:"state"`
		}
		jrows := make([]juser, 0, len(rows))
		for _, r := range rows {
			jrows = append(jrows, juser{r.UID, r.Name, r.Linger, r.State})
		}
		var raw []byte
		var err error
		if pretty {
			raw, err = json.MarshalIndent(jrows, "", "  ")
		} else {
			raw, err = json.Marshal(jrows)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Println(string(raw))
		return 0
	}
	if !opts.noLegend {
		fmt.Println("UID USER LINGER STATE")
	}
	for _, r := range rows {
		fmt.Printf("%s %s %s %s\n", r.UID, r.Name, r.Linger, r.State)
	}
	fmt.Printf("%d users listed.\n", len(rows))
	return 0
}

// handleUserStatus implements `loginctl user-status [USER...]` (and the
// `status` alias kept for the old shim surface).
func handleUserStatus(opts loginctlOptions, args []string) int {
	_ = opts
	names := args
	if len(names) == 0 {
		name, err := callerName()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		names = []string{name}
	}
	code := 0
	for _, name := range names {
		u, err := resolveUser(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "User %s is not logged in.\n", name)
			code = 1
			continue
		}
		linger := "no"
		if isLinger(u.Username) {
			linger = "yes"
		}
		fmt.Printf(" %s (%s)\n", u.Username, u.Uid)
		fmt.Printf("         State: %s\n", userState(u.Username))
		fmt.Println("      Sessions: 0")
		fmt.Printf("        Linger: %s\n", linger)
	}
	return code
}

// userProps builds the show-user properties for one user.
func userProps(u *user.User) map[string]string {
	linger := "no"
	if isLinger(u.Username) {
		linger = "yes"
	}
	return map[string]string{
		"Name":     u.Username,
		"UID":      u.Uid,
		"GID":      u.Gid,
		"RealName": u.Name,
		"Home":     u.HomeDir,
		"Shell":    userShell(u.Username),
		"Linger":   linger,
		"State":    userState(u.Username),
	}
}

// printProps prints a property map for show-* verbs: KEY=VALUE at column
// zero (greppable), a single value with --value, empties skipped unless -a.
func printProps(props map[string]string, opts loginctlOptions) int {
	if opts.property != "" {
		val, ok := props[opts.property]
		if !ok {
			fmt.Fprintf(os.Stderr, "Unknown property %s.\n", opts.property)
			return 1
		}
		if opts.value {
			fmt.Println(val)
		} else {
			fmt.Printf("%s=%s\n", opts.property, val)
		}
		return 0
	}
	keys := make([]string, 0, len(props))
	for k, v := range props {
		if v == "" && !opts.all {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, props[k])
	}
	return 0
}

func handleShowUser(opts loginctlOptions, args []string) int {
	if len(args) == 0 {
		return printProps(map[string]string{
			"Type":     "manager",
			"Sessions": "0",
			"Users":    "0",
			"Seats":    "0",
		}, opts)
	}
	code := 0
	for _, name := range args {
		u, err := resolveUser(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "User %s not found.\n", name)
			code = 1
			continue
		}
		if c := printProps(userProps(u), opts); c != 0 {
			code = c
		}
	}
	return code
}

func handleLinger(verb string, args []string) int {
	names := args
	if len(names) == 0 {
		name, err := callerName()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		names = []string{name}
	}
	code := 0
	for _, name := range names {
		u, err := resolveUser(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			code = 1
			continue
		}
		if verb == "enable-linger" {
			if err := setLinger(u.Username); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to enable linger for %s: %v\n", u.Username, err)
				code = 1
			}
		} else {
			clearLinger(u.Username)
		}
	}
	return code
}

// handleListLinger implements `loginctl list-linger`, kept from the old
// shim surface so linger probes keep working.
func handleListLinger(opts loginctlOptions, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "list-linger takes no arguments\n")
		return 1
	}
	names := lingerUsers()
	if !opts.noLegend {
		fmt.Println("USER")
	}
	for _, n := range names {
		fmt.Println(n)
	}
	fmt.Printf("%d rows\n", len(names))
	return 0
}

func handleEndUser(verb string, opts loginctlOptions, args []string) int {
	_ = opts
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s requires at least one user.\n", verb)
		return 1
	}
	// No sessions are tracked, so there is nothing to stop: succeed for
	// known users exactly like real loginctl does when a user has no
	// sessions left.
	code := 0
	for _, name := range args {
		if _, err := resolveUser(name); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			code = 1
		}
	}
	return code
}

// --- seats --------------------------------------------------------------------

func handleListSeats(opts loginctlOptions, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "list-seats takes no arguments\n")
		return 1
	}
	if asJSON, pretty := opts.jsonPretty(); asJSON {
		if pretty {
			fmt.Println("[]")
		} else {
			fmt.Println("[]")
		}
		return 0
	}
	if !opts.noLegend {
		fmt.Println("SEAT")
	}
	fmt.Println("0 seats listed.")
	return 0
}

func handleSeatStatus(opts loginctlOptions, args []string) int {
	_ = opts
	if len(args) == 0 {
		fmt.Println(" Seat: seat0 (none present under initd)")
		fmt.Println("Sessions: 0")
		return 0
	}
	code := 0
	for _, name := range args {
		fmt.Fprintf(os.Stderr, "No such seat %q: initd does not manage seats.\n", name)
		code = 1
	}
	return code
}

func handleShowSeat(opts loginctlOptions, args []string) int {
	if len(args) == 0 {
		return printProps(map[string]string{
			"Type":   "manager",
			"Seats":  "0",
			"Active": "no",
		}, opts)
	}
	code := 0
	for _, name := range args {
		fmt.Fprintf(os.Stderr, "No such seat %q: initd does not manage seats.\n", name)
		code = 1
	}
	return code
}

func handleTerminateSeat(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "terminate-seat requires at least one seat name.")
		return 1
	}
	code := 0
	for _, name := range args {
		fmt.Fprintf(os.Stderr, "No such seat %q: initd does not manage seats.\n", name)
		code = 1
	}
	return code
}

// userShell returns the login shell for name from /etc/passwd, or "" if unknown.
func userShell(name string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), ":", 7)
		if len(fields) >= 7 && fields[0] == name {
			return fields[6]
		}
	}
	return ""
}

func usage() {
	fmt.Println("Usage: loginctl [OPTIONS...] COMMAND ...")
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	if len(args) > 0 && args[0] == "help" {
		return true
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

func printHelp() {
	fmt.Println("loginctl [OPTIONS...] COMMAND ...")
	fmt.Println()
	fmt.Println("Send control commands to or query the login manager.")
	fmt.Println()
	fmt.Println("Session Commands:")
	fmt.Println("  list-sessions            List sessions")
	fmt.Println("  session-status [ID...]   Show session status")
	fmt.Println("  show-session [ID...]     Show properties of sessions or the manager")
	fmt.Println("  activate [ID]            Activate a session")
	fmt.Println("  lock-session [ID...]     Screen lock one or more sessions")
	fmt.Println("  unlock-session [ID...]   Screen unlock one or more sessions")
	fmt.Println("  lock-sessions            Screen lock all current sessions")
	fmt.Println("  unlock-sessions          Screen unlock all current sessions")
	fmt.Println("  terminate-session ID...  Terminate one or more sessions")
	fmt.Println("  kill-session ID...       Send signal to processes of a session")
	fmt.Println()
	fmt.Println("User Commands:")
	fmt.Println("  list-users               List users")
	fmt.Println("  user-status [USER...]    Show user status")
	fmt.Println("  show-user [USER...]      Show properties of users or the manager")
	fmt.Println("  enable-linger [USER...]  Enable linger state of one or more users")
	fmt.Println("  disable-linger [USER...] Disable linger state of one or more users")
	fmt.Println("  terminate-user USER...   Terminate all sessions of one or more users")
	fmt.Println("  kill-user USER...        Send signal to processes of a user")
	fmt.Println("  list-linger              List users with lingering enabled")
	fmt.Println()
	fmt.Println("Seat Commands:")
	fmt.Println("  list-seats               List seats")
	fmt.Println("  seat-status [NAME...]    Show seat status")
	fmt.Println("  show-seat [NAME...]      Show properties of seats or the manager")
	fmt.Println("  attach NAME DEVICE...    Attach one or more devices to a seat")
	fmt.Println("  flush-devices            Flush all device associations")
	fmt.Println("  terminate-seat NAME...   Terminate all sessions on one or more seats")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -h, --help           Show this help")
	fmt.Println("  -V, --version        Show version")
	fmt.Println("  -p, --property=NAME  Show only the given property")
	fmt.Println("  -P NAME              Equivalent to --value --property=NAME")
	fmt.Println("  -a, --all            Show all properties, including empty ones")
	fmt.Println("      --value          With -p, print only the value")
	fmt.Println("      --no-legend      Do not show the headers and footers")
	fmt.Println("      --legend=BOOL    Enable/disable the legend")
	fmt.Println("      --no-pager       Do not pipe output into a pager")
	fmt.Println("      --json=MODE      Generate JSON output (pretty, short, off)")
	fmt.Println("  -j                   Same as --json=pretty on tty, --json=short otherwise")
	fmt.Println("  -l, --full           Do not ellipsize output")
	fmt.Println("  -n, --lines=INTEGER  Number of journal entries to show")
	fmt.Println("  -o, --output=MODE    Change journal output mode")
	fmt.Println("  -s, --signal=SIGNAL  Which signal to send")
	fmt.Println("      --kill-whom=WHOM Whom to send signal to")
	fmt.Println()
	fmt.Println("Report bugs to: https://github.com/prabhatkrmishra/initd.git")
}

func printVersion() {
	fmt.Printf("loginctl (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n", loginctlVersion)
}

// isTerminal reports whether stdout is a tty (for -j auto JSON mode).
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
