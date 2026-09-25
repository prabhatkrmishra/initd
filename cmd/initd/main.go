package main

import (
	"context"
	"errors"
	"fmt"
	gdbus "github.com/godbus/dbus/v5"
	"initd/internal/boot"
	"initd/internal/build"
	"initd/internal/cgroup"
	"initd/internal/dbus"
	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/supervisor"
	"initd/internal/userpaths"

	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var initdVersion = build.String()

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

	// Under memory pressure the OOM killer picks the fattest victim, which
	// is often the supervisor itself on a box whose units log hard. Make
	// the daemon the last resort: best-effort oom_score_adj of -500 so the
	// kernel reaps a runaway child before its supervisor. Read-only /proc
	// (containers) just skips this; supervision never depends on it.
	protectFromOOM()

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
	// The pid file is written once this process has won the singleton race
	// (see below): writing it here let a losing instance repoint the file at
	// a process that supervises nothing.

	signals := make(chan os.Signal, 16)

	if initMode {
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGCHLD, syscall.SIGPWR)
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

	// One line for the box, not one per unit: whether the kernel can tell this
	// daemon's processes from a stranger's is decided here, once.
	if tree := cgroup.Default(); !tree.Available() {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"unit cgroups unavailable (%s); stops fall back to process groups", tree.Reason())
	}

	// Fallback for non-root: /run/initd.sock not writable
	if socketPath == "/run/initd.sock" && os.Getuid() != 0 {
		if _, err := os.Stat("/run"); err == nil {
			f, err := os.CreateTemp("/run", ".initd-probe-*")
			if err != nil {
				fallback := userpaths.SystemSocketPath()
				if fallback == "/run/initd.sock" {
					fallback = "/tmp/initd.sock"
				}
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"no permission for %s, falling back to %s", socketPath, fallback)
				socketPath = fallback
			} else {
				name := f.Name()
				_ = f.Close()
				_ = os.Remove(name)
			}
		}
	}

	// stopServe is closed by shutdownDaemon. Serve loops check it before
	// rebinding: without this a shutdown that removes the socket file
	// would be undone a second later by the retry loop resurrecting it.
	stopServe := make(chan struct{})
	serveManager := func(path string, mgr *supervisor.Manager) {
		backoff := time.Second
		for {
			if err := ipc.Serve(path, mgr); err != nil {
				select {
				case <-stopServe:
					return
				default:
				}
				// Losing the bound path is not a failure to back off from: the
				// address is still ours and the lock is still held, so Serve
				// rebinds it immediately. Backing off would leave the control
				// plane dark for up to 30s after a stray unlink.
				if ipc.IsSocketPathLost(err) {
					logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
						"control socket %s lost its path; rebinding", path)
					continue
				}
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"ipc server error on %s: %v (retrying in %s)", path, err, backoff)
				select {
				case <-stopServe:
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			select {
			case <-stopServe:
				return
			default:
			}
		}
	}

	// Singleton for the user manager: same user logging in many times must
	// not create duplicate daemons/sockets.
	//
	// The flock is the only authority. LOCK_EX|LOCK_NB is released by the
	// kernel the instant its holder dies, so a refused acquisition always
	// means a *live* daemon owns this uid - and that instance has to stand
	// down rather than supervise a second copy of the same units. Probing
	// the socket instead (and deleting it when the probe failed) used to
	// unlink a listening peer's path during a restart, leaving the uid with
	// no reachable supervisor while its units kept running under a daemon
	// nobody could talk to.
	var userLock *os.File
	startUserManager := func(path string, mgr *supervisor.Manager) bool {
		lock, err := userpaths.AcquireUserLock()
		if err != nil {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"user daemon already owns %s (%v) - not starting a second supervisor", path, err)
			return false
		}
		userLock = lock
		go serveManager(path, mgr)
		return true
	}

	// Singleton for the system manager too — two concurrent `initd --init`
	// invocations (e.g. two logins racing) must not both bind /run/initd.sock.
	var systemLock *os.File
	startSystemManager := func(path string, mgr *supervisor.Manager) bool {
		lock, err := userpaths.AcquireSystemLock()
		if err != nil {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"system daemon already owns %s (%v) - not starting a second supervisor", path, err)
			return false
		}
		systemLock = lock
		go serveManager(path, mgr)
		return true
	}

	// Which manager owns the requested socket is decided by the socket's
	// identity: binding systemManager at the user socket answered user-scope
	// requests from the system unit dirs while the D-Bus path served user
	// units - one daemon giving two different answers per transport.
	primaryIsUser := primaryIsUserSocket(socketPath)
	primaryServes := false
	if primaryIsUser {
		primaryServes = startUserManager(socketPath, userManager)
	} else {
		primaryServes = startSystemManager(socketPath, systemManager)
	}
	userSocket := userpaths.UserSocketPath()
	if !primaryIsUser && userSocket != socketPath {
		// A system daemon also serves its own uid's user socket; losing that
		// race is not fatal - that uid's own daemon keeps running.
		startUserManager(userSocket, userManager)
	}
	if !primaryServes && os.Getpid() != 1 {
		// Another daemon owns the transport this instance was asked to
		// serve. Staying alive would mean a second supervisor plus an idle
		// process that outlives its socket, so hand the uid back to the
		// daemon that already has it. PID 1 must never exit here.
		if userLock != nil {
			_ = userLock.Close()
		}
		if systemLock != nil {
			_ = systemLock.Close()
		}
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"leaving %s to the daemon that already owns it", socketPath)
		// exitHandedOff is the private signal `--daemonize` uses to tell its
		// launcher that the supervisor the caller asked for is already up;
		// an interactive invocation just exits 0 like any idempotent start.
		if os.Getenv("INITD_DAEMONIZED") == "1" {
			os.Exit(exitHandedOff)
		}
		os.Exit(0)
	}
	// The pid file names the daemon a restart script signals, so it is only
	// written once this process has won the singleton race: an instance that
	// is about to exit must not overwrite the live supervisor's pid file.
	if cfg.pidFile != "" {
		if err := writePidFile(cfg.pidFile); err != nil {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "pid file %s: %v", cfg.pidFile, err)
			if os.Getpid() != 1 {
				os.Exit(1)
			}
		}
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
	dbusCtx, stopDBus := context.WithCancel(context.Background())
	go startDBusServers(dbusCtx, systemManager, userManager)

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
					case syscall.SIGPWR:
						logging.KernelPrintf(os.Stderr, "initd", 1,
							"power failure, shutting down")
						boot.Shutdown(systemManager, "poweroff")
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
					case syscall.SIGTERM, syscall.SIGINT:
						shutdownDaemon(sigName(sig), stopServe, stopDBus, socketPath, userSocket, userLock, systemLock, systemManager, userManager, cfg.pidFile)
					}
				}
			}
		}
	}
	// socket-only mode
	sig := <-signals
	if sig == syscall.SIGTERM || sig == syscall.SIGINT {
		shutdownDaemon(sigName(sig), stopServe, stopDBus, socketPath, userSocket, userLock, systemLock, systemManager, userManager, cfg.pidFile)
	}

}

