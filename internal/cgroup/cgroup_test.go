package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func treeAt(t *testing.T) *Tree {
	t.Helper()
	root := t.TempDir()
	t.Setenv("INITD_CGROUP_ROOT", filepath.Join(root, "initd"))
	return New()
}

// seedGroup models the two things a temporary directory does not do on its own:
// the kernel creates cgroup.procs with the group, and it lists every member
// there, while a write moves exactly one process in. A test that skipped this
// would pass against code that only works on an ordinary file.
func seedGroup(t *testing.T, tree *Tree, unit string, members ...int) string {
	t.Helper()
	path, err := tree.LeafPath(unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "cgroup.procs")
	var b strings.Builder
	for _, pid := range members {
		b.WriteString(strconv.Itoa(pid))
		b.WriteString("\n")
	}
	// Real cgroup.procs is 0644 for reading and 0200 for the move-in write on
	// some kernels; either way the writer never depends on creating it.
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestEnsureCreatesTheLeafTheOtherCallsActOn(t *testing.T) {
	tree := treeAt(t)
	if !tree.Available() {
		t.Fatalf("tree over a temporary directory should be usable: %s", tree.Reason())
	}
	if err := tree.Ensure("widget.service"); err != nil {
		t.Fatal(err)
	}
	if !tree.HasLeaf("widget.service") {
		t.Fatal("HasLeaf() = false for a leaf that was just created")
	}
	// Idempotent, and a restart must not clear the group it is reusing.
	file := seedGroup(t, tree, "widget.service", 4242)
	if err := tree.Ensure("widget.service"); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got, err := tree.Members("widget.service"); err != nil || len(got) != 1 || got[0] != 4242 {
		t.Fatalf("Ensure() disturbed an existing group: %v, %v", got, err)
	}
	if err := tree.Remove("widget.service"); err == nil {
		t.Fatal("Remove() succeeded on a leaf that still holds processes")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("a refused Remove() took the group's file with it: %v", err)
	}
}

// Attach is a write of one PID and nothing else: the kernel's handler parses
// the whole buffer as a pid, so a newline or a second number would be refused.
func TestAttachWritesOnePIDToTheGroupsFile(t *testing.T) {
	tree := treeAt(t)
	file := seedGroup(t, tree, "widget.service")
	if err := tree.Attach("widget.service", 4242); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4242" {
		t.Fatalf("cgroup.procs received %q, want the bare pid", data)
	}
	if got, err := tree.Members("widget.service"); err != nil || len(got) != 1 || got[0] != 4242 {
		t.Fatalf("Members() after attach = %v, %v", got, err)
	}
}

func TestAttachRefusesWhatIsNotAGroup(t *testing.T) {
	tree := treeAt(t)
	if err := tree.Ensure("widget.service"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Attach("widget.service", 0); err == nil {
		t.Fatal("Attach(0) succeeded")
	}
	if err := tree.Attach("widget.service", -1); err == nil {
		t.Fatal("Attach(-1) succeeded")
	}
	// A leaf with no cgroup.procs is not a group, and creating one there would
	// leave a file the kernel never reads.
	if err := tree.Attach("absent.service", 1234); err == nil {
		t.Fatal("Attach() into a leaf that was never ensured")
	}
	path, _ := tree.LeafPath("absent.service")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the refused attach left a directory behind")
	}
	// The group's own file may exist read-only on a kernel that says so; a
	// silent success there would report a process as supervised when the
	// kernel never moved it.
	file := seedGroup(t, tree, "readonly.service")
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && tree.Attach("readonly.service", 4242) == nil {
		t.Fatal("Attach() reported success on an unwritable cgroup.procs")
	}
}

func TestMembersReadsEveryPIDInTheGroup(t *testing.T) {
	tree := treeAt(t)
	file := seedGroup(t, tree, "widget.service")
	lines := "42\nnotapid\n-7\n0\n43\n"
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tree.Members("widget.service")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 42 || got[1] != 43 {
		t.Fatalf("Members() = %v, want [42 43]", got)
	}
}

