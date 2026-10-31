# nomad-driver-steamcmd

A Nomad task driver that wraps `steamcmd` for installing/updating
Steam-distributed dedicated servers, then optionally launching and
supervising the resulting binary as the task's tracked process.

## Design decisions (and why)

- **No filesystem/network isolation (`FSIsolationNone`, host networking
  only).** This is deliberately `raw_exec`-style rather than
  container-style. Nomad does no bind-mounting for such a driver, so the
  driver itself symlinks every `volume_mount` (CSI or host volume) into the
  task dir -- see "Volumes" below -- and it sidesteps container-network DNS/IPv6 flakiness
  entirely, since the task talks to Steam's CDN over the host's network
  stack directly.
- **Every task gets its own `install_dir`** (defaults to `local/steamapp`,
  inside Nomad's per-allocation task dir). This avoids steamcmd's known
  flakiness with concurrent installs sharing one appcache. It costs
  duplicate downloads per server; if that becomes a real problem, put a
  transparent caching proxy (e.g. lancache) in front of the Steam CDN
  rather than adding shared-install locking to the driver.
- **`StartTask` is synchronous through the install phase, matching how the
  built-in `docker`/`qemu` drivers handle slow starts (image pulls, VM
  boot).** Nomad only persists a task's recoverable `DriverState` once
  `StartTask` returns, and there's no mechanism for a driver to push
  updated state into that record afterwards -- so the install can't happen
  in a detached goroutine if `RecoverTask` is going to reattach correctly
  later. `StartTask` now blocks until steamcmd finishes and (if a `launch`
  block is configured) the server process has actually started, and only
  then persists state -- so a recovered handle always reflects a real PID,
  never a stale "was installing" snapshot. Progress is visible in the
  meantime the same way Docker surfaces image-pull progress: raw steamcmd
  output streams to the task's log files immediately (`nomad alloc logs`),
  and a throttled (every 5s) human-readable summary is emitted as
  `TaskEvent`s (`nomad alloc status`), parsed from steamcmd's own
  `Update state (0x..) downloading, progress: NN.NN` lines.
- **`RecoverTask` only has one real case to handle: reattaching to an
  already-launched process by PID.** Because a handle is only ever
  persisted post-install, a driver/client crash *during* an install means
  Nomad never received a handle for that task in the first place --
  Nomad's own client just calls `StartTask` again from scratch, same as it
  would for any driver interrupted before `StartTask` returns. There's no
  "resume a partial install" case to reconcile.
- **steamcmd's exit code is not trusted.** It's well known to return 0 on
  partial/failed installs and nonzero on some successful ones. `StartTask`
  parses stdout for explicit `Success! App '<id>' ...` / `ERROR!` /
  auth-failure markers instead. See `driver/steamcmd.go`.
- **Auth is sourced generically, not Vault-specifically.** `login.password`
  and `login.password_file` are plain fields; how the value gets there
  (Vault via `template`, Nomad variables, or a literal) is entirely up to
  the job author. The driver has no secrets-backend dependency.
- **Client-level default login.** A `plugin "nomad-driver-steamcmd" { config { ... } }`
  stanza in the Nomad agent config (block label matches the plugin binary's
  filename, not `driver = "steamcmd"`'s internal name -- see the Plugin
  config reference below) can set a fleet-wide `default_login_anonymous`.
  A task's own `login_*` fields, if any are set, are a full override — no
  field-level merging with the default in v1.
- **Cron-based restarts are intentionally NOT a driver feature.** Use
  Nomad's own `periodic` batch-job stanza to call
  `POST /v1/allocation/:id/restart` on a schedule instead. Keeps the
  driver's job restricted to "run steamcmd + supervise a process."
- **Disk-space preflight.** `StartTask` checks free space on `install_dir`
  before invoking steamcmd and fails fast with a clear task event rather
  than letting steamcmd hang/fail confusingly mid-download.

## Volumes (`volume_mount`)

CSI and host volumes work, with one rule: set the `volume_mount`
`destination` to a path **relative to the task directory** (`data`; a
leading `/` is ignored, so `/data` means `<task dir>/data`, not the host's
`/data`). The driver symlinks `<task dir>/<destination>` to the volume's
staged host path before install/launch, so `install_dir`, `compat_data_path`
and game save paths can point into it:

```hcl
volume "data" { type = "csi" source = "my-nfs" ... }

task "server" {
  volume_mount { volume = "data" destination = "data" }
  config {
    install_dir      = "data/steamapp"
    compat_data_path = "data/compat"
  }
}
```

Limits: `read_only = true` cannot be enforced (no isolation; the driver logs a
warning), destinations that escape the task dir are rejected, and an
existing non-empty directory at the destination is never replaced. Several
tasks in one allocation (e.g. prestart install + server) can mount the same
volume and see the same files.

## Task config reference

```hcl
config {
  app_id          = "2394010"        # required, Steam App ID
  install_dir     = "local/steamapp" # default shown
  beta            = ""               # optional beta branch
  beta_password   = ""               # optional, only if beta requires one
  validate        = false            # steamcmd `validate` flag
  platform        = ""               # "windows", "macos", or "linux" -- see below
  windows_display = false            # wrap with xvfb-run --auto-servernum; only with platform="windows"
  update_on_start = true             # re-run steamcmd before every launch
  install_timeout = "20m"            # optional, positive Go duration; an invalid value fails the start
  repair_stale_manifest = false      # see "Stale app manifest" below

  # Sets WINEPREFIX for the launched process; overrides the plugin-level
  # default_wine_prefix if both are set; only meaningful with platform="windows"
  wine_prefix = ""

  # Windows runner (platform="windows" only). "wine" (default) or "proton" --
  # see "Running Windows servers under Proton" below.
  runner             = "wine"
  proton_path        = ""   # Proton dir (contains `proton`); overrides default_proton_path
  compat_data_path   = ""   # STEAM_COMPAT_DATA_PATH; default local/compat (per task)
  compat_client_path = ""   # STEAM_COMPAT_CLIENT_INSTALL_PATH; default ~/.steam/steam

  login_anonymous     = true   # or:
  login_username      = ""
  login_password      = ""     # literal, or:
  login_password_file = ""     # path to a file (e.g. rendered by `template`)

  # Copied into install_dir after install and before launch (source -> destination).
  # Source: relative to the task dir (or absolute). Destination: relative to install_dir.
  # Lets a `template` rendered to local/ end up next to the game's binary.
  config_files = { "local/server.json" = "server.json" }

  # Stream these log files (relative to install_dir, or absolute) into the task's
  # stdout so `nomad alloc logs` shows them. For servers that log only to a file.
  follow_logs = ["logs/server.log"]

  launch {                 # omit for an install-only task
    command = "local/steamapp/PalServer.sh"
    args    = ["-port=8211"]
    env     = { KEY = "value" }
  }
}
```

`launch.command` resolution has bitten this driver twice in production, so
it's worth being explicit about exactly how it works: a command
containing a `/` and not already absolute (`local/steamapp/PalServer.sh`)
is resolved against `install_dir`. A **bare** command with no `/` at all
(`xvfb-run`, `wine`) is left untouched for a `$PATH` lookup instead --
this matters because a binary directly inside `install_dir` with no
subdirectory (`command = "PalServer.sh"`, no `local/steamapp/` prefix)
looks identical to a bare PATH-lookup command under this rule and will
almost certainly fail to launch. Use `./PalServer.sh` or the full relative
path from the task root instead of a bare filename in that case. Note
this resolution only applies when `platform` isn't `"windows"` -- see below.

**`launch.args` are passed through completely unmodified** -- none of the
above applies to them, and this has *also* now bitten in production
(`xvfb-run wine local/steamapp/FoundryDedicatedServer.exe`, failing with
`wine: failed to open "local/steamapp/FoundryDedicatedServer.exe"`). The
launched process's working directory is always set to `install_dir`
(regardless of whether `command` needed resolving against it), so any
path-like value inside `args` is resolved by *the launched program
itself*, relative to `install_dir` -- not the task root. Write path
arguments as `"FoundryDedicatedServer.exe"`, not
`"local/steamapp/FoundryDedicatedServer.exe"`, when `install_dir` is
`local/steamapp`. This is deliberately not auto-corrected by the driver:
there's no reliable way to tell a path-like argument apart from an
arbitrary flag or value (`-port=8211` isn't a path; some other app's
`config/server.cfg` argument might be) without guessing wrong sometimes.

`platform` sets steamcmd's `@sSteamCmdForcePlatformType` convar, overriding
which platform's depot gets downloaded regardless of the client node's
actual OS. Needed for dedicated servers that only publish a build for one
platform -- some apps (e.g. `2915550`, FOUNDRY) have no Linux build at
all, and installing without this produces install_dir containing only
Steam's own runtime scaffolding (`steamclient.so`, `linux64/`,
`steamapps/`) with no actual app content and no error.

