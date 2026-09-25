package service

import (
	"bufio"
	"errors"
	"fmt"
	"initd/internal/notify"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"initd/internal/logging"
	"initd/internal/parser"
	"initd/internal/userpaths"

	"github.com/google/shlex"
)

type State string

const (
	StateInactive   State = "inactive"
	StateActivating State = "activating"
	StateActive     State = "active"
	StateStopping   State = "stopping"
	StateFailed     State = "failed"
)

type Runtime struct {
	State     State
	MainPID   int
	ExitCode  int
	LastError string
	// MainCode is systemd's ExecMainCode: 0 never ran, 1 exited, 2 killed by
	// a signal, 3 dumped core.
	MainCode int
	// Result mirrors systemd's unit Result property: a keyword ("success",
	// "exit-code", "timeout") that scripts read, never the message.
	Result string
	// ExecMainPID is systemd's ExecMainPID: the PID the main process *had*.
	// MainPID goes to 0 the moment the process is reaped so nothing tries to
	// signal it; this stays for the status lines that report how it ended.
	ExecMainPID         int
	StartedAt           time.Time
	FinishedAt          time.Time
	StartedAtMonotonic  time.Duration
	FinishedAtMonotonic time.Duration
}

type Unit struct {
	mu             sync.Mutex
	configMu       sync.RWMutex
	Config         *parser.Unit
	Path           string
	Runtime        Runtime
	Cmd            *exec.Cmd
	Logs           *logging.Buffer
	restartHistory []time.Time
	// pgid is the supervisor-side process group created for the last start
	// (Setpgid=true makes it equal the starter's PID). MainPID may later be
	// replaced by PIDFile/notify adoption, which is often NOT the group
	// leader, so group kills must use pgid, never -MainPID.
	pgid int
	// failFwdHistory bounds OnFailure forwarding storms (see
	// allowFailureForward); independent from restartHistory.
	failFwdHistory []time.Time
	startToken     int
	stopRequested  bool
	// nRestarts counts automatic restarts performed by the manager; manual
	// starts do not count. Exposed as systemd's NRestarts.
	nRestarts    int
	reaper       ExitReaper
	notifyServer *notify.Server
	// spawnResult receives the main-process spawn outcome of the start
	// tagged spawnResultToken, so a synchronous starter (systemctl start)
	// learns about a refused exec instead of reporting success for a unit
	// that never ran. nil once the outcome has been delivered.
	spawnResult      chan error
	spawnResultToken int
	socketFiles      []*os.File
	socketEnv        map[string]string
	// managerUserMode is the scope of the manager that holds this unit, nil
	// until one claims it (see inSystemScope).
	managerUserMode  *bool
	onFailureHandler func(string)
	// managedDirs records what the last start created under
	// RuntimeDirectory=/StateDirectory=/... so the stop can undo the /run
	// half and the exec path can export the same variables systemd does.
	managedDirs managedDirectories
	// uidMu guards the expectedUID memo, and is deliberately not mu: the memo
	// is read from the stop and adoption scans, which release mu before walking
	// /proc so that no pass over every PID on the box holds the unit lock.
	uidMu        sync.Mutex
	uidCacheUID  uint32
	uidCacheName string
	// keepDirsOnStop tells the next Stop that this unit is being restarted,
	// not brought down, which is how RuntimeDirectoryPreserve=restart is
	// distinguished from the default.
	keepDirsOnStop bool
	// cgroupLeaf records that this unit has a leaf cgroup, so every
	// membership question can be answered "no evidence" instead of "nobody is
	// running" on a box where groups cannot be created.
	cgroupLeaf bool
}

type managedDirectories struct {
	// runtime lists the absolute RuntimeDirectory= paths to remove on stop.
	runtime []string
	// env holds the computed RUNTIME/STATE/CACHE/LOGS/CONFIGURATION_DIRECTORY
	// assignments in systemd's KEY=value form.
	env []string
}

type ExitReaper interface {
	Register(pid int, handler func(syscall.WaitStatus))
}

type credentialSpec struct {
	uid    uint32
	gid    uint32
	groups []uint32
	set    bool
}

type commandOptions struct {
	rootOnly bool
}

func NewUnit(config *parser.Unit, path string) *Unit {
	return &Unit{
		Config: config,
		Path:   path,
		Logs:   logging.NewBuffer(200),
		Runtime: Runtime{
			State: StateInactive,
		},
	}
}

// SetUserMode records which manager owns the unit. Only a manager knows: the
// same daemon process runs a system manager and a user manager side by side.
func (u *Unit) SetUserMode(userMode bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.managerUserMode = &userMode
}

// GetConfig returns the parsed unit config. The pointer itself is swapped
// under configMu by daemon-reload; the pointed-to Unit is immutable after
// publish, so callers may read fields without further locking.
func (u *Unit) GetConfig() *parser.Unit {
	u.configMu.RLock()
	defer u.configMu.RUnlock()
	return u.Config
}

// SetConfig swaps the parsed config, preserving runtime state across
// daemon-reload. Uses a dedicated mutex so it never deadlocks with mu.
func (u *Unit) SetConfig(c *parser.Unit) {
	u.configMu.Lock()
	u.Config = c
	u.configMu.Unlock()
}

func (u *Unit) SetReaper(reaper ExitReaper) {
	u.mu.Lock()
	u.reaper = reaper
	u.mu.Unlock()
}

func (u *Unit) SetOnFailureHandler(fn func(string)) {
	u.mu.Lock()
	u.onFailureHandler = fn
	u.mu.Unlock()
}

func (u *Unit) Reaper() ExitReaper {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reaper
}

func (u *Unit) Snapshot() Runtime {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Runtime
}

// supervisedPGID returns the last start's process group (0 when none).
func (u *Unit) supervisedPGID() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pgid
}

// NRestarts reports how many automatic restarts the manager has performed,
// matching systemd's NRestarts property (manual starts are not counted).
func (u *Unit) NRestarts() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.nRestarts
}

// NoteRestart records one performed automatic restart.
func (u *Unit) NoteRestart() {
	u.mu.Lock()
	u.nRestarts++
	u.mu.Unlock()
}

// AdmitStart enforces StartLimitBurst for every Manager-driven start path
// (manual systemctl starts, dependency starts, OnFailure forwards), not
// just the automatic restart loop. Already-running units are no-ops and
// cost nothing. Over budget it marks the unit failed, mirroring the
// restart loop's "repeated too quickly" behavior.
func (u *Unit) AdmitStart() error {
	interval, burst := u.StartLimit()
	if interval <= 0 || burst <= 0 {
		return nil
	}
	snap := u.Snapshot()
	if snap.State == StateActive || snap.State == StateActivating {
		return nil
	}
	if n := u.RecordRestart(time.Now(), interval); n > burst {
		u.MarkFailed("Start request repeated too quickly")
		u.Log(logging.LevelError, "Start request repeated too quickly.")
		return fmt.Errorf("Start request repeated too quickly")
	}
	return nil
}

// AllowFailureForward reports whether an OnFailure edge into this unit may
// fire now. It consumes from the unit's StartLimit burst budget, so an
// A<->B failure ping-pong trips "repeated too quickly" instead of looping
// forever. Units without a configured limit are unbounded (as before).
func (u *Unit) AllowFailureForward() bool {
	interval, burst := u.StartLimit()
	if interval <= 0 || burst <= 0 {
		return true
	}
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := now.Add(-interval)
	kept := u.failFwdHistory[:0]
	for _, s := range u.failFwdHistory {
		if s.After(cutoff) {
			kept = append(kept, s)
		}
	}
	u.failFwdHistory = kept
	if len(u.failFwdHistory) >= burst {
		return false
	}
	u.failFwdHistory = append(u.failFwdHistory, now)
	return true
}

// processGroupAlive reports whether the unit still has one of its own
// processes running in pgid.
func (u *Unit) processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	return u.processGroupMemberPID(pgid, 0) != 0
}

// signalUnitPID sends sig to the service process in the mode KillMode
// selects. Group kills never fabricate a group ID from a bare (possibly
// adopted) PID: the group is resolved via Getpgid on the live leader,
// with the recorded starter group as fallback for orphans whose leader
// already exited. The leader itself is always signalled too, covering a
// daemon that left the group (setsid) between resolution and kill.
//
// Safety rule: the supervisor never signals its own group. If the resolved
// or recorded group contains initd itself (group collapse from a shared
// session, or PID reuse landing on the daemon), that group kill is skipped
// and only the leader PID is signalled — taking the daemon down with the
// unit is always the worse outcome.
//
// A group kill on the recorded starter group additionally requires that the
// group still holds one of this unit's processes. The recording is a number,
// and once our starter exits the kernel can hand that number to a new,
// unrelated group; sweeping it then signals processes nothing here supervises.
func (u *Unit) signalUnitPID(pid, fallbackPGID int, group bool, sig syscall.Signal) {
	// Membership is added to the group path, never substituted for it: a process
	// forked before the starter was moved in, and one adopted from a pid file,
	// belong to the unit without being listed. A process may be signalled twice
	// by that, which is the cheaper mistake than leaving one running.
	if group {
		for _, member := range u.cgroupMembers() {
			if member > 0 && member != os.Getpid() {
				_ = syscall.Kill(member, sig)
			}
		}
	}
	selfPGID := syscall.Getpgrp()
	groupHasSelf := func(gid int) bool {
		if gid <= 0 || gid != selfPGID {
			return false
		}
		// Same numeric group: confirm the daemon really is still in it
		// (guards PID-reuse races on the group id).
		if cur, err := syscall.Getpgid(os.Getpid()); err == nil && cur == gid {
			return true
		}
		return false
	}
	sweepFallbackGroup := func() bool {
		return fallbackPGID > 0 && !groupHasSelf(fallbackPGID) &&
			groupHasOwnedMember(fallbackPGID, u.expectedUID())
	}
	if pid <= 0 {
		if group && sweepFallbackGroup() {
			_ = syscall.Kill(-fallbackPGID, sig)
		}
		return
	}
	if !group {
		_ = syscall.Kill(pid, sig)
		return
	}
	if gid, err := syscall.Getpgid(pid); err == nil && gid > 0 && !groupHasSelf(gid) {
		_ = syscall.Kill(-gid, sig)
	}
	_ = syscall.Kill(pid, sig)
	if fallbackPGID > 0 {
		if gid, err := syscall.Getpgid(pid); err != nil || gid != fallbackPGID {
			if sweepFallbackGroup() {
				_ = syscall.Kill(-fallbackPGID, sig)
			}
		}
	}
}

// unitGroupAlive reports whether the service still has live processes:
// the main PID, or (in group mode) any member of its resolved or starter
// group. Without cgroups this is best-effort, but it never reports stopped
// while supervised children remain.
func (u *Unit) unitGroupAlive(pid, fallbackPGID int, group bool) bool {
	if pid > 0 && processAlive(pid) {
		return true
	}
	if !group {
		return false
	}
	// The group answers exactly where a /proc scan can only guess, and answers
	// on a box where hidepid hides the processes entirely. It can only ever say
	// someone is alive: a group listing nobody is no evidence rather than proof
	// of an idle unit, so the scan below still gets the last word.
	if alive, known := u.cgroupAlive(); known && alive {
		return true
	}
	if pid > 0 {
		if gid, err := syscall.Getpgid(pid); err == nil && gid > 0 {
			if u.processGroupAlive(gid) {
				return true
			}
		}
	}
	return u.processGroupAlive(fallbackPGID)
}

func (u *Unit) Start() (int, error) {
	u.mu.Lock()
	if u.Runtime.State == StateActive || u.Runtime.State == StateActivating {
		token := u.startToken
		u.mu.Unlock()
		return token, nil
	}
	state := u.Runtime.State
	ignored := u.GetConfig().Ignored
	u.mu.Unlock()
	if state == StateStopping {
		return 0, fmt.Errorf("unit %s is stopping, try again", u.GetConfig().Name)
	}
	// Surface silently-dropped hardening directives before supervising.
	// The Ignored snapshot above is read without the log lock held (Log
	// takes it per entry), so no mutex is carried across the writes.
	u.warnIgnoredDirectives(ignored)
	u.mu.Lock()
	u.startToken++
	u.stopRequested = false
	token := u.startToken
	u.spawnResult = make(chan error, 1)
	u.spawnResultToken = token
	u.Runtime.State = StateActivating
	u.Runtime.LastError = ""
	u.Runtime.Result = ""
	u.Runtime.MainCode = 0
	u.Runtime.ExitCode = 0
	u.Runtime.FinishedAt = time.Time{}
	u.Runtime.FinishedAtMonotonic = 0
	u.Runtime.StartedAtMonotonic = 0
	u.Runtime.MainPID = 0
	u.Runtime.ExecMainPID = 0
	u.pgid = 0
	u.mu.Unlock()

	if u.checkConditions() {
		// An unmet Condition= skips the start upstream and the job still
		// succeeds: failing it here turned a deliberately conditional unit
		// into a red "failed" entry in status and is-failed.
		u.skipStart()
		return token, nil
	}

	if status, err := u.runExecCondition(); err != nil {
		u.markFailed(err, false)
		return token, err
	} else if status == "skip" {
		u.skipStart()
		return token, nil
	}

	execStart := strings.TrimSpace(u.GetConfig().Service.ExecStart)
	if execStart == "" {
		err := errors.New("ExecStart is empty")
		u.markFailed(err, false)
		return token, err
	}

	if err := u.ensureManagedDirectories(); err != nil {
		u.markFailed(err, false)
		return token, err
	}

	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		u.markFailed(err, false)
		return token, err
	}

	execStart, ignoreFailure := stripPrefix(execStart)
	execStart = u.expandSpecifiers(execStart)
	expandedStart := expandWithEnv(execStart, envMap)
	args, err := shlex.Split(expandedStart)
	if err != nil {
		u.markFailed(err, ignoreFailure)
		if ignoreFailure {
			return token, nil
		}
		return token, fmt.Errorf("parse ExecStart: %w", err)
	}
	if len(args) == 0 {
		err := errors.New("ExecStart parsed to empty")
		u.markFailed(err, ignoreFailure)
		if ignoreFailure {
			return token, nil
		}
		return token, err
	}
	// Handle systemd argv[0] override: ExecStart=@/path/to/exe @argv0 args...
	// After stripPrefix removes the leading @, args[1] may still carry @argv0.
	var argv0 string
	if len(args) > 1 && strings.HasPrefix(args[1], "@") {
		argv0 = args[1][1:]
		args = append(args[:1], args[2:]...)
	}

	go u.runStartSequence(token, args, envMap, envList, ignoreFailure, argv0)
	return token, nil
}

