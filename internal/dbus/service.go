package dbus

import (
	"context"
	"fmt"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	sservice "initd/internal/service"
	"initd/internal/supervisor"
)

const (
	managerPathString  = "/org/freedesktop/systemd1"
	managerBusName     = "org.freedesktop.systemd1"
	managerInterface   = "org.freedesktop.systemd1.Manager"
	unitInterface      = "org.freedesktop.systemd1.Unit"
	serviceInterface   = "org.freedesktop.systemd1.Service"
	propertiesInterface = "org.freedesktop.DBus.Properties"
	unitBasePathString = "/org/freedesktop/systemd1/unit"
)

var managerPath = dbus.ObjectPath(managerPathString)

// emptyManager is a nil-safe fallback used when a bus connection has no
// backing supervisor Manager (e.g. initd running as a non-root user has no
// system manager). Query methods on it return "not found"/empty results so the
// D-Bus service can still own org.freedesktop.systemd1 and answer probes
// gracefully instead of erroring.
var emptyManager = &supervisor.Manager{}

// systemd1Manager is the exported object at /org/freedesktop/systemd1
// implementing org.freedesktop.systemd1.Manager (and org.freedesktop.DBus.Properties).
// Each bus connection registers its own instance bound to the primary supervisor
// Manager for that scope (the user manager on the session bus, the system
// manager on the system bus). A nil primary is valid: queries answer as
// not-found rather than erroring, which is what openclaw's ownership probe needs
// from the system bus.
type systemd1Manager struct {
	primary *supervisor.Manager
}

func newManager(primary *supervisor.Manager) *systemd1Manager {
	return &systemd1Manager{primary: primary}
}

func (m *systemd1Manager) primarySafe() *supervisor.Manager {
	if m.primary != nil {
		return m.primary
	}
	return emptyManager
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

func (m *systemd1Manager) unitPathFor(name string) dbus.ObjectPath {
	return unitObjectPath(name)
}

// ---------- Manager interface methods ----------

func (m *systemd1Manager) GetUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	if name == "" {
		return "", dbus.MakeFailedError(fmt.Errorf("No unit name specified"))
	}
	if _, ok := managerUnitProps(m.primary, name); ok {
		return m.unitPathFor(name), nil
	}
	return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
}

// LoadUnit mirrors org.freedesktop.systemd1.Manager.LoadUnit. systemd returns
// the loaded unit's object path; for a unit that is not loaded it raises
// org.freedesktop.systemd1.NoSuchUnit with the body "Unit %s not found.". That
// exact body is what busctl surfaces on stderr as
//   "Call failed: Unit <name> not found."
// and callers such as openclaw match it verbatim to treat the unit as absent.
// Do not change the message text or the error name without updating that
// match.
func (m *systemd1Manager) LoadUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	if name == "" {
		return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"Unit  not found."}}
	}
	if _, ok := managerUnitProps(m.primary, name); ok {
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
	return m.primarySafe().UnitFileState(file), nil
}

func (m *systemd1Manager) EnableUnitFiles(files []string, runtime bool, force bool) (bool, []struct {
	Type, Filename, Destination string
}, *dbus.Error) {
	_ = runtime
	_ = force
	mgr := m.primarySafe()
	changed := false
	infos := []struct {
		Type, Filename, Destination string
	}{}
	for _, f := range files {
		if err := mgr.EnableUnit(f); err == nil {
			changed = true
			infos = append(infos, struct {
				Type, Filename, Destination string
			}{Type: "symlink", Filename: f, Destination: mgr.EnabledRoot})
		}
	}
	return changed, infos, nil
}

func (m *systemd1Manager) DisableUnitFiles(files []string, runtime bool) (bool, []struct {
	Type, Filename, Destination string
}, *dbus.Error) {
	_ = runtime
	mgr := m.primarySafe()
	changed := false
	infos := []struct {
		Type, Filename, Destination string
	}{}
	for _, f := range files {
		if err := mgr.DisableUnit(f); err == nil {
			changed = true
			infos = append(infos, struct {
				Type, Filename, Destination string
			}{Type: "unlink", Filename: f, Destination: mgr.EnabledRoot})
		}
	}
	return changed, infos, nil
}

