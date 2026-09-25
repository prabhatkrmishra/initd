package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"initd/internal/build"
	"initd/internal/ipc"
	"initd/internal/userpaths"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var systemctlVersion = build.String()

func main() {
	if wantsHelp(os.Args[1:]) {
		printHelp()
		return
	}
	if wantsVersion(os.Args[1:]) {
		printVersion()
		return
	}

	// Extract --user/--system before flag parsing so they work
	// regardless of position (systemd allows them before command).
	rawArgs := os.Args[1:]
	var userFlag, systemFlag bool
	filtered := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		switch a {
		case "--user":
			userFlag = true
		case "--system":
			systemFlag = true
		case "--root", "--image":
			fmt.Fprintf(os.Stderr, "%s is not supported: operating on an alternate root would silently touch the live system instead\n", a)
			os.Exit(1)
		case "-H", "--host":
			fmt.Fprintf(os.Stderr, "remote hosts are not supported: no -H/--host in chroot\n")
			os.Exit(1)
		case "-M", "--machine":
			fmt.Fprintf(os.Stderr, "containers are not supported: no -M/--machine (single-host only)\n")
			os.Exit(1)
		case "-C", "--capsule":
			fmt.Fprintf(os.Stderr, "capsules are not supported: no -C/--capsule in chroot\n")
			os.Exit(1)
		default:
			if strings.HasPrefix(a, "--root=") || strings.HasPrefix(a, "--image=") ||
				strings.HasPrefix(a, "--image-policy=") || strings.HasPrefix(a, "-H") ||
				strings.HasPrefix(a, "--host=") || strings.HasPrefix(a, "-M") ||
				strings.HasPrefix(a, "--machine=") || strings.HasPrefix(a, "-C") ||
				strings.HasPrefix(a, "--capsule=") {
				fmt.Fprintf(os.Stderr, "%s is not supported in this chroot build (single-host only)\n", a)
				os.Exit(1)
			}
			filtered = append(filtered, a)
		}
	}
	if userFlag && systemFlag {
		fmt.Fprintln(os.Stderr, "Cannot combine --user and --system")
		os.Exit(1)
	}

	// Systemd accepts global flags on either side of the verb. The flag
	// package stops at the first positional word, so recognized compat
	// flags placed before the verb are moved into the verb args, where
	// each verb parser already handles them. Unknown flags stay put so
	// they still fail loudly in flag.Parse.
	var forwardedGlobal []string
	if idx := commandIndex(filtered); idx > 0 {
		forwardedGlobal, filtered = splitForwardedFlags(filtered, idx)
	}

	flags := flag.NewFlagSet("systemctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socketPath := flags.String("socket", "", "path to initd unix socket")
	if err := flags.Parse(filtered); err != nil {
		usage()
		os.Exit(1)
	}

	resolvedSocket := *socketPath
	if resolvedSocket == "" {
		if userFlag {
			resolvedSocket = userpaths.UserSocketPath()
			ensureUserDaemon(resolvedSocket)
		} else {
			resolvedSocket = userpaths.SystemSocketPath()
		}
	}

	if flags.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	cmd := flags.Arg(0)
	cmdArgs := append(forwardedGlobal, flags.Args()[1:]...)

	client := &ipc.Client{SocketPath: resolvedSocket}
	// Like systemd, timeouts are server-side (TimeoutStartSec/
	// TimeoutStopSec). The client just waits for the job to finish.
	// Use a longer deadline for restart/stop which blocks for
	// Stop+Start. A unit may set a large TimeoutStopSec (e.g. the
	// openclaw gateway uses 330s), so 90s was too short and the client
	// gave up with an i/o timeout while the server was still stopping.
	// 600s covers a 330s stop + 30s start with margin.
	switch cmd {
	case "restart", "stop":
		client.Timeout = 600 * time.Second
	}

	switch cmd {
	case "enable", "disable":
		os.Exit(handleEnableDisable(client, cmd, cmdArgs))
	case "show":
		handleShow(client, cmdArgs)
	case "reenable":
		// disable + enable back, like real reenable.
		if len(stripUnitFlags(cmdArgs)) < 1 {
			fmt.Fprintf(os.Stderr, "reenable requires a unit name\n")
			os.Exit(1)
		}
		code := 0
		for _, u := range stripUnitFlags(cmdArgs) {
			if c := handleEnableDisable(client, "disable", []string{u}); c != 0 && code == 0 {
				code = c
			}
			if c := handleEnableDisable(client, "enable", []string{u}); c != 0 && code == 0 {
				code = c
			}
		}
		os.Exit(code)
	case "preset", "preset-all":
		// No preset files shipped; preset UNIT behaves as enable so
		// packaging scripts don't fail. preset-all is a no-op success.
		if cmd == "preset-all" {
			break
		}
		units := stripUnitFlags(cmdArgs)
		if len(units) < 1 {
			fmt.Fprintf(os.Stderr, "preset requires a unit name\n")
			os.Exit(1)
		}
		code := 0
		for _, u := range units {
			if c := handleEnableDisable(client, "enable", []string{u}); c != 0 && code == 0 {
				code = c
			}
		}
		os.Exit(code)
	case "try-restart", "reload-or-restart", "try-reload-or-restart":
		units := stripUnitFlags(cmdArgs)
		if len(units) < 1 {
			fmt.Fprintf(os.Stderr, "%s requires a unit name\n", cmd)
			os.Exit(1)
		}
		code := 0
		for _, u := range units {
			if cmd == "try-restart" || cmd == "try-reload-or-restart" {
				// Only act when active; otherwise no-op success.
				if st := unitActiveState(client, u); st != "active" {
					continue
				}
			}
			if c := doUnitCommand(client, "restart", u, 0, false); c != 0 && code == 0 {
				code = c
			}
		}
		os.Exit(code)
	case "start", "stop", "restart", "reload", "status", "is-active", "is-enabled", "mask", "unmask", "cat", "kill":
		if cmd == "kill" {
			os.Exit(handleKillCommand(client, stripKillFlags(cmdArgs)))
		}
		logArgs, statusLines := cmdArgs, 10
		if cmd == "status" {
			logArgs, statusLines = splitStatusLogOpts(cmdArgs)
		}
		quiet := false
		if cmd == "is-active" || cmd == "is-enabled" {
			logArgs, quiet = splitQuietFlag(logArgs)
		}
		units := stripUnitFlags(logArgs)
		if len(units) < 1 {
			fmt.Fprintf(os.Stderr, "%s requires a unit name\n", cmd)
			os.Exit(1)
		}
		// Like real systemctl every unit is acted on; the exit code
		// combines the per-unit results (see runUnits).
		os.Exit(runUnits(client, cmd, units, statusLines, quiet))

	case "is-failed":
		handleIsFailed(client, cmdArgs)

	case "reset-failed":
		handleResetFailed(client, cmdArgs)

	case "list-units":
		handleListUnits(client, cmdArgs)

	case "list-unit-files":
		handleListUnitFiles(client, cmdArgs)

	case "daemon-reload", "daemon-reexec", "reboot", "poweroff", "halt":
		if cmd == "daemon-reexec" {
			fmt.Fprintf(os.Stderr, "warning: daemon-reexec treated as daemon-reload (no re-exec)\n")
			cmd = "daemon-reload"
		}
		handleSimple(client, cmd)

	case "is-system-running":
		handleIsSystemRunning(client)

	case "help":
		// Real help shows unit manuals; without man pages the unit
		// file itself is the useful reference, so map to cat.
		units := stripUnitFlags(cmdArgs)
		if len(units) < 1 {
			printHelp()
			break
		}
		code := 0
		for _, u := range units {
			if c := doUnitCommand(client, "cat", u, 0, false); c != 0 && code == 0 {
				code = c
			}
		}
		os.Exit(code)

	case "log":
		fmt.Fprintf(os.Stderr, "systemctl log has been removed, use journalctl -u UNIT [-n N]\n")
		os.Exit(1)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command verb %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func handleSimple(client *ipc.Client, action string) {
	resp, err := client.Do(ipc.Request{Action: action})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// Upstream `show` of a unit that does not exist prints nothing and
	// exits 0; the "not found" sentence is not its business.
	if !resp.Success && !isNotFoundMessage(resp.Message) {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}
}

// warnIfReloadNeeded best-effort checks whether unit files changed on disk
// since the daemon last loaded them (apt install/purge, edits, drop-ins).
// Offline or failed checks are silently ignored so degraded-mode output
// stays clean.
func warnIfReloadNeeded(client *ipc.Client) {
	resp, err := client.Do(ipc.Request{Action: "need-daemon-reload"})
	if err != nil || !resp.Success {
		return
	}
	if need, ok := resp.Data.(bool); ok && need {
		fmt.Fprintf(os.Stderr, "Warning: unit files changed on disk, run 'systemctl daemon-reload'\n")
	}
}

func handleIsSystemRunning(client *ipc.Client) {
	resp, err := client.Do(ipc.Request{Action: "is-system-running"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	state := fmt.Sprintf("%v", resp.Data)
	fmt.Println(state)

	switch state {
	case "running":
		os.Exit(0)
	case "degraded":
		os.Exit(1)
	default:
		os.Exit(3)
	}
}

// runUnits runs one verb over every unit instead of stopping after the
// first, like real systemctl. Exit codes follow the LSB table systemd
// documents: status reports failed (1) over not-active (3); is-active is 0
// when at least one unit is active; is-enabled is 0 when at least one unit
// file is enabled.
func runUnits(client *ipc.Client, action string, units []string, statusLines int, quiet bool) int {
	code := 0
	sawActive := false
	sawEnabled := false
	sawUnknown := false
	for _, u := range units {
		c := doUnitCommand(client, action, u, statusLines, quiet)
		switch action {
		case "is-active":
			if c == 0 {
				sawActive = true
			} else if c == 4 {
				sawUnknown = true
			} else if code == 0 {
				code = c
			}
		case "is-enabled":
			if c == 0 {
				sawEnabled = true
			} else if c == 4 {
				sawUnknown = true
			} else if code == 0 {
				code = c
			}
		case "status":
			if c == 1 {
				code = 1
			} else if c != 0 && code == 0 {
				code = c
			}
		default:
			if c != 0 && code == 0 {
				code = c
			}
		}
	}
	switch action {
	case "is-active":
		if sawActive {
			return 0
		}
		if sawUnknown && code == 0 {
			return 4
		}
		if code == 0 {
			return 3
		}
		return code
	case "is-enabled":
		if sawEnabled {
			return 0
		}
		if sawUnknown && code == 0 {
			return 4
		}
		if code == 0 {
			return 1
		}
		return code
	}
	return code
}

func handleEnableDisable(client *ipc.Client, action string, args []string) int {
	now := false
	units := []string{}
	for _, a := range args {
		switch {
		case a == "--now":
			now = true
		case a == "-f" || a == "--force":
			// Override symlinks like real enable -f; our enable already
			// overwrites, so just accept.
		case a == "--runtime":
			fmt.Fprintf(os.Stderr, "warning: --runtime treated as persistent (no transient store)\n")
		case a == "--global":
			fmt.Fprintf(os.Stderr, "warning: --global treated as current user (no system preset)\n")
		case a == "--no-reload" || a == "-q" || a == "--quiet":
			// Accepted and ignored for script compat.
		case isIgnoredGlobalFlag(a):
			// Output/async plumbing from before the verb; irrelevant here.
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			return 1
		default:
			units = append(units, a)
		}
	}
	if len(units) == 0 {
		fmt.Fprintf(os.Stderr, "%s requires a unit name\n", action)
		return 1
	}
	code := 0
	for _, unit := range units {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: action, Unit: resolved, Now: now})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			code = 1
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			if now && action == "enable" {
				if hint := jobFailureHint("start", resolved, resp.Message, client.Scope()); hint != "" {
					fmt.Fprint(os.Stderr, hint)
				}
			}
			code = 1
		}
	}
	return code
}

// parseShowArgs parses the argument vector of `systemctl show` into the list
// of requested properties, whether --value was requested, and the unit names.
// It tolerates unknown flags (e.g. --no-pager) and comma-separated property
// lists (--property=A,B).
func parseShowArgs(args []string) (properties []string, valueOnly bool, units []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--value" || strings.HasPrefix(a, "--value"):
			valueOnly = true
		case a == "-P":
			// -P NAME is --value --property=NAME in real systemctl.
			valueOnly = true
			if i+1 < len(args) {
				i++
				if args[i] != "" {
					properties = append(properties, strings.Split(args[i], ",")...)
				}
			}
		case strings.HasPrefix(a, "-P") && len(a) > 2:
			// Compact -P<prop>? Rare; treat remainder as property.
			valueOnly = true
			p := strings.TrimPrefix(a, "-P")
			p = strings.TrimPrefix(p, "=")
			if p != "" {
				properties = append(properties, strings.Split(p, ",")...)
			}
		case a == "-p" || a == "--property" || a == "--properties":
			if i+1 < len(args) {
				i++
				if args[i] != "" {
					properties = append(properties, strings.Split(args[i], ",")...)
				}
			}
		case strings.HasPrefix(a, "-p") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			// Compact -p<prop>.
			p := strings.TrimPrefix(a, "-p")
			p = strings.TrimPrefix(p, "=")
			if p != "" {
				properties = append(properties, strings.Split(p, ",")...)
			}
		case a == "--no-pager" || strings.HasPrefix(a, "--no-pager") ||
			a == "--no-legend" || strings.HasPrefix(a, "--no-legend") ||
			a == "-l" || a == "--full" || a == "-a" || a == "--all":
			// compatibility flag from callers; ignored
		case strings.HasPrefix(a, "--property="):
			p := strings.TrimPrefix(a, "--property=")
			if p != "" {
				properties = append(properties, strings.Split(p, ",")...)
			}
		case strings.HasPrefix(a, "--"):
			// unknown flag; ignore for the units-only query path
		default:
			units = append(units, a)
		}
	}
	return properties, valueOnly, units
}