**When `platform = "windows"`, `launch.command`/`args` are automatically
wrapped with `wine`** (and, if `windows_display = true`, additionally
with `xvfb-run --auto-servernum`) -- write the `.exe` and its own
arguments directly, the same as you would for a native Linux binary:

```hcl
config {
  app_id          = "2915550"
  platform        = "windows"
  windows_display = true                # only if the app actually needs a display -- see below
  wine_prefix     = "/root/.wine-test"   # optional; sets WINEPREFIX for you

  launch {
    command = "FoundryDedicatedServer.exe"  # NOT "local/steamapp/..." -- same
                                             # resolution rule as launch.args above,
                                             # since this is really an argument to
                                             # wine/xvfb-run once wrapped, not the
                                             # actual exec target
    args    = ["-batchmode", "-nographics"]
  }
}
```

This expands internally to `wine FoundryDedicatedServer.exe -batchmode
-nographics` (working directory `install_dir`), or with
`windows_display = true`, `xvfb-run --auto-servernum wine
FoundryDedicatedServer.exe -batchmode -nographics`. See
`buildLaunchCommand` in `driver/lifecycle.go` for the exact logic.

**`windows_display` defaults to `false` and is never auto-enabled just
because `platform = "windows"`** -- confirmed in production this exact
combination is what one specific app (FOUNDRY) needs, but plenty of
Windows console apps genuinely don't need any display at all (this
driver's own Unity-specific troubleshooting confirmed `-batchmode
-nographics` alone is *not* always sufficient either -- it reduces what
the app does graphics-wise, but doesn't remove Wine's own need for a
display to exist at all, for apps that check). Forcing Xvfb
unconditionally onto every Windows-platform task would reintroduce
failure surface (stale display locks, an extra runtime dependency) this
driver deliberately keeps opt-in. Check your specific app's own behavior
before deciding whether you need it.

Requires `wine` (and, if using `windows_display`, `xvfb-run`) installed on the client node (Wine because
the server is a Windows binary; Xvfb because this particular server
apparently wants a display even in "dedicated" mode -- confirmed working
by the community for this app, not something this driver does anything
special for beyond getting the right binary downloaded in the first place).

### Running Windows servers under Proton

Set `runner = "proton"` (per task, or `default_runner` fleet-wide) to run
`platform = "windows"` tasks through a pinned [Proton](https://github.com/ValveSoftware/Proton)
/ [GE-Proton](https://github.com/GloriousEggroll/proton-ge-custom) build
instead of whatever `wine` the distro ships. `wine` stays the default, so
existing jobs are unaffected.

```hcl
config {
  app_id           = "2915550"
  platform         = "windows"
  runner           = "proton"
  proton_path      = "/opt/GE-Proton9-20"   # absolute; dir containing the `proton` script
  compat_data_path = "local/compat"         # default; Proton creates pfx/ underneath
  windows_display  = true                   # still opt-in, same as wine

  launch {
    command = "FoundryDedicatedServer.exe"
    args    = ["-batchmode", "-nographics"]
  }
}
```

This runs `<proton_path>/proton run FoundryDedicatedServer.exe ...` (wrapped
in `xvfb-run --auto-servernum` if `windows_display`) and sets
`STEAM_COMPAT_DATA_PATH`, `STEAM_COMPAT_CLIENT_INSTALL_PATH` (defaults to
`~/.steam/steam` if it exists, else the compat dir), `STEAM_COMPAT_INSTALL_PATH`,
`SteamAppId` and `SteamGameId`. `WINEPREFIX`/`wine_prefix` are **not** used
with Proton -- the prefix is `<compat_data_path>/pfx`.

Install Proton on the client by unpacking a release tarball somewhere
stable (e.g. `/opt/GE-Proton9-20`); the driver never downloads it.

**Prefix sharing.** The default `compat_data_path` is per task, so each
task gets its own prefix and the first run pays Proton's prefix-creation
cost. Don't point two *concurrently running* tasks at one prefix. To share
one between sequential tasks (see below), give them the same absolute path,
e.g. `${NOMAD_ALLOC_DIR}/data/compat`.

**Stopping.** `wineserver` daemonizes and escapes the task's process
group, so after a stop/kill the driver also runs `wineserver -k` against the
task's prefix (Proton's bundled `wineserver`, or the system one for the
`wine` runner). This is persisted, so it also works for tasks recovered
after a client restart.

### Getting file-only server logs into `nomad alloc logs`

Some servers (Enshrouded, for one) write their log to a file and print nothing to stdout.
`follow_logs = ["logs/enshrouded_server.log"]` makes the driver follow those files and write
new lines to the task's stdout, so `nomad alloc logs -f <alloc>` and the UI show them.

- Paths are relative to `install_dir` (the process's working directory) or absolute.
- A file that already exists at launch is treated as the previous run's and skipped. If the
  game then replaces it (rotates to a new file) it is followed from the start; if it is
  appended to, only the new lines are shown; if it is truncated, following restarts at the top.
- It polls (every 500 ms) rather than using inotify, so it works on NFS/CSI volumes.
- Partial lines are held until their newline arrives and flushed when the task ends.
- Limitations: it is not re-attached for a task recovered after the *plugin process* itself
  restarts (the log file is still written, just not streamed until the next start), and an
  in-place rewrite that is already as long as before by the next poll cannot be told from an append.

### Separating install from run (prestart tasks)

Install and run don't have to live in one task. A task with no `launch`
block is install-only, so a Nomad `lifecycle { hook = "prestart" }` task can
do the download and the main task only runs the server:

1. `install` (prestart): `app_id` + `install_dir`, no `launch` -- downloads and exits.
2. `server` (main): `launch` only. With `update_on_start = false` (the default)
   and a populated `install_dir`, steamcmd is skipped entirely.

Both tasks must point at the same storage: use a `volume_mount` (see
"Volumes") with the same `install_dir`/`compat_data_path`, or an absolute
path under `${NOMAD_ALLOC_DIR}/data`. Note that **Nomad runs prestart tasks
in parallel, not in order**, so only put one task that writes `install_dir` in
the prestart phase. Proton creates its prefix itself on first `proton run`,
so no separate prefix-setup task is needed.

Nomad also renders `template` blocks *before* the driver starts, i.e. before
install_dir or any volume symlink exists. Render config to `local/` and use
`config_files` to copy it into `install_dir` at launch time.

See `example/foundry-proton.nomad.hcl`.

### Setting up Wine + Xvfb on the client node

Needed on any client that'll run a `platform = "windows"` task through
Wine (Ubuntu/Debian shown; adjust for other distros). This is normal
system setup on the client, not something this driver installs or manages.

```sh
# 32-bit support -- almost certainly already enabled if you followed
# steamcmd's own setup on this node, but harmless to repeat.
sudo dpkg --add-architecture i386

