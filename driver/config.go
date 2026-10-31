package steamcmd

import (
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

// PluginConfig is the client-level (agent-wide) configuration for the
// plugin, set via a `plugin "nomad-driver-steamcmd" { config { ... } }`
// stanza in the Nomad client's agent config. It provides fleet-wide
// defaults that a task can omit and inherit from.
//
// HISTORY: explicit values in this block were observed not reaching
// SetConfig at all on Nomad v1.9.3 -- fields silently fell back to their
// hclspec.NewDefault literal regardless of what nomad.hcl actually set.
// This was extensively isolated (not a nested-block issue, not a
// bool-vs-string issue, not a NewDefault issue, not file-merge/adjacency,
// not this driver's own SetConfig/ConfigSchema pattern -- compared
// line-by-line against hashicorp/nomad-driver-podman's current source and
// matches it exactly) and turned out to be a genuine version-specific
// Nomad regression: a canary test (a field set to a value deliberately
// different from its own default) confirmed forwarding works correctly
// on Nomad v1.10.0. DefaultLoginAnonymous was temporarily a string
// ("true"/"false") during that investigation on the theory that bools
// specifically were affected -- confirmed a red herring once the real
// (version-specific) cause was found, so it's back to a native bool.
// See driver/config_test.go and .github/workflows/e2e.yml's "Assert
// plugin-level agent config values are actually forwarded" step, which
// exercises this on every CI run going forward as a regression guard.
type PluginConfig struct {
	SteamCmdPath             string `codec:"steamcmd_path"`
	DefaultLoginAnonymous    bool   `codec:"default_login_anonymous"`
	DefaultLoginUsername     string `codec:"default_login_username"`
	DefaultLoginPassword     string `codec:"default_login_password"`
	DefaultLoginPasswordFile string `codec:"default_login_password_file"`
	DefaultProtonPath        string `codec:"default_proton_path"`
	InstallRoot              string `codec:"install_root"`
	MaxConcurrent            int    `codec:"max_concurrent_installs"`
}

// DefaultLogin builds a LoginConfig from the flat default_login_* fields.
func (p PluginConfig) DefaultLogin() LoginConfig {
	return LoginConfig{
		Anonymous:    p.DefaultLoginAnonymous,
		Username:     p.DefaultLoginUsername,
		Password:     p.DefaultLoginPassword,
		PasswordFile: p.DefaultLoginPasswordFile,
	}
}

// configSpec is the hclspec for the plugin-level config block.
var configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"steamcmd_path": hclspec.NewDefault(
		hclspec.NewAttr("steamcmd_path", "string", false),
		hclspec.NewLiteral(`"steamcmd"`),
	),
	"install_root": hclspec.NewDefault(
		hclspec.NewAttr("install_root", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"max_concurrent_installs": hclspec.NewDefault(
		hclspec.NewAttr("max_concurrent_installs", "number", false),
		hclspec.NewLiteral("0"),
	),
	"default_login_anonymous": hclspec.NewDefault(
		hclspec.NewAttr("default_login_anonymous", "bool", false),
		hclspec.NewLiteral("true"),
	),
	"default_login_username": hclspec.NewDefault(
		hclspec.NewAttr("default_login_username", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"default_login_password": hclspec.NewDefault(
		hclspec.NewAttr("default_login_password", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"default_login_password_file": hclspec.NewDefault(
		hclspec.NewAttr("default_login_password_file", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"default_proton_path": hclspec.NewDefault(
		hclspec.NewAttr("default_proton_path", "string", false),
		hclspec.NewLiteral(`""`),
	),
})

// TaskConfig is the per-task configuration set in a job spec's
// `driver = "steamcmd"` task's `config { ... }` block.
//
// RepairManifest (repair_stale_manifest) opts in to deleting a stale
// steamapps/appmanifest_<app_id>.acf and retrying the install once when
// steamcmd fails with "state is 0x6 after update job". See installWithRepair.
//
// Login fields are flat here too (login_anonymous, not a nested login{}
// block), for the same reason as PluginConfig above.
type TaskConfig struct {
	AppID             string              `codec:"app_id"`
	InstallDir        string              `codec:"install_dir"`
	Beta              string              `codec:"beta"`
	BetaPassword      string              `codec:"beta_password"`
	Validate          bool                `codec:"validate"`
	Platform          string              `codec:"platform"`
	ProtonPath        string              `codec:"proton_path"`
	CompatDataPath    string              `codec:"compat_data_path"`
	CompatClientPath  string              `codec:"compat_client_path"`
	WindowsDisplay    bool                `codec:"windows_display"`
	LoginAnonymous    bool                `codec:"login_anonymous"`
	LoginUsername     string              `codec:"login_username"`
	LoginPassword     string              `codec:"login_password"`
	LoginPasswordFile string              `codec:"login_password_file"`
	ConfigFiles       []map[string]string `codec:"config_files"`
	FollowLogs        []string            `codec:"follow_logs"`
	Launch            *LaunchConfig       `codec:"launch"`
	UpdateOnStart     bool                `codec:"update_on_start"`
	InstallTimeout    string              `codec:"install_timeout"`
	RepairManifest    bool                `codec:"repair_stale_manifest"`
}

// Login builds a LoginConfig from the task's flat login_* fields.
func (t TaskConfig) Login() LoginConfig {
	return LoginConfig{
		Anonymous:    t.LoginAnonymous,
		Username:     t.LoginUsername,
		Password:     t.LoginPassword,
		PasswordFile: t.LoginPasswordFile,
	}
}

// LoginConfig describes how steamcmd should authenticate. Built from
// either TaskConfig's or PluginConfig's flat login_*/default_login_*
// fields -- it is a plain internal convenience type, not itself an
// hclspec decode target.
type LoginConfig struct {
	Anonymous    bool
	Username     string
	Password     string
	PasswordFile string
}

// LaunchConfig describes the process to exec once install/update completes.
// If nil, the task is install-only and exits after steamcmd finishes.
type LaunchConfig struct {
	Command string            `codec:"command"`
	Args    []string          `codec:"args"`
	Env     map[string]string `codec:"env"`
}

var launchSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"command": hclspec.NewAttr("command", "string", true),
	"args": hclspec.NewDefault(
		hclspec.NewAttr("args", "list(string)", false),
		hclspec.NewLiteral("[]"),
	),
	"env": hclspec.NewDefault(
		hclspec.NewAttr("env", "map(string)", false),
		hclspec.NewLiteral("{}"),
	),
})

// taskConfigSpec is the hclspec Nomad uses to validate/decode a task's
// `config` block for this driver. Every optional attribute is wrapped in
// hclspec.NewDefault -- see the PluginConfig doc comment for why.
var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"app_id": hclspec.NewAttr("app_id", "string", true),
	"install_dir": hclspec.NewDefault(
		hclspec.NewAttr("install_dir", "string", false),
		hclspec.NewLiteral(`"local/steamapp"`),
	),
	"beta": hclspec.NewDefault(
		hclspec.NewAttr("beta", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"beta_password": hclspec.NewDefault(
		hclspec.NewAttr("beta_password", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"validate": hclspec.NewDefault(
		hclspec.NewAttr("validate", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"platform": hclspec.NewDefault(
		hclspec.NewAttr("platform", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"proton_path": hclspec.NewDefault(
		hclspec.NewAttr("proton_path", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"compat_data_path": hclspec.NewDefault(
		hclspec.NewAttr("compat_data_path", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"compat_client_path": hclspec.NewDefault(
		hclspec.NewAttr("compat_client_path", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"windows_display": hclspec.NewDefault(
		hclspec.NewAttr("windows_display", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"update_on_start": hclspec.NewDefault(
		hclspec.NewAttr("update_on_start", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"install_timeout": hclspec.NewDefault(
		hclspec.NewAttr("install_timeout", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"repair_stale_manifest": hclspec.NewDefault(
		hclspec.NewAttr("repair_stale_manifest", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"login_anonymous": hclspec.NewDefault(
		hclspec.NewAttr("login_anonymous", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"login_username": hclspec.NewDefault(
		hclspec.NewAttr("login_username", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"login_password": hclspec.NewDefault(
		hclspec.NewAttr("login_password", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"login_password_file": hclspec.NewDefault(
		hclspec.NewAttr("login_password_file", "string", false),
		hclspec.NewLiteral(`""`),
	),
	"config_files": hclspec.NewDefault(
		// list(map(string)), not map(string): Nomad's job parser wraps an HCL
		// map in a list (same as the docker driver's labels/sysctl).
		hclspec.NewAttr("config_files", "list(map(string))", false),
		hclspec.NewLiteral("[]"),
	),
	"follow_logs": hclspec.NewDefault(
		hclspec.NewAttr("follow_logs", "list(string)", false),
		hclspec.NewLiteral("[]"),
	),
	"launch": hclspec.NewBlock("launch", false, launchSpec),
})

// resolveLogin returns the effective login to use for a task: the task's
// own login_* fields if any are meaningfully set, otherwise the plugin's
// default_login_* fields.
func resolveLogin(task LoginConfig, def LoginConfig) LoginConfig {
	if !isZeroLogin(task) {
		return task
	}
	return def
}

func isZeroLogin(l LoginConfig) bool {
	return !l.Anonymous && l.Username == "" && l.Password == "" && l.PasswordFile == ""
}