func (m *systemd1Manager) RestartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.primarySafe()
	if err := mgr.RestartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) StopUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.primarySafe()
	if err := mgr.StopUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) StartUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.primarySafe()
	if err := mgr.StartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) ReloadUnit(name string, mode string) (dbus.ObjectPath, *dbus.Error) {
	mgr := m.primarySafe()
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
	mgr := m.primarySafe()
	if err := mgr.RestartUnit(name); err != nil && !isNotFoundErr(err) {
		return "", dbus.MakeFailedError(err)
	}
	return m.unitPathFor(name), nil
}

func (m *systemd1Manager) KillUnit(name string, whom string, signal int32) *dbus.Error {
	mgr := m.primarySafe()
	if err := mgr.KillUnit(name, fmt.Sprintf("%d", signal)); err != nil && !isNotFoundErr(err) {
		return dbus.MakeFailedError(err)
	}
	return nil
}

func (m *systemd1Manager) ResetFailedUnit(name string) *dbus.Error {
	mgr := m.primarySafe()
	if err := mgr.ResetFailed(name); err != nil && !isNotFoundErr(err) {
		return dbus.MakeFailedError(err)
	}
	return nil
}

// listUnitEntry matches systemd's a(st) unit-listing return shape.
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
	mgr := m.primarySafe()
	units := mgr.ListUnits()
	result := make([]listUnitEntry, 0, len(units))
	for _, u := range units {
		snap := u.Snapshot()
		desc := u.Description()
		data, _ := managerUnitProps(mgr, u.Config.Name)
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
	_ = m.primarySafe().Reload()
	return nil
}

// ---------- Unit (subtree) methods ----------
// Unit objects live at /org/freedesktop/systemd1/unit/<escaped> and are served
// via a subtree handler so calls on any such path are routed here; the unit name
// is recovered from the message's object path.

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
	return u.Reload(mode)
}

