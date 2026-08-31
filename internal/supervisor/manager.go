package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"initd/internal/logging"
	"initd/internal/parser"
	"initd/internal/service"
	"initd/internal/tmpfiles"
	"initd/internal/userpaths"
)

type Manager struct {
	mu          sync.Mutex
	startMu     sync.Mutex
	Units       map[string]*service.Unit
	SearchPaths []string
	UnitOrder   []string
	reaper      service.ExitReaper
	bootStarted bool
	bootDone    bool
	UserMode    bool
	EnabledRoot string
}

func NewManager() *Manager {
	return NewSystemManager()
}

func NewSystemManager() *Manager {
	return &Manager{
		Units: map[string]*service.Unit{},
		SearchPaths: []string{
			"/etc/systemd/system",
			"/lib/systemd/system",
			"/usr/lib/systemd/system",
		},
		EnabledRoot: "/etc/systemd/system",
		UserMode:    false,
	}
}

func NewUserManager() *Manager {
	return &Manager{
		Units:       map[string]*service.Unit{},
		SearchPaths: userpaths.UserUnitsPaths(),
		EnabledRoot: userpaths.UserEnabledRoot(),
		UserMode:    true,
	}
}

func NewManagerWithMode(userMode bool) *Manager {
	if userMode {
		return NewUserManager()
	}
	return NewSystemManager()
}

func (m *Manager) SetReaper(reaper service.ExitReaper) {
	m.mu.Lock()
	m.reaper = reaper
	for _, unit := range m.Units {
		unit.SetReaper(reaper)
	}
	m.mu.Unlock()
}

func (m *Manager) LoadUnits() error {
	if !m.UserMode {
		if err := tmpfiles.ApplyRuntimeDirs(); err != nil {
			logKernelWarning(fmt.Sprintf("tmpfiles setup failed: %v", err))
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	oldUnits := m.Units
	units := map[string]*service.Unit{}
	order := []string{}

	for _, dir := range m.SearchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".service") {
				continue
			}
			if _, exists := units[entry.Name()]; exists {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			unitConfig, err := parser.ParseUnitWithDropins(path, m.SearchPaths, m.EnabledRoot)
			if err != nil {
				continue
			}
			unitConfig.Name = entry.Name()
			if old, ok := oldUnits[entry.Name()]; ok {
				old.Config = unitConfig
				old.Path = path
				if old.Reaper() == nil && m.reaper != nil {
					old.SetReaper(m.reaper)
				}
				units[entry.Name()] = old
			} else {
				unit := service.NewUnit(unitConfig, path)
				if m.reaper != nil {
					unit.SetReaper(m.reaper)
				}
				units[entry.Name()] = unit
			}
			order = append(order, entry.Name())
		}
	}

	// Preserve template instances that were previously instantiated
	for name, oldUnit := range oldUnits {
		if _, exists := units[name]; exists {
			continue
		}
		if !isTemplateInstance(name) {
			continue
		}
		tmplName, instance := parseTemplateInstance(name)
		if tmplName == "" {
			continue
		}
		tmpl, ok := units[tmplName]
		if !ok {
			continue
		}
		// Re-instantiate from updated template
		newConfig := cloneAndExpandTemplate(tmpl.Config, name, instance)
		oldUnit.Config = newConfig
		oldUnit.Path = tmpl.Path
		if oldUnit.Reaper() == nil && m.reaper != nil {
			oldUnit.SetReaper(m.reaper)
		}
		units[name] = oldUnit
		order = append(order, name)
	}

	m.Units = units
	m.UnitOrder = order
	return nil
}

func (m *Manager) FindUnit(name string) (*service.Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if unit, ok := m.Units[name]; ok {
		return unit, nil
	}
	// Try template instance: foo@bar.service -> foo@.service
	if isTemplateInstance(name) {
		tmplName, instance := parseTemplateInstance(name)
		if tmpl, ok := m.Units[tmplName]; ok {
			newConfig := cloneAndExpandTemplate(tmpl.Config, name, instance)
			unit := service.NewUnit(newConfig, tmpl.Path)
			if m.reaper != nil {
				unit.SetReaper(m.reaper)
			}
			m.Units[name] = unit
			m.UnitOrder = append(m.UnitOrder, name)
			return unit, nil
		}
	}
	return nil, fmt.Errorf("unit %s not found", name)
}