// spawnResultTimeout bounds how long StartAndWait waits for the spawn
// outcome. A child either forks/executes within milliseconds or the start is
// stuck behind a slow ExecStartPre, in which case waiting longer buys nothing
// that the asynchronous path did not already leave unanswered.
const spawnResultTimeout = 10 * time.Second

// oneshotWaitTimeout bounds waiting for a oneshot to finish inside
// StartAndWait, tighter than TimeoutStartSec because manager starts are
// serialised behind a single mutex.
const oneshotWaitTimeout = 30 * time.Second

// startOutcomeGrace is added to a job's wait so the timer that decides the
// outcome - notify's readiness deadline, forking's PIDFile poll - fires first.
// Both resolve at exactly TimeoutStartSec, and a waiter that gives up in the
// same instant reports success over a unit that failed a moment later.
const startOutcomeGrace = 500 * time.Millisecond

// reportSpawn delivers a start's main-process spawn outcome to a synchronous
// waiter. Only the first outcome of the current token counts: a spawn that
// succeeded and later failed on ExecStartPost was already reported as
// started, exactly as an asynchronous caller observed it before.
func (u *Unit) reportSpawn(token int, err error) {
	u.mu.Lock()
	if u.spawnResult != nil && u.spawnResultToken == token {
		select {
		case u.spawnResult <- err:
		default:
		}
		u.spawnResult = nil
	}
	u.mu.Unlock()
}

// StartAndWait starts a unit and reports the outcome the way systemd's start
// job would:
//
//   - Type=simple/idle: the job is complete as soon as the child is forked, so
//     upstream `systemctl start` answers success even when the exec then fails
//     with status=203/EXEC. Do the same; the failure still lands in the unit
//     state, Result and exit status.
//   - Type=oneshot: the job waits for the process to exit, so a refused exec
//     AND a non-zero exit are both reported.
//   - Type=forking/notify/exec: the job waits for activation to begin, which
//     can never happen after a refused exec, so that is reported.
//
// Every wait is bounded: a unit still activating past the window is reported
// as started, matching the asynchronous path this replaces.
// StartAndWait starts the unit and, for the service types whose job is not
// finished at fork+exec, waits for how it ended. waitForJob is false on the
// paths nobody is watching the answer for - boot and socket activation: a
// unit with a large TimeoutStartSec would otherwise hold up every start behind
// the manager's serialising mutex.
func (u *Unit) StartAndWait(waitForJob bool) (int, error) {
	token, err := u.Start()
	if err != nil {
		return token, err
	}
	waitSpawnOutcome := true
	waitCompletion := false
	switch u.canonicalServiceType() {
	case "simple", "idle":
		waitSpawnOutcome = false
	case "oneshot", "notify":
		waitCompletion = true
	case "forking":
		// The job is not done when ExecStart returns - only when the daemon
		// it forked has been adopted (or the guess gave up), so `start` does
		// not report success over a unit still in "activating" with no PID.
		waitCompletion = true
	}
	if !waitForJob {
		waitSpawnOutcome, waitCompletion = false, false
	}
	if !waitSpawnOutcome {
		return token, nil
	}
	u.mu.Lock()
	ch, chToken := u.spawnResult, u.spawnResultToken
	u.mu.Unlock()
	if ch == nil || chToken != token {
		return token, nil
	}
	select {
	case spawnErr := <-ch:
		if spawnErr != nil {
			// Upstream reports a failed job, not the errno: the reason
			// belongs to the unit state (LastError / status / journal), and
			// scripts parse this sentence.
			return token, u.jobFailureError()
		}
	case <-time.After(spawnResultTimeout):
		return token, nil
	}
	if !waitCompletion {
		return token, nil
	}
	// The child ran; a oneshot job is only done once it is gone, and a
	// notify job once the service says READY. Cap the wait below
	// TimeoutStartSec because the manager serialises starts behind one
	// mutex, and one long unit must not freeze `start`.
	budget := oneshotWaitTimeout
	if to := u.StartTimeout(); to > 0 && to < budget {
		budget = to
	}
	if u.canonicalServiceType() == "notify" {
		// waitNotify's own timer is what turns a silent service into
		// Result=timeout; it fires at StartTimeout, so wait a moment past it
		// instead of returning success over a unit still in "activating".
		budget += startOutcomeGrace
	} else if u.canonicalServiceType() == "forking" {
		// Same race on the other side of the same deadline: resolveForkingMainPID
		// stops polling at StartTimeout, so a start that never produced a
		// daemon needs the grace to be seen failing.
		budget += startOutcomeGrace
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		switch snap := u.Snapshot(); snap.State {
		case StateFailed:
			return token, u.jobFailureError()
		case StateActive, StateInactive:
			return token, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return token, nil
}

// jobFailureError is the sentence upstream `systemctl start` prints when a
// start job ends failed. The wording keys on the unit's Result keyword: a
// start that ran out of TimeoutStartSec blames the timeout, everything else
// blames the control process.
func (u *Unit) jobFailureError() error {
	name := u.GetConfig().Name
	if u.Snapshot().Result == "timeout" {
		return fmt.Errorf("Job for %s failed because a timeout was exceeded.", name)
	}
	return fmt.Errorf("Job for %s failed because the control process exited with error code.", name)
}

// newInvocationID mints a 128-bit run id like systemd's invocation ids:
// the kernel uuid when available, else time plus pid (unique per host).
func newInvocationID() string {
	if raw, err := os.ReadFile("/proc/sys/kernel/random/uuid"); err == nil {
		if id := strings.ReplaceAll(strings.TrimSpace(string(raw)), "-", ""); len(id) == 32 {
			return id
		}
	}
	return fmt.Sprintf("%016x%016x", time.Now().UnixNano(), os.Getpid())
}

// socketWrapShell is the shell that hands a socket-activated daemon its own
// pid in $LISTEN_PID. It is a variable for one reason: the honest thing to do
// when there is no shell is fail the start, and the only way to test that is to
// take a shell away.
var socketWrapShell = "/bin/sh"

func (u *Unit) runStartSequence(token int, args []string, envMap map[string]string, envList []string, ignoreFailure bool, argv0 string) {
	// The sockets handed over for activation are claimed here, before anything
	// can fail. They used to be taken further down, so a start that returned
	// early left the dup'd descriptors in the unit - and the next trigger
	// overwrote them without closing. A socket-activated unit with a failing
	// pre-exec leaks a listening fd every 100ms the manager polls that socket,
	// which is how it takes the whole daemon down with it.
	socketFiles, socketEnv := u.takeSocketActivation()
	closeSocketFiles := func() {
		for _, f := range socketFiles {
			_ = f.Close()
		}
		socketFiles = nil
	}

	if !u.isCurrentToken(token) {
		closeSocketFiles()
		return
	}

	// Every start (including restarts) is a new invocation: lines logged
	// from here on carry its id so journalctl -I/--invocation can isolate
	// this run from earlier ones.
	u.Logs.SetInvocation(newInvocationID())

	// The group exists before anything runs, so every process this start makes
	// - the helper scripts included - can be placed in it.
	u.prepareCgroup()

	if err := u.runExecStartPre(token, envMap, envList); err != nil {
		closeSocketFiles()
		u.markFailed(err, false)
		u.reportSpawn(token, err)
		return
	}
	if !u.isCurrentToken(token) {
		closeSocketFiles()
		return
	}

	serviceType := u.canonicalServiceType()

	// A tolerated ("-prefixed") ExecStart failure is not a failed start: the
	// waiter must see success, as upstream `systemctl start` reports.
	report := func(err error) {
		if ignoreFailure {
			u.reportSpawn(token, nil)
			return
		}
		u.reportSpawn(token, err)
	}

	cmd, err := u.buildExecCommand(args, commandOptions{})
	if err != nil {
		closeSocketFiles()
		u.recordExecFailure(err, ignoreFailure)
		report(err)
		return
	}
	// Socket activation: the descriptors claimed at the top of this start go to
	// the child, which gets its own dups. The parent copies are released after
	// Start and on every failure path.
	socketActivated := false
	if len(socketFiles) > 0 {
		for k, v := range socketEnv {
			envList = append(envList, k+"="+v)
		}
		// sd_listen_fds() returns nothing to a process whose pid does not match
		// $LISTEN_PID, and that pid is only known once the child exists. The
		// shell assigns its own $$ and execs over itself, so the number the
		// daemon reads is the daemon.
		wrappedArgs := []string{socketWrapShell, "-c", `LISTEN_PID=$$ exec "$@"`, "_"}
		wrappedArgs = append(wrappedArgs, args...)
		wrappedCmd, werr := u.buildExecCommand(wrappedArgs, commandOptions{})
		if werr != nil {
			// Without the wrap there is no honest LISTEN_PID, and a daemon
			// started with none sees an empty fd set: it comes up and never
			// answers its socket, which reads as a working service. Fail the
			// start instead.
			wrapped := fmt.Errorf("socket activation handover failed: %w", werr)
			closeSocketFiles()
			u.recordExecFailure(wrapped, ignoreFailure)
			report(wrapped)
			return
		}
		wrappedCmd.ExtraFiles = socketFiles
		cmd = wrappedCmd
		socketActivated = true
		if argv0 != "" {
			// The wrap passes argv through to exec, and there is no portable
			// way to rename argv[0] through a shell, so an ExecStart=@… @name
			// override is the one thing a socket-activated start gives up.
			u.Log(logging.LevelInfo, "Warning: argv[0] override ignored for a socket-activated start")
		}
	}
	if argv0 != "" && !socketActivated {
		cmd.Args[0] = argv0
	}

	// -------------------------------------------------
	// Proper notify socket creation
	// -------------------------------------------------
	if serviceType == "notify" {
		server, err := notify.Start()
		if err != nil {
			closeSocketFiles()
			wrapped := fmt.Errorf("notify socket create failed: %w", err)
			u.markFailed(wrapped, ignoreFailure)
			report(wrapped)
			return
		}

		envList = append(envList, "NOTIFY_SOCKET="+server.Path)

		u.mu.Lock()
		u.notifyServer = server
		u.mu.Unlock()
	}

	stdoutLogger := &logging.LineLogger{
		Unit:   u.GetConfig().Name,
		PID:    0,
		Level:  logging.LevelInfo,
		Buffer: u.Logs,
		Output: os.Stdout,
	}
	stderrLogger := &logging.LineLogger{
		Unit:   u.GetConfig().Name,
		PID:    0,
		Level:  logging.LevelError,
		Buffer: u.Logs,
		Output: os.Stderr,
	}

	u.configureCommand(cmd, envList, stdoutLogger, stderrLogger)

	if err := cmd.Start(); err != nil {
		closeSocketFiles()
		u.recordExecFailure(err, ignoreFailure)
		report(err)

		u.mu.Lock()
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		u.mu.Unlock()

		return
	}

	// Child has its own dups now; release the parent copies.
	closeSocketFiles()

	// Claim the process for this unit's group before anything looks at it.
	if cmd.Process != nil {
		u.placeInCgroup(cmd.Process.Pid)
	}

	// Record the starter's OWN group, not its PID-as-group: children that
	// share the daemon's session (common when the daemon was launched from
	// the same shell/session, e.g. hermes) would otherwise make the daemon
	// a member of the unit's recorded group, and a later group kill would
	// take the supervisor down with the unit.
	starterPGID := 0
	if cmd.Process != nil {
		if gid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && gid > 0 {
			starterPGID = gid
		}
	}

	u.mu.Lock()
	u.pgid = starterPGID

	if u.startToken != token {
		u.mu.Unlock()
		_ = cmd.Process.Kill()
		return
	}

	u.Cmd = cmd
	u.Runtime.StartedAt = time.Now()
	u.Runtime.StartedAtMonotonic = logging.MonotonicNow()
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid
	// systemd records ExecMainPID the moment it forks, before the type is
	// confirmed, so a unit that dies in "activating" still reports how its
	// process ended. MainPID stays zero until the type is known to be up.
	u.Runtime.ExecMainPID = cmd.Process.Pid

	if serviceType == "simple" {
		u.Runtime.State = StateActive
		u.Runtime.MainPID = cmd.Process.Pid
	}

	u.mu.Unlock()
	report(nil)

	// -------------------------------------------------
	// Register reaper first (avoid race)
	// -------------------------------------------------
	if u.reaper != nil {
		resetActive := serviceType == "simple" || serviceType == "notify"
		pid := cmd.Process.Pid

		u.reaper.Register(pid, func(status syscall.WaitStatus) {
			if serviceType == "oneshot" {
				exitCode := commandExitStatus(status)
				if exitCode == 0 {
					if err := u.runExecStartPost(token, envMap, envList); err != nil {
						u.markFailed(err, ignoreFailure)
						return
					}
				}
			}
			u.handleExitStatusForPID(token, pid, status, ignoreFailure, resetActive)
		})
	}

	// -------------------------------------------------
	// Dispatch wait handlers
	// -------------------------------------------------
	switch serviceType {
	case "oneshot":
		go u.waitOneshot(token, envMap, envList, ignoreFailure)
	case "forking":
		go u.waitForking(token, envMap, envList, ignoreFailure)
	case "notify":
		go u.waitNotify(token, envMap, envList, ignoreFailure)
	default:
		go u.waitSimple(token, envMap, envList, ignoreFailure)
	}
}

func (u *Unit) waitSimple(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		u.killMainProcess(u.stopSignal())
		u.markFailed(err, ignoreFailure)
		return
	}
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, true)
}

