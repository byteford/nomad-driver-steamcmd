package steamcmd

import (
	"os"
	"syscall"
	"testing"
)

func TestBuildLaunchCommand(t *testing.T) {
	cases := []struct {
		name        string
		cfg         TaskConfig
		rt          windowsRuntime
		wantCommand string
		wantArgs    []string
	}{
		{
			name: "non-windows platform is left untouched",
			cfg: TaskConfig{
				Platform: "",
				Launch:   &LaunchConfig{Command: "PalServer.sh", Args: []string{"-port=8211"}},
			},
			wantCommand: "PalServer.sh",
			wantArgs:    []string{"-port=8211"},
		},
		{
			name: "windows platform wraps with <proton>/proton run, no display",
			cfg: TaskConfig{
				Platform: "windows",
				Launch:   &LaunchConfig{Command: "FoundryDedicatedServer.exe", Args: []string{"-batchmode", "-nographics"}},
			},
			rt:          windowsRuntime{ProtonPath: "/opt/GE-Proton9-20"},
			wantCommand: "/opt/GE-Proton9-20/proton",
			wantArgs:    []string{"run", "FoundryDedicatedServer.exe", "-batchmode", "-nographics"},
		},
		{
			name: "windows platform with windows_display nests proton under xvfb-run --auto-servernum",
			cfg: TaskConfig{
				Platform:       "windows",
				WindowsDisplay: true,
				Launch:         &LaunchConfig{Command: "FoundryDedicatedServer.exe", Args: []string{"-batchmode", "-nographics"}},
			},
			rt:          windowsRuntime{ProtonPath: "/opt/GE-Proton9-20"},
			wantCommand: "xvfb-run",
			wantArgs:    []string{"--auto-servernum", "/opt/GE-Proton9-20/proton", "run", "FoundryDedicatedServer.exe", "-batchmode", "-nographics"},
		},
		{
			name: "windows platform, no launch args at all",
			cfg: TaskConfig{
				Platform: "windows",
				Launch:   &LaunchConfig{Command: "server.exe"},
			},
			rt:          windowsRuntime{ProtonPath: "/opt/GE-Proton9-20"},
			wantCommand: "/opt/GE-Proton9-20/proton",
			wantArgs:    []string{"run", "server.exe"},
		},
		{
			name: "proton runner wraps with <proton>/proton run",
			cfg: TaskConfig{
				Platform: "windows",
				Launch:   &LaunchConfig{Command: "server.exe", Args: []string{"-batchmode"}},
			},
			rt:          windowsRuntime{ProtonPath: "/opt/GE-Proton9-20"},
			wantCommand: "/opt/GE-Proton9-20/proton",
			wantArgs:    []string{"run", "server.exe", "-batchmode"},
		},
		{
			name: "proton runner with windows_display nests under xvfb-run",
			cfg: TaskConfig{
				Platform:       "windows",
				WindowsDisplay: true,
				Launch:         &LaunchConfig{Command: "server.exe"},
			},
			rt:          windowsRuntime{ProtonPath: "/opt/GE-Proton9-20/proton"},
			wantCommand: "xvfb-run",
			wantArgs:    []string{"--auto-servernum", "/opt/GE-Proton9-20/proton", "run", "server.exe"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCommand, gotArgs := buildLaunchCommand(tc.cfg, tc.rt)
			if gotCommand != tc.wantCommand {
				t.Errorf("command = %q, want %q", gotCommand, tc.wantCommand)
			}
			if len(gotArgs) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", gotArgs, tc.wantArgs)
			}
			for i := range gotArgs {
				if gotArgs[i] != tc.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q (full: got=%v want=%v)", i, gotArgs[i], tc.wantArgs[i], gotArgs, tc.wantArgs)
				}
			}
		})
	}
}

func TestResolveLaunchPath(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		installDir string
		want       string
	}{
		{
			name:       "relative path with directory component resolves against installDir",
			command:    "local/steamapp/PalServer.sh",
			installDir: "/alloc/task/local/steamapp",
			want:       "/alloc/task/local/steamapp/local/steamapp/PalServer.sh",
		},
		{
			name:       "bare command name is left alone for PATH lookup",
			command:    "xvfb-run",
			installDir: "/alloc/task/local/steamapp",
			want:       "xvfb-run",
		},
		{
			name:       "bare command name 'wine' is left alone for PATH lookup",
			command:    "wine",
			installDir: "/alloc/task/local/steamapp",
			want:       "wine",
		},
		{
			name:       "already-absolute path is left alone",
			command:    "/usr/bin/wine",
			installDir: "/alloc/task/local/steamapp",
			want:       "/usr/bin/wine",
		},
		{
			name:       "relative path directly under installDir",
			command:    "PalServer.sh",
			installDir: "/alloc/task/local/steamapp",
			want:       "PalServer.sh", // no "/" in command -> bare name, PATH lookup
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLaunchPath(tc.command, tc.installDir)
			if got != tc.want {
				t.Errorf("resolveLaunchPath(%q, %q) = %q, want %q", tc.command, tc.installDir, got, tc.want)
			}
		})
	}
}

func TestSignalProcessGroup_NonexistentPIDReturnsProcessDone(t *testing.T) {
	// A process group that's already gone (e.g. the task already exited
	// by the time StopTask/DestroyTask gets to it) must be treated as
	// "already done", not as a real failure -- same contract
	// cmd.Process.Signal() had via os.ErrProcessDone before this switched
	// to signaling the whole group via syscall.Kill(-pid, ...).
	const definitelyNotARealPID = 999999999

	err := signalProcessGroup(definitelyNotARealPID, syscall.SIGTERM)
	if err != os.ErrProcessDone {
		t.Errorf("signalProcessGroup on a nonexistent PID = %v, want os.ErrProcessDone", err)
	}
}