func (m *Manager) findUnitLocked(name string) (*service.Unit, error) {
	if unit, ok := m.Units[name]; ok {
		return unit, nil
	}
	if isTemplateInstance(name) {
		tmplName, instance := parseTemplateInstance(name)
		if tmpl, ok := m.Units[tmplName]; ok {
			newConfig := cloneAndExpandTemplate(tmpl.Config, name, instance)
			unit := service.NewUnit(newConfig, tmpl.Path)
			if m.reaper != nil {
				unit.SetReaper(m.reaper)
			}
			m.Units[name] = unit
			m.UnitOrder = append(m.UnitOrder, name)
			return unit, nil
		}
	}
	return nil, fmt.Errorf("unit %s not found", name)
}

func (m *Manager) StartUnit(name string) error {
	if m.IsMasked(name) {
		return fmt.Errorf("unit %s is masked", name)
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()

	started := map[string]struct{}{}
	stack := map[string]struct{}{}
	return m.startUnitWithDependencies(name, started, stack)
}

func (m *Manager) startUnitWithDependencies(name string, started map[string]struct{}, stack map[string]struct{}) error {
	if _, ok := started[name]; ok {
		return nil
	}
	if _, ok := stack[name]; ok {
		logKernelWarning(fmt.Sprintf("Dependency cycle detected while starting %s; breaking cycle.", name))
		return nil
	}

	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	stack[name] = struct{}{}

	deps := m.collectDependencies(unit)
	if err := m.startDependencies(unit, deps, started, stack); err != nil {
		unit.MarkFailed(err.Error())
		delete(stack, name)
		return err
	}

	token, err := unit.Start()
	if err != nil {
		delete(stack, name)
		return err
	}
	m.applyRestartPolicy(unit, token)
	started[name] = struct{}{}
	delete(stack, name)
	return nil
}

func (m *Manager) StartEnabledUnits() error {
	units, err := m.EnabledUnits()
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.bootStarted = true
	m.bootDone = false
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.bootDone = true
		m.mu.Unlock()
	}()

	m.startMu.Lock()
	defer m.startMu.Unlock()

	ordered := m.orderUnitsByAfter(units)
	started := map[string]struct{}{}
	for _, unit := range ordered {
		if err := m.startUnitWithDependencies(unit.Config.Name, started, map[string]struct{}{}); err != nil {
			unit.Log(logging.LevelError, fmt.Sprintf("Failed to start enabled unit: %v", err))
		}
	}
	return nil
}

type dependency struct {
	name     string
	required bool
}

func (m *Manager) collectDependencies(unit *service.Unit) []dependency {
	deps := make([]dependency, 0, len(unit.Config.Requires)+len(unit.Config.Wants))
	seen := map[string]struct{}{}
	add := func(name string, required bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		deps = append(deps, dependency{name: name, required: required})
	}
	for _, dep := range unit.Config.Requires {
		add(dep, true)
	}
	for _, dep := range unit.Config.Wants {
		add(dep, false)
	}
	return deps
}

func (m *Manager) startDependencies(unit *service.Unit, deps []dependency, started map[string]struct{}, stack map[string]struct{}) error {
	if len(deps) == 0 {
		return nil
	}

	depUnits := make([]*service.Unit, 0, len(deps))
	depMeta := make(map[string]dependency, len(deps))
	for _, dep := range deps {
		depUnit, err := m.FindUnit(dep.name)
		if err != nil {
			if dep.required {
				return fmt.Errorf("required unit %s not found", dep.name)
			}
			unit.Log(logging.LevelError, fmt.Sprintf("Wanted unit %s not found", dep.name))
			continue
		}
		depUnits = append(depUnits, depUnit)
		depMeta[depUnit.Config.Name] = dep
	}

	ordered := m.orderUnitsByAfter(depUnits)
	for _, depUnit := range ordered {
		meta := depMeta[depUnit.Config.Name]
		if err := m.startUnitWithDependencies(depUnit.Config.Name, started, stack); err != nil {
			if meta.required {
				return fmt.Errorf("required unit %s failed: %w", depUnit.Config.Name, err)
			}
			unit.Log(logging.LevelError, fmt.Sprintf("Wanted unit %s failed: %v", depUnit.Config.Name, err))
			continue
		}
		if meta.required {
			if err := m.waitForUnitReady(depUnit, 30*time.Second); err != nil {
				return fmt.Errorf("required unit %s failed: %w", depUnit.Config.Name, err)
			}
		}
	}
	return nil
}

