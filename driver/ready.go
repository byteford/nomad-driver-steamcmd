package steamcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// defaultReadyTimeout is how long a launched server gets to open its port when
// ready_timeout is not set. A Windows server under Proton builds its prefix on
// the first start, which can take minutes.
const defaultReadyTimeout = 5 * time.Minute

// readyPollInterval is how often the port is checked.
const readyPollInterval = 2 * time.Second

// readyCheck is the startup check configured with ready_port, ready_protocol
// and ready_timeout: the launched server must have one of its processes
// listening on Port within Timeout.
type readyCheck struct {
	Protocol string // "tcp" or "udp"
	Port     int
	Timeout  time.Duration
}

// parseReadyCheck validates the ready_* options. nil means no check is
// configured. Options that would be silently ignored are errors, the same
// stance as install_timeout.
func parseReadyCheck(t TaskConfig) (*readyCheck, error) {
	if t.ReadyPort == 0 {
		if t.ReadyProtocol != "" || t.ReadyTimeout != "" {
			return nil, errors.New("ready_protocol and ready_timeout need ready_port to be set")
		}
		return nil, nil
	}
	if t.ReadyPort < 1 || t.ReadyPort > 65535 {
		return nil, fmt.Errorf("invalid ready_port %d: must be 1-65535", t.ReadyPort)
	}
	if t.Launch == nil {
		return nil, errors.New("ready_port needs a launch block: an install-only task has no server to check")
	}
	if t.ReadyProtocol != "tcp" && t.ReadyProtocol != "udp" {
		return nil, fmt.Errorf("invalid ready_protocol %q: must be \"tcp\" or \"udp\" when ready_port is set", t.ReadyProtocol)
	}
	timeout := defaultReadyTimeout
	if t.ReadyTimeout != "" {
		d, err := time.ParseDuration(t.ReadyTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid ready_timeout %q: %w", t.ReadyTimeout, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid ready_timeout %q: must be a positive duration", t.ReadyTimeout)
		}
		timeout = d
	}
	return &readyCheck{Protocol: t.ReadyProtocol, Port: t.ReadyPort, Timeout: timeout}, nil
}

func (rc readyCheck) String() string { return fmt.Sprintf("%s port %d", rc.Protocol, rc.Port) }

// boundSocketInodes returns the inodes of the sockets bound to port for
// protocol ("tcp" or "udp", IPv4 and IPv6) according to procDir/net. TCP only
// counts listening sockets (state 0A); a bound UDP socket has no state to
// speak of, so any UDP socket on the port counts. A missing table counts as
// nothing bound.
func boundSocketInodes(procDir, protocol string, port int) map[string]bool {
	out := make(map[string]bool)
	for _, table := range []string{protocol, protocol + "6"} {
		b, err := os.ReadFile(procDir + "/net/" + table)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if i == 0 || len(f) < 10 {
				continue // header or short line
			}
			colon := strings.LastIndexByte(f[1], ':')
			if colon < 0 {
				continue
			}
			p, err := strconv.ParseUint(f[1][colon+1:], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			if protocol == "tcp" && f[3] != "0A" {
				continue
			}
			out[f[9]] = true
		}
	}
	return out
}

// portReady reports whether a process of the task holds a socket on the
// configured port. Matching the socket to the task's own processes means a
// stranger (or a leftover from an earlier run) holding the port does not make
// a server that never bound it look ready. When some task process's fds cannot
// be listed (the driver is not root), that proof is unavailable, so it falls
// back to "the port is bound by someone".
func portReady(procDir string, rc readyCheck, pids []int32) bool {
	inodes := boundSocketInodes(procDir, rc.Protocol, rc.Port)
	if len(inodes) == 0 {
		return false
	}
	unreadable := false
	for _, pid := range pids {
		fdDir := procDir + "/" + strconv.Itoa(int(pid)) + "/fd"
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			if !os.IsNotExist(err) {
				unreadable = true
			}
			continue // a process that exited since it was listed is not a problem
		}
		for _, e := range entries {
			target, err := os.Readlink(fdDir + "/" + e.Name())
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			if inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
				return true
			}
		}
	}
	return unreadable
}

type readyResult int

const (
	readyOK      readyResult = iota // the port is listening
	readyTimeout                    // the deadline passed first
	readyAborted                    // ctx was cancelled first (the task ended)
)

// waitReady polls until the task is listening on rc's port, rc.Timeout passes,
// or ctx is cancelled. members returns the task's current live pids.
func waitReady(ctx context.Context, rc readyCheck, procDir string, members func() []int32, interval time.Duration) readyResult {
	deadline := time.NewTimer(rc.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if portReady(procDir, rc, members()) {
			return readyOK
		}
		select {
		case <-ctx.Done():
			return readyAborted
		case <-deadline.C:
			if portReady(procDir, rc, members()) {
				return readyOK
			}
			return readyTimeout
		case <-tick.C:
		}
	}
}

// killNotReady stops every process of a task that never became ready. The
// server is hung, so there is nothing to save: SIGKILL straight away, then the
// support processes (wineserver, Xvfb) in the usual order. It reports whether
// everything is gone.
func killNotReady(procDir string, list func() []int32, logger hclog.Logger) bool {
	return terminateProcesses(procDir, list, syscall.SIGKILL, 5*time.Second, logger)
}

// finalExitResult turns the launched process's Wait error into the task's exit
// result. If the task was killed for not becoming ready, the result is always
// a failure carrying that reason, even when the wrapper happened to exit 0, so
// Nomad applies the job's restart policy instead of treating it as done.
func finalExitResult(waitErr, notReady error) *drivers.ExitResult {
	var res *drivers.ExitResult
	switch {
	case waitErr == nil:
		res = &drivers.ExitResult{ExitCode: 0}
	default:
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			res = &drivers.ExitResult{ExitCode: exitErr.ExitCode()}
		} else {
			res = &drivers.ExitResult{ExitCode: -1, Err: waitErr}
		}
	}
	if notReady != nil {
		if res.ExitCode == 0 {
			res.ExitCode = 1
		}
		res.Err = notReady
	}
	return res
}

// watchReady runs the startup check for a freshly launched task and, if the
// deadline passes with the port still closed, kills the task. It returns when
// the task is ready, killed, or already gone.
func (d *Driver) watchReady(h *taskHandle, cfg *drivers.TaskConfig, rc readyCheck) {
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	go func() {
		select {
		case <-h.doneCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	c := d.collectorFor(h)
	switch waitReady(ctx, rc, "/proc", c.liveMembers, readyPollInterval) {
	case readyOK:
		d.eventer.EmitEvent(&drivers.TaskEvent{TaskID: cfg.ID, TaskName: cfg.Name, Timestamp: time.Now(),
			Message: fmt.Sprintf("ready: %s is listening", rc)})
	case readyTimeout:
		reason := fmt.Errorf("%s was not listening %s after launch; the task was killed so it restarts", rc, rc.Timeout)
		d.logger.Warn("task never became ready, killing it", "task_id", cfg.ID, "check", rc.String(), "timeout", rc.Timeout)
		d.eventer.EmitEvent(&drivers.TaskEvent{TaskID: cfg.ID, TaskName: cfg.Name, Timestamp: time.Now(), Message: reason.Error()})
		h.stateLock.Lock()
		h.notReady = reason
		h.stateLock.Unlock()
		if !killNotReady("/proc", c.liveMembers, d.logger) {
			d.logger.Warn("some task processes survived the not-ready kill", "task_id", cfg.ID)
		}
		d.cleanupWine(h)
	}
}
