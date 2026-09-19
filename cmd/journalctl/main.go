package main

import (
	"encoding/json"
	"fmt"
	"initd/internal/ipc"
	"initd/internal/userpaths"
	"os"
	"strconv"
	"strings"
)

const journalctlVersion = "1.0.3"

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

	socketPath := opts.socket
	if socketPath == "" {
		if opts.user {
			socketPath = userpaths.UserSocketPath()
		} else {
			socketPath = userpaths.SystemSocketPath()
		}
	}
	client := &ipc.Client{SocketPath: socketPath}

	units := opts.units
	if len(units) == 0 {
		var err error
		units, err = allUnits(client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}

	multi := len(units) > 1
	exitCode := 0
	for _, unit := range units {
		resolved, err := resolveUnit(client, unit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			exitCode = 1
			continue
		}
		resp, err := client.Do(ipc.Request{Action: "logs", Unit: resolved, Lines: opts.lines})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			exitCode = 1
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			exitCode = 1
			continue
		}
		var lines []string
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &lines)
		if multi {
			fmt.Printf("-- %s --\n", resolved)
		}
		for _, line := range lines {
			fmt.Println(line)
		}
	}
	os.Exit(exitCode)
}

type journalOpts struct {
	units  []string
	lines  int
	user   bool
	socket string
}

// parseArgs handles the subset of journalctl flags initd supports. Pager and
// output-formatting flags are accepted and ignored so existing invocations
// keep working; follow mode is rejected because the log store is an
// in-memory ring with no stream.
func parseArgs(args []string) (journalOpts, error) {
	var opts journalOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unit":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a unit name", a)
			}
			opts.units = append(opts.units, args[i])
		case strings.HasPrefix(a, "-u"):
			opts.units = append(opts.units, strings.TrimPrefix(a, "-u"))
		case strings.HasPrefix(a, "--unit="):
			opts.units = append(opts.units, strings.TrimPrefix(a, "--unit="))
		case a == "-n" || a == "--lines":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a number", a)
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				return opts, fmt.Errorf("invalid line count %q", args[i])
			}
			opts.lines = n
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
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--lines="))
			if err != nil || n < 0 {
				return opts, fmt.Errorf("invalid line count %q", a)
			}
			opts.lines = n
		case a == "-e" || a == "--pager-end" || a == "--no-pager" ||
			a == "-q" || a == "--quiet" || a == "--no-hostname" ||
			a == "-x" || a == "--catalog":
			// Accepted and ignored: no pager, no hostname/catalog metadata.
		case strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output="):
			// Accepted and ignored: single plain-text format.
			if a == "-o" {
				i++ // consume the format name
				if i >= len(args) {
					return opts, fmt.Errorf("-o requires a format name")
				}
			}
		case a == "-f" || a == "--follow":
			return opts, fmt.Errorf("follow mode is not supported; use 'systemctl log UNIT' for a snapshot")
		case a == "--user":
			opts.user = true
		case a == "--system":
			opts.user = false
		case strings.HasPrefix(a, "--socket="):
			opts.socket = strings.TrimPrefix(a, "--socket=")
		case a == "--socket":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--socket requires a path")
			}
			opts.socket = args[i]
		case strings.HasPrefix(a, "-"):
			return opts, fmt.Errorf("unknown option %q", a)
		default:
			return opts, fmt.Errorf("unexpected argument %q (did you mean -u %s?)", a, a)
		}
	}
	return opts, nil
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
	fmt.Fprintln(os.Stderr, "Usage: journalctl [OPTIONS...]")
	fmt.Fprintln(os.Stderr, "Show logs kept by the initd manager (in-memory, newest last).")
}

func printHelp() {
	fmt.Println("journalctl [OPTIONS...]")
	fmt.Println()
	fmt.Println("Show logs kept by the initd manager (in-memory ring, newest last).")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -u, --unit=UNIT      Show logs for UNIT (repeatable; default: all units)")
	fmt.Println("  -n, --lines=N        Show only the last N lines")
	fmt.Println("  -e, --pager-end      Accepted (output always ends at newest)")
	fmt.Println("  --no-pager           Accepted (output is never paged)")
	fmt.Println("  -o, --output=FORMAT  Accepted and ignored (plain-text output)")
	fmt.Println("  --user               Talk to user manager")
	fmt.Println("  --system             Talk to system manager (default)")
	fmt.Println("  --socket=PATH        Path to initd control socket")
	fmt.Println("  -h, --help           Show this help")
	fmt.Println("  -V, --version        Show version")
	fmt.Println()
	fmt.Println("Follow mode (-f) is not supported; use 'systemctl log UNIT' for a snapshot.")
}