func (m *Manager) waitForUnitReady(unit *service.Unit, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := unit.Snapshot()
		if snapshot.State == service.StateFailed {
			if snapshot.LastError != "" {
				return errors.New(snapshot.LastError)
			}
			return fmt.Errorf("unit %s failed", unit.Config.Name)
		}
		if snapshot.State != service.StateActivating && snapshot.State != service.StateStopping {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	logKernelWarning(fmt.Sprintf("Timeout waiting for %s to finish activating; continuing.", unit.Config.Name))
	return nil
}

func (m *Manager) StopAllUnits() {
	units := m.ListUnits()

	ordered := m.orderUnitsByAfter(units)

	for i := len(ordered) - 1; i >= 0; i-- {
		unit := ordered[i]
		snap := unit.Snapshot()

		if snap.State != service.StateActive &&
			snap.State != service.StateActivating {
			continue
		}

		unit.Log(logging.LevelInfo, "Stopping for system shutdown")
		_ = unit.Stop(unit.StopTimeout())
	}
}

func (m *Manager) StopUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	return unit.Stop(unit.StopTimeout())
}

func (m *Manager) RestartUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	return unit.Restart(unit.StopTimeout())
}

func (m *Manager) ReloadUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	return unit.Reload()
}

func (m *Manager) ListUnits() []*service.Unit {
	m.mu.Lock()
	defer m.mu.Unlock()

	units := make([]*service.Unit, 0, len(m.Units))
	for _, unit := range m.Units {
		units = append(units, unit)
	}
	return units
}

func (m *Manager) SystemState() string {
	units := m.ListUnits()
	hasFailed := false

	for _, unit := range units {
		if unit.Snapshot().State == service.StateFailed {
			hasFailed = true
		}
	}

	m.mu.Lock()
	bootStarted := m.bootStarted
	bootDone := m.bootDone
	m.mu.Unlock()

	switch {
	case bootStarted && !bootDone:
		return "starting"
	case hasFailed:
		return "degraded"
	default:
		return "running"
	}
}

func (m *Manager) Reload() error {
	return m.LoadUnits()
}

func (m *Manager) applyRestartPolicy(unit *service.Unit, token int) {
	restart := strings.ToLower(strings.TrimSpace(unit.Config.Service.Restart))
	if restart == "" || restart == "no" {
		return
	}

	restartSec := 0 * time.Second
	if unit.Config.Service.RestartSec != "" {
		if parsed, err := time.ParseDuration(unit.Config.Service.RestartSec); err == nil {
			restartSec = parsed
		} else if seconds, err := time.ParseDuration(unit.Config.Service.RestartSec + "s"); err == nil {
			restartSec = seconds
		}
	}

	// systemd uses StartLimit* to avoid restart storms; we hardcode a small window.
	startLimitInterval := 10 * time.Second
	startLimitBurst := 5

	preventStatuses := unit.RestartPreventExitStatus()

	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			if !unit.IsCurrentToken(token) {
				return
			}
			unitState := unit.Snapshot().State
			if unitState == service.StateActive || unitState == service.StateActivating || unitState == service.StateStopping {
				continue
			}
			exitCode := unit.Snapshot().ExitCode
			if _, blocked := preventStatuses[exitCode]; blocked {
				return
			}
			shouldRestart := false
			switch restart {
			case "always":
				shouldRestart = true
			case "on-failure":
				shouldRestart = exitCode != 0
			}
			if !shouldRestart {
				return
			}
			restartCount := unit.RecordRestart(time.Now(), startLimitInterval)
			if restartCount > startLimitBurst {
				unit.MarkFailed("Start request repeated too quickly")
				unit.Log(logging.LevelError, "Start request repeated too quickly.")
				return
			}
			unit.Log(logging.LevelInfo, fmt.Sprintf("Restarting service (attempt %d).", restartCount))
			time.Sleep(restartSec)
			newToken, err := unit.Start()
			if err != nil {
				unit.Log(logging.LevelError, fmt.Sprintf("Restart failed: %v", err))
				return
			}
			token = newToken
		}
	}()
}

