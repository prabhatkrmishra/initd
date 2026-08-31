package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"initd/internal/boot"
	"initd/internal/logging"
	"initd/internal/service"
	"initd/internal/supervisor"
)

type Request struct {
	Action string `json:"action"`
	Unit   string `json:"unit,omitempty"`
	Signal string `json:"signal,omitempty"`
	Now    bool   `json:"now,omitempty"`
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
	MainPID             int           `json:"main_pid"`
	StartedAt           time.Time     `json:"started_at"`
	FinishedAt          time.Time     `json:"finished_at"`
	StartedAtMonotonic  time.Duration `json:"started_at_monotonic"`
	FinishedAtMonotonic time.Duration `json:"finished_at_monotonic"`
	LastError           string        `json:"last_error"`
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
				continue
			}
			go handleConn(conn, manager)
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
						continue
					}
					go handleConn(conn, manager)
				}
			}
		}
		return err
	}
	defer listener.Close()
	if manager != nil && manager.UserMode {
		_ = os.Chmod(socketPath, 0700)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConn(conn, manager)
	}
}

func abstractFallback(manager *supervisor.Manager) string {
	uid := os.Getuid()
	if manager != nil && manager.UserMode {
		return fmt.Sprintf("@initd-user-%d.sock", uid)
	}
	return fmt.Sprintf("@initd-system-%d.sock", uid)
}

func handleConn(conn net.Conn, manager *supervisor.Manager) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req Request
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(Response{Success: false, Message: err.Error()})
		return
	}

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
			logs := unit.Logs.Entries()
			logLines := make([]string, 0, len(logs))
			for _, entry := range logs {
				logLines = append(logLines, logging.FormatEntry(entry))
			}
			return Response{Success: true, Data: StatusData{
				Name:                unit.Config.Name,
				Description:         unit.Description(),
				State:               snapshot.State,
				MainPID:             snapshot.MainPID,
				StartedAt:           snapshot.StartedAt,
				FinishedAt:          snapshot.FinishedAt,
				StartedAtMonotonic:  snapshot.StartedAtMonotonic,
				FinishedAtMonotonic: snapshot.FinishedAtMonotonic,
				LastError:           snapshot.LastError,
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
			state := unit.Snapshot().State
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
			snapshot := unit.Snapshot()
			data = append(data, UnitData{Name: unit.Config.Name, Description: unit.Description(), State: snapshot.State, Type: unit.Config.Type})
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
			state := manager.UnitFileState(unit.Config.Name)
			data = append(data, UnitFileData{Name: unit.Config.Name, State: state, Path: unit.Path})
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
