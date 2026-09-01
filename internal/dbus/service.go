package dbus

import (
	"context"
	"fmt"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	"initd/internal/service"
	"initd/internal/supervisor"
)

const (
	managerPathString  = "/org/freedesktop/systemd1"
	managerBusName     = "org.freedesktop.systemd1"
	managerInterface   = "org.freedesktop.systemd1.Manager"
	unitInterface      = "org.freedesktop.systemd1.Unit"
	unitBasePathString = "/org/freedesktop/systemd1/unit"
)

var managerPath = dbus.ObjectPath(managerPathString)

// systemd1Manager is the exported object at /org/freedesktop/systemd1
// implementing org.freedesktop.systemd1.Manager plus org.freedesktop.DBus.Properties.
type systemd1Manager struct {
	userMgr   *supervisor.Manager
	systemMgr *supervisor.Manager
}

// newManager creates the exported manager object.
func newManager(userMgr, systemMgr *supervisor.Manager) *systemd1Manager {
	return &systemd1Manager{userMgr: userMgr, systemMgr: systemMgr}
}

// frontend returns the primary manager for this connection: system manager for a
// system bus registration, user manager for a user bus.
func (m *systemd1Manager) frontend() *supervisor.Manager {
	if m.systemMgr != nil {
		return m.systemMgr
	}
	return m.userMgr
}

// lookupManager returns the manager that actually holds a given unit
// (system first when both are configured, else the frontend user manager).
func (m *systemd1Manager) lookupManager(name string) *supervisor.Manager {
	if m.systemMgr != nil {
		if _, ok := managerUnitProps(m.systemMgr, name); ok {
			return m.systemMgr
		}
	}
	return m.userMgr
}

func managerUnitProps(mgr *supervisor.Manager, name string) (map[string]string, bool) {
	if mgr == nil {
		return nil, false
	}
	if data, err := mgr.ShowSocketUnit(name); err == nil {
		return data, true
	}
	if data, err := mgr.ShowUnit(name); err == nil {
		return data, true
	}
	return nil, false
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found")
}

// unitObjectPath converts a unit name to its D-Bus object path. systemd
// escapes the unit name: '.' -> '_2e', '-' -> '_2d', '@' -> '_40' (and other
// chars as _XX hex). We replicate the documented subset.
func unitObjectPath(name string) dbus.ObjectPath {
	return dbus.ObjectPath(fmt.Sprintf("%s/%s", unitBasePathString, dbusEscape(name)))
}

func dbusEscape(name string) string {
	repl := strings.NewReplacer(".", "_2e", "-", "_2d", "@", "_40")
	return repl.Replace(name)
}

func dbusUnescape(escaped string) string {
	repl := strings.NewReplacer("_2e", ".", "_2d", "-", "_40", "@")
	return repl.Replace(escaped)
}

func unitNameFromPath(path string) (string, bool) {
	prefix := unitBasePathString + "/"
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || rest == path {
		return "", false
	}
	return dbusUnescape(rest), true
}

// unitPathFor returns the canonical D-Bus object path for a unit on this manager.
func (m *systemd1Manager) unitPathFor(name string) dbus.ObjectPath {
	return unitObjectPath(name)
}

// ---------- Manager methods ----------
// The method set below is exported as interface "org.freedesktop.systemd1.Manager"
// on object path /org/freedesktop/systemd1. godbus reflects method names to
// D-Bus members and marshals the Go signatures, so they must be stable:
//   - GetUnit(s) -> o
//   - StartUnit(ss) -> o  (mode param omitted to keep signature simple; systemd's
//     StartUnit has an extra "replace" flag in newer versions, but the
//     openclaw/systemctl usage only sends the name and mode.)

func (m *systemd1Manager) GetUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	if name == "" {
		return "", dbus.MakeFailedError(fmt.Errorf("No unit name specified"))
	}
	mgr := m.lookupManager(name)
	if _, ok := managerUnitProps(mgr, name); ok {
		return m.unitPathFor(name), nil
	}
	return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
}

