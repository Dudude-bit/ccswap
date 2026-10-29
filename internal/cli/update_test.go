package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/daemon"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/launchd"
	"github.com/Dudude-bit/ccswap/internal/update"
)

// github stands in for the newest release and records the asks and what ccswap update installs.
type github struct {
	latest    string
	asks      int
	installed []string
}

func (g *github) seam(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ccswap")
	oldLatest, oldInstall, oldTarget := latestRelease, installRelease, updateTarget
	t.Cleanup(func() { latestRelease, installRelease, updateTarget = oldLatest, oldInstall, oldTarget })
	latestRelease = func(context.Context, string) (update.Release, error) {
		g.asks++
		return update.Release{Version: g.latest, URL: "https://github.com/Dudude-bit/ccswap/releases/tag/" + g.latest}, nil
	}
	installRelease = func(_ context.Context, rel update.Release, to string) error {
		g.installed = append(g.installed, rel.Version+" "+to)
		return nil
	}
	updateTarget = func() (string, error) { return bin, nil }
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

// service is a LaunchAgent that runs bin, installed from a shell with a proxy, and loaded; the returned
// func is the file's content and every launchctl call since.
func service(t *testing.T, bin string) func() (string, []string) {
	t.Helper()
	setGOOS(t, "darwin")
	oldDir, oldRun := launchd.Dir, launchd.Run
	t.Cleanup(func() { launchd.Dir, launchd.Run = oldDir, oldRun })
	launchd.Dir = t.TempDir()
	var calls []string
	launchd.Run = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	plist := filepath.Join(launchd.Dir, launchd.Label+".plist")
	if err := os.WriteFile(plist, launchd.Plist(bin, "/h"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "") // ccswap update runs from a shell without it
	return func() (string, []string) {
		b, _ := os.ReadFile(plist)
		return string(b), calls
	}
}

func TestUpdateCheck(t *testing.T) {
	env(t, nil)
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
	g.asks = 0
	if _, err := runAs(t, "dev", "update"); err == nil || err.Error() != i18n.T("update.not_release_build", "dev") || g.asks != 0 {
		t.Fatalf("a build without a version asked GitHub %d times: %v", g.asks, err)
	}
	if len(g.installed) != 0 {
		t.Fatalf("installed %v", g.installed)
	}
}

// TestUpdateRestartsTheDaemonInPlace: ccswap update restarts a daemon that runs the binary it replaced as
// the page's Update does, and leaves the service, its proxy and the hook as they are.
func TestUpdateRestartsTheDaemonInPlace(t *testing.T) {
	g := &github{latest: "v0.2.0"}
	bin := g.seam(t)
	f := &fake{bin: bin, version: "v0.2.0"}
	env(t, f)
	installed := service(t, bin)
	plist, _ := installed()
	out, err := runAs(t, "v0.1.0", "update")
	want := i18n.T("update.installing", "v0.1.0", "v0.2.0") + i18n.T("update.installed", bin) +
		i18n.T("update.restarting", "v0.2.0") + i18n.T("update.daemon_runs", "v0.2.0")
	if err != nil || out != want {
		t.Fatalf("%v\n%q, want\n%q", err, out, want)
	}
	if len(g.installed) != 1 || g.installed[0] != "v0.2.0 "+bin || f.migrateCalls() != "restart "+bin {
		t.Fatalf("installed %v, daemon asked %q", g.installed, f.migrateCalls())
	}
	if now, calls := installed(); now != plist || len(calls) != 0 {
		t.Fatalf("the service was touched: launchctl %q\n%s", calls, now)
	}
}

func TestUpdateLeavesAnotherDaemon(t *testing.T) {
	g := &github{latest: "v0.2.0"}
	g.seam(t)
	f := &fake{bin: "/usr/local/bin/ccswap", version: "v0.1.0"}
	env(t, f)
	installed := service(t, "/usr/local/bin/ccswap")
	plist, _ := installed()
	out, err := runAs(t, "v0.1.0", "update")
	if err != nil || !strings.HasSuffix(out, i18n.T("update.other_daemon", "/usr/local/bin/ccswap")) {
		t.Fatalf("%v %q", err, out)
	}
	if now, calls := installed(); now != plist || len(calls) != 0 {
		t.Fatalf("the other binary's service was touched: launchctl %q\n%s", calls, now)
	}
}

func TestUpdateWithoutDaemon(t *testing.T) {
	g := &github{latest: "v0.2.0"}
	bin := g.seam(t)
	env(t, nil)
	installed := service(t, bin) // unloaded on purpose: an update must not load it
	plist, _ := installed()
	out, err := runAs(t, "v0.1.0", "update")
	if err != nil || !strings.HasSuffix(out, i18n.T("update.next_start", "v0.2.0")) {
		t.Fatalf("%v %q", err, out)
	}
	if now, calls := installed(); now != plist || len(calls) != 0 {
		t.Fatalf("the service was touched: launchctl %q\n%s", calls, now)
	}
}

func TestUpdateDaemonNotBack(t *testing.T) {
	old := restartWait
	t.Cleanup(func() { restartWait = old })
	restartWait = 50 * time.Millisecond
	g := &github{latest: "v0.2.0"}
	bin := g.seam(t)
	env(t, &fake{bin: bin, version: "v0.1.0"})
	if _, err := runAs(t, "v0.1.0", "update"); err == nil || err.Error() != i18n.T("update.daemon_lost", "v0.2.0") {
		t.Fatal(err)
	}
}

func TestUpdateWithAnOlderDaemon(t *testing.T) {
	g := &github{latest: "v0.2.0"}
	g.seam(t)
	home := env(t, nil)
	ln, err := net.Listen("unix", filepath.Join(home, "ccswap.sock")) // a daemon without POST /v1/restart
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, http.NotFoundHandler())
	t.Cleanup(func() { ln.Close() })
	out, err := runAs(t, "v0.1.0", "update")
	if err != nil || !strings.HasSuffix(out, i18n.T("update.restart_daemon", "v0.2.0")) {
		t.Fatalf("%v %q", err, out)
	}
}

func TestStatusShowsTheUpdate(t *testing.T) {
	env(t, &fake{update: &api.Release{Version: "v0.2.0"}})
	if out, err := run(t, "", "status"); err != nil || !strings.Contains(out, i18n.T("status.update", "v0.2.0", "")) {
		t.Fatalf("status: %v %s", err, out)
	}
}

// TestDaemonRunRefreshesItsService: a daemon brings the service that runs its binary up to date as it starts,
// without launchctl, even one that then finds another daemon running.
func TestDaemonRunRefreshesItsService(t *testing.T) {
	offTerminal(t, true)
	env(t, nil)
	bin, err := selfBin()
	if err != nil {
		t.Fatal(err)
	}
	installed := service(t, bin)
	cur, _ := installed()
	plist := filepath.Join(launchd.Dir, launchd.Label+".plist")
	os.WriteFile(plist, []byte(strings.Replace(cur, "\t<key>ExitTimeOut</key>\n\t<integer>30</integer>\n", "", 1)), 0o600)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := config.DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	lf, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var ee *ExitError
	if _, err := run(t, "", "daemon", "run"); !errors.As(err, &ee) || !errors.Is(ee.Err, daemon.ErrAlreadyRunning) {
		t.Fatal(err)
	}
	if now, calls := installed(); now != cur || len(calls) != 0 {
		t.Fatalf("launchctl %q\n%s", calls, now)
	}
}
