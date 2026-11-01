package steamcmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSteamcmd writes a tiny shell script that mimics steamcmd's observed
// (mis)behaviors so we can test our parsing logic without the real binary
// or network access.
func fakeSteamcmd(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fake steamcmd script requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "steamcmd")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
	return path
}

func TestRunInstall_GenuineSuccess(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "Success! App '2394010' fully installed."
exit 0`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.NoError(t, err)
	require.True(t, res.Success)
}

func TestRunInstall_ExitZeroButNoSuccessMarker(t *testing.T) {
	// This is the well-known steamcmd footgun: exit 0 with no real
	// completion. Our parser must not trust the exit code alone.
	bin := fakeSteamcmd(t, `echo "doing something inconclusive"
exit 0`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.False(t, res.Success)
}

func TestRunInstall_AuthFailure(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "FAILED (Invalid Password)"
exit 5`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.True(t, res.AuthFailed)
}

func TestRunInstall_SetlocaleWarningIsNotAnAuthFailure(t *testing.T) {
	// Regression test: steamcmd prints this harmless warning on every run
	// where the locale isn't generated. It contains "failed", which the old
	// case-insensitive bare-FAILED pattern treated as a login failure, so any
	// unrelated install error was reported as "authentication failed".
	bin := fakeSteamcmd(t, `echo "WARNING: setlocale('en_US.UTF-8') failed, using locale: 'C'. International characters may not work."
echo "Connecting anonymously to Steam Public...OK"
echo "Error! App '2278520' state is 0x6 after update job."
exit 8`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.False(t, res.AuthFailed)
	require.Contains(t, err.Error(), "install/update did not complete")
	require.NotContains(t, err.Error(), "authentication failed")
	require.Contains(t, err.Error(), "state is 0x6 after update job")
}

func TestRunInstall_SetlocaleWarningAfterErrorKeepsRealMessage(t *testing.T) {
	// The setlocale line must not overwrite the real error message either,
	// whichever order the two lines arrive in.
	bin := fakeSteamcmd(t, `echo "Error! App '2278520' state is 0x6 after update job."
echo "WARNING: setlocale('en_US.UTF-8') failed, using locale: 'C'."
exit 8`)
	var stdout, stderr bytes.Buffer

	_, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "state is 0x6 after update job")
}

func TestReAuthErr_Patterns(t *testing.T) {
	for _, line := range []string{
		"Logging in user 'someone' to Steam Public...FAILED (Invalid Password)",
		"FAILED login with result code Invalid Password",
		"Logging in user 'someone' to Steam Public...FAILED (Rate Limit Exceeded)",
		"Login Failure: Account Logon Denied",
		"Two-factor code mismatch",
	} {
		require.True(t, reAuthErr.MatchString(line), "should match: %q", line)
	}
	for _, line := range []string{
		"WARNING: setlocale('en_US.UTF-8') failed, using locale: 'C'.",
		"Connecting anonymously to Steam Public...OK",
		"Error! App '2278520' state is 0x6 after update job.",
		"Success! App '2278520' fully installed.",
	} {
		require.False(t, reAuthErr.MatchString(line), "should not match: %q", line)
	}
}

func TestRunInstall_SuccessMarkerDespiteNonzeroExit(t *testing.T) {
	// Some steamcmd versions return nonzero even after a clean install.
	// A real success marker should still be trusted.
	bin := fakeSteamcmd(t, `echo "Success! App '2394010' fully updated."
exit 7`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.NoError(t, err)
	require.True(t, res.Success)
}

func TestRunInstall_AlreadyUpToDateCountsAsSuccess(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "Success! App '2394010' already up to date."
exit 0`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.NoError(t, err)
	require.True(t, res.Success)
}

func TestRunInstall_SuccessAfterTransientAuthLikeLineIsNotAFailure(t *testing.T) {
	// Regression test for a real production bug: steamcmd sometimes
	// prints a line matching reAuthErr's (intentionally broad) patterns
	// during a normal login -- e.g. a retry, or 2FA-related text on a
	// session that ultimately authenticates via a cached token -- and
	// then goes on to install successfully. AuthFailed latching true on
	// that earlier line must not override a genuine success marker that
	// follows it. Confirmed in production: a real install completed
	// successfully end-to-end but was reported as a Driver Failure with
	// the success line itself as the (garbled) error message.
	bin := fakeSteamcmd(t, `echo "Login Failure retrying with cached session"
echo "Success! App '2915550' fully installed."
exit 0`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.NoError(t, err)
	require.True(t, res.Success)
	require.True(t, res.AuthFailed, "the transient line should still be recorded, just not treated as fatal")
}

func TestRunInstall_ProgressCallback(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "Update state (0x61) downloading, progress: 12.34 (100 / 811)"
echo "Success! App '90' fully installed."
exit 0`)
	var stdout, stderr bytes.Buffer
	var messages []string

	_, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, func(msg string) {
		messages = append(messages, msg)
	})
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	require.Contains(t, messages[0], "12.34")
}

