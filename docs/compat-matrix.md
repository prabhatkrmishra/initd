# Compat matrix vs systemd 259 (Ubuntu 26.04 amd64) + upstream main

Oracle: `systemctl/journalctl/loginctl/systemd --help` on this box, `man
systemctl/journalctl/loginctl`, upstream `/tmp/systemd-upstream`
(`src/systemctl`, `src/journal`, `src/login`, `src/shared` pager,
`systemctl-is-active.c` / `systemctl-is-enabled.c` exit logic).
Shim sources: `cmd/systemctl/main.go`, `cmd/journalctl/*.go`,
`cmd/loginctl/main.go`, `cmd/initd/main.go`.

Legend: match = already behaves like systemd; gap = must be fixed in its
phase; loud-fail = initd intentionally refuses (containers, images, remote
hosts, FSS/catalog/namespaces) with a clear stderr message.

## systemctl (real help: 209 lines, ours: 38)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Global flags anywhere (`--no-pager/--quiet/-q/--full/-l/--all/--now/--no-reload/--runtime/--global/-f/--force/-n/--lines/-o/--output/--state/--type/-p/--property/-P/--value/-s/--signal`) | accepted before or after command | only `--socket/--user/--system` parsed before command (`flag.NewFlagSet`), rest give `Usage exit 1` | gap (Phase 1) |
| Verbs `start/stop/restart/reload` with `UNIT...` | act on all units | only `units[0]` | gap (Phase 1) |
| `status/is-active/is-enabled/cat` with several units | show all, combined exit | `os.Exit` inside first unit | gap (Phase 1) |
| `status [PATTERN\|PID]` | PID lookup, `-n/-o` honored, logs capped | `-n/-o` stripped and ignored, unbounded logs | gap (Phase 1) |
| `show [PATTERN\|JOB]` + manager (no unit) | live manager props, `-p/-P/--value/-a`, not-found defaults exit 0 | no-unit path queries `status` with empty unit so always offline | gap (Phase 1) |
| `is-active/is-enabled/is-failed/is-system-running` | LSB exits 0/1/3/4, `--quiet` suppresses print | close but `is-active` unknown exits 1 not 4, `show`/`status` mix | gap (Phase 1) |
| `list-units/list-unit-files PATTERN...` | glob patterns, `--state/--type/--failed/--all`, legend, `--full` no-ellipsize | patterns partial, legend only `--no-legend`, truncation differs | gap (Phase 1) |
| `enable/disable/reenable/preset/mask/unmask/link/revert/add-wants/add-requires` | full unit-file set | subset only | gap (Phase 1, remainder loud-fail or implement) |
| `list-jobs/cancel/list-dependencies/isolate/kill/clean/freeze/thaw/set-property` etc. | present | missing | gap (Phase 1: implement cheap ones, loud-fail rest) |
| `-H/--host/-M/--machine/--root/--image` | remote/container/image ops | loud-fail | loud-fail (keep, fix `-H` prefix over-match) |
| Pager | `SYSTEMD_PAGER` > `PAGER`, `--no-pager`, only when tall+tty | no pager (tolerates `--no-pager`) | match (keep) |

## journalctl (real help: 92 lines, ours: 63)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Sources `-M/--image/--namespace` | container/image/namespace journals | loud-fail | loud-fail (keep) |
| Sources `-D/--directory/-i/--file/--root/--system/--user/-m` | all present (`-i` is file) | `--file` ok, `-i` missing (ours has no `-i` short) | gap (Phase 2) |
| Filters `--invocation/-I/-T/--facility/--since/--until/--boot/--unit/--user-unit/--identifier/--priority/--grep/--cursor` | all present | `--invocation/-I/-T/--facility` missing | gap (Phase 2) |
| Outputs `-o` | `short плюс short-delta` + verbose/export/json*/cat/with-unit | missing `short-delta`, unknown mode silently falls back to short | gap (Phase 2: add `short-delta`, error or keep fallback explicitly) |
| Pager | `SYSTEMD_PAGERSECURE`, `--no-pager`, `-e` jump-to-end, never page in `-f`, only when tall | `PAGER`-only, unsplit args, always pages on tty, `-e` double-prints, `-f` blocks on pager | gap (Phase 2) |
| Memory | streams, tail-first | `ReadAll` + `Join` OOM chain, 250ms full-rescan follow | gap (Phase 2) |
| `--verify/--sync/--flush/--rotate/--header/--disk-usage/--vacuum-*/-N/-F/--list-boots` | present | present (verify semantics differ) | match-ish (Phase 2 polishes) |
| FSS/catalog `--setup-keys/--verify-key/--interval/--list-catalog/--dump-catalog/--update-catalog` | sealed-hash chain | loud-fail | loud-fail (keep) |

## loginctl (real help: 59 lines, ours: 27)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Verbs `session-status/show-session/lock-sessions/unlock-sessions/user-status/seat-status/show-seat/flush-devices` | present | missing or silent no-op | gap (Phase 3) |
| Multi `ID.../USER.../NAME...` | all shown | only first | gap (Phase 3) |
| Flags `-P/-a/--all/-l/--full/--kill-whom/-s/--signal/-n/--lines/-o/--output/--json/-j/--value` | present | only `-p/--value/--no-legend/--no-pager` | gap (Phase 3) |
| Unknown flags | error | silently ignored | gap (Phase 3) |
| `-p` compact | `-pNAME` only | over-matches `-plain` etc. | gap (Phase 3) |
| `show-user` format | `Name=` at col 0 | indented, breaks `grep ^Name=` | gap (Phase 3) |
| Linger | real per-user state | hardcoded `yes` | gap (Phase 3) |
| Destructive verbs | act | silent success doing nothing | gap (Phase 3: warn + non-zero unless implemented) |
| `-H/-M` remote | remote ops | missing (should loud-fail like systemctl) | gap (Phase 3) |

## initd vs systemd PID1 (real `systemd --help`: 34 lines, ours: 20)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Signals | SIGTERM/SIGINT/SIGPWR/reboot path | SIGINT never handled, PID1 only TERM-ignore/CHLD | gap (Phase 4) |
| Serve loop | clean handover | re-binds after shutdown removes socket | gap (Phase 4) |
| pidfile/runtime/log | safe ownership/perms/rotation | EPERM treated as stale, `0755` runtime dir, unbounded log, spawn race | gap (Phase 4) |
| `--test/--dump-*/--log-target/--log-level` etc. | present | missing | gap (Phase 4: implement cheap ones or loud-fail) |
| D-Bus shutdown | clean cancel | ctx never cancelled | gap (Phase 4) |

## Cross-cutting (Phase 5)

- One shared pager/legend/quiet helper used by all shims.
- `--help/-h/--version/-V` shape consistent; errors to stderr; exit codes per man tables.
- Shell completions + man pages; `CHANGELOG.md`; full `go test ./...`; all four binaries rebuild.
