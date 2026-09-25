package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"initd/internal/boot"
	"initd/internal/logging"
	"initd/internal/service"
	"initd/internal/supervisor"
	"initd/internal/userpaths"
)

type Request struct {
	Action            string   `json:"action"`
	Unit              string   `json:"unit,omitempty"`
	Signal            string   `json:"signal,omitempty"`
	Now               bool     `json:"now,omitempty"`
	Lines             int      `json:"lines,omitempty"`
	LinesPlus         bool     `json:"lines_plus,omitempty"`
	Units             []string `json:"units,omitempty"`
	Boot              string   `json:"boot,omitempty"`
	Since             int64    `json:"since,omitempty"`
	Until             int64    `json:"until,omitempty"`
	Priority          int      `json:"priority,omitempty"`
	PrioritySet       bool     `json:"priority_set,omitempty"`
	Grep              string   `json:"grep,omitempty"`
	CaseSensitive     bool     `json:"case_sensitive,omitempty"`
	Identifier        string   `json:"identifier,omitempty"`
	Invocation        string   `json:"invocation,omitempty"`
	ExcludeIdentifier string   `json:"exclude_identifier,omitempty"`
	LatestInvocation  bool     `json:"latest_invocation,omitempty"`
	Cursor            string   `json:"cursor,omitempty"`
	CursorAfter       bool     `json:"cursor_after,omitempty"`
	Reverse           bool     `json:"reverse,omitempty"`
	MaxBytes          int64    `json:"max_bytes,omitempty"`
	MaxFiles          int      `json:"max_files,omitempty"`
	MaxDays           int      `json:"max_days,omitempty"`
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
	// FragmentPath is where the unit was loaded from, empty for a transient
	// unit. `systemctl status` prints it inside the Loaded: line.
	FragmentPath string `json:"fragment_path,omitempty"`
	// Result, MainCode and ExitCode are systemd's Result, ExecMainCode and
	// ExecMainStatus, which the failed/inactive status lines are built from.
	Result   string `json:"result,omitempty"`
	MainCode int    `json:"main_code,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	// ExecMainPID is the PID the main process had. MainPID is zero once it
	// was reaped; systemd keeps showing this one in the Process: line.
	ExecMainPID int `json:"exec_main_pid,omitempty"`
	// ExecStart is the unit's main command line, shown as the Process: line
	// once it has run.
	ExecStart string `json:"exec_start,omitempty"`
	// CGroup is systemd's ControlGroup, and CGroupPIDs the processes the kernel
	// puts in it. Both are empty where this box has no unit cgroups, so the
	// status output simply has no CGroup stanza there.
	CGroup     string `json:"cgroup,omitempty"`
	CGroupPIDs []int  `json:"cgroup_pids,omitempty"`
}

// firstExecStart is the unit's main command line, which systemd echoes
// verbatim in the Process: status line.
func firstExecStart(unit *service.Unit) string {
	config := unit.GetConfig()
	if config == nil {
		return ""
	}
	return config.Service.ExecStart
}

type UnitData struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	State       service.State `json:"state"`
	// SubState is systemd's second state axis (running/exited/dead/...),
	// which `list-units` prints as its own column and filters match on.
	SubState string `json:"sub_state,omitempty"`
	Type     string `json:"type"`
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

// socketHasListener reports whether a live supervisor is accepting
// connections at path. connect(2) on AF_UNIX succeeds against a listening
// socket without exchanging a byte, and fails immediately for a leftover
// file whose owner is gone.
func socketHasListener(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// errSocketPathLost reports that the path this listener bound no longer names
// its socket, so the listener is unreachable by name and has to be rebound.
// Distinct from a generic bind failure because it is the caller's cue to retry
// immediately rather than back off: nothing is wrong with the address, and the
// daemon still holds the lock for it.
var errSocketPathLost = errors.New("control socket path was removed")

// IsSocketPathLost reports whether err means "rebind the same address now".
func IsSocketPathLost(err error) bool { return errors.Is(err, errSocketPathLost) }

// socketIdentity is the (device, inode) pair the filesystem handed a bound
// socket. Comparing it is the only way to tell "the path is still mine" from
// "something rebound this address": a listener keeps accepting on an unlinked
// inode, so the socket looks healthy to its owner while every new client gets
// ENOENT from the path.
type socketIdentity struct {
	dev uint64
	ino uint64
}

// lstatIdentity reads a path's identity without following symlinks, so a
// symlink dropped at the socket path cannot be mistaken for the socket.
func lstatIdentity(path string) (socketIdentity, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return socketIdentity{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, false
	}
	return socketIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

// socketPathOwned reports whether path still resolves to want.
func socketPathOwned(path string, want socketIdentity) bool {
	got, ok := lstatIdentity(path)
	return ok && got == want
}

// boundSockets tracks the identity of every filesystem socket this process has
// bound, and listeners holds the matching net.Listener, so shutdown can tell
// its own paths from a successor's.
//
// The locks already make that mostly true - a successor cannot rebind while
// this process holds the flock - but "mostly" is doing real work in
// shutdownDaemon: the unlink is only safe because the lock is released after
// it, and nothing records that ordering as a rule. A daemon that is slow to
// stop, restarted twice, or serving an address a successor also wants, turns
// that into a race where the loser's shutdown removes the winner's socket and
// leaves a live supervisor unreachable by name. Recording the identity makes
// the check explicit instead of positional.
//
// Keyed by path, because that is the name a client dials.
var boundSockets = struct {
	mu sync.Mutex
	m  map[string]socketIdentity
}{m: map[string]socketIdentity{}}

// listeners holds the live listener for each bound path so StopServing can
// close it and unblock Accept.
var listeners = struct {
	mu sync.Mutex
	m  map[string]net.Listener
}{m: map[string]net.Listener{}}

func recordBoundSocket(path string, id socketIdentity) {
	boundSockets.mu.Lock()
	boundSockets.m[path] = id
	boundSockets.mu.Unlock()
}

func forgetBoundSocket(path string) {
	boundSockets.mu.Lock()
	delete(boundSockets.m, path)
	boundSockets.mu.Unlock()
}

// StopServing closes the listener bound at path, if this process bound one, so
// shutdown can stop accepting before it removes the name.
func StopServing(path string) {
	listeners.mu.Lock()
	listener, ours := listeners.m[path]
	listeners.mu.Unlock()
	if !ours {
		return
	}
	_ = listener.Close()
}

// RemoveIfOurs deletes the socket at path only when this process is the one
// that bound it. A path this process never bound, or one a successor has since
// rebound, is left alone: removing it would hide a live supervisor exactly the
// way an unlink does, except deliberately.
func RemoveIfOurs(path string) {
	boundSockets.mu.Lock()
	id, ours := boundSockets.m[path]
	boundSockets.mu.Unlock()
	if !ours {
		return
	}
	if !socketPathOwned(path, id) {
		// Already replaced or unlinked; either way it is not ours to remove.
		forgetBoundSocket(path)
		return
	}
	if err := os.Remove(path); err == nil {
		forgetBoundSocket(path)
	}
}

// socketOwnershipCheckInterval is how often a live listener re-checks that the
// path it bound still names its own socket. An lstat every couple of seconds is
// free next to the supervisor's own polling, and it bounds an outage caused by
// an unlink to seconds rather than until the next restart. A var so tests can
// shrink it instead of sleeping through it.
var socketOwnershipCheckInterval = 2 * time.Second

// prepareControlSocketPath clears the way for a bind, or refuses.
//
// A path that already carries a live listener is left completely alone: its
// owner is a supervisor that is still serving, and deleting the name would hide
// it without stopping it. Everything else is a leftover from a daemon that is
// gone, and replacing it is the only way to bind. A symlink is never followed
// and never removed - the socket path is not ours to reinterpret - and neither
// is anything that is not a socket or a regular file, so a stray directory or
// fifo is reported instead of silently deleted.
func prepareControlSocketPath(socketPath string, manager *supervisor.Manager) error {
	fi, err := os.Lstat(socketPath)
	if err != nil {
		return nil // absent: nothing to clear
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink, refusing to replace it", socketPath)
	case fi.Mode()&os.ModeSocket != 0:
		if socketHasListener(socketPath) {
			return fmt.Errorf("%s is held by a live listener", socketPath)
		}
	case fi.Mode().IsRegular():
		// A regular file here is debris from an interrupted start (or a stray
		// marker); the bind needs the name, and the type says it is not a
		// socket anybody is serving.
	default:
		return fmt.Errorf("%s exists and is neither a socket nor a regular file", socketPath)
	}
	_ = os.Remove(socketPath)
	return nil
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
	if err := prepareControlSocketPath(socketPath, manager); err != nil {
		return err
	}
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

	// Record what we bound before serving, so shutdownDaemon can prove the
	// path is still ours before it removes the name. A rebind re-records.
	own, haveOwn := lstatIdentity(socketPath)
	if haveOwn {
		recordBoundSocket(socketPath, own)
		listeners.mu.Lock()
		listeners.m[socketPath] = listener
		listeners.mu.Unlock()
		defer func() {
			listeners.mu.Lock()
			delete(listeners.m, socketPath)
			listeners.mu.Unlock()
		}()
	}

	// From here the daemon is serving, and the path is the only way in. Watch
	// it: an unlink leaves this listener accepting on an inode no client can
	// name, which reads to the operator as a dead daemon while its units keep
	// running. Nobody else may rebind the address (the caller holds the lock),
	// so losing the path means something outside the daemon removed it, and the
	// only repair is to bind again.
	lost := make(chan struct{})
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	if haveOwn {
		go func() {
			ticker := time.NewTicker(socketOwnershipCheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-ticker.C:
					if socketPathOwned(socketPath, own) {
						continue
					}
					// Closing the listener is what unblocks Accept; the error
					// it produces is turned into errSocketPathLost below.
					_ = listener.Close()
					close(lost)
					return
				}
			}
		}()
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-lost:
				return errSocketPathLost
			default:
			}
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
				FragmentPath:        unit.Path,
				Result:              snapshot.Result,
				MainCode:            snapshot.MainCode,
				ExitCode:            snapshot.ExitCode,
				ExecMainPID:         snapshot.ExecMainPID,
				ExecStart:           firstExecStart(unit),
				CGroup:              unit.ControlGroup(),
				CGroupPIDs:          unit.CGroupPIDs(),
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
			_, sub := service.StatePair(effState, unit.RemainActive())
			data = append(data, UnitData{Name: unit.GetConfig().Name, Description: unit.Description(), State: effState, SubState: sub, Type: utype})
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
			sub := "inactive"
			if state == "active" {
				sub = "listening"
			}
			data = append(data, UnitData{Name: name, Description: desc, State: service.State(state), SubState: sub, Type: "socket"})
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
		// systemd answers with a state (including "not-found") rather than an
		// error, and lets the client turn "not-found" into exit code 4.
		return Response{Success: true, Data: manager.IsEnabledState(req.Unit)}
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
		// A unitless show is the manager's own property set. Answering it with
		// "unit not found" made `systemctl show -p UnitPath` fail against a
		// live supervisor, so callers that locate unit files through the load
		// path gave up instead of finding nothing.
		if strings.TrimSpace(req.Unit) == "" {
			return Response{Success: true, Data: manager.ManagerProperties()}
		}
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
			BootID:            req.Boot,
			SinceUsec:         req.Since,
			UntilUsec:         req.Until,
			PriorityMax:       req.Priority,
			PrioritySet:       req.PrioritySet,
			Grep:              req.Grep,
			CaseSensitive:     req.CaseSensitive,
			Identifier:        req.Identifier,
			Invocation:        req.Invocation,
			ExcludeIdentifier: req.ExcludeIdentifier,
			LatestInvocation:  req.LatestInvocation,
			Cursor:            req.Cursor,
			CursorAfter:       req.CursorAfter,
			Lines:             req.Lines,
			LinesPlus:         req.LinesPlus,
			Reverse:           req.Reverse,
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