# WineHQ's own repo, rather than Ubuntu's bundled `wine` package, which
# tends to lag behind. Key + repo setup:
sudo mkdir -pm755 /etc/apt/keyrings
sudo wget -O /etc/apt/keyrings/winehq-archive.key https://dl.winehq.org/wine-builds/winehq.key
sudo wget -NP /etc/apt/sources.list.d/ \
  https://dl.winehq.org/wine-builds/ubuntu/dists/$(lsb_release -sc)/winehq-$(lsb_release -sc).sources

sudo apt update
sudo apt install --install-recommends winehq-stable xvfb -y
```

**One-time Wine prefix setup, as whatever user Nomad actually runs
tasks as** (root, in a typical homelab client config -- check yours).
Skipping this means the *first* time a Nomad task tries to launch under
Wine, Wine may try to pop up a Mono/Gecko installer dialog to initialize
its prefix -- which will simply hang forever under Nomad, since there's
no display/interaction to click through:

```sh
sudo WINEDLLOVERRIDES="mscoree,mshtml=" wineboot --init
```

`WINEDLLOVERRIDES="mscoree,mshtml="` disables the Mono (.NET) and Gecko
(HTML rendering) components most dedicated servers don't need, avoiding
their install prompts entirely rather than needing to click through them
once interactively first. If a specific app's server *does* need one of
these (check its own setup docs), drop the override for that component
and initialize the prefix interactively once first instead.

Verify before wiring it into a job spec:

```sh
sudo xvfb-run wine --version
```

**Always use `--auto-servernum`, not the plain default, for any app that
genuinely needs a virtual display.** `xvfb-run` defaults to a *fixed*
display number (`:99`) -- fine for a one-off manual test, but genuinely
unsafe for anything that restarts: a redeploy, a crash-and-retry, or just
a Nomad reschedule can start a new allocation's `Xvfb` while an old
allocation's `Xvfb` is still tearing down (within `kill_timeout`), both
wanting the same fixed display. This driver signals the whole process
group on stop (not just the top-level wrapper -- see
`signalProcessGroup` in `driver/lifecycle.go`), which fixes *eventual*
cleanup, but that's a different problem from the race window itself; only
`--auto-servernum` actually closes it. The driver always passes
`--auto-servernum` automatically when `windows_display = true` (see the
Task config reference above) -- this only matters if you're testing Wine
manually outside of Nomad, or writing the raw wrapper yourself for some
other reason:

```hcl
# What windows_display=true expands to internally -- write this yourself
# only if you're not using platform="windows" (e.g. wrapping some other
# non-Steam binary through this driver's raw exec path).
launch {
  command = "xvfb-run"
  args    = ["--auto-servernum", "wine", "FoundryDedicatedServer.exe"]
}
```

**For a Unity-based Windows app specifically, also pass `-batchmode
-nographics`** in `launch.args` (alongside `windows_display = true`, not
instead of it). These are Unity's own headless-run flags, not something
this driver knows about -- worth checking whether your app's engine has
an equivalent, since it can reduce what the app itself tries to do
graphics-wise even with a display present. Note this is *additive* to
`windows_display`, not a replacement for it: confirmed directly against
FOUNDRY (app `2915550`) that `-batchmode -nographics` alone, with
`windows_display = false`, still fails at `Failed to create batch mode
window` -- Wine's window-driver check still needs *some* display to
satisfy it, that flag just changes what the app itself does once one
exists.

**⚠️ Confirmed broken: `wine-stable 11.0.0.0~noble-1` (WineHQ's own
package, as of writing) reliably fails with `wine: could not load
kernel32.dll, status c0000135` on Ubuntu 24.04, even against a completely
fresh prefix with correct architecture and a file-integrity-verified
install.** This was isolated thoroughly before concluding it's the
package itself: ruled out prefix corruption (fresh `wineboot --init`
every time), architecture mismatch (confirmed `win64`), a
mixed-Wine-source conflict (purged Ubuntu's own `wine64`/`libwine`
packages, confirmed only WineHQ's remained), environment/wrapper-chain
issues (identical failure with `sudo`/`xvfb-run` completely stripped
away, `WINEPREFIX` set explicitly), and file corruption (`kernel32.dll`
verified as a valid, correctly-sized `PE32+` DLL via `file`).
**`wine-staging 11.15` does not have this problem** -- switching to it
was what actually got FOUNDRY running. If `wine winecfg` (or any Wine
invocation) fails with this exact error against `wine-stable`, don't
keep debugging your own setup -- try `staging` instead:

```sh
sudo apt-get install --allow-downgrades winehq-staging
which wine && wine --version   # confirm it's actually staging now
```

This is worth reporting to WineHQ directly if you hit it -- it's a
genuine packaging/build issue, not a configuration problem, and this
investigation already did the work to isolate it cleanly.

Login fields are flat top-level attributes, not a nested `login {}` block.
That's not a style choice -- a nested block here was found to decode
incorrectly specifically in the Nomad client agent's plugin-config parsing
path (flat attributes decoded fine; the nested block's contents silently
came back as zero values even when set in HCL). See `driver/config.go` for
the full note.

### Stale app manifest (`repair_stale_manifest`)

If the game ships a new build while a server is running, the next install can
fail with `Error! App '<id>' state is 0x6 after update job.` and no download
(`Update state (0x0) unknown, progress: 0.00 (0 / 0)`). steamcmd's own
`logs/content_log.txt` shows `Failed to get manifest request code, 'Access
Denied'` for the depot manifest recorded in
`<install_dir>/steamapps/appmanifest_<app_id>.acf`: Steam no longer serves the
superseded manifest. Deleting that one file by hand makes steamcmd download
the app again.