func (m *systemd1Manager) GetUnitByPID(pid uint32) (dbus.ObjectPath, *dbus.Error) {
	_ = pid
	return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"Not tracked"}}
}

func (m *systemd1Manager) GetUnitByControlGroup(cgroup string) (dbus.ObjectPath, *dbus.Error) {
	_ = cgroup
	return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"Not tracked"}}
}

func (m *systemd1Manager) GetUnitFileState(file string) (string, *dbus.Error) {
	if file == "" {
		return "", dbus.MakeFailedError(fmt.Errorf("empty unit file name"))
	}
	state := m.userMgr.UnitFileState(file)
	if m.systemMgr != nil && state == "disabled" {
		if s := m.systemMgr.UnitFileState(file); s != "disabled" {
			state = s
		}
	}
	return state, nil
}

func (m *systemd1Manager) EnableUnitFiles(files []string, runtime bool, force bool) (bool, []struct {
	Type, Filename, Destination string
}, *dbus.Error) {
	_ = runtime
	_ = force
	changed := false
	infos := []struct {
		Type, Filename, Destination string
	}{}
	for _, f := range files {
		if err := m.userMgr.EnableUnit(f); err == nil {
			changed = true
			infos = append(infos, struct {
				Type, Filename, Destination string
			}{Type: "symlink", Filename: f, Destination: m.userMgr.EnabledRoot})
		}
	}
	return changed, infos, nil
}

func (m *systemd1Manager) DisableUnitFiles(files []string, runtime bool) (bool, []struct {
	Type, Filename, Destination string
}, *dbus.Error) {
	_ = runtime
	changed := false
	infos := []struct {
		Type, Filename, Destination string
	}{}
	for _, f := range files {
		if err := m.userMgr.DisableUnit(f); err == nil {
			changed = true
			infos = append(infos, struct {
				Type, Filename, Destination string
			}{Type: "unlink", Filename: f, Destination: m.userMgr.EnabledRoot})
		}
	}
	return changed, infos, nil
}

func (m *systemd1Manager) RestartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.lookupManager(name)
	if err := mgr.RestartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) StopUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.lookupManager(name)
	if err := mgr.StopUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) StartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.lookupManager(name)
	if err := mgr.StartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) ReloadUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.lookupManager(name)
	if err := mgr.ReloadUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) ReloadOrRestartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	return m.RestartUnit(name, mode)
}

func (m *systemd1Manager) ReloadOrTryRestartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	return m.RestartUnit(name, mode)
}

func (m *systemd1Manager) TryRestartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.lookupManager(name)
	if err := mgr.RestartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) KillUnit(name string, whom string, signal int32) *dbus.Error {
	mgr := m.lookupManager(name)
	if err := mgr.KillUnit(name, fmt.Sprintf("%d", signal)); err != nil && !isNotFoundErr(err) {
		return dbus.MakeFailedError(err)
	}
	return nil
}

func (m *systemd1Manager) ResetFailedUnit(name string) *dbus.Error {
	if err := m.frontend().ResetFailed(name); err != nil && !isNotFoundErr(err) {
		return dbus.MakeFailedError(err)
	}
	return nil
}

// ListUnits matches systemd's a(st) return shape for the unit listing.
type listUnitEntry struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Followed    string
	Path        dbus.ObjectPath
	JobId       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

func (m *systemd1Manager) ListUnits() ([]listUnitEntry, *dbus.Error) {
	units := m.frontend().ListUnits()
	result := make([]listUnitEntry, 0, len(units))
	for _, u := range units {
		snap := u.Snapshot()
		desc := u.Description()
		data, _ := managerUnitProps(m.frontendSafe(), u.Config.Name)
		if data != nil {
			if d, ok := data["Description"]; ok && d != "" {
				desc = d
			}
		}
		result = append(result, listUnitEntry{
			Name:        u.Config.Name,
			Description: desc,
			LoadState:   "loaded",
			ActiveState: string(snap.State),
			SubState:    string(snap.State),
			Followed:    "",
			Path:        m.unitPathFor(u.Config.Name),
			JobId:       0,
			JobType:     "",
			JobPath:     "/",
		})
	}
	return result, nil
}

