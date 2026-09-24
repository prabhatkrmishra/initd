# Compat matrix vs systemd 259 (Ubuntu 26.04 amd64) + upstream main

Oracle: `systemctl/journalctl/loginctl/systemd --help` on this box, `man
systemctl/journalctl/loginctl`, upstream `/tmp/systemd-upstream`
(`src/systemctl`, `src/journal`, `src/login`, `src/shared` pager,
`systemctl-is-active.c` / `systemctl-is-enabled.c` exit logic).
Shim sources: `cmd/systemctl/main.go`, `cmd/journalctl/*.go`,
`cmd/loginctl/main.go`, `cmd/initd/main.go`.

Legend: match = behaves like systemd; partial = matches except noted
deviations; loud-fail = initd intentionally refuses (containers, images,
remote hosts, FSS/catalog) with a clear stderr message.

## systemctl (real help: 209 lines)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Global flags anywhere | accepted before or after command | verb detected first, recognized flags forwarded into verb args | match (Phase 1) |
| Verbs `start/stop/restart/reload` with `UNIT...` | act on all units | all units, first failure wins the exit | match (Phase 1) |
| `status/is-active/is-enabled/cat` with several units | show all, combined exit | all shown; status prefers failed(1) over not-active(3); is-active 0 if any active; is-enabled 0 if any enabled | match (Phase 1) |
| `status [PATTERN\|PID]` | PID lookup, `-n/-o` honored, logs capped | PID lookup kept, `-n` honored (default 10 like systemd), `-o` accepted | match (Phase 1) |
| `show [PATTERN\|JOB]` + manager (no unit) | live manager props, `-p/-P/--value/-a`, not-found defaults exit 0 | per-unit behavior matches; no-unit still serves offline fallback props | partial (live manager endpoint deferred: needs a daemon API) |
| `is-active/is-enabled/is-failed/is-system-running` | LSB exits 0/1/3/4, `--quiet` suppresses print | unknown maps to 4, `--quiet` suppresses `is-active/is-enabled/is-failed` | match (Phase 1) |
| `list-units/list-unit-files PATTERN...` | glob patterns, `--state/--type/--failed/--all`, legend, `--full` | patterns, filters and legend flags accepted on either side | match (Phase 1) |
| `enable/disable/reenable/preset/mask/unmask` | full unit-file set | implemented subset; `link/revert/add-wants/add-requires/edit` etc. still missing | partial |
| `list-jobs/cancel/list-dependencies/isolate/kill/clean/freeze/thaw/set-property` etc. | present | `kill` implemented; rest missing (unknown verb error) | partial |
| `-H/--host/-M/--machine/-C/--capsule/--root/--image` | remote/container/image ops | loud-fail | loud-fail (keep) |
| Pager | `SYSTEMD_PAGER` > `PAGER`, `--no-pager`, only when tall+tty | no pager; `--no-pager` accepted anywhere | match (keep) |
| User daemon autostart | n/a | `--socket=` now passed so the spawned daemon listens on the user socket | match (Phase 5) |