func TestRemoveAndMembersTolerateAMissingLeaf(t *testing.T) {
	tree := treeAt(t)
	if err := tree.Remove("never.started.service"); err != nil {
		t.Fatalf("Remove() of a unit that never ran: %v", err)
	}
	if got, err := tree.Members("never.started.service"); err != nil || len(got) != 0 {
		t.Fatalf("Members() of a unit that never ran = %v, %v", got, err)
	}
	if tree.HasLeaf("never.started.service") {
		t.Fatal("HasLeaf() = true for a unit that never ran")
	}
	if err := tree.Remove("widget.service"); err != nil {
		t.Fatalf("Remove() of an empty leaf: %v", err)
	}
}

// kernfs refuses a name that begins with a dot, so neither the writability
// probe nor a unit leaf may use one: a dotted name would report every real
// cgroup as unusable.
func TestProbeNameIsAValidCgroupLeaf(t *testing.T) {
	name := probeName()
	if _, err := leafName(name); err != nil {
		t.Fatalf("probe name %q is not a valid leaf: %v", name, err)
	}
	for _, dotted := range []string{".", "..", ".probe"} {
		if _, err := leafName(dotted); err == nil {
			t.Errorf("leafName(%q) accepted a name cgroupfs rejects", dotted)
		}
	}
	base := filepath.Join(t.TempDir(), "initd")
	if probed, ok, why := probe(base, ""); !probed || !ok {
		t.Fatalf("probe of a writable directory failed: %s", why)
	}
	leftovers, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("probe left %d entries behind; it must clean up after itself", len(leftovers))
	}
}

// A base that cannot be created is the host's case: the prefix is mounted and
// simply not ours to write. The daemon must learn that from Available(), not
// from an error on the first start.
func TestUnavailableWhenBaseCannotBeWritten(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITD_CGROUP_ROOT", filepath.Join(blocked, "initd"))
	tree := New()
	if tree.Available() {
		t.Fatal("Available() = true under a path that is a file")
	}
	if tree.Reason() == "" {
		t.Fatal("unavailable tree gave no reason to log")
	}
	if err := tree.Ensure("widget.service"); err == nil {
		t.Fatal("Ensure() reported success where nothing can be created")
	}
	if cg := tree.GroupPath("widget.service"); cg != "" {
		t.Fatalf("GroupPath() = %q for an unusable tree", cg)
	}
}

func TestRefusesNamesThatWouldEscapeTheNamespace(t *testing.T) {
	tree := treeAt(t)
	for _, name := range []string{"", ".", "..", ".probe", "../elsewhere", "a/b", `a\b`, "svc\x00"} {
		if _, err := tree.LeafPath(name); err == nil {
			t.Errorf("LeafPath(%q) accepted a name that is not one safe directory", name)
		}
	}
	if _, err := tree.LeafPath("foo@bar.service"); err != nil {
		t.Errorf("a template instance must be a valid leaf: %v", err)
	}
}

// unifiedEntry is the group named by the cgroup2 line of a process's own
// cgroup file. Every v1 line carries a controller between its colons, so
// reading any line would accept a path that has nothing to do with this tree.
func unifiedEntry(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) == 3 && f[0] == "0" && f[1] == "" {
			return f[2]
		}
	}
	t.Fatalf("%s has no unified hierarchy entry: %q", path, strings.TrimSpace(string(data)))
	return ""
}