// handleShow implements `systemctl show [options] [UNIT...]`. It parses the
// common --property and --value flags and resolves each requested property
// against the manager. Units that are not loaded are reported with
// LoadState=not-found and a successful exit code, mirroring systemd so that
// probes which merely want to verify absence (e.g. openclaw's ownership check)
// do not fail on "Failed to connect to bus".
func handleShow(client *ipc.Client, args []string) {
	properties, valueOnly, units := parseShowArgs(args)

	if len(units) == 0 {
		// No unit given: report manager-level info. When the daemon is
		// reachable, ask it. When it is not, the honest answer is a transport
		// failure like upstream's, so only UnitPath - which is knowable from
		// disk - is answered offline (openclaw's findInstalledSystemUnit
		// scans the load paths itself when the manager is unreachable);
		// anything else exits with the connection error rather than
		// inventing an "active (running)" manager that does not exist.
		resp, err := client.Do(ipc.Request{Action: "show"})
		if err == nil && resp.Success {
			printManagerProps(resp, properties, valueOnly)
			warnIfReloadNeeded(client)
			os.Exit(0)
		}
		if err != nil && printOfflineManagerProps(properties, valueOnly) {
			os.Exit(0)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	exitCode := 0
	for _, unit := range units {
		resolved, err := resolveUnitName(client, unit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}
		resp, qerr := client.Do(ipc.Request{Action: "show", Unit: resolved})
		if qerr != nil {
			// A daemon that cannot be reached cannot say whether a unit is
			// loaded, so this is the transport failure upstream reports.
			// Treating it as "not loaded" rendered a dead supervisor's running
			// services as inactive/dead, which is how a broken socket looked
			// like an idle system.
			fmt.Fprintf(os.Stderr, "%v\n", qerr)
			os.Exit(1)
		}
		loaded := resp.Success
		if !loaded {
			// Not loaded by the manager. systemd emits ALL requested properties
			// (with empty/default values) for a not-found unit and exits 0, so
			// callers that parse specific properties (e.g. openclaw's
			// readProperties("Unit", [FragmentPath, DropInPaths,
			// NeedDaemonReload, LoadState])) get a well-formed key=value for
			// each requested prop rather than a missing one that they'd then
			// mis-parse as another field's value. Emit LoadState=not-found and
			// the systemd defaults for the other standard unit properties.
			// NeedDaemonReload is filled live when the daemon is reachable
			// so absence probes don't mask a stale unit set.
			needReload := "False"
			if r, err := client.Do(ipc.Request{Action: "need-daemon-reload"}); err == nil && r.Success {
				if need, ok := r.Data.(bool); ok && need {
					needReload = "True"
				}
			}
			notFoundDefaults := map[string]string{
				"LoadState":             "not-found",
				"ActiveState":           "inactive",
				"SubState":              "dead",
				"FragmentPath":          "",
				"DropInPaths":           "",
				"NeedDaemonReload":      needReload,
				"UnitFileState":         "disabled",
				"Description":           "",
				"Restart":               "",
				"RestartSec":            "",
				"StartLimitIntervalSec": "",
				"StartLimitBurst":       "",
				"IgnoredDirectives":     "",
			}
			if len(properties) == 0 {
				keys := make([]string, 0, len(notFoundDefaults))
				for k := range notFoundDefaults {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					printProp(k, notFoundDefaults[k], valueOnly)
				}
			} else {
				for _, p := range properties {
					if v, ok := notFoundDefaults[p]; ok {
						printProp(p, v, valueOnly)
					}
				}
			}
			exitCode = 0
			continue
		}

		dataMap := map[string]string{}
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &dataMap)
		// Guarantee LoadState is present for consistency with systemd.
		if _, ok := dataMap["LoadState"]; !ok {
			dataMap["LoadState"] = "loaded"
		}
		if active, ok := dataMap["ActiveState"]; !ok || active == "" {
			dataMap["ActiveState"] = "active"
		}
		if sub, ok := dataMap["SubState"]; !ok || sub == "" {
			dataMap["SubState"] = "running"
		}

		if len(properties) == 0 {
			// Print all, sorted, like systemd.
			keys := make([]string, 0, len(dataMap))
			for k := range dataMap {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				printProp(k, dataMap[k], valueOnly)
			}
		} else {
			for _, p := range properties {
				if v, ok := dataMap[p]; ok {
					printProp(p, v, valueOnly)
				}
			}
		}
	}
	warnIfReloadNeeded(client)
	os.Exit(exitCode)
}

func printProp(key, value string, valueOnly bool) {
	if valueOnly {
		fmt.Println(value)
	} else {
		fmt.Printf("%s=%s\n", key, value)
	}
}

// printManagerProps prints the requested manager-level properties from a live
// daemon response (key=value), or just the values when --value was requested.
func printManagerProps(resp ipc.Response, properties []string, valueOnly bool) {
	dataMap := map[string]string{}
	raw, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(raw, &dataMap)
	if len(properties) == 0 {
		keys := make([]string, 0, len(dataMap))
		for k := range dataMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			printProp(k, dataMap[k], valueOnly)
		}
	} else {
		for _, p := range properties {
			if v, ok := dataMap[p]; ok {
				printProp(p, v, valueOnly)
			}
		}
	}
}

