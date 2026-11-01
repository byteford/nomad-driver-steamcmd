package steamcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// windowsRuntime describes how a platform="windows" launch command gets
// executed on the Linux client: through a Proton build.
//
// Proton is Valve's pinned Wine fork. It is invoked as
// `<proton_path>/proton run <exe> <args>` and wants STEAM_COMPAT_DATA_PATH (it
// creates `pfx/` underneath) and STEAM_COMPAT_CLIENT_INSTALL_PATH.
type windowsRuntime struct {
	ProtonPath     string // proton dir (containing the `proton` script) or the script itself
	CompatDataPath string // absolute; STEAM_COMPAT_DATA_PATH
	ClientPath     string // absolute; STEAM_COMPAT_CLIENT_INSTALL_PATH
}

// protonScript returns the path to the `proton` script given either the
// Proton directory or the script path itself.
func protonScript(p string) string {
	if filepath.Base(p) == "proton" {
		return p
	}
	return filepath.Join(p, "proton")
}

// protonWineserver locates the wineserver binary shipped inside a Proton
// build. Valve's builds use files/bin, GE-Proton uses files/bin as well,
// older/community layouts use dist/bin. Returns "" if none is found.
func protonWineserver(p string) string {
	dir := p
	if filepath.Base(p) == "proton" {
		dir = filepath.Dir(p)
	}
	for _, rel := range []string{"files/bin/wineserver", "dist/bin/wineserver"} {
		c := filepath.Join(dir, rel)
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// resolveWindowsRuntime builds the effective runtime for a task. taskRoot is
// the task directory that relative paths resolve against. home is used to
// find an existing Steam install for STEAM_COMPAT_CLIENT_INSTALL_PATH.
func resolveWindowsRuntime(task TaskConfig, plugin PluginConfig, taskRoot, home string) (windowsRuntime, error) {
	protonPath := task.ProtonPath
	if protonPath == "" {
		protonPath = plugin.DefaultProtonPath
	}
	if protonPath == "" {
		return windowsRuntime{}, fmt.Errorf("platform = \"windows\" requires proton_path (task) or default_proton_path (plugin)")
	}
	if !filepath.IsAbs(protonPath) {
		return windowsRuntime{}, fmt.Errorf("proton_path %q must be absolute", protonPath)
	}

	compat := task.CompatDataPath
	if compat == "" {
		compat = "local/compat"
	}
	if !filepath.IsAbs(compat) {
		compat = filepath.Join(taskRoot, compat)
	}

	client := task.CompatClientPath
	if client == "" {
		client = defaultSteamClientPath(home, compat)
	}
	return windowsRuntime{ProtonPath: protonPath, CompatDataPath: compat, ClientPath: client}, nil
}

// defaultSteamClientPath prefers an existing Steam install (~/.steam/steam,
// as left by steamcmd's own first run) and falls back to the compat data
// dir, which Proton accepts for headless use.
func defaultSteamClientPath(home, fallback string) string {
	if home != "" {
		c := filepath.Join(home, ".steam", "steam")
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return fallback
}

// prepare checks the runtime's host-side prerequisites and creates its dirs.
func (w windowsRuntime) prepare() error {
	script := protonScript(w.ProtonPath)
	st, err := os.Stat(script)
	if err != nil || st.IsDir() {
		return fmt.Errorf("proton script not found at %q (check proton_path)", script)
	}
	if err := os.MkdirAll(w.CompatDataPath, 0o755); err != nil {
		return fmt.Errorf("failed to create compat_data_path %q: %w", w.CompatDataPath, err)
	}
	return nil
}

// env returns the environment variables this runtime needs.
func (w windowsRuntime) env(appID, installDir string) []string {
	e := []string{
		"STEAM_COMPAT_DATA_PATH=" + w.CompatDataPath,
		"STEAM_COMPAT_CLIENT_INSTALL_PATH=" + w.ClientPath,
		"STEAM_COMPAT_INSTALL_PATH=" + installDir,
	}
	if appID != "" {
		e = append(e, "SteamAppId="+appID, "SteamGameId="+appID)
	}
	return e
}

// executable is the program that runs the .exe, plus any fixed leading args.
func (w windowsRuntime) executable() (string, []string) {
	return protonScript(w.ProtonPath), []string{"run"}
}

// wineCleanup is what's needed to kill a Proton prefix's leftover wineserver after
// the task's process group is gone. It's persisted in taskState so it
// survives driver/client restarts.
type wineCleanup struct {
	Bin string
	Env []string
}

// cleanup returns nil when there is nothing to clean up (no wineserver found
// in the Proton build).
func (w windowsRuntime) cleanup() *wineCleanup {
	bin := protonWineserver(w.ProtonPath)
	if bin == "" {
		return nil
	}
	return &wineCleanup{Bin: bin, Env: []string{"WINEPREFIX=" + filepath.Join(w.CompatDataPath, "pfx")}}
}

// run executes `wineserver -k`. wineserver daemonizes, so it routinely
// escapes the task's process group and would otherwise linger (holding the
// prefix) after every stop.
func (c *wineCleanup) run(timeout time.Duration) error {
	if c == nil || c.Bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "-k")
	cmd.Env = append(os.Environ(), c.Env...)
	return cmd.Run()
}