func (u *Unit) waitOneshot(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	if err != nil {
		u.handleExit(token, err, ignoreFailure, false)
		return
	}
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}
	u.handleExit(token, nil, ignoreFailure, false)
}

func (u *Unit) waitForking(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	startedAt := time.Now()
	startedAtMonotonic := logging.MonotonicNow()
	timeout := u.StartTimeout()
	poll := 100 * time.Millisecond

	// systemd waits for the PIDFile to appear for Type=forking; without cgroups
	// we treat the PIDFile PID as the main process once it shows up.
	pid, err := u.resolveForkingMainPID(timeout, poll)

	if err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}

	u.mu.Lock()
	if u.startToken != token || u.Runtime.State == StateFailed {
		u.mu.Unlock()
		return
	}
	u.Runtime.State = StateActive
	u.Runtime.MainPID = pid
	u.Runtime.ExecMainPID = pid
	u.Runtime.StartedAt = startedAt
	u.Runtime.StartedAtMonotonic = startedAtMonotonic
	u.mu.Unlock()
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		if pid != 0 {
			_ = syscall.Kill(pid, u.stopSignal())
		}
		u.markFailed(err, ignoreFailure)
		return
	}
	if u.reaper != nil {
		return
	}

	err = u.Cmd.Wait()
	if err != nil {
		u.mu.Lock()
		shouldHandle := u.startToken == token && u.Runtime.MainPID == 0 && u.Runtime.State != StateActive
		u.mu.Unlock()
		if shouldHandle {
			u.handleExit(token, err, ignoreFailure, false)
		}
	}
}

