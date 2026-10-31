package steamcmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// config_files must not change a file's permissions: a template rendered 0600
// (secrets such as an RCON password) used to become world-readable, and an
// executable script lost its exec bit.
func TestStageConfigFiles_PreservesFileMode(t *testing.T) {
	taskDir := t.TempDir()
	installDir := filepath.Join(taskDir, "install")
	require.NoError(t, os.MkdirAll(filepath.Join(taskDir, "local"), 0o755))

	for name, mode := range map[string]os.FileMode{"secret.cfg": 0o600, "run.sh": 0o755, "plain.cfg": 0o644} {
		src := filepath.Join(taskDir, "local", name)
		require.NoError(t, os.WriteFile(src, []byte("x"), 0o600))
		require.NoError(t, os.Chmod(src, mode)) // bypass umask
		require.NoError(t, stageConfigFiles(taskDir, installDir, map[string]string{"local/" + name: name}))

		st, err := os.Stat(filepath.Join(installDir, name))
		require.NoError(t, err)
		require.Equal(t, mode, st.Mode().Perm(), name)
	}
}
