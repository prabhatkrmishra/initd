package main

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strings"
)

const loginctlVersion = "1.0.1"

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

	opts, positional := parseOptions(args)
	if len(positional) == 0 {
		usage()
		os.Exit(1)
	}

	verb := positional[0]
	rest := positional[1:]

	switch verb {
	case "list-linger":
		handleListLinger(opts, rest)
	case "enable-linger", "disable-linger":
		// No-op success. Under initd, user services are persisted by the
		// /etc/profile.d/initd.sh autostart hook (which starts enabled units on
		// every session login), so systemd-style lingering is redundant. Accept
		// the call so callers (e.g. openclaw's gateway install) don't fail on a
		// missing logind.
	case "list-users":
		handleListUsers(opts, rest)
	case "list-sessions":
		handleListSessions(opts, rest)
	case "show-user":
		handleShowUser(opts, rest)
	case "status":
		handleStatus(rest)
	case "show-seat", "show-session", "attach", "lock-session", "unlock-session",
		"terminate-seat", "terminate-session", "terminate-user", "kill-session",
		"kill-user":
		// Session-management subcommands are irrelevant (and unimplemented)
		// under initd, which does not own login sessions. Report success so
		// generic probes don't abort.
	default:
		fmt.Fprintf(os.Stderr, "Unknown command verb '%s'.\n", verb)
		usage()
		os.Exit(1)
	}
}

// loginctlOptions carries the global options that affect output formatting.
type loginctlOptions struct {
	noLegend bool   // --no-legend: suppress header rows
	value    bool   // --value: print only the property value (with -p)
	property string // -p/--property NAME: filter show-user to one property
}

// parseOptions separates global options from positional arguments. Unlike the
// previous naive "drop anything starting with -" loop, it consumes the value of
// -p/--property so a value that itself starts with '-' is not mistaken for a
// flag, and it records the formatting flags that change output.
func parseOptions(args []string) (loginctlOptions, []string) {
	var opts loginctlOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-legend":
			opts.noLegend = true
		case a == "--value":
			opts.value = true
		case a == "--no-pager" || a == "--plain" || a == "--quiet" || a == "-q":
			// accepted and ignored
		case a == "-p" || a == "--property":
			if i+1 < len(args) {
				opts.property = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--property="):
			opts.property = strings.TrimPrefix(a, "--property=")
		case strings.HasPrefix(a, "-p") && len(a) > 2:
			opts.property = a[2:]
		case strings.HasPrefix(a, "-"):
			// Unknown option; ignore it like real loginctl tolerates.
		default:
			positional = append(positional, a)
		}
	}
	return opts, positional
}

// handleListLinger implements `loginctl list-linger`.
//
// initd has no per-user linger table; instead its autostart hook starts enabled
// units for a user whenever they log in. Report the invoking user as lingering
// (exit success) so probes such as openclaw's install path see a populated list
// instead of failing on a missing logind (e.g. "Unable to read loginctl linger
// status").
func handleListLinger(opts loginctlOptions, args []string) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		if !opts.noLegend {
			fmt.Println("USER")
		}
		fmt.Println("0 rows")
		return
	}
	if !opts.noLegend {
		fmt.Println("USER")
	}
	fmt.Println(u.Username)
	fmt.Println("1 row")
}

