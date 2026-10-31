#!/usr/bin/env bash
# Helpers shared by the e2e/*.sh scripts. Source, do not execute.
# Needs: nomad (CLI), curl, python3. NOMAD_ADDR selects the agent under test.

NOMAD_ADDR="${NOMAD_ADDR:-http://127.0.0.1:4646}"
export NOMAD_ADDR

api() { curl -sf "$NOMAD_ADDR/v1/$1"; }

# latest_alloc <job>: ID of the newest allocation of a job ("" if none).
latest_alloc() {
  api "job/$1/allocations" | python3 -c '
import sys, json
a = sorted(json.load(sys.stdin), key=lambda x: x["CreateIndex"])
print(a[-1]["ID"] if a else "")'
}

# task_field <alloc> <task> <State|Restarts|Failed>
task_field() {
  api "allocation/$1" | python3 -c '
import sys, json
t = (json.load(sys.stdin).get("TaskStates") or {}).get(sys.argv[1]) or {}
print(t.get(sys.argv[2], ""))' "$2" "$3"
}

# task_events <alloc> <task>: "Type|DisplayMessage" per line, oldest first.
task_events() {
  api "allocation/$1" | python3 -c '
import sys, json
t = (json.load(sys.stdin).get("TaskStates") or {}).get(sys.argv[1]) or {}
for e in t.get("Events") or []:
    print("%s|%s" % (e.get("Type"), e.get("DisplayMessage")))' "$2"
}

# udp_query <port> <hex payload>: prints the hex of the first reply, or fails
# if nothing comes back within 1s. Used as a "is the game actually serving" probe.
udp_query() {
  python3 - "$1" "$2" <<'EOF'
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(1)
try:
    s.sendto(bytes.fromhex(sys.argv[2]), ("127.0.0.1", int(sys.argv[1])))
    print(s.recv(4096).hex())
except OSError:
    sys.exit(1)
EOF
}

# wait_for <description> <seconds> <command...>: poll a command until it succeeds.
wait_for() {
  local desc=$1 secs=$2
  shift 2
  for _ in $(seq 1 "$secs"); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $desc"
      return 0
    fi
    sleep 1
  done
  echo "::error::timed out after ${secs}s waiting for: $desc"
  return 1
}

# dump_alloc <alloc> <task>: everything useful to diagnose a failure.
dump_alloc() {
  echo "--- alloc status ---"
  nomad alloc status -verbose "$1" || true
  echo "--- task events ---"
  task_events "$1" "$2" || true
  echo "--- stdout ---"
  nomad alloc logs "$1" "$2" || true
  echo "--- stderr ---"
  nomad alloc logs -stderr "$1" "$2" || true
}

fail() {
  echo "::error::$*"
  exit 1
}
