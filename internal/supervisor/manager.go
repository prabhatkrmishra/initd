package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	mu             sync.Mutex
	startMu        sync.Mutex
	Units          map[string]*service.Unit
	SocketUnits    map[string]*parser.Unit
	SocketPaths    map[string]string
	SocketRuntimes map[string]*socketRuntime
	SearchPaths    []string
	UnitOrder      []string
	reaper         service.ExitReaper
	bootStarted    bool
	bootDone       bool
	UserMode       bool
	EnabledRoot    string
}

type socketRuntime struct {
	unit      *parser.Unit
	listeners []net.Listener
	packets   []net.PacketConn
	paths     []string
	path      string
	listener  net.Listener
	packet    net.PacketConn
	active    bool
	stopCh    chan struct{}
}

func NewManager() *Manager {
	return NewSystemManager()
}

func NewSystemManager() *Manager {
	return &Manager{
		Units:          map[string]*service.Unit{},
		SocketUnits:    map[string]*parser.Unit{},
		SocketRuntimes: map[string]*socketRuntime{},
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
		Units:          map[string]*service.Unit{},
		SocketUnits:    map[string]*parser.Unit{},
		SocketRuntimes: map[string]*socketRuntime{},
		SearchPaths:    userpaths.UserUnitsPaths(),
		EnabledRoot:    userpaths.UserEnabledRoot(),
		UserMode:       true,
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
	oldSocketUnits := m.SocketUnits
	oldSocketPaths := m.SocketPaths
	units := map[string]*service.Unit{}
	order := []string{}
	newSocketUnits := map[string]*parser.Unit{}
	newSocketPaths := map[string]string{}
	// Preserve existing runtimes map
	if m.SocketRuntimes == nil {
		m.SocketRuntimes = map[string]*socketRuntime{}
	}

	for _, dir := range m.SearchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			isService := strings.HasSuffix(entry.Name(), ".service")
			isSocket := strings.HasSuffix(entry.Name(), ".socket")
			if !isService && !isSocket {
				continue
			}
			if _, exists := units[entry.Name()]; exists {
				continue
			}
			if _, exists := newSocketUnits[entry.Name()]; exists {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			unitConfig, err := parser.ParseUnitWithDropins(path, m.SearchPaths, m.EnabledRoot)
			if err != nil {
				continue
			}
			unitConfig.Name = entry.Name()
			if isSocket {
				newSocketUnits[entry.Name()] = unitConfig
				newSocketPaths[entry.Name()] = path
				// Preserve old config if exists but update
				if _, ok := oldSocketUnits[entry.Name()]; ok {
					// keep runtime if active
				}
				_ = oldSocketPaths
				continue
			}
			if old, ok := oldUnits[entry.Name()]; ok {
				old.Config = unitConfig
				old.Path = path
				if old.Reaper() == nil && m.reaper != nil {
					old.SetReaper(m.reaper)
				}
				old.SetOnFailureHandler(m.onFailureCallback(old.Config.Name))
				units[entry.Name()] = old
			} else {
				unit := service.NewUnit(unitConfig, path)
				if m.reaper != nil {
					unit.SetReaper(m.reaper)
				}
				unit.SetOnFailureHandler(m.onFailureCallback(unit.Config.Name))
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
	m.SocketUnits = newSocketUnits
	m.SocketPaths = newSocketPaths
	// Clean up runtimes for removed socket units
	for name, rt := range m.SocketRuntimes {
		if _, ok := newSocketUnits[name]; !ok {
			if rt.active {
				close(rt.stopCh)
				if rt.listener != nil {
					_ = rt.listener.Close()
				}
				if rt.packet != nil {
					_ = rt.packet.Close()
				}
				if rt.path != "" {
					_ = os.Remove(rt.path)
				}
			}
			delete(m.SocketRuntimes, name)
		}
	}
	return nil
}

func (m *Manager) onFailureCallback(unitName string) func(string) {
	return func(failedUnit string) {
		m.mu.Lock()
		cfg, ok := m.Units[failedUnit]
		if !ok {
			m.mu.Unlock()
			return
		}
		targets := append([]string{}, cfg.Config.OnFailure...)
		// Collect BindsTo dependents that should be stopped on failure
		bindsDependents := []string{}
		for otherName, otherUnit := range m.Units {
			for _, b := range otherUnit.Config.BindsTo {
				if strings.TrimSpace(b) == failedUnit {
					bindsDependents = append(bindsDependents, otherName)
					break
				}
			}
		}
		m.mu.Unlock()
		for _, t := range targets {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			_ = m.StartUnit(t)
		}
		for _, dep := range bindsDependents {
			if u, err := m.FindUnit(dep); err == nil {
				if snap := u.Snapshot(); snap.State == service.StateActive || snap.State == service.StateActivating {
					_ = u.Stop(u.StopTimeout())
				} else if snap.State != service.StateFailed {
					u.MarkFailed(fmt.Sprintf("BindsTo=%s failed", failedUnit))
				}
			}
		}
		_ = unitName
	}
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
			unit.SetOnFailureHandler(m.onFailureCallback(name))
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
	if strings.HasSuffix(name, ".socket") {
		return m.startSocketUnit(name)
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()

	started := map[string]struct{}{}
	stack := map[string]struct{}{}
	return m.startUnitWithDependencies(name, started, stack)
}

func (m *Manager) startSocketUnit(name string) error {
	m.mu.Lock()
	cfg, ok := m.SocketUnits[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unit %s not found", name)
	}
	if rt, ok := m.SocketRuntimes[name]; ok && rt.active {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	if len(cfg.Socket.ListenStream) == 0 && len(cfg.Socket.ListenDatagram) == 0 {
		return fmt.Errorf("socket unit %s has no ListenStream/ListenDatagram", name)
	}

	var listeners []net.Listener
	var packets []net.PacketConn
	var paths []string
	var firstPath string

	for _, sockPath := range cfg.Socket.ListenStream {
		dir := filepath.Dir(sockPath)
		_ = os.MkdirAll(dir, 0755)
		_ = os.Remove(sockPath)
		l, err := net.Listen("unix", sockPath)
		if err != nil {
			for _, ll := range listeners {
				_ = ll.Close()
			}
			for _, pp := range packets {
				_ = pp.Close()
			}
			for _, p := range paths {
				_ = os.Remove(p)
			}
			return fmt.Errorf("failed to listen on %s: %w", sockPath, err)
		}
		if cfg.Socket.SocketMode != "" {
			if mode, perr := strconv.ParseUint(cfg.Socket.SocketMode, 8, 32); perr == nil {
				_ = os.Chmod(sockPath, os.FileMode(mode))
			}
		}
		listeners = append(listeners, l)
		paths = append(paths, sockPath)
		if firstPath == "" {
			firstPath = sockPath
		}
	}
	for _, sockPath := range cfg.Socket.ListenDatagram {
		dir := filepath.Dir(sockPath)
		_ = os.MkdirAll(dir, 0755)
		_ = os.Remove(sockPath)
		addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
		pc, err := net.ListenUnixgram("unixgram", addr)
		if err != nil {
			for _, ll := range listeners {
				_ = ll.Close()
			}
			for _, pp := range packets {
				_ = pp.Close()
			}
			for _, p := range paths {
				_ = os.Remove(p)
			}
			return fmt.Errorf("failed to listen on %s: %w", sockPath, err)
		}
		if cfg.Socket.SocketMode != "" {
			if mode, perr := strconv.ParseUint(cfg.Socket.SocketMode, 8, 32); perr == nil {
				_ = os.Chmod(sockPath, os.FileMode(mode))
			}
		}
		packets = append(packets, pc)
		paths = append(paths, sockPath)
		if firstPath == "" {
			firstPath = sockPath
		}
	}

	rt := &socketRuntime{
		unit:      cfg,
		listeners: listeners,
		packets:   packets,
		paths:     paths,
		path:      firstPath,
		active:    true,
		stopCh:    make(chan struct{}),
	}
	if len(listeners) > 0 {
		rt.listener = listeners[0]
	}
	if len(packets) > 0 {
		rt.packet = packets[0]
	}

	m.mu.Lock()
	m.SocketRuntimes[name] = rt
	m.mu.Unlock()

	for _, l := range listeners {
		go m.acceptLoop(name, rt, l)
	}

	return nil
}

func (m *Manager) acceptLoop(socketName string, rt *socketRuntime, l net.Listener) {
	serviceName := strings.TrimSuffix(socketName, ".socket") + ".service"
	for {
		select {
		case <-rt.stopCh:
			return
		default:
		}
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-rt.stopCh:
				return
			default:
				continue
			}
		}
		go func(c net.Conn) {
			defer c.Close()
			unit, err := m.FindUnit(serviceName)
			if err != nil {
				return
			}
			snap := unit.Snapshot()
			if snap.State == service.StateActive || snap.State == service.StateActivating {
				return
			}
			if err := m.startWithSocketActivation(serviceName, socketName, rt); err != nil {
				_ = m.StartUnit(serviceName)
			}
		}(conn)
	}
}

func (m *Manager) startWithSocketActivation(serviceName, socketName string, rt *socketRuntime) error {
	unit, err := m.FindUnit(serviceName)
	if err != nil {
		return err
	}
	var files []*os.File
	for _, l := range rt.listeners {
		if ul, ok := l.(*net.UnixListener); ok {
			if f, err := ul.File(); err == nil {
				files = append(files, f)
			}
		} else if fder, ok := l.(interface{ File() (*os.File, error) }); ok {
			if f, err := fder.File(); err == nil {
				files = append(files, f)
			}
		}
	}
	for _, pc := range rt.packets {
		if fder, ok := interface{}(pc).(interface{ File() (*os.File, error) }); ok {
			if f, err := fder.File(); err == nil {
				files = append(files, f)
			}
		}
	}
	if len(files) > 0 {
		listenEnv := map[string]string{
			"LISTEN_PID": "1",
			"LISTEN_FDS": strconv.Itoa(len(files)),
		}
		unit.SetSocketActivation(files, listenEnv)
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	started := map[string]struct{}{}
	stack := map[string]struct{}{}
	return m.startUnitWithDependencies(serviceName, started, stack)
}

func (m *Manager) stopSocketUnit(name string) error {
	m.mu.Lock()
	rt, ok := m.SocketRuntimes[name]
	if !ok || !rt.active {
		m.mu.Unlock()
		return nil
	}
	close(rt.stopCh)
	for _, l := range rt.listeners {
		_ = l.Close()
	}
	if rt.listener != nil && len(rt.listeners) == 0 {
		_ = rt.listener.Close()
	}
	for _, pc := range rt.packets {
		_ = pc.Close()
	}
	if rt.packet != nil && len(rt.packets) == 0 {
		_ = rt.packet.Close()
	}
	for _, p := range rt.paths {
		_ = os.Remove(p)
	}
	if len(rt.paths) == 0 && rt.path != "" {
		_ = os.Remove(rt.path)
	}
	rt.active = false
	m.mu.Unlock()
	return nil
}

func (m *Manager) IsSocketActive(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, ok := m.SocketRuntimes[name]; ok {
		return rt.active
	}
	return false
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

	// Conflicts: stop conflicting active units before starting
	for _, conflict := range unit.Config.Conflicts {
		conflict = strings.TrimSpace(conflict)
		if conflict == "" {
			continue
		}
		if cu, err := m.FindUnit(conflict); err == nil {
			if snap := cu.Snapshot(); snap.State == service.StateActive || snap.State == service.StateActivating {
				_ = cu.Stop(cu.StopTimeout())
			}
		}
		if _, err := m.FindSocketUnit(conflict); err == nil {
			_ = m.stopSocketUnit(conflict)
		}
	}
	// Also check reverse conflicts: any active unit that conflicts with this one
	m.mu.Lock()
	allNames := make([]string, 0, len(m.Units))
	for n := range m.Units {
		allNames = append(allNames, n)
	}
	m.mu.Unlock()
	for _, otherName := range allNames {
		if otherName == name {
			continue
		}
		other, err := m.FindUnit(otherName)
		if err != nil {
			continue
		}
		for _, c := range other.Config.Conflicts {
			if strings.TrimSpace(c) == name {
				if snap := other.Snapshot(); snap.State == service.StateActive || snap.State == service.StateActivating {
					_ = other.Stop(other.StopTimeout())
				}
			}
		}
	}

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
	// Start enabled socket units
	for _, name := range m.EnabledSocketUnits() {
		if err := m.startSocketUnit(name); err != nil {
			logKernelWarning(fmt.Sprintf("Failed to start enabled socket %s: %v", name, err))
		}
	}
	return nil
}

type dependency struct {
	name     string
	required bool
}

func (m *Manager) collectDependencies(unit *service.Unit) []dependency {
	deps := make([]dependency, 0, len(unit.Config.Requires)+len(unit.Config.Wants)+len(unit.Config.BindsTo))
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
	for _, dep := range unit.Config.BindsTo {
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
	// Stop socket units
	m.mu.Lock()
	sockets := make([]string, 0, len(m.SocketRuntimes))
	for name := range m.SocketRuntimes {
		sockets = append(sockets, name)
	}
	m.mu.Unlock()
	for _, name := range sockets {
		_ = m.stopSocketUnit(name)
	}
}

func (m *Manager) StopUnit(name string) error {
	if strings.HasSuffix(name, ".socket") {
		return m.stopSocketUnit(name)
	}
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	if err := unit.Stop(unit.StopTimeout()); err != nil {
		return err
	}
	// Cascade to PartOf and BindsTo dependents
	m.mu.Lock()
	dependents := []string{}
	for otherName, otherUnit := range m.Units {
		if otherName == name {
			continue
		}
		for _, p := range otherUnit.Config.PartOf {
			if strings.TrimSpace(p) == name {
				dependents = append(dependents, otherName)
				break
			}
		}
		for _, b := range otherUnit.Config.BindsTo {
			if strings.TrimSpace(b) == name {
				found := false
				for _, d := range dependents {
					if d == otherName {
						found = true
						break
					}
				}
				if !found {
					dependents = append(dependents, otherName)
				}
				break
			}
		}
	}
	m.mu.Unlock()
	for _, dep := range dependents {
		if u, err := m.FindUnit(dep); err == nil {
			if snap := u.Snapshot(); snap.State == service.StateActive || snap.State == service.StateActivating {
				_ = u.Stop(u.StopTimeout())
			}
		}
	}
	return nil
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

func (m *Manager) ListAllUnitNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.Units)+len(m.SocketUnits))
	for name := range m.Units {
		names = append(names, name)
	}
	for name := range m.SocketUnits {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) SocketUnitNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.SocketUnits))
	for name := range m.SocketUnits {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) FindSocketUnit(name string) (*parser.Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if unit, ok := m.SocketUnits[name]; ok {
		return unit, nil
	}
	return nil, fmt.Errorf("unit %s not found", name)
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
	// Try service unit first, then socket
	var unitPath string
	var install parser.InstallSection
	var alsoList, aliasList []string
	if unit, err := m.FindUnit(name); err == nil {
		unitPath = unit.Path
		install = unit.Config.Install
		alsoList = unit.Config.Install.Also
		aliasList = unit.Config.Install.Alias
	} else if sock, err := m.FindSocketUnit(name); err == nil {
		if p, ok := m.SocketPaths[name]; ok {
			unitPath = p
		} else {
			unitPath = sock.Name
		}
		install = sock.Install
		alsoList = sock.Install.Also
		aliasList = sock.Install.Alias
	} else {
		return err
	}
	hasWantedBy := len(install.WantedBy) > 0
	hasAlso := len(alsoList) > 0
	hasAlias := len(aliasList) > 0
	if !hasWantedBy && !hasAlso && !hasAlias {
		return errors.New("WantedBy not set")
	}
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	for _, target := range install.WantedBy {
		wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
		if err := os.MkdirAll(wantsDir, 0o755); err != nil {
			return err
		}
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
		if err := os.Symlink(unitPath, linkPath); err != nil {
			return err
		}
	}
	// Handle Also= — enable those units as well
	for _, alsoName := range alsoList {
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
		} else if alsoSock, err := m.FindSocketUnit(alsoName); err == nil {
			for _, target := range alsoSock.Install.WantedBy {
				wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
				_ = os.MkdirAll(wantsDir, 0o755)
				linkPath := filepath.Join(wantsDir, alsoName)
				_ = os.Remove(linkPath)
				if p, ok := m.SocketPaths[alsoName]; ok {
					_ = os.Symlink(p, linkPath)
				}
			}
		}
	}
	// Handle Alias= — create alias symlinks at EnabledRoot
	for _, alias := range aliasList {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		aliasPath := filepath.Join(root, alias)
		_ = os.Remove(aliasPath)
		if err := os.Symlink(unitPath, aliasPath); err != nil {
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
	var unitPath string
	var install parser.InstallSection
	var alsoList, aliasList []string
	if unit, err := m.FindUnit(name); err == nil {
		unitPath = unit.Path
		install = unit.Config.Install
		alsoList = unit.Config.Install.Also
		aliasList = unit.Config.Install.Alias
	} else if sock, err := m.FindSocketUnit(name); err == nil {
		if p, ok := m.SocketPaths[name]; ok {
			unitPath = p
		} else {
			unitPath = sock.Name
		}
		install = sock.Install
		alsoList = sock.Install.Also
		aliasList = sock.Install.Alias
	} else {
		return err
	}
	root := m.EnabledRoot
	if root == "" {
		root = "/etc/systemd/system"
	}
	for _, target := range install.WantedBy {
		wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
	}
	for _, alsoName := range alsoList {
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
		} else if alsoSock, err := m.FindSocketUnit(alsoName); err == nil {
			for _, target := range alsoSock.Install.WantedBy {
				wantsDir := filepath.Join(root, fmt.Sprintf("%s.wants", target))
				linkPath := filepath.Join(wantsDir, alsoName)
				_ = os.Remove(linkPath)
			}
		}
	}
	for _, alias := range aliasList {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		aliasPath := filepath.Join(root, alias)
		if fi, err := os.Lstat(aliasPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(aliasPath); err == nil && target == unitPath {
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
	_, sockOk := m.SocketUnits[name]
	m.mu.Unlock()
	if !ok && !sockOk {
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
	_, sockOk := m.SocketUnits[name]
	m.mu.Unlock()
	if !ok && !sockOk {
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

// UnitState returns the current state of a service or socket unit as a string.
// With an empty name it reports the overall system state: "failed" if any unit
// has failed, otherwise "running".
func (m *Manager) UnitState(name string) (string, error) {
	if name == "" {
		for _, u := range m.ListUnits() {
			if u.Snapshot().State == service.StateFailed {
				return "failed", nil
			}
		}
		return "running", nil
	}
	if unit, err := m.FindUnit(name); err == nil {
		return string(unit.Snapshot().State), nil
	}
	if _, err := m.FindSocketUnit(name); err == nil {
		return m.SocketActiveState(name)
	}
	return "", fmt.Errorf("unit %s not found", name)
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
		"Id":                  cfg.Name,
		"Names":               cfg.Name,
		"Description":         unit.Description(),
		"LoadState":           "loaded",
		"ActiveState":         string(snap.State),
		"SubState":            string(snap.State),
		"FragmentPath":        unit.Path,
		"UnitFileState":       m.UnitFileState(cfg.Name),
		"MainPID":             fmt.Sprintf("%d", snap.MainPID),
		"ExecMainPID":         fmt.Sprintf("%d", snap.MainPID),
		"ExitCode":            fmt.Sprintf("%d", snap.ExitCode),
		"Result":              snap.LastError,
		"Type":                cfg.Service.Type,
		"After":               strings.Join(cfg.After, " "),
		"Before":              strings.Join(cfg.Before, " "),
		"Requires":            strings.Join(cfg.Requires, " "),
		"Wants":               strings.Join(cfg.Wants, " "),
		"Conflicts":           strings.Join(cfg.Conflicts, " "),
		"OnFailure":           strings.Join(cfg.OnFailure, " "),
		"PartOf":              strings.Join(cfg.PartOf, " "),
		"BindsTo":             strings.Join(cfg.BindsTo, " "),
		"DefaultDependencies": cfg.DefaultDependencies,
		"ExecStart":           cfg.Service.ExecStart,
		"WantedBy":            strings.Join(cfg.Install.WantedBy, " "),
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

	units := make([]*service.Unit, 0, len(m.Units)+len(m.SocketUnits))
	for _, unit := range m.Units {
		units = append(units, unit)
	}
	// Add socket units as pseudo service units for listing
	for name, cfg := range m.SocketUnits {
		path := ""
		if p, ok := m.SocketPaths[name]; ok {
			path = p
		}
		units = append(units, service.NewUnit(cfg, path))
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
			if !strings.HasSuffix(name, ".service") && !strings.HasSuffix(name, ".socket") {
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

func (m *Manager) EnabledSocketUnits() []string {
	enabled, err := m.enabledUnitNames()
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []string
	for name := range enabled {
		if strings.HasSuffix(name, ".socket") {
			if _, ok := m.SocketUnits[name]; ok {
				result = append(result, name)
			}
		}
	}
	sort.Strings(result)
	return result
}

func (m *Manager) ShowSocketUnit(name string) (map[string]string, error) {
	m.mu.Lock()
	cfg, ok := m.SocketUnits[name]
	path := m.SocketPaths[name]
	rt, hasRt := m.SocketRuntimes[name]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unit %s not found", name)
	}
	activeState := "inactive"
	if hasRt && rt.active {
		activeState = "active"
	}
	data := map[string]string{
		"Id":            cfg.Name,
		"Names":         cfg.Name,
		"Description":   cfg.Description,
		"LoadState":     "loaded",
		"ActiveState":   activeState,
		"SubState":      activeState,
		"FragmentPath":  path,
		"UnitFileState": m.UnitFileState(cfg.Name),
		"Type":          "socket",
		"ListenStream":  strings.Join(cfg.Socket.ListenStream, " "),
		"ListenDatagram": strings.Join(cfg.Socket.ListenDatagram, " "),
		"WantedBy":      strings.Join(cfg.Install.WantedBy, " "),
	}
	return data, nil
}

func (m *Manager) CatSocketUnit(name string) (string, error) {
	m.mu.Lock()
	path := m.SocketPaths[name]
	_, hasUnit := m.SocketUnits[name]
	m.mu.Unlock()
	if !hasUnit {
		return "", fmt.Errorf("unit %s not found", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (m *Manager) SocketActiveState(name string) (string, error) {
	m.mu.Lock()
	_, ok := m.SocketUnits[name]
	rt, hasRt := m.SocketRuntimes[name]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unit %s not found", name)
	}
	if hasRt && rt.active {
		return "active", nil
	}
	return "inactive", nil
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
		for _, dep := range unit.Config.Before {
			if strings.HasSuffix(dep, ".target") || !strings.HasSuffix(dep, ".service") {
				continue
			}
			if _, ok := nameToUnit[dep]; !ok || dep == unit.Config.Name {
				continue
			}
			adj[unit.Config.Name] = append(adj[unit.Config.Name], dep)
			indegree[dep]++
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
	clone.Before = expandSlice(clone.Before, instanceName, instance)
	clone.Requires = expandSlice(clone.Requires, instanceName, instance)
	clone.Wants = expandSlice(clone.Wants, instanceName, instance)
	clone.Conflicts = expandSlice(clone.Conflicts, instanceName, instance)
	clone.OnFailure = expandSlice(clone.OnFailure, instanceName, instance)
	clone.PartOf = expandSlice(clone.PartOf, instanceName, instance)
	clone.BindsTo = expandSlice(clone.BindsTo, instanceName, instance)
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