func (m *Manager) EnableUnit(name string) error {
	return m.enableUnitInternal(name, false)
}

func (m *Manager) EnableUnitWithNow(name string, now bool) error {
	return m.enableUnitInternal(name, now)
}

func (m *Manager) enableUnitInternal(name string, now bool) error {
	if m.IsMasked(name) {
		return fmt.Errorf("unit %s is masked", name)
	}
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	// If WantedBy is empty but Also/Alias exist, still allow enable for those
	hasWantedBy := len(unit.Config.Install.WantedBy) > 0
	hasAlso := len(unit.Config.Install.Also) > 0
	hasAlias := len(unit.Config.Install.Alias) > 0
	if !hasWantedBy && !hasAlso && !hasAlias {
		return errors.New("WantedBy not set")
	}
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	for _, target := range unit.Config.Install.WantedBy {
		wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
		if err := os.MkdirAll(wantsDir, 0o755); err != nil {
			return err
		}
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
		if err := os.Symlink(unit.Path, linkPath); err != nil {
			return err
		}
	}
	// Handle Also= — enable those units as well
	for _, alsoName := range unit.Config.Install.Also {
		alsoName = strings.TrimSpace(alsoName)
		if alsoName == "" {
			continue
		}
		if alsoUnit, err := m.FindUnit(alsoName); err == nil {
			for _, target := range alsoUnit.Config.Install.WantedBy {
				wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
				_ = os.MkdirAll(wantsDir, 0o755)
				linkPath := filepath.Join(wantsDir, alsoName)
				_ = os.Remove(linkPath)
				_ = os.Symlink(alsoUnit.Path, linkPath)
			}
		}
	}
	// Handle Alias= — create alias symlinks at EnabledRoot
	for _, alias := range unit.Config.Install.Alias {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		aliasPath := filepath.Join(root, alias)
		_ = os.Remove(aliasPath)
		if err := os.Symlink(unit.Path, aliasPath); err != nil {
			return err
		}
	}
	if now {
		// Best-effort start; don't fail enable if start fails
		_ = m.StartUnit(name)
	}
	return nil
}

func (m *Manager) DisableUnit(name string) error {
	return m.disableUnitInternal(name, false)
}

func (m *Manager) DisableUnitWithNow(name string, now bool) error {
	return m.disableUnitInternal(name, now)
}

func (m *Manager) disableUnitInternal(name string, now bool) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	for _, target := range unit.Config.Install.WantedBy {
		wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
	}
	// Also disable Also= units
	for _, alsoName := range unit.Config.Install.Also {
		alsoName = strings.TrimSpace(alsoName)
		if alsoName == "" {
			continue
		}
		if alsoUnit, err := m.FindUnit(alsoName); err == nil {
			for _, target := range alsoUnit.Config.Install.WantedBy {
				wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
				linkPath := filepath.Join(wantsDir, alsoName)
				_ = os.Remove(linkPath)
			}
		}
	}
	// Remove Alias symlinks
	for _, alias := range unit.Config.Install.Alias {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		aliasPath := filepath.Join(root, alias)
		if fi, err := os.Lstat(aliasPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(aliasPath); err == nil && target == unit.Path {
				_ = os.Remove(aliasPath)
			}
		}
	}
	if now {
		_ = m.StopUnit(name)
	}
	return nil
}

func (m *Manager) IsEnabled(name string) (bool, error) {
	m.mu.Lock()
	_, ok := m.Units[name]
	m.mu.Unlock()
	if !ok {
		// Template instance not yet instantiated — check if template exists
		if isTemplateInstance(name) {
			tmplName, _ := parseTemplateInstance(name)
			m.mu.Lock()
			_, tmplOk := m.Units[tmplName]
			m.mu.Unlock()
			if tmplOk {
				if m.IsMasked(name) {
					return false, nil
				}
				enabled, err := m.enabledUnitNames()
				if err != nil {
					return false, err
				}
				_, ok = enabled[name]
				return ok, nil
			}
		}
		return false, fmt.Errorf("unit %s not found", name)
	}
	if m.IsMasked(name) {
		return false, nil
	}
	enabled, err := m.enabledUnitNames()
	if err != nil {
		return false, err
	}
	_, ok = enabled[name]
	return ok, nil
}