func (u *Unit) Stop(timeout time.Duration) error {
	u.mu.Lock()
	u.stopRequested = true
	u.mu.Unlock()
	// A release that was already on its way when this stop started may have just
	// handed the leaf back, and every helper below is meant to run inside it -
	// one with no group of its own lands in the daemon's, where it can see every
	// process the supervisor has.
	u.prepareCgroup()

	runStopPost := true
	defer func() {
		u.mu.Lock()
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		u.mu.Unlock()
		if runStopPost {
			_ = u.runExecStopPost()
		}
		u.removeManagedRuntimeDirectories()
		// Last, because ExecStopPost runs inside the group: sweeping it before
		// the script finished would kill the helper that was asked to clean up.
		u.finishCgroupStop()
	}()

	stopCommand := strings.TrimSpace(u.GetConfig().Service.ExecStop)
	if stopCommand != "" {
		if err := u.runStopCommand(stopCommand); err != nil {
			return err
		}
	}

	// Oneshot units kept Active by RemainAfterExit have no process to reap:
	// a successful ExecStop means the stop is done.
	u.mu.Lock()
	remainDone := u.Runtime.State == StateActive && u.Runtime.MainPID == 0
	u.mu.Unlock()
	if remainDone && u.remainAfterExit() {
		u.mu.Lock()
		u.Runtime.State = StateInactive
		u.Runtime.LastError = ""
		u.Runtime.ExitCode = 0
		u.Runtime.FinishedAt = time.Now()
		u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
		u.mu.Unlock()
		return nil
	}

	serviceType := u.canonicalServiceType()
	killProcessGroup := !u.killModeProcess()

	u.mu.Lock()
	if u.Runtime.State == StateInactive {
		runStopPost = false
		u.mu.Unlock()
		return nil
	}
	if u.Runtime.State == StateActivating {
		// Cancel an in-flight activation. Bump the token so the
		// runStartSequence goroutine aborts instead of later marking
		// the unit active after we've asked it to stop.
		u.startToken++
		pid := u.Runtime.MainPID
		pgid := u.pgid
		cmd := u.Cmd
		srv := u.notifyServer
		u.notifyServer = nil
		if pid == 0 && (cmd == nil || cmd.Process == nil) {
			u.Runtime.State = StateInactive
			u.Runtime.MainPID = 0
			u.pgid = 0
			u.Runtime.LastError = ""
			u.Runtime.FinishedAt = time.Now()
			u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
			u.mu.Unlock()
			if srv != nil {
				srv.Stop()
			}
			return nil
		}
		u.Runtime.State = StateStopping
		u.mu.Unlock()
		if srv != nil {
			srv.Stop()
		}
		// Kill the activating process and wait for it to exit, then
		// go inactive. Don't rely on handleExit (token was bumped).
		if pid != 0 || (killProcessGroup && pgid > 0) {
			u.signalUnitPID(pid, pgid, killProcessGroup, u.stopSignal())
		} else if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(u.stopSignal())
		}
		// timeout <=0 means infinity (systemd's "infinity"); wait
		// without deadline in that case.
		if timeout <= 0 {
			for {
				if pid != 0 && !processAlive(pid) {
					u.mu.Lock()
					u.Runtime.State = StateInactive
					u.Runtime.MainPID = 0
					u.pgid = 0
					u.Runtime.LastError = ""
					u.Runtime.ExitCode = 0
					u.Runtime.FinishedAt = time.Now()
					u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
					u.mu.Unlock()
					return nil
				}
				if pid == 0 && cmd != nil && cmd.Process != nil {
					if !processAlive(cmd.Process.Pid) {
						u.mu.Lock()
						u.Runtime.State = StateInactive
						u.Runtime.MainPID = 0
						u.pgid = 0
						u.Runtime.LastError = ""
						u.Runtime.ExitCode = 0
						u.Runtime.FinishedAt = time.Now()
						u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
						u.mu.Unlock()
						return nil
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if pid != 0 && !processAlive(pid) {
				u.mu.Lock()
				u.Runtime.State = StateInactive
				u.Runtime.MainPID = 0
				u.pgid = 0
				u.Runtime.LastError = ""
				u.Runtime.ExitCode = 0
				u.Runtime.FinishedAt = time.Now()
				u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
				u.mu.Unlock()
				return nil
			}
			if pid == 0 && cmd != nil && cmd.Process != nil {
				if !processAlive(cmd.Process.Pid) {
					u.mu.Lock()
					u.Runtime.State = StateInactive
					u.Runtime.MainPID = 0
					u.pgid = 0
					u.Runtime.LastError = ""
					u.Runtime.ExitCode = 0
					u.Runtime.FinishedAt = time.Now()
					u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
					u.mu.Unlock()
					return nil
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		u.signalUnitPID(pid, pgid, killProcessGroup, syscall.SIGKILL)
		if pid == 0 && cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		time.Sleep(150 * time.Millisecond)
		u.mu.Lock()
		u.Runtime.State = StateInactive
		u.Runtime.MainPID = 0
		u.pgid = 0
		u.Runtime.LastError = ""
		u.Runtime.ExitCode = 0
		u.Runtime.FinishedAt = time.Now()
		u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
		u.mu.Unlock()
		return nil
	}
	u.Runtime.State = StateStopping
	pid := u.Runtime.MainPID
	pgid := u.pgid
	cmd := u.Cmd
	u.mu.Unlock()

	// Notify uses the same stop path as simple: kill the (possibly adopted)
	// main PID or the whole process group per KillMode, poll for exit and
	// escalate to SIGKILL after the timeout. The old notify-only branch
	// killed a single PID and returned without waiting when a reaper was
	// set, leaving children behind and reporting inactive while alive.
	// -----------------------------
	// Forking handling
	// -----------------------------
	if serviceType == "forking" {
		if pid == 0 {
			if mainPID, err := u.readPIDFile(); err == nil {
				pid = mainPID
			}
		}
		// A forking daemon may have left the supervisor's group (setsid),
		// so signal both its group (when KillMode allows) and the leader
		// itself; group membership is resolved, never assumed from the PID.
		u.signalUnitPID(pid, pgid, killProcessGroup, u.stopSignal())
	} else {
		// simple / others (includes adopted notify PIDs)
		u.signalUnitPID(pid, pgid, killProcessGroup, u.stopSignal())
	}

	waitUntilStopped := func() bool {
		if serviceType == "forking" {
			if pid == 0 && (!killProcessGroup || !u.unitGroupAlive(0, pgid, killProcessGroup)) {
				u.transitionState(StateInactive, "")
				return true
			}
			if pid != 0 && !u.unitGroupAlive(pid, pgid, killProcessGroup) {
				u.transitionState(StateInactive, "")
				return true
			}
		} else {
			// For simple/notify/exec/oneshot, the process exiting should
			// drive the state to inactive via handleExit. Poll both the
			// state and the live PID so we don't hang if handleExit races
			// or the reaper hasn't yet reaped the child. This mirrors
			// systemd's behavior of tracking the main PID liveness.
			// If there is no PID at all (lost track, external gone,
			// oneshot with no process), nothing is running: a stopping
			// unit must go inactive instead of sleeping until timeout.
			if pid == 0 && (cmd == nil || cmd.Process == nil || !processAlive(cmd.Process.Pid)) {
				u.transitionState(StateInactive, "")
				return true
			}
			if pid != 0 && !u.unitGroupAlive(pid, pgid, killProcessGroup) {
				u.transitionState(StateInactive, "")
				return true
			}
			state := u.Snapshot().State
			if state == StateInactive || state == StateFailed {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	}

	if timeout <= 0 {
		for {
			if waitUntilStopped() {
				return nil
			}
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if waitUntilStopped() {
			return nil
		}
	}

	// Timeout escalation mirrors the initial mode: group kills stay group
	// kills, process kills stay process kills.
	u.signalUnitPID(pid, pgid, killProcessGroup, syscall.SIGKILL)

	// Give SIGKILL a moment to take effect before judging.
	time.Sleep(150 * time.Millisecond)
	if !u.unitGroupAlive(pid, pgid, killProcessGroup) {
		u.transitionState(StateInactive, "")
		return nil
	}
	u.transitionState(StateFailed, "terminated after timeout")
	return errors.New("stop timeout")
}

func (u *Unit) Restart(timeout time.Duration) error {
	if err := u.Stop(timeout); err != nil {
		return err
	}
	_, err := u.Start()
	return err
}

func (u *Unit) Reload() error {
	u.mu.Lock()
	active := u.Runtime.State == StateActive
	u.mu.Unlock()
	if !active {
		return errors.New("unit is not active")
	}
	if len(u.GetConfig().Service.ExecReload) == 0 {
		return errors.New("ExecReload not set")
	}

	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}
	for _, command := range u.GetConfig().Service.ExecReload {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{}, u.StartTimeout()); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runStopCommand(command string) error {
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}

	return u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.GetConfig().Service.PermissionsStartOnly}, u.StopTimeout())
}

// inSystemScope reports the scope of the manager holding this unit rather than
// the process uid, because one daemon serves both kinds at once. An unclaimed
// unit falls back to the uid.
func (u *Unit) inSystemScope() bool {
	u.mu.Lock()
	userMode := u.managerUserMode
	u.mu.Unlock()
	if userMode == nil {
		return os.Geteuid() == 0
	}
	return !*userMode
}

// passEnvironmentHas reports whether a manager variable is on the unit's
// PassEnvironment= list. An entry given in assignment form ("FOO=bar") counts
// as its name, where upstream would match that string against nothing.
func passEnvironmentHas(list []string, name string) bool {
	if name == "" {
		return false
	}
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if key, _, cut := strings.Cut(entry, "="); cut {
			entry = strings.TrimSpace(key)
		}
		if entry == name {
			return true
		}
	}
	return false
}

func (u *Unit) buildEnvironment() (map[string]string, []string, error) {
	// Deliberate deviation from systemd's minimal default: every unit
	// inherits the daemon's full environment (container/chroot sessions
	// rely on PATH/HOME/proxy flowing through), with Environment= and
	// EnvironmentFile= overlaying on top. Units opt out per-variable
	// with UnsetEnvironment= (parsed above, applied last).
	//
	// PassEnvironment= is the opt-in at the other end, and only a system manager
	// acts on it: a user manager gives every unit its whole environment either
	// way. Naming variables filters the inheritance above down to that list - a
	// name the manager lacks stays absent, an empty list keeps the inheritance.
	// Unlike systemd, a filtered unit gets no default environment beyond those.
	pass := u.GetConfig().Service.PassEnvironment
	filterInherited := u.inSystemScope() && len(pass) > 0
	envMap := map[string]string{}
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || (filterInherited && !passEnvironmentHas(pass, key)) {
			continue
		}
		envMap[key] = value
	}

	// systemd --user always exports XDG_RUNTIME_DIR to its units; a daemon
	// started outside a login shell (no profile.d run, service launcher,
	// container init) has no such variable to inherit, and unit commands
	// then see an empty ${XDG_RUNTIME_DIR}. Guarantee it for the user scope
	// only: system units do not get it upstream. Applied before
	// EnvironmentFile=/Environment= so an explicit assignment still wins.
	if os.Geteuid() != 0 {
		if envMap["XDG_RUNTIME_DIR"] == "" {
			envMap["XDG_RUNTIME_DIR"] = userpaths.UserRuntimeDir()
		}
	}

	for _, entry := range u.GetConfig().Service.EnvironmentFile {
		if err := u.loadEnvironmentFile(entry, envMap); err != nil {
			return nil, nil, err
		}
	}

	for _, entry := range u.GetConfig().Service.Environment {
		parts, err := shlex.Split(entry)
		if err != nil {
			return nil, nil, fmt.Errorf("parse Environment: %w", err)
		}
		for _, assignment := range parts {
			if key, value, ok := strings.Cut(assignment, "="); ok {
				envMap[key] = value
			}
		}
	}

	for _, name := range u.GetConfig().Service.UnsetEnvironment {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// Strip optional VAR= form down to the name.
		if key, _, ok := strings.Cut(name, "="); ok {
			name = strings.TrimSpace(key)
		}
		delete(envMap, name)
	}
	// Computed last, like systemd: the supervisor decides where a unit's
	// RuntimeDirectory= and friends live, Environment= cannot redirect them.
	u.mu.Lock()
	dirEnv := u.managedDirs.env
	u.mu.Unlock()
	for _, assignment := range dirEnv {
		if key, value, ok := strings.Cut(assignment, "="); ok {
			envMap[key] = value
		}
	}
	envList := make([]string, 0, len(envMap))
	for key, value := range envMap {
		envList = append(envList, fmt.Sprintf("%s=%s", key, value))
	}
	return envMap, envList, nil
}

func (u *Unit) loadEnvironmentFile(entry string, envMap map[string]string) error {
	paths, err := shlex.Split(entry)
	if err != nil {
		return fmt.Errorf("parse EnvironmentFile: %w", err)
	}
	for _, path := range paths {
		optional := strings.HasPrefix(path, "-")
		if optional {
			path = strings.TrimPrefix(path, "-")
		}
		file, err := os.Open(path)
		if err != nil {
			if optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			// Systemd allows `export KEY=val` and `KEY=val`; strip the
			// export prefix so the key does not become "export FOO".
			if rest, ok := strings.CutPrefix(line, "export "); ok {
				line = strings.TrimSpace(rest)
			} else if rest, ok := strings.CutPrefix(line, "export\t"); ok {
				line = strings.TrimSpace(rest)
			}
			// Handle tab-separated export as well.
			if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "export" {
				line = strings.TrimSpace(strings.TrimPrefix(line, "export"))
				line = strings.TrimSpace(line)
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" || strings.ContainsAny(key, " \t") {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
				// Single quotes are literal in systemd (no escapes).
				value = value[1 : len(value)-1]
			} else if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			envMap[key] = value
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return err
		}
		_ = file.Close()
	}
	return nil
}

func (u *Unit) waitForPIDFile(timeout time.Duration, poll time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// livePIDFilePID, not readPIDFile+processAlive: a pid file left behind
		// by a dead daemon points at a number the kernel may have handed to an
		// unrelated process, and adopting that made the unit "active" over a
		// process it cannot signal.
		if pid := u.livePIDFilePID(); pid != 0 {
			return pid, nil
		}
		time.Sleep(poll)
	}
	return 0, errors.New("PIDFile not found or process not running")
}

// resolveForkingMainPID decides which process a Type=forking start left
// behind. systemd reads it from the unit cgroup; without one, a configured
// PIDFile is authoritative, and failing that the surviving member of the
// starter's own process group is the daemon. A daemon that setsid()s out of
// the group without writing a pid file is invisible to both - which is why
// SysV scripts on this box ship one.
func (u *Unit) resolveForkingMainPID(timeout, poll time.Duration) (int, error) {
	if strings.TrimSpace(u.GetConfig().Service.PIDFile) != "" {
		return u.waitForPIDFile(timeout, poll)
	}
	deadline := time.Now().Add(timeout)
	for {
		u.mu.Lock()
		starter, pgid := 0, u.pgid
		if u.Cmd != nil && u.Cmd.Process != nil {
			starter = u.Cmd.Process.Pid
		}
		u.mu.Unlock()
		if starter == 0 {
			break
		}
		// The daemon is only identifiable once the process that forked it is
		// gone: until then every member of the group might still be the
		// starter itself.
		if !processAlive(starter) {
			// Membership first: the kernel says who is left in the unit. The
			// process-group scan is the fallback for a box with no group, and
			// for a daemon that escaped it.
			if pid := u.cgroupMemberPID(starter); pid != 0 {
				return pid, nil
			}
			if pid := u.processGroupMemberPID(pgid, starter); pid != 0 {
				return pid, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poll)
	}
	return 0, errors.New("no process left after the forking start")
}

func (u *Unit) readPIDFile() (int, error) {
	path := strings.TrimSpace(u.GetConfig().Service.PIDFile)
	if path == "" {
		return 0, errors.New("PIDFile not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	return strconv.Atoi(pidStr)
}

// waitStatusCode maps a wait status onto systemd's ExecMainCode property:
// 1 = the process exited, 2 = it was killed by a signal, 3 = it dumped core.
func waitStatusCode(status syscall.WaitStatus) int {
	switch {
	case status.CoreDump():
		return 3
	case status.Signaled():
		return 2
	case status.Exited():
		return 1
	}
	return 0
}

func (u *Unit) handleExit(token int, err error, ignoreFailure bool, resetActive bool) {
	// A signaled death still arrives here as an *exec.ExitError, and
	// status.ExitStatus() is -1 for those: route through the wait status so
	// the signal is reported as 128+sig/killed instead of a bogus -1.
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			u.handleExitStatusForPID(token, 0, status, ignoreFailure, resetActive)
			return
		}
	}
	exitCode, mainCode := 0, 0
	if err != nil {
		exitCode, mainCode = 1, 1
	}
	u.handleExitCode(token, 0, exitCode, mainCode, err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitStatus(token int, status syscall.WaitStatus, ignoreFailure bool, resetActive bool) {
	u.handleExitStatusForPID(token, 0, status, ignoreFailure, resetActive)
}

func (u *Unit) handleExitStatusForPID(token int, watchedPID int, status syscall.WaitStatus, ignoreFailure bool, resetActive bool) {
	exitCode := 0
	var err error
	switch {
	case status.Exited():
		exitCode = status.ExitStatus()
		if exitCode != 0 {
			err = fmt.Errorf("exit status %d", exitCode)
		}
	case status.Signaled():
		exitCode = 128 + int(status.Signal())
		err = fmt.Errorf("terminated by signal %s", status.Signal())
	default:
		err = fmt.Errorf("process exited")
		exitCode = 1
	}
	u.handleExitCode(token, watchedPID, exitCode, waitStatusCode(status), err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitCode(token int, watchedPID int, exitCode int, mainCode int, err error, ignoreFailure bool, resetActive bool) {
	defer func() {
		// Every terminal write below is inline, not through transitionState, so
		// this is where a unit that ended by itself hands its group back: nobody
		// is running a stop, and nothing else would ever release the leaf. The
		// branches that adopt a successor end up Active and are filtered here.
		switch u.Snapshot().State {
		case StateInactive, StateFailed:
			u.releaseEmptyCgroup()
		}
	}()
	serviceType := u.canonicalServiceType()
	if serviceType == "notify" && watchedPID != 0 && !u.StopRequested() {
		// Non-blocking adopt check only: the old code waited up to
		// StartTimeout (30s) inside the reaper callback, delaying
		// failure reporting and racing waitNotify's timer. A daemon
		// that forks-then-exits must have its successor visible already
		// (PIDFile or group member); otherwise this exit is real.
		if adoptedPID := u.adoptedNotifyPID(watchedPID); adoptedPID != 0 && adoptedPID != watchedPID {
			u.mu.Lock()
			if u.startToken == token {
				if u.notifyServer != nil {
					u.notifyServer.Stop()
					u.notifyServer = nil
				}
				u.Runtime.State = StateActive
				u.Runtime.MainPID = adoptedPID
				u.Runtime.ExecMainPID = adoptedPID
				u.Runtime.ExitCode = 0
				u.Runtime.LastError = ""
			}
			u.mu.Unlock()
			return
		}
	}

	u.mu.Lock()
	if u.startToken != token {
		u.mu.Unlock()
		return
	}

	// Preserve an earlier failure reason (e.g. ExecStartPost): waitSimple
	// kills the main process after marking Failed, and the reaper's exit
	// for that SIGTERM must not overwrite LastError with "terminated by
	// signal SIGTERM" / 143.
	if u.Runtime.State == StateFailed {
		u.mu.Unlock()
		return
	}

	// Every branch below records how this invocation ended, so the wait code
	// (exited / killed / dumped) belongs with it: systemd renders the pair as
	// "code=exited, status=N" and `show -p ExecMainCode` reads it.
	u.Runtime.MainCode = mainCode

	if u.Runtime.State == StateStopping || u.stopRequested {
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		if resetActive {
			u.Runtime.MainPID = 0
		}
		// A unit that was stopped, rather than one that died, leaves no
		// execution trace behind: upstream zeroes ExecMainPID/Code/Status on a
		// clean stop so `show` reads success for an inactive unit.
		u.Runtime.State = StateInactive
		u.Runtime.LastError = ""
		u.Runtime.ExitCode = 0
		u.Runtime.MainCode = 0
		u.Runtime.ExecMainPID = 0
		u.Runtime.FinishedAt = time.Now()
		u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
		u.mu.Unlock()
		return
	}

	if serviceType == "notify" && watchedPID != 0 {
		if adoptedPID := u.adoptedNotifyPIDWithCurrent(watchedPID, u.Runtime.MainPID); adoptedPID != 0 && adoptedPID != watchedPID {
			if u.notifyServer != nil {
				u.notifyServer.Stop()
				u.notifyServer = nil
			}
			u.Runtime.State = StateActive
			u.Runtime.MainPID = adoptedPID
			u.Runtime.ExecMainPID = adoptedPID
			u.Runtime.ExitCode = 0
			u.Runtime.LastError = ""
			u.mu.Unlock()
			return
		}
		if currentPID := u.Runtime.MainPID; currentPID != 0 && currentPID != watchedPID && processAlive(currentPID) {
			if u.notifyServer != nil {
				u.notifyServer.Stop()
				u.notifyServer = nil
			}
			u.Runtime.State = StateActive
			u.Runtime.ExitCode = 0
			u.Runtime.LastError = ""
			u.mu.Unlock()
			return
		}
	}

	// Respect RestartPreventExitStatus
	if prevent := u.RestartPreventExitStatus(); prevent != nil {
		if _, blocked := prevent[exitCode]; blocked {
			if u.notifyServer != nil {
				u.notifyServer.Stop()
				u.notifyServer = nil
			}
			u.Runtime.State = StateFailed
			if err != nil {
				u.Runtime.LastError = err.Error()
			} else {
				u.Runtime.LastError = fmt.Sprintf("exit status %d", exitCode)
			}
			u.Runtime.ExitCode = exitCode
			u.Runtime.FinishedAt = time.Now()
			u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
			didFail := true
			handler := u.onFailureHandler
			name := u.GetConfig().Name
			u.mu.Unlock()
			if handler != nil {
				go handler(name)
			}
			_ = didFail
			return
		}
	}
	if success := u.SuccessExitStatus(); success != nil {
		if _, ok := success[exitCode]; ok {
			err = nil
			exitCode = 0
		}
	}

	// Cleanup notify socket
	if u.notifyServer != nil {
		u.notifyServer.Stop()
		u.notifyServer = nil
	}

	if u.Runtime.State == StateActive && resetActive {
		u.Runtime.MainPID = 0
	}

	if err != nil {
		if ignoreFailure {
			if u.Runtime.State != StateActive {
				u.Runtime.State = StateInactive
			} else if resetActive {
				u.Runtime.MainPID = 0
				u.Runtime.State = StateInactive
			}
			u.Runtime.LastError = ""
			// A tolerated ("-") failure leaves no trace in the properties:
			// upstream reads ExecMainCode=0 ExecMainStatus=0 for `-/bin/false`,
			// not the process's real exit status.
			exitCode = 0
			u.Runtime.MainCode = 0
		} else if u.Runtime.State == StateActive && resetActive {
			// Simple service that exited with failure -> mark failed so OnFailure triggers
			u.Runtime.State = StateFailed
			u.Runtime.MainPID = 0
			u.Runtime.LastError = err.Error()
		} else if u.Runtime.State != StateActive {
			u.Runtime.State = StateFailed
			u.Runtime.LastError = err.Error()
		} else {
			u.Runtime.State = StateFailed
			u.Runtime.MainPID = 0
			u.Runtime.LastError = err.Error()
		}
		u.Runtime.ExitCode = exitCode
	} else {
		if u.remainAfterExit() {
			// Clean oneshot exit with RemainAfterExit=yes: stay Active
			// with no main PID (systemd's "active (exited)").
			u.Runtime.State = StateActive
			u.Runtime.MainPID = 0
			u.Runtime.LastError = ""
		} else if u.Runtime.State != StateActive {
			u.Runtime.State = StateInactive
		}
		u.Runtime.ExitCode = exitCode
	}

	// A process that ran and left a non-zero status is an exit-code result;
	// everything else this point reaches (clean exit, tolerated failure,
	// RemainAfterExit) reads as success, matching upstream's Result keyword.
	if err != nil && !ignoreFailure {
		u.Runtime.Result = "exit-code"
	} else {
		u.Runtime.Result = "success"
	}
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()

	didFail2 := u.Runtime.State == StateFailed
	handler2 := u.onFailureHandler
	name2 := u.GetConfig().Name
	origErr := err
	u.mu.Unlock()
	if didFail2 && handler2 != nil && origErr != nil {
		go handler2(name2)
	}
}

func (u *Unit) Log(level logging.Level, message string) {
	u.mu.Lock()
	pid := u.Runtime.MainPID
	u.mu.Unlock()
	u.Logs.Add(logging.Entry{
		Timestamp: logging.MonotonicNow(),
		WallTime:  time.Now(),
		Unit:      u.GetConfig().Name,
		PID:       pid,
		Level:     level,
		Message:   message,
	})
}

func (u *Unit) RecordRestart(now time.Time, interval time.Duration) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := now.Add(-interval)
	pruned := u.restartHistory[:0]
	for _, stamp := range u.restartHistory {
		if stamp.After(cutoff) {
			pruned = append(pruned, stamp)
		}
	}
	u.restartHistory = append(pruned, now)
	return len(u.restartHistory)
}

func (u *Unit) MarkFailed(reason string) {
	u.mu.Lock()
	current := u.Runtime.LastError
	u.mu.Unlock()
	if current != "" && current != reason {
		u.transitionState(StateFailed, reason+": "+current)
		return
	}
	u.transitionState(StateFailed, reason)
}

func (u *Unit) ResetFailed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Runtime.State != StateFailed {
		return false
	}
	u.Runtime.State = StateInactive
	u.Runtime.LastError = ""
	u.Runtime.ExitCode = 0
	u.Runtime.FinishedAt = time.Time{}
	u.Runtime.FinishedAtMonotonic = 0
	return true
}

func (u *Unit) IsFailed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Runtime.State == StateFailed
}

func (u *Unit) Kill(sig syscall.Signal) error {
	u.mu.Lock()
	pid := u.Runtime.MainPID
	pgid := u.pgid
	state := u.Runtime.State
	killProcessGroup := !u.killModeProcess()
	u.mu.Unlock()
	if state != StateActive && state != StateActivating {
		return fmt.Errorf("unit not active")
	}
	if pid <= 0 || !processAlive(pid) {
		return fmt.Errorf("no main PID")
	}
	// Route through the self-guarded path (never -PID when that group
	// holds the daemon); a failed direct kill still surfaces an error.
	u.signalUnitPID(pid, pgid, killProcessGroup, sig)
	if pid > 0 && syscall.Kill(pid, 0) != nil {
		// Leader already gone; the group signal (if any) was still sent.
		return nil
	}
	return nil
}

// expandWithEnv applies systemd's Exec* variable substitution: only a plain
// $NAME or ${NAME} is a variable, and an unset one expands to the empty
// string. Anything else - shell constructs like ${NAME:-fallback}, positional
// parameters, bare dollars - is passed through untouched so the child shell
// still sees what the unit author wrote. os.Expand used to swallow
// `${VAR:-default}` whole (its "name" is the entire brace body), which
// silently deleted default values from unit command lines.
//
// $$ is systemd's escape for a literal dollar, matching upstream.
func expandWithEnv(input string, envMap map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(input); {
		if input[i] != '$' {
			out.WriteByte(input[i])
			i++
			continue
		}
		if i+1 < len(input) && input[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(input) && input[i+1] == '{' {
			if end := strings.IndexByte(input[i+2:], '}'); end >= 0 {
				name := input[i+2 : i+2+end]
				width := end + 3
				if isEnvVarName(name) {
					out.WriteString(envMap[name])
				} else {
					// Not a plain variable reference: hand it to the shell.
					out.WriteString(input[i : i+width])
				}
				i += width
				continue
			}
			out.WriteString(input[i:])
			return out.String()
		}
		j := i + 1
		for j < len(input) && isEnvVarByte(input[j], j == i+1) {
			j++
		}
		if j == i+1 {
			// A lone '$' (or "$/" and friends): not a reference.
			out.WriteByte('$')
			i++
			continue
		}
		out.WriteString(envMap[input[i+1:j]])
		i = j
	}
	return out.String()
}

func isEnvVarByte(c byte, first bool) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}

func isEnvVarName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isEnvVarByte(name[i], i == 0) {
			return false
		}
	}
	return true
}

func (u *Unit) expandSpecifiers(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	fullName := u.GetConfig().Name
	prefix := fullName
	instance := ""
	if idx := strings.Index(fullName, "@"); idx >= 0 {
		prefix = fullName[:idx]
		rest := fullName[idx+1:]
		rest = strings.TrimSuffix(rest, ".service")
		rest = strings.TrimSuffix(rest, ".socket")
		instance = rest
		if instance == "." {
			instance = ""
		}
	}
	nameWithoutSuffix := strings.TrimSuffix(fullName, ".service")
	nameWithoutSuffix = strings.TrimSuffix(nameWithoutSuffix, ".socket")
	// %p is the template prefix: for a non-instanced unit that is the name
	// without its suffix, which is %N (verified against systemd 259).
	prefix = strings.TrimSuffix(strings.TrimSuffix(prefix, ".service"), ".socket")
	// user and home for %u/%h
	userName := ""
	homeDir := ""
	if cur, err := user.Current(); err == nil {
		userName = cur.Username
		homeDir = cur.HomeDir
	}
	s = strings.ReplaceAll(s, "%%", "\x00")
	s = strings.ReplaceAll(s, "%n", fullName)
	s = strings.ReplaceAll(s, "%N", nameWithoutSuffix)
	s = strings.ReplaceAll(s, "%p", prefix)
	s = strings.ReplaceAll(s, "%i", instance)
	s = strings.ReplaceAll(s, "%I", instance)
	s = strings.ReplaceAll(s, "%u", userName)
	s = strings.ReplaceAll(s, "%h", homeDir)
	// Directory specifiers follow the same scope rule as the managed
	// directories: the daemon's uid decides the base, so %t under a user
	// manager is /run/user/<uid> and /run under a system one.
	runBase, stateBase, cacheBase, logsBase, _ := u.directoryBases()
	s = strings.ReplaceAll(s, "%t", runBase)
	s = strings.ReplaceAll(s, "%S", stateBase)
	s = strings.ReplaceAll(s, "%C", cacheBase)
	s = strings.ReplaceAll(s, "%L", logsBase)
	s = strings.ReplaceAll(s, "\x00", "%")
	return s
}

func stripPrefix(command string) (string, bool) {
	ignoreFailure := false
	for {
		command = strings.TrimSpace(command)
		switch {
		case strings.HasPrefix(command, "-"):
			ignoreFailure = true
			command = strings.TrimPrefix(command, "-")
		case strings.HasPrefix(command, "@"):
			command = strings.TrimPrefix(command, "@")
		case strings.HasPrefix(command, ":"):
			command = strings.TrimPrefix(command, ":")
		case strings.HasPrefix(command, "+"):
			command = strings.TrimPrefix(command, "+")
		case strings.HasPrefix(command, "!!"):
			command = strings.TrimPrefix(command, "!!")
		case strings.HasPrefix(command, "!"):
			command = strings.TrimPrefix(command, "!")
		default:
			return command, ignoreFailure
		}
	}
}

// processAlive reports whether a PID is still a running process. It answers
// false only when the process is certainly gone: a PID we cannot look at
// (hidepid=2, another user) counts as alive, because a supervisor that called
// it dead would report "stopped" over a process it could neither signal nor
// watch. Callers that must not adopt an invisible process - external and
// main-PID detection - ask procOwnedBy instead.
func processAlive(pid int) bool {
	return checkLiveness(pid).notDead()
}

func (u *Unit) canonicalServiceType() string {
	serviceType := strings.ToLower(strings.TrimSpace(u.GetConfig().Service.Type))

	switch serviceType {
	case "", "simple":
		return "simple"

	case "forking", "oneshot", "idle", "exec":
		return serviceType

	case "notify", "notify-reload":
		return "notify"

	case "dbus":
		u.Log(logging.LevelInfo, "DBus type treated as simple")
		return "simple"

	default:
		u.Log(logging.LevelError, fmt.Sprintf("Unsupported service type %q; treating as simple", serviceType))
		return "simple"
	}
}

func (u *Unit) transitionState(next State, reason string) {
	u.applyStateTransition(next, reason)
	// Every end of a unit's run passes through here, which is the only place
	// that can notice a death nobody asked for: a stop releases the group, a
	// crash released nothing, and the empty leaf outlived the process.
	if next == StateInactive || next == StateFailed {
		u.releaseEmptyCgroup()
	}
}

// applyStateTransition is the locked half of transitionState. The group is
// released after the lock is down, never under it.
func (u *Unit) applyStateTransition(next State, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	// Any path to inactive releases the PID. Previously only Active→Inactive
	// cleared it, so a stop that raced the reaper could leave MainPID set
	// on an inactive unit (or a stopping unit reporting a stale PID).
	if next == StateInactive {
		u.Runtime.MainPID = 0
		u.pgid = 0
	}
	u.Runtime.State = next
	if reason != "" {
		u.Runtime.LastError = reason
	}
	switch next {
	case StateFailed:
		// Upstream keeps Result a keyword; "terminated after timeout" is the
		// only failure this path invents and it maps to a distinct keyword.
		if strings.Contains(reason, "timeout") {
			u.Runtime.Result = "timeout"
		} else {
			u.Runtime.Result = "exit-code"
		}
	case StateActive:
		u.Runtime.Result = "success"
	}
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
}

// skipStart parks a unit that a Condition= or ExecCondition= decided not to
// run: inactive, no error, and a success result, so it reads the same as a
// unit that was simply never started rather than as one that failed.
func (u *Unit) skipStart() {
	u.transitionState(StateInactive, "")
	u.mu.Lock()
	u.Runtime.LastError = ""
	u.Runtime.Result = "success"
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
	u.mu.Unlock()
}

// markTimeout fails a unit whose start ran out of TimeoutStartSec. Upstream
// keeps Result as a keyword and `systemctl start` renders it as "failed
// because a timeout was exceeded", so reusing markFailed's exit-code default
// would misreport why the job died. Every keyword is written with the state in
// one critical section: this path kills the process, and a two-step write let
// that exit land in between and rewrite Result back to exit-code.
func (u *Unit) markTimeout(err error, ignoreFailure bool) {
	if ignoreFailure {
		u.transitionState(StateInactive, "")
		return
	}
	u.mu.Lock()
	u.Runtime.State = StateFailed
	u.Runtime.LastError = err.Error()
	u.Runtime.Result = "timeout"
	// The timeout terminates the process with the stop signal, so upstream
	// reports a signaled death: code=killed with the signal as the status.
	u.Runtime.MainCode = 2
	u.Runtime.ExitCode = int(u.stopSignal())
	u.Runtime.MainPID = 0
	u.pgid = 0
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
	u.mu.Unlock()
	u.releaseEmptyCgroup()
}

func (u *Unit) markFailed(err error, ignoreFailure bool) {
	if ignoreFailure {
		u.transitionState(StateInactive, "")
		return
	}
	u.mu.Lock()
	u.Runtime.State = StateFailed
	u.Runtime.LastError = err.Error()
	u.Runtime.Result = "exit-code"
	u.Runtime.ExitCode = 1
	u.Runtime.MainPID = 0
	u.pgid = 0
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
	u.mu.Unlock()
	// markFailed writes the state itself rather than through transitionState,
	// so it owes the group back here as well.
	u.releaseEmptyCgroup()
}

// execFailureStatus maps a spawn that never happened onto the exit status
// systemd invents for it: 200 when the working directory could not be entered,
// 203 when the program could not be exec'd at all, 216 when the failure is a
// refused credential switch (User=/Group= as an unprivileged manager).
// `systemctl status` renders these as "status=200/CHDIR", "status=203/EXEC" and
// "status=216/GROUP"; scripts key on the number.
func (u *Unit) execFailureStatus(err error) int {
	svc := u.GetConfig().Service
	creds := strings.TrimSpace(svc.User) != "" || strings.TrimSpace(svc.Group) != ""
	msg := err.Error()
	// Go performs WorkingDirectory= as a chdir in the child; a failure there
	// surfaces as an *os.PathError with Op "chdir", distinct from the
	// "fork/exec" of a missing binary.
	if strings.Contains(msg, "chdir") {
		return 200
	}
	if creds && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) ||
		strings.Contains(msg, "operation not permitted")) {
		return 216
	}
	return 203
}

