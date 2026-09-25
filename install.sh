#!/usr/bin/env bash
# install.sh — one-shot installer for the initd systemd1 compatibility layer.
#
# Targets the root-in-chroot deployment model: the invoking user is the owner
# of the gateway — most commonly `root` logging in inside a chroot. systemd is
# NOT PID 1 on this host; real systemctl is backed up and shadowed by the
# initd wrapper.
#
# Run it once:
#
#     bash install.sh
#
# It will:
#   1. locate (or build) the initd + systemctl + loginctl + journalctl binaries for this host's arch;
#   2. install (as root directly, or via sudo for a non-root user):
#        /usr/bin/initd                                   (supervisor)
#        /usr/bin/systemctl                               (wrapper)
#        /usr/bin/systemctl.real.systemd255               (backup of real systemd systemctl)
#        /usr/bin/loginctl                               (initd compatibility shim)
#        /usr/bin/loginctl.real                         (backup of real loginctl, if any)
#        /usr/bin/journalctl                             (log viewer for the in-memory ring)
#        /usr/bin/journalctl.real                       (backup of real journalctl, if any)
#        /usr/share/dbus-1/services/org.freedesktop.systemd1.service
#        /usr/share/dbus-1/system.d/org.freedesktop.systemd1.conf
#   3. ensure a session bus at $XDG_RUNTIME_DIR/bus;
#   4. start the initd daemon via `initd --init` (init-lite: brings up the IPC
#      socket, registers org.freedesktop.systemd1 on the D-Bus session+system
#      buses, and starts ALL enabled units), and install an
#      /etc/profile.d/initd.sh autostart hook so the same `initd --init` command
#      re-runs automatically on every session login when the daemon is down;
#   5. verify org.freedesktop.systemd1 is owned and that an arbitrary unit
#      resolves (so openclaw, or any service tooling, works without real systemd).
set -euo pipefail

# --- who am I / how do I gain privileges ---------------------------------------
RUN_USER="$(id -un)"
RUN_UID="$(id -u)"

# `as_root` runs a command with privileges. Root needs no sudo; a non-root
# invoker uses sudo with normal credential handling (prompt or cached
# timestamp). Passwords are never taken from the environment: env vars are
# visible to other local processes and would leak the credential.
if [ "$RUN_UID" -eq 0 ]; then
  as_root() { "$@"; }
else
  as_root() { sudo "$@"; }
  echo "sudo is required to install binaries and D-Bus configuration under /usr." >&2
  if ! sudo -n true 2>/dev/null; then
    sudo -v 2>/dev/null || { echo "sudo required; aborting." >&2; exit 1; }
  fi
  # keep the credential fresh for the duration of the install
  ( while true; do sudo -n true 2>/dev/null || true; sleep 30; done ) &
  SUDO_GUARD=$!
  trap 'kill "$SUDO_GUARD" 2>/dev/null || true' EXIT
fi

# --- runtime environment -------------------------------------------------------
# In a chroot there is usually no logind, so XDG_RUNTIME_DIR is unset; pin it
# per-uid so the user/session bus and initd's user IPC socket land somewhere
# stable and world-resolvable by that uid's tools.
XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$RUN_UID}"
export XDG_RUNTIME_DIR
DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
export DBUS_SESSION_BUS_ADDRESS

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- arch / binary location ----------------------------------------------------
goarch() {
  local m
  m="$(uname -m)"
  case "$m" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "unsupported arch: $m" >&2; exit 1 ;;
  esac
}
ARCH="$(goarch)"

have_go=0
if command -v go >/dev/null 2>&1; then have_go=1; fi

INITD_BIN="${INITD_BIN:-}"
SYSTEMCTL_BIN="${SYSTEMCTL_BIN:-}"
LOGINCTL_BIN="${LOGINCTL_BIN:-}"
JOURNALCTL_BIN="${JOURNALCTL_BIN:-}"

