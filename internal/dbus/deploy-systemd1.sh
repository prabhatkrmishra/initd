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
# Prereqs: /usr/local/bin/initd (>=1.0.0, with the internal/dbus package),
#          a running system dbus-daemon, and the initd system dbus config
#          policy installed next to this script as
#          org.freedesktop.systemd1.system-policy.conf.
#
set -euo pipefail

SYSTEMD1_SERVICE="/usr/share/dbus-1/services/org.freedesktop.systemd1.service"
SYSTEMD1_POLICY="/usr/share/dbus-1/system.d/org.freedesktop.systemd1.conf"
INITD_BIN="${INITD_BIN:-/usr/local/bin/initd}"
RUN_USER="${RUN_USER:-pkm}"

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
pkill -u "$RUN_USER" -f "$INITD_BIN --socket" 2>/dev/null || true
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
