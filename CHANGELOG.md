# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[1.0.2]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.2
[1.0.1]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.1
[1.0.0]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.0