func (m *Manager) IsMasked(name string) bool {
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	linkPath := filepath.Join(root, name)
	fi, err := os.Lstat(linkPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		return false
	}
	return target == "/dev/null"
}

func (m *Manager) MaskUnit(name string) error {
	m.mu.Lock()
	_, ok := m.Units[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unit %s not found", name)
	}
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	linkPath := filepath.Join(root, name)
	_ = os.Remove(linkPath)
	if err := os.Symlink("/dev/null", linkPath); err != nil {
		return err
	}
	return nil
}

func (m *Manager) UnmaskUnit(name string) error {
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	linkPath := filepath.Join(root, name)
	fi, err := os.Lstat(linkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		return err
	}
	if target != "/dev/null" {
		return nil
	}
	return os.Remove(linkPath)
}

func (m *Manager) IsFailed(name string) (bool, error) {
	if name == "" {
		units := m.ListUnits()
		for _, u := range units {
			if u.Snapshot().State == service.StateFailed {
				return true, nil
			}
		}
		return false, nil
	}
	unit, err := m.FindUnit(name)
	if err != nil {
		return false, err
	}
	return unit.Snapshot().State == service.StateFailed, nil
}

func (m *Manager) ResetFailed(name string) error {
	if name == "" {
		units := m.ListUnits()
		for _, u := range units {
			u.ResetFailed()
		}
		return nil
	}
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	unit.ResetFailed()
	return nil
}

func (m *Manager) KillUnit(name string, sigStr string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	sig := parseKillSignal(sigStr)
	return unit.Kill(sig)
}

func (m *Manager) ShowUnit(name string) (map[string]string, error) {
	unit, err := m.FindUnit(name)
	if err != nil {
		return nil, err
	}
	snap := unit.Snapshot()
	cfg := unit.Config
	data := map[string]string{
		"Id":                cfg.Name,
		"Names":             cfg.Name,
		"Description":       unit.Description(),
		"LoadState":         "loaded",
		"ActiveState":       string(snap.State),
		"SubState":          string(snap.State),
		"FragmentPath":      unit.Path,
		"UnitFileState":     m.UnitFileState(cfg.Name),
		"MainPID":           fmt.Sprintf("%d", snap.MainPID),
		"ExecMainPID":       fmt.Sprintf("%d", snap.MainPID),
		"ExitCode":          fmt.Sprintf("%d", snap.ExitCode),
		"Result":            snap.LastError,
		"Type":              cfg.Service.Type,
		"After":             strings.Join(cfg.After, " "),
		"Requires":          strings.Join(cfg.Requires, " "),
		"Wants":             strings.Join(cfg.Wants, " "),
		"ExecStart":         cfg.Service.ExecStart,
		"WantedBy":          strings.Join(cfg.Install.WantedBy, " "),
	}
	if !snap.StartedAt.IsZero() {
		data["ActiveEnterTimestamp"] = snap.StartedAt.Format(time.RFC3339)
		data["ActiveEnterTimestampMonotonic"] = fmt.Sprintf("%d", snap.StartedAtMonotonic.Microseconds())
	}
	if !snap.FinishedAt.IsZero() {
		data["InactiveEnterTimestamp"] = snap.FinishedAt.Format(time.RFC3339)
		data["InactiveEnterTimestampMonotonic"] = fmt.Sprintf("%d", snap.FinishedAtMonotonic.Microseconds())
	}
	return data, nil
}

