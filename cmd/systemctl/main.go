package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"initd/internal/ipc"
	"initd/internal/userpaths"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const systemctlVersion = "0.0.2"

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
		default:
			filtered = append(filtered, a)
		}
	}
	if userFlag && systemFlag {
		fmt.Fprintln(os.Stderr, "Cannot combine --user and --system")
		os.Exit(1)
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
	cmdArgs := flags.Args()[1:]

	client := &ipc.Client{SocketPath: resolvedSocket}

	switch cmd {
	case "enable", "disable":
		handleEnableDisable(client, cmd, cmdArgs)
	case "start", "stop", "restart", "reload", "status", "is-active", "is-enabled", "mask", "unmask", "show", "cat", "kill":
		if cmd == "kill" {
			handleKillCommand(client, cmdArgs)
			break
		}
		if len(cmdArgs) < 1 {
			fmt.Fprintf(os.Stderr, "%s requires a unit name\n", cmd)
			os.Exit(1)
		}
		handleUnitCommand(client, cmd, cmdArgs[0])

	case "is-failed":
		handleIsFailed(client, cmdArgs)

	case "reset-failed":
		handleResetFailed(client, cmdArgs)

	case "list-units":
		handleListUnits(client, cmdArgs)

	case "list-unit-files":
		handleListUnitFiles(client)

	case "daemon-reload", "reboot", "poweroff", "halt":
		handleSimple(client, cmd)

	case "is-system-running":
		handleIsSystemRunning(client)

	default:
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
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
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

func handleEnableDisable(client *ipc.Client, action string, args []string) {
	now := false
	units := []string{}
	for _, a := range args {
		if a == "--now" {
			now = true
		} else if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		} else {
			units = append(units, a)
		}
	}
	if len(units) == 0 {
		fmt.Fprintf(os.Stderr, "%s requires a unit name\n", action)
		os.Exit(1)
	}
	for _, unit := range units {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: action, Unit: resolved, Now: now})
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

func handleUnitCommand(client *ipc.Client, action, unit string) {
	resolvedUnit, err := resolveUnitName(client, unit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
	resp, err := client.Do(ipc.Request{Action: action, Unit: resolvedUnit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	switch action {
	case "status":
		status := decodeStatus(resp)
		enabled := fetchEnabledState(client, resolvedUnit)
		printStatus(status, enabled)
		exitForState(string(status.State))
	case "is-active":
		state := fmt.Sprintf("%v", resp.Data)
		fmt.Println(state)
		if state == "active" {
			os.Exit(0)
		}
		os.Exit(3)
	case "is-enabled":
		state := fmt.Sprintf("%v", resp.Data)
		fmt.Println(state)
		if state == "enabled" {
			os.Exit(0)
		}
		os.Exit(1)
	case "show":
		dataMap := map[string]string{}
		raw, _ := json.Marshal(resp.Data)
		_ = json.Unmarshal(raw, &dataMap)
		keys := make([]string, 0, len(dataMap))
		for k := range dataMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, dataMap[k])
		}
	case "cat":
		content := fmt.Sprintf("%v", resp.Data)
		fmt.Print(content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Println()
		}
	}
}

func handleIsFailed(client *ipc.Client, args []string) {
	if len(args) == 0 {
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
		fmt.Println(state)
		if state == "failed" {
			os.Exit(0)
		}
		os.Exit(1)
	}
	anyFailed := false
	for _, unit := range args {
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
		fmt.Println(state)
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

func handleKillCommand(client *ipc.Client, args []string) {
	signal := ""
	units := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--signal" || a == "-s" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "kill: --signal requires an argument")
				os.Exit(1)
			}
			signal = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--signal=") {
			signal = strings.TrimPrefix(a, "--signal=")
		} else if strings.HasPrefix(a, "-s=") {
			signal = strings.TrimPrefix(a, "-s=")
		} else if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown option %s\n", a)
			os.Exit(1)
		} else {
			units = append(units, a)
		}
	}
	if len(units) == 0 {
		fmt.Fprintln(os.Stderr, "kill requires a unit name")
		os.Exit(1)
	}
	for _, unit := range units {
		resolved, _ := resolveUnitName(client, unit)
		resp, err := client.Do(ipc.Request{Action: "kill", Unit: resolved, Signal: signal})
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
	for _, u := range units {
		if len(u.Name) > unitW {
			unitW = len(u.Name)
		}
		if len(string(u.State)) > activeW {
			activeW = len(string(u.State))
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

	headerFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%s\n", unitW, loadW, activeW)
	rowFmt := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%s\n", unitW, loadW, activeW)

	fmt.Printf(headerFmt, "UNIT", "LOAD", "ACTIVE", "DESCRIPTION")
	fmt.Printf(headerFmt, strings.Repeat("-", unitW), strings.Repeat("-", loadW), strings.Repeat("-", activeW), strings.Repeat("-", 11))
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
		fmt.Printf(rowFmt, name, "loaded", active, desc)
	}
	fmt.Printf("\n%d units listed.\n", len(units))
}

func handleListUnitFiles(client *ipc.Client) {
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

	if len(units) == 0 {
		fmt.Println("No unit files found.")
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
	if fileW < 20 {
		fileW = 20
	}
	if fileW > 50 {
		fileW = 50
	}

	headerFmt := fmt.Sprintf("%%-%ds  %%-%ds\n", fileW, stateW)
	fmt.Printf(headerFmt, "UNIT FILE", "STATE")
	fmt.Printf(headerFmt, strings.Repeat("-", fileW), strings.Repeat("-", stateW))
	for _, unit := range units {
		name := unit.Name
		if len(name) > fileW {
			name = name[:fileW-3] + "..."
		}
		fmt.Printf(headerFmt, name, unit.State)
	}
	fmt.Printf("\n%d unit files listed.\n", len(units))
}

func decodeStatus(resp ipc.Response) ipc.StatusData {
	var status ipc.StatusData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &status)
	return status
}

func printStatus(status ipc.StatusData, enabled string) {
	unitBase := strings.TrimSuffix(status.Name, ".service")

	fmt.Printf("● %s - %s\n", status.Name, status.Description)
	fmt.Printf("   Loaded: loaded (%s; %s)\n", status.Name, enabled)

	activeLine := string(status.State)
	if status.State == "active" {
		activeLine = "active (running)"
	}

	if !status.StartedAt.IsZero() {
		startedAt := status.StartedAt.Local()
		monotonicSince := formatSince(status.StartedAtMonotonic)
		fmt.Printf(
			"   Active: %s since %s; %s ago\n",
			activeLine,
			startedAt.Format("Mon, 02 Jan 2006 15:04:05 MST"),
			monotonicSince,
		)
	} else {
		fmt.Printf("   Active: %s\n", activeLine)
	}

	if status.MainPID > 0 {
		fmt.Printf(" Main PID: %d\n", status.MainPID)
	}

	if status.LastError != "" {
		fmt.Printf("   Error: %s\n", status.LastError)
	}

	if len(status.Logs) > 0 {
		fmt.Println("\nLogs:")
		for _, raw := range status.Logs {
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

func formatSince(start time.Duration) string {
	if start <= 0 {
		return "0s"
	}
	now := monotonicNow()
	if now <= start {
		return "0s"
	}
	delta := now - start
	if delta < 0 {
		return "0s"
	}
	delta = delta.Round(time.Second)
	seconds := int(delta.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, secs)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func fetchEnabledState(client *ipc.Client, unit string) string {
	resp, err := client.Do(ipc.Request{Action: "is-enabled", Unit: unit})
	if err != nil || !resp.Success {
		return "disabled"
	}
	return fmt.Sprintf("%v", resp.Data)
}

func exitForState(state string) {
	switch state {
	case "active":
		os.Exit(0)
	case "failed":
		os.Exit(1)
	default:
		os.Exit(3)
	}
}

func usage() {
	fmt.Println("Usage: systemctl [OPTIONS...] {COMMAND} [UNIT...]")
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	if len(args) > 0 && args[0] == "help" {
		return true
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
	fmt.Println("  status UNIT...       Show runtime status of one or more units")
	fmt.Println("  is-active UNIT...    Check whether units are active")
	fmt.Println("  is-failed [UNIT...]  Check whether units are failed")
	fmt.Println("  is-enabled UNIT...   Check whether unit files are enabled")
	fmt.Println("  is-system-running    Check overall system state")
	fmt.Println("  enable UNIT...       Enable one or more unit files")
	fmt.Println("  disable UNIT...      Disable one or more unit files")
	fmt.Println("  mask UNIT...         Mask one or more unit files")
	fmt.Println("  unmask UNIT...       Unmask one or more unit files")
	fmt.Println("  show UNIT...         Show properties of one or more units")
	fmt.Println("  cat UNIT...          Show unit file contents")
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
	cmd := exec.Command(bin, "--socket")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