find_binary() {
  local name="$1" arch="$2" cand
  for cand in \
    "$REPO_ROOT/bin/linux-$arch/$name" \
    "$REPO_ROOT/bin/$arch/$name" \
    "$REPO_ROOT/build/linux-$arch/$name" \
    "$REPO_ROOT/build/$arch/$name" \
    "$REPO_ROOT/$name" ; do
    if [ -x "$cand" ]; then printf '%s' "$cand"; return 0; fi
  done
  return 1
}

build_binary() {
  local cmd="$1" name="${1#cmd/}"
  echo "Building $cmd for linux/$ARCH with go ..." >&2
  ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -ldflags="-s -w" -o "/tmp/initd-install-$ARCH/$name" "./$cmd" ) >/dev/null
  printf '/tmp/initd-install-%s/%s' "$ARCH" "$name"
}

if [ -z "$INITD_BIN" ]; then
  INITD_BIN="$(find_binary initd "$ARCH" || true)"
fi
if [ -z "$SYSTEMCTL_BIN" ]; then
  SYSTEMCTL_BIN="$(find_binary systemctl "$ARCH" || true)"
fi
if [ -z "$LOGINCTL_BIN" ]; then
  LOGINCTL_BIN="$(find_binary loginctl "$ARCH" || true)"
fi
if [ -z "$JOURNALCTL_BIN" ]; then
  JOURNALCTL_BIN="$(find_binary journalctl "$ARCH" || true)"
fi
if [ -z "$INITD_BIN" ] && [ "$have_go" -eq 1 ]; then
  INITD_BIN="$(build_binary cmd/initd)"
fi
if [ -z "$SYSTEMCTL_BIN" ] && [ "$have_go" -eq 1 ]; then
  SYSTEMCTL_BIN="$(build_binary cmd/systemctl)"
fi
if [ -z "$LOGINCTL_BIN" ] && [ "$have_go" -eq 1 ]; then
  LOGINCTL_BIN="$(build_binary cmd/loginctl)"
fi
if [ -z "$JOURNALCTL_BIN" ] && [ "$have_go" -eq 1 ]; then
  JOURNALCTL_BIN="$(build_binary cmd/journalctl)"
fi

: "${INITD_BIN:?}"
: "${SYSTEMCTL_BIN:?}"
: "${LOGINCTL_BIN:?}"
: "${JOURNALCTL_BIN:?}"
echo "Using initd binary:      $INITD_BIN"
echo "Using systemctl binary:  $SYSTEMCTL_BIN"
echo "Using loginctl binary:   $LOGINCTL_BIN"
echo "Using journalctl binary: $JOURNALCTL_BIN"

INITD_DST="/usr/bin/initd"
SYSTEMCTL_DST="/usr/bin/systemctl"
LOGINCTL_DST="/usr/bin/loginctl"
JOURNALCTL_DST="/usr/bin/journalctl"
REAL_BACKUP="/usr/bin/systemctl.real.systemd255"
LOGINCTL_REAL_BACKUP="/usr/bin/loginctl.real"
JOURNALCTL_REAL_BACKUP="/usr/bin/journalctl.real"

# --- install binaries ----------------------------------------------------------
echo "Backing up real systemctl (if present and not already backed up)..."
if [ -f "$SYSTEMCTL_DST" ] && [ ! -f "$REAL_BACKUP" ]; then
  if file "$SYSTEMCTL_DST" 2>/dev/null | grep -q "dynamically linked"; then
    as_root cp "$SYSTEMCTL_DST" "$REAL_BACKUP"
  fi
fi
echo "Backing up real loginctl (if present and not already backed up)..."
if [ -f "$LOGINCTL_DST" ] && [ ! -f "$LOGINCTL_REAL_BACKUP" ]; then
  if file "$LOGINCTL_DST" 2>/dev/null | grep -q "dynamically linked"; then
    as_root cp "$LOGINCTL_DST" "$LOGINCTL_REAL_BACKUP"
  fi
fi
echo "Backing up real journalctl (if present and not already backed up)..."
if [ -f "$JOURNALCTL_DST" ] && [ ! -f "$JOURNALCTL_REAL_BACKUP" ]; then
  if file "$JOURNALCTL_DST" 2>/dev/null | grep -q "dynamically linked"; then
    as_root cp "$JOURNALCTL_DST" "$JOURNALCTL_REAL_BACKUP"
  fi