func (m *Manager) CatUnit(name string) (string, error) {
	unit, err := m.FindUnit(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(unit.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (m *Manager) UnitFileState(name string) string {
	if m.IsMasked(name) {
		return "masked"
	}
	enabled, _ := m.IsEnabled(name)
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func parseKillSignal(raw string) syscall.Signal {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return syscall.SIGTERM
	}
	raw = strings.TrimPrefix(raw, "-")
	// numeric signal like "9" or "15"
	if n, err := fmt.Sscanf(raw, "%d", new(int)); err == nil && n == 1 {
		var num int
		fmt.Sscanf(raw, "%d", &num)
		if num > 0 {
			return syscall.Signal(num)
		}
	}
	raw = strings.ToUpper(raw)
	if !strings.HasPrefix(raw, "SIG") {
		raw = "SIG" + raw
	}
	signals := map[string]syscall.Signal{
		"SIGHUP":  syscall.SIGHUP,
		"SIGINT":  syscall.SIGINT,
		"SIGQUIT": syscall.SIGQUIT,
		"SIGILL":  syscall.SIGILL,
		"SIGTRAP": syscall.SIGTRAP,
		"SIGABRT": syscall.SIGABRT,
		"SIGBUS":  syscall.SIGBUS,
		"SIGFPE":  syscall.SIGFPE,
		"SIGKILL": syscall.SIGKILL,
		"SIGUSR1": syscall.SIGUSR1,
		"SIGSEGV": syscall.SIGSEGV,
		"SIGUSR2": syscall.SIGUSR2,
		"SIGPIPE": syscall.SIGPIPE,
		"SIGALRM": syscall.SIGALRM,
		"SIGTERM": syscall.SIGTERM,
		"SIGCHLD": syscall.SIGCHLD,
		"SIGCONT": syscall.SIGCONT,
		"SIGSTOP": syscall.SIGSTOP,
	}
	if sig, ok := signals[raw]; ok {
		return sig
	}
	return syscall.SIGTERM
}

func (m *Manager) ListUnitFiles() ([]*service.Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	units := make([]*service.Unit, 0, len(m.Units))
	for _, unit := range m.Units {
		units = append(units, unit)
	}
	return units, nil
}

func (m *Manager) EnabledUnits() ([]*service.Unit, error) {
	enabled, err := m.enabledUnitNames()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := make([]*service.Unit, 0, len(enabled))
	for _, name := range m.UnitOrder {
		if _, ok := enabled[name]; !ok {
			continue
		}
		if unit, ok := m.Units[name]; ok {
			ordered = append(ordered, unit)
		}
	}
	return ordered, nil
}

func (m *Manager) enabledUnitNames() (map[string]struct{}, error) {
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}

	enabled := map[string]struct{}{}
	for _, entry := range dirs {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wants") {
			continue
		}
		wantsDir := filepath.Join(root, entry.Name())
		wantsEntries, err := os.ReadDir(wantsDir)
		if err != nil {
			continue
		}
		for _, want := range wantsEntries {
			name := want.Name()
			if !strings.HasSuffix(name, ".service") {
				continue
			}
			linkPath := filepath.Join(wantsDir, name)
			fi, err := os.Lstat(linkPath)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			enabled[name] = struct{}{}
		}
	}
	return enabled, nil
}

func (m *Manager) orderUnitsByAfter(units []*service.Unit) []*service.Unit {
	if len(units) <= 1 {
		return units
	}

	orderIndex := map[string]int{}
	nameToUnit := map[string]*service.Unit{}
	for idx, unit := range units {
		orderIndex[unit.Config.Name] = idx
		nameToUnit[unit.Config.Name] = unit
	}

	adj := map[string][]string{}
	indegree := map[string]int{}
	for _, unit := range units {
		indegree[unit.Config.Name] = 0
	}

	for _, unit := range units {
		for _, dep := range unit.Config.After {
			if strings.HasSuffix(dep, ".target") || !strings.HasSuffix(dep, ".service") {
				continue
			}
			if _, ok := nameToUnit[dep]; !ok || dep == unit.Config.Name {
				continue
			}
			adj[dep] = append(adj[dep], unit.Config.Name)
			indegree[unit.Config.Name]++
		}
	}

	queue := make([]string, 0, len(units))
	for _, unit := range units {
		if indegree[unit.Config.Name] == 0 {
			queue = append(queue, unit.Config.Name)
		}
	}

	sorted := make([]*service.Unit, 0, len(units))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		sorted = append(sorted, nameToUnit[name])

		neighbors := adj[name]
		if len(neighbors) > 1 {
			sort.SliceStable(neighbors, func(i, j int) bool {
				return orderIndex[neighbors[i]] < orderIndex[neighbors[j]]
			})
		}
		for _, next := range neighbors {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(units) {
		// Best-effort ordering: on cycles fall back to file order.
		logKernelWarning("After= dependency cycle detected; falling back to file order.")
		return units
	}
	return sorted
}

func logKernelWarning(message string) {
	logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "%s", message)
}

func isTemplateInstance(name string) bool {
	if !strings.HasSuffix(name, ".service") {
		return false
	}
	base := strings.TrimSuffix(name, ".service")
	atIdx := strings.Index(base, "@")
	if atIdx < 0 {
		return false
	}
	instance := base[atIdx+1:]
	return instance != "" && instance != "."
}

func parseTemplateInstance(name string) (string, string) {
	if !isTemplateInstance(name) {
		return "", ""
	}
	suffix := ".service"
	base := strings.TrimSuffix(name, suffix)
	atIdx := strings.Index(base, "@")
	prefix := base[:atIdx]
	instance := base[atIdx+1:]
	tmpl := prefix + "@" + suffix
	return tmpl, instance
}

func cloneAndExpandTemplate(tmpl *parser.Unit, instanceName, instance string) *parser.Unit {
	if tmpl == nil {
		return nil
	}
	clone := *tmpl
	clone.Name = instanceName
	clone.Description = expandSpecifiers(clone.Description, instanceName, instance)
	clone.After = expandSlice(clone.After, instanceName, instance)
	clone.Requires = expandSlice(clone.Requires, instanceName, instance)
	clone.Wants = expandSlice(clone.Wants, instanceName, instance)
	clone.ConditionPathExists = expandSlice(clone.ConditionPathExists, instanceName, instance)
	clone.Service.ExecStart = expandSpecifiers(clone.Service.ExecStart, instanceName, instance)
	clone.Service.ExecStop = expandSpecifiers(clone.Service.ExecStop, instanceName, instance)
	clone.Service.ExecCondition = expandSlice(clone.Service.ExecCondition, instanceName, instance)
	clone.Service.ExecStartPre = expandSlice(clone.Service.ExecStartPre, instanceName, instance)
	clone.Service.ExecStartPost = expandSlice(clone.Service.ExecStartPost, instanceName, instance)
	clone.Service.ExecStopPost = expandSlice(clone.Service.ExecStopPost, instanceName, instance)
	clone.Service.ExecReload = expandSlice(clone.Service.ExecReload, instanceName, instance)
	clone.Service.WorkingDirectory = expandSpecifiers(clone.Service.WorkingDirectory, instanceName, instance)
	clone.Service.RootDirectory = expandSpecifiers(clone.Service.RootDirectory, instanceName, instance)
	clone.Service.PIDFile = expandSpecifiers(clone.Service.PIDFile, instanceName, instance)
	clone.Service.Environment = expandSlice(clone.Service.Environment, instanceName, instance)
	clone.Service.EnvironmentFile = expandSlice(clone.Service.EnvironmentFile, instanceName, instance)
	clone.Service.RuntimeDirectory = expandSlice(clone.Service.RuntimeDirectory, instanceName, instance)
	clone.Service.StateDirectory = expandSlice(clone.Service.StateDirectory, instanceName, instance)
	clone.Service.CacheDirectory = expandSlice(clone.Service.CacheDirectory, instanceName, instance)
	clone.Service.LogsDirectory = expandSlice(clone.Service.LogsDirectory, instanceName, instance)
	clone.Service.ConfigurationDirectory = expandSlice(clone.Service.ConfigurationDirectory, instanceName, instance)
	clone.Install.WantedBy = expandSlice(clone.Install.WantedBy, instanceName, instance)
	clone.Install.Also = expandSlice(clone.Install.Also, instanceName, instance)
	clone.Install.Alias = expandSlice(clone.Install.Alias, instanceName, instance)
	return &clone
}

func expandSlice(in []string, fullName, instance string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expandSpecifiers(s, fullName, instance)
	}
	return out
}

func expandSpecifiers(s, fullName, instance string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	prefix := fullName
	if idx := strings.Index(fullName, "@"); idx >= 0 {
		prefix = fullName[:idx]
	}
	nameWithoutSuffix := strings.TrimSuffix(fullName, ".service")
	// Escape handling: %% -> %
	s = strings.ReplaceAll(s, "%%", "\x00")
	s = strings.ReplaceAll(s, "%i", instance)
	s = strings.ReplaceAll(s, "%I", instance)
	s = strings.ReplaceAll(s, "%n", fullName)
	s = strings.ReplaceAll(s, "%N", nameWithoutSuffix)
	s = strings.ReplaceAll(s, "%p", prefix)
	s = strings.ReplaceAll(s, "\x00", "%")
	return s
}
