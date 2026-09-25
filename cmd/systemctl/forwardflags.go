package main

import (
	"strconv"
	"strings"
)

// Forwarding lets global flags work before the command like systemd does.
// The stdlib flag parser stops at the first positional word, so compat flags
// placed before the verb (e.g. `systemctl --no-pager status foo`) would
// otherwise die in flag.Parse with a Usage error. Each verb parser already
// accepts these flags after the verb, so detecting the verb and moving the
// recognized flags from in front of it into the verb args makes both
// positions behave the same.

// takesSeparateValue reports flags whose next token is their value. Command
// detection must skip that token so it is not mistaken for the verb
// (`--property status foo` means verb `foo`, not `status`).
func takesSeparateValue(a string) bool {
	switch a {
	case "-n", "--lines", "-o", "--output", "--state", "--type",
		"-p", "--property", "--properties", "-P",
		"-s", "--signal", "--kill-whom", "--kill-value", "--kill-subgroup",
		"--what", "--job-mode", "--timestamp", "--message", "--preset-mode",
		"--when", "--socket", "-C", "--capsule", "-H", "--host",
		"-M", "--machine":
		return true
	}
	return false
}

// commandIndex returns the index of the verb: the first token that is not a
// flag, skipping values of flags that take one. -1 when there is no verb.
func commandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if takesSeparateValue(a) && i+1 < len(args) {
				i++
			}
			continue
		}
		return i
	}
	return -1
}

// isIgnoredGlobalFlag reports output/async plumbing that verbs accept
// anywhere and that changes nothing for verbs which do not use it. Systemd
// parses these globally and ignores the irrelevant ones per verb.
func isIgnoredGlobalFlag(a string) bool {
	switch a {
	case "--no-pager", "--no-warn", "--wait", "--with-dependencies", "--reverse",
		"--before", "--after", "-r", "--recursive", "--failed",
		"--dry-run", "--plain", "--no-wall", "--no-ask-password", "--show-types",
		"--show-transaction", "-T", "--value", "-i", "-v", "--verbose",
		"-l", "--full", "-a", "--all", "-q", "--no-legend":
		return true
	}
	for _, p := range []string{
		"--legend", "--no-pager", "--quiet", "--no-legend",
		"--check-inhibitors", "--timestamp", "--message", "--job-mode",
		"--what", "--preset-mode", "--when",
	} {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// forwardableFlag matches one compat flag at args[i] (i < end) that may live
// before the verb. It returns the tokens to move and how many slots they
// occupy. Unknown flags return nil so they still fail loudly in flag.Parse.
func forwardableFlag(args []string, i, end int) ([]string, int) {
	a := args[i]
	if isIgnoredGlobalFlag(a) {
		return []string{a}, 1
	}
	// The enable family owns these; forward them so `systemctl --now
	// enable foo` works, the verb parser applies them.
	switch a {
	case "--now", "--runtime", "--global", "--no-reload", "-f", "--force":
		return []string{a}, 1
	}
	// Value flags in --flag=value form move as one token.
	for _, p := range []string{
		"--lines=", "--output=", "--state=", "--type=",
		"--property=", "--signal=", "--kill-whom=", "--kill-value=",
		"--kill-subgroup=", "--what=", "--job-mode=", "--timestamp=",
		"--message=", "--preset-mode=", "--when=",
	} {
		if strings.HasPrefix(a, p) {
			return []string{a}, 1
		}
	}
	// Compact shorts: -n20, -ocat, -pName, -PName, -sSIG.
	if strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--") {
		return []string{a}, 1
	}
	if strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "--") {
		return []string{a}, 1
	}
	if strings.HasPrefix(a, "-p") && len(a) > 2 && !strings.HasPrefix(a, "--") {
		return []string{a}, 1
	}
	if strings.HasPrefix(a, "-P") && len(a) > 2 {
		return []string{a}, 1
	}
	if strings.HasPrefix(a, "-s=") {
		return []string{a}, 1
	}
	// Separate-value form: move the flag plus its value.
	switch a {
	case "-n", "--lines", "-o", "--output", "--state", "--type",
		"-p", "--property", "--properties", "-P",
		"-s", "--signal", "--kill-whom", "--kill-value", "--kill-subgroup",
		"--what", "--job-mode", "--timestamp", "--message", "--preset-mode",
		"--when":
		if i+1 < end {
			return []string{a, args[i+1]}, 2
		}
		return []string{a}, 1
	}
	return nil, 0
}

// splitForwardedFlags pulls recognized compat flags out of args[:cmdIndex]
// and returns them plus the remaining args (everything from the verb on,
// with theExtras removed from the front).
func splitForwardedFlags(args []string, cmdIndex int) ([]string, []string) {
	var forwarded, kept []string
	i := 0
	for i < cmdIndex {
		if toks, n := forwardableFlag(args, i, cmdIndex); toks != nil {
			forwarded = append(forwarded, toks...)
			i += n
			continue
		}
		kept = append(kept, args[i])
		i++
	}
	kept = append(kept, args[cmdIndex:]...)
	return forwarded, kept
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitStatusLogOpts extracts -n/--lines for status (last wins, default 10
// like systemd, "all" means unlimited) and returns the remaining args.
func splitStatusLogOpts(args []string) ([]string, int) {
	lines := 10
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-n" || a == "--lines" {
			if i+1 < len(args) {
				i++
				if strings.EqualFold(args[i], "all") {
					lines = -1
				} else if n, err := strconv.Atoi(args[i]); err == nil && n >= 0 {
					lines = n
				}
			}
			continue
		}
		if strings.HasPrefix(a, "--lines=") {
			v := strings.TrimPrefix(a, "--lines=")
			if strings.EqualFold(v, "all") {
				lines = -1
			} else if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				lines = n
			}
			continue
		}
		if strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--") && isAllDigits(a[2:]) {
			if n, err := strconv.Atoi(a[2:]); err == nil {
				lines = n
			}
			continue
		}
		rest = append(rest, a)
	}
	return rest, lines
}

// splitQuietFlag pulls -q/--quiet out of verb args (both positions end up here
// after forwarding) and reports whether output should be suppressed.
func splitQuietFlag(args []string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	quiet := false
	for _, a := range args {
		if a == "-q" || a == "--quiet" {
			quiet = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, quiet
}

// isNotFoundMessage reports daemon errors meaning "the unit itself is unknown",
// which systemd maps to the not-found exit codes (4 for queries, 5 for job
// verbs). Substring matching on errno prose is not enough: "open
// /etc/foo.conf: no such file or directory" is a start failure of a unit that
// exists, and calling it not-found tells the caller the unit is missing.
func isNotFoundMessage(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if !strings.HasPrefix(m, "unit ") {
		return false
	}
	return strings.Contains(m, "not found") ||
		strings.Contains(m, "not loaded") ||
		strings.Contains(m, "could not be found")
}
