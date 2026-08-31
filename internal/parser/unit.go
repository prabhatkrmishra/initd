package parser

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Unit struct {
	Name                string
	Type                string
	Description         string
	After               []string
	Requires            []string
	Wants               []string
	ConditionPathExists []string
	Service             ServiceSection
	Socket              SocketSection
	Install             InstallSection
	Ignored             map[string]string
}

type ServiceSection struct {
	Type                     string
	PermissionsStartOnly     bool
	ExecCondition            []string
	ExecStartPre             []string
	ExecStartPost            []string
	ExecStart                string
	ExecStartSet             bool
	ExecStop                 string
	ExecStopPost             []string
	ExecReload               []string
	Restart                  string
	RestartSec               string
	RestartPreventExitStatus string
	SuccessExitStatus        string
	PIDFile                  string
	WorkingDirectory         string
	RootDirectory            string
	RuntimeDirectory         []string
	RuntimeDirectoryMode     string
	StateDirectory           []string
	CacheDirectory           []string
	LogsDirectory            []string
	ConfigurationDirectory   []string
	KillMode                 string
	TimeoutStartSec          string
	TimeoutStopSec           string
	TimeoutSec               string
	User                     string
	Group                    string
	SupplementaryGroups      []string
	UMask                    string
	LimitNOFILE              string
	Environment              []string
	EnvironmentFile          []string
}

type SocketSection struct {
	ListenStream   []string
	ListenDatagram []string
	SocketMode     string
}

type InstallSection struct {
	WantedBy []string
	Also     []string
	Alias    []string
}

var ignoredKeys = map[string]struct{}{
	"MemoryMax":               {},
	"CPUQuota":                {},
	"TasksMax":                {},
	"IOWeight":                {},
	"DeviceAllow":             {},
	"DeviceDeny":              {},
	"PrivateTmp":              {},
	"ProtectSystem":           {},
	"RestrictNamespaces":      {},
	"CapabilityBoundingSet":   {},
	"AmbientCapabilities":     {},
	"SystemCallFilter":        {},
	"SystemCallArchitectures": {},
	"ProtectProc":             {},
	"ProcSubset":              {},
	"NoExecPaths":             {},
	"ExecPaths":               {},
	"PrivateDevices":          {},
	"ProtectHome":             {},
	"ReadWritePaths":          {},
	"ReadOnlyPaths":           {},
	"InaccessiblePaths":       {},
	"ReadWriteDirectories":    {},
	"ReadOnlyDirectories":     {},
	"InaccessibleDirectories": {},
	"NoNewPrivileges":         {},
	"LockPersonality":         {},
	"MemoryDenyWriteExecute":  {},
	"PrivateUsers":            {},
	"ProtectClock":            {},
	"ProtectControlGroups":    {},
	"ProtectHostname":         {},
	"ProtectKernelLogs":       {},
	"ProtectKernelModules":    {},
	"ProtectKernelTunables":   {},
	"RemoveIPC":               {},
	"RestrictAddressFamilies": {},
	"RestrictRealtime":        {},
	"RestrictSUIDSGID":        {},
	"OOMScoreAdjust":          {},
	"Nice":                    {},
	"IOSchedulingClass":       {},
	"IOSchedulingPriority":    {},
	"CPUSchedulingPolicy":     {},
	"CPUSchedulingPriority":   {},
	"CPUAffinity":             {},
	"LimitNPROC":              {},
	"LimitCORE":               {},
	"LimitMEMLOCK":            {},
	"LimitAS":                 {},
	"LimitRSS":                {},
	"LimitDATA":               {},
	"LimitSTACK":              {},
	"LimitCPU":                {},
	"Slice":                   {},
	"Delegate":                {},
	"TasksAccounting":         {},
	"MemoryAccounting":        {},
	"CPUAccounting":           {},
	"IOAccounting":            {},
	"BlockIOAccounting":       {},
	"DefaultDependencies":     {},
}