// printOfflineManagerProps answers the manager-level `systemctl show` (no
// unit) properties that are knowable without a running daemon: the standard
// unit load paths, so callers that scan them to locate a unit file (and find
// none) conclude the unit is absent rather than erroring out. It reports
// whether the request was fully answered - an unreachable manager has no
// state to report, so anything else is a transport failure, and inventing
// ActiveState=active for a daemon that is not running is exactly how a dead
// supervisor gets mistaken for a healthy one.
func printOfflineManagerProps(properties []string, valueOnly bool) bool {
	if len(properties) == 0 {
		printProp("UnitPath", systemdUnitLoadPaths, valueOnly)
		return true
	}
	answered := true
	for _, p := range properties {
		if p == "UnitPath" {
			printProp(p, systemdUnitLoadPaths, valueOnly)
			continue
		}
		answered = false
	}
	return answered
}

// systemdUnitLoadPaths mirrors systemd's default unit load path, colon
// separated, in priority order. Used by the offline `systemctl show` fallback.
const systemdUnitLoadPaths = "/etc/systemd/system:/run/systemd/system:/run/systemd/transient:/usr/lib/systemd/system:/lib/systemd/system"

func containsProp(props []string, name string) bool {
	for _, p := range props {
		if p == name {
			return true
		}
	}
	return false
}