func (u *systemd1Unit) ReloadOrTryRestart(mode string) (dbus.ObjectPath, *dbus.Error) {
	return u.Reload(mode)
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

type unitSubtreeHandler struct {
	mgr *supervisor.Manager
}

func (h *unitSubtreeHandler) unitObj(msg dbus.Message) (*systemd1Unit, *dbus.Error) {
	path, ok := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	if !ok || !path.IsValid() {
		return nil, dbus.MakeFailedError(fmt.Errorf("missing object path"))
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

// unitPropsSubtree serves org.freedesktop.DBus.Properties on unit subtree paths.
type unitPropsSubtree struct {
	mgr *supervisor.Manager
}

func (u *unitPropsSubtree) unitNameFromPath(msg dbus.Message) (string, *dbus.Error) {
	path, ok := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	if !ok || !path.IsValid() {
		return "", dbus.MakeFailedError(fmt.Errorf("missing object path"))
	}
	name, found := unitNameFromPath(string(path))
	if !found {
		return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{"No such unit"}}
	}
	return name, nil
}

func (u *unitPropsSubtree) Get(iface string, propName string, msg dbus.Message) (dbus.Variant, *dbus.Error) {
	name, derr := u.unitNameFromPath(msg)
	if derr != nil {
		return dbus.Variant{}, derr
	}
	switch iface {
	case serviceInterface:
		props := buildServiceProps(u.mgr, name)
		if props == nil {
			return dbus.Variant{}, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
		}
		p, ok := props[propName]
		if !ok {
			return dbus.Variant{}, &dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownProperty", Body: []interface{}{fmt.Sprintf("Unknown property %s", propName)}}
		}
		return dbus.MakeVariant(p.Value), nil
	default:
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
}

func (u *unitPropsSubtree) GetAll(iface string, msg dbus.Message) (map[string]dbus.Variant, *dbus.Error) {
	name, derr := u.unitNameFromPath(msg)
	if derr != nil {
		return nil, derr
	}
	switch iface {
	case serviceInterface:
		props := buildServiceProps(u.mgr, name)
		if props == nil {
			return nil, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", name)}}
		}
		out := make(map[string]dbus.Variant, len(props))
		for k, p := range props {
			out[k] = dbus.MakeVariant(p.Value)
		}
		return out, nil
	default:
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
}

func (u *unitPropsSubtree) Set(iface string, propName string, val dbus.Variant, msg dbus.Message) *dbus.Error {
	_ = iface
	_ = propName
	_ = val
	_ = msg
	return &dbus.Error{Name: "org.freedesktop.DBus.Error.PropertyReadOnly", Body: []interface{}{"Property is read-only"}}
}

// ---------- property builders ----------

func buildManagerProps(mgr *supervisor.Manager) map[string]map[string]*prop.Prop {
	frontend := mgr
	if frontend == nil {
		frontend = emptyManager
	}
	sysState := frontend.SystemState()
	mgrIf := map[string]*prop.Prop{
		"Version":                     {Value: "1.0.1 (initd)", Writable: false, Emit: prop.EmitConst},
		"Features":                    {Value: "", Writable: false, Emit: prop.EmitConst},
		"Virtualization":              {Value: "", Writable: false, Emit: prop.EmitConst},
		"ConfidentialVirtualization":    {Value: "", Writable: false, Emit: prop.EmitConst},
		"Architecture":                  {Value: "aarch64", Writable: false, Emit: prop.EmitConst},
		"Tainted":                       {Value: "", Writable: false, Emit: prop.EmitConst},
		"SystemState":                   {Value: sysState, Writable: false, Emit: prop.EmitTrue},
		"FirmwareTimestamp":             {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FirmwareTimestampMonotonic":    {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"InitRDTimestamp":               {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"InitRDTimestampMonotonic":      {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"UserspaceTimestamp":            {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"UserspaceTimestampMonotonic":   {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FinishTimestamp":               {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"FinishTimestampMonotonic":      {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"LogLevel":                      {Value: "info", Writable: false, Emit: prop.EmitTrue},
		"LogTarget":                     {Value: "journal", Writable: false, Emit: prop.EmitTrue},
		"NNames":                        {Value: uint32(len(frontend.ListAllUnitNames())), Writable: false, Emit: prop.EmitTrue},
		"NFailedUnits":                  {Value: countFailed(frontend), Writable: false, Emit: prop.EmitTrue},
		"NJobs":                         {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NUnique":                       {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NInstalledJobs":                {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"NFailedJobs":                   {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"Progress":                      {Value: float64(0), Writable: false, Emit: prop.EmitTrue},
		"Environment":                   {Value: []string{}, Writable: false, Emit: prop.EmitTrue},
		"ConfirmSpawn":                  {Value: false, Writable: false, Emit: prop.EmitTrue},
		"ShowStatus":                    {Value: false, Writable: false, Emit: prop.EmitTrue},
		"UnitPath":                      {Value: frontend.SearchPaths, Writable: false, Emit: prop.EmitConst},
		"DefaultStandardOutput":         {Value: "journal", Writable: false, Emit: prop.EmitConst},
		"DefaultStandardError":          {Value: "inherit", Writable: false, Emit: prop.EmitConst},
		"RuntimeWatchdogUSec":           {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"RebootWatchdogUSec":            {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"KExecWatchdogUSec":             {Value: uint64(0), Writable: false, Emit: prop.EmitConst},
		"ServiceWatchdogs":              {Value: false, Writable: false, Emit: prop.EmitConst},
		"ControlGroup":                  {Value: "", Writable: false, Emit: prop.EmitConst},
		"ExitCode":                      {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
	}
	return map[string]map[string]*prop.Prop{managerInterface: mgrIf}
}

func countFailed(mgr *supervisor.Manager) uint32 {
	var c uint32
	for _, u := range mgr.ListUnits() {
		if u.Snapshot().State == sservice.StateFailed {
			c++
		}
	}
	return c
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
		"Id":                      {Value: id, Writable: false, Emit: prop.EmitConst},
		"Names":                   {Value: []string{id}, Writable: false, Emit: prop.EmitConst},
		"Description":             {Value: desc, Writable: false, Emit: prop.EmitConst},
		"LoadState":               {Value: loadState, Writable: false, Emit: prop.EmitTrue},
		"ActiveState":             {Value: activeState, Writable: false, Emit: prop.EmitTrue},
		"SubState":                {Value: subState, Writable: false, Emit: prop.EmitTrue},
		"UnitFileState":           {Value: mgr.UnitFileState(id), Writable: false, Emit: prop.EmitConst},
		"UnitFilePreset":          {Value: "disabled", Writable: false, Emit: prop.EmitConst},
		"Result":                  {Value: "", Writable: false, Emit: prop.EmitTrue},
		"FragmentPath":            {Value: data["FragmentPath"], Writable: false, Emit: prop.EmitConst},
		"DropInPaths":          {Value: []string{}, Writable: false, Emit: prop.EmitConst},
		"NeedDaemonReload":     {Value: false, Writable: false, Emit: prop.EmitConst},
		"SourcePath":              {Value: "", Writable: false, Emit: prop.EmitConst},
		"MainPID":                 {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainPID":             {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainStatus":          {Value: int32(0), Writable: false, Emit: prop.EmitTrue},
		"ExecMainCode":            {Value: int32(0), Writable: false, Emit: prop.EmitTrue},
		"ExitCode":                {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
		"ExitStatus":              {Value: uint8(0), Writable: false, Emit: prop.EmitTrue},
		"NRestarts":               {Value: uint32(0), Writable: false, Emit: prop.EmitConst},
		"StartLimitBurst":         {Value: uint32(5), Writable: false, Emit: prop.EmitConst},
		"StartLimitIntervalUSec":  {Value: uint64(10e6), Writable: false, Emit: prop.EmitConst},
		"StartLimitAction":        {Value: "none", Writable: false, Emit: prop.EmitConst},
		"MemoryCurrent":           {Value: uint64(18446744073709551615), Writable: false, Emit: prop.EmitTrue},
		"CPUWeight":               {Value: uint64(100), Writable: false, Emit: prop.EmitTrue},
		"TasksCurrent":            {Value: uint64(18446744073709551615), Writable: false, Emit: prop.EmitTrue},
	}
}

// buildServiceProps returns the org.freedesktop.systemd1.Service interface
// properties for a unit. These are served on the unit's object path so that
// tools (e.g. openclaw's requireEffective inspection) can read ExecStart,
// WorkingDirectory and Type via `busctl get-property`.
//
// ExecStart is marshaled with systemd's ExecCommand struct signature
// (sasbttttuii): binary path, argv, ignore-errors flag, uid, gid, and a
// handful of integer status/timestamp fields. openclaw only consumes argv
// (the 2nd element) and structurally validates the 10-tuple, so the integer
// fields are filled with their zero defaults.
func buildServiceProps(mgr *supervisor.Manager, name string) map[string]*prop.Prop {
	cfg, err := mgr.FindUnit(name)
	if err != nil || cfg == nil || cfg.Config == nil {
		return nil
	}
	execStart := cfg.Config.Service.ExecStart
	argv := shellSplitExecStart(execStart)
	execPath := ""
	if len(argv) > 0 {
		execPath = argv[0]
	}
	return map[string]*prop.Prop{
		"Type":             {Value: cfg.Config.Service.Type, Writable: false, Emit: prop.EmitConst},
		"WorkingDirectory": {Value: cfg.Config.Service.WorkingDirectory, Writable: false, Emit: prop.EmitConst},
		"ExecStart":        {Value: execStartCommands(execPath, argv), Writable: false, Emit: prop.EmitConst},
		"Environment":      {Value: cfg.Config.Service.Environment, Writable: false, Emit: prop.EmitConst},
		// EnvironmentFiles is a(sb): one (path, ignore-missing) struct per file.
		"EnvironmentFiles": {Value: envFileSpecs(cfg.Config.Service.EnvironmentFile), Writable: false, Emit: prop.EmitConst},
		// The initd unit parser does not track UnsetEnvironment; report empty.
		"UnsetEnvironment": {Value: []string{}, Writable: false, Emit: prop.EmitConst},
	}
}

// execCommand mirrors systemd's D-Bus ExecCommand struct (sasbttttuii).
// Field order and types MUST match the signature so godbus marshals it to
// a(sasbttttuii) and callers that parse the tuple get a 10-element array.
type execCommand struct {
	Binary      string   // s  ExecPath
	Args        []string // as ExecArguments
	IgnoreError bool     // b  ExecIgnoreErrors
	UID         uint64   // t
	GID         uint64   // t
	StartLimit  uint64   // t
	Timeout     uint64   // t
	ExitType    uint32   // u
	ExitStatus  int32    // i
	Result      int32    // i
}

func execStartCommands(path string, argv []string) []execCommand {
	if len(argv) == 0 {
		return nil
	}
	return []execCommand{{
		Binary:      path,
		Args:        argv,
		IgnoreError: false,
	}}
}

// envFile mirrors one element of the EnvironmentFiles property (a(sb)):
// the file path and whether missing files are ignored (a leading '-' prefix
// in EnvironmentFile= toggles ignore-missing, per systemd semantics).
type envFile struct {
	Path          string
	IgnoreMissing bool
}

// envFileSpecs converts the parser's EnvironmentFile values into the
// (path, ignore-missing) struct array that the D-Bus EnvironmentFiles property
// expects.
func envFileSpecs(files []string) []envFile {
	out := make([]envFile, 0, len(files))
	for _, f := range files {
		ignore := false
		if strings.HasPrefix(f, "-") {
			ignore = true
			f = strings.TrimPrefix(f, "-")
		}
		out = append(out, envFile{Path: f, IgnoreMissing: ignore})
	}
	return out
}

// shellSplitExecStart splits a systemd ExecStart= value into argv, honoring
// the double- and single-quoted substrings systemd supports. It is a
// pragmatic implementation good enough for service definitions that do not
// use the full systemd command-line specifier grammar.
func shellSplitExecStart(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var (
		args []string
		cur  strings.Builder
		inQ  byte
	)
	flush := func() {
		args = append(args, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '"' || c == '\'':
			if inQ == c {
				inQ = 0
			} else if inQ == 0 {
				inQ = c
			} else {
				cur.WriteByte(c)
			}
		case c == ' ' || c == '\t':
			if inQ != 0 {
				cur.WriteByte(c)
			} else if cur.Len() > 0 {
				flush()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 || len(args) > 0 && (value[len(value)-1] == '"' || value[len(value)-1] == '\'') {
		flush()
	}
	return args
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
		if err := serveManager(ctx, conn, mgr); err != nil {
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
		if err := serveManager(ctx, conn, systemMgr); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func serveManager(ctx context.Context, conn *dbus.Conn, primary *supervisor.Manager) error {
	_ = ctx

	mgrObj := newManager(primary)
	handler := &unitSubtreeHandler{mgr: primarySafe(primary)}

	// Properties layer (org.freedesktop.DBus.Properties) on the manager path.
	if _, err := prop.Export(conn, managerPath, buildManagerProps(primary)); err != nil {
		return fmt.Errorf("export manager properties: %w", err)
	}

	// Manager methods on the manager interface at /org/freedesktop/systemd1.
	if err := conn.Export(mgrObj, managerPath, managerInterface); err != nil {
		return fmt.Errorf("export manager interface: %w", err)
	}

	// Unit subtree: calls on /.../unit/<name> route to handler.
	if err := conn.ExportSubtree(handler, dbus.ObjectPath(unitBasePathString), unitInterface); err != nil {
		return fmt.Errorf("export unit subtree: %w", err)
	}

	// org.freedesktop.DBus.Properties on the unit subtree (dynamic per-unit).
	propsHandler := &unitPropsSubtree{mgr: primarySafe(primary)}
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

func primarySafe(primary *supervisor.Manager) *supervisor.Manager {
	if primary != nil {
		return primary
	}
	return emptyManager
}