## journalctl (real help: 92 lines)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Sources `-M/--image/--namespace` | container/image/namespace journals | loud-fail | loud-fail (keep) |
| Sources `-D/--directory/-i/--file/--root/--system/--user/-m` | all present (`-i` is file) | all present including `-i` | match (Phase 2) |
| Filters `--invocation/-I/-T/--facility/--since/--until/--boot/--unit/--user-unit/--identifier/--priority/--grep/--cursor` | all present | all present; `--facility` accepted but unfiltered (no facility field stored); runs get a fresh `_SYSTEMD_INVOCATION_ID` on every unit start | match (Phase 2, facility deviation noted) |
| Outputs `-o` | short plus short-delta + verbose/export/json*/cat/with-unit | short-delta added with upstream delta format; unknown mode still falls back to short (tested leniency, real errors) | partial (fallback deviation kept) |
| Pager | `SYSTEMD_PAGERSECURE`, `--no-pager`, `-e` jump-to-end, never page in `-f`, only when tall | `SYSTEMD_PAGER` > `PAGER` precedence, shell-split, empty disables, `LESS=FRSXMK` defaults, own process group + held SIGINT, single-print `-e` with `+G` for less, `-f` never pages | match (Phase 2; `SYSTEMD_PAGERSECURE` sandboxing not replicated) |
| Memory | streams, tail-first | entries stream to output/pager with no formatted copy and no joined blob; single-scope queries skip the merge copy; unfiltered queries skip the filter copy (52MB journal file: ~121MB steady vs 5-6x plus a contiguous blob before) | match (Phase 2; true tail-first server reads need a protocol change) |
| `--verify/--sync/--flush/--rotate/--header/--disk-usage/--vacuum-*/-N/-F/--list-boots/--list-invocations/--list-namespaces` | present | all present; `--list-namespaces` prints `No namespaces found.` like real | match (Phase 2) |
| FSS/catalog `--setup-keys/--verify-key/--interval/--list-catalog/--dump-catalog/--update-catalog` | sealed-hash chain | loud-fail | loud-fail (keep) |

## loginctl (real help: 59 lines)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Verbs (sessions/users/seats, incl. `session-status/user-status/seat-status`, lock/unlock plural) | present | full table; `list-linger` kept as a compat extra | match (Phase 3) |
| Multi `ID.../USER.../NAME...` | all shown | all shown, per-item errors combine | match (Phase 3) |
| Flags `-P/-a/--all/-l/--full/--kill-whom/-s/--signal/-n/--lines/-o/--output/--json/-j/--value/--legend` | present | all parsed; `-n/-o` accepted (no session logs to shape) | match (Phase 3) |
| Unknown flags | error | `unknown option` exit 1 | match (Phase 3) |
| `-p` compact | `-pNAME` attached form (getopt) | same | match |
| `show-*` format | `KEY=VALUE` at column zero, `-p/-P/--value`, `-a` for empties, manager props with no args | same; user props use real names (`UID/GID/Home`, plus `Linger/State`) | match (Phase 3) |
| Linger | real per-user state | file store (`/var/lib/initd/linger`, else user state dir); `enable/disable` with no args act on caller | match (Phase 3) |
| Session/seat actions | act on sessions | ID-targeted verbs report no-such-session/seat exit 1; empty plural verbs succeed trivially; `attach/flush-devices` loud-fail | match (Phase 3) |
| `-H/-M` remote | remote ops | loud-fail | loud-fail (keep) |
| `list-users` | `UID USER LINGER STATE` + count | same, plus `--json` | match (Phase 3) |

## initd vs systemd PID1 (real `systemd --help`: 34 lines)

| Area | Real behavior | Ours | Status |
| --- | --- | --- | --- |
| Signals | TERM ignored, PWR powers off, CHLD reaped | same; SIGINT/SIGTERM shut down gracefully outside PID 1 | match (Phase 4) |
| Serve loop | clean handover | stop-aware with backoff, no resurrection after shutdown | match (Phase 4) |
| pidfile/runtime/log | safe ownership/perms/rotation | EPERM counts as live, 0700 runtime dirs, 64MB log rotation, pid wait checks our own child, detach works from any cwd | match (Phase 4) |
| `--test/--dump-*/--log-target/--log-level` etc. | present | unknown args already error loudly | loud-fail (keep) |
| D-Bus shutdown | clean handover | bus names released on shutdown | match (Phase 4) |
| Unit shutdown bound | per-unit timeouts | overall 2-minute bound per manager in the daemon path (PID 1 keeps the 400s shutdown path) | match (Phase 4) |

## Cross-cutting (Phase 5)

- Usage errors go to stderr in all four shims; `--help/-h/--version/-V` shapes kept per tool.
- `scripts/compat-probe.sh` enforces the fixed surface (parse, short-delta, col-0 props) plus build.
- Full `go test ./...`, all four binaries rebuild, CHANGELOG updated.
