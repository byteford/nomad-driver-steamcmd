package steamcmd

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	pluginName = "steamcmd"

	fingerprintPeriod = 30 * time.Second
)

// pluginVersion is overridden at release-build time via
// `-ldflags "-X github.com/byteford/nomad-driver-steamcmd/driver.pluginVersion=vX.Y.Z"`
// (see .github/workflows/release.yml). Left as this dev default for local
// builds (`make build`) and CI runs that don't inject a real tag.
var pluginVersion = "v0.0.0-dev"

var PluginInfo = &base.PluginInfoResponse{
	Type:              base.PluginTypeDriver,
	PluginApiVersions: []string{drivers.ApiVersion010},
	PluginVersion:     pluginVersion,
	Name:              pluginName,
}

// Capabilities declares what this driver supports. Notably: no FSIsolation
// (we deliberately run raw_exec-style, no chroot/namespace) and
// NetIsolationModes is host-only for the same reason. Because Nomad does no
// bind-mounting for a non-isolating driver, volume_mount destinations
// (cfg.Mounts) are symlinked into the task dir by setupMounts in mounts.go.
var driverCapabilities = &drivers.Capabilities{
	SendSignals: true,
	Exec:        false,
	FSIsolation: drivers.FSIsolationNone,
	NetIsolationModes: []drivers.NetIsolationMode{
		drivers.NetIsolationModeHost,
	},
	MustInitiateNetwork: false,
	MountConfigs:        drivers.MountConfigSupportAll,
}

// Driver implements drivers.DriverPlugin for steamcmd-managed tasks.
type Driver struct {
	eventer     *eventer.Eventer
	config      PluginConfig
	nomadConfig *base.ClientDriverConfig
	compute     cpustats.Compute // node CPU capacity, used to report TotalTicks

	tasks  sync.Map // task ID -> *taskHandle
	ctx    context.Context
	cancel context.CancelFunc
	logger hclog.Logger

	installSem chan struct{} // bounds concurrent steamcmd installs if MaxConcurrent > 0
}

func NewPlugin(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)
	return &Driver{
		eventer: eventer.NewEventer(ctx, logger),
		config:  PluginConfig{SteamCmdPath: "steamcmd"},
		ctx:     ctx,
		cancel:  cancel,
		logger:  logger,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return PluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var pluginConfig PluginConfig
	if len(cfg.PluginConfig) > 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &pluginConfig); err != nil {
			return err
		}
	}
	if pluginConfig.SteamCmdPath == "" {
		pluginConfig.SteamCmdPath = "steamcmd"
	}

	d.config = pluginConfig
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
		d.compute = cfg.AgentConfig.Compute()
	}
	if pluginConfig.MaxConcurrent > 0 {
		d.installSem = make(chan struct{}, pluginConfig.MaxConcurrent)
	} else {
		d.installSem = nil
	}

	// Diagnostic: kept in place as an ongoing sanity check (and a
	// regression guard exercised by e2e.yml's "Assert plugin-level agent
	// config values are actually forwarded" step) that the agent's
	// `plugin "nomad-driver-steamcmd" { config { ... } }` stanza is
	// actually reaching this driver, after a real Nomad version-specific
	// bug in this exact mechanism was found and fixed by upgrading the
	// tested Nomad version -- see PluginConfig's doc comment.
	d.logger.Info("steamcmd plugin config loaded",
		"raw_config_bytes", len(cfg.PluginConfig),
		"steamcmd_path", pluginConfig.SteamCmdPath,
		"default_login_anonymous", pluginConfig.DefaultLoginAnonymous,
		"default_login_username", pluginConfig.DefaultLoginUsername,
		"max_concurrent_installs", pluginConfig.MaxConcurrent,
	)
	return nil
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return driverCapabilities, nil
}

// Fingerprint reports node health/attributes at fingerprintPeriod. A node
// is only eligible to schedule steamcmd tasks if the binary is present and
// executable -- this is what lets job specs rely on the scheduler placing
// work only where steamcmd actually exists, rather than failing at
// StartTask time. It deliberately does not run steamcmd: that happens every
// fingerprintPeriod, can trigger a steamcmd self-update, and can collide with
// a real install using the same ~/Steam.
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTicker(fingerprintPeriod)
	defer ticker.Stop()

	for {
		ch <- d.buildFingerprint()
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	binPath, err := exec.LookPath(d.config.SteamCmdPath)
	if err != nil {
		return &drivers.Fingerprint{
			Health:            drivers.HealthStateUndetected,
			HealthDescription: fmt.Sprintf("steamcmd not found on PATH (%s): %v", d.config.SteamCmdPath, err),
		}
	}

	attrs := map[string]*pstructs.Attribute{
		"driver.steamcmd":      pstructs.NewBoolAttribute(true),
		"driver.steamcmd.path": pstructs.NewStringAttribute(binPath),
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            drivers.HealthStateHealthy,
		HealthDescription: "steamcmd ready",
	}
}

func (d *Driver) acquireInstallSlot(ctx context.Context) func() {
	if d.installSem == nil {
		return func() {}
	}
	select {
	case d.installSem <- struct{}{}:
	case <-ctx.Done():
		return func() {}
	}
	return func() { <-d.installSem }
}