// recordExecFailure is markFailed for a main process that was never spawned,
// carrying the manager-invented status instead of a generic 1.
func (u *Unit) recordExecFailure(err error, ignoreFailure bool) {
	status := u.execFailureStatus(err)
	u.markFailed(err, ignoreFailure)
	if ignoreFailure {
		return
	}
	u.mu.Lock()
	if u.Runtime.State == StateFailed {
		u.Runtime.ExitCode = status
		// systemd's helper process is the one that "exits" with the invented
		// status, so the wait code is a plain exit.
		u.Runtime.MainCode = 1
	}
	u.mu.Unlock()
}

// directoryBases resolves Runtime/State/Cache/Logs/Configuration roots for
// the manager scope. System daemons (root) use the FHS paths; anything else
// is a user manager that cannot create /run or /var/*, so the XDG bases are
// used instead - the same ones systemd uses, so a unit keeps the state
// directory it already wrote. The daemon UID decides, not per-unit User=
// (which only affects the child credentials, not where the supervisor may
// write).
func (u *Unit) directoryBases() (run, state, cache, logs, config string) {
	if os.Geteuid() == 0 {
		return "/run", "/var/lib", "/var/cache", "/var/log", "/etc"
	}
	state = userpaths.UserStateBase()
	return userpaths.UserRuntimeDir(), state, userpaths.UserCacheBase(), filepath.Join(state, "log"), userpaths.UserConfigHome()
}