// jobFailureHint is the follow-up line systemd prints under a failed job,
// naming the two commands that explain it. Only the job verbs get it, and
// only for the daemon's job-failure message: `status` and `is-active` report
// state, and a "unit not found" answer has nothing further to show.
func jobFailureHint(action, unit, message, scope string) string {
	switch action {
	case "start", "stop", "restart", "reload":
	default:
		return ""
	}
	if !strings.HasPrefix(message, "Job for ") {
		return ""
	}
	flag := ""
	if scope == "user" {
		flag = "--user "
	}
	return fmt.Sprintf("See \"systemctl %sstatus %s\" and \"journalctl %s-xeu %s\" for details.\n",
		flag, unit, flag, unit)
}

// doUnitCommand runs one action for one unit and returns the process exit
// code for it instead of exiting, so multi-unit verbs keep going past the
// first failure like real systemctl. Unknown units map to LSB exit 4.
func doUnitCommand(client *ipc.Client, action, unit string, statusLines int, quiet bool) int {
	// Real status accepts PIDs as well as unit names. Resolve a bare
	// number to the unit owning it so `status <pid>` works for debuggers.
	if action == "status" && isPID(unit) {
		resolved, err := resolvePIDToUnit(client, unit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			return 4
		}
		unit = resolved
	}
	resolvedUnit, err := resolveUnitName(client, unit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return 1
	}
	resp, err := client.Do(ipc.Request{Action: action, Unit: resolvedUnit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if !resp.Success {
		if isNotFoundMessage(resp.Message) {
			switch action {
			case "is-active":
				// systemd reports an unknown unit as inactive, not
				// "unknown", while still exiting 4.
				fmt.Println("inactive")
				return 4
			case "start", "stop", "restart", "reload":
				// Job verbs use EXIT_NOT_FOUND=5 (from the LSB table
				// systemd follows); reporting 1 hid "this unit does not
				// exist" behind the generic "something failed".
				fmt.Fprintf(os.Stderr, "Failed to %s %s: %s\n", action, resolvedUnit, resp.Message)
				return 5
			case "status":
				fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
				return 4
			}
		}
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		if hint := jobFailureHint(action, resolvedUnit, resp.Message, client.Scope()); hint != "" {
			fmt.Fprint(os.Stderr, hint)
		}
		return 1
	}

	switch action {
	case "status":
		status := decodeStatus(resp)
		enabled := fetchEnabledState(client, resolvedUnit)
		printStatus(status, enabled, statusLines)
		warnIfReloadNeeded(client)
		return exitForState(string(status.State))
	case "is-active":
		state := fmt.Sprintf("%v", resp.Data)
		if !quiet {
			fmt.Println(state)
		}
		warnIfReloadNeeded(client)
		if state == "active" {
			return 0
		}
		return 3
	case "is-enabled":
		state := fmt.Sprintf("%v", resp.Data)
		if !quiet {
			fmt.Println(state)
		}
		warnIfReloadNeeded(client)
		// A unit with no fragment is "not-found": systemd prints that on stdout
		// and exits 4 (EXIT_NA), distinct from a unit that exists but is off.
		if state == "not-found" {
			return 4
		}
		// systemd treats a static unit as enabled-looking: it has no
		// [Install] section to enable because something else pulls it in, and
		// that is not an error. Only disabled/masked states exit 1.
		switch state {
		case "enabled", "static", "alias", "indirect":
			return 0
		}
		return 1
	case "cat":
		content := fmt.Sprintf("%v", resp.Data)
		fmt.Print(content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Println()
		}
		return 0
	}
	return 0
}

func handleIsFailed(client *ipc.Client, args []string) {
	quiet := false
	units := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "-q" || a == "--quiet" || isIgnoredGlobalFlag(a):
			if a == "-q" || a == "--quiet" {
				quiet = true
			}
			// Accepted and ignored (quiet suppresses the print below).
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		default:
			units = append(units, a)
		}
	}
	printState := func(state string) {
		if !quiet {
			fmt.Println(state)
		}
	}
	if len(units) == 0 {
		resp, err := client.Do(ipc.Request{Action: "is-failed"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			os.Exit(1)
		}
		state := fmt.Sprintf("%v", resp.Data)
		printState(state)
		if state == "failed" {
			os.Exit(0)
		}
		os.Exit(1)
	}
	anyFailed := false
	for _, unit := range units {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: "is-failed", Unit: resolved})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			os.Exit(1)
		}
		state := fmt.Sprintf("%v", resp.Data)
		printState(state)
		if state == "failed" {
			anyFailed = true
		}
	}
	if anyFailed {
		os.Exit(0)
	}
	os.Exit(1)
}

func handleResetFailed(client *ipc.Client, args []string) {
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-q" || strings.HasPrefix(a, "--quiet") || isIgnoredGlobalFlag(a) {
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		resp, err := client.Do(ipc.Request{Action: "reset-failed"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			os.Exit(1)
		}
		return
	}
	for _, unit := range args {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: "reset-failed", Unit: resolved})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			os.Exit(1)
		}
	}
}

func handleKillCommand(client *ipc.Client, args []string) int {
	signal := ""
	units := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--signal" || a == "-s" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "kill: --signal requires an argument")
				return 1
			}
			signal = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--signal=") {
			signal = strings.TrimPrefix(a, "--signal=")
		} else if strings.HasPrefix(a, "-s=") {
			signal = strings.TrimPrefix(a, "-s=")
		} else if a == "--kill-whom" || strings.HasPrefix(a, "--kill-whom=") ||
			a == "--kill-value" || strings.HasPrefix(a, "--kill-value=") ||
			a == "--kill-subgroup" || strings.HasPrefix(a, "--kill-subgroup=") {
			// Accepted for compat; initd kills the main process.
			if (a == "--kill-whom" || a == "--kill-value" || a == "--kill-subgroup") && i+1 < len(args) {
				i++
			}
		} else if isIgnoredGlobalFlag(a) {
			// Output/async plumbing from before the verb; irrelevant here.
		} else if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			return 1
		} else {
			units = append(units, a)
		}
	}
	if len(units) == 0 {
		fmt.Fprintln(os.Stderr, "kill requires a unit name")
		return 1
	}
	code := 0
	for _, unit := range units {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: "kill", Unit: resolved, Signal: signal})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			code = 1
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
			code = 1
		}
	}
	return code
}