// primaryIsUserSocket reports whether the daemon's requested socket is the one
// `systemctl --user` connects to. That socket must carry the user manager:
// binding the system manager there made user-scope requests answer from the
// system unit dirs while the D-Bus path answered from user units.
func primaryIsUserSocket(socketPath string) bool {
	return socketPath == userpaths.UserSocketPath()
}

// sigName renders a signal for shutdown logging.
func sigName(sig os.Signal) string {
	if sig == syscall.SIGINT {
		return "SIGINT"
	}
	return "SIGTERM"
}

// stopManagerBounded stops one manager's units but never longer than the
// bound: a replacement daemon (e.g. from install.sh's restart) must not
// hang behind a stuck unit. Per-unit timeouts still apply inside.
func stopManagerBounded(mgr *supervisor.Manager, bound time.Duration) {
	done := make(chan struct{})
	go func() {
		mgr.StopAllUnits()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bound):
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"unit shutdown timed out, continuing anyway")
	}
}

// shutdownDaemon performs a clean shutdown: it stops the serve loops first
// (so they can't resurrect the sockets below), stops managed units,
// removes the IPC sockets, pid file and releases the locks, then exits. This
// lets a replacement daemon (e.g. from install.sh's restart) take over without
// a stale socket or lock leaving a split-brain supervisor behind.
func shutdownDaemon(why string, stopServe chan struct{}, stopDBus context.CancelFunc, socketPath, userSocket string, userLock, systemLock *os.File, systemManager, userManager *supervisor.Manager, pidFile string) {
	logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "received %s, shutting down", why)
	close(stopServe)
	stopDBus()
	stopManagerBounded(userManager, 2*time.Minute)
	stopManagerBounded(systemManager, 2*time.Minute)
	systemManager.CloseJournal()
	userManager.CloseJournal()
	// Stop the listeners before the paths go, so nothing can accept a request
	// this daemon will never answer, and so the identity check below is taken
	// against a socket that is genuinely ours and not mid-rebind.
	ipc.StopServing(userSocket)
	if socketPath != userSocket {
		ipc.StopServing(socketPath)
	}
	ipc.RemoveIfOurs(userSocket)
	if socketPath != userSocket {
		ipc.RemoveIfOurs(socketPath)
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

// busLivenessPoll is how often a held bus connection is checked for having gone
// away. Short enough that a restarted bus is re-advertised promptly, long
// enough not to be a busy loop.
const busLivenessPoll = 5 * time.Second

// advertise keeps one D-Bus name registered for as long as the daemon lives.
//
// Registration is maintained state, not a one-shot event. The session bus is
// often started by something else - profile.d/initd.sh forks dbus-daemon, or a
// session starts without one - so a daemon that loses that race is invisible
// to every D-Bus client from then on. That is not a cosmetic gap: a client
// that finds the name unowned falls back to D-Bus service activation, and the
// activation file's Exec cannot take the name either, so the client blocks
// until service_start_timeout (120s) instead of getting an answer.
//
// So this retries until it succeeds or ctx is cancelled, holds the connection
// rather than discarding it, and re-acquires if the bus goes away underneath
// us. Backoff is capped and jittered so daemons started together do not
// synchronise. Only a change of state is logged, so a bus that never appears
// does not fill the log.
func advertise(ctx context.Context, label string, register func(context.Context) (*gdbus.Conn, error)) {
	const (
		minBackoff = 100 * time.Millisecond
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff
	held := false
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := register(ctx)
		if err == nil {
			if !held {
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"registered %s", label)
			}
			held = true
			// Hold the connection. A bus that goes away takes the name with
			// it, so re-acquire rather than pretending it is still ours.
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-time.After(busLivenessPoll):
				if !dbus.ConnAlive(conn) {
					_ = conn.Close()
					logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
						"lost %s; re-acquiring", label)
					held = false
					backoff = minBackoff
				}
			}
			continue
		}
		if held {
			logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
				"lost %s: %v", label, err)
			held = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// jitter spreads a retry by +/-10% so daemons started together do not all retry
