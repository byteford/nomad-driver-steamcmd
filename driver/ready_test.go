package steamcmd

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/stretchr/testify/require"
)

func TestParseReadyCheck(t *testing.T) {
	launch := &LaunchConfig{Command: "server"}
	cases := []struct {
		name    string
		cfg     TaskConfig
		want    *readyCheck
		wantErr string
	}{
		{name: "disabled by default", cfg: TaskConfig{Launch: launch}},
		{name: "udp with default timeout",
			cfg:  TaskConfig{Launch: launch, ReadyPort: 15637, ReadyProtocol: "udp"},
			want: &readyCheck{Protocol: "udp", Port: 15637, Timeout: 5 * time.Minute}},
		{name: "tcp with timeout",
			cfg:  TaskConfig{Launch: launch, ReadyPort: 25565, ReadyProtocol: "tcp", ReadyTimeout: "90s"},
			want: &readyCheck{Protocol: "tcp", Port: 25565, Timeout: 90 * time.Second}},
		{name: "protocol without port", cfg: TaskConfig{Launch: launch, ReadyProtocol: "udp"}, wantErr: "ready_port"},
		{name: "timeout without port", cfg: TaskConfig{Launch: launch, ReadyTimeout: "1m"}, wantErr: "ready_port"},
		{name: "port without protocol", cfg: TaskConfig{Launch: launch, ReadyPort: 1234}, wantErr: "ready_protocol"},
		{name: "unknown protocol", cfg: TaskConfig{Launch: launch, ReadyPort: 1234, ReadyProtocol: "sctp"}, wantErr: "ready_protocol"},
		{name: "port too large", cfg: TaskConfig{Launch: launch, ReadyPort: 70000, ReadyProtocol: "udp"}, wantErr: "ready_port"},
		{name: "negative port", cfg: TaskConfig{Launch: launch, ReadyPort: -1, ReadyProtocol: "udp"}, wantErr: "ready_port"},
		{name: "bad timeout", cfg: TaskConfig{Launch: launch, ReadyPort: 1234, ReadyProtocol: "udp", ReadyTimeout: "5 m"}, wantErr: "ready_timeout"},
		{name: "zero timeout", cfg: TaskConfig{Launch: launch, ReadyPort: 1234, ReadyProtocol: "udp", ReadyTimeout: "0s"}, wantErr: "ready_timeout"},
		{name: "install-only task", cfg: TaskConfig{ReadyPort: 1234, ReadyProtocol: "udp"}, wantErr: "launch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseReadyCheck(c.cfg)
			if c.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

// fakeProc builds a /proc-like tree: net/<file> tables and, per pid, an fd
// directory of socket symlinks.
func fakeProc(t *testing.T, tables map[string]string, fds map[int32][]string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "net"), 0o755))
	for name, body := range tables {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "net", name), []byte(body), 0o644))
	}
	for pid, inodes := range fds {
		fdDir := filepath.Join(dir, itoa(pid), "fd")
		require.NoError(t, os.MkdirAll(fdDir, 0o755))
		for i, ino := range inodes {
			require.NoError(t, os.Symlink("socket:["+ino+"]", filepath.Join(fdDir, itoa(int32(i+3)))))
		}
	}
	return dir
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestBoundSocketInodes(t *testing.T) {
	// 0x3C15 = 15381, 0x3D15 = 15637.
	proc := fakeProc(t, map[string]string{
		"tcp": tcpHeader +
			"   0: 00000000:3D15 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0 100 0 0 10 0\n" + // listening on 15637
			"   1: 0100007F:3D15 0100007F:9999 01 00000000:00000000 00:00000000 00000000     0        0 1002 1 0 100 0 0 10 0\n" + // established, not a listener
			"   2: 00000000:3C15 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1003 1 0 100 0 0 10 0\n", // other port
		"tcp6": tcpHeader +
			"   0: 00000000000000000000000000000000:3D15 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004 1 0 100 0 0 10 0\n",
		"udp": tcpHeader +
			"   0: 00000000:3D15 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 2001 2 0 0\n" +
			"   1: 00000000:3C15 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 2002 2 0 0\n",
		"udp6": tcpHeader,
	}, nil)

	require.Equal(t, map[string]bool{"1001": true, "1004": true}, boundSocketInodes(proc, "tcp", 15637))
	require.Equal(t, map[string]bool{"2001": true}, boundSocketInodes(proc, "udp", 15637))
	require.Empty(t, boundSocketInodes(proc, "udp", 9999))
	require.Empty(t, boundSocketInodes(t.TempDir(), "udp", 15637), "missing tables mean not listening, not a panic")
}

func TestPortReady_OnlyTaskProcessesCount(t *testing.T) {
	udp := tcpHeader + "   0: 00000000:3D15 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 2001 2 0 0\n"
	rc := readyCheck{Protocol: "udp", Port: 15637, Timeout: time.Minute}

	// pid 10 belongs to the task and holds a different socket; pid 99 is a
	// stranger holding the game port. The task is not ready.
	proc := fakeProc(t, map[string]string{"udp": udp}, map[int32][]string{10: {"555"}, 99: {"2001"}})
	require.False(t, portReady(proc, rc, []int32{10}), "a foreign process holding the port must not count")

	// The task's own process now holds the socket.
	proc = fakeProc(t, map[string]string{"udp": udp}, map[int32][]string{10: {"555", "2001"}})
	require.True(t, portReady(proc, rc, []int32{10}))

	// No task process at all: nothing to be ready.
	require.False(t, portReady(proc, rc, nil))
}

