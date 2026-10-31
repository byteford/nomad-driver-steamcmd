package steamcmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveWindowsRuntime(t *testing.T) {
	root := "/alloc/task"

	t.Run("requires proton_path", func(t *testing.T) {
		if _, err := resolveWindowsRuntime(TaskConfig{}, PluginConfig{}, root, ""); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("proton_path must be absolute", func(t *testing.T) {
		if _, err := resolveWindowsRuntime(TaskConfig{ProtonPath: "rel/p"}, PluginConfig{}, root, ""); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("proton defaults: per-task compat dir, client falls back to compat", func(t *testing.T) {
		rt, err := resolveWindowsRuntime(TaskConfig{}, PluginConfig{DefaultProtonPath: "/opt/p"}, root, "/nonexistent-home")
		if err != nil {
			t.Fatal(err)
		}
		want := windowsRuntime{ProtonPath: "/opt/p", CompatDataPath: "/alloc/task/local/compat", ClientPath: "/alloc/task/local/compat"}
		if rt != want {
			t.Errorf("got %+v want %+v", rt, want)
		}
	})
	t.Run("absolute compat_data_path shared across tasks; existing ~/.steam/steam preferred", func(t *testing.T) {
		home := t.TempDir()
		steam := filepath.Join(home, ".steam", "steam")
		if err := os.MkdirAll(steam, 0o755); err != nil {
			t.Fatal(err)
		}
		rt, err := resolveWindowsRuntime(TaskConfig{ProtonPath: "/opt/p", CompatDataPath: "/alloc/data/compat"}, PluginConfig{}, root, home)
		if err != nil {
			t.Fatal(err)
		}
		if rt.CompatDataPath != "/alloc/data/compat" || rt.ClientPath != steam {
			t.Errorf("got %+v", rt)
		}
	})
}

func TestWindowsRuntimeEnv(t *testing.T) {
	p := windowsRuntime{CompatDataPath: "/c", ClientPath: "/s"}
	got := p.env("2915550", "/install")
	want := []string{
		"STEAM_COMPAT_DATA_PATH=/c",
		"STEAM_COMPAT_CLIENT_INSTALL_PATH=/s",
		"STEAM_COMPAT_INSTALL_PATH=/install",
		"SteamAppId=2915550",
		"SteamGameId=2915550",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proton env = %v, want %v", got, want)
	}
	if got := p.env("", "/install"); len(got) != 3 {
		t.Errorf("env without app id = %v, want no SteamAppId/SteamGameId", got)
	}
}

func TestProtonScriptAndWineserver(t *testing.T) {
	dir := t.TempDir()
	if got := protonScript(dir); got != filepath.Join(dir, "proton") {
		t.Errorf("dir form: %q", got)
	}
	if got := protonScript(filepath.Join(dir, "proton")); got != filepath.Join(dir, "proton") {
		t.Errorf("script form: %q", got)
	}
	if got := protonWineserver(dir); got != "" {
		t.Errorf("no wineserver expected, got %q", got)
	}
	ws := filepath.Join(dir, "files", "bin", "wineserver")
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := protonWineserver(dir); got != ws {
		t.Errorf("got %q want %q", got, ws)
	}
	if got := protonWineserver(filepath.Join(dir, "proton")); got != ws {
		t.Errorf("script form: got %q want %q", got, ws)
	}
	c := (windowsRuntime{ProtonPath: dir, CompatDataPath: "/c"}).cleanup()
	if c == nil || c.Bin != ws || c.Env[0] != "WINEPREFIX=/c/pfx" {
		t.Errorf("cleanup = %+v", c)
	}
}

func TestPrepareProtonMissingScript(t *testing.T) {
	rt := windowsRuntime{ProtonPath: t.TempDir(), CompatDataPath: filepath.Join(t.TempDir(), "c")}
	if err := rt.prepare(); err == nil {
		t.Fatal("expected error for missing proton script")
	}
	if err := os.WriteFile(filepath.Join(rt.ProtonPath, "proton"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rt.prepare(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(rt.CompatDataPath); err != nil || !st.IsDir() {
		t.Errorf("compat dir not created: %v", err)
	}
}