func (u *Unit) ensureManagedDirectories() error {
	runBase, stateBase, cacheBase, logsBase, configBase := u.directoryBases()
	runtime, err := u.ensureNamedDirectories(runBase, u.GetConfig().Service.RuntimeDirectory, u.GetConfig().Service.RuntimeDirectoryMode)
	if err != nil {
		return err
	}
	state, err := u.ensureNamedDirectories(stateBase, u.GetConfig().Service.StateDirectory, "0755")
	if err != nil {
		return err
	}
	cache, err := u.ensureNamedDirectories(cacheBase, u.GetConfig().Service.CacheDirectory, "0755")
	if err != nil {
		return err
	}
	logs, err := u.ensureNamedDirectories(logsBase, u.GetConfig().Service.LogsDirectory, "0755")
	if err != nil {
		return err
	}
	config, err := u.ensureNamedDirectories(configBase, u.GetConfig().Service.ConfigurationDirectory, "0755")
	if err != nil {
		return err
	}
	// systemd exports these as computed variables after Environment=, so a
	// unit cannot override where its own directories are. Multiple names are
	// colon-joined in one value.
	var env []string
	for _, entry := range []struct {
		key   string
		paths []string
	}{
		{"RUNTIME_DIRECTORY", runtime},
		{"STATE_DIRECTORY", state},
		{"CACHE_DIRECTORY", cache},
		{"LOGS_DIRECTORY", logs},
		{"CONFIGURATION_DIRECTORY", config},
	} {
		if len(entry.paths) > 0 {
			env = append(env, entry.key+"="+strings.Join(entry.paths, ":"))
		}
	}
	u.mu.Lock()
	u.managedDirs = managedDirectories{runtime: runtime, env: env}
	u.mu.Unlock()
	return nil
}

// removeManagedRuntimeDirectories undoes the /run half of a start. State,
// cache, logs and configuration directories survive a stop (they are the
// point of those settings) but runtime ones do not, unless
// RuntimeDirectoryPreserve=yes asked for them to. A crash is not a stop:
// systemd leaves the directory of a service that died for its restart to
// find, and initd does the same.
func (u *Unit) removeManagedRuntimeDirectories() {
	u.mu.Lock()
	paths, keep := u.managedDirs.runtime, u.keepDirsOnStop
	u.keepDirsOnStop = false
	u.managedDirs = managedDirectories{}
	u.mu.Unlock()

	value := strings.TrimSpace(u.GetConfig().Service.RuntimeDirectoryPreserve)
	if keep || value == "yes" || value == "true" || value == "1" {
		return
	}
	for _, path := range paths {
		_ = os.RemoveAll(path)
	}
}

// KeepDirectoriesForRestart marks the unit as stopped in order to be started
// again, which is all RuntimeDirectoryPreserve=restart asks for.
func (u *Unit) KeepDirectoriesForRestart() {
	u.mu.Lock()
	u.keepDirsOnStop = true
	u.mu.Unlock()
}

func (u *Unit) ensureNamedDirectories(base string, names []string, modeStr string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	mode, err := parseFileMode(modeStr, 0o755)
	if err != nil {
		return nil, err
	}
	creds, err := u.resolveCredentialsForStart(false)
	if err != nil {
		return nil, err
	}
	created := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.Contains(name, "\\") {
			return nil, fmt.Errorf("invalid directory name %q", name)
		}
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == "." {
			return nil, fmt.Errorf("invalid directory name %q", name)
		}
		if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.HasSuffix(clean, "/..") || clean == ".." {
			return nil, fmt.Errorf("invalid directory name %q", name)
		}
		for _, part := range strings.Split(clean, "/") {
			if part == "." || part == ".." || part == "" {
				return nil, fmt.Errorf("invalid directory name %q", name)
			}
		}
		// Accept harmless normalizations (trailing slash, ./x, x//y):
		// validate `clean` and use it for the join.
		path := filepath.Join(base, clean)
		if err := os.MkdirAll(path, mode); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return nil, err
		}
		if creds.set {
			if err := os.Chown(path, int(creds.uid), int(creds.gid)); err != nil {
				return nil, fmt.Errorf("chown directory %s: %w", path, err)
			}
		}
		created = append(created, path)
	}
	return created, nil
}

func (u *Unit) runExecStartPre(token int, envMap map[string]string, envList []string) error {
	timeout := u.StartTimeout()
	for _, command := range u.GetConfig().Service.ExecStartPre {
		if !u.isCurrentToken(token) {
			return nil
		}
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.GetConfig().Service.PermissionsStartOnly}, timeout); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runExecCondition() (string, error) {
	if len(u.GetConfig().Service.ExecCondition) == 0 {
		return "continue", nil
	}
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return "", err
	}
	for _, command := range u.GetConfig().Service.ExecCondition {
		if strings.TrimSpace(command) == "" {
			continue
		}
		status, err := u.runCommandStatus(command, envMap, envList, commandOptions{rootOnly: u.GetConfig().Service.PermissionsStartOnly}, u.StartTimeout())
		if err != nil {
			return "", err
		}
		switch {
		case status == 0:
			continue
		case status >= 1 && status <= 254:
			return "skip", nil
		default:
			return "", fmt.Errorf("ExecCondition failed with status %d", status)
		}
	}
	return "continue", nil
}

func (u *Unit) runExecStartPost(token int, envMap map[string]string, envList []string) error {
	timeout := u.StartTimeout()
	for _, command := range u.GetConfig().Service.ExecStartPost {
		if !u.isCurrentToken(token) {
			return nil
		}
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.GetConfig().Service.PermissionsStartOnly}, timeout); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runExecStopPost() error {
	timeout := u.StopTimeout()
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}
	for _, command := range u.GetConfig().Service.ExecStopPost {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.GetConfig().Service.PermissionsStartOnly}, timeout); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runCommand(command string, envMap map[string]string, envList []string, opts commandOptions, timeout time.Duration) error {
	status, err := u.runCommandStatus(command, envMap, envList, opts, timeout)
	command, ignoreFailure := stripPrefix(command)
	if err != nil {
		if ignoreFailure {
			return nil
		}
		return err
	}
	if status != 0 {
		if ignoreFailure {
			return nil
		}
		return fmt.Errorf("exit status %d", status)
	}
	return nil
}

func (u *Unit) runCommandStatus(command string, envMap map[string]string, envList []string, opts commandOptions, timeout time.Duration) (int, error) {
	command, ignoreFailure := stripPrefix(command)
	command = u.expandSpecifiers(command)
	expanded := expandWithEnv(command, envMap)
	args, err := shlex.Split(expanded)
	if err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, fmt.Errorf("parse command: %w", err)
	}
	if len(args) == 0 {
		return 0, nil
	}
	cmd, err := u.buildExecCommand(args, opts)
	if err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, err
	}
	stdoutLogger := &logging.LineLogger{Unit: u.GetConfig().Name, PID: 0, Level: logging.LevelInfo, Buffer: u.Logs, Output: os.Stdout}
	stderrLogger := &logging.LineLogger{Unit: u.GetConfig().Name, PID: 0, Level: logging.LevelError, Buffer: u.Logs, Output: os.Stderr}
	u.configureCommand(cmd, envList, stdoutLogger, stderrLogger)
	if err := cmd.Start(); err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, err
	}
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid
	// systemd runs ExecStartPre and ExecStopPost inside the unit's cgroup, so a
	// KillMode that sweeps the group is meant to reach them too.
	if cmd.Process != nil {
		u.placeInCgroup(cmd.Process.Pid)
	}
	// Helpers honor the unit's start/stop timeout instead of waiting forever:
	// a stuck ExecStartPre must fail the start, not wedge the unit in
	// activating. timeout <= 0 means systemd "infinity" (wait unbounded).
	killHelper := func() {
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		if pid > 0 {
			// Helpers run in their own process group (Setpgid); take the
			// whole group so orphaned grandchildren don't linger.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	timeoutErr := func() error {
		if ignoreFailure {
			return nil
		}
		return fmt.Errorf("command timed out after %s: %s", timeout, command)
	}
	if u.reaper != nil {
		done := make(chan syscall.WaitStatus, 1)
		u.reaper.Register(cmd.Process.Pid, func(status syscall.WaitStatus) {
			select {
			case done <- status:
			default:
			}
		})
		if timeout <= 0 {
			status := <-done
			return commandExitStatus(status), nil
		}
		select {
		case status := <-done:
			return commandExitStatus(status), nil
		case <-time.After(timeout):
			killHelper()
			if ignoreFailure {
				return 0, nil
			}
			return 0, timeoutErr()
		}
	}
	type waitRes struct {
		status int
		err    error
	}
	resCh := make(chan waitRes, 1)
	go func() {
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					resCh <- waitRes{status: commandExitStatus(status)}
					return
				}
			}
			resCh <- waitRes{err: err}
			return
		}
		resCh <- waitRes{}
	}()
	if timeout <= 0 {
		res := <-resCh
		if res.err != nil {
			if ignoreFailure {
				return 0, nil
			}
			return 0, res.err
		}
		return res.status, nil
	}
	select {
	case res := <-resCh:
		if res.err != nil {
			if ignoreFailure {
				return 0, nil
			}
			return 0, res.err
		}
		return res.status, nil
	case <-time.After(timeout):
		killHelper()
		if ignoreFailure {
			return 0, nil
		}
		return 0, timeoutErr()
	}
}

func (u *Unit) buildExecCommand(args []string, opts commandOptions) (*exec.Cmd, error) {
	if len(args) == 0 {
		return nil, errors.New("command parsed to empty")
	}
	// systemd does not create WorkingDirectory= for a service, and entering a
	// missing one fails the spawn with 200/CHDIR - distinct from the 203 a
	// missing binary gets. Go performs the chdir in the child and reports the
	// resulting errno as a "fork/exec" failure of the program path, so the
	// distinction is lost at cmd.Start; check it here instead. Skipped under
	// RootDirectory=, where the path is relative to the chroot.
	if wd := strings.TrimSpace(u.GetConfig().Service.WorkingDirectory); wd != "" &&
		strings.TrimSpace(u.GetConfig().Service.RootDirectory) == "" {
		dir := u.expandSpecifiers(wd)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("chdir %s: no such file or directory", dir)
		}
	}
	umask := strings.TrimSpace(u.GetConfig().Service.UMask)
	limitNOFILE := strings.TrimSpace(u.GetConfig().Service.LimitNOFILE)
	var cmd *exec.Cmd
	if umask != "" || limitNOFILE != "" {
		// Validate before spawning: a malformed value must fail the start
		// instead of running silently without the requested setting.
		if umask != "" {
			if _, err := validateUMask(umask); err != nil {
				return nil, err
			}
		}
		if limitNOFILE != "" {
			if _, err := validateLimitNOFILE(limitNOFILE); err != nil {
				return nil, err
			}
		}
		// NOTE: configuring either directive routes exec through /bin/sh,
		// changing the immediate child from the service binary to a shell
		// that sets up and execs (same PID after exec, but an extra fork
		// and shell signal semantics before it). A Linux-specific pre-exec
		// setup (prctl/setrlimit in the child) would avoid the wrapper;
		// until then failures abort via && so they can never be silent.
		setup := make([]string, 0, 2)
		if umask != "" {
			setup = append(setup, fmt.Sprintf("umask %s", umask))
		}
		if limitNOFILE != "" {
			setup = append(setup, fmt.Sprintf("ulimit -n %s", limitNOFILE))
		}
		setup = append(setup, `exec "$@"`)
		shellArgs := []string{"-c", strings.Join(setup, " && "), "_"}
		shellArgs = append(shellArgs, args...)
		cmd = exec.Command("/bin/sh", shellArgs...)
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}

	creds, err := u.resolveCredentialsForStart(opts.rootOnly)
	if err != nil {
		return nil, err
	}
	sysProcAttr := &syscall.SysProcAttr{Setpgid: true}
	if creds.set {
		sysProcAttr.Credential = &syscall.Credential{Uid: creds.uid, Gid: creds.gid, Groups: creds.groups}
	}
	if rootDir := strings.TrimSpace(u.GetConfig().Service.RootDirectory); rootDir != "" {
		sysProcAttr.Chroot = rootDir
	}
	cmd.SysProcAttr = sysProcAttr
	cmd.Dir = u.workingDirectory()
	return cmd, nil
}

func (u *Unit) SetSocketActivation(files []*os.File, env map[string]string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	// Whatever is already here was never claimed by a start, so this is the
	// last place that can still close it: each trigger dups the listener's fd,
	// and dropping the previous set on the floor is a descriptor per attempt.
	for _, f := range u.socketFiles {
		_ = f.Close()
	}
	u.socketFiles = files
	u.socketEnv = env
}

func (u *Unit) takeSocketActivation() ([]*os.File, map[string]string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	files := u.socketFiles
	env := u.socketEnv
	u.socketFiles = nil
	u.socketEnv = nil
	return files, env
}

func (u *Unit) configureCommand(cmd *exec.Cmd, envList []string, stdoutLogger, stderrLogger *logging.LineLogger) {
	extra := map[string]string{
		"MAINPID": strconv.Itoa(u.mainPID()),
	}
	u.mu.Lock()
	for k, v := range u.socketEnv {
		extra[k] = v
	}
	u.mu.Unlock()
	cmd.Env = mergeEnvList(envList, extra)
	cmd.Stdout = stdoutLogger
	cmd.Stderr = stderrLogger
}

// checkConditions reports whether any Condition= is unmet. An unmet condition
// is not a failure upstream: the start is skipped and the unit is left
// inactive, so this only answers yes/no and lets the caller take the skip
// path (as ExecCondition's non-zero exit already does).
func (u *Unit) checkConditions() bool {
	for _, condition := range u.GetConfig().ConditionPathExists {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		negated := strings.HasPrefix(condition, "!")
		path := strings.TrimPrefix(condition, "!")
		_, err := os.Stat(path)
		exists := err == nil
		if negated {
			exists = !exists
		}
		if !exists {
			u.Log(logging.LevelInfo, fmt.Sprintf("ConditionPathExists=%s was not met, skipping start", condition))
			return true
		}
	}
	return false
}

