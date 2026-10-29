package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/launchd"
	"github.com/Dudude-bit/ccswap/internal/update"
)

// github stands in for the newest release and records what ccswap update installs and reinstalls.
type github struct {
	latest      string
	installed   []string
	reinstalled []string
}

func (g *github) seam(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ccswap")
	oldLatest, oldInstall, oldTarget, oldRe := latestRelease, installRelease, updateTarget, reinstall
	t.Cleanup(func() {
		latestRelease, installRelease, updateTarget, reinstall = oldLatest, oldInstall, oldTarget, oldRe
	})
	latestRelease = func(context.Context, string) (update.Release, error) {
		return update.Release{Version: g.latest, URL: "https://github.com/Dudude-bit/ccswap/releases/tag/" + g.latest}, nil
	}
	installRelease = func(_ context.Context, rel update.Release, to string) error {
		g.installed = append(g.installed, rel.Version+" "+to)
		return nil
	}
	updateTarget = func() (string, error) { return bin, nil }
	reinstall = func(_ *cobra.Command, b string) error {
		g.reinstalled = append(g.reinstalled, b)
		return nil
	}
	return bin
}

// runAs runs ccswap built as version.
func runAs(t *testing.T, version string, args ...string) (string, error) {
	t.Helper()
	root := NewRoot(version)
	var out syncBuf
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := execute(context.Background(), root)
	return out.String(), err
}

// noService has no LaunchAgent installed.
func noService(t *testing.T) string {
	setGOOS(t, "darwin")
	oldDir, oldRun := launchd.Dir, launchd.Run
	t.Cleanup(func() { launchd.Dir, launchd.Run = oldDir, oldRun })
	launchd.Dir = t.TempDir()
	launchd.Run = func(...string) ([]byte, error) { return nil, &exec.ExitError{} }
	return launchd.Dir
}

func TestUpdateCheck(t *testing.T) {
	env(t, nil)
	noService(t)
	g := &github{latest: "v0.2.0"}
	g.seam(t)
	out, err := runAs(t, "v0.1.0", "update", "--check")
	if err != nil || out != i18n.T("update.available", "v0.2.0", "v0.1.0", "https://github.com/Dudude-bit/ccswap/releases/tag/v0.2.0") {
		t.Fatalf("%v %q", err, out)
	}
	for _, v := range []string{"v0.2.0", "v0.2.1-0.20261101000000-abcdefabcdef"} {
		if out, err := runAs(t, v, "update"); err != nil || out != i18n.T("update.latest", v) {
			t.Fatalf("%s: %v %q", v, err, out)
		}
	}
	if _, err := runAs(t, "dev", "update", "--check"); err == nil || err.Error() != i18n.T("update.not_release", "dev", "v0.2.0") {
		t.Fatalf("a build without a version: %v", err)
	}
	if len(g.installed) != 0 {
		t.Fatalf("installed %v", g.installed)
	}
}

func TestUpdateReinstallsTheService(t *testing.T) {
	env(t, nil)
	dir := noService(t)
	if err := os.WriteFile(filepath.Join(dir, launchd.Label+".plist"), launchd.Plist("/old/ccswap", "/h"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &github{latest: "v0.2.0"}
	bin := g.seam(t)
	out, err := runAs(t, "v0.1.0", "update")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := i18n.T("update.installing", "v0.1.0", "v0.2.0") + i18n.T("update.installed", bin); out != want {
		t.Fatalf("%q, want %q", out, want)
	}
	if len(g.installed) != 1 || g.installed[0] != "v0.2.0 "+bin || len(g.reinstalled) != 1 || g.reinstalled[0] != bin {
		t.Fatalf("installed %v, reinstalled %v", g.installed, g.reinstalled)
	}
	reinstall = func(*cobra.Command, string) error { return errors.New("launchctl bootstrap: 5") }
	if _, err := runAs(t, "v0.1.0", "update"); err == nil || !strings.Contains(err.Error(), "ccswap daemon install") {
		t.Fatalf("a failed reinstall: %v", err)
	}
}

func TestUpdateWithoutService(t *testing.T) {
	g := &github{latest: "v0.2.0"}
	g.seam(t)
	env(t, &fake{update: &api.Release{Version: "v0.2.0"}})
	noService(t)
	out, err := runAs(t, "v0.1.0", "update")
	if err != nil || !strings.HasSuffix(out, i18n.T("update.restart_daemon", "v0.2.0")) || len(g.reinstalled) != 0 {
		t.Fatalf("a daemon run by hand: %v %q %v", err, out, g.reinstalled)
	}
	if out, err := run(t, "", "status"); err != nil || !strings.Contains(out, i18n.T("status.update", "v0.2.0", "")) {
		t.Fatalf("status: %v %s", err, out)
	}
}