fi
echo "Installing initd -> $INITD_DST"
as_root install -m 0755 "$INITD_BIN" "$INITD_DST"
echo "Installing systemctl -> $SYSTEMCTL_DST"
as_root install -m 0755 "$SYSTEMCTL_BIN" "$SYSTEMCTL_DST"
echo "Installing loginctl -> $LOGINCTL_DST"
as_root install -m 0755 "$LOGINCTL_BIN" "$LOGINCTL_DST"
echo "Installing journalctl -> $JOURNALCTL_DST"
as_root install -m 0755 "$JOURNALCTL_BIN" "$JOURNALCTL_DST"

# --- D-Bus configuration -------------------------------------------------------
DBUS_SERVICE_FILE="/usr/share/dbus-1/services/org.freedesktop.systemd1.service"
DBUS_POLICY_FILE="/usr/share/dbus-1/system.d/org.freedesktop.systemd1.conf"
POLICY_SRC="$REPO_ROOT/internal/dbus/org.freedesktop.systemd1.system-policy.conf"

echo "Installing D-Bus activation service -> $DBUS_SERVICE_FILE"
as_root mkdir -p "$(dirname "$DBUS_SERVICE_FILE")"
tmp_service=$(mktemp)
cat > "$tmp_service" <<EOF
[D-BUS Service]
Name=org.freedesktop.systemd1
Exec=$INITD_DST --socket
EOF
as_root install -m 0644 "$tmp_service" "$DBUS_SERVICE_FILE"
rm -f "$tmp_service"

echo "Installing system bus policy -> $DBUS_POLICY_FILE"
if [ -f "$POLICY_SRC" ]; then
  as_root install -D -m 0644 "$POLICY_SRC" "$DBUS_POLICY_FILE"
else
  echo "WARN: policy source $POLICY_SRC not found; skipping policy install." >&2
fi

# --- autostart hook (runs `initd --init` on session login) ---------------------
# This exports a stable session-bus env for the user AND launches the daemon
# (init-lite: IPC + D-Bus systemd1 + enabled units) when not already running.
AUTOSTART_FILE="/etc/profile.d/initd.sh"
echo "Installing session-login autostart hook -> $AUTOSTART_FILE"
as_root mkdir -p "$(dirname "$AUTOSTART_FILE")"
tmp_autostart=$(mktemp)
cat > "$tmp_autostart" <<'EOSH'
# Installed by initd install.sh — expose org.freedesktop.systemd1 and start the
# initd supervisor on session login. systemd is not PID 1 here; initd provides
# the systemd1 D-Bus interface and unit supervision instead.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
# XDG requires the runtime dir to be private; enforce 0700 whether new or not.
mkdir -p "$XDG_RUNTIME_DIR" 2>/dev/null || true
# Never chmod /tmp or / itself if XDG_RUNTIME_DIR was overridden to shared space.
case "$XDG_RUNTIME_DIR" in
  /tmp|/) : ;;
  *) chmod 0700 "$XDG_RUNTIME_DIR" 2>/dev/null || true ;;
esac
if [ -S "$XDG_RUNTIME_DIR/bus" ]; then
    if ! dbus-send --session --dest=org.freedesktop.DBus --type=method_call /org/freedesktop/DBus org.freedesktop.DBus.ListNames >/dev/null 2>&1; then
        # Unresponsive bus: only replace the socket when no daemon is serving
        # it. Removing a live daemon's socket forks a second, competing bus.
        if pgrep -f "dbus-daemon.*$XDG_RUNTIME_DIR/bus" >/dev/null 2>&1; then
            echo "session bus unresponsive but its daemon is still running; leaving the socket alone." >&2
        else
            rm -f "$XDG_RUNTIME_DIR/bus"
            dbus-daemon --session --fork --address="$DBUS_SESSION_BUS_ADDRESS" --print-pid=1 >/dev/null 2>&1 || true
        fi
    fi
else
    dbus-daemon --session --fork --address="$DBUS_SESSION_BUS_ADDRESS" --print-pid=1 >/dev/null 2>&1 || true
