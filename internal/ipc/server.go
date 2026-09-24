package ipc

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"initd/internal/boot"
	"initd/internal/logging"
	"initd/internal/service"
	"initd/internal/supervisor"
	"initd/internal/userpaths"
)

type Request struct {
	Action string `json:"action"`
	Unit   string `json:"unit,omitempty"`
	Signal string `json:"signal,omitempty"`
	Now    bool   `json:"now,omitempty"`
	Lines  int    `json:"lines,omitempty"`
	LinesPlus bool `json:"lines_plus,omitempty"`
	Units  []string `json:"units,omitempty"`
	Boot   string   `json:"boot,omitempty"`
	Since  int64    `json:"since,omitempty"`
	Until  int64    `json:"until,omitempty"`
	Priority int  `json:"priority,omitempty"`
	PrioritySet bool `json:"priority_set,omitempty"`
	Grep   string   `json:"grep,omitempty"`
	CaseSensitive bool `json:"case_sensitive,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Invocation string `json:"invocation,omitempty"`
	ExcludeIdentifier string `json:"exclude_identifier,omitempty"`
	LatestInvocation bool `json:"latest_invocation,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
	CursorAfter bool `json:"cursor_after,omitempty"`
	Reverse bool    `json:"reverse,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`
	MaxFiles int   `json:"max_files,omitempty"`
	MaxDays  int   `json:"max_days,omitempty"`
}

type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type StatusData struct {
	Name                string        `json:"name"`
	Description         string        `json:"description"`
	State               service.State `json:"state"`
	SubState            string        `json:"sub_state,omitempty"`
	MainPID             int           `json:"main_pid"`
	StartedAt           time.Time     `json:"started_at"`
	FinishedAt          time.Time     `json:"finished_at"`
	StartedAtMonotonic  time.Duration `json:"started_at_monotonic"`
	FinishedAtMonotonic time.Duration `json:"finished_at_monotonic"`
	LastError           string        `json:"last_error"`
	Warnings            []string      `json:"warnings,omitempty"`
	Logs                []string      `json:"logs"`
}

type UnitData struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	State       service.State `json:"state"`
	Type        string        `json:"type"`
}

type UnitFileData struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Path  string `json:"path"`
}

const (
	// maxRequestBytes caps a single IPC request (real requests are a few
	// hundred bytes of JSON); larger payloads fail decoding instead of
	// ballooning decoder buffers.
	maxRequestBytes = 1 << 20
	// maxIPCConns bounds concurrent control connections (goroutine/FD
	// pressure from a hostile or wedged local client).
	maxIPCConns = 128
	// acceptErrorDelay keeps EMFILE-style accept failures from spinning.
	acceptErrorDelay = 50 * time.Millisecond
	// readTimeout bounds the request read; the response deadline is
	// generous separately (stops can legitimately take minutes).
	readTimeout  = 30 * time.Second
	writeTimeout = 10 * time.Minute
)

// ipcConnSem bounds in-flight control connections across every listener a
// daemon serves (filesystem + abstract fallback share it).
var ipcConnSem = make(chan struct{}, maxIPCConns)

// serveConn admits one accepted connection with bounded concurrency. When
// saturated the connection is closed immediately without a reply or a new
// goroutine: answering inline would stall the accept loop behind a peer
// that never reads, and a detached replier would reintroduce the pressure
// being bounded. Excess peers see EOF and retry.
func serveConn(conn net.Conn, manager *supervisor.Manager) {
	select {
	case ipcConnSem <- struct{}{}:
		go func() {
			defer func() { <-ipcConnSem }()
			handleConn(conn, manager)
		}()
	default:
		_ = conn.Close()
	}
}

