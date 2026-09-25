# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- A process the daemon is not allowed to see is no longer a process that is gone. `/proc` mounted with `hidepid=2` made `kill(pid,0)` answer `EPERM` for every process another user owns, and liveness read that as dead - so a running daemon was reported stopped, and a restart ended up with two of them. Liveness is three-valued now (alive, dead, not-permitted-to-know). The same blind scan was guessing wrong in three more places: external-process matching refuses command lines this unit could not own, a recorded starter process group is swept only while it still holds one of the unit's own live processes, and a `PIDFile=` number is rejected when the process it names started long after the file was written. A `forking` start that never produced a daemon also reports failure to `systemctl start` instead of success over a unit still activating.
- Socket activation tells the truth about its file descriptors. `$LISTEN_PID` is the daemon's own pid - a shell assigns it at exec time, because `sd_listen_fds()` ignores the entire fd set when the number does not match - `$LISTEN_FDNAMES` names every fd with the socket unit it came from, and a start that cannot make that handover fails instead of coming up deaf. Descriptors a failed start never exec'd are now released (by the start itself, and by a later handover that replaces them), so a socket-activated unit with a failing `ExecStartPre` no longer leaks one fd per 100ms poll until the daemon runs out of descriptors.
- Unbounded `journalctl` queries no longer load the whole journal into RAM on either end: clients page head-after-cursor in fixed chunks (including offline single-source reads and pager output), the daemon answers bounded pages by scanning to the limit and stopping, and tail `-n` reads newest-first with early stop. A spam-looping unit can no longer OOM the daemon or the client.
- The daemon now caps journal retention automatically (user scope ~20MB/10 files, system ~100MB/10 files, active file never deleted) so history cannot grow without bound between explicit vacuums.
- `journalctl --vacuum-time=` can expire history the daemon has open. The active journal file always carries the freshest mtime, so a unit that logged once and went quiet pinned that history forever: age policy walked past it and only a size or file-count vacuum could reach it. An age vacuum closes the stale file into a dated generation first, then applies the age policy, so old history actually leaves the disk. The file the writer holds and the newest one on disk stay exempt, so a vacuum cannot empty the journal directory, and the daemon keeps journalling into the new generation.
- Journal retention trims oldest-by-time. Generations were deleted in name order, and a boot ID is a random UUID, so a machine with several boots could lose this boot's newest history while keeping a generation from months ago. Rotation-driven retention now shares the mtime ordering the readers use.
- The first line written after a rotation reaches the disk. Rotation handed over a fresh buffer but kept the previous file's flush timestamp, so the flush-once-a-second test was already satisfied and a unit's line - often its last before a restart - waited in RAM for some later write to flush it, invisible to `journalctl`.
- A refused exec reports one exit status, not two. `recordExecFailure` published the unit as failed carrying the generic `ExitCode=1`, then took the lock a second time to swap in the status systemd invents for a spawn that never happened (200/CHDIR, 203/EXEC, 216/GROUP). A `systemctl show`/`status`/D-Bus read landing in that window - which `ShowUnit` does by capturing one snapshot - saw `1` instead of `203`, and a script keying on 203, exactly as the status line invites, read the wrong number. The follow-up write also guarded on `State == StateFailed`, so any state change arriving in the window dropped the real status altogether. State and status are now published in one critical section.
- The `systemctl status` timestamp test no longer depends on the machine's timezone. It asserted the zone was literally `UTC` or `MST`, but `MST` in `at.Local().Format("Mon 2006-01-02 15:04:05 MST")` is Go's placeholder for the zone abbreviation rather than the text systemd prints, so the test failed on every box outside UTC or a zone that happens to abbreviate to MST. It matches systemd's weekday-first shape now and passes under any `TZ`.
- A superseded `Type=notify` start no longer leaks its notify socket. The socket is created before the child exists, so a start that was overtaken while it exec'd (a stop or a newer start bumping the token) killed the child and returned with the socket still open - and because whoever superseded it had already cleared the unit's handle, nothing was left to close it. The abstract name then stayed in the kernel for the life of the daemon; one box had 24 of them accumulated from dead PIDs. A start now hands the socket to the unit explicitly, and a deferred release closes it on every other path, so a new early return cannot leak one. `releaseNotifyServer` only clears the handle while it still names the server being closed, so a superseded start can never strand a successor's socket.

