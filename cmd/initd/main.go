package main

import (
	"context"
	"fmt"
	"initd/internal/boot"
	"initd/internal/dbus"
	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/supervisor"
	"initd/internal/userpaths"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const initdVersion = "1.1.0"

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "%v", err)
		os.Exit(1)
	}
	socketPath := cfg.socketPath
	initMode := cfg.initMode

	// A supervisor must not die with its launching terminal. Ignore hangup
	// in every mode; --daemonize additionally leaves the session entirely.
	signal.Ignore(syscall.SIGHUP)

	if cfg.daemonize && os.Getpid() == 1 {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
		 "--daemonize refused under PID 1: the initial parent exit would panic the kernel; run without --daemonize as init")
		os.Exit(1)
	}
	if cfg.daemonize && os.Getenv("INITD_DAEMONIZED") != "1" {
		if err := spawnDetached(cfg); err != nil {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "daemonize: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if cfg.pidFile != "" {
		if err := writePidFile(cfg.pidFile); err != nil {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "pid file %s: %v", cfg.pidFile, err)
			os.Exit(1)
		}
	}

	signals := make(chan os.Signal, 16)

	if initMode {
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGCHLD)
	} else {
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	}

	systemManager := supervisor.NewSystemManager()
	userManager := supervisor.NewUserManager()

	if os.Getpid() == 1 {
		reaper := supervisor.NewProcessReaper()
		reaper.Start()
		systemManager.SetReaper(reaper)
		userManager.SetReaper(reaper)
	}

	if err := systemManager.LoadUnits(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "failed to load system units: %v", err)
	}
	if err := userManager.LoadUnits(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "failed to load user units: %v", err)
	}
	// Durable logs are best-effort: a read-only /var/log must not stop
	// supervision, the rings just stay RAM-only.
	if err := systemManager.OpenJournal(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "system journal unavailable: %v", err)
	}
	if err := userManager.OpenJournal(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "user journal unavailable: %v", err)
	}

	// Fallback for non-root: /run/initd.sock not writable
	if socketPath == "/run/initd.sock" && os.Getuid() != 0 {
		if _, err := os.Stat("/run"); err == nil {
			if f, err := os.OpenFile("/run/initd.sock.test", os.O_CREATE|os.O_WRONLY, 0600); err != nil {
				fallback := userpaths.SystemSocketPath()
				if fallback == "/run/initd.sock" {
					fallback = "/tmp/initd.sock"
				}
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"no permission for %s, falling back to %s", socketPath, fallback)
				socketPath = fallback
			} else {
				_ = f.Close()
				_ = os.Remove("/run/initd.sock.test")
			}
		}
	}

	serveManager := func(path string, mgr *supervisor.Manager) {
		for {
			if err := ipc.Serve(path, mgr); err != nil {
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"ipc server error on %s: %v (retrying)", path, err)
				time.Sleep(time.Second)
				continue
			}
		}
	}

	// Singleton for the user manager: same user logging in many times must
	// not create duplicate daemons/sockets. Use a flock on a per-user lock
	// file and probe the socket to handle stale files.
	var userLock *os.File
	startUserManager := func(path string, mgr *supervisor.Manager) {
		if lock, err := userpaths.AcquireUserLock(); err == nil {
			userLock = lock
			go serveManager(path, mgr)
			return
		}
		if userpaths.IsUserDaemonRunning() {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"user daemon already running at %s - skipping", path)
			return
		}
		// Stale socket/lock - clean up and try once more.
		_ = os.Remove(path)
		if lock, err := userpaths.AcquireUserLock(); err == nil {
			userLock = lock
			go serveManager(path, mgr)
		} else {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"failed to acquire user lock for %s: %v", path, err)
		}
	}

	// Singleton for the system manager too — two concurrent `initd --init`
	// invocations (e.g. two logins racing) must not both bind /run/initd.sock.
	var systemLock *os.File
	startSystemManager := func(path string, mgr *supervisor.Manager) {
		if lock, err := userpaths.AcquireSystemLock(); err == nil {
			systemLock = lock
			go serveManager(path, mgr)
			return
		}
		if userpaths.IsSystemDaemonRunning() {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"system daemon already running at %s - skipping", path)
			return
		}
		_ = os.Remove(path)
		if lock, err := userpaths.AcquireSystemLock(); err == nil {
			systemLock = lock
			go serveManager(path, mgr)
		} else {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"failed to acquire system lock for %s: %v", path, err)
		}
	}

	startSystemManager(socketPath, systemManager)
	userSocket := userpaths.UserSocketPath()
	if userSocket != socketPath {
		startUserManager(userSocket, userManager)
	}
	// Hold the locks for the lifetime of the daemon.
	if userLock != nil {
		defer func() {
			_ = userLock.Close()
		}()
	}
	if systemLock != nil {
		defer func() {
			_ = systemLock.Close()
		}()
	}

	// Advertise org.freedesktop.systemd1 over D-Bus so that tools which
	// talk to the systemd1 D-Bus interface (e.g. /usr/bin/systemctl in
	// system scope, openclaw's ownership probe) get verifiable answers
	// instead of "Failed to connect to bus: Permission denied". The user
	// (session) bus is always attempted; the system bus is attempted too
	// but is non-fatal: if initd is not root or no system dbus-daemon is
	// reachable, we simply skip it and rely on the user bus instead.
	go startDBusServers(systemManager, userManager)

	if initMode {
		// System units live under /etc/systemd/system and /usr/lib/systemd/system
		// and require root to start. A non-root daemon must not attempt them:
		// they fail with EACCES and, with Restart=always, spin in a permission
		// denied restart storm that also blocks the user manager's startMu.
		// User units must be started by the per-user manager, not by the
		// system (root) manager. When running as root, only start system
		// units; when running as a user, only start user units. This mirrors
		// systemd's separation of PID 1 (system) and systemd --user.
		isRoot := os.Getuid() == 0

		if os.Getpid() == 1 {
			// full init
			boot.SetupConsole()
			boot.RemountRootRW()
			boot.ApplyHostname()

			if isRoot {
				if err := systemManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", 1,
						"failed to start enabled system units: %v", err)
				}
			}
			if !isRoot {
				if err := userManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", 1,
						"failed to start enabled user units: %v", err)
				}
			}

			boot.SpawnVirtualTerminals()

			for {
				select {
				case sig := <-signals:
					switch sig {
					case syscall.SIGTERM:
						logging.KernelPrintf(os.Stderr, "initd", 1,
							"SIGTERM ignored by init")
					case syscall.SIGCHLD:
						// reaper handles
					}
				}
			}
		} else {
			// init-lite
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"WARNING: --init requested but PID != 1, running init-lite mode")

			if isRoot {
				if err := systemManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
						"failed to start enabled system units: %v", err)
				}
			} else {
				if err := userManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
						"failed to start enabled user units: %v", err)
				}
			}

			for {
				select {
				case sig := <-signals:
					switch sig {
					case syscall.SIGTERM:
						shutdownDaemon(socketPath, userSocket, userLock, systemLock, systemManager, userManager, cfg.pidFile)
					}
				}
			}
		}
	}
	// socket-only mode
	sig := <-signals
	if sig == syscall.SIGTERM {
		shutdownDaemon(socketPath, userSocket, userLock, systemLock, systemManager, userManager, cfg.pidFile)
	}

}