func resolveUnitName(_ *ipc.Client, unit string) (string, error) {
	knownSuffixes := []string{
		".service",
		".socket",
		".target",
		".mount",
		".timer",
		".path",
		".slice",
		".scope",
	}
	for _, suffix := range knownSuffixes {
		if strings.HasSuffix(unit, suffix) {
			return unit, nil
		}
	}
	if strings.HasPrefix(unit, ".") {
		return unit, nil
	}
	return unit + ".service", nil
}

// stripUnitFlags drops output/async plumbing installers add to unit verbs
// (--no-block, --quiet, status -n/-o/-l, ...) and returns the unit names.
// Flags needing a value consume the next arg so it isn't mistaken for a unit.
func stripUnitFlags(args []string) []string {
	var units []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-block" || a == "--no-warn" || a == "--wait" ||
			a == "-q" || strings.HasPrefix(a, "--quiet") ||
			a == "-l" || a == "--full" || a == "-a" || a == "--all" ||
			a == "--no-pager" || strings.HasPrefix(a, "--no-pager") ||
			a == "--with-dependencies" || a == "--reverse" ||
			isIgnoredGlobalFlag(a):
			// Accepted and ignored.
		case a == "-n" || a == "--lines" || a == "-o" || a == "--output":
			// Value flag for status; drop value too.
			if i+1 < len(args) {
				i++
			}
		case a == "--state" || a == "--type" || a == "-s" || a == "--signal" ||
			a == "--kill-whom" || a == "--kill-value" || a == "--kill-subgroup" ||
			a == "--what" || a == "--job-mode" || a == "--timestamp" ||
			a == "--message" || a == "--preset-mode" || a == "--when" ||
			a == "-p" || a == "--property" || a == "--properties" || a == "-P":
			// Value flags that are irrelevant to these verbs; drop value too.
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--lines=") || strings.HasPrefix(a, "--output=") ||
			strings.HasPrefix(a, "--state=") || strings.HasPrefix(a, "--type=") ||
			strings.HasPrefix(a, "--signal=") || strings.HasPrefix(a, "--kill-whom=") ||
			strings.HasPrefix(a, "--kill-value=") || strings.HasPrefix(a, "--kill-subgroup=") ||
			strings.HasPrefix(a, "--what=") || strings.HasPrefix(a, "--job-mode=") ||
			strings.HasPrefix(a, "--timestamp=") || strings.HasPrefix(a, "--message=") ||
			strings.HasPrefix(a, "--preset-mode=") || strings.HasPrefix(a, "--when=") ||
			strings.HasPrefix(a, "--property=") || strings.HasPrefix(a, "-s=") ||
			strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--") && isAllDigits(a[2:]) ||
			strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			// Compact -n20/-ocat or --lines=20/--output=cat; ignored.
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		default:
			units = append(units, a)
		}
	}
	return units
}

// stripKillFlags drops generic plumbing but keeps --signal/-s and kill
// targeting flags for handleKillCommand to parse.
func stripKillFlags(args []string) []string {
	var out []string
	for _, a := range args {
		switch {
		case a == "-q" || strings.HasPrefix(a, "--quiet") || a == "--no-warn" ||
			isIgnoredGlobalFlag(a):
			// Ignored.
		default:
			out = append(out, a)
		}
	}
	return out
}

// unitActiveState best-effort reports active/inactive for try-restart gating.
// Unknown units return "" so callers treat them as not active (no-op).
func unitActiveState(client *ipc.Client, unit string) string {
	resolved, _ := resolveUnitName(client, unit)
	resp, err := client.Do(ipc.Request{Action: "is-active", Unit: resolved})
	if err != nil || !resp.Success {
		return ""
	}
	return fmt.Sprintf("%v", resp.Data)
}

// isPID reports whether s is a bare process id.
func isPID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolvePIDToUnit finds the loaded unit whose MainPID matches pid.
func resolvePIDToUnit(client *ipc.Client, pid string) (string, error) {
	resp, err := client.Do(ipc.Request{Action: "list-units"})
	if err != nil {
		return "", fmt.Errorf("PID %s not found as a unit process", pid)
	}
	if !resp.Success {
		return "", fmt.Errorf("PID %s not found as a unit process", pid)
	}
	var units []ipc.UnitData
	raw, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(raw, &units)
	for _, u := range units {
		sresp, err := client.Do(ipc.Request{Action: "show", Unit: u.Name})
		if err != nil || !sresp.Success {
			continue
		}
		dataMap := map[string]string{}
		raw, _ := json.Marshal(sresp.Data)
		_ = json.Unmarshal(raw, &dataMap)
		if dataMap["MainPID"] == pid {
			return u.Name, nil
		}
	}
	return "", fmt.Errorf("PID %s not found as a unit process", pid)
}

