#!/bin/sh
# Phase 0 harness: verifies the reference is captured and the tree builds.
# Later phases extend the BEHAVIOR section; Phase 0 only pins the oracle.
# Safe to run from any cwd: paths resolve from this script's location.
set -u
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fail=0
bad() { echo "FAIL: $1"; fail=1; }
ok() { echo "ok: $1"; }

[ -f "$ROOT/docs/compat-matrix.md" ] && ok "matrix present" || bad "docs/compat-matrix.md missing"
for f in /tmp/real-systemctl-help.txt /tmp/real-journalctl-help.txt /tmp/real-loginctl-help.txt; do
  if [ ! -s "$f" ]; then
    case "$f" in
      *systemctl*) systemctl --help > "$f" 2>&1 ;;
      *journalctl*) journalctl --help > "$f" 2>&1 ;;
      *loginctl*) loginctl --help > "$f" 2>&1 ;;
    esac
  fi
  [ -s "$f" ] && ok "reference $(basename "$f")" || bad "reference $f missing"
done
if [ "${SKIP_BUILD:-0}" != "1" ]; then
  # Sandboxes often mount the default build cache read-only; fall back to
  # a temp cache so the harness works there too.
  _gocache=$(go env GOCACHE 2>/dev/null)
  if [ -z "$_gocache" ] || [ ! -w "$_gocache" ]; then
    export GOCACHE=/tmp/gocache
    mkdir -p "$GOCACHE"
  fi
  (cd "$ROOT" && go build ./... >/tmp/compat-build.log 2>&1) \
    && ok "go build ./..." || { bad "go build ./... (see /tmp/compat-build.log)"; tail -5 /tmp/compat-build.log; }
fi
for v in systemctl journalctl loginctl initd; do
  "$ROOT/build/linux-amd64/$v" --help >/dev/null 2>&1 && ok "$v --help runs" || bad "$v --help fails"
done
echo "--- behavior checks (enforced since Phases 1-3) ---"
if "$ROOT/build/linux-amd64/systemctl" --no-pager status foo >/tmp/compat-sc.log 2>&1; then
  echo "systemctl --no-pager status foo exit=0 (daemon answered, unexpected here)"
else
  if grep -q "Usage:" /tmp/compat-sc.log; then
    bad "systemctl --no-pager status foo rejected at parse"
  else
    ok "systemctl before-verb flags parse"
  fi
fi
if "$ROOT/build/linux-amd64/journalctl" --help 2>&1 | grep -q "short-delta"; then
  ok "journalctl short-delta present"
else
  bad "journalctl short-delta missing"
fi
if "$ROOT/build/linux-amd64/journalctl" --help 2>&1 | grep -q -- "--list-invocations"; then
  ok "journalctl invocation flags present"
else
  bad "journalctl invocation flags missing"
fi
if "$ROOT/build/linux-amd64/loginctl" show-user root 2>&1 | grep -q "^Name="; then
  ok "loginctl show-user col0"
else
  bad "loginctl show-user col0"
fi
if "$ROOT/build/linux-amd64/loginctl" --nolegend list-users >/tmp/compat-lc.log 2>&1; then
  bad "loginctl typo flag accepted"
else
  ok "loginctl typo flag rejected"
fi
if "$ROOT/build/linux-amd64/loginctl" --help 2>&1 | grep -q "list-sessions"; then
  ok "loginctl session verbs present"
else
  bad "loginctl session verbs missing"
fi
if [ "$fail" -ne 0 ]; then echo "PROBE FAILED"; exit 1; fi
echo "PROBE PASSED"