// shutdownDaemon performs a clean shutdown on SIGTERM: it stops managed units,
// removes the IPC sockets, pid file and releases the locks, then exits. This
// lets a replacement daemon (e.g. from install.sh's restart) take over without
// a stale socket or lock leaving a split-brain supervisor behind.
func shutdownDaemon(socketPath, userSocket string, userLock, systemLock *os.File, systemManager, userManager *supervisor.Manager, pidFile string) {
	logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "received SIGTERM, shutting down")
	userManager.StopAllUnits()
	systemManager.StopAllUnits()
	systemManager.CloseJournal()
	userManager.CloseJournal()
	_ = os.Remove(userSocket)
	if socketPath != userSocket {
		_ = os.Remove(socketPath)
	}
	removeOwnPidFile(pidFile)
	if userLock != nil {
		_ = userLock.Close()
	}
	if systemLock != nil {
		_ = systemLock.Close()
	}
	os.Exit(0)
}

// removeOwnPidFile deletes the pid file only when it still points at this
// process, so shutdown never removes a successor's file after a restart race.
func removeOwnPidFile(path string) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// startDBusServers registers org.freedesktop.systemd1 on the D-Bus session bus
// (and the system bus when reachable) so that systemctl/system-scope probes and
// D-Bus clients talk to initd instead of failing. It runs in goroutines and is
// non-fatal: if a bus isn't available (no dbus-daemon, non-root for system bus),
// it logs and moves on. The user/session bus is the important one for the VPS
// case, since initd's own session bus already exists at $XDG_RUNTIME_DIR/bus.
func startDBusServers(systemManager, userManager *supervisor.Manager) {
	ctx := context.Background()
	// User (session) bus — initd already owns org.freedesktop.DBus here, so we
	// also own org.freedesktop.systemd1 and answer systemctl --user introspection.
	// Retry briefly: the session bus may still be starting (install.sh or the
	// autostart hook just forked dbus-daemon).
	for i := 0; i < 5; i++ {
		if _, err := dbus.ServeUserBus(ctx, userManager); err == nil {
			break
		} else if i == 4 {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"dbus user bus registration disabled: %v", err)
		} else {
			time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		}
	}
	// System bus — allows /usr/bin/systemctl (system scope) to connect and get a
	// verifiable answer. Non-fatal: fails for non-root or when no system
	// dbus-daemon is running with a permissive systemd1 policy.
	if _, err := dbus.ServeSystemBus(ctx, systemManager, userManager); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"dbus system bus registration unavailable: %v", err)
	}
}