// handleShowUser implements `loginctl show-user NAME`, honoring -p/--property
// and --value so callers can extract a single value (e.g. Linger) cleanly.
func handleShowUser(opts loginctlOptions, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "show-user requires a user name")
		os.Exit(1)
	}
	u, err := user.Lookup(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "User %s not found.\n", args[0])
		os.Exit(1)
	}
	props := map[string]string{
		"Name":        u.Username,
		"PrimaryUID":  u.Uid,
		"PrimaryGID":  u.Gid,
		"RealName":    u.Name,
		"Directory":   u.HomeDir,
		"Shell":       userShell(u.Username),
		"Linger":      "yes",
	}
	if opts.property != "" {
		val, ok := props[opts.property]
		if !ok {
			fmt.Fprintf(os.Stderr, "Unknown property %s.\n", opts.property)
			os.Exit(1)
		}
		if opts.value {
			fmt.Println(val)
		} else {
			fmt.Printf("%s=%s\n", opts.property, val)
		}
		return
	}
	fmt.Printf("        Name=%s\n", props["Name"])
	fmt.Printf("  PrimaryUID=%s\n", props["PrimaryUID"])
	fmt.Printf(" PrimaryGID=%s\n", props["PrimaryGID"])
	fmt.Printf("   RealName=%s\n", props["RealName"])
	fmt.Printf("   Directory=%s\n", props["Directory"])
	fmt.Printf("      Shell=%s\n", props["Shell"])
	// initd persists this user's units via the autostart hook.
	fmt.Println("       Linger=yes")
}

// handleListUsers implements `loginctl list-users`.
func handleListUsers(opts loginctlOptions, args []string) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		if !opts.noLegend {
			fmt.Println("UID USER")
		}
		fmt.Println("0 users listed.")
		return
	}
	if !opts.noLegend {
		fmt.Println("UID USER")
	}
	fmt.Printf("%s %s\n", u.Uid, u.Username)
	fmt.Println("1 user listed.")
}

// handleListSessions implements `loginctl list-sessions` (always empty under
// initd, which does not own login sessions).
func handleListSessions(opts loginctlOptions, args []string) {
	if !opts.noLegend {
		fmt.Println("SESSION UID USER SEAT STATE")
	}
	fmt.Println("0 sessions listed.")
}

// handleStatus implements `loginctl status [USER]`.
func handleStatus(args []string) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	} else {
		u, err := user.Current()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to determine current user.")
			os.Exit(1)
		}
		name = u.Username
	}
	u, err := user.Lookup(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "User %s not logged in.\n", name)
		os.Exit(1)
	}
	fmt.Printf(" %s (%s)\n", name, u.Uid)
	fmt.Println("         Sessions: 0")
	fmt.Println("          Linger: yes")
}

// userShell returns the login shell for name from /etc/passwd, or "" if unknown.
func userShell(name string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), ":", 7)
		if len(fields) >= 7 && fields[0] == name {
			return fields[6]
		}
	}
	return ""
}

func usage() {
	fmt.Println("Usage: loginctl [OPTIONS...] COMMAND [USER...]")
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
	fmt.Println("loginctl [OPTIONS...] COMMAND [USER...]")
	fmt.Println()
	fmt.Println("Query or send control commands related to login sessions, users and")
	fmt.Println("linger state. This is the initd compatibility shim: under initd there")
	fmt.Println("is no logind/login1, so user services are persisted via the autostart")
	fmt.Println("hook instead of lingering.")
	fmt.Println()
	fmt.Println("Linger Commands:")
	fmt.Println("  list-linger            List users with lingering enabled")
	fmt.Println("  enable-linger USER...  Enable lingering for a user (no-op under initd)")
	fmt.Println("  disable-linger USER... Disable lingering for a user (no-op under initd)")
	fmt.Println()
	fmt.Println("User Commands:")
	fmt.Println("  show-user USER...      Show properties of a user")
	fmt.Println("  list-users             List logged-in users")
	fmt.Println("  list-sessions          List sessions")
	fmt.Println("  status [USER]          Show user status")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -h, --help           Show this help")
	fmt.Println("  -V, --version        Show version")
	fmt.Println("  -p, --property=NAME  Show only the given property (show-user)")
	fmt.Println("      --value          With -p, print only the value")
	fmt.Println("      --no-legend      Suppress header rows in list commands")
	fmt.Println("      --no-pager       Accepted and ignored")
	fmt.Println()
	fmt.Println("Report bugs to: https://github.com/prabhatkrmishra/initd.git")
}

func printVersion() {
	fmt.Printf("loginctl (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n", loginctlVersion)
}