func Serve(socketPath string, manager *supervisor.Manager) error {
	if strings.HasPrefix(socketPath, "@") {
		addr := &net.UnixAddr{Name: "\x00" + strings.TrimPrefix(socketPath, "@"), Net: "unix"}
		listener, err := net.ListenUnix("unix", addr)
		if err != nil {
			return err
		}
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				time.Sleep(acceptErrorDelay)
				continue
			}
			serveConn(conn, manager)
		}
	}

	dir := filepath.Dir(socketPath)
	if dir != "" && dir != "." {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			perm := os.FileMode(0755)
			if manager != nil && manager.UserMode {
				perm = 0700
			}
			_ = os.MkdirAll(dir, perm)
		}
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		if len(socketPath) > 90 && !strings.HasPrefix(socketPath, "@") {
			abstract := abstractFallback(manager)
			addr := &net.UnixAddr{Name: "\x00" + strings.TrimPrefix(abstract, "@"), Net: "unix"}
			abstractListener, aerr := net.ListenUnix("unix", addr)
			if aerr == nil {
				defer abstractListener.Close()
				for {
					conn, err := abstractListener.Accept()
					if err != nil {
						time.Sleep(acceptErrorDelay)
						continue
					}
					serveConn(conn, manager)
				}
			}
		}
		return err
	}
	defer listener.Close()
	// The control socket is privileged regardless of user/system mode; keep
	// it owner-only so other users can't connect and issue commands.
	_ = os.Chmod(socketPath, 0600)

	for {
		conn, err := listener.Accept()
		if err != nil {
			time.Sleep(acceptErrorDelay)
			continue
		}
		serveConn(conn, manager)
	}
}

func abstractFallback(manager *supervisor.Manager) string {
	uid := os.Getuid()
	if manager != nil && manager.UserMode {
		// Keep the abstract fallback consistent with the filesystem user
		// socket so a daemon started via sudo and its client agree.
		uid = userpaths.RealUID()
		return fmt.Sprintf("@initd-user-%d.sock", uid)
	}
	return fmt.Sprintf("@initd-system-%d.sock", uid)
}

// peerUID returns the caller's UID via SO_PEERCRED. False when the
// connection is not a Unix socket or credentials are unavailable
// (tests using net.Pipe, non-Linux builds): callers treat that as
// "no evidence" and fall back to filesystem permissions.
func peerUID(conn net.Conn) (uint32, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	ok = false
	if err := raw.Control(func(fd uintptr) {
		cred, cerr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if cerr != nil {
			return
		}
		uid = cred.Uid
		ok = true
	}); err != nil || !ok {
		return 0, false
	}
	return uid, true
}

// allowedPeer mirrors the filesystem socket's 0600 for transports without
// permission bits (Linux abstract namespace, used when the path exceeds
// ~90 bytes). The daemon's own UID may always connect; root may always
// connect (admin + sudo flows); anyone else is rejected before dispatch
// regardless of action, including read-only ones, matching the 0600
// posture of the filesystem socket.
func allowedPeer(manager *supervisor.Manager, uid uint32) bool {
	if uid == 0 {
		return true
	}
	return int(uid) == os.Getuid()
}

func handleConn(conn net.Conn, manager *supervisor.Manager) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	decoder := json.NewDecoder(io.LimitReader(conn, maxRequestBytes))
	encoder := json.NewEncoder(conn)
	// Enforce only when we have kernel credentials. Filesystem sockets
	// remain protected by 0600 even if this check is skipped (tests).
	if uid, ok := peerUID(conn); ok && !allowedPeer(manager, uid) {
		_ = encoder.Encode(Response{Success: false, Message: "access denied: caller UID not authorized for this manager"})
		return
	}

	var req Request
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(Response{Success: false, Message: err.Error()})
		return
	}

	// The request arrived whole; the response may legitimately take
	// minutes (stop/restart waits out TimeoutStopSec), so clear the read
	// deadline and allow a generous write deadline instead.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	response := dispatch(req, manager)
	_ = encoder.Encode(response)
}

