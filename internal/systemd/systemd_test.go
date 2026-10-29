package systemd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

var envKeys = []string{"CCSWAP_HOME", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG", "CCSWAP_CLAUDE_BIN", "PATH"}

func seams(t *testing.T) *[]string {
	t.Helper()
	oldDir, oldRun, oldUser := Dir, Run, User
	t.Cleanup(func() { Dir, Run, User = oldDir, oldRun, oldUser })
	Dir = t.TempDir()
	User = func() string { return "bob" }
	var calls []string
	Run = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	return &calls
}

func TestUnitGolden(t *testing.T) {
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	want := "[Unit]\nDescription=" + i18n.T("systemd.unit_description") + `

[Service]
ExecStart="/opt/a b/ccswap" daemon run
Restart=on-failure
RestartPreventExitStatus=3 78
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30
StandardOutput=append:/h/daemon.log
StandardError=append:/h/daemon.log
Environment=CCSWAP_HOME=/h

[Install]
WantedBy=default.target
`
	if got := string(Unit("/opt/a b/ccswap", "/h")); got != want {
		t.Fatalf("got:\n%s", got)
	}
	t.Setenv("CCSWAP_HOME", "/x y") // the shell's value is not what the daemon uses: home is
	t.Setenv("https_proxy", "http://p:1")
	t.Setenv("LANG", "ru_RU.UTF-8")
	g := string(Unit("/b", "/h 50%"))
	if !strings.Contains(g, "Environment=\"CCSWAP_HOME=/h 50%%\"\nEnvironment=https_proxy=http://p:1\nEnvironment=LANG=ru_RU.UTF-8\n\n[Install]") || strings.Contains(g, "/x y") {
		t.Fatalf("env:\n%s", g)
	}
	t.Setenv("PATH", "/n/bin:/usr/bin") // the claude a login from the page runs, and its node
	if g := string(Unit("/b", "/h")); !strings.Contains(g, "Environment=PATH=/n/bin:/usr/bin\n\n[Install]") {
		t.Fatalf("PATH:\n%s", g)
	}
}

func TestInstallRunsSystemctlInOrder(t *testing.T) {
	calls := seams(t)
	unit := filepath.Join(Dir, Label)
	os.WriteFile(unit, []byte("old"), 0o644)
	if err := Install("/b/ccswap", "/h"); err != nil {
		t.Fatal(err)
	}
	want := "systemctl --user is-system-running|systemctl --user daemon-reload|systemctl --user enable ccswap.service|systemctl --user restart ccswap.service|loginctl enable-linger bob"
	if strings.Join(*calls, "|") != want {
		t.Fatalf("calls %q", *calls)
	}
	if fi, err := os.Stat(unit); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("unit %v %v", fi, err)
	}
}