With `repair_stale_manifest = true` the driver does that itself: on exactly
that error it deletes `<install_dir>/steamapps/appmanifest_<app_id>.acf`
(nothing else in `install_dir`), emits a task event naming the removed file,
and runs steamcmd once more. If there is no manifest to remove, or the retry
fails too, the task fails as before. Off by default. Expect a large
download on the retry.

## Plugin (client agent) config reference

⚠️ The block label must be the plugin **binary's filename** in
`plugin_dir` (`nomad-driver-steamcmd`), not the driver's internal
self-reported name (`steamcmd`, used in `driver = "steamcmd"` in job
specs). Get this wrong and Nomad silently skips loading the plugin
entirely -- it won't appear in the agent's own "detected plugin" log at
all. This is a *separate* issue from the agent-config-forwarding
investigation above, discovered only while testing against a newer
Nomad version -- on the older Nomad this driver has actually been run
against, the plugin loaded fine despite the same label, so this
filename-matching enforcement appears to be new/stricter in later
Nomad releases, not the explanation for the original bug.

```hcl
plugin "nomad-driver-steamcmd" {
  config {
    steamcmd_path                = "steamcmd" # must be on the node's PATH
    install_root                 = ""         # reserved, unused in v1
    max_concurrent_installs      = 0          # 0 = unbounded
    default_login_anonymous      = true       # native bool
    default_login_username       = ""
    default_login_password       = ""
    default_login_password_file  = ""
    default_wine_prefix          = ""         # fleet-wide WINEPREFIX default;
                                               # task-level wine_prefix overrides
                                               # this if both are set
    default_runner               = ""         # "wine" (default) | "proton"
    default_proton_path          = ""         # fleet-wide Proton dir
  }
}
```