func dispatch(req Request, manager *supervisor.Manager) Response {
	switch req.Action {
	case "start":
		if err := manager.StartUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "reload":
		if err := manager.ReloadUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "stop":
		if err := manager.StopUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "restart":
		if err := manager.RestartUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "status":
		if unit, err := manager.FindUnit(req.Unit); err == nil {
			snapshot := unit.Snapshot()
			effState, effPID := unit.EffectiveState()
			lastErr := snapshot.LastError
			if effState == service.StateActive && snapshot.State != service.StateActive && lastErr == "" {
				lastErr = "external-process"
			}
			logs := unit.Logs.Entries()
			logLines := make([]string, 0, len(logs))
			for _, entry := range logs {
				logLines = append(logLines, logging.FormatEntry(entry))
			}
			return Response{Success: true, Data: StatusData{
				Name:                unit.GetConfig().Name,
				Description:         unit.Description(),
				State:               effState,
				SubState:            string(unit.SubState()),
				MainPID:             effPID,
				StartedAt:           snapshot.StartedAt,
				FinishedAt:          snapshot.FinishedAt,
				StartedAtMonotonic:  snapshot.StartedAtMonotonic,
				FinishedAtMonotonic: snapshot.FinishedAtMonotonic,
				LastError:           lastErr,
				Warnings:            unit.IgnoredSecurityNotes(),
				Logs:                logLines,
			}}
		}
		if _, err := manager.FindSocketUnit(req.Unit); err == nil {
			state, _ := manager.SocketActiveState(req.Unit)
			return Response{Success: true, Data: StatusData{
				Name:        req.Unit,
				Description: req.Unit,
				State:       service.State(state),
			}}
		}
		return Response{Success: false, Message: fmt.Sprintf("unit %s not found", req.Unit)}
	case "is-active":
		if unit, err := manager.FindUnit(req.Unit); err == nil {
			state, _ := unit.EffectiveState()
			return Response{Success: true, Data: state}
		}
		if _, err := manager.FindSocketUnit(req.Unit); err == nil {
			state, _ := manager.SocketActiveState(req.Unit)
			return Response{Success: true, Data: state}
		}
		return Response{Success: false, Message: fmt.Sprintf("unit %s not found", req.Unit)}
	case "list-units":
		units := manager.ListUnits()
		data := make([]UnitData, 0, len(units)+len(manager.SocketUnitNames()))
		for _, unit := range units {
			effState, _ := unit.EffectiveState()
			// ListUnits holds *service.Unit; Config.Type is the unit type
			// (simple/forking/...) — fall back to service type string.
			utype := ""
			if unit.GetConfig() != nil {
				utype = unit.GetConfig().Type
			}
			data = append(data, UnitData{Name: unit.GetConfig().Name, Description: unit.Description(), State: effState, Type: utype})
		}
		for _, name := range manager.SocketUnitNames() {
			state, _ := manager.SocketActiveState(name)
			cfg, _ := manager.FindSocketUnit(name)
			desc := name
			if cfg != nil {
				desc = cfg.Description
				if desc == "" {
					desc = name
				}
			}
			data = append(data, UnitData{Name: name, Description: desc, State: service.State(state), Type: "socket"})
		}
		return Response{Success: true, Data: data}
	case "list-unit-files":
		units, err := manager.ListUnitFiles()
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		data := make([]UnitFileData, 0, len(units))
		for _, unit := range units {
			state := manager.UnitFileState(unit.GetConfig().Name)
			data = append(data, UnitFileData{Name: unit.GetConfig().Name, State: state, Path: unit.Path})
		}
		return Response{Success: true, Data: data}
	case "enable":
		if req.Now {
			if err := manager.EnableUnitWithNow(req.Unit, true); err != nil {
				return Response{Success: false, Message: err.Error()}
			}
		} else {
			if err := manager.EnableUnit(req.Unit); err != nil {
				return Response{Success: false, Message: err.Error()}
			}
		}
		return Response{Success: true}
	case "disable":
		if req.Now {
			if err := manager.DisableUnitWithNow(req.Unit, true); err != nil {
				return Response{Success: false, Message: err.Error()}
			}
		} else {
			if err := manager.DisableUnit(req.Unit); err != nil {
				return Response{Success: false, Message: err.Error()}
			}
		}
		return Response{Success: true}
	case "is-enabled":
		if manager.IsMasked(req.Unit) {
			return Response{Success: true, Data: "masked"}
		}
		enabled, err := manager.IsEnabled(req.Unit)
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		if enabled {
			return Response{Success: true, Data: "enabled"}
		}
		return Response{Success: true, Data: "disabled"}
	case "is-failed":
		state, err := manager.UnitState(req.Unit)
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true, Data: state}
	case "reset-failed":
		if err := manager.ResetFailed(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "show":
		if data, err := manager.ShowUnit(req.Unit); err == nil {
			return Response{Success: true, Data: data}
		}
		if data, err := manager.ShowSocketUnit(req.Unit); err == nil {
			return Response{Success: true, Data: data}
		}
		return Response{Success: false, Message: fmt.Sprintf("unit %s not found", req.Unit)}
	case "cat":
		if content, err := manager.CatUnit(req.Unit); err == nil {
			return Response{Success: true, Data: content}
		}
		if content, err := manager.CatSocketUnit(req.Unit); err == nil {
			return Response{Success: true, Data: content}
		}
		return Response{Success: false, Message: fmt.Sprintf("unit %s not found", req.Unit)}
	case "mask":
		if err := manager.MaskUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "unmask":
		if err := manager.UnmaskUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "kill":
		if err := manager.KillUnit(req.Unit, req.Signal); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "is-system-running":
		return Response{Success: true, Data: manager.SystemState()}
	case "daemon-reload":
		if err := manager.Reload(); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "need-daemon-reload":
		if manager == nil {
			return Response{Success: false, Message: "no manager"}
		}
		return Response{Success: true, Data: manager.NeedDaemonReload()}
	case "journal":
		filter := logging.JournalFilter{
			Units:             req.Units,
			BootID:           req.Boot,
			SinceUsec:        req.Since,
			UntilUsec:        req.Until,
			PriorityMax:      req.Priority,
			PrioritySet:      req.PrioritySet,
			Grep:             req.Grep,
			CaseSensitive:    req.CaseSensitive,
			Identifier:       req.Identifier,
			Invocation:       req.Invocation,
			ExcludeIdentifier: req.ExcludeIdentifier,
			LatestInvocation:  req.LatestInvocation,
			Cursor:           req.Cursor,
			CursorAfter:      req.CursorAfter,
			Lines:             req.Lines,
			LinesPlus:         req.LinesPlus,
			Reverse:          req.Reverse,
		}
		// Bounded head-after-cursor queries stream file-by-file and stop
		// at the limit, so listing a huge journal never loads it whole.
		// Every other shape (tails, reverse, invocation grouping) still
		// needs the full set and uses the materializing path.
		if req.Lines > 0 && req.LinesPlus && !req.Reverse && !req.LatestInvocation {
			out, err := logging.QueryJournalStream(manager.JournalFiles(), filter)
			if err != nil {
				return Response{Success: false, Message: err.Error()}
			}
			return Response{Success: true, Data: out}
		}
		// Tail-N queries stop after banking N matches from the newest
		// files, so `-n` is finally a safe workaround on huge journals.
		if req.Lines > 0 && !req.LinesPlus && !req.Reverse && !req.LatestInvocation && req.Cursor == "" {
			out, err := logging.QueryJournalTail(manager.JournalFiles(), filter)
			if err != nil {
				return Response{Success: false, Message: err.Error()}
			}
			return Response{Success: true, Data: out}
		}
		entries, err := logging.ReadAll(manager.JournalFiles())
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		out := logging.QueryJournal(entries, filter)
		return Response{Success: true, Data: out}
	case "journal-boots":
		entries, err := logging.ReadAll(manager.JournalFiles())
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true, Data: logging.BootList(entries)}
	case "journal-usage":
		usage := manager.JournalUsage()
		return Response{Success: true, Data: map[string]any{
			"bytes": usage["bytes"],
			"files": usage["files"],
			"dir":   manager.ActiveJournalDir(),
		}}
	case "journal-vacuum":
		if err := manager.VacuumJournal(req.MaxBytes, req.MaxFiles, req.MaxDays); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "journal-sync":
		if err := manager.SyncJournal(); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "journal-rotate":
		if err := manager.RotateJournal(); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "logs":
		if req.Unit == "" {
			return Response{Success: false, Message: "no unit name specified"}
		}
		unit, err := manager.FindUnit(req.Unit)
		if err == nil {
			logs := unit.Logs.Entries()
			lines := make([]string, 0, len(logs))
			for _, entry := range logs {
				lines = append(lines, logging.FormatEntry(entry))
			}
			if req.Lines > 0 && len(lines) > req.Lines {
				lines = lines[len(lines)-req.Lines:]
			}
			return Response{Success: true, Data: lines}
		}
		// Socket units keep no log ring; report empty rather than not-found
		// so `journalctl -u foo.socket` degrades to no output.
		if _, serr := manager.FindSocketUnit(req.Unit); serr == nil {
			return Response{Success: true, Data: []string{}}
		}
		return Response{Success: false, Message: fmt.Sprintf("unit %s not found", req.Unit)}
	case "reboot", "poweroff", "halt":
		if manager != nil && manager.UserMode {
			return Response{Success: false, Message: "reboot/poweroff/halt not allowed for user manager"}
		}
		go func() {
			boot.Shutdown(manager, req.Action)
		}()
		return Response{Success: true}
	default:
		return Response{Success: false, Message: "unknown action"}
	}
}
