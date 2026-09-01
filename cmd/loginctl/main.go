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

	// Strip leading global options (--no-pager, --plain, --no-legend, etc.)
	// so the command verb is always the first positional argument, mirroring
	// how real loginctl tolerates options before the verb.
	positional := []string{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) == 0 {
		usage()
		os.Exit(1)
	}

	verb := positional[0]
	rest := positional[1:]

	switch verb {
	case "list-linger":
		handleListLinger(rest)
	case "enable-linger", "disable-linger":
		// No-op success. Under initd, user services are persisted by the
		// /etc/profile.d/initd.sh autostart hook (which starts enabled units on
		// every session login), so systemd-style lingering is redundant. Accept
		// the call so callers (e.g. openclaw's gateway install) don't fail on a
		// missing logind.
	case "list-users":
		handleListUsers(rest)
	case "list-sessions":
		handleListSessions(rest)
	case "show-user":
		handleShowUser(rest)
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

// handleListLinger implements `loginctl list-linger`.
//
// initd has no per-user linger table; instead its autostart hook starts enabled
// units for a user whenever they log in. Report the invoking user as lingering
// (exit success) so probes such as openclaw's install path see a populated list
// instead of failing on a missing logind (e.g. "Unable to read loginctl linger
// status").
func handleListLinger(args []string) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		fmt.Println("USER")
		fmt.Println("0 rows")
		return
	}
	fmt.Println("USER")
	fmt.Println(u.Username)
	fmt.Println("1 row")
}

// handleShowUser implements `loginctl show-user NAME` (no -p filtering).
func handleShowUser(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "show-user requires a user name")
		os.Exit(1)
	}
	u, err := user.Lookup(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "User %s not found.\n", args[0])
		os.Exit(1)
	}
	fmt.Printf("        Name=%s\n", u.Username)
	fmt.Printf("  PrimaryUID=%s\n", u.Uid)
	fmt.Printf(" PrimaryGID=%s\n", u.Gid)
	fmt.Printf("   RealName=%s\n", u.Name)
	fmt.Printf("   Directory=%s\n", u.HomeDir)
	fmt.Printf("      Shell=%s\n", userShell(u.Username))
	// initd persists this user's units via the autostart hook.
	fmt.Println("       Linger=yes")
}

// handleListUsers implements `loginctl list-users`.
func handleListUsers(args []string) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		fmt.Println("UID USER")
		fmt.Println("0 users listed.")
		return
	}
	fmt.Println("UID USER")
	fmt.Printf("%s %s\n", u.Uid, u.Username)
	fmt.Println("1 user listed.")
}

// handleListSessions implements `loginctl list-sessions` (always empty under
// initd, which does not own login sessions).
func handleListSessions(args []string) {
	fmt.Println("SESSION UID USER SEAT STATE")
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
	fmt.Println()
	fmt.Println("Report bugs to: https://github.com/prabhatkrmishra/initd.git")
}

func printVersion() {
	fmt.Printf("loginctl (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n", loginctlVersion)
}