func (m *systemd1Manager) ListUnitsFiltered(states []string) ([]listUnitEntry, *dbus.Error) {
	_ = states
	return m.ListUnits()
}

func (m *systemd1Manager) ListUnitsByNames(names []string) ([]listUnitEntry, *dbus.Error) {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	all, _ := m.ListUnits()
	result := make([]listUnitEntry, 0, len(all))
	for _, u := range all {
		if wanted[u.Name] {
			result = append(result, u)
		}
	}
	return result, nil
}

func (m *systemd1Manager) Reload() *dbus.Error {
	_ = m.frontend().Reload()
	return nil
}

// ---------- Unit (subtree) methods ----------
// The unit objects live at /org/freedesktop/systemd1/unit/<escaped>. We register
// a subtree handler so that calls on any such path are routed here; the unit
// name is recovered from the object path in the message.

type systemd1Unit struct {
	mgr  *supervisor.Manager
	name string
}

func (u *systemd1Unit) Start(mode string) (dbus.ObjectPath, *dbus.Error) {
	if err := u.mgr.StartUnit(u.name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return unitObjectPath(u.name), nil
}

func (u *systemd1Unit) Stop(mode string) (dbus.ObjectPath, *dbus.Error) {
	if err := u.mgr.StopUnit(u.name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return unitObjectPath(u.name), nil
}

func (u *systemd1Unit) Restart(mode string) (dbus.ObjectPath, *dbus.Error) {
	if err := u.mgr.RestartUnit(u.name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return unitObjectPath(u.name), nil
}

func (u *systemd1Unit) Reload(mode string) (dbus.ObjectPath, *dbus.Error) {
	if err := u.mgr.ReloadUnit(u.name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return unitObjectPath(u.name), nil
}

func (u *systemd1Unit) TryRestart(mode string) (dbus.ObjectPath, *dbus.Error) {
	return u.Restart(mode)
}

func (u *systemd1Unit) ReloadOrRestart(mode string) (dbus.ObjectPath, *dbus.Error) {
	return u.Restart(mode)
}

func (u *systemd1Unit) ReloadOrTryRestart(mode string) (dbus.ObjectPath, *dbus.Error) {
	return u.Restart(mode)
}

func (u *systemd1Unit) Kill(whom string, signal int32) *dbus.Error {
	if err := u.mgr.KillUnit(u.name, fmt.Sprintf("%d", signal)); err != nil && !isNotFoundErr(err) {
		return dbus.MakeFailedError(err)
	}
	return nil
}

func (u *systemd1Unit) ResetFailed() *dbus.Error {
	_ = u.mgr.ResetFailed(u.name)
	return nil
}

// unitSubtreeHandler is exported at /org/freedesktop/systemd1/unit and handles
// method calls whose object path is a unit child. It recovers the unit name
// from the message path and dispatches to a per-unit systemd1Unit.
type unitSubtreeHandler struct {
	mgr *supervisor.Manager
}

func (h *unitSubtreeHandler) unitObj(msg dbus.Message) (*systemd1Unit, *dbus.Error) {
	p, ok := msg.Headers[dbus.FieldPath]
	if !ok {
		return nil, dbus.MakeFailedError(fmt.Errorf("missing object path"))
	}
	path, ok := p.Value().(dbus.ObjectPath)
	if !ok || !path.IsValid() {
		return nil, dbus.MakeFailedError(fmt.Errorf("invalid object path"))
	}
	name, found := unitNameFromPath(string(path))
	if !found {
		return nil, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"No such unit"}}
	}
	return &systemd1Unit{mgr: h.mgr, name: name}, nil
}

func (h *unitSubtreeHandler) Start(msg dbus.Message, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, derr := h.unitObj(msg)
	if derr != nil {
		return "", derr
	}
	return u.Start(mode)
}

func (h *unitSubtreeHandler) Stop(msg dbus.Message, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, derr := h.unitObj(msg)
	if derr != nil {
		return "", derr
	}
	return u.Stop(mode)
}

func (h *unitSubtreeHandler) Restart(msg dbus.Message, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, derr := h.unitObj(msg)
	if derr != nil {
		return "", derr
	}
	return u.Restart(mode)
}

func (h *unitSubtreeHandler) Reload(msg dbus.Message, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, derr := h.unitObj(msg)
	if derr != nil {
		return "", derr
	}
	return u.Reload(mode)
}

func (h *unitSubtreeHandler) Kill(msg dbus.Message, whom string, signal int32) *dbus.Error {
	u, derr := h.unitObj(msg)
	if derr != nil {
		return derr
	}
	u.Kill(whom, signal)
	return nil
}

func (h *unitSubtreeHandler) ResetFailed(msg dbus.Message) *dbus.Error {
	return nil
}

// unitPropsSubtree handles org.freedesktop.DBus.Properties on unit subtree paths.
type unitPropsSubtree struct {
	mgr *supervisor.Manager
}

func (u *unitPropsSubtree) unitNameFromPath(msg dbus.Message) (string, *dbus.Error) {
	p, ok := msg.Headers[dbus.FieldPath]
	if !ok {
		return "", dbus.MakeFailedError(fmt.Errorf("missing object path"))
	}
	path, ok := p.Value().(dbus.ObjectPath)
	if !ok || !path.IsValid() {
		return "", dbus.MakeFailedError(fmt.Errorf("invalid object path"))
	}
	name, found := unitNameFromPath(string(path))
	if !found {
		return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"No such unit"}}
	}
	return name, nil
}

