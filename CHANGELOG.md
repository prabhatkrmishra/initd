# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.1] - 2026-09-01

### Added
- `install.sh` one-shot installer for root-in-chroot deployment (`d8d20fe`).
- D-Bus `org.freedesktop.systemd1` compatibility service registered by the `initd` daemon (`9c222c3`).
- `Manager.LoadUnit` method on the systemd1 D-Bus service (`3860e55`).
- `DropInPaths` and `NeedDaemonReload` on the unit D-Bus Properties map (`fb4b2bf`).
- `org.freedesktop.systemd1.Service` unit properties, including `ExecStart` (`3d63ba0`).
- `Service.Interface` `Environment`, `EnvironmentFiles`, and `UnsetEnvironment` (`8947d83`).
- `%t` specifier expansion for socket `ListenStream` / `ListenDatagram` (`0465099`).
- System bus policy artifact (`org.freedesktop.systemd1.system-policy.conf`) and a deploy script for D-Bus systemd1 ownership via `initd` (`6506d81`).
- Tests: `cmd/systemctl/parse_show_args_test.go`, `internal/dbus/dbus_test.go`, `internal/service/service_test.go`, `internal/supervisor/manager_test.go`.
- Version markers `initdVersion`/`systemctlVersion` set to `1.0.1` and reported over D-Bus (`Version = "1.0.1 (initd)"`).

### Changed
- `make build` / `build-all` and `make package` now source binaries from the `make build` output by default (`66b529c`).
- Deploy now installs `initd` + `systemctl` to `/usr/bin` and restarts the daemon using `-x initd` (`d9b84a0`).

### Fixed
- Resolve units on demand from disk in `FindUnit` / `IsEnabled` (`b05a706`).
- Fix D-Bus service nil-manager handling and add the system bus policy artifact (`6506d81`).
- Make `systemctl show` (manager-level) degrade gracefully when offline (`fdd0f4d`).
- Fix notify services stuck in `activating` or wrongly reported as active (`5208234`).
- Fix `ExecStart` `@`/`!` prefix handling, `.target` warnings, and the notify reaper (`5028100`).

### Packaging
- Release artifacts: `releases/initd_1.0.1_linux_arm64.zip` and `releases/initd_1.0.1_linux_amd64.zip`, each with `sha256sum`.

## [1.0.0]

- Initial stable release: a lightweight, systemd-compatible init system and service manager for containers, chroot/proot, embedded Linux and minimal environments.
- `initd` runs as PID 1 (full init) or in init-lite / service-manager-only mode.
- Unmodified systemd `*.service` support for `simple`, `oneshot`, `forking`, `notify`/`notify-reload`; safe fallback to `simple` for `exec`, `idle`, `dbus` and unknown types.
- Familiar `systemctl` workflow: start/stop/restart, enable/disable, status/is-active/is-enabled, list-units, list-unit-files, daemon-reload, plus `reboot`/`poweroff`/`halt`.

[1.0.1]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.1
[1.0.0]: https://github.com/prabhatkrmishra/initd/releases/tag/v1.0.0