fi
PIDFILE="$XDG_RUNTIME_DIR/initd.pid"
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; then
    : # daemon already running
else
    # A login spawned by sudo (or a chroot entrypoint) inherits SUDO_USER*,
    # which would make initd's RealUID() resolve to the sudo user and route
    # its user-manager at that user's units. Clear them so a root daemon is a
    # true root user-manager ($HOME=/root, /root/.config/systemd/user).
    unset SUDO_USER SUDO_UID SUDO_GID SUDO_COMMAND
    rm -f "$PIDFILE"
    # Self-detaching start: pid file + log handled by the daemon itself.
    # Fall back to the old nohup path for binaries predating --daemonize.
    if ! initd --init --daemonize --pid-file "$PIDFILE" >>"$XDG_RUNTIME_DIR/initd-daemon.log" 2>&1; then
      if ! pgrep -u "$(id -u)" -x initd >/dev/null 2>&1; then
        nohup initd --init >>"$XDG_RUNTIME_DIR/initd-daemon.log" 2>&1 &
        disown 2>/dev/null || true
      fi
    fi
fi
EOSH
as_root install -m 0755 "$tmp_autostart" "$AUTOSTART_FILE"
rm -f "$tmp_autostart"

# --- session bus for the daemon ------------------------------------------------
echo "Ensuring session bus at $XDG_RUNTIME_DIR/bus ..."
mkdir -p "$XDG_RUNTIME_DIR" 2>/dev/null || true
# Never chmod /tmp or / itself if XDG_RUNTIME_DIR was overridden to shared space.
case "$XDG_RUNTIME_DIR" in
  /tmp|/) : ;;
  *) chmod 0700 "$XDG_RUNTIME_DIR" 2>/dev/null || true ;;
esac
if [ -S "$XDG_RUNTIME_DIR/bus" ]; then
  if dbus-send --session --dest=org.freedesktop.DBus --type=method_call /org/freedesktop/DBus org.freedesktop.DBus.ListNames >/dev/null 2>&1; then
    echo "session bus socket already exists and is responsive."
  elif pgrep -f "dbus-daemon.*$XDG_RUNTIME_DIR/bus" >/dev/null 2>&1; then
    echo "session bus socket is unresponsive but its daemon is still running; leaving it alone (no duplicate bus)." >&2
  else
    echo "session bus socket is stale, removing and restarting..."
    rm -f "$XDG_RUNTIME_DIR/bus"
    dbus-daemon --session --fork --address="$DBUS_SESSION_BUS_ADDRESS" \
      --print-address=1 --print-pid=1 >/dev/null 2>&1 \
      || { echo "ERROR: could not start session dbus-daemon." >&2; exit 1; }
    sleep 1
  fi
else
  dbus-daemon --session --fork --address="$DBUS_SESSION_BUS_ADDRESS" \
    --print-address=1 --print-pid=1 >/dev/null 2>&1 \
    || { echo "ERROR: could not start session dbus-daemon." >&2; exit 1; }
  sleep 1
fi

# Never signal PID 1: if initd itself is the init system and this script runs
# as root, a bare `pkill -x initd` pattern can match PID 1. All kills below go
# through kill_initd_user, which filters it out.
kill_initd_user() {
  # $1 = signal number or name (default TERM). Kills exact-name initd
  # processes owned by $RUN_USER except PID 1. Never fails (|| true callers).
  local sig="${1:-TERM}"
  local pid
  for pid in $(pgrep -u "$RUN_USER" -x initd 2>/dev/null); do
    if [ "$pid" = "1" ]; then
      echo "refusing to signal PID 1 ($sig); use the control interface to manage a PID-1 initd." >&2
      continue
    fi
    kill "-$sig" "$pid" 2>/dev/null || true
  done
}