### Added
- `systemctl` accepts global flags (`--no-pager`, `--quiet`, `--now`, `-n`, `-o`, `--state`, `--type`, `-p`, `-P`, `--value`, `-s`, …) before the verb as well as after it, like real systemctl. `try-restart`, `reload-or-restart`, `try-reload-or-restart`, `reenable`, `preset`, and `help` are now listed in `--help`.
- `journalctl` gained `-i`, `-W`, `-T/--exclude-identifier`, `-I/--invocation`, `--list-invocations`, `--list-namespaces`, `--synchronize-on-exit`, and `-o short-delta` (upstream delta format). Units stamp a fresh `_SYSTEMD_INVOCATION_ID` on every start so `-I` and `--invocation` isolate runs.
- `loginctl` covers the full session/user/seat verb set with strict flag parsing, `-P`, `-a/--all`, `--legend=`, `--json`/`-j`, greppable column-zero `show-*` output, and a real linger store (`/var/lib/initd/linger`, else the user state dir).
- Every unit gets a leaf cgroup where the kernel lets the daemon make one, so a stop is answered by kernel membership instead of by a command line any process may copy. `KillMode=control-group` and `mixed` sweep that group (`mixed` lets the main process go first, then kills what is left behind), `process` and `none` leave children running, and `systemctl status`, `Show=` and D-Bus report `ControlGroup` (on `org.freedesktop.systemd1.Service`, where systemd puts it) and the PIDs inside it the way systemd does. A unit that ends with no stop coming - a crash, an exit, a start that timed out - hands the group back as well, once the kill that ended it has landed and the kernel agrees nothing of the unit is inside; a process that outlived its main program keeps the group, because it is ours and it is still running. Where the prefix is not ours to write - a container, a WSL distro whose cgroup tree belongs to root - the daemon says why once at startup and supervises exactly as it did before.
- `PassEnvironment=` is parsed and applied. systemd only listens to it in its system manager - which inherits nothing from its own environment unless a unit names it - and ignores it in the user manager, which hands every unit its whole environment (measured against 259, and stated in `systemd.exec(5)`). Since initd inherits everything, naming variables filters that inheritance: a unit asking for two variables gets those two, a name the manager does not have is silently absent as upstream, and `Environment=`/`EnvironmentFile=` keep overriding it with `UnsetEnvironment=` still having the last word. The filter keys on the scope of the manager that holds the unit rather than the daemon's uid, because one process serves a system and a user manager at once.