func TestRunInstall_FlagsStaleManifestError(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "Update state (0x0) unknown, progress: 0.00 (0 / 0)"
echo "Error! App '2278520' state is 0x6 after update job."
exit 8`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.True(t, res.StaleManifest)
}

func TestRunInstall_OtherErrorsAreNotStaleManifest(t *testing.T) {
	bin := fakeSteamcmd(t, `echo "Error! App '2278520' state is 0x202 after update job."
exit 8`)
	var stdout, stderr bytes.Buffer

	res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
	require.Error(t, err)
	require.False(t, res.StaleManifest)
}

// writeManifest creates steamapps/appmanifest_<appID>.acf under installDir
// plus a savegame file that must survive any repair.
func writeManifest(t *testing.T, installDir, appID string) (manifest, save string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(installDir, "steamapps"), 0o755))
	manifest = filepath.Join(installDir, "steamapps", "appmanifest_"+appID+".acf")
	require.NoError(t, os.WriteFile(manifest, []byte("stale"), 0o644))
	save = filepath.Join(installDir, "savegame")
	require.NoError(t, os.WriteFile(save, []byte("precious"), 0o644))
	return manifest, save
}

var errStale = errors.New("steamcmd: install/update did not complete: App '2278520' state is 0x6 after update job.")

func staleResult() *installResult { return &installResult{StaleManifest: true} }

func TestInstallWithRepair_Disabled_DoesNothing(t *testing.T) {
	dir := t.TempDir()
	manifest, _ := writeManifest(t, dir, "2278520")
	calls := 0

	_, err := installWithRepair(context.Background(), false, dir, "2278520",
		func() (*installResult, error) { calls++; return staleResult(), errStale },
		func(string) { t.Fatal("onRepair must not be called when disabled") })

	require.ErrorIs(t, err, errStale)
	require.Equal(t, 1, calls)
	require.FileExists(t, manifest)
}

func TestInstallWithRepair_RemovesManifestAndRetriesOnce(t *testing.T) {
	dir := t.TempDir()
	manifest, save := writeManifest(t, dir, "2278520")
	calls := 0
	var repaired []string

	res, err := installWithRepair(context.Background(), true, dir, "2278520",
		func() (*installResult, error) {
			calls++
			if calls == 1 {
				return staleResult(), errStale
			}
			require.NoFileExists(t, manifest, "manifest must be gone before the retry")
			return &installResult{Success: true}, nil
		},
		func(p string) { repaired = append(repaired, p) })

	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 2, calls)
	require.Equal(t, []string{manifest}, repaired)
	require.FileExists(t, save, "nothing but the manifest may be removed")
}

func TestInstallWithRepair_OnlyRetriesOnce(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "2278520")
	calls := 0

	_, err := installWithRepair(context.Background(), true, dir, "2278520",
		func() (*installResult, error) { calls++; return staleResult(), errStale },
		func(string) {})

	require.Error(t, err)
	require.ErrorIs(t, err, errStale)
	require.Contains(t, err.Error(), "retried once")
	require.Equal(t, 2, calls)
}

func TestInstallWithRepair_NoManifestMeansNoRetry(t *testing.T) {
	dir := t.TempDir()
	calls := 0

	_, err := installWithRepair(context.Background(), true, dir, "2278520",
		func() (*installResult, error) { calls++; return staleResult(), errStale },
		func(string) { t.Fatal("nothing was removed") })

	require.ErrorIs(t, err, errStale)
	require.Equal(t, 1, calls)
}

func TestInstallWithRepair_OtherFailuresAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	manifest, _ := writeManifest(t, dir, "2278520")
	other := errors.New("steamcmd: authentication failed: x")
	calls := 0

	_, err := installWithRepair(context.Background(), true, dir, "2278520",
		func() (*installResult, error) { calls++; return &installResult{AuthFailed: true}, other },
		func(string) { t.Fatal("must not repair a non-stale failure") })

	require.ErrorIs(t, err, other)
	require.Equal(t, 1, calls)
	require.FileExists(t, manifest)
}

func TestInstallWithRepair_RejectsNonNumericAppID(t *testing.T) {
	dir := t.TempDir()
	// The file an empty app_id would hit if it were joined into the path unchecked.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "steamapps"), 0o755))
	victim := filepath.Join(dir, "steamapps", "appmanifest_.acf")
	require.NoError(t, os.WriteFile(victim, []byte("x"), 0o644))
	calls := 0

	for _, id := range []string{"", "../x", "12 34", "1/../../2"} {
		_, err := installWithRepair(context.Background(), true, dir, id,
			func() (*installResult, error) { calls++; return staleResult(), errStale },
			func(string) { t.Fatal("must not repair with an invalid app id") })
		require.ErrorIs(t, err, errStale)
	}
	require.Equal(t, 4, calls)
	require.FileExists(t, victim)
}

func TestInstallWithRepair_NoRetryWhenContextDone(t *testing.T) {
	dir := t.TempDir()
	manifest, _ := writeManifest(t, dir, "2278520")
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	_, err := installWithRepair(ctx, true, dir, "2278520",
		func() (*installResult, error) { calls++; cancel(); return staleResult(), errStale },
		func(string) { t.Fatal("must not repair after cancellation") })

	require.ErrorIs(t, err, errStale)
	require.Equal(t, 1, calls)
	require.FileExists(t, manifest)
}

func TestInstallWithRepair_EndToEndWithFakeSteamcmd(t *testing.T) {
	// A fake steamcmd that fails with the 0x6 error exactly while a manifest
	// exists in the install dir (arg 1) and succeeds once it is gone, like the
	// real one did after the manifest was deleted by hand.
	bin := fakeSteamcmd(t, `if [ -f "$1/steamapps/appmanifest_2278520.acf" ]; then
  echo "WARNING: setlocale('en_US.UTF-8') failed, using locale: 'C'."
  echo "Error! App '2278520' state is 0x6 after update job."
  exit 8
fi
echo "Success! App '2278520' fully installed."
exit 0`)
	dir := t.TempDir()
	manifest, save := writeManifest(t, dir, "2278520")
	var stdout, stderr bytes.Buffer
	var repaired []string

	res, err := installWithRepair(context.Background(), true, dir, "2278520",
		func() (*installResult, error) {
			return runInstall(context.Background(), bin, []string{dir}, &stdout, &stderr, nil)
		},
		func(p string) { repaired = append(repaired, p) })

	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, []string{manifest}, repaired)
	require.NoFileExists(t, manifest)
	require.FileExists(t, save)
}

// A single output line longer than the scanner's 1MB limit used to stop the
// scanner; with nothing draining the pipe, steamcmd then blocked on its next
// write and runInstall never returned.
func TestRunInstall_OverlongLineDoesNotHang(t *testing.T) {
	bin := fakeSteamcmd(t, `head -c 2000000 /dev/zero | tr '\0' 'x'
echo
echo "Success! App '2394010' fully installed."
exit 0`)
	var stdout, stderr bytes.Buffer

	type out struct {
		res *installResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := runInstall(context.Background(), bin, []string{"+quit"}, &stdout, &stderr, nil)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		require.NoError(t, o.err)
		require.True(t, o.res.Success)
	case <-time.After(20 * time.Second):
		t.Fatal("runInstall hung on an overlong output line")
	}
}