func handleListUnits(client *ipc.Client, args []string) {
	all := false
	stateFilter := map[string]struct{}{}
	typeFilter := map[string]struct{}{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--all" || a == "-a":
			all = true
		case strings.HasPrefix(a, "--state="):
			for _, s := range strings.Split(strings.TrimPrefix(a, "--state="), ",") {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "" {
					stateFilter[s] = struct{}{}
				}
			}
		case a == "--state" && i+1 < len(args):
			i++
			for _, s := range strings.Split(args[i], ",") {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "" {
					stateFilter[s] = struct{}{}
				}
			}
		case strings.HasPrefix(a, "--type="):
			for _, t := range strings.Split(strings.TrimPrefix(a, "--type="), ",") {
				t = strings.ToLower(strings.TrimSpace(t))
				if t != "" {
					typeFilter[t] = struct{}{}
				}
			}
		case a == "--type" && i+1 < len(args):
			i++
			for _, t := range strings.Split(args[i], ",") {
				t = strings.ToLower(strings.TrimSpace(t))
				if t != "" {
					typeFilter[t] = struct{}{}
				}
			}
		case a == "--no-legend" || strings.HasPrefix(a, "--no-legend") ||
			a == "--no-pager" || strings.HasPrefix(a, "--no-pager") ||
			a == "-l" || a == "--full" || strings.HasPrefix(a, "--legend") ||
			a == "-q" || a == "--quiet" || strings.HasPrefix(a, "--quiet") ||
			a == "--failed":
			// Output formatting only; accepted and ignored so parsing
			// pipelines (list-units --no-legend --no-pager) keep working.
			if a == "--failed" {
				stateFilter["failed"] = struct{}{}
			}
		case isIgnoredGlobalFlag(a):
			// Verb-independent plumbing forwarded from before the verb.
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		default:
			fmt.Fprintf(os.Stderr, "unexpected argument %s\n", a)
			os.Exit(1)
		}
	}

	resp, err := client.Do(ipc.Request{Action: "list-units"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	var units []ipc.UnitData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &units)

	// Apply filters.
	_ = all // --all currently means show all (compat); filtering is via --state/--type
	filtered := make([]ipc.UnitData, 0, len(units))
	for _, u := range units {
		if len(stateFilter) > 0 {
			if _, ok := stateFilter[strings.ToLower(string(u.State))]; !ok {
				continue
			}
		}
		if len(typeFilter) > 0 {
			t := strings.ToLower(u.Type)
			if t == "" {
				t = "service"
			}
			if _, ok := typeFilter[t]; !ok {
				continue
			}
		}
		filtered = append(filtered, u)
	}
	units = filtered

	if len(units) == 0 {
		fmt.Println("No units loaded.")
		return
	}

	sort.Slice(units, func(i, j int) bool {
		return units[i].Name < units[j].Name
	})

	unitW := len("UNIT")
	loadW := len("loaded")
	activeW := len("ACTIVE")
	subW := len("SUB")
	for _, u := range units {
		if len(u.Name) > unitW {
			unitW = len(u.Name)
		}
		if len(string(u.State)) > activeW {
			activeW = len(string(u.State))
		}
		if len(u.SubState) > subW {
			subW = len(u.SubState)
		}
	}
	if unitW < 20 {
		unitW = 20
	}
	if unitW > 50 {
		unitW = 50
	}
	if loadW < len("LOAD") {
		loadW = len("LOAD")
	}

	// systemd's table has a SUB column between ACTIVE and DESCRIPTION;
	// scripts index these columns by position, so omitting it shifts every
	// later field.
	headerFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%-%ds  %%s\n", unitW, loadW, activeW, subW)
	rowFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%-%ds  %%s\n", unitW, loadW, activeW, subW)

	fmt.Printf(headerFmt, "UNIT", "LOAD", "ACTIVE", "SUB", "DESCRIPTION")
	fmt.Printf(headerFmt, strings.Repeat("-", unitW), strings.Repeat("-", loadW), strings.Repeat("-", activeW), strings.Repeat("-", subW), strings.Repeat("-", 11))
	for _, unit := range units {
		active := string(unit.State)
		desc := unit.Description
		if desc == "" {
			desc = "-"
		}
		name := unit.Name
		if len(name) > unitW {
			name = name[:unitW-3] + "..."
		}
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		sub := unit.SubState
		if sub == "" {
			sub = active
		}
		fmt.Printf(rowFmt, name, "loaded", active, sub, desc)
	}
	fmt.Printf("\n%d units listed.\n", len(units))
	warnIfReloadNeeded(client)
}

func handleListUnitFiles(client *ipc.Client, args []string) {
	// Real list-unit-files takes optional glob patterns; installers use
	// them for existence checks, so filter instead of listing everything.
	var patterns []string
	for _, a := range args {
		if isIgnoredGlobalFlag(a) {
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		}
		patterns = append(patterns, a)
	}
	resp, err := client.Do(ipc.Request{Action: "list-unit-files"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	var units []ipc.UnitFileData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &units)

	if len(patterns) > 0 {
		filtered := units[:0]
		for _, u := range units {
			for _, pat := range patterns {
				if ok, _ := filepath.Match(pat, u.Name); ok {
					filtered = append(filtered, u)
					break
				}
				// Bare names match the unit directly.
				if pat == u.Name {
					filtered = append(filtered, u)
					break
				}
			}
		}
		units = filtered
	}

	if len(units) == 0 {
		if len(patterns) > 0 {
			fmt.Println("No unit files matching pattern.")
		} else {
			fmt.Println("No unit files found.")
		}
		return
	}

	sort.Slice(units, func(i, j int) bool {
		return units[i].Name < units[j].Name
	})

	fileW := len("UNIT FILE")
	stateW := len("STATE")
	for _, u := range units {
		if len(u.Name) > fileW {
			fileW = len(u.Name)
		}
		if len(u.State) > stateW {
			stateW = len(u.State)
		}
	}
	// systemd lays this out as a left-aligned table: each column as wide as
	// its widest cell (header included) and one space between them, with a
	// PRESET column that initd does not implement and so prints as "-". The
	// final column is not padded and there is no dashed underline.
	headerFmt := fmt.Sprintf("%%-%ds %%-%ds %%s\n", fileW, stateW)
	fmt.Printf(headerFmt, "UNIT FILE", "STATE", "PRESET")
	for _, unit := range units {
		name := unit.Name
		if len(name) > fileW {
			name = name[:fileW-3] + "..."
		}
		fmt.Printf(headerFmt, name, unit.State, "-")
	}
	fmt.Printf("\n%d unit files listed.\n", len(units))
	warnIfReloadNeeded(client)
}

func decodeStatus(resp ipc.Response) ipc.StatusData {
	var status ipc.StatusData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &status)
	return status
}

// statusKey prints one of the fact lines systemd's status aligns in an
// eleven-column field: "     Loaded: ", "   Main PID: ", "        CPU: ".
func statusKey(key, value string) {
	fmt.Printf("%11s: %s\n", key, value)
}

// statusGlyph is the bullet before the unit name: filled while the unit is up
// or coming up, a multiplication sign for a failed one, empty when it is down.
func statusGlyph(state string) string {
	switch state {
	case "failed":
		return "×"
	case "active", "activating", "reloading", "deactivating", "stopping":
		return "●"
	default:
		return "○"
	}
}

func defaultSubState(state string) string {
	switch state {
	case "active":
		return "running"
	case "activating":
		return "start"
	case "stopping", "deactivating":
		return "stop"
	case "failed":
		return "failed"
	default:
		return "dead"
	}
}

// formatActiveLine is the Active: line: state, sub-state in parentheses (a
// failed unit names its Result there instead), and when it got there. A unit
// that is simply down says "inactive (dead)" with no timestamp.
func formatActiveLine(status ipc.StatusData) string {
	state := string(status.State)
	if state == "failed" {
		line := state
		if status.Result != "" {
			line = fmt.Sprintf("failed (Result: %s)", status.Result)
		}
		return withSince(line, status.FinishedAt, status.FinishedAtMonotonic)
	}
	if state == "inactive" {
		if sub := status.SubState; sub != "" && sub != "dead" {
			return fmt.Sprintf("inactive (%s)", sub)
		}
		return "inactive (dead)"
	}
	sub := status.SubState
	if sub == "" {
		sub = defaultSubState(state)
	}
	if state == "active" && status.LastError == "external-process" {
		sub = "external"
	}
	return withSince(fmt.Sprintf("%s (%s)", state, sub), status.StartedAt, status.StartedAtMonotonic)
}