func ParseUnit(path string) (*Unit, error) {
	return parseUnitFile(path, filepath.Base(path))
}

func ParseUnitWithDropins(path string, searchPaths []string, enabledRoot string) (*Unit, error) {
	baseName := filepath.Base(path)
	unit, err := parseUnitFile(path, baseName)
	if err != nil {
		return nil, err
	}
	// Collect drop-in files: <searchDir>/<unit>.d/*.conf and <enabledRoot>/<unit>.d/*.conf
	seen := map[string]struct{}{}
	var dropins []string
	for _, dir := range searchPaths {
		dropinDir := filepath.Join(dir, baseName+".d")
		entries, err := os.ReadDir(dropinDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			full := filepath.Join(dropinDir, e.Name())
			if _, ok := seen[full]; ok {
				continue
			}
			seen[full] = struct{}{}
			dropins = append(dropins, full)
		}
	}
	if enabledRoot != "" {
		dropinDir := filepath.Join(enabledRoot, baseName+".d")
		entries, err := os.ReadDir(dropinDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
					continue
				}
				full := filepath.Join(dropinDir, e.Name())
				if _, ok := seen[full]; ok {
					continue
				}
				seen[full] = struct{}{}
				dropins = append(dropins, full)
			}
		}
	}
	// Sort lexically for deterministic override order
	if len(dropins) > 1 {
		// simple sort
		for i := 0; i < len(dropins)-1; i++ {
			for j := i + 1; j < len(dropins); j++ {
				if dropins[j] < dropins[i] {
					dropins[i], dropins[j] = dropins[j], dropins[i]
				}
			}
		}
	}
	for _, dp := range dropins {
		overlay, err := parseUnitFile(dp, baseName)
		if err != nil {
			continue
		}
		mergeUnit(unit, overlay)
	}
	return unit, nil
}