func (u *unitPropsSubtree) Get(iface string, propName string, msg dbus.Message) (dbus.Variant, *dbus.Error) {
	_ = iface
	name, derr := u.unitNameFromPath(msg)
	if derr != nil {
		return dbus.Variant{}, derr
	}
	props := buildUnitProps(u.mgr, name)
	if props == nil {
		return dbus.Variant{}, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
	}
	p, ok := props[propName]
	if !ok {
		return dbus.Variant{}, &dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownProperty", Body: []interface{}{fmt.Sprintf("Unknown property %s", propName)}}
	}
	return dbus.MakeVariant(p.Value), nil
}

func (u *unitPropsSubtree) GetAll(iface string, msg dbus.Message) (map[string]dbus.Variant, *dbus.Error) {
	_ = iface
	name, derr := u.unitNameFromPath(msg)
	if derr != nil {
		return nil, derr
	}
	props := buildUnitProps(u.mgr, name)
	if props == nil {
		return nil, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
	}
	out := make(map[string]dbus.Variant, len(props))
	for k, p := range props {
		out[k] = dbus.MakeVariant(p.Value)
	}
	return out, nil
}

func (u *unitPropsSubtree) Set(iface string, propName string, val dbus.Variant, msg dbus.Message) *dbus.Error {
	_ = iface
	_ = propName
	_ = val
	_ = msg
	return &dbus.Error{Name: "org.freedesktop.DBus.Error.PropertyReadOnly", Body: []interface{}{"Property is read-only"}}
}

// ---------- helpers for Properties ----------

