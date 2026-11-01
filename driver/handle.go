package steamcmd

import (
	"context"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// taskPhase tracks where a task is in its install -> launch lifecycle so
// RecoverTask knows which recovery path applies.
type taskPhase string

const (
	// phaseInstalling exists for documentation purposes only -- it is
	// never actually persisted, since StartTask now runs the install
	// synchronously and only calls SetDriverState once the outcome
	// (running or exited) is already known. See lifecycle.go StartTask.
	phaseInstalling taskPhase = "installing"
	phaseRunning    taskPhase = "running"
	phaseExited     taskPhase = "exited"
)

// taskState is the durable state persisted via handle.DriverState so a
// client agent restart can reattach correctly. It is intentionally small:
// enough to know which recovery path to take, not a full re-derivation of
// TaskConfig (Nomad hands that back separately on Recover).
type taskState struct {
	TaskConfig drivers.TaskConfig
	Phase      taskPhase
	Pid        int
	StartedAt  time.Time
	AppID      string
	InstallDir string

	// WineCleanup, if set, is run (`wineserver -k`) after the process group
	// is stopped. Persisted so recovered tasks can still clean up.
	WineCleanup *wineCleanup
}

// taskHandle is the in-memory handle for a running (or installing) task.
// One exists per task ID for the lifetime of the driver process.
type taskHandle struct {
	logger hclog.Logger

	taskConfig *drivers.TaskConfig
	state      taskState

	mu          sync.RWMutex
	cmd         *exec.Cmd
	exitResult  *drivers.ExitResult
	doneCh      chan struct{}
	stateLock   sync.Mutex
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitCode    int
	signal      int
	notReady    error
	ctx         context.Context
	cancel      context.CancelFunc
}

func newTaskHandle(logger hclog.Logger, cfg *drivers.TaskConfig) *taskHandle {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskHandle{
		logger:     logger.Named("handle").With("task_id", cfg.ID),
		taskConfig: cfg,
		doneCh:     make(chan struct{}),
		procState:  drivers.TaskStateRunning,
		startedAt:  time.Now(),
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
		DriverAttributes: map[string]string{
			"pid":         strconv.Itoa(h.state.Pid),
			"phase":       string(h.state.Phase),
			"app_id":      h.state.AppID,
			"install_dir": h.state.InstallDir,
		},
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) setPhase(p taskPhase) {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()
	h.state.Phase = p
}
