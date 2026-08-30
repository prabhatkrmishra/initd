package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"initd/internal/boot"
	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/supervisor"
	"initd/internal/userpaths"
)

const initdVersion = "0.0.2"

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

	go serveManager(socketPath, systemManager)
	userSocket := userpaths.UserSocketPath()
	if userSocket != socketPath {
		go serveManager(userSocket, userManager)
	}

if initMode {
    if os.Getpid() == 1 {
        // full init
        boot.SetupConsole()
        boot.RemountRootRW()
        boot.ApplyHostname()

        if err := systemManager.StartEnabledUnits(); err != nil {
            logging.KernelPrintf(os.Stderr, "initd", 1,
                "failed to start enabled system units: %v", err)
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

        if err := systemManager.StartEnabledUnits(); err != nil {
            logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
                "failed to start enabled system units: %v", err)
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

/* ---------------- arg parsing ---------------- */

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