func buildManagerProps(mgr *supervisor.Manager, systemMgr *supervisor.Manager) map[string]map[string]*prop.Prop {
	frontend := mgr
	if frontend == nil {
		frontend = systemMgr
	}
	sysState := "running"
	if frontend != nil {
		sysState = frontend.SystemState()
	}
	mgrIf := map[string]*prop.Prop{
		"Version":                {Value: "1.0.0 (initd)", Writable: false, Emit: prop.EmitConst},
		"Features":               {Value: "", Writable: false, Emit: prop.EmitConst},
		"Virtualization":         {Value: "", Writable: false, Emit: prop.EmitConst},
		"ConfidentialVirtualization": {Value: "", Writable: false, Emit: prop.EmitConst},
		"Architecture":           {Value: "aarch64", Writable: false, Emit: prop.EmitConst},
		"Tainted":                {Value: "", Writable: false, Emit: prop.EmitConst},
		"SystemState":            {Value: sysState, Writable: false, Emit: prop.EmitTrue},
		"FirmwareTimestamp":      {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FirmwareTimestampMonotonic": {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"InitRDTimestamp":        {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"InitRDTimestampMonotonic": {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"UserspaceTimestamp":     {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"UserspaceTimestampMonotonic": {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FinishTimestamp":        {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FinishTimestampMonotonic": {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"LogLevel":               {Value: "info", Writable: false, Emit: prop.EmitTrue},
		"LogTarget":              {Value: "journal", Writable: false, Emit: prop.EmitTrue},
		"NNames":                 {Value: func() uint32 { if frontend == nil { return 0 }; return uint32(len(frontend.ListAllUnitNames())) }(), Writable: false, Emit: prop.EmitTrue},
		"NFailedUnits":           {Value: func() uint32 { if frontend == nil { return 0 }; c := uint32(0); for _, u := range frontend.ListUnits() { if u.Snapshot().State == service.StateFailed { c++ } }; return c }(), Writable: false, Emit: prop.EmitTrue},
		"NJobs":                  {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NUnique":                {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NInstalledJobs":         {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NFailedJobs":            {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"Progress":               {Value: float64(0), Writable: false, Emit: prop.EmitTrue},
		"Environment":            {Value: []string{}, Writable: false, Emit: prop.EmitTrue},
		"ConfirmSpawn":           {Value: false, Writable: false, Emit: prop.EmitTrue},
		"ShowStatus":             {Value: false, Writable: false, Emit: prop.EmitTrue},
		"UnitPath":               {Value: func() []string { if frontend == nil { return []string{} }; return frontend.SearchPaths }(), Writable: false, Emit: prop.EmitConst},
		"DefaultStandardOutput":  {Value: "journal", Writable: false, Emit: prop.EmitConst},
		"DefaultStandardError":   {Value: "inherit", Writable: false, Emit: prop.EmitConst},
		"RuntimeWatchdogUSec":    {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"RebootWatchdogUSec":     {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"KExecWatchdogUSec":      {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"ServiceWatchdogs":       {Value: false, Writable: false, Emit: prop.EmitConst},
		"ControlGroup":           {Value: "", Writable: false, Emit: prop.EmitConst},
		"ExitCode":               {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
	}
	return map[string]map[string]*prop.Prop{managerInterface: mgrIf}
}

func buildUnitProps(mgr *supervisor.Manager, name string) map[string]*prop.Prop {
	data, ok := managerUnitProps(mgr, name)
	if !ok {
		return nil
	}

	activeState := data["ActiveState"]
	subState := data["SubState"]
	loadState := data["LoadState"]
	if loadState == "" {
		loadState = "loaded"
	}
	id := data["Id"]
	if id == "" {
		id = name
	}
	desc := data["Description"]
	if desc == "" {
		desc = id
	}

	return map[string]*prop.Prop{
		"Id":                   {Value: id, Writable: false, Emit: prop.EmitConst},
		"Names":                {Value: []string{id}, Writable: false, Emit: prop.EmitConst},
		"Description":          {Value: desc, Writable: false, Emit: prop.EmitConst},
		"LoadState":            {Value: loadState, Writable: false, Emit: prop.EmitTrue},
		"ActiveState":          {Value: activeState, Writable: false, Emit: prop.EmitTrue},
		"SubState":             {Value: subState, Writable: false, Emit: prop.EmitTrue},
		"UnitFileState":        {Value: mgr.UnitFileState(id), Writable: false, Emit: prop.EmitConst},
		"UnitFilePreset":       {Value: "disabled", Writable: false, Emit: prop.EmitConst},
		"Result":               {Value: "", Writable: false, Emit: prop.EmitTrue},
		"FragmentPath":         {Value: data["FragmentPath"], Writable: false, Emit: prop.EmitConst},
		"SourcePath":           {Value: "", Writable: false, Emit: prop.EmitConst},
		"MainPID":              {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainPID":          {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainStatus":       {Value: int32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainCode":         {Value: int32(0), Writable: false, Emit: prop.EmitTrue},
		"ExitCode":             {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
		"ExitStatus":           {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
		"NRestarts":            {Value: uint32(0), Writable: false, Emit: prop.EmitConst},
		"StartLimitBurst":      {Value: uint32(5), Writable: false, Emit: prop.EmitConst},
		"StartLimitIntervalUSec": {Value: uint64(10e6), Writable: false, Emit: prop.EmitConst},
		"StartLimitAction":     {Value: "none", Writable: false, Emit: prop.EmitConst},
		"MemoryCurrent":        {Value: uint64(18446744073709551615), Writable: false, Emit: prop.EmitTrue},
		"CPUWeight":            {Value: uint64(100), Writable: false, Emit: prop.EmitTrue},
		"TasksCurrent":         {Value: uint64(18446744073709551615), Writable: false, Emit: prop.EmitTrue},
	}
}

// ---------- public registration API ----------

// ServeUserBus connects to the session bus, requests org.freedesktop.systemd1,
// and exports the Manager interface at /org/freedesktop/systemd1 with per-unit
// subtrees. The connection is kept open for the lifetime of the returned conn.
func ServeUserBus(ctx context.Context, mgr *supervisor.Manager) (*dbus.Conn, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("dbus session bus: %w", err)
	}
	if mgr != nil {
		if err := serveManager(ctx, conn, mgr, nil, "user"); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// ServeSystemBus connects to the system bus. initd owns org.freedesktop.systemd1
// on the system bus so that tools like /usr/bin/systemctl (system scope) get
// verifiable answers instead of "Failed to connect to bus: Permission denied".
func ServeSystemBus(ctx context.Context, systemMgr, userMgr *supervisor.Manager) (*dbus.Conn, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("dbus system bus: %w", err)
	}
	if systemMgr != nil {
		if err := serveManager(ctx, conn, systemMgr, userMgr, "system"); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func serveManager(ctx context.Context, conn *dbus.Conn, frontend *supervisor.Manager, secondary *supervisor.Manager, scope string) error {
	_ = ctx

	mgrObj := newManager(secondary, frontend)

	// Export Properties (org.freedesktop.DBus.Properties) + Manager methods on the
	// manager object path at /org/freedesktop/systemd1. The manager interface is
	// primary; Properties is served alongside it on the same (path, interface).
	if _, err := prop.Export(conn, managerPath, buildManagerProps(frontend, secondary)); err != nil {
		return fmt.Errorf("export manager properties: %w", err)
	}

	// Manager methods.
	if err := conn.Export(mgrObj, managerPath, managerInterface); err != nil {
		return fmt.Errorf("export manager interface: %w", err)
	}

	// Unit subtree: any call on /.../unit/<name> goes here.
	handler := &unitSubtreeHandler{mgr: frontend}
	if err := conn.ExportSubtree(handler, dbus.ObjectPath(unitBasePathString), unitInterface); err != nil {
		return fmt.Errorf("export unit subtree: %w", err)
	}

	// Properties on the unit subtree.
	propsHandler := &unitPropsSubtree{mgr: frontend}
	if err := conn.ExportSubtree(propsHandler, dbus.ObjectPath(unitBasePathString), "org.freedesktop.DBus.Properties"); err != nil {
		return fmt.Errorf("export unit properties subtree: %w", err)
	}

	// Acquire the bus name. ReplaceExisting so restarts work; DoNotQueue so we
	// fail fast against a stale owner.
	reply, err := conn.RequestName(managerBusName, dbus.NameFlagReplaceExisting|dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request name %s: %w", managerBusName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner && reply != dbus.RequestNameReplyAlreadyOwner {
		return fmt.Errorf("request name %s: reply=%v", managerBusName, reply)
	}

	return nil
}

// frontendSafe returns the frontend manager with a nil guard, used by ListUnits
// and property builders which must not dereference a nil manager.
func (m *systemd1Manager) frontendSafe() *supervisor.Manager {
	if m.systemMgr != nil {
		return m.systemMgr
	}
	return m.userMgr
}