# --- (re)start the initd daemon as `initd --init` (starts enabled units) -------
# Everything here runs while the previous daemon may still be alive, so nothing
# may remove a socket the old daemon is still serving: that leaves it accepting
# on an inode no client can name, and the user loses systemctl entirely while
# their units keep running. The old daemon is stopped and *confirmed gone*
# first, and the sockets are then cleared only through initd's own connection
# check, which distinguishes a live listener from a leftover file.
echo "Stopping any existing initd daemon for $RUN_USER ..."
OLD_INITD_PIDS="$(pgrep -u "$RUN_USER" -x initd 2>/dev/null | grep -v '^1$' || true)"
if [ -n "$OLD_INITD_PIDS" ]; then
  kill_initd_user TERM
  # Wait for the old daemon to actually exit before touching its sockets. A
  # stale daemon that ignores SIGTERM would otherwise still hold them, and a
  # second daemon would split the supervisor in two (two listeners, two
  # competing restart loops). Escalate to SIGKILL after a short grace period,
  # then confirm again - SIGKILL is not instant, and a daemon still in the
  # process of dying must not be treated as gone.
  for _ in $(seq 1 20); do
    still_up=""
    for pid in $OLD_INITD_PIDS; do
      kill -0 "$pid" 2>/dev/null && still_up="yes"
    done
    [ -z "$still_up" ] && break
    sleep 0.25
  done
  still_up=""
  for pid in $OLD_INITD_PIDS; do
    kill -0 "$pid" 2>/dev/null && still_up="yes"
  done
  if [ -n "$still_up" ]; then
    echo "initd did not exit on SIGTERM; sending SIGKILL." >&2
    kill_initd_user KILL
    for _ in $(seq 1 20); do
      still_up=""
      for pid in $OLD_INITD_PIDS; do
        kill -0 "$pid" 2>/dev/null && still_up="yes"
      done
      [ -z "$still_up" ] && break
      sleep 0.25
    done
  fi
  if [ -n "$still_up" ]; then
    # Refuse to continue: starting a second supervisor beside a live one is
    # worse than not restarting, and clearing the sockets now would hide it.
    echo "ERROR: initd (PID $(echo "$OLD_INITD_PIDS" | tr '\n' ' ')) is still running and could not be stopped." >&2
    echo "       Not clearing its sockets and not starting a second daemon; resolve this by hand." >&2
    exit 1
  fi
fi

# Clear leftover sockets, but only ones nobody is serving. initd refuses to
# replace a path with a live listener, and reports the rest; treating that as
# fatal is deliberate, because it means something is serving this address that
# this script does not know about.
clear_stale_socket() {
  local path="$1" what="$2"
  [ -e "$path" ] || [ -S "$path" ] || return 0
  if command -v python3 >/dev/null 2>&1; then
    if python3 - "$path" <<'PYEOF'
import socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(1.0)
try:
    s.connect(sys.argv[1])
except Exception:
    sys.exit(1)   # nothing listening: safe to clear
finally:
    s.close()
sys.exit(0)       # a live listener holds this address
PYEOF
    then
      echo "ERROR: $what ($path) is still being served by a live daemon; leaving it alone." >&2
      return 1
    fi
  fi
  rm -f "$path" 2>/dev/null || true
  return 0
}

STALE_SOCKET=0
clear_stale_socket "$XDG_RUNTIME_DIR/initd.sock" "user control socket" || STALE_SOCKET=1
clear_stale_socket "$XDG_RUNTIME_DIR/initd-system.sock" "system control socket" || STALE_SOCKET=1
if [ "$RUN_UID" -eq 0 ]; then
  clear_stale_socket "/run/initd.sock" "system control socket" || STALE_SOCKET=1
fi
# The lock file is deliberately NOT removed. It carries no state a stale daemon
# can leave behind - it is flocked, and the kernel drops the lock when the
# holder dies - while deleting it destroys mutual exclusion for the window
# before the new daemon starts.
if [ "$STALE_SOCKET" -ne 0 ]; then
  echo "ERROR: a control socket is still served by a daemon this script does not own." >&2
  echo "       Not starting a second supervisor. Stop that daemon first, then re-run." >&2
  exit 1
fi

