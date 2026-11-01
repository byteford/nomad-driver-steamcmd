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

      # A private file: staging must keep its 0600 mode (checked below).
      template {
        destination = "local/secret.cfg"
        perms       = "0600"
        data        = "token=not-a-real-secret"
      }

      config {
        app_id          = "$APP_ID"
        install_dir     = "$INSTALL"
        update_on_start = false
        config_files    = { "local/server.py" = "server.py", "local/secret.cfg" = "secret.cfg" }

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

echo "== app not installed -> steamcmd must run, server must not be launched =="
# update_on_start=false only skips steamcmd for a fully installed app. Here
# install_dir holds a leftover steamapps/ directory but no manifest, which used
# to count as "installed": the driver skipped steamcmd and launched a server
# from a half-empty directory. Now it must run steamcmd, which cannot install
# this made-up app, so the task must fail without ever starting the server.
NJOB="$JOB-nomanifest"
NINSTALL="$GAME_ROOT/install-nomanifest"
trap 'cleanup; nomad job stop -purge -detach "$NJOB" >/dev/null 2>&1 || true' EXIT
mkdir -p "$NINSTALL/steamapps"
chmod -R a+rwX "$NINSTALL"
sed -e "s/^job \"$JOB\"/job \"$NJOB\"/" -e "s#$INSTALL#$NINSTALL#" \
  "$GAME_ROOT/job.nomad.hcl" >"$GAME_ROOT/job-nomanifest.nomad.hcl"
nomad job run -detach "$GAME_ROOT/job-nomanifest.nomad.hcl"
have_nalloc() { [ -n "$(latest_alloc "$NJOB")" ]; }
wait_for "no-manifest allocation exists" 30 have_nalloc
NALLOC=$(latest_alloc "$NJOB")
# Running is the failure we are guarding against; dead (failed) is the pass.
ndead() { s=$(task_field "$NALLOC" "$TASK" State); [ "$s" = dead ] || [ "$s" = running ]; }
wait_for "no-manifest task settles" 240 ndead || { dump_alloc "$NALLOC" "$TASK"; exit 1; }
[ "$(task_field "$NALLOC" "$TASK" State)" = dead ] ||
  { dump_alloc "$NALLOC" "$TASK"; fail "task launched the server without an installed app manifest (steamcmd was skipped)"; }
[ "$(task_field "$NALLOC" "$TASK" Failed)" = True ] ||
  { dump_alloc "$NALLOC" "$TASK"; fail "no-manifest task ended without failing"; }
[ ! -e "$NINSTALL/server.pid" ] || fail "server ran even though the app was not installed"
nomad job stop -purge -detach "$NJOB" >/dev/null
trap cleanup EXIT

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
[ -s "$INSTALL/secret.cfg" ] || fail "config_files did not stage secret.cfg into install_dir"
# The source is rendered 0600; staging must not widen it to 0644.
[ "$(stat -c %a "$INSTALL/secret.cfg")" = 600 ] ||
  fail "staged secret.cfg is mode $(stat -c %a "$INSTALL/secret.cfg"), expected 600 (the source's mode)"
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

echo "== ready_port: a server that never opens its port is killed and restarted; a healthy one is left alone =="
# HJOB runs `sleep 301`, which never listens on udp/27097. With ready_timeout=8s
# the driver must kill it, exit non-zero, and let Nomad's restart policy retry
# (attempts=1, then the task fails for good). Without the check it would sit
# "running" forever.
HJOB="$JOB-hung"
HINSTALL="$GAME_ROOT/install-hung"
HPORT=27097
RJOB="$JOB-ready"
RINSTALL="$GAME_ROOT/install-ready"
RPORT=27096
trap 'cleanup; nomad job stop -purge -detach "$HJOB" >/dev/null 2>&1 || true; nomad job stop -purge -detach "$RJOB" >/dev/null 2>&1 || true' EXIT
mkdir -p "$HINSTALL/steamapps"
cp "$INSTALL/steamapps/appmanifest_${APP_ID}.acf" "$HINSTALL/steamapps/"
chmod -R a+rwX "$HINSTALL"
cat >"$GAME_ROOT/job-hung.nomad.hcl" <<HUNGEOF
job "$HJOB" {
  datacenters = ["dc1"]
  type        = "service"

  group "game" {
    restart {
      attempts = 1
      interval = "5m"
      delay    = "1s"
      mode     = "fail"
    }

    task "$TASK" {
      driver = "steamcmd"

      config {
        app_id          = "$APP_ID"
        install_dir     = "$HINSTALL"
        update_on_start = false
        ready_port      = $HPORT
        ready_protocol  = "udp"
        ready_timeout   = "8s"

        launch {
          command = "sleep"
          args    = ["301"]
        }
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
HUNGEOF
nomad job run -detach "$GAME_ROOT/job-hung.nomad.hcl"
have_halloc() { [ -n "$(latest_alloc "$HJOB")" ]; }
wait_for "hung-server allocation exists" 30 have_halloc
HALLOC=$(latest_alloc "$HJOB")
hdead() { [ "$(task_field "$HALLOC" "$TASK" State)" = dead ]; }
wait_for "hung server is killed, restarted once, then fails" 120 hdead || { dump_alloc "$HALLOC" "$TASK"; exit 1; }
[ "$(task_field "$HALLOC" "$TASK" Failed)" = True ] ||
  { dump_alloc "$HALLOC" "$TASK"; fail "a server that never opened its port ended without failing"; }
[ "$(task_field "$HALLOC" "$TASK" Restarts)" -ge 1 ] ||
  { dump_alloc "$HALLOC" "$TASK"; fail "Nomad did not restart the server that never became ready"; }
task_events "$HALLOC" "$TASK" | grep -q "not listening" ||
  { dump_alloc "$HALLOC" "$TASK"; fail "exit reason does not mention the port that never opened"; }
if pgrep -f '^sleep 301$' >/dev/null; then fail "the hung server's process survived the not-ready kill"; fi
nomad job stop -purge -detach "$HJOB" >/dev/null

# RJOB is the normal stand-in server with the same check and a short deadline.
# It opens its port at once, so it must still be running, with no restarts,
# well after the deadline has passed.
mkdir -p "$RINSTALL/steamapps"
cp "$INSTALL/steamapps/appmanifest_${APP_ID}.acf" "$RINSTALL/steamapps/"
chmod -R a+rwX "$RINSTALL"
sed -e "s/^job \"$JOB\"/job \"$RJOB\"/" -e "s#$INSTALL#$RINSTALL#" -e "s/\"$PORT\"/\"$RPORT\"/" \
  -e "s/^        update_on_start = false/        update_on_start = false\n        ready_port      = $RPORT\n        ready_protocol  = \"udp\"\n        ready_timeout   = \"6s\"/" \
  "$GAME_ROOT/job.nomad.hcl" >"$GAME_ROOT/job-ready.nomad.hcl"
grep -q "ready_port      = $RPORT" "$GAME_ROOT/job-ready.nomad.hcl" || fail "test bug: ready_port job was not generated"
nomad job run -detach "$GAME_ROOT/job-ready.nomad.hcl"
have_ralloc() { [ -n "$(latest_alloc "$RJOB")" ]; }
wait_for "ready-check allocation exists" 30 have_ralloc
RALLOC=$(latest_alloc "$RJOB")
rrunning() { [ "$(task_field "$RALLOC" "$TASK" State)" = running ]; }
wait_for "ready-check task running" 60 rrunning || { dump_alloc "$RALLOC" "$TASK"; exit 1; }
wait_for "ready-check server answers on udp/$RPORT" 30 udp_query "$RPORT" 70696e67 ||
  { dump_alloc "$RALLOC" "$TASK"; exit 1; }
sleep 12
[ "$(task_field "$RALLOC" "$TASK" State)" = running ] && [ "$(task_field "$RALLOC" "$TASK" Restarts)" = 0 ] ||
  { dump_alloc "$RALLOC" "$TASK"; fail "a healthy server with ready_port was killed or restarted"; }
nomad job stop -purge -detach "$RJOB" >/dev/null
trap cleanup EXIT

echo "PASS: game server started, served, restarted after a crash, saved and exited on stop"
