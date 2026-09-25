package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"initd/internal/userpaths"
)

// The scenario that hides a live daemon: a replacement daemon is installed and
// started while the previous one is still serving, and the replacement's
// startup path clears the socket out from under it. The incumbent keeps
// accepting on an inode no client can name, so every systemctl reports a dead
// system manager while the units keep running.
//
// The daemon's own defence is the flock (a second instance cannot serve the
// same scope), so the hazard is entirely in whatever clears the path. These
// cases pin the rule that clearing is only safe when nothing is serving.
func TestSingletonFlockRefusesSecondDaemonForSameScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	first, err := userpaths.AcquireUserLock()
	if err != nil {
		t.Fatalf("first AcquireUserLock: %v", err)
	}
	defer first.Close()

	// A second daemon for the same scope must be refused, not merely
	// discouraged: this is the only thing standing between a restart and two
	// supervisors sharing one address.
	if _, err := userpaths.AcquireUserLock(); err == nil {
		t.Fatal("second AcquireUserLock succeeded; a second daemon could serve the same scope")
	}
}

// The lock is released by the kernel the moment its holder dies, so a crashed
// daemon must not leave the scope permanently unstartable. This is why the
// lock file is never deleted by an installer: deleting the file would let a
// new daemon flock a *different* inode and both would run.
func TestSingletonFlockIsReleasedWhenHolderDies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	holder, err := userpaths.AcquireUserLock()
	if err != nil {
		t.Fatalf("AcquireUserLock: %v", err)
	}

	// A child holds the lock and exits without releasing it, the way a crash
	// or a SIGKILL looks from here.
	cmd := exec.Command("/bin/sh", "-c", `
		exec 9>"$1"
		flock -n 9 || exit 1
		sleep 30
	`, "sh", filepath.Join(dir, "initd.lock"))
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	// Wait until the child really holds it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := userpaths.AcquireUserLock(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Skip("helper never took the lock (flock unavailable?)")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	_ = holder.Close()

	// The kernel drops the flock with the fd, so the scope must be startable
	// again promptly. An installer that waits on this is what makes a restart
	// safe; one that deletes the lock file instead would break the guarantee
	// while appearing to work.
	deadline = time.Now().Add(5 * time.Second)
	for {
		next, err := userpaths.AcquireUserLock()
		if err == nil {
			_ = next.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lock was not released after the holder died; the scope stays unstartable")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writePidFile must refuse to repoint a pid file that a live process owns, and
// must accept one whose owner is gone. Restart scripts wait on this file, so a
// pid file naming the wrong process sends them at the wrong daemon.
func TestWritePidFileRefusesLiveOwnerAndTakesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initd.pid")

	if err := writePidFile(path); err != nil {
		t.Fatalf("writePidFile on a free path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if got, want := len(raw) > 0, true; got != want {
		t.Fatalf("pid file empty after write")
	}

	// A pid file naming a live process (this test binary) must be refused, so
	// a replacement daemon cannot overwrite the live one's identity.
	live := os.Getpid()
	if err := os.WriteFile(path, []byte(itoaTest(live)), 0o644); err != nil {
		t.Fatalf("write live pid: %v", err)
	}
	if err := writePidFile(path); err == nil {
		t.Error("writePidFile took over a pid file owned by a live process")
	}

	// A pid file naming a dead process is stale and must be replaceable, or a
	// crashed daemon would block every future start.
	dead := 999999
	for {
		if err := syscall.Kill(dead, 0); err != nil {
			break
		}
		dead++
	}
	if err := os.WriteFile(path, []byte(itoaTest(dead)), 0o644); err != nil {
		t.Fatalf("write dead pid: %v", err)
	}
	if err := writePidFile(path); err != nil {
		t.Errorf("writePidFile refused a stale pid file: %v", err)
	}
}

// removeOwnPidFile must only delete a file that still names this process, or a
// shutdown that races a restart deletes the successor's pid file and every
// waiting script concludes the daemon is gone.
func TestRemoveOwnPidFileOnlyRemovesOurOwn(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "mine.pid")
	theirs := filepath.Join(dir, "theirs.pid")

	if err := os.WriteFile(mine, []byte(itoaTest(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(theirs, []byte(itoaTest(os.Getpid()+1)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	removeOwnPidFile(theirs)
	if _, err := os.Stat(theirs); err != nil {
		t.Error("removeOwnPidFile deleted a pid file naming another process")
	}

	removeOwnPidFile(mine)
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("removeOwnPidFile kept our own pid file (err=%v)", err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
