package steamcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// steamcmd.sh runs the real steamcmd binary as a child. Cancelling the install
// (install_timeout, task kill) must take the whole process group down, not just
// the top-level process, or the child keeps running and keeps downloading.
func TestRunInstall_CancelKillsChildProcesses(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	bin := fakeSteamcmd(t, `sleep 300 &
echo $! > "$1"
wait`)
	var stdout, stderr bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runInstall(ctx, bin, []string{pidFile}, &stdout, &stderr, nil)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("runInstall did not return after its context was cancelled")
	}

	b, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !procAlive("/proc", int32(pid)) },
		5*time.Second, 50*time.Millisecond, "child process %d survived cancellation of the install", pid)
}