func TestInstallFillsRuntimeDir(t *testing.T) {
	old := RuntimeDir
	t.Cleanup(func() { RuntimeDir = old })
	t.Setenv("XDG_RUNTIME_DIR", "")
	d := t.TempDir()
	RuntimeDir = func() string { return d }
	has := func() bool {
		for _, e := range runEnv() {
			if e == "XDG_RUNTIME_DIR="+d {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("existing dir not added")
	}
	RuntimeDir = func() string { return filepath.Join(d, "nope") }
	for _, e := range runEnv() {
		if strings.HasPrefix(e, "XDG_RUNTIME_DIR=") && e != "XDG_RUNTIME_DIR=" {
			t.Fatal("missing dir added")
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", "/keep")
	RuntimeDir = func() string { return d }
	if has() {
		t.Fatal("overrode a set XDG_RUNTIME_DIR")
	}
}

func TestInstallLingerFailureIsReported(t *testing.T) {
	seams(t)
	Run = func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("denied"), fmt.Errorf("exit 1")
		}
		return nil, nil
	}
	err := Install("/b", "/h")
	var le *LingerError
	if !errors.As(err, &le) || !strings.Contains(err.Error(), "sudo loginctl enable-linger bob") {
		t.Fatal(err)
	}
}

func TestNoUserBusMessage(t *testing.T) {
	for _, c := range []struct {
		out string
		err error
	}{
		{"Failed to connect to bus: No medium found", fmt.Errorf("exit 1")},
		{"Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined", fmt.Errorf("exit 1")},
		{"Failed to get D-Bus connection: Operation not permitted", fmt.Errorf("exit 1")},
		{"System has not been booted with systemd as init system (PID 1). Can't operate.", fmt.Errorf("exit 1")},
		{"", &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}},
	} {
		calls := seams(t)
		Run = func(name string, args ...string) ([]byte, error) {
			*calls = append(*calls, name)
			return []byte(c.out), c.err
		}
		err := Install("/b", "/h")
		if err == nil || err.Error() != i18n.T("systemd.no_user_session") || len(*calls) != 1 {
			t.Fatalf("%q: %v %q", c.out, err, *calls)
		}
		if _, err := os.Stat(filepath.Join(Dir, Label)); !os.IsNotExist(err) {
			t.Fatalf("%q: unit written without systemd", c.out)
		}
	}
	seams(t)
	Run = func(string, ...string) ([]byte, error) { return []byte("degraded"), fmt.Errorf("exit 1") }
	if err := Install("/b", "/h"); err == nil || !strings.Contains(err.Error(), "systemctl --user daemon-reload") {
		t.Fatalf("a degraded manager is a systemd: %v", err)
	}
}

func TestUninstallMissingUnitOK(t *testing.T) {
	calls := seams(t)
	if err := Uninstall(); err != nil || len(*calls) != 0 {
		t.Fatal(err, *calls)
	}
	if err := Install("/b", "/h"); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*calls, "|") != "systemctl --user disable --now ccswap.service|systemctl --user daemon-reload" {
		t.Fatalf("calls %q", *calls)
	}
	if _, err := os.Stat(filepath.Join(Dir, Label)); !os.IsNotExist(err) {
		t.Fatal("unit not removed")
	}
}

func TestProgramPathParsesExecStart(t *testing.T) {
	seams(t)
	if _, err := ProgramPath(); err == nil {
		t.Fatal("no unit: want error")
	}
	for _, bin := range []string{"/opt/a b/ccswap", "/opt/ccswap", `/o"p/50%/ccswap`} {
		os.WriteFile(filepath.Join(Dir, Label), Unit(bin, "/h"), 0o644)
		if p, err := ProgramPath(); err != nil || p != bin {
			t.Fatalf("%q: %q %v", bin, p, err)
		}
	}
}

func TestLoadedAndLinger(t *testing.T) {
	seams(t)
	out := ""
	Run = func(string, ...string) ([]byte, error) { return []byte(out), nil }
	for in, want := range map[string]bool{"active\n": true, "inactive\n": false} {
		out = in
		if on, _ := Loaded(); on != want {
			t.Fatalf("%q: %v", in, on)
		}
	}
	for _, e := range []struct {
		out string
		err error
	}{{"", &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}}, {"System has not been booted with systemd as init system (PID 1). Can't operate.", fmt.Errorf("exit 1")}} {
		Run = func(string, ...string) ([]byte, error) { return []byte(e.out), e.err }
		if on, err := Loaded(); on || !errors.Is(err, ErrNoSystemd) {
			t.Fatalf("%q: %v %v", e.out, on, err)
		}
	}
	Run = func(string, ...string) ([]byte, error) { return []byte(out), nil }
	for in, want := range map[string]bool{"Linger=yes\n": true, "Linger=no\n": false} {
		out = in
		if on, err := Linger(); err != nil || on != want {
			t.Fatalf("%q: %v %v", in, on, err)
		}
	}
}

// 6eb169c (the first Linux build) wrote os.UserConfigDir()/systemd/user/ccswap.service: install and
// uninstall must handle that same file, never leave a second unit running.
func TestUnitPathOfEarlierLinuxBuild(t *testing.T) {
	old := Dir
	t.Cleanup(func() { Dir = old })
	Dir = ""
	x, h := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", x)
	if p, err := unitPath(); err != nil || p != filepath.Join(x, "systemd", "user", "ccswap.service") {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", h)
	if p, err := unitPath(); err != nil || p != filepath.Join(h, ".config", "systemd", "user", "ccswap.service") {
		t.Fatal(p, err)
	}
}