func (u *Unit) killModeProcess() bool {
	mode := strings.ToLower(strings.TrimSpace(u.GetConfig().Service.KillMode))
	// Only the group flavors take the whole process group. Everything else
	// — process, mixed, none, unset — kills the main PID only.
	//
	// mixed is the first half of what upstream means: SIGTERM to the main
	// process now, and whatever is left in the unit's cgroup is killed by
	// finishCgroupStop when the stop ends. With no cgroup on this box that
	// second half has nothing to enumerate, so a mixed unit's children can
	// still outlive its main process there.
	switch mode {
	case "control-group", "controlgroup", "control_group":
		return false
	default:
		return true
	}
}

// stopSignal resolves KillSignal= (SIGTERM default). Accepts names with or
// without the SIG prefix and bare numbers, mirroring systemctl kill.
func (u *Unit) stopSignal() syscall.Signal {
	raw := strings.TrimSpace(u.GetConfig().Service.KillSignal)
	if raw == "" {
		return syscall.SIGTERM
	}
	stripped := strings.TrimPrefix(raw, "-")
	if n, err := strconv.Atoi(stripped); err == nil && n > 0 {
		return syscall.Signal(n)
	}
	upper := strings.ToUpper(stripped)
	if !strings.HasPrefix(upper, "SIG") {
		upper = "SIG" + upper
	}
	if sig, ok := signalByName(upper); ok {
		return sig
	}
	return syscall.SIGTERM
}

func signalByName(name string) (syscall.Signal, bool) {
	switch name {
	case "SIGHUP":
		return syscall.SIGHUP, true
	case "SIGINT":
		return syscall.SIGINT, true
	case "SIGQUIT":
		return syscall.SIGQUIT, true
	case "SIGTERM":
		return syscall.SIGTERM, true
	case "SIGUSR1":
		return syscall.SIGUSR1, true
	case "SIGUSR2":
		return syscall.SIGUSR2, true
	case "SIGKILL":
		return syscall.SIGKILL, true
	case "SIGSTOP":
		return syscall.SIGSTOP, true
	case "SIGCONT":
		return syscall.SIGCONT, true
	case "SIGWINCH":
		return syscall.SIGWINCH, true
	}
	return 0, false
}

func (u *Unit) killMainProcess(sig syscall.Signal) {
	u.mu.Lock()
	cmd := u.Cmd
	pgid := u.pgid
	mainPID := u.Runtime.MainPID
	killProcessGroup := !u.killModeProcess()
	u.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		// No starter process (adopted PIDFile/notify unit, already
		// reaped, etc.): signal the recorded main PID directly so
		// KillSignal still reaches the daemon.
		if mainPID > 0 {
			u.signalUnitPID(mainPID, pgid, killProcessGroup, sig)
		}
		return
	}

	pid := cmd.Process.Pid
	// Prefer the recorded MainPID when it differs: the starter (sh
	// wrapper, double-fork parent) may not be the supervised daemon,
	// and killing its group can hit siblings — including the daemon
	// itself when groups are shared (see group-collapse below).
	target := mainPID
	if target <= 0 || target == cmd.Process.Pid {
		target = pid
	}
	u.signalUnitPID(target, pgid, killProcessGroup, sig)
}

func (u *Unit) mainPID() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Runtime.MainPID
}

func (u *Unit) isCurrentToken(token int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.startToken == token
}

func (u *Unit) IsCurrentToken(token int) bool {
	return u.isCurrentToken(token)
}

func (u *Unit) StopRequested() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.stopRequested
}

func (u *Unit) RestartPreventExitStatus() map[int]struct{} {
	return parseExitStatusSet(u.GetConfig().Service.RestartPreventExitStatus)
}

// remainAfterExit reports whether a clean oneshot exit should leave the unit
// Active (systemd's RemainAfterExit=yes). Only meaningful for oneshot; other
// types ignore the setting just like systemd does. The type is read raw
// (not via canonicalServiceType) so probing never emits duplicate
// "unsupported type" log lines on the exit path.
func (u *Unit) remainAfterExit() bool {
	if strings.ToLower(strings.TrimSpace(u.GetConfig().Service.Type)) != "oneshot" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(u.GetConfig().Service.RemainAfterExit)) {
	case "yes", "true", "1", "on":
		return true
	default:
		return false
	}
}

// RemainActive reports whether the unit is kept Active after its process
// exited cleanly (oneshot + RemainAfterExit=yes). Display layers use it for
// the "active (exited)" state.
func (u *Unit) RemainActive() bool {
	snap := u.Snapshot()
	return snap.State == StateActive && snap.MainPID == 0 && u.remainAfterExit()
}

// securityNotes maps silently-dropped sandbox/hardening directives to a
// one-line plain-language explanation. Only directives the parser actually
// records in Ignored are listed; anything else is a code bug, not a warn.
var securityNotes = map[string]string{
	"PrivateTmp":              "runs with the shared /tmp (no private mount namespace)",
	"PrivateDevices":          "runs with host /dev visible (no device namespace)",
	"PrivateUsers":            "runs without a user namespace remap",
	"ProtectSystem":           "runs without read-only /usr//etc remounts",
	"ProtectHome":             "runs with home directories fully visible",
	"ProtectKernelTunables":   "runs without /sys//proc hardening",
	"ProtectKernelModules":    "runs without kernel module load blocking",
	"ProtectKernelLogs":       "runs without kernel log access blocking",
	"ProtectClock":            "runs without clock write blocking",
	"ProtectHostname":         "runs without hostname change blocking",
	"ProtectControlGroups":    "runs without cgroup write blocking",
	"RestrictNamespaces":      "runs without namespace creation blocking",
	"RestrictAddressFamilies": "runs without socket family filtering",
	"RestrictRealtime":        "runs without realtime scheduling blocking",
	"RestrictSUIDSGID":        "runs without setuid/setgid blocking",
	"SystemCallFilter":        "runs without syscall filtering",
	"SystemCallArchitectures": "runs without syscall arch filtering",
	"CapabilityBoundingSet":   "runs without capability bounding",
	"AmbientCapabilities":     "ambient capabilities are not granted",
	"NoNewPrivileges":         "runs without the no-new-privileges flag",
	"MemoryDenyWriteExecute":  "runs without W^X memory enforcement",
	"LockPersonality":         "runs without personality lock",
	"RemoveIPC":               "IPC objects are not cleaned up on stop",
	"DeviceAllow":             "runs without device allowlisting",
	"DeviceDeny":              "runs without device denylisting",
}

// IgnoredSecurityNotes returns sorted human-readable warnings for the
// sandbox/hardening directives a unit requested but initd does not enforce.
// Resource-control directives (MemoryMax, CPUQuota, ...) are intentionally
// left out: they are inert accounting knobs, not promises of isolation.
func (u *Unit) IgnoredSecurityNotes() []string {
	return IgnoredSecurityNotes(u.GetConfig().Ignored)
}

// IgnoredSecurityNotes renders warnings for a raw Ignored map without
// needing a live unit, so manager layers can warn before supervision.
func IgnoredSecurityNotes(ignored map[string]string) []string {
	notes := []string{}
	for key := range ignored {
		short := key
		if i := strings.LastIndexByte(key, '.'); i >= 0 {
			short = key[i+1:]
		}
		if explain, ok := securityNotes[short]; ok {
			notes = append(notes, fmt.Sprintf("%s is not enforced (%s)", short, explain))
		}
	}
	if len(notes) > 1 {
		for i := 0; i < len(notes)-1; i++ {
			for j := i + 1; j < len(notes); j++ {
				if notes[j] < notes[i] {
					notes[i], notes[j] = notes[j], notes[i]
				}
			}
		}
	}
	return notes
}

// warnIgnoredDirectives logs one line per unenforced hardening directive.
// The caller passes the Ignored snapshot taken before locking so the mutex
// is never held while writing to the log buffer.
func (u *Unit) warnIgnoredDirectives(ignored map[string]string) {
	for _, note := range IgnoredSecurityNotes(ignored) {
		u.Log(logging.LevelInfo, "Warning: "+note)
	}
}

// StatePair maps the internal state machine onto the two axes systemd
// reports: ActiveState (inactive/activating/active/deactivating/failed) and
// SubState (dead/start/running/exited/stop/failed). Callers that echo
// ActiveState into SubState make a running process report "active", so tools
// filtering on "running"/"exited"/"dead" silently match nothing.
func StatePair(eff State, remainActive bool) (string, string) {
	switch eff {
	case StateActive:
		if remainActive {
			// Oneshot kept alive by RemainAfterExit: active, but no process.
			return "active", "exited"
		}
		return "active", "running"
	case StateActivating:
		return "activating", "start"
	case StateStopping:
		return "deactivating", "stop"
	case StateFailed:
		return "failed", "failed"
	default:
		return "inactive", "dead"
	}
}

// SubState refines Active for display: oneshot units kept alive by
// RemainAfterExit report "exited" (systemd's active (exited)), everything
// else mirrors the raw state.
func (u *Unit) SubState() State {
	_, sub := StatePair(u.Snapshot().State, u.RemainActive())
	return State(sub)
}

// StartLimit reads the unit's StartLimitIntervalSec/StartLimitBurst,
// defaulting to systemd's 10s window with a burst of 5. An interval <= 0
// disables rate limiting; a burst <= 0 means no cap inside the window.
func (u *Unit) StartLimit() (time.Duration, int) {
	interval := parseSystemdDuration(u.GetConfig().StartLimitIntervalSec, 10*time.Second)
	if interval <= 0 {
		return 0, 0
	}
	burst := 5
	if raw := strings.TrimSpace(u.GetConfig().StartLimitBurst); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			burst = n
		}
	}
	return interval, burst
}

// StartLimitBurstValue exposes the burst for D-Bus/systemd property
// consumers, falling back to the default when unset or unparsable.
func (u *Unit) StartLimitBurstValue() uint32 {
	_, burst := u.StartLimit()
	if burst <= 0 {
		return 5
	}
	return uint32(burst)
}

// StartLimitIntervalUsec exposes the window in microseconds for D-Bus
// consumers, falling back to the 10s default.
func (u *Unit) StartLimitIntervalUsec() uint64 {
	interval, _ := u.StartLimit()
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return uint64(interval.Microseconds())
}

// restartBaseDelay is the RestartSec delay before any backoff growth.
func (u *Unit) restartBaseDelay() time.Duration {
	return parseSystemdDuration(u.GetConfig().Service.RestartSec, 0)
}

// restartDelay returns the delay before restart attempt n (1-based): the
// base RestartSec doubled per attempt while RestartSteps allows, capped at
// RestartMaxDelaySec (which defaults to the base, i.e. a fixed delay).
func (u *Unit) RestartDelay(attempt int) time.Duration {
	base := u.restartBaseDelay()
	if base <= 0 || attempt <= 1 {
		return base
	}
	steps := 0
	if raw := strings.TrimSpace(u.GetConfig().Service.RestartSteps); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			steps = n
		}
	}
	maxDelay := parseSystemdDuration(u.GetConfig().Service.RestartMaxDelaySec, base)
	if maxDelay <= 0 {
		maxDelay = base
	}
	delay := base
	for i := 1; i < attempt && (steps <= 0 || i <= steps); i++ {
		if delay >= maxDelay {
			return maxDelay
		}
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	return delay
}

// exitedCleanly reports whether an exit code counts as success: 0 or an
// explicitly listed SuccessExitStatus. Signal deaths surface as 128+signo
// via commandExitStatus, so they never count as clean.
func (u *Unit) exitedCleanly(exitCode int) bool {
	if exitCode == 0 {
		return true
	}
	_, ok := u.SuccessExitStatus()[exitCode]
	return ok
}

// shouldRestart maps Restart= modes onto an exit code. on-abnormal is
// approximated as death by signal (codes above 128); watchdog/abort modes
// have no trigger source here and never restart.
func (u *Unit) ShouldRestart(mode string, exitCode int) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		return true
	case "on-failure":
		return !u.exitedCleanly(exitCode)
	case "on-success":
		return u.exitedCleanly(exitCode)
	case "on-abnormal":
		return exitCode > 128
	default:
		return false
	}
}

func (u *Unit) StopTimeout() time.Duration {
	raw := strings.TrimSpace(u.GetConfig().Service.TimeoutStopSec)
	if raw == "" {
		raw = strings.TrimSpace(u.GetConfig().Service.TimeoutSec)
	}
	return parseSystemdDuration(raw, 10*time.Second)
}

func (u *Unit) StartTimeout() time.Duration {
	raw := strings.TrimSpace(u.GetConfig().Service.TimeoutStartSec)
	if raw == "" {
		raw = strings.TrimSpace(u.GetConfig().Service.TimeoutSec)
	}
	return parseSystemdDuration(raw, 30*time.Second)
}

func waitStatusError(status syscall.WaitStatus) error {
	switch {
	case status.Exited():
		if status.ExitStatus() != 0 {
			return fmt.Errorf("exit status %d", status.ExitStatus())
		}
		return nil
	case status.Signaled():
		return fmt.Errorf("terminated by signal %s", status.Signal())
	default:
		return fmt.Errorf("process exited")
	}
}

func commandExitStatus(status syscall.WaitStatus) int {
	switch {
	case status.Exited():
		return status.ExitStatus()
	case status.Signaled():
		return 128 + int(status.Signal())
	default:
		return 1
	}
}