func parseUnitFile(path string, name string) (*Unit, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	unit := &Unit{
		Name:    name,
		Ignored: map[string]string{},
	}

	switch {
	case strings.HasSuffix(unit.Name, ".socket"):
		unit.Type = "socket"
	case strings.HasSuffix(unit.Name, ".service"):
		unit.Type = "service"
	default:
		unit.Type = "unknown"
	}

	scanner := bufio.NewScanner(file)
	section := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid line in %s: %s", path, line)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if _, ok := ignoredKeys[key]; ok {
			unit.Ignored[key] = value
			continue
		}

		switch section {
		case "Unit":
			switch key {
			case "Description":
				if value == "" {
					unit.Description = ""
				} else {
					unit.Description = value
				}
			case "After":
				if value == "" {
					unit.After = nil
				} else {
					unit.After = append(unit.After, splitList(value)...)
				}
			case "Requires":
				if value == "" {
					unit.Requires = nil
				} else {
					unit.Requires = append(unit.Requires, splitList(value)...)
				}
			case "Wants":
				if value == "" {
					unit.Wants = nil
				} else {
					unit.Wants = append(unit.Wants, splitList(value)...)
				}
			case "ConditionPathExists":
				if value == "" {
					unit.ConditionPathExists = nil
				} else {
					unit.ConditionPathExists = append(unit.ConditionPathExists, value)
				}
			default:
				unit.Ignored["Unit."+key] = value
			}
		case "Service":
			switch key {
			case "Type":
				unit.Service.Type = value
			case "PermissionsStartOnly":
				unit.Service.PermissionsStartOnly = strings.EqualFold(value, "yes") || strings.EqualFold(value, "true")
			case "ExecCondition":
				if value == "" {
					unit.Service.ExecCondition = nil
				} else {
					unit.Service.ExecCondition = append(unit.Service.ExecCondition, value)
				}
			case "ExecStartPre":
				if value == "" {
					unit.Service.ExecStartPre = nil
				} else {
					unit.Service.ExecStartPre = append(unit.Service.ExecStartPre, value)
				}
			case "ExecStartPost":
				if value == "" {
					unit.Service.ExecStartPost = nil
				} else {
					unit.Service.ExecStartPost = append(unit.Service.ExecStartPost, value)
				}
			case "ExecStart":
				unit.Service.ExecStart = value
				unit.Service.ExecStartSet = true
			case "ExecStop":
				unit.Service.ExecStop = value
			case "ExecStopPost":
				if value == "" {
					unit.Service.ExecStopPost = nil
				} else {
					unit.Service.ExecStopPost = append(unit.Service.ExecStopPost, value)
				}
			case "ExecReload":
				if value == "" {
					unit.Service.ExecReload = nil
				} else {
					unit.Service.ExecReload = append(unit.Service.ExecReload, value)
				}
			case "Restart":
				unit.Service.Restart = value
			case "RestartSec":
				unit.Service.RestartSec = value
			case "RestartPreventExitStatus":
				unit.Service.RestartPreventExitStatus = value
			case "SuccessExitStatus":
				unit.Service.SuccessExitStatus = value
			case "PIDFile":
				unit.Service.PIDFile = value
			case "WorkingDirectory":
				unit.Service.WorkingDirectory = value
			case "RootDirectory":
				unit.Service.RootDirectory = value
			case "RuntimeDirectory":
				if value == "" {
					unit.Service.RuntimeDirectory = nil
				} else {
					unit.Service.RuntimeDirectory = append(unit.Service.RuntimeDirectory, splitList(value)...)
				}
			case "RuntimeDirectoryMode":
				unit.Service.RuntimeDirectoryMode = value
			case "StateDirectory":
				if value == "" {
					unit.Service.StateDirectory = nil
				} else {
					unit.Service.StateDirectory = append(unit.Service.StateDirectory, splitList(value)...)
				}
			case "CacheDirectory":
				if value == "" {
					unit.Service.CacheDirectory = nil
				} else {
					unit.Service.CacheDirectory = append(unit.Service.CacheDirectory, splitList(value)...)
				}
			case "LogsDirectory":
				if value == "" {
					unit.Service.LogsDirectory = nil
				} else {
					unit.Service.LogsDirectory = append(unit.Service.LogsDirectory, splitList(value)...)
				}
			case "ConfigurationDirectory":
				if value == "" {
					unit.Service.ConfigurationDirectory = nil
				} else {
					unit.Service.ConfigurationDirectory = append(unit.Service.ConfigurationDirectory, splitList(value)...)
				}
			case "KillMode":
				unit.Service.KillMode = value
			case "TimeoutStartSec":
				unit.Service.TimeoutStartSec = value
			case "TimeoutStopSec":
				unit.Service.TimeoutStopSec = value
			case "TimeoutSec":
				unit.Service.TimeoutSec = value
			case "User":
				unit.Service.User = value
			case "Group":
				unit.Service.Group = value
			case "SupplementaryGroups":
				if value == "" {
					unit.Service.SupplementaryGroups = nil
				} else {
					unit.Service.SupplementaryGroups = append(unit.Service.SupplementaryGroups, splitList(value)...)
				}
			case "UMask":
				unit.Service.UMask = value
			case "LimitNOFILE":
				unit.Service.LimitNOFILE = value
			case "Environment":
				if value == "" {
					unit.Service.Environment = nil
				} else {
					unit.Service.Environment = append(unit.Service.Environment, value)
				}
			case "EnvironmentFile":
				if value == "" {
					unit.Service.EnvironmentFile = nil
				} else {
					unit.Service.EnvironmentFile = append(unit.Service.EnvironmentFile, value)
				}
			default:
				unit.Ignored["Service."+key] = value
			}
		case "Socket":
			switch key {
			case "ListenStream":
				if value == "" {
					unit.Socket.ListenStream = nil
				} else {
					unit.Socket.ListenStream = append(unit.Socket.ListenStream, value)
				}
			case "ListenDatagram":
				if value == "" {
					unit.Socket.ListenDatagram = nil
				} else {
					unit.Socket.ListenDatagram = append(unit.Socket.ListenDatagram, value)
				}
			case "SocketMode":
				unit.Socket.SocketMode = value
			default:
				unit.Ignored["Socket."+key] = value
			}

		case "Install":
			switch key {
			case "WantedBy":
				if value == "" {
					unit.Install.WantedBy = nil
				} else {
					unit.Install.WantedBy = append(unit.Install.WantedBy, splitList(value)...)
				}
			case "Also":
				if value == "" {
					unit.Install.Also = nil
				} else {
					unit.Install.Also = append(unit.Install.Also, splitList(value)...)
				}
			case "Alias":
				if value == "" {
					unit.Install.Alias = nil
				} else {
					unit.Install.Alias = append(unit.Install.Alias, splitList(value)...)
				}
			default:
				unit.Ignored["Install."+key] = value
			}
		default:
			unit.Ignored[section+"."+key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if unit.Type == "socket" {
		if len(unit.Socket.ListenStream) == 0 && len(unit.Socket.ListenDatagram) == 0 {
			return nil, fmt.Errorf("socket unit missing ListenStream/ListenDatagram")
		}
	}

	return unit, nil
}

func mergeUnit(base, overlay *Unit) {
	if overlay.Description != "" {
		base.Description = overlay.Description
	}
	if len(overlay.After) > 0 {
		base.After = append(base.After, overlay.After...)
	}
	if len(overlay.Requires) > 0 {
		base.Requires = append(base.Requires, overlay.Requires...)
	}
	if len(overlay.Wants) > 0 {
		base.Wants = append(base.Wants, overlay.Wants...)
	}
	if len(overlay.ConditionPathExists) > 0 {
		base.ConditionPathExists = append(base.ConditionPathExists, overlay.ConditionPathExists...)
	}
	// Service
	if overlay.Service.Type != "" {
		base.Service.Type = overlay.Service.Type
	}
	if len(overlay.Service.ExecCondition) > 0 {
		base.Service.ExecCondition = append(base.Service.ExecCondition, overlay.Service.ExecCondition...)
	}
	if len(overlay.Service.ExecStartPre) > 0 {
		base.Service.ExecStartPre = append(base.Service.ExecStartPre, overlay.Service.ExecStartPre...)
	}
	if len(overlay.Service.ExecStartPost) > 0 {
		base.Service.ExecStartPost = append(base.Service.ExecStartPost, overlay.Service.ExecStartPost...)
	}
	if overlay.Service.ExecStartSet {
		base.Service.ExecStart = overlay.Service.ExecStart
		base.Service.ExecStartSet = true
	}
	if overlay.Service.ExecStop != "" {
		base.Service.ExecStop = overlay.Service.ExecStop
	}
	if len(overlay.Service.ExecStopPost) > 0 {
		base.Service.ExecStopPost = append(base.Service.ExecStopPost, overlay.Service.ExecStopPost...)
	}
	if len(overlay.Service.ExecReload) > 0 {
		base.Service.ExecReload = append(base.Service.ExecReload, overlay.Service.ExecReload...)
	}
	if overlay.Service.Restart != "" {
		base.Service.Restart = overlay.Service.Restart
	}
	if overlay.Service.RestartSec != "" {
		base.Service.RestartSec = overlay.Service.RestartSec
	}
	if overlay.Service.RestartPreventExitStatus != "" {
		base.Service.RestartPreventExitStatus = overlay.Service.RestartPreventExitStatus
	}
	if overlay.Service.SuccessExitStatus != "" {
		base.Service.SuccessExitStatus = overlay.Service.SuccessExitStatus
	}
	if overlay.Service.PIDFile != "" {
		base.Service.PIDFile = overlay.Service.PIDFile
	}
	if overlay.Service.WorkingDirectory != "" {
		base.Service.WorkingDirectory = overlay.Service.WorkingDirectory
	}
	if overlay.Service.RootDirectory != "" {
		base.Service.RootDirectory = overlay.Service.RootDirectory
	}
	if len(overlay.Service.RuntimeDirectory) > 0 {
		base.Service.RuntimeDirectory = append(base.Service.RuntimeDirectory, overlay.Service.RuntimeDirectory...)
	}
	if overlay.Service.RuntimeDirectoryMode != "" {
		base.Service.RuntimeDirectoryMode = overlay.Service.RuntimeDirectoryMode
	}
	if len(overlay.Service.StateDirectory) > 0 {
		base.Service.StateDirectory = append(base.Service.StateDirectory, overlay.Service.StateDirectory...)
	}
	if len(overlay.Service.CacheDirectory) > 0 {
		base.Service.CacheDirectory = append(base.Service.CacheDirectory, overlay.Service.CacheDirectory...)
	}
	if len(overlay.Service.LogsDirectory) > 0 {
		base.Service.LogsDirectory = append(base.Service.LogsDirectory, overlay.Service.LogsDirectory...)
	}
	if len(overlay.Service.ConfigurationDirectory) > 0 {
		base.Service.ConfigurationDirectory = append(base.Service.ConfigurationDirectory, overlay.Service.ConfigurationDirectory...)
	}
	if overlay.Service.KillMode != "" {
		base.Service.KillMode = overlay.Service.KillMode
	}
	if overlay.Service.TimeoutStartSec != "" {
		base.Service.TimeoutStartSec = overlay.Service.TimeoutStartSec
	}
	if overlay.Service.TimeoutStopSec != "" {
		base.Service.TimeoutStopSec = overlay.Service.TimeoutStopSec
	}
	if overlay.Service.TimeoutSec != "" {
		base.Service.TimeoutSec = overlay.Service.TimeoutSec
	}
	if overlay.Service.User != "" {
		base.Service.User = overlay.Service.User
	}
	if overlay.Service.Group != "" {
		base.Service.Group = overlay.Service.Group
	}
	if len(overlay.Service.SupplementaryGroups) > 0 {
		base.Service.SupplementaryGroups = append(base.Service.SupplementaryGroups, overlay.Service.SupplementaryGroups...)
	}
	if overlay.Service.UMask != "" {
		base.Service.UMask = overlay.Service.UMask
	}
	if overlay.Service.LimitNOFILE != "" {
		base.Service.LimitNOFILE = overlay.Service.LimitNOFILE
	}
	if len(overlay.Service.Environment) > 0 {
		base.Service.Environment = append(base.Service.Environment, overlay.Service.Environment...)
	}
	if len(overlay.Service.EnvironmentFile) > 0 {
		base.Service.EnvironmentFile = append(base.Service.EnvironmentFile, overlay.Service.EnvironmentFile...)
	}
	if len(overlay.Socket.ListenStream) > 0 {
		base.Socket.ListenStream = append(base.Socket.ListenStream, overlay.Socket.ListenStream...)
	}
	if len(overlay.Socket.ListenDatagram) > 0 {
		base.Socket.ListenDatagram = append(base.Socket.ListenDatagram, overlay.Socket.ListenDatagram...)
	}
	if overlay.Socket.SocketMode != "" {
		base.Socket.SocketMode = overlay.Socket.SocketMode
	}
	if len(overlay.Install.WantedBy) > 0 {
		base.Install.WantedBy = append(base.Install.WantedBy, overlay.Install.WantedBy...)
	}
	if len(overlay.Install.Also) > 0 {
		base.Install.Also = append(base.Install.Also, overlay.Install.Also...)
	}
	if len(overlay.Install.Alias) > 0 {
		base.Install.Alias = append(base.Install.Alias, overlay.Install.Alias...)
	}
	for k, v := range overlay.Ignored {
		base.Ignored[k] = v
	}
}

func splitList(value string) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
