#!/bin/bash
# Deploy script: expose org.freedesktop.systemd1 over D-Bus via initd on an
# initd-backed host so real /usr/bin/systemctl (system scope) and openclaw's
# ownership probe get verifiable answers instead of
# "Failed to connect to bus: Permission denied".
#
# This installs the D-Bus policy + bus-activation service file that point
# org.freedesktop.systemd1 at the initd daemon, then restarts services.
#
# Usage (as a user with sudo):
#   sudo bash deploy-systemd1.sh
#
# Prereqs: build both binaries for this host first, e.g.
#   make build GOOS=linux GOARCH=arm64   (produces build/linux-arm64/{initd,systemctl})
# The script installs them into /usr/bin so that any tool on PATH (which on
# this host lists /usr/bin before /usr/local/bin, e.g. npm/node-managed shells)
# resolves `systemctl` -> the initd wrapper. The real systemd systemctl is
# preserved at /usr/bin/systemctl.real.systemd255 (systemd is not PID 1 here).
#
INITD_BIN_SRC="${INITD_BIN_SRC:-build/linux-arm64/initd}"
SYSTEMCTL_BIN_SRC="${SYSTEMCTL_BIN_SRC:-build/linux-arm64/systemctl}"
INITD_BIN="${INITD_BIN:-/usr/bin/initd}"
SYSTEMCTL_BIN="${SYSTEMCTL_BIN:-/usr/bin/systemctl}"
REAL_SYSTEMCTL_BACKUP="${REAL_SYSTEMCTL_BACKUP:-/usr/bin/systemctl.real.systemd255}"
RUN_USER="${RUN_USER:-pkm}"

SYSTEMD1_SERVICE="/usr/share/dbus-1/services/org.freedesktop.systemd1.service"
SYSTEMD1_POLICY="/usr/share/dbus-1/system.d/org.freedesktop.systemd1.conf"

set -euo pipefail

# Install the initd + systemctl binaries into /usr/bin (canonical location).
echo "Installing initd -> $INITD_BIN ..."
install -D -m 0755 "$INITD_BIN_SRC" "$INITD_BIN"
echo "Installing systemctl wrapper -> $SYSTEMCTL_BIN ..."
# Preserve the real systemd systemctl if present (systemd is not PID 1 here,
# so /usr/bin/systemctl is non-functional and gets shadowed by the wrapper).
if [ -f "$SYSTEMCTL_BIN" ] && ! [ -f "$REAL_SYSTEMCTL_BACKUP" ]; then
	if file "$SYSTEMCTL_BIN" | grep -q "dynamically linked"; then
		cp "$SYSTEMCTL_BIN" "$REAL_SYSTEMCTL_BACKUP"
	fi
fi
install -m 0755 "$SYSTEMCTL_BIN_SRC" "$SYSTEMCTL_BIN"

echo "Installing org.freedesktop.systemd1 D-Bus service file..."
cat > "$SYSTEMD1_SERVICE" <<EOF
[D-BUS Service]
Name=org.freedesktop.systemd1
Exec=$INITD_BIN --socket
User=$RUN_USER
EOF
chmod 0644 "$SYSTEMD1_SERVICE"

echo "Installing system bus policy..."
install -D -m 0644 \
  "$(dirname "$0")/org.freedesktop.systemd1.system-policy.conf" \
  "$SYSTEMD1_POLICY"

echo "Reloading/restarting the system D-Bus daemon to pick up the new policy..."
if pgrep -x dbus-daemon >/dev/null 2>&1; then
  # A system dbus-daemon is running; restart it so the new policy takes effect
  # and any stale systemd1 name owner is released.
  pkill -x dbus-daemon || true
  sleep 1
fi
rm -f /run/dbus/pid
dbus-daemon --system --fork
sleep 1

echo "Restarting initd daemon (owns org.freedesktop.systemd1 on both buses)..."
pkill -u "$RUN_USER" -x initd 2>/dev/null || true
sleep 1
rm -f "/run/user/$(id -u "$RUN_USER")/initd.lock" \
      "/run/user/$(id -u "$RUN_USER")/initd.sock" \
      "/run/user/$(id -u "$RUN_USER")/initd-system.sock" 2>/dev/null || true

# Launch the daemon as the non-root user so it creates user-owned sockets and
# can own the systemd1 name on the user/session bus; system-bus ownership is
# granted by the installed policy above.
sudo -u "$RUN_USER" \
  XDG_RUNTIME_DIR="/run/user/$(id -u "$RUN_USER")" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u "$RUN_USER")/bus" \
  nohup "$INITD_BIN" --socket >/dev/null 2>&1 &
sleep 2

echo "Verifying..."
if dbus-send --system --dest=org.freedesktop.DBus --print-reply \
     --type=method_call /org/freedesktop/DBus \
     org.freedesktop.DBus.GetNameOwner string:org.freedesktop.systemd1 \
     | grep -q "string"; then
  echo "OK: org.freedesktop.systemd1 is owned on the system bus."
else
  echo "WARN: org.freedesktop.systemd1 not owned on the system bus." >&2
fi