// on the same tick.
func jitter(d time.Duration) time.Duration {
	spread := time.Duration(rand.Int63n(int64(d)/5+1)) - d/10
	if out := d + spread; out > time.Millisecond {
		return out
	}
	return time.Millisecond
}

// startDBusServers advertises org.freedesktop.systemd1 on the D-Bus session bus
// (and the system bus when reachable) so that systemctl/system-scope probes and
// third-party service managers talk to initd instead of failing. Neither bus is
// required: a box with no dbus-daemon, or a non-root daemon with no system bus,
// still supervises over its own socket. The user/session bus is the important
// one, since that is where a user's tools look for a user manager.
func startDBusServers(ctx context.Context, systemManager, userManager *supervisor.Manager) {
	// The user (session) bus: initd already owns org.freedesktop.DBus there, so
	// owning org.freedesktop.systemd1 too is what makes `systemctl --user` and
	// third-party tooling answer from initd.
	go advertise(ctx, "org.freedesktop.systemd1 on the user bus",
		func(ctx context.Context) (*gdbus.Conn, error) {
			return dbus.ServeUserBus(ctx, userManager)
		})
	// The system bus: lets a system-scope systemctl connect and get a
	// verifiable answer. Non-fatal where there is no system bus.
	go advertise(ctx, "org.freedesktop.systemd1 on the system bus",
		func(ctx context.Context) (*gdbus.Conn, error) {
			return dbus.ServeSystemBus(ctx, systemManager, userManager)
		})
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

// exitHandedOff is the status a --daemonize child exits with when another
// daemon already owns the socket it was asked to serve: the launcher's goal
// (a supervisor running for this uid) is already met.
const exitHandedOff = 3

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
		_ = os.MkdirAll(dir, dirModeFor(dir))
	}
	maybeRotateLog(logPath)
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
	// The child starts with Dir=/, so a bare argv[0] must be resolved before
	// the exec. /proc/self/exe is authoritative; resolving "initd" against the
	// login cwd instead can name an unrelated file (the source tree at
	// ~/initd) and fail --daemonize with EACCES.
	bin, err := os.Executable()
	if err != nil {
		bin = os.Args[0]
		if found, lookErr := exec.LookPath(bin); lookErr == nil {
			bin = found
		} else if abs, absErr := filepath.Abs(bin); absErr == nil {
			bin = abs
		}
	}
	cmd := exec.Command(bin, childArgs...)
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
		if childWrotePidFile(pidPath, cmd.Process.Pid) {
			return nil
		}
		if status, exited := pollChildExit(cmd.Process.Pid); exited {
			if status == exitHandedOff {
				// The socket belongs to a daemon that is already running, so
				// the state --daemonize was asked to produce holds.
				return nil
			}
			return fmt.Errorf("child exited with status %d before writing %s", status, pidPath)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", pidPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pollChildExit reaps the detached child when it has already exited and
// reports its status. kill(pid,0) still succeeds on the zombie, so checking
// for liveness alone kept the launcher polling for the whole deadline after an
// instance that handed the socket over.
func pollChildExit(pid int) (int, bool) {
	var ws syscall.WaitStatus
	done, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err != nil || done != pid {
		return 0, false
	}
	return ws.ExitStatus(), true
}

// dirModeFor keeps per-user runtime dirs private: /run/user/* and the
// XDG runtime dir are 0700 on a normal system, so creating them 0755 would
// leak pid files and sockets to other users.
func dirModeFor(path string) os.FileMode {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && (path == xdg || len(path) > len(xdg)+1 && path[:len(xdg)+1] == xdg+"/") {
		return 0o700
	}
	if len(path) > len("/run/user/") && path[:len("/run/user/")] == "/run/user/" {
		return 0o700
	}
	return 0o755
}

// writePidFile records the daemon PID so hooks and boot scripts can wait on
// a file instead of pgrep. Stale files from a dead owner are replaced.
func writePidFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirModeFor(dir)); err != nil {
			return err
		}
	}
	if raw, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
			if proc, err := os.FindProcess(pid); err == nil {
				if err := proc.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("pid file %s already owned by live PID %d", path, pid)
				}
				if errors.Is(err, syscall.EPERM) {
					// The process exists but belongs to another user;
					// overwriting its pid file would hijack it.
					return fmt.Errorf("pid file %s already owned by live PID %d", path, pid)
				}
			}
		}
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// maxDetachedLog is the point at which the detach log rotates: the daemon
// appends there forever, so without a cap one verbose unit fills the disk.
const maxDetachedLog = 64 << 20

// maybeRotateLog moves a full detach log aside (keeping one backup) so it
// stays bounded across months of appends.
func maybeRotateLog(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= maxDetachedLog {
		return
	}
	_ = os.Remove(path + ".prev")
	_ = os.Rename(path, path+".prev")
}

// childWrotePidFile reports whether pidPath already names our child. The
// detach parent waits on the pid file, and a stale file from a previous
// crash (naming some other pid) must not count as a successful start.
func childWrotePidFile(pidPath string, childPid int) bool {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return err == nil && pid == childPid
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

// protectFromOOM lowers /proc/self/oom_score_adj best-effort so the
// supervisor survives memory pressure longer than the units it watches.
// -500 mirrors a protected system daemon: adjustable by admins afterwards,
// still killable when truly nothing else can go. Failures (read-only
// /proc, containers, non-Linux) are silent by design.
func protectFromOOM() {
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.WriteString("-500"); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"oom_score_adj not writable: %v", err)
	}
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
