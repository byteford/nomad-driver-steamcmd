#!/usr/bin/env bash
# E2E: does a Windows server start under REAL Proton through the driver?
#
# Downloads a pinned GE-Proton, cross-compiles a tiny Windows UDP server
# (e2e/winserver.c, a real PE executable), and runs it with
# platform = "windows" / runner = "proton" exactly as a production Windows
# server is run. Passes only if the .exe itself answers over UDP, Proton created
# its prefix, and nothing (server.exe, wineserver) survives a stop.
#
# No Steam involved: the app counts as installed (fully-installed appmanifest,
# update_on_start=false), as it does on a restart of a real server.
#
# CI downloads Proton and installs mingw-w64 itself. Elsewhere set
# E2E_DOWNLOAD_PROTON=1 to allow the ~450MB download, or PROTON_DIR to an
# existing Proton directory. E2E_FAKE_EXE=1 + a stand-in `proton` script is only
# for checking this script's own logic without Proton (see PR description).
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$E2E_DIR/lib.sh"

JOB=steamcmd-e2e-proton
TASK=server
APP_ID=9998
PORT=27098
PROTON_VERSION="${PROTON_VERSION:-GE-Proton9-20}"
PROTON_DIR="${PROTON_DIR:-/tmp/e2e-proton/$PROTON_VERSION}"
NOMAD_DATA_DIR="${NOMAD_DATA_DIR:-/tmp/nomad-data}"
GAME_ROOT="${E2E_PROTON_GAME_DIR:-/tmp/e2e-proton-game}"
INSTALL="$GAME_ROOT/install"

cleanup() {
  nomad job stop -purge -detach "$JOB" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---- 1. a real Proton ---------------------------------------------------
if [ ! -f "$PROTON_DIR/proton" ]; then
  if [ -z "${GITHUB_ACTIONS:-}" ] && [ -z "${E2E_DOWNLOAD_PROTON:-}" ]; then
    fail "no Proton at $PROTON_DIR; set PROTON_DIR, or E2E_DOWNLOAD_PROTON=1 to download $PROTON_VERSION"
  fi
  base="https://github.com/GloriousEggroll/proton-ge-custom/releases/download/$PROTON_VERSION"
  work="$(dirname "$PROTON_DIR")"
  mkdir -p "$work"
  echo "== downloading $PROTON_VERSION =="
  curl -fsSL -o "$work/$PROTON_VERSION.tar.gz" "$base/$PROTON_VERSION.tar.gz"
  curl -fsSL -o "$work/$PROTON_VERSION.sha512sum" "$base/$PROTON_VERSION.sha512sum"
  (cd "$work" && sha512sum -c "$PROTON_VERSION.sha512sum")
  tar -xzf "$work/$PROTON_VERSION.tar.gz" -C "$work"
  rm -f "$work/$PROTON_VERSION.tar.gz"
fi
[ -f "$PROTON_DIR/proton" ] || fail "Proton script missing at $PROTON_DIR/proton after setup"
echo "using Proton at $PROTON_DIR"

# ---- 2. a real Windows executable --------------------------------------
rm -rf "$GAME_ROOT" 2>/dev/null || sudo rm -rf "$GAME_ROOT"
mkdir -p "$INSTALL/steamapps"
if [ -n "${E2E_FAKE_EXE:-}" ]; then
  : >"$INSTALL/server.exe"
else
  if ! command -v x86_64-w64-mingw32-gcc >/dev/null; then
    [ -n "${GITHUB_ACTIONS:-}" ] || fail "x86_64-w64-mingw32-gcc not found (apt install mingw-w64)"
    sudo apt-get update -y >/dev/null
    sudo apt-get install -y mingw-w64 >/dev/null
  fi
  x86_64-w64-mingw32-gcc -O2 -o "$INSTALL/server.exe" "$E2E_DIR/winserver.c" -lws2_32
  head -c 2 "$INSTALL/server.exe" | grep -q MZ || fail "server.exe is not a Windows executable"
fi
printf '"AppState"\n{\n\t"appid"\t\t"%s"\n\t"StateFlags"\t\t"4"\n}\n' "$APP_ID" \
  >"$INSTALL/steamapps/appmanifest_${APP_ID}.acf"
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
        update_on_start = false
        platform        = "windows"
        runner          = "proton"
        proton_path     = "$PROTON_DIR"

        launch {
          command = "server.exe"
          args    = ["$PORT"]
        }
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
EOF

diagnose() {
  dump_alloc "$ALLOC" "$TASK"
  echo "--- compat dir ---"
  ls -la "$NOMAD_DATA_DIR/alloc/$ALLOC/$TASK/local/compat" 2>&1 | head -20 || true
  echo "--- wine processes ---"
  pgrep -a 'wineserver|server.exe|proton' || true
}

echo "== start under Proton (first start builds the prefix: slow) =="
nomad job run -detach "$GAME_ROOT/job.nomad.hcl"
have_alloc() { [ -n "$(latest_alloc "$JOB")" ]; }
wait_for "allocation exists" 30 have_alloc
ALLOC=$(latest_alloc "$JOB")

answered=false
for _ in $(seq 1 420); do
  if REPLY=$(udp_query "$PORT" 70696e67 2>/dev/null); then answered=true; break; fi
  state=$(task_field "$ALLOC" "$TASK" State)
  if [ "$state" = dead ]; then
    echo "::error::task died before the Windows server answered"
    diagnose
    exit 1
  fi
  sleep 1
done
$answered || { echo "::error::no reply from the Windows server within 420s"; diagnose; exit 1; }
# "WINPONG ping": only server.exe can produce this.
[ "$REPLY" = "57494e504f4e472070696e67" ] || { diagnose; fail "unexpected reply from the Windows server: $REPLY"; }
echo "ok: server.exe answered under Proton"

PFX="$NOMAD_DATA_DIR/alloc/$ALLOC/$TASK/local/compat/pfx"
[ -d "$PFX" ] || { diagnose; fail "Proton did not create its prefix at $PFX"; }
echo "ok: Proton prefix created"
# Not asserted: the exe's stdout. Under Proton the game's console output does not
# reach the task's stdout (Proton's steam.exe wrapper sits between them), which is
# what follow_logs is for with real servers.

echo "== stop: server.exe and wineserver must both be gone =="
nomad job stop -detach "$JOB"
stopped() { [ "$(api "allocation/$ALLOC" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ClientStatus"])')" = complete ]; }
wait_for "allocation complete" 90 stopped || { diagnose; exit 1; }
gone() { ! pgrep -f 'wineserver|server\.exe' >/dev/null; }
wait_for "no wineserver / server.exe left" 30 gone || { diagnose; fail "Proton processes survived the stop (wineserver -k cleanup)"; }
if udp_query "$PORT" 70696e67 >/dev/null; then fail "udp/$PORT still answers after stop"; fi

echo "PASS: Windows server started under Proton, answered, and stopped cleanly"
