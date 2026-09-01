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
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const initdVersion = "1.0.1"

func main() {
	socketPath, initMode, err := parseArgs(os.Args[1:])
	if err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "%v", err)
		os.Exit(1)
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

	go serveManager(socketPath, systemManager)
	userSocket := userpaths.UserSocketPath()
	if userSocket != socketPath {
		startUserManager(userSocket, userManager)
	}
	// Hold the lock for the lifetime of the daemon.
	if userLock != nil {
		defer func() {
			_ = userLock.Close()
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
		startSystemUnits := os.Getuid() == 0

		if os.Getpid() == 1 {
			// full init
			boot.SetupConsole()
			boot.RemountRootRW()
			boot.ApplyHostname()

			if startSystemUnits {
				if err := systemManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", 1,
						"failed to start enabled system units: %v", err)
				}
			}
			if err := userManager.StartEnabledUnits(); err != nil {
				logging.KernelPrintf(os.Stderr, "initd", 1,
					"failed to start enabled user units: %v", err)
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

			if startSystemUnits {
				if err := systemManager.StartEnabledUnits(); err != nil {
					logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
						"failed to start enabled system units: %v", err)
				}
			}
			if err := userManager.StartEnabledUnits(); err != nil {
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"failed to start enabled user units: %v", err)
			}

			for {
				<-signals
			}
		}
	}
	// socket-only mode
	<-signals

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
	if _, err := dbus.ServeUserBus(ctx, userManager); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"dbus user bus registration disabled: %v", err)
	}
	// System bus — allows /usr/bin/systemctl (system scope) to connect and get a
	// verifiable answer. Non-fatal: fails for non-root or when no system
	// dbus-daemon is running with a permissive systemd1 policy.
	if _, err := dbus.ServeSystemBus(ctx, systemManager, userManager); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"dbus system bus registration unavailable: %v", err)
	}
}

func parseArgs(args []string) (string, bool, error) {
	socketPath := "/run/initd.sock"
	initMode := true
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
			initMode = true
		case arg == "--socket":
			socketProvided = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				socketPath = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--socket="):
			socketProvided = true
			value := strings.TrimPrefix(arg, "--socket=")
			if value != "" {
				socketPath = value
			}
		case arg == "":
			continue
		default:
			return "", false, fmt.Errorf("unknown argument: %s", arg)
		}
	}

	if socketProvided {
		initMode = false
	}

	return socketPath, initMode, nil
}

func printHelp() {
	fmt.Printf(`Usage: initd [OPTIONS...]

Default behavior:
  Running initd with NO arguments defaults to init/supervisor mode (equivalent to --init).

Options:
  --init               Run as init/supervisor (autostart enabled units).
  --socket[=PATH]      Run as a pure daemon/service manager without init/PID1 behaviors.
                       If PATH omitted, defaults to /run/initd.sock.
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