func withSince(line string, at time.Time, monotonic time.Duration) string {
	if at.IsZero() {
		return line
	}
	return fmt.Sprintf("%s since %s; %s ago", line, at.Local().Format("Mon 2006-01-02 15:04:05 MST"), formatSince(monotonic))
}

// signalShortName is the name systemd prints for a signaled death: the
// uppercase signal without its SIG prefix (TERM, KILL, SEGV). Unknown numbers
// fall back to the raw value so the line never loses the information.
func signalShortName(n int) string {
	switch syscall.Signal(n) {
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGILL:
		return "ILL"
	case syscall.SIGTRAP:
		return "TRAP"
	case syscall.SIGABRT:
		return "ABRT"
	case syscall.SIGBUS:
		return "BUS"
	case syscall.SIGFPE:
		return "FPE"
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGUSR1:
		return "USR1"
	case syscall.SIGSEGV:
		return "SEGV"
	case syscall.SIGUSR2:
		return "USR2"
	case syscall.SIGPIPE:
		return "PIPE"
	case syscall.SIGALRM:
		return "ALRM"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGCHLD:
		return "CHLD"
	case syscall.SIGCONT:
		return "CONT"
	case syscall.SIGSTOP:
		return "STOP"
	case syscall.SIGTSTP:
		return "TSTP"
	case syscall.SIGWINCH:
		return "WINCH"
	}
	return strconv.Itoa(n)
}

// execStatusName is the reason systemd appends to the handful of exit statuses
// it synthesises for a spawn that never happened (a missing binary, an
// unenterable WorkingDirectory=, a refused credential switch). Ordinary exit
// codes get no name. Only the codes initd itself produces are mapped, and each
// string was read off a live systemd rather than guessed.
func execStatusName(n int) string {
	switch n {
	case 200:
		return "CHDIR"
	case 203:
		return "EXEC"
	case 216:
		return "GROUP"
	}
	return ""
}

// formatExecOutcome is systemd's "(code=exited, status=3)": how the main
// process was released and with what number. Empty when none ever ran.
func formatExecOutcome(status ipc.StatusData) string {
	switch status.MainCode {
	case 1:
		if name := execStatusName(status.ExitCode); name != "" {
			return fmt.Sprintf("(code=exited, status=%d/%s)", status.ExitCode, name)
		}
		return fmt.Sprintf("(code=exited, status=%d)", status.ExitCode)
	case 2:
		return fmt.Sprintf("(code=killed, signal=%s)", signalShortName(status.ExitCode))
	case 3:
		return fmt.Sprintf("(code=dumped, signal=%s)", signalShortName(status.ExitCode))
	}
	return ""
}

func processCommand(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func printStatus(status ipc.StatusData, enabled string, maxLines int) {
	unitBase := strings.TrimSuffix(status.Name, ".service")

	fmt.Printf("%s %s - %s\n", statusGlyph(string(status.State)), status.Name, status.Description)
	if enabled == "masked" {
		statusKey("Loaded", fmt.Sprintf("masked (Reason: Unit %s is masked.)", status.Name))
	} else {
		fragment := status.FragmentPath
		if fragment == "" {
			fragment = "transient"
		}
		statusKey("Loaded", fmt.Sprintf("loaded (%s; %s)", fragment, enabled))
	}
	statusKey("Active", formatActiveLine(status))

	if status.MainPID > 0 {
		mainPID := strconv.Itoa(status.MainPID)
		if name := processCommand(status.MainPID); name != "" {
			mainPID = fmt.Sprintf("%d (%s)", status.MainPID, name)
		}
		statusKey("Main PID", mainPID)
		if status.LastError == "external-process" {
			statusKey("Note", fmt.Sprintf("process running outside initd (SysV/manual/nohup); adopt with `systemctl restart %s` to supervise", status.Name))
		}
	} else if ended := formatExecOutcome(status); ended != "" {
		// systemd shows how the main process ended only once it is gone: a
		// Process: line naming the command, then the same code on Main PID.
		if status.ExecStart != "" {
			statusKey("Process", fmt.Sprintf("%d %s %s", status.ExecMainPID, status.ExecStart, ended))
		}
		statusKey("Main PID", fmt.Sprintf("%d %s", status.ExecMainPID, ended))
	}

	if status.CGroup != "" {
		statusKey("CGroup", status.CGroup)
		// Upstream's tree is capped, and every line here costs a /proc read: a
		// unit that forked a thousand ways would turn `status` into a thousand
		// reads and a thousand lines. Say what was left out, do not hide it.
		const maxTree = 20
		shown, hidden := status.CGroupPIDs, 0
		if len(shown) > maxTree {
			hidden = len(shown) - maxTree
			shown = shown[:maxTree]
		}
		for i, pid := range shown {
			glyph := "├─"
			if i == len(shown)-1 && hidden == 0 {
				glyph = "└─"
			}
			line := strconv.Itoa(pid)
			if name := processCommand(pid); name != "" {
				line = fmt.Sprintf("%d (%s)", pid, name)
			}
			// Upstream hangs the tree under the value column of the CGroup line.
			fmt.Printf("             %s %s\n", glyph, line)
		}
		if hidden > 0 {
			fmt.Printf("             └─ %d more\n", hidden)
		}
	}

	if status.LastError != "" && status.LastError != "external-process" {
		fmt.Printf("   Error: %s\n", status.LastError)
	}

	if len(status.Warnings) > 0 {
		for _, w := range status.Warnings {
			fmt.Printf("   Warning: %s\n", w)
		}
	}

	logs := status.Logs
	if maxLines >= 0 && len(logs) > maxLines {
		logs = logs[len(logs)-maxLines:]
	}
	if len(logs) > 0 {
		fmt.Println("\nLogs:")
		for _, raw := range logs {
			line := strings.TrimSpace(raw)

			// strip kernel-style monotonic prefix: [1234.567890]
			if strings.HasPrefix(line, "[") {
				if idx := strings.Index(line, "]"); idx > 0 {
					line = strings.TrimSpace(line[idx+1:])
				}
			}

			// map unit[0] → systemd[1]
			if strings.HasPrefix(line, status.Name+"[0]:") {
				line = "systemd[1]:" + strings.TrimPrefix(line, status.Name+"[0]:")
			}

			// unit.service[pid] → unit[pid]
			line = strings.ReplaceAll(line, status.Name+"[", unitBase+"[")

			fmt.Printf(" %s\n", line)
		}
	}
}

func monotonicNow() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
}

// formatSince renders how long ago a monotonic stamp was, in systemd's
// shape: at most the two most significant non-zero units, suffixed
// year/month/week/day/h/min/s/ms, with a plural only on the spelled-out
// ones. "9h", "1min 30s", "14ms", "2days 3h".
func formatSince(start time.Duration) string {
	if start <= 0 {
		return "0s"
	}
	return formatSpan(monotonicNow() - start)
}