// livePIDFilePID reads PIDFile= and returns the number only if it can be this
// unit's daemon. A pid file is a stale-number generator by nature: the daemon
// that wrote it may be gone and the kernel may have handed its number to an
// unrelated process, which must not be adopted as the main PID.
//
// Three answers reject it, and none of them is "is the owner the unit's user" -
// a daemon that drops privileges after writing its file is still the daemon:
//   - the process is gone;
//   - the process exists but we cannot signal it (unknown liveness), so
//     supervising it could never end in a stop;
//   - the process began long after the file was last written, so it did not
//     write it.
func (u *Unit) livePIDFilePID() int {
	pid, err := u.readPIDFile()
	if err != nil || pid <= 0 {
		return 0
	}
	if liveness := checkLiveness(pid); liveness != livenessAlive {
		return 0
	}
	if pidFileNamesFreshProcess(strings.TrimSpace(u.GetConfig().Service.PIDFile), pid) {
		return 0
	}
	return pid
}

func (u *Unit) waitForLivePIDFile(timeout time.Duration, poll time.Duration) int {
	if strings.TrimSpace(u.GetConfig().Service.PIDFile) == "" {
		return 0
	}
	deadline := time.Now().Add(timeout)
	for {
		if pid := u.livePIDFilePID(); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(poll)
	}
}

// processGroupMemberPID returns a live process this unit owns that still sits
// in process group pgid, excluding one PID. It is how a Type=forking start and
// an adopted notify daemon find their main PID without cgroups, so the
// ownership test is the point: a group number is only a number, and once our
// starter is gone a recycled one belongs to somebody else's processes.
func (u *Unit) processGroupMemberPID(pgid int, exclude int) int {
	if pgid <= 0 {
		return 0
	}
	uid := u.expectedUID()
	for _, pid := range procPIDs() {
		if pid == exclude {
			continue
		}
		memberPGID, err := syscall.Getpgid(pid)
		if err != nil || memberPGID != pgid {
			continue
		}
		if !procOwnedBy(pid, uid) || checkLiveness(pid) != livenessAlive {
			continue
		}
		return pid
	}
	return 0
}

func (u *Unit) adoptedNotifyPIDWithCurrent(watchedPID int, currentPID int) int {
	if pid := u.livePIDFilePID(); pid != 0 && pid != watchedPID {
		return pid
	}
	if currentPID != 0 && currentPID != watchedPID && processAlive(currentPID) {
		return currentPID
	}
	if memberPID := u.cgroupMemberPID(watchedPID); memberPID != 0 {
		return memberPID
	}
	if memberPID := u.processGroupMemberPID(watchedPID, watchedPID); memberPID != 0 {
		return memberPID
	}
	return 0
}

func (u *Unit) adoptedNotifyPID(watchedPID int) int {
	return u.adoptedNotifyPIDWithCurrent(watchedPID, u.mainPID())
}

func (u *Unit) waitForNotifyMainPID(watchedPID int, timeout time.Duration, poll time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		if pid := u.adoptedNotifyPID(watchedPID); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(poll)
	}
}

func (u *Unit) notifyMainPID(cmd *exec.Cmd) int {
	if pid := u.waitForLivePIDFile(500*time.Millisecond, 25*time.Millisecond); pid != 0 {
		return pid
	}
	if cmd != nil && cmd.Process != nil {
		if pid := u.processGroupMemberPID(cmd.Process.Pid, cmd.Process.Pid); pid != 0 {
			return pid
		}
	}
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Pid
	}
	return 0
}

func (u *Unit) waitNotify(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	u.mu.Lock()
	server := u.notifyServer
	cmd := u.Cmd
	u.mu.Unlock()

	if server == nil {
		u.markFailed(fmt.Errorf("notify server missing"), ignoreFailure)
		return
	}

	timer := time.NewTimer(u.StartTimeout())
	defer timer.Stop()

	// Monitor process exit during the activating phase. Without this, a
	// Type=notify process that dies before sending READY=1 is left as a
	// zombie and the unit is stuck in "activating" until timeout, then
	// wrongly marked "active" because a zombie still passes processAlive.
	// A single Wait() goroutine is shared by all branches below. When a
	// reaper owns Wait4 (production daemon), poll liveness instead so an
	// early exit still fails fast; the reaper's own handler will also
	// fire and duplicate handleExit is idempotent.
	waitCh := make(chan error, 1)
	if cmd != nil && cmd.Process != nil {
		if u.reaper == nil {
			go func(c *exec.Cmd) {
				waitCh <- c.Wait()
			}(cmd)
		} else {
			go func(pid int, token int) {
				for {
					// Stop polling once this start is superseded or the
					// unit left activating (READY arrived or reaper marked
					// it Failed): post-READY exits belong to the reaper
					// handler from runStartSequence, not this watcher.
					if !u.IsCurrentToken(token) || u.StopRequested() {
						return
					}
					if snap := u.Snapshot(); snap.State == StateActive || snap.State == StateFailed || snap.State == StateInactive {
						return
					}
					if !processAlive(pid) {
						select {
						case waitCh <- fmt.Errorf("process exited"):
						default:
						}
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
			}(cmd.Process.Pid, token)
		}
	}

	select {

	case <-server.Ready:
		pid := u.notifyMainPID(cmd)
		u.mu.Lock()
		if u.startToken == token && !u.stopRequested {
			u.Runtime.State = StateActive
			if pid != 0 {
				u.Runtime.MainPID = pid
				u.Runtime.ExecMainPID = pid
			}
		}
		u.mu.Unlock()
		if err := u.runExecStartPost(token, envMap, envList); err != nil && !u.StopRequested() {
			u.killMainProcess(u.stopSignal())
			u.markFailed(err, ignoreFailure)
			return
		}
		if u.reaper == nil && cmd != nil {
			go func() {
				u.handleExit(token, <-waitCh, ignoreFailure, true)
			}()
		}

		return

	case err := <-waitCh:
		// Process exited before sending READY=1.
		u.handleExit(token, err, ignoreFailure, true)
		return

	case <-timer.C:
		u.mu.Lock()
		if u.startToken != token || u.stopRequested {
			u.mu.Unlock()
			return
		}
		// The reaper handler may have already resolved an early exit
		// while we slept: never resurrect Failed/Inactive with Active.
		if u.Runtime.State == StateFailed || u.Runtime.State == StateInactive {
			u.mu.Unlock()
			return
		}
		cmd = u.Cmd
		u.mu.Unlock()

		// A service that never said READY=1 fails: upstream terminates the
		// process and reports Result=timeout. The previous behaviour marked it
		// active because the process was still running, which turned a service
		// with a broken (or absent) notify protocol into a permanently healthy
		// looking lie - and into a start job that reported success.
		if cmd != nil && cmd.Process != nil {
			u.killMainProcess(u.stopSignal())
		}
		u.markTimeout(fmt.Errorf("notify timeout"), ignoreFailure)
		return
	}
}

func mergeEnvList(envList []string, extra map[string]string) []string {
	envMap := make(map[string]string, len(envList)+len(extra))
	for _, pair := range envList {
		if key, value, ok := strings.Cut(pair, "="); ok {
			envMap[key] = value
		}
	}
	for key, value := range extra {
		if key == "" {
			continue
		}
		envMap[key] = value
	}
	merged := make([]string, 0, len(envMap))
	for key, value := range envMap {
		merged = append(merged, fmt.Sprintf("%s=%s", key, value))
	}
	return merged
}

func (u *Unit) resolveCredentialsForStart(rootOnly bool) (credentialSpec, error) {
	if rootOnly {
		return credentialSpec{}, nil
	}
	userName := strings.TrimSpace(u.GetConfig().Service.User)
	groupName := strings.TrimSpace(u.GetConfig().Service.Group)
	if userName == "" && groupName == "" {
		return credentialSpec{}, nil
	}

	var (
		uid uint32
		gid uint32
		err error
	)

	if userName != "" {
		uid, gid, err = lookupUser(userName)
		if err != nil {
			return credentialSpec{}, err
		}
	}

	if groupName != "" {
		gid, err = lookupGroup(groupName)
		if err != nil {
			return credentialSpec{}, err
		}
	} else if userName == "" {
		gid = uint32(os.Getgid())
	}

	if userName == "" {
		uid = uint32(os.Getuid())
	}

	groups, err := lookupSupplementaryGroups(u.GetConfig().Service.SupplementaryGroups)
	if err != nil {
		return credentialSpec{}, err
	}

	return credentialSpec{uid: uid, gid: gid, groups: groups, set: true}, nil
}

func (u *Unit) workingDirectory() string {
	if dir := strings.TrimSpace(u.GetConfig().Service.WorkingDirectory); dir != "" {
		return u.expandSpecifiers(dir)
	}
	// systemd defaults system services to the root directory when
	// WorkingDirectory= is unset; mirror that instead of the unit file's dir.
	return "/"
}

func (u *Unit) Description() string {
	// Descriptions are literal (LSB headers may contain % or $). No
	// specifier/env expansion here — that belongs to command lines only.
	if u.GetConfig().Description != "" {
		return u.GetConfig().Description
	}
	return u.GetConfig().Name
}

func (u *Unit) SuccessExitStatus() map[int]struct{} {
	return parseExitStatusSet(u.GetConfig().Service.SuccessExitStatus)
}

func lookupUser(name string) (uint32, uint32, error) {
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(uid), uint32(os.Getgid()), nil
	}
	info, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(info.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid for %q: %w", name, err)
	}
	gid, err := strconv.ParseUint(info.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid for %q: %w", name, err)
	}
	return uint32(uid), uint32(gid), nil
}

func lookupGroup(name string) (uint32, error) {
	if gid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(gid), nil
	}
	info, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("lookup group %q: %w", name, err)
	}
	gid, err := strconv.ParseUint(info.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse gid for %q: %w", name, err)
	}
	return uint32(gid), nil
}

func lookupSupplementaryGroups(entries []string) ([]uint32, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	groups := make([]uint32, 0, len(entries))
	for _, entry := range entries {
		gid, err := lookupGroup(entry)
		if err != nil {
			return nil, err
		}
		groups = append(groups, gid)
	}
	return groups, nil
}

func parseExitStatusSet(raw string) map[int]struct{} {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	result := make(map[int]struct{}, len(fields))
	for _, entry := range fields {
		if value, err := strconv.Atoi(entry); err == nil {
			result[value] = struct{}{}
		}
	}
	return result
}

func parseSystemdDuration(raw string, defaultValue time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue
	}
	if raw == "0" || strings.EqualFold(raw, "infinity") {
		return 0
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		return parsed
	}
	// Systemd accepts bare numbers (seconds) and a wider suffix table
	// than Go: us/usec, ms/msec, s/sec, m/min, h/hr, d/day, w/week,
	// M/month, y/year (plus plurals). Go already covered ns/us/ms/s/m/h
	// above, so this handles the rest plus bare numbers without hiding
	// typos behind the default.
	lower := strings.ToLower(strings.TrimSpace(raw))
	// Split numeric prefix from unit suffix.
	i := 0
	for i < len(lower) && (lower[i] >= '0' && lower[i] <= '9' || lower[i] == '.' || lower[i] == '+' || lower[i] == '-') {
		i++
	}
	numPart := strings.TrimSpace(lower[:i])
	unitPart := strings.TrimSpace(lower[i:])
	if numPart == "" {
		return defaultValue
	}
	val, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return defaultValue
	}
	var mult time.Duration
	switch unitPart {
	case "", "s", "sec", "secs", "second", "seconds":
		mult = time.Second
	case "ns", "nsec", "nsecs", "nanosecond", "nanoseconds":
		mult = time.Nanosecond
	case "us", "usec", "usecs", "microsecond", "microseconds":
		mult = time.Microsecond
	case "ms", "msec", "msecs", "millisecond", "milliseconds":
		mult = time.Millisecond
	case "m", "min", "mins", "minute", "minutes":
		mult = time.Minute
	case "h", "hr", "hrs", "hour", "hours":
		mult = time.Hour
	case "d", "day", "days":
		mult = 24 * time.Hour
	case "w", "week", "weeks":
		mult = 7 * 24 * time.Hour
	case "month", "months":
		// Systemd month = 30.44 days; use 30d like most parsers.
		mult = 30 * 24 * time.Hour
	case "y", "year", "years":
		mult = 365 * 24 * time.Hour
	default:
		// Unknown suffix: do not guess, fall back so callers keep
		// previous behaviour instead of failing open with 0.
		return defaultValue
	}
	// Handle the "M" (month) vs "m" (minute) case-sensitivity lost by
	// lower-casing: a raw trailing "M" means month.
	if strings.HasSuffix(strings.TrimSpace(raw), "M") {
		mult = 30 * 24 * time.Hour
	}
	return time.Duration(val * float64(mult))
}

// validateUMask parses a UMask value (octal, 000-777). Anything else fails
// the start instead of running with an unintended mask.
func validateUMask(raw string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(raw), 8, 32)
	if err != nil || parsed > 0o777 {
		return 0, fmt.Errorf("invalid UMask %q: want octal 000-777", raw)
	}
	return os.FileMode(parsed), nil
}

// validateLimitNOFILE parses LimitNOFILE (integer or infinity/unlimited).
func validateLimitNOFILE(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.EqualFold(s, "infinity") || strings.EqualFold(s, "unlimited") {
		return s, nil
	}
	if _, err := strconv.ParseUint(s, 10, 64); err != nil || s == "" {
		return "", fmt.Errorf("invalid LimitNOFILE %q: want integer or infinity", raw)
	}
	return s, nil
}

func parseFileMode(raw string, defaultValue os.FileMode) (os.FileMode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("parse file mode %q: %w", raw, err)
	}
	return os.FileMode(parsed), nil
}
