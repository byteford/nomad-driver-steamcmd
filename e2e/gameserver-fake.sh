#!/usr/bin/env bash
# E2E: can the driver start, supervise and stop a game server? No Steam needed.
#
# Runs a stand-in server (e2e/server.py) through the real Nomad -> driver path
# exactly like a production server restart does: install_dir already holds an
# installed app (update_on_start=false, so steamcmd is skipped), the server
# files arrive via template + config_files, and the process is launched,
# queried, crashed, restarted and stopped. Fast and deterministic, so it also
# works as a local check against any running agent that has the plugin loaded:
#
#   NOMAD_ADDR=http://127.0.0.1:4646 e2e/gameserver-fake.sh
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$E2E_DIR/lib.sh"

JOB=steamcmd-e2e-gameserver
TASK=server
APP_ID=9999
PORT=27099
GAME_ROOT="${E2E_GAME_DIR:-/tmp/e2e-gameserver}"
INSTALL="$GAME_ROOT/install"

cleanup() {
  nomad job stop -purge -detach "$JOB" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# An "installed" app: steamcmd leaves steamapps/appmanifest_<id>.acf behind.
rm -rf "$GAME_ROOT"
mkdir -p "$INSTALL/steamapps"
# StateFlags 4 = fully installed, which is what the driver checks before it
# skips steamcmd.
printf '"AppState"\n{\n\t"appid"\t\t"%s"\n\t"StateFlags"\t\t"4"\n}\n' "$APP_ID" >"$INSTALL/steamapps/appmanifest_${APP_ID}.acf"
chmod -R a+rwX "$GAME_ROOT"

cat >"$GAME_ROOT/job.nomad.hcl" <<EOF
job "$JOB" {
  datacenters = ["dc1"]
  type        = "service"

  group "game" {
    restart {
      attempts = 2
      interval = "5m"
      delay    = "1s"
      mode     = "fail"
    }

    task "$TASK" {
      driver      = "steamcmd"
      kill_signal  = "SIGINT"
      kill_timeout = "20s"

      # Inline rather than "source": Nomad's template file sandbox refuses
      # sources outside the alloc dir.
      template {
        destination = "local/server.py"
        data        = <<PYEOF
$(cat "$E2E_DIR/server.py")
PYEOF
      }

      config {
        app_id          = "$APP_ID"
        install_dir     = "$INSTALL"
        update_on_start = false
        config_files    = { "local/server.py" = "server.py" }

        launch {
          command = "python3"
          args    = ["server.py", "$PORT"]
        }
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
EOF

echo "== start =="
nomad job run -detach "$GAME_ROOT/job.nomad.hcl"
have_alloc() { [ -n "$(latest_alloc "$JOB")" ]; }
wait_for "allocation exists" 30 have_alloc
ALLOC=$(latest_alloc "$JOB")
running() { [ "$(task_field "$ALLOC" "$TASK" State)" = running ]; }
wait_for "task running" 60 running || { dump_alloc "$ALLOC" "$TASK"; exit 1; }
wait_for "server answers on udp/$PORT" 30 udp_query "$PORT" 70696e67 ||
  { dump_alloc "$ALLOC" "$TASK"; exit 1; }
[ "$(udp_query "$PORT" 70696e67)" = "504f4e472070696e67" ] || # "PONG ping"
  fail "server replied, but not with the expected payload"
nomad alloc logs "$ALLOC" "$TASK" | grep -q "server listening" ||
  fail "server stdout did not reach nomad alloc logs"
[ -s "$INSTALL/server.py" ] || fail "config_files did not stage server.py into install_dir"
PID1=$(cat "$INSTALL/server.pid")
echo "server up, pid $PID1"

echo "== crash -> Nomad restarts it =="
kill -9 "$PID1" 2>/dev/null || sudo kill -9 "$PID1"
restarted() {
  [ "$(task_field "$ALLOC" "$TASK" Restarts)" -ge 1 ] && [ "$(task_field "$ALLOC" "$TASK" State)" = running ] &&
    [ -s "$INSTALL/server.pid" ] && [ "$(cat "$INSTALL/server.pid")" != "$PID1" ]
}
wait_for "task restarted with a new process" 60 restarted || { dump_alloc "$ALLOC" "$TASK"; exit 1; }
wait_for "restarted server answers on udp/$PORT" 30 udp_query "$PORT" 70696e67 ||
  { dump_alloc "$ALLOC" "$TASK"; exit 1; }
PID2=$(cat "$INSTALL/server.pid")
echo "server back, pid $PID2"

echo "== graceful stop: server must get its kill_signal and save =="
rm -f "$INSTALL/save.txt"
nomad job stop -detach "$JOB"
stopped() { [ "$(api "allocation/$ALLOC" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ClientStatus"])')" = complete ]; }
wait_for "allocation complete" 60 stopped || { dump_alloc "$ALLOC" "$TASK"; exit 1; }
grep -q "saved on SIGINT" "$INSTALL/save.txt" 2>/dev/null ||
  { dump_alloc "$ALLOC" "$TASK"; fail "server was not given SIGINT and a chance to save before exit"; }
if kill -0 "$PID2" 2>/dev/null; then fail "server process $PID2 still alive after the job was stopped"; fi
if udp_query "$PORT" 70696e67 >/dev/null; then fail "udp/$PORT still answers after stop"; fi

echo "== kill_signal SIGQUIT reaches the server (not downgraded to SIGTERM) =="
# Same job on its own install_dir/port with kill_signal = SIGQUIT. The driver
# used to reject SIGQUIT and silently send SIGTERM, so the server's save file
# would say SIGTERM.
QJOB="$JOB-quit"
QINSTALL="$GAME_ROOT/install-quit"
QPORT=27098
trap 'cleanup; nomad job stop -purge -detach "$QJOB" >/dev/null 2>&1 || true' EXIT
mkdir -p "$QINSTALL/steamapps"
cp "$INSTALL/steamapps/appmanifest_${APP_ID}.acf" "$QINSTALL/steamapps/"
chmod -R a+rwX "$QINSTALL"
sed -e "s/^job \"$JOB\"/job \"$QJOB\"/" \
  -e "s#$INSTALL#$QINSTALL#" \
  -e 's/kill_signal  = "SIGINT"/kill_signal  = "SIGQUIT"/' \
  -e "s/\"$PORT\"/\"$QPORT\"/" \
  "$GAME_ROOT/job.nomad.hcl" >"$GAME_ROOT/job-quit.nomad.hcl"
grep -q 'kill_signal  = "SIGQUIT"' "$GAME_ROOT/job-quit.nomad.hcl" || fail "test bug: SIGQUIT job was not generated"
nomad job run -detach "$GAME_ROOT/job-quit.nomad.hcl"
have_qalloc() { [ -n "$(latest_alloc "$QJOB")" ]; }
wait_for "SIGQUIT allocation exists" 30 have_qalloc
QALLOC=$(latest_alloc "$QJOB")
qrunning() { [ "$(task_field "$QALLOC" "$TASK" State)" = running ]; }
wait_for "SIGQUIT task running" 60 qrunning || { dump_alloc "$QALLOC" "$TASK"; exit 1; }
wait_for "SIGQUIT server answers on udp/$QPORT" 30 udp_query "$QPORT" 70696e67 ||
  { dump_alloc "$QALLOC" "$TASK"; exit 1; }
nomad job stop -detach "$QJOB"
qstopped() { [ "$(api "allocation/$QALLOC" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ClientStatus"])')" = complete ]; }
wait_for "SIGQUIT allocation complete" 60 qstopped || { dump_alloc "$QALLOC" "$TASK"; exit 1; }
grep -q "saved on SIGQUIT" "$QINSTALL/save.txt" 2>/dev/null ||
  { dump_alloc "$QALLOC" "$TASK"; fail "server was not given SIGQUIT (save file: $(cat "$QINSTALL/save.txt" 2>/dev/null || echo none))"; }
trap cleanup EXIT

echo "PASS: game server started, served, restarted after a crash, saved and exited on stop"