# Start the daemon with a clean identity: drop any SUDO_USER* inherited from
# the installer's invocation so the daemon's RealUID()/user-manager is the real
# invoking user (root here), not the sudo'ing user.
unset SUDO_USER SUDO_UID SUDO_GID SUDO_COMMAND
DAEMON_PIDFILE="$XDG_RUNTIME_DIR/initd.pid"
rm -f "$DAEMON_PIDFILE"
if ! "$INITD_DST" --init --daemonize --pid-file "$DAEMON_PIDFILE" >>"$XDG_RUNTIME_DIR/initd-daemon.log" 2>&1; then
  echo "daemonize failed, falling back to nohup start." >&2
  nohup "$INITD_DST" --init >"$XDG_RUNTIME_DIR/initd-daemon.log" 2>&1 &
  disown 2>/dev/null || true
  sleep 3
  # No pid file on this path: fall back to process-name readiness.
  if ! pgrep -u "$RUN_USER" -x initd >/dev/null 2>&1; then
    echo "ERROR: initd daemon failed to start. Log:" >&2
    tail -20 "$XDG_RUNTIME_DIR/initd-daemon.log" >&2 || true
    exit 1
  fi
else
  # Wait for the pid file (not just any matching process name) as readiness.
  for _ in $(seq 1 40); do
    if [ -f "$DAEMON_PIDFILE" ] && kill -0 "$(cat "$DAEMON_PIDFILE" 2>/dev/null)" 2>/dev/null; then
      break
    fi
    sleep 0.25
  done
  if ! [ -f "$DAEMON_PIDFILE" ] || ! kill -0 "$(cat "$DAEMON_PIDFILE" 2>/dev/null)" 2>/dev/null; then
    echo "ERROR: initd daemon failed to start. Log:" >&2
    tail -20 "$XDG_RUNTIME_DIR/initd-daemon.log" >&2 || true
    exit 1
  fi
fi

# --- verify --------------------------------------------------------------------
echo "Verifying org.freedesktop.systemd1 ownership ..."
if [ -S "$XDG_RUNTIME_DIR/bus" ]; then
  if dbus-send --session --dest=org.freedesktop.DBus --print-reply \
       --type=method_call /org/freedesktop/DBus org.freedesktop.DBus.GetNameOwner \
       string:org.freedesktop.systemd1 2>/dev/null | grep -q "string"; then
    echo "OK: org.freedesktop.systemd1 is owned on the session bus."
  else
    echo "WARN: org.freedesktop.systemd1 not yet owned on the session bus;" >&2
    echo "      see $XDG_RUNTIME_DIR/initd-daemon.log" >&2
  fi
fi

# System-bus ownership is non-fatal (only relevant if a system dbus-daemon is
# running with the installed policy); verify it opportunistically.
if pgrep -x dbus-daemon >/dev/null 2>&1 && [ -S /run/dbus/system_bus_socket ]; then
  if dbus-send --system --dest=org.freedesktop.DBus --print-reply \
      --type=method_call /org/freedesktop/DBus org.freedesktop.DBus.GetNameOwner \
      string:org.freedesktop.systemd1 2>/dev/null | grep -q "string"; then
    echo "OK: org.freedesktop.systemd1 is owned on the system bus."
  fi
fi

echo "Verifying unit resolution (graceful not-found, no real systemd required) ..."
if systemctl show --property=LoadState --value does-not-exist.service 2>/dev/null | grep -q "not-found"; then
  echo "OK: nonexistent unit resolves to 'not-found' gracefully."
fi

cat <<EOF

install.sh complete.

  initd daemon : $INITD_DST  (running as $RUN_USER, init-lite)
  systemctl     : $SYSTEMCTL_DST
  loginctl     : $LOGINCTL_DST
  journalctl   : $JOURNALCTL_DST
  real backup   : $REAL_BACKUP
  session bus   : $XDG_RUNTIME_DIR/bus
  autostart     : $AUTOSTART_FILE  (runs \`initd --init\` on each session login)

After this, on each session login, if the daemon is not running, initd --init starts and
brings up ALL enabled units automatically.

To install the openclaw gateway as a managed service owned by $RUN_USER:

  openclaw gateway install
  systemctl --user start openclaw-gateway.service   # if not auto-started

EOF
