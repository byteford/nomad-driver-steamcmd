package steamcmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeAppState(t *testing.T, installDir, appID, content string) {
	t.Helper()
	dir := filepath.Join(installDir, "steamapps")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "appmanifest_"+appID+".acf"), []byte(content), 0o644))
}

func TestAppInstalled(t *testing.T) {
	manifest := func(flags string) string {
		return "\"AppState\"\n{\n\t\"appid\"\t\t\"2915550\"\n\t\"StateFlags\"\t\t\"" + flags + "\"\n}\n"
	}

	t.Run("missing dir", func(t *testing.T) {
		require.False(t, appInstalled(filepath.Join(t.TempDir(), "nope"), "2915550"))
	})
	t.Run("empty dir", func(t *testing.T) {
		require.False(t, appInstalled(t.TempDir(), "2915550"))
	})
	t.Run("populated but no manifest", func(t *testing.T) {
		// steamcmd scaffolding, a failed install, or config left by an earlier run.
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "steamapps"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "something"), []byte("x"), 0o644))
		require.False(t, appInstalled(dir, "2915550"))
	})
	t.Run("fully installed", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "2915550", manifest("4"))
		require.True(t, appInstalled(dir, "2915550"))
	})
	t.Run("installed but update required (flags 6)", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "2915550", manifest("6"))
		require.True(t, appInstalled(dir, "2915550"))
	})
	t.Run("interrupted download (flags 1026)", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "2915550", manifest("1026"))
		require.False(t, appInstalled(dir, "2915550"))
	})
	t.Run("manifest of another app", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "1234", manifest("4"))
		require.False(t, appInstalled(dir, "2915550"))
	})
	t.Run("unparsable manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "2915550", "garbage")
		require.False(t, appInstalled(dir, "2915550"))
	})
	t.Run("non-numeric app id never touches the filesystem path", func(t *testing.T) {
		dir := t.TempDir()
		writeAppState(t, dir, "2915550", manifest("4"))
		require.False(t, appInstalled(dir, "../../etc"))
		require.False(t, appInstalled(dir, ""))
	})
}