// daemonConfig carries the process-level options: where to listen, which
// mode to run, and (for --daemonize) where to record the detached PID.
type daemonConfig struct {
	socketPath string
	initMode   bool
	daemonize  bool
	pidFile    string
	logFile    string
}

// runtimeDir returns a writable per-uid directory for pid/log files,
// mirroring the XDG_RUNTIME_DIR convention install.sh establishes.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	uid := os.Getuid()
	if uid == 0 {
		return "/run"
	}
	return fmt.Sprintf("/run/user/%d", uid)
}

func defaultPidFile() string { return filepath.Join(runtimeDir(), "initd.pid") }
func defaultLogFile() string { return filepath.Join(runtimeDir(), "initd-daemon.log") }

// spawnDetached re-executes this binary without --daemonize in a new session
// (setsid) with stdio wired to the log file, then waits for the child to
// record its pid file. The parent exits 0 once the child is up; startup
// failures surface through a missing pid file instead of a lost terminal.
func spawnDetached(cfg daemonConfig) error {
	logPath := cfg.logFile
	if logPath == "" {
		logPath = defaultLogFile()
	}
	if dir := filepath.Dir(logPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer logF.Close()

	null, err := os.OpenFile("/dev/null", os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer null.Close()

	childArgs := []string{}
	for _, a := range os.Args[1:] {
		if a == "--daemonize" {
			continue
		}
		childArgs = append(childArgs, a)
	}
	cmd := exec.Command(os.Args[0], childArgs...)
	cmd.Env = append(os.Environ(), "INITD_DAEMONIZED=1")
	cmd.Dir = "/"
	cmd.Stdin = null
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached child: %w", err)
	}
	// Detached: do not wait (that would reattach fate to the child).
	// Confirm it stays alive and records its pid file.
	pidPath := cfg.pidFile
	if pidPath == "" {
		pidPath = defaultPidFile()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			return nil
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("child exited before writing %s", pidPath)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", pidPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writePidFile records the daemon PID so hooks and boot scripts can wait on
// a file instead of pgrep. Stale files from a dead owner are replaced.
func writePidFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if raw, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
			if proc, err := os.FindProcess(pid); err == nil {
				if err := proc.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("pid file %s already owned by live PID %d", path, pid)
				}
			}
		}
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

func parseArgs(args []string) (daemonConfig, error) {
	cfg := daemonConfig{socketPath: "/run/initd.sock", initMode: true}
	socketProvided := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "-help":
			printHelp()
			os.Exit(0)
		case arg == "-V" || arg == "--version":
			printVersion()
			os.Exit(0)
		case arg == "--init":
			cfg.initMode = true
		case arg == "--daemonize":
			cfg.daemonize = true
		case arg == "--pid-file":
			i++
			if i >= len(args) {
				return cfg, fmt.Errorf("--pid-file requires a path")
			}
			cfg.pidFile = args[i]
		case strings.HasPrefix(arg, "--pid-file="):
			cfg.pidFile = strings.TrimPrefix(arg, "--pid-file=")
		case arg == "--log-file":
			i++
			if i >= len(args) {
				return cfg, fmt.Errorf("--log-file requires a path")
			}
			cfg.logFile = args[i]
		case strings.HasPrefix(arg, "--log-file="):
			cfg.logFile = strings.TrimPrefix(arg, "--log-file=")
		case arg == "--socket":
			socketProvided = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				cfg.socketPath = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--socket="):
			socketProvided = true
			value := strings.TrimPrefix(arg, "--socket=")
			if value != "" {
				cfg.socketPath = value
			}
		case arg == "":
			continue
		default:
			return cfg, fmt.Errorf("unknown argument: %s", arg)
		}
	}

	if socketProvided {
		cfg.initMode = false
	}
	if cfg.daemonize && cfg.pidFile == "" {
		cfg.pidFile = defaultPidFile()
	}

	return cfg, nil
}

func printHelp() {
	fmt.Printf(`Usage: initd [OPTIONS...]

Default behavior:
  Running initd with NO arguments defaults to init/supervisor mode (equivalent to --init).

Options:
  --init               Run as init/supervisor (autostart enabled units).
  --socket[=PATH]      Run as a pure daemon/service manager without init/PID1 behaviors.
                       If PATH omitted, defaults to /run/initd.sock.
  --daemonize          Detach into a new session (setsid), wire stdio to the
                       log file and record a pid file, then exit 0. Combine
                       with --init for boot/login hooks.
  --pid-file[=PATH]    Write the daemon PID here (default $XDG_RUNTIME_DIR/initd.pid).
                       Implied by --daemonize.
  --log-file[=PATH]    Append daemon output here when detaching
                       (default $XDG_RUNTIME_DIR/initd-daemon.log).
  -h, --help           Show this help.
  -V, --version        Show version.

Report bugs to: https://github.com/prabhatkrmishra/initd.git
`)
}

func printVersion() {
	fmt.Printf(
		"initd (initd) %s by prabhatkrmishra (https://github.com/prabhatkrmishra/initd.git) MIT License\n",
		initdVersion,
	)
}