**⚠️ Confirmed, now-fixed version-specific Nomad bug: explicit values in
this agent-config block did not reach the plugin at all on Nomad
v1.9.3.** Debugging traced a persistent failure (reproduced both in CI
and on a real client) down to `default_login_anonymous` decoding as
`"false"` -- a genuine string, matching this schema's own default literal
exactly -- even though `nomad.hcl` clearly set it to `"true"`. This was
isolated thoroughly: not a nested-block issue, not a bool-vs-string issue,
not a `NewDefault` issue, not a file-merge or block-adjacency issue, and
not this driver's own `SetConfig`/`ConfigSchema` pattern -- that code was
compared line-by-line against `hashicorp/nomad-driver-podman`'s current
source (a real, actively-maintained external plugin using the exact same
`config{}` mechanism) and matches it exactly, including a bare
unwrapped-string attribute (`socket_path`) that's documented as working
for podman.

**Confirmed fixed by v1.10.0.** A canary test was added
(`.github/workflows/e2e.yml`'s "Assert plugin-level agent config values
are actually forwarded" step) that sets two fields to values deliberately
different from their own schema defaults -- the only way to actually
distinguish "explicit value honored" from "silently fell back to a
default that happens to match", which is what let this hide for so long
in the first place. On v1.10.0, both values decoded correctly. This is
now a permanent regression guard, not just a one-off diagnostic: it runs
on every CI run and will catch this specific class of bug returning.

**If you're still on v1.9.3 (or any version between the two that hasn't
been checked):** don't rely on this block for anything where the desired
value differs from its schema default. `login_username`/`login_password`
on the task itself (`TaskConfig`'s fields, via job-spec parsing) have
been directly proven reliable on v1.9.3 too, using real credentials
against a real steamcmd auth rejection -- set those explicitly per-task
instead of trusting the plugin-level default on an unconfirmed version.

**Note on why the fix was verified on v1.10.0 specifically, not
something newer:** every `v1.10.x`/`v1.11.x` patch release beyond
`v1.10.0` turned out to be Enterprise-only with no public tag (confirmed
from GitHub's own release listing) -- `v1.10.0` is the last plain-public
tag on that line. `v2.0.5` (the actual latest public release, on Nomad's
newer release line) was tried first and is a dead end for now: Go's
module system requires a v2+ tag's own `go.mod` to either declare
`module .../nomad/v2` or have no `go.mod` at all (the `+incompatible`
fallback) -- Nomad's `v2.0.5` tag has a `go.mod` that still declares
plain `github.com/hashicorp/nomad`, so it satisfies neither mechanism
and can't be consumed as a normal dependency at all. That's a real
inconsistency in how that tag was published, not something fixable
from this side. If you're planning to upgrade past `1.9.3`, `v1.10.0`
is confirmed to have this fixed; nothing conclusive is known about
`v2.0.x`.

## Installing a release

Every tag matching `vX.Y.Z` triggers `.github/workflows/release.yml`, which
builds and publishes binaries for both `linux/amd64` (the main devcastops
node) and `linux/arm64` (the Raspberry Pi node) to GitHub Releases.

```sh
# on the target Nomad client node:
curl -fsSLO https://github.com/Devcastops/nomad-driver-steamcmd/releases/download/vX.Y.Z/nomad-driver-steamcmd_vX.Y.Z_linux_amd64.tar.gz
curl -fsSLO https://github.com/Devcastops/nomad-driver-steamcmd/releases/download/vX.Y.Z/nomad-driver-steamcmd_vX.Y.Z_linux_amd64.tar.gz.sha256
sha256sum -c nomad-driver-steamcmd_vX.Y.Z_linux_amd64.tar.gz.sha256

tar -xzf nomad-driver-steamcmd_vX.Y.Z_linux_amd64.tar.gz
sudo mv nomad-driver-steamcmd_vX.Y.Z_linux_amd64/nomad-driver-steamcmd \
  /path/to/plugin_dir/nomad-driver-steamcmd
sudo systemctl restart nomad   # or however you restart the client agent
```

Swap `linux_amd64` for `linux_arm64` on the Pi. `nomad node status -verbose <node>`
(or `curl :4646/v1/agent/health`) will show the loaded driver version
matching the tag once the client picks it back up — the plugin's own
`PluginInfo` reports whatever version was stamped into the binary at build
time (see `pluginVersion` in `driver/driver.go`), so a mismatch there means
the wrong binary landed on that node.

### Cutting a release

```sh
git tag v0.2.0
git push origin v0.2.0
```

That's the whole process — the workflow handles the rest. The release
workflow first runs the full `e2e.yml` (real Nomad + steamcmd) against the
tagged commit; if it fails, nothing is published. Re-run the failed workflow
once the cause is fixed, or delete and re-push the tag. To test a build
locally first (e.g. directly on the Pi) without waiting on CI:

```sh
make release-build VERSION=v0.2.0   # -> dist/release/nomad-driver-steamcmd_v0.2.0_linux_{amd64,arm64}
```

## Building

```sh
make build      # -> dist/plugins/nomad-driver-steamcmd, version-stamped from `git describe`
make test       # unit tests (no Nomad/steamcmd required)
make dev-agent  # single-node `nomad agent -dev` with the plugin loaded
```

> **Note on this repo's origin:** this codebase was scaffolded in a
> sandboxed environment without access to `proxy.golang.org` or the
> various `golang.org/x/...` vanity-import domains Nomad's dependency
> tree pulls in, so `go mod tidy`/`go build` could not be fully verified
> there. Every file is confirmed `gofmt`-clean (syntax-valid Go). The CI
> workflow (`.github/workflows/ci.yml`) runs on a normal GitHub-hosted
> runner with unrestricted network access and does the real
> `go mod tidy && go build && go test` — treat a green CI run as the
> actual first compilation, and expect to fix any real API-surface
> mismatches against `github.com/hashicorp/nomad@v1.10.0`'s current
> `plugins/drivers` interface that a from-memory scaffold can't
   guarantee against (see `driver/lifecycle.go`, `driver/driver.go`).

## CI

- **`ci.yml`** — `go vet`, `gofmt -l`, unit tests with race detector and
  coverage, build the plugin binary.
- **`e2e.yml`** — the real thing:
  1. Installs a real Nomad binary and a real `steamcmd` (via apt,
     `i386` arch + EULA auto-accepted via `debconf-set-selections`).
  2. Starts a single-node `nomad agent -dev` with this plugin loaded and
     `default_login_anonymous = true`.
  3. Confirms the driver **fingerprints healthy**.
  4. Runs a real install-only job against a small, anonymous-downloadable
     app (Half-Life Dedicated Server, App ID `90`) and asserts the task
     exits cleanly and files actually land on disk.
  5. Runs a **deliberate bad-credentials job** and asserts it fails
     cleanly rather than hanging (exercises the auth-failure parsing
     path).
  6. Runs a long-running "launch" job, then kills and restarts the Nomad
     agent, and confirms the task is still reported `running` afterward
     (exercises `RecoverTask`'s PID-reattach path).
  7. Uploads all Nomad/task logs as artifacts regardless of outcome.

  CSI is deliberately **not** exercised end-to-end in CI (it would need an
  external NFS server or an in-CI CSI plugin, which is disproportionate
  infra weight for what the driver itself needs to guarantee — see
  `driverCapabilities` in `driver/driver.go`, `MountConfigSupportAll` +
  `FSIsolationNone` is the whole contract, and that's exercised by the
  install-job's `local/` task-dir mount already going through the same
  no-isolation code path a CSI mount would).

## Known gaps / things to harden before production use

- `StartTask` blocks for the full install duration -- for a large game
  server on a slow link this could be many minutes. This is intentional
  (see design notes above) but means `nomad job run` itself will appear to
  hang for that long; progress is visible via `nomad alloc status`/`nomad
  alloc logs` in another terminal in the meantime.
- No shared-install dedup across tasks on the same node (by design, see
  above) — revisit only if disk usage becomes a real problem.
- `password`/`password_file` are read once at `StartTask` time; rotating a
  credential requires a task restart, same as any other Nomad `template`
  driven secret.