// A temporary directory is not cgroup2fs, so everything above proves the code
// against the shape of the files rather than the kernel's behaviour. This test
// runs the same calls against the real tree and is skipped unless
// INITD_CGROUP_REAL=1 asks for it, which is how the arm64 test binary is
// verified on the chroot device.
func TestAgainstTheRealCgroupTree(t *testing.T) {
	if os.Getenv("INITD_CGROUP_REAL") != "1" {
		t.Skip("set INITD_CGROUP_REAL=1 to exercise the kernel's cgroup tree")
	}
	t.Setenv("INITD_CGROUP_ROOT", "")
	tree := New()
	if !tree.Available() {
		t.Fatalf("no usable cgroup here: %s", tree.Reason())
	}
	const unit = "initd-selftest.service"
	t.Cleanup(func() { _ = tree.Remove(unit) })
	if err := tree.Ensure(unit); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if err := tree.Attach(unit, pid); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := tree.Members(unit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != pid {
		t.Fatalf("the kernel reported %v for the group holding pid %d", got, pid)
	}
	// The kernel's own answer about where a process runs must agree with ours,
	// and name this unit's group rather than any path that appears in the file.
	cg := tree.GroupPath(unit)
	if got := unifiedEntry(t, filepath.Join(procRoot(), strconv.Itoa(pid), "cgroup")); got != cg {
		t.Fatalf("the child runs in %q, want the leaf %q", got, cg)
	}
	// Leaves sit under the prefix this process is in, inside the daemon's own
	// namespace - never in the group the daemon itself runs in.
	self := unifiedEntry(t, filepath.Join(procRoot(), "self", "cgroup"))
	if want := filepath.Join(self, namespace, unit); cg != want {
		t.Fatalf("GroupPath() = %q, want %q", cg, want)
	}
	// rmdir of a group with a live member is refused, and the member survives.
	if err := tree.Remove(unit); err == nil {
		t.Fatal("Remove() deleted a group that still held a running process")
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		members, err := tree.Members(unit)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the group still reports %v after its process was killed", members)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tree.Remove(unit); err != nil {
		t.Fatalf("Remove() of the emptied group: %v", err)
	}
	if tree.HasLeaf(unit) {
		t.Fatal("leaf survived Remove()")
	}
}

// The real layout comes from two kernel files: which mount is the unified
// hierarchy, and where this process sits in it. Leaves belong under the
// daemon's own prefix, never beside the host's units.
func TestDiscoveryUnderAStubProcTree(t *testing.T) {
	proc := t.TempDir()
	mount := filepath.Join(t.TempDir(), "sysroot")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "mounts"),
		"tmpfs "+mount+" tmpfs rw 0 0\ncgroup2 "+mount+" cgroup2 rw,nsec 0 0\n")
	write(filepath.Join(proc, "self", "cgroup"), "0::/initd.slice\n1:pids:/\n")
	t.Setenv("INITD_PROC_ROOT", proc)

	tree := New()
	if !tree.Available() {
		t.Fatalf("a writable stub mount should be usable: %s", tree.Reason())
	}
	want := filepath.Join(mount, "initd.slice", namespace, "widget.service")
	got, err := tree.LeafPath("widget.service")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LeafPath() = %q, want %q", got, want)
	}
	if cg := tree.GroupPath("widget.service"); cg != "/initd.slice/"+namespace+"/widget.service" {
		t.Fatalf("GroupPath() = %q", cg)
	}

	// A box with only cgroup v1 has no unified hierarchy to join.
	write(filepath.Join(proc, "mounts"), "cgroup "+mount+" cgroup rw 0 0\n")
	v1 := New()
	if v1.Available() {
		t.Fatal("Available() = true without a cgroup2 mount")
	}
	if !strings.Contains(v1.Reason(), "cgroup2") {
		t.Fatalf("reason should name the missing filesystem, got %q", v1.Reason())
	}
}

// The daemon's own prefix is the root on a plain box, which must not turn into
// a double slash or a leaf beside the mount point.
func TestDiscoveryAtTheRootPrefix(t *testing.T) {
	proc := t.TempDir()
	mount := filepath.Join(t.TempDir(), "cgroup2")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "mounts"),
		[]byte("cgroup2 "+mount+" cgroup2 rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "cgroup"),
		[]byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITD_PROC_ROOT", proc)
	tree := New()
	if !tree.Available() {
		t.Fatalf("unavailable: %s", tree.Reason())
	}
	got, err := tree.LeafPath("svc.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(mount, namespace, "svc.service"); got != want {
		t.Fatalf("LeafPath() = %q, want %q", got, want)
	}
	if cg := tree.GroupPath("svc.service"); cg != "/"+namespace+"/svc.service" {
		t.Fatalf("GroupPath() = %q", cg)
	}
}
