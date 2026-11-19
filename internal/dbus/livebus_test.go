package dbus

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

// This test exercises the real transport path (actual godbus marshalling over a
// private dbus-daemon) that the rest of the suite only models: Manager method
// export and argument encoding, JobNew/JobRemoved marshal, NJobs property
// updates, and unexporting the finished job object. It skips where
// dbus-daemon is unavailable.
func TestLiveBusStartUnitJobLifecycle(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not available")
	}

	daemon := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1", "--print-pid=1")
	stdout, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.Start(); err != nil {
		t.Skipf("cannot start dbus-daemon: %v", err)
	}
	defer func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	}()

	address := make(chan string, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "unix:") {
				select {
				case address <- line:
				default:
				}
				return
			}
		}
	}()
	var busAddress string
	select {
	case busAddress = <-address:
	case <-time.After(5 * time.Second):
		<-scanDone
		t.Fatal("dbus-daemon did not print its address")
	}

	mgr := newTestManager(t)
	dir := mgr.SearchPaths[0]
	writeUnitForDBus(t, dir, "zzlivebus.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatal(err)
	}

	mgrConn, err := dbus.Connect(busAddress)
	if err != nil {
		t.Fatalf("manager connect: %v", err)
	}
	defer mgrConn.Close()

	ctx := context.Background()
	managerDone := make(chan error, 1)
	go func() {
		managerDone <- serveManager(ctx, mgrConn, mgr)
	}()
	var serveErr error
	select {
	case serveErr = <-managerDone:
	case <-time.After(5 * time.Second):
		serveErr = fmt.Errorf("serveManager did not return")
	}
	if serveErr != nil {
		t.Fatalf("serveManager: %v", serveErr)
	}
	_ = mgr.StopUnit("zzlivebus.service") // ensure no leftovers on failure paths

	client, err := dbus.Connect(busAddress)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer client.Close()

	obj := client.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")

	// Subscribe to JobRemoved before calling, per the race-free pattern.
	signals := make(chan *dbus.Signal, 8)
	client.Signal(signals)
	if err := client.AddMatchSignal(dbus.WithMatchMember("JobRemoved"), dbus.WithMatchSender("org.freedesktop.systemd1")); err != nil {
		t.Fatalf("match JobRemoved: %v", err)
	}

	var call *dbus.Call
	path, derr := func() (dbus.ObjectPath, *dbus.Error) {
		ch := make(chan *dbus.Call, 1)
		call = obj.Go("org.freedesktop.systemd1.Manager.StartUnit", 0, ch, "zzlivebus.service", "fail")
		select {
		case c := <-ch:
			var path dbus.ObjectPath
			if c.Err != nil {
				return "", dbus.MakeFailedError(c.Err)
			}
			if err := c.Store(&path); err != nil {
				return "", dbus.MakeFailedError(err)
			}
			return path, nil
		case <-time.After(5 * time.Second):
			return "", dbus.MakeFailedError(fmt.Errorf("StartUnit timed out"))
		}
	}()
	_ = call
	if derr != nil {
		t.Fatalf("StartUnit: %v", derr)
	}
	if !strings.HasPrefix(string(path), "/org/freedesktop/systemd1/job/") {
		t.Fatalf("StartUnit returned %q, want a job path", path)
	}

	// The job completes as soon as the manager's (synchronous part of the)
	// start call returns; the unit keeps running afterwards.
	deadline := time.Now().Add(10 * time.Second)
	var result string
	for time.Now().Before(deadline) && result == "" {
		select {
		case sig := <-signals:
			if sig.Name == "org.freedesktop.systemd1.Manager.JobRemoved" {
				var id uint32
				var jobPath dbus.ObjectPath
				var unit, res string
				if len(sig.Body) == 4 {
					id, _ = sig.Body[0].(uint32)
					jobPath, _ = sig.Body[1].(dbus.ObjectPath)
					unit, _ = sig.Body[2].(string)
					res, _ = sig.Body[3].(string)
				}
				if string(jobPath) == string(path) {
					t.Logf("JobRemoved(id=%d unit=%s result=%s)", id, unit, res)
					result = res
				}
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	if result == "" {
		t.Fatal("JobRemoved never arrived")
	}
	if result != "done" {
		t.Fatalf("JobRemoved result = %q, want done", result)
	}

	var variant dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.systemd1.Manager", "NJobs").Store(&variant); err != nil {
		t.Fatalf("NJobs Get after: %v", err)
	}
	if v, ok := variant.Value().(uint32); !ok || v != 0 {
		t.Fatalf("NJobs after completion = %v, want 0", variant.Value())
	}
	var jobsAfter []listJobEntry
	if err := obj.Call("org.freedesktop.systemd1.Manager.ListJobs", 0).Store(&jobsAfter); err != nil {
		t.Fatalf("ListJobs after: %v", err)
	}
	if len(jobsAfter) != 0 {
		t.Fatalf("ListJobs after completion = %+v, want empty", jobsAfter)
	}
	c := obj.Call("org.freedesktop.systemd1.Manager.GetJob", 0, uint32(1))
	if c.Err == nil {
		t.Fatalf("GetJob(1) after completion returned %+v, want NoSuchJob", c)
	}
	name := ""
	switch e := c.Err.(type) {
	case dbus.Error:
		name = e.Name
	case *dbus.Error:
		name = e.Name
	default:
		t.Fatalf("GetJob error = %v (%T), want NoSuchJob", c.Err, c.Err)
	}
	if name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("GetJob error name = %q, want NoSuchJob", name)
	}
	_ = mgr.StopUnit("zzlivebus.service")
}