### Fixed
- `systemctl` verbs act on every unit instead of just the first, with combined LSB exit codes (unknown units report `unknown` and exit 4); `status` caps logs at 10 lines unless `-n` says otherwise; `--quiet` suppresses `is-active`/`is-enabled`/`is-failed`; usage errors go to stderr in all shims.
- `journalctl` streams entries to stdout/pager instead of joining them into one giant string (52MB journal: ~185MB steady down to ~121MB), pages with `SYSTEMD_PAGER` > `PAGER` precedence, shell-split pager commands, `LESS=FRSXMK` defaults, and SIGINT held while the pager owns the screen; `-e` prints once with `+G` for less; `-f` never pages.
- `loginctl` no longer swallows flag typos and no longer reports linger as always on; session/seat actions fail honestly instead of pretending to work.
- `initd` shuts down cleanly on SIGINT, no longer resurrects sockets after shutdown (stop-aware serve loops with backoff), bounds unit stops during daemon shutdown, refuses to steal other users' pid files, keeps runtime dirs 0700, rotates the 64MB detach log, verifies the detached child via its pid file, detaches from any cwd, releases D-Bus names on shutdown, and powers off orderly on SIGPWR as PID 1.
- Liveness and parsing no longer lie: `processAlive` treats a vanished `/proc` as dead (only `EPERM` stays alive for other users' processes); `TimeoutSec`/`RestartSec` accept systemd values (`5min`, `1d`, `1w`, `M/month`, bare seconds) instead of silently falling back; `EnvironmentFile` handles `export`, `;` comments and single quotes; `RuntimeDirectory` accepts trailing slashes and `./` forms.
- `Type=notify` stops the whole group per `KillMode` with `SIGKILL` escalation like simple services, fails fast when the starter exits before `READY=1` even with a reaper, and no longer blocks the exit path waiting for a successor or resurrects `Failed` on timeout.
- Journal keeps order across rotates and restarts (sequence carried as the max across the directory, no duplicate cursors); long (>90ch) socket paths dial the same abstract fallback the daemon listens on; socket-activated fds are closed in the parent after fork.
- Supervision agrees everywhere: external-process detection requires `ExecStart` args to match (no more `sleep 10` lighting up `sleep infinity`) and only applies to `inactive` units; D-Bus object paths use full systemd escaping and `ListUnits` reports the effective state; unit configs swap under a dedicated lock so `daemon-reload` can't race starters.
- Socket activation no longer eats its trigger: the supervisor polls for readability and never `Accept()`s, so the first connection stays queued for the child via `LISTEN_FDS` (datagram sockets trigger too); a cold-start dial test guards it.
- IPC and D-Bus enforce who may act: abstract-socket peers prove UID via `SO_PEERCRED` (daemon UID or root, others get access-denied before dispatch); the system-bus policy now allows only read-only `Manager` members to everyone and full control to root, with the misleading read-only comment corrected.
- Wrapped unit lines parse: backslash continuations join before the `=` split (odd trailing run, next-line indent stripped, 1MB buffer) and unparsable files log which path was skipped instead of vanishing; helper commands (`ExecStartPre`, `ExecCondition`, `ExecStop`, `ExecStartPost`, `ExecReload`) honor `TimeoutStartSec`/`TimeoutStopSec` with group `SIGKILL` instead of waiting forever; a `Failed` unit keeps its `ExecStartPost` reason instead of being rewritten to `SIGTERM`/143.
- Environment is explicit: full daemon inheritance stays intentional for containers (documented in code) with `UnsetEnvironment=` as opt-out; D-Bus `ExecStart` introspection keeps interior empty args (`""`).
- Stopping kills the right tree: group signals resolve via `Getpgid` on the live leader (recorded starter group covers orphans) instead of assuming an adopted `MainPID` is a group leader; `forking` honors `KillMode`; liveness waits out group members, not just the leader; required dependencies fail on their own `TimeoutStartSec` instead of a fixed 30s pass; socket creation is serialized; `OnFailure` spends `StartLimit` budget against ping-pong storms; `PartOf`/`BindsTo` stops recurse.
- Every start counts: manual, dependency, failure-forward and restart starts share one `StartLimit` admission; D-Bus reports missing units as `NoSuchUnit` instead of success and `ListUnitsFiltered` filters; user managers use XDG roots; bad `UMask`/`LimitNOFILE` fail visibly with `&&`-chained setup.
- The control socket is bounded (1MB requests, 128 conns, read/write deadlines, accept backoff); install scripts never signal PID 1, restart only the system bus by pidfile, keep runtime dirs 0700 without duplicating live buses, and no longer take sudo passwords from the environment.
- Releases are reproducible from the pipeline (`make build-all` gives all four stripped static binaries per arch, including the missing arm64 `journalctl`; `make package` writes `SHA256SUMS` plus a manifest with per-binary hashes and fails loudly without `zip`); `/proc` argv matching is documented as a fallback pending cgroup identity.


## [1.0.3] - 2026-09-02

### Fixed
- `initd --init` now respects system/user separation: when running as root it only starts system units, when running as non-root it only starts user units. Previously the daemon started both, causing user services like `openclaw-gateway.service` to be started by the system manager as root and then raced with the user manager, leading to duplicate supervisors and `Active: stopping since ... 13m ago` hangs (`cmd/initd/main.go`).
- `Stop()` for `simple`/`exec`/`oneshot`/`notify` services no longer hangs in `stopping` until `TimeoutStopSec` expires. The poll loop now also checks `processAlive(MainPID)` and transitions to `inactive` immediately when the process exits, mirroring systemd's PID liveness tracking. This fixes `systemctl --user stop/restart` blocking for 330s and makes it return in <1s for services that handle `SIGTERM` correctly (e.g. openclaw gateway shuts down in ~470ms) (`internal/service/service.go:630`).

### Changed
- Version markers `initdVersion`/`systemctlVersion`/`loginctlVersion` and D-Bus `Version` bumped to `1.0.3`.

### Packaging
- Release artifacts: `releases/initd_1.0.3_linux_arm64.zip` and `releases/initd_1.0.3_linux_amd64.zip`, each containing `initd` + `systemctl` + `loginctl` + `install.sh` with `sha256sum`.

## [1.0.2] - 2026-09-02

### Added
- System socket lock (`/run/initd.sock.lock` / `$XDG_RUNTIME_DIR/initd-system.sock.lock`) to prevent split-brain when two `initd --init` invocations race for the system socket (`be00bf4`).
- Stale session bus detection and recovery in `install.sh` and `/etc/profile.d/initd.sh` — probes the bus with `dbus-send ListNames` and recreates it if unresponsive (`2bd636d`).
- D-Bus session bus retry in the daemon — retries `ServeUserBus` briefly when the bus is still coming up (`2bd636d`).

### Changed
- Autostart hook (`/etc/profile.d/initd.sh`) now scopes the daemon check to the current user (`pgrep -u "$(id -u)" -x initd`) so one user's daemon no longer blocks another user's (`8d66b06`).
- `install.sh` D-Bus service and autostart hook are now written via temp file + `install` instead of `sudo tee` heredoc, fixing stdin conflict when `SUDO_PASSWORD` is set (`8d66b06`).
- `systemctl` `restart`/`stop` client timeout raised from 90s to 600s to cover large `TimeoutStopSec` (e.g. 330s gateway) (`39a6b9c`).
- `boot/shutdown.go` stop timeout raised from 10s to 400s to cover sequential `StopAllUnits` with large timeouts (`fe13287`).
- Version markers `initdVersion`/`systemctlVersion`/`loginctlVersion` and D-Bus `Version` bumped to `1.0.2`.

### Fixed
- `StopUnit` no longer reports `failed` when the process is already gone — checks `processAlive` after SIGKILL and marks `inactive` (`f4db3bf`).
- IPC socket permission tightened from `0700` to `0600` (`f4db3bf`).
- `loginctl` now correctly parses `-p`/`--property`, `--value`, `--no-legend` and other flags instead of dropping values (`18fada1`).
- D-Bus activation service no longer sets `User=` (was wrong under `sudo`) (`18fada1`).
- Non-root daemon no longer attempts to start system units — gated on `os.Getuid() == 0` to avoid `EACCES` restart storms (`fac4b3b`).
- Daemon now shuts down cleanly on `SIGTERM` (stops units, removes sockets, releases locks, `os.Exit(0)`) and `install.sh` waits for the old daemon to exit before starting a new one (`477389c`).
- `StartEnabledUnits` no longer holds `startMu` for the entire boot, unblocking concurrent IPC `StartUnit`/`StopUnit`/`RestartUnit` calls (`39a6b9c`).
- D-Bus `MainPID` now reflects the live snapshot instead of stale `ShowUnit` data (`fe13287`).
- `Start()` while `StateStopping` now returns an error instead of pretending success (`be00bf4`).
- `Stop()` while `StateActivating` now bumps `startToken`, kills the activating process and waits correctly (including `infinity` timeout) (`01f413d`).
- `install.sh` `go build` now uses `./` prefix for Go 1.22+ compatibility (`11c2fbd`).

### Packaging
- Release artifacts: `releases/initd_1.0.2_linux_arm64.zip` and `releases/initd_1.0.2_linux_amd64.zip`, each containing `initd` + `systemctl` + `loginctl` + `install.sh` with `sha256sum`.

## [1.0.1] - 2026-09-01

### Added
- `loginctl` compatibility shim (`cmd/loginctl`), installed by `install.sh`
  alongside `systemctl`. Under initd there is no `logind`/`login1`; instead
  user services are persisted by the `/etc/profile.d/initd.sh` autostart hook
  (which starts enabled units on every session login). The shim makes
  `loginctl list-linger` / `enable-linger` / `show-user` / `list-users` /
  `list-sessions` succeed so tools like `openclaw gateway install` no longer
  abort with "Unable to read loginctl linger status" on initd-managed hosts.
- `install.sh` one-shot installer for root-in-chroot deployment (`d8d20fe`).
- D-Bus `org.freedesktop.systemd1` compatibility service registered by the `initd` daemon (`9c222c3`).
- `Manager.LoadUnit` method on the systemd1 D-Bus service (`3860e55`).
- `DropInPaths` and `NeedDaemonReload` on the unit D-Bus Properties map (`fb4b2bf`).
- `org.freedesktop.systemd1.Service` unit properties, including `ExecStart` (`3d63ba0`).
- `Service.Interface` `Environment`, `EnvironmentFiles`, and `UnsetEnvironment` (`8947d83`).
- `%t` specifier expansion for socket `ListenStream` / `ListenDatagram` (`0465099`).
- System bus policy artifact (`org.freedesktop.systemd1.system-policy.conf`) and a deploy script for D-Bus systemd1 ownership via `initd` (`6506d81`).
- Tests: `cmd/systemctl/parse_show_args_test.go`, `internal/dbus/dbus_test.go`, `internal/service/service_test.go`, `internal/supervisor/manager_test.go`.
- Version markers `initdVersion`/`systemctlVersion`/`loginctlVersion` set to `1.0.1` and reported over D-Bus (`Version = "1.0.1 (initd)"`).

### Changed
- `make package` creates per-arch `initd_1.0.1_linux_<arch>.zip` containing
  `initd`, `systemctl`, `loginctl` and `install.sh` (junked to the archive root).
- `install.sh` backs up any real `systemctl`/`loginctl` to
  `/usr/bin/systemctl.real.systemd255` and `/usr/bin/loginctl.real` before
  installing the initd wrappers.
- `make build` / `build-all` and `make package` now source binaries from the `make build` output by default (`66b529c`).
- Deploy now installs `initd` + `systemctl` to `/usr/bin` and restarts the daemon using `-x initd` (`d9b84a0`).

### Fixed
- Resolve units on demand from disk in `FindUnit` / `IsEnabled` (`b05a706`).
- Fix D-Bus service nil-manager handling and add the system bus policy artifact (`6506d81`).
- Make `systemctl show` (manager-level) degrade gracefully when offline (`fdd0f4d`).
- Fix notify services stuck in `activating` or wrongly reported as active (`5208234`).
- Fix `ExecStart` `@`/`!` prefix handling, `.target` warnings, and the notify reaper (`5028100`).

### Packaging
- Release artifacts: `releases/initd_1.0.1_linux_arm64.zip` and `releases/initd_1.0.1_linux_amd64.zip`, each containing `initd` + `systemctl` + `loginctl` + `install.sh` with `sha256sum`.

## [1.0.0]

- Initial stable release: a lightweight, systemd-compatible init system and service manager for containers, chroot/proot, embedded Linux and minimal environments.
- `initd` runs as PID 1 (full init) or in init-lite / service-manager-only mode.
- Unmodified systemd `*.service` support for `simple`, `oneshot`, `forking`, `notify`/`notify-reload`; safe fallback to `simple` for `exec`, `idle`, `dbus` and unknown types.
- Familiar `systemctl` workflow: start/stop/restart, enable/disable, status/is-active/is-enabled, list-units, list-unit-files, daemon-reload, plus `reboot`/`poweroff`/`halt`.

[1.0.3]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.3
[1.0.2]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.2
[1.0.1]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.1
[1.0.0]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.0
