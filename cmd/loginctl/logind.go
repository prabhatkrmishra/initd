package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	gdbus "github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// The login manager is real systemd-logind on this host; initd only speaks the
// manager interface. list-sessions and list-users therefore ask logind rather
// than inventing rows from local state.
const (
	login1BusName      = "org.freedesktop.login1"
	login1Path         = gdbus.ObjectPath("/org/freedesktop/login1")
	login1ManagerIface = "org.freedesktop.login1.Manager"
	login1SessionIface = "org.freedesktop.login1.Session"
	login1UserIface    = "org.freedesktop.login1.User"
)

// loginSession is one row of `loginctl list-sessions`.
type loginSession struct {
	ID    string
	UID   uint32
	User  string
	Seat  string
	TTY   string
	State string
	Idle  bool
	Since string // how long the session has been idle, "-" when it is not
}

// loginUser is one row of `loginctl list-users`.
type loginUser struct {
	UID    uint32
	Name   string
	Linger string
	State  string
}

// Fetching is behind variables so the formatters and the fallback paths can be
// tested without a bus. An error means "logind is not reachable", not "there
// are no sessions": the handlers keep their old bus-less output in that case.
var (
	fetchLoginSessions = login1ListSessions
	fetchLoginUsers    = login1ListUsers
)

// login1ListSessions calls Manager.ListSessions and decorates each row with
// the session properties list-sessions displays. Property failures degrade to
// "-" rather than failing the listing: a session can end between the list call
// and the property read.
func login1ListSessions() ([]loginSession, error) {
	conn, err := gdbus.SystemBus()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID   string
		UID  uint32
		User string
		Seat string
		Path gdbus.ObjectPath
	}
	if err := conn.Object(login1BusName, login1Path).
		Call(login1ManagerIface+".ListSessions", 0).Store(&raw); err != nil {
		return nil, err
	}
	sessions := make([]loginSession, 0, len(raw))
	for _, e := range raw {
		s := loginSession{ID: e.ID, UID: e.UID, User: e.User, Seat: e.Seat}
		s.TTY = login1String(conn, e.Path, login1SessionIface, "TTY")
		s.State = login1String(conn, e.Path, login1SessionIface, "State")
		s.Idle = login1Bool(conn, e.Path, login1SessionIface, "IdleHint")
		if s.Idle {
			s.Since = idleSince(login1Uint64(conn, e.Path, login1SessionIface, "IdleSinceHintMonotonic"))
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, nil
}

// login1ListUsers calls Manager.ListUsers and reads the two properties the
// table shows. Linger is a boolean in systemd 255 (it was a string in older
// releases).
func login1ListUsers() ([]loginUser, error) {
	conn, err := gdbus.SystemBus()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		UID  uint32
		Name string
		Path gdbus.ObjectPath
	}
	if err := conn.Object(login1BusName, login1Path).
		Call(login1ManagerIface+".ListUsers", 0).Store(&raw); err != nil {
		return nil, err
	}
	users := make([]loginUser, 0, len(raw))
	for _, e := range raw {
		u := loginUser{UID: e.UID, Name: e.Name, Linger: "no", State: "unknown"}
		if login1Bool(conn, e.Path, login1UserIface, "Linger") {
			u.Linger = "yes"
		}
		if state := login1String(conn, e.Path, login1UserIface, "State"); state != "" {
			u.State = state
		}
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].UID < users[j].UID })
	return users, nil
}

func login1Property(conn *gdbus.Conn, path gdbus.ObjectPath, iface, name string) (interface{}, bool) {
	call := conn.Object(login1BusName, path).
		Call("org.freedesktop.DBus.Properties.Get", 0, iface, name)
	if call.Err != nil {
		return nil, false
	}
	var v gdbus.Variant
	if err := call.Store(&v); err != nil {
		return nil, false
	}
	return v.Value(), true
}

func login1String(conn *gdbus.Conn, path gdbus.ObjectPath, iface, name string) string {
	v, ok := login1Property(conn, path, iface, name)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func login1Bool(conn *gdbus.Conn, path gdbus.ObjectPath, iface, name string) bool {
	v, ok := login1Property(conn, path, iface, name)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func login1Uint64(conn *gdbus.Conn, path gdbus.ObjectPath, iface, name string) uint64 {
	v, ok := login1Property(conn, path, iface, name)
	if !ok {
		return 0
	}
	n, _ := v.(uint64)
	return n
}

// idleSince turns a CLOCK_MONOTONIC timestamp into the relative text the SINCE
// column shows. An unreadable clock prints "-", like an absent timestamp.
func idleSince(usec uint64) string {
	if usec == 0 {
		return "-"
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return "-"
	}
	now := uint64(ts.Sec)*1e6 + uint64(ts.Nsec)/1e3
	if now <= usec {
		return "0s ago"
	}
	return shortDuration(time.Duration(now-usec)*time.Microsecond) + " ago"
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		m, s := int(d/time.Minute), int((d%time.Minute)/time.Second)
		if s == 0 {
			return fmt.Sprintf("%dmin", m)
		}
		return fmt.Sprintf("%dmin %ds", m, s)
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int((d%time.Hour)/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dmin", h, m)
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// printColumns writes a header and rows, padding each column to its widest
// cell with the two-space gap the other initd tools use. numeric marks the
// columns that are right-aligned; noLegend keeps the rows and drops the header.
func printColumns(headers []string, rows [][]string, numeric []bool, noLegend bool) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	write := func(row []string) {
		var b strings.Builder
		for i, c := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			if numeric[i] {
				fmt.Fprintf(&b, "%*s", widths[i], c)
			} else {
				fmt.Fprintf(&b, "%-*s", widths[i], c)
			}
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
	if !noLegend {
		write(headers)
	}
	for _, row := range rows {
		write(row)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// sessionRows shapes fetched sessions for the table and JSON encoders.
func sessionRows(sessions []loginSession) [][]string {
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, []string{
			s.ID,
			strconv.FormatUint(uint64(s.UID), 10),
			s.User,
			dash(s.Seat),
			dash(s.TTY),
			dash(s.State),
			yesNo(s.Idle),
			dash(s.Since),
		})
	}
	return rows
}

// userRows merges logind's users with the linger store. A linger file can
// exist for a user logind has never seen (the shim writes the file directly),
// and list-users should still show that user.
func userRows(users []loginUser) []userRow {
	rows := make([]userRow, 0, len(users))
	seen := make(map[string]bool, len(users))
	for _, u := range users {
		rows = append(rows, userRow{
			UID:    strconv.FormatUint(uint64(u.UID), 10),
			Name:   u.Name,
			Linger: u.Linger,
			State:  u.State,
		})
		seen[u.Name] = true
	}
	for _, name := range lingerUsers() {
		if seen[name] {
			continue
		}
		u, err := resolveUser(name)
		if err != nil {
			continue
		}
		rows = append(rows, userRow{UID: u.Uid, Name: u.Username, Linger: "yes", State: "lingering"})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, aerr := strconv.Atoi(rows[i].UID)
		b, berr := strconv.Atoi(rows[j].UID)
		if aerr == nil && berr == nil {
			return a < b
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}