func TestPortReady_UnreadableFdsFallBackToPortBound(t *testing.T) {
	udp := tcpHeader + "   0: 00000000:3D15 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 2001 2 0 0\n"
	proc := fakeProc(t, map[string]string{"udp": udp}, nil)
	// pid 10's fd entry is a file, so listing it fails (like EACCES for a
	// process the driver may not inspect): fall back to "the port is bound".
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "10"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "10", "fd"), nil, 0o644))
	rc := readyCheck{Protocol: "udp", Port: 15637, Timeout: time.Minute}
	require.True(t, portReady(proc, rc, []int32{10}))

	// Still false when nothing is bound at all.
	require.False(t, portReady(proc, readyCheck{Protocol: "udp", Port: 1, Timeout: time.Minute}, []int32{10}))
}

func TestWaitReady_RealSockets(t *testing.T) {
	self := func() []int32 { return []int32{int32(os.Getpid())} }

	t.Run("udp listener present", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer pc.Close()
		port := pc.LocalAddr().(*net.UDPAddr).Port
		got := waitReady(context.Background(), readyCheck{Protocol: "udp", Port: port, Timeout: 5 * time.Second}, "/proc", self, 20*time.Millisecond)
		require.Equal(t, readyOK, got)
	})

	t.Run("tcp listener present", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer l.Close()
		port := l.Addr().(*net.TCPAddr).Port
		got := waitReady(context.Background(), readyCheck{Protocol: "tcp", Port: port, Timeout: 5 * time.Second}, "/proc", self, 20*time.Millisecond)
		require.Equal(t, readyOK, got)
	})

	t.Run("listener appears after a delay", func(t *testing.T) {
		// Reserve a free port, release it, and bind it again 300ms later.
		tmp, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		port := tmp.LocalAddr().(*net.UDPAddr).Port
		tmp.Close()
		go func() {
			time.Sleep(300 * time.Millisecond)
			if pc, err := net.ListenPacket("udp", "127.0.0.1:"+portStr(port)); err == nil {
				time.AfterFunc(5*time.Second, func() { pc.Close() })
			}
		}()
		got := waitReady(context.Background(), readyCheck{Protocol: "udp", Port: port, Timeout: 5 * time.Second}, "/proc", self, 20*time.Millisecond)
		require.Equal(t, readyOK, got)
	})

	t.Run("nothing listens: times out", func(t *testing.T) {
		tmp, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		port := tmp.LocalAddr().(*net.UDPAddr).Port
		tmp.Close()
		start := time.Now()
		got := waitReady(context.Background(), readyCheck{Protocol: "udp", Port: port, Timeout: 300 * time.Millisecond}, "/proc", self, 20*time.Millisecond)
		require.Equal(t, readyTimeout, got)
		require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		got := waitReady(ctx, readyCheck{Protocol: "udp", Port: 1, Timeout: time.Minute}, "/proc", self, 20*time.Millisecond)
		require.Equal(t, readyAborted, got)
	})
}

func portStr(p int) string { return strconv.Itoa(p) }

// A task process that never opens its port is killed whole (child included),
// and a process that does is left alone.
func TestKillNotReady_StopsTheWholeTree(t *testing.T) {
	// sh forks a long sleep: killing only the top-level pid would orphan it.
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	rootPid := int32(cmd.Process.Pid)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-int(rootPid), syscall.SIGKILL) })

	c := newUsageCollector("test/not-ready", rootPid, cpustats.Compute{})
	require.Eventually(t, func() bool { return len(c.liveMembers()) >= 2 }, 5*time.Second, 20*time.Millisecond, "sh and its sleep child")
	before := c.liveMembers()

	require.True(t, killNotReady("/proc", c.liveMembers, hclog.NewNullLogger()))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("task root process still running after killNotReady")
	}
	// Check the pids themselves: once the root is gone the collector no longer
	// finds an orphaned child through the process tree.
	for _, pid := range before {
		require.Eventually(t, func() bool { return !procAlive("/proc", pid) }, 5*time.Second, 20*time.Millisecond, "pid %d must be gone", pid)
	}
}

func TestFinalExitResult(t *testing.T) {
	// Normal exits are reported as before.
	require.Equal(t, 0, finalExitResult(nil, nil).ExitCode)
	require.True(t, finalExitResult(nil, nil).Successful())

	// A not-ready kill is always a failure, even if the wrapper exited 0, so
	// Nomad applies the job's restart policy.
	notReady := errors.New("udp port 15637 not listening")
	r := finalExitResult(nil, notReady)
	require.False(t, r.Successful())
	require.NotEqual(t, 0, r.ExitCode)
	require.ErrorIs(t, r.Err, notReady)

	// A signal-killed wrapper keeps its exit code and gains the reason.
	killed := exec.Command("/bin/sh", "-c", "kill -9 $$")
	werr := killed.Run()
	r = finalExitResult(werr, notReady)
	require.False(t, r.Successful())
	require.ErrorIs(t, r.Err, notReady)
}
