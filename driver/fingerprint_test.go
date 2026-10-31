package steamcmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"
)

// The fingerprint runs every 30s. It must only check that the binary exists
// and is executable; actually running steamcmd each time can trigger a
// self-update and collides with a real install using the same ~/Steam.
func TestBuildFingerprint_DoesNotRunSteamcmd(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := fakeSteamcmd(t, `echo ran > "`+marker+`"`)

	d := newTestDriver(PluginConfig{SteamCmdPath: bin})
	fp := d.buildFingerprint()

	require.Equal(t, drivers.HealthStateHealthy, fp.Health)
	require.Equal(t, bin, mustString(t, fp, "driver.steamcmd.path"))
	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "fingerprint executed steamcmd")
}

func TestBuildFingerprint_MissingBinaryIsUndetected(t *testing.T) {
	d := newTestDriver(PluginConfig{SteamCmdPath: filepath.Join(t.TempDir(), "nope")})
	require.Equal(t, drivers.HealthStateUndetected, d.buildFingerprint().Health)
}

func newTestDriver(cfg PluginConfig) *Driver {
	d := NewPlugin(hclog.NewNullLogger()).(*Driver)
	d.config = cfg
	return d
}

func mustString(t *testing.T, fp *drivers.Fingerprint, key string) string {
	t.Helper()
	a, ok := fp.Attributes[key]
	require.True(t, ok, "attribute %s missing", key)
	s, ok := a.GetString()
	require.True(t, ok)
	return s
}
