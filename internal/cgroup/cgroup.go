// Package cgroup gives each unit a leaf cgroup so the daemon can tell which
// processes it supervises from kernel membership rather than from a command
// line that any other process may copy.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// namespace is the directory under the daemon's own cgroup that holds every
// unit leaf, so initd never rmdir's a group somebody else made.
const namespace = "initd"

// Tree is one view of the cgroup filesystem, created once by the daemon.
type Tree struct {
	mu     sync.Mutex
	mount  string
	base   string
	avail  bool
	reason string
	probed bool
}

// New discovers the layout. INITD_CGROUP_ROOT overrides the whole base so a
// test can use a temporary directory instead of the kernel's cgroup tree.
func New() *Tree {
	t := &Tree{}
	if root := strings.TrimSpace(os.Getenv("INITD_CGROUP_ROOT")); root != "" {
		t.base = root
		t.mount = filepath.Dir(root)
		return t
	}
	mount, err := v2Mount(procRoot())
	if err != nil {
		t.reason = err.Error()
		return t
	}
	t.mount = mount
	prefix, err := ownCgroup(procRoot())
	if err != nil {
		t.reason = err.Error()
		return t
	}
	t.base = filepath.Join(mount, filepath.Clean("/"+prefix), namespace)
	return t
}

// Available reports whether unit leaves can actually be created here. This is
// decided by trying, because the answer depends on the ownership of a
// directory the mount table does not describe: on a container or a WSL prefix
// the path is there and simply not ours to write.
func (t *Tree) Available() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.probed {
		t.probed, t.avail, t.reason = probe(t.base, t.reason)
	}
	return t.avail
}

// Reason explains an unavailable tree, for the one line the daemon logs at
// startup. Empty when the tree is usable.
func (t *Tree) Reason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reason
}

// LeafPath is the directory of one unit on the filesystem.
func (t *Tree) LeafPath(unit string) (string, error) {
	if t.base == "" {
		return "", fmt.Errorf("no cgroup base")
	}
	name, err := leafName(unit)
	if err != nil {
		return "", err
	}
	return filepath.Join(t.base, name), nil
}

// GroupPath is the unit's path within the hierarchy, which is what systemd's
// ControlGroup property reports.
func (t *Tree) GroupPath(unit string) string {
	path, err := t.LeafPath(unit)
	if err != nil || !t.Available() {
		return ""
	}
	root := filepath.Clean("/"+t.mount) + "/"
	group := filepath.Clean("/" + path)
	if !strings.HasPrefix(group, root) {
		return ""
	}
	return group[len(root)-1:]
}

// Ensure creates the unit's leaf. It is idempotent: a unit that restarts keeps
// the same group, and a leaf already holding processes stays valid.
func (t *Tree) Ensure(unit string) error {
	path, err := t.LeafPath(unit)
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0o755)
}

// Attach moves a running process into the unit's leaf. Only the process itself
// is moved; anything it had already forked stays where it was. The file is
// opened without O_CREATE: the kernel creates it with the group, so a leaf
// that has no cgroup.procs is not a group to move into.
func (t *Tree) Attach(unit string, pid int) error {
	path, err := t.LeafPath(unit)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	f, err := os.OpenFile(filepath.Join(path, "cgroup.procs"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(strconv.Itoa(pid)))
	return err
}

// Members lists the PIDs the kernel says belong to the unit's leaf. An error
// is returned only when the answer is unknowable; an empty leaf is an empty
// list and no error.
func (t *Tree) Members(unit string) ([]int, error) {
	path, err := t.LeafPath(unit)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// HasLeaf reports whether this unit currently has a group at all, so callers
// can tell "no members" from "never placed anything in a cgroup".
func (t *Tree) HasLeaf(unit string) bool {
	path, err := t.LeafPath(unit)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Remove drops an empty leaf. A group that still holds processes is left
// alone, since deleting it would take a live service's accounting with it.
func (t *Tree) Remove(unit string) error {
	path, err := t.LeafPath(unit)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func leafName(unit string) (string, error) {
	name := strings.TrimSpace(unit)
	// A leading dot is refused by kernfs, so it can never name a cgroup.
	if name == "" || name[0] == '.' || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("invalid unit name %q for a cgroup leaf", unit)
	}
	return name, nil
}

// probe creates the base and one scratch child in it. Writing a directory is
// the only test that also proves the leaf's files can be written later.
func probe(base, reason string) (probed, ok bool, why string) {
	if base == "" {
		return true, false, reason
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return true, false, fmt.Sprintf("cannot create %s: %v", base, err)
	}
	scratch := filepath.Join(base, probeName())
	if err := os.Mkdir(scratch, 0o755); err != nil {
		return true, false, fmt.Sprintf("%s is not writable: %v", base, err)
	}
	if err := os.Remove(scratch); err != nil {
		return true, false, fmt.Sprintf("cannot remove %s: %v", scratch, err)
	}
	return true, true, ""
}

// probeName must not begin with a dot: kernfs rejects such names outright, so
// a dotted probe would report every real cgroup as unusable.
func probeName() string { return "initd-probe-" + strconv.Itoa(os.Getpid()) }

// v2Mount returns the mount point of the unified hierarchy. A box without it
// (cgroup v1 only) has no leaves to create.
func v2Mount(proc string) (string, error) {
	data, err := os.ReadFile(filepath.Join(proc, "mounts"))
	if err != nil {
		return "", fmt.Errorf("cannot read %s/mounts: %w", proc, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[2] != "cgroup2" {
			continue
		}
		// mounts(5) escapes spaces and tabs in the mount point.
		return unescapeMount(f[1]), nil
	}
	return "", fmt.Errorf("no cgroup2 filesystem in %s/mounts", proc)
}

// ownCgroup reads the hierarchy-0 entry for this process, which is the prefix
// the daemon may create groups under.
func ownCgroup(proc string) (string, error) {
	data, err := os.ReadFile(filepath.Join(proc, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("cannot read %s/self/cgroup: %w", proc, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// The unified entry is "0::<path>"; the controller list between the
		// colons is empty there and non-empty on every v1 line.
		f := strings.SplitN(line, ":", 3)
		if len(f) == 3 && f[0] == "0" && f[1] == "" {
			if f[2] == "" {
				return "/", nil
			}
			return f[2], nil
		}
	}
	return "", fmt.Errorf("no unified hierarchy entry in %s/self/cgroup", proc)
}

func unescapeMount(path string) string {
	if !strings.Contains(path, `\`) {
		return path
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(path)
}

var (
	defaultOnce sync.Once
	defaultTree *Tree
)

// Default is the tree the daemon uses, discovered on first use.
func Default() *Tree {
	defaultOnce.Do(func() { defaultTree = New() })
	return defaultTree
}

// procRoot mirrors the service package's seam so a test can stand in for both
// the mount table and this process's own cgroup entry.
func procRoot() string {
	if p := strings.TrimSpace(os.Getenv("INITD_PROC_ROOT")); p != "" {
		return p
	}
	return "/proc"
}
