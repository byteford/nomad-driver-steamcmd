#!/usr/bin/env bash
# E2E: install a REAL dedicated server with real steamcmd, start it through the
# driver, and prove it is serving by querying it over UDP.
#
# Half-Life Dedicated Server (app 90) is used because it is small, anonymous and
# long-lived (the same app the install test already uses). It answers the Source
# "A2S_INFO" query on its game port once it is up, so a reply means the game
# server itself is running, not just that a process exists.
#
# Needs real steamcmd and network access. Against a local agent:
#
#   NOMAD_ADDR=http://127.0.0.1:4646 e2e/gameserver-hlds.sh
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$E2E_DIR/lib.sh"

JOB=steamcmd-e2e-hlds
TASK=hlds
APP_ID="${TEST_APP_ID:-90}"
PORT=27015
GAME_ROOT="${E2E_HLDS_DIR:-/tmp/e2e-hlds}"
INSTALL="$GAME_ROOT/install"
# "TSource Engine Query\0" prefixed by 0xFFFFFFFF
A2S_INFO=ffffffff54536f7572636520456e67696e6520517565727900

cleanup() {
  nomad job stop -purge -detach "$JOB" >/dev/null 2>&1 || true
}
trap cleanup EXIT

diagnose() {
  dump_alloc "$ALLOC" "$TASK"
  echo "--- install_dir ---"
  ls -la "$INSTALL" 2>&1 | head -40 || true
  echo "--- server binaries ---"
  find "$INSTALL" -maxdepth 2 \( -name 'hlds_*' -o -name 'steam_appid.txt' -o -name '*.acf' \) 2>&1 | head -20 || true
  echo "--- processes ---"
  pgrep -a 'hlds' || true
}

rm -rf "$GAME_ROOT"
mkdir -p "$INSTALL"
chmod -R a+rwX "$GAME_ROOT"

cat >"$GAME_ROOT/job.nomad.hcl" <<EOF
job "$JOB" {
  datacenters = ["dc1"]
  type        = "service"

  group "game" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "$TASK" {
      driver       = "steamcmd"
      kill_timeout = "30s"

      config {
        app_id          = "$APP_ID"
        install_dir     = "$INSTALL"
        install_timeout = "15m"

        launch {
          command = "./hlds_run"
          args    = ["-game", "valve", "-port", "$PORT", "+maxplayers", "8", "+map", "crossfire", "+sv_lan", "1", "-norestart"]
        }
      }

      resources {
        cpu    = 1000
        memory = 1024
      }
    }
  }
}
EOF

echo "== install + start (StartTask installs synchronously, then launches) =="
nomad job run -detach "$GAME_ROOT/job.nomad.hcl"
have_alloc() { [ -n "$(latest_alloc "$JOB")" ]; }
wait_for "allocation exists" 30 have_alloc
ALLOC=$(latest_alloc "$JOB")
running() { [ "$(task_field "$ALLOC" "$TASK" State)" = running ]; }
wait_for "task running (install finished, server launched)" 900 running || { diagnose; exit 1; }

echo "== the game server must answer a Source query =="
wait_for "server answers A2S_INFO on udp/$PORT" 180 udp_query "$PORT" "$A2S_INFO" || { diagnose; exit 1; }
REPLY=$(udp_query "$PORT" "$A2S_INFO")
case "$REPLY" in
  ffffffff*) echo "server replied: ${REPLY:0:60}..." ;;
  *) diagnose; fail "unexpected reply to A2S_INFO: $REPLY" ;;
esac
[ "$(task_field "$ALLOC" "$TASK" State)" = running ] || { diagnose; fail "task is no longer running"; }

echo "== stop: the server and everything it spawned must go away =="
nomad job stop -detach "$JOB"
stopped() { [ "$(api "allocation/$ALLOC" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ClientStatus"])')" = complete ]; }
wait_for "allocation complete" 90 stopped || { diagnose; exit 1; }
gone() { ! pgrep -f "$INSTALL/hlds_linux|hlds_run" >/dev/null; }
wait_for "no hlds processes left" 30 gone || { diagnose; fail "hlds processes survived the stop"; }
if udp_query "$PORT" "$A2S_INFO" >/dev/null; then fail "udp/$PORT still answers after stop"; fi

echo "PASS: real app $APP_ID installed, started, answered a server query and stopped cleanly"