func formatSpan(delta time.Duration) string {
	if delta < time.Millisecond {
		return "0s"
	}
	if delta < time.Second {
		return fmt.Sprintf("%dms", delta/time.Millisecond)
	}
	delta = delta.Round(time.Second)
	units := []struct {
		size   time.Duration
		suffix string
		plural bool
	}{
		{365 * 24 * time.Hour, "year", true},
		{30 * 24 * time.Hour, "month", true},
		{7 * 24 * time.Hour, "week", true},
		{24 * time.Hour, "day", true},
		{time.Hour, "h", false},
		{time.Minute, "min", false},
		{time.Second, "s", false},
	}
	parts := make([]string, 0, 2)
	for _, unit := range units {
		if len(parts) == cap(parts) {
			break
		}
		n := int(delta / unit.size)
		if n == 0 {
			continue
		}
		delta -= time.Duration(n) * unit.size
		suffix := unit.suffix
		if unit.plural && n != 1 {
			suffix += "s"
		}
		parts = append(parts, strconv.Itoa(n)+suffix)
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

func fetchEnabledState(client *ipc.Client, unit string) string {
	resp, err := client.Do(ipc.Request{Action: "is-enabled", Unit: unit})
	if err != nil || !resp.Success {
		return "disabled"
	}
	return fmt.Sprintf("%v", resp.Data)
}

// exitForState maps a unit state to the LSB exit code systemd documents:
// active is 0, failed is 1, anything else (inactive, dead, unknown) is 3.
func exitForState(state string) int {
	switch state {
	case "active":
		return 0
	case "failed":
		return 1
	default:
		return 3
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: systemctl [OPTIONS...] {COMMAND} [UNIT...]")
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func wantsVersion(args []string) bool {
	for _, arg := range args {
		if arg == "-V" || arg == "--version" {
			return true
		}
	}
	return false
}

func printHelp() {
	fmt.Println("systemctl [OPTIONS...] COMMAND [UNIT...]")
	fmt.Println()
	fmt.Println("Query or send control commands to the initd system manager.")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --user               Talk to user manager")
	fmt.Println("  --system             Talk to system manager (default)")
	fmt.Println("  --socket=PATH        Path to initd control socket")
	fmt.Println("  -h, --help           Show this help")
	fmt.Println("  -V, --version        Show version")
	fmt.Println()
	fmt.Println("Unit Commands:")
	fmt.Println("  start UNIT...        Start (activate) one or more units")
	fmt.Println("  stop UNIT...         Stop (deactivate) one or more units")
	fmt.Println("  restart UNIT...      Restart one or more units")
	fmt.Println("  reload UNIT...       Reload one or more units")
	fmt.Println("  try-restart UNIT...  Restart one or more units if active")
	fmt.Println("  reload-or-restart UNIT...  Reload if possible, else restart")
	fmt.Println("  try-reload-or-restart UNIT...  Reload if active, else restart")
	fmt.Println("  status UNIT...       Show runtime status of one or more units")
	fmt.Println("  is-active UNIT...    Check whether units are active")
	fmt.Println("  is-failed [UNIT...]  Check whether units are failed")
	fmt.Println("  is-enabled UNIT...   Check whether unit files are enabled")
	fmt.Println("  is-system-running    Check overall system state")
	fmt.Println("  enable UNIT...       Enable one or more unit files")
	fmt.Println("  disable UNIT...      Disable one or more unit files")
	fmt.Println("  reenable UNIT...     Reenable one or more unit files")
	fmt.Println("  preset UNIT...       Enable/disable based on presets")
	fmt.Println("  mask UNIT...         Mask one or more unit files")
	fmt.Println("  unmask UNIT...       Unmask one or more unit files")
	fmt.Println("  show UNIT...         Show properties of one or more units")
	fmt.Println("  cat UNIT...          Show unit file contents")
	fmt.Println("  help UNIT...         Show unit file contents")
	fmt.Println("  kill UNIT...         Send signal to unit main process")
	fmt.Println("  reset-failed [UNIT...] Reset failed state")
	fmt.Println("  list-units [OPTIONS] List loaded units (--all, --state=, --type=)")
	fmt.Println("  list-unit-files      List installed unit files")
	fmt.Println("  daemon-reload        Reload unit files")
	fmt.Println("System Commands:")
	fmt.Println("  reboot               Reboot the system")
	fmt.Println("  poweroff             Power off the system")
	fmt.Println("  halt                 Halt the system")
	fmt.Println()
	fmt.Println("Report bugs to: https://github.com/prabhatkrmishra/initd.git")
}

func printVersion() {
	fmt.Printf("systemctl (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n", systemctlVersion)
}

func ensureUserDaemon(socketPath string) {
	if strings.HasPrefix(socketPath, "@") {
		return
	}
	if userpaths.IsUserDaemonRunning() {
		return
	}
	if canDial(socketPath) {
		return
	}
	bin := findInitdBinary()
	if bin == "" {
		return
	}
	// Aim at the user socket explicitly: a bare --socket would listen on
	// the system default instead, and the dial check below would never
	// succeed. (Abstract sockets return before this point.)
	cmd := exec.Command(bin, "--socket="+socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// When root runs systemctl --user (via sudo), the spawned daemon must
	// run as the target user so socket and lock files get the correct
	// ownership. Without this, root-owned sockets on /tmp (sticky bit)
	// block the target user from connecting or cleaning up stale files.
	if os.Getuid() == 0 {
		applyTargetUser(cmd)
	}

	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Start()
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if userpaths.IsUserDaemonRunning() || canDial(socketPath) {
			return
		}
	}
}

// applyTargetUser sets the child process credentials and environment to
// match the sudo target user so the daemon creates files with correct ownership.
func applyTargetUser(cmd *exec.Cmd) {
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return
	}
	u, err := user.Lookup(sudoUser)
	if err != nil {
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(uid),
		Gid: uint32(gid),
	}
	// Rebuild environment without SUDO_* vars and with the target user's HOME.
	var cleanEnv []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(key, "SUDO_") {
			continue
		}
		cleanEnv = append(cleanEnv, e)
	}
	cleanEnv = append(cleanEnv,
		"HOME="+u.HomeDir,
		"USER="+u.Username,
		"LOGNAME="+u.Username,
	)
	cmd.Env = cleanEnv
}

func canDial(path string) bool {
	if strings.HasPrefix(path, "@") {
		path = "\x00" + strings.TrimPrefix(path, "@")
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func findInitdBinary() string {
	if p, err := exec.LookPath("initd"); err == nil {
		return p
	}
	candidates := []string{
		"/usr/local/bin/initd",
		"/usr/bin/initd",
		filepath.Join(filepath.Dir(os.Args[0]), "initd"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}
