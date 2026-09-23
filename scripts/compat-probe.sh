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
  (cd "$ROOT" && GOCACHE=/tmp/gocache GOPROXY=off go build ./... >/tmp/compat-build.log 2>&1) \
    && ok "go build ./..." || { bad "go build ./... (see /tmp/compat-build.log)"; tail -5 /tmp/compat-build.log; }
fi
for v in systemctl journalctl loginctl initd; do
  "$ROOT/build/linux-amd64/$v" --help >/dev/null 2>&1 && ok "$v --help runs" || bad "$v --help fails"
done
echo "--- behavior spot checks (informational in Phase 0) ---"
if "$ROOT/build/linux-amd64/systemctl" --no-pager status foo >/tmp/compat-sc.log 2>&1; then
  echo "systemctl --no-pager status foo exit=0 (daemon answered, unexpected here)"
else
  if grep -q "Usage:" /tmp/compat-sc.log; then
    echo "systemctl --no-pager status foo: rejected at parse (Phase 1 target: parses past Usage)"
  else
    echo "systemctl --no-pager status foo: parsed, failed past parse (dial, Phase 1 parse part done)"
  fi
fi
"$ROOT/build/linux-amd64/journalctl" --help 2>&1 | grep -q "short-delta" && echo "journalctl short-delta: present" || echo "journalctl short-delta: missing (Phase 2)"
"$ROOT/build/linux-amd64/loginctl" show-user root 2>&1 | grep -q "^Name=" && echo "loginctl show-user col0: yes" || echo "loginctl show-user col0: no (Phase 3)"
if [ "$fail" -ne 0 ]; then echo "PROBE FAILED"; exit 1; fi
echo "PROBE PASSED"
