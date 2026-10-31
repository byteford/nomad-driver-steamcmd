package steamcmd

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSignal(t *testing.T) {
	for in, want := range map[string]syscall.Signal{
		"SIGTERM": syscall.SIGTERM, "TERM": syscall.SIGTERM,
		"SIGINT": syscall.SIGINT, "SIGKILL": syscall.SIGKILL,
		"SIGHUP": syscall.SIGHUP, "SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2,
		"SIGQUIT": syscall.SIGQUIT, "QUIT": syscall.SIGQUIT,
		"3": syscall.Signal(3),
	} {
		got, err := parseSignal(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "SIGBOGUS", "nope"} {
		_, err := parseSignal(bad)
		require.Error(t, err, bad)
	}
}
