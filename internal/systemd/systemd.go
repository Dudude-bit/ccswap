// Package systemd installs ccswap's user service for Linux hosts.
package systemd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

const Label = "ccswap.service"

// Dir is the user unit directory ("" = $XDG_CONFIG_HOME/systemd/user or ~/.config/systemd/user).
// Run runs systemctl/loginctl; RuntimeDir is the /run/user/<uid> seam; User is the linger user.
var (
	Dir        = ""
	RuntimeDir = func() string { return fmt.Sprintf("/run/user/%d", os.Getuid()) }
	User       = func() string {
		if u, err := user.Current(); err == nil {
			return u.Username
		}
		return os.Getenv("USER")
	}
	Run = func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...)
		cmd.Env = runEnv()
		return cmd.CombinedOutput()
	}
)

// ErrNoSystemd: systemctl is missing, or the host was not booted with systemd.
var ErrNoSystemd error = i18n.Err("systemd.not_found")

var errNoBus error = i18n.Err("systemd.no_user_session")

// noSystemd: Run's failure means there is no systemd user manager to talk to.
func noSystemd(out []byte, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	for _, m := range []string{"Failed to connect to bus", "Failed to connect to user scope bus", "Failed to get D-Bus connection", "System has not been booted with systemd", "No medium found"} {
		if strings.Contains(string(out), m) {
			return true
		}
	}
	return false
}

// LingerError: the service is installed, only `loginctl enable-linger` failed.
type LingerError struct{ User string }

func (e *LingerError) Error() string {
	return i18n.T("systemd.linger_failed", e.User)
}

// runEnv adds XDG_RUNTIME_DIR (absent in some SSH sessions) when the dir exists.
func runEnv() []string {
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		if d := RuntimeDir(); d != "" {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				env = append(env, "XDG_RUNTIME_DIR="+d)
			}
		}
	}
	return env
}

// quote makes s one systemd word: quoted when it holds spaces or quotes; % and $ doubled for specifiers.
func quote(s string, dollar bool) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if dollar {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// daemon.ExitAlreadyRunning and daemon.ExitConfig: another daemon runs, or its files or folders are
// unusable, so a restart would only loop.
const alreadyRunning, exConfig = 3, 78

// Unit pins CCSWAP_HOME: the user manager's XDG_CONFIG_HOME may differ from the installing shell's.
func Unit(binPath, home string) []byte {
	var b bytes.Buffer
	log := "append:" + strings.ReplaceAll(filepath.Join(home, "daemon.log"), "%", "%%")
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\nExecStart=%s daemon run\nRestart=on-failure\nRestartPreventExitStatus=%d %d\nRestartSec=5\nKillSignal=SIGTERM\nTimeoutStopSec=30\nStandardOutput=%s\nStandardError=%s\nEnvironment=%s\n",
		i18n.T("systemd.unit_description"), quote(binPath, true), alreadyRunning, exConfig, log, log, quote("CCSWAP_HOME="+home, false))
	// the locale picks the language of the daemon's messages
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "Environment=%s\n", quote(k+"="+v, false))
		}
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.Bytes()
}

func unitPath() (string, error) {
	d := Dir
	if d == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			d = filepath.Join(x, "systemd", "user")
		} else {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			d = filepath.Join(h, ".config", "systemd", "user")
		}
	}
	return filepath.Join(d, Label), nil
}

// ProgramPath is the binary the installed unit runs (first word of ExecStart=).
func ProgramPath() (string, error) {
	p, err := unitPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(l, "ExecStart=")
		if !ok {
			continue
		}
		var w strings.Builder
		if strings.HasPrefix(v, `"`) {
			for i := 1; i < len(v) && v[i] != '"'; i++ {
				if v[i] == '\\' && i+1 < len(v) {
					i++
				}
				w.WriteByte(v[i])
			}
		} else {
			w.WriteString(strings.Fields(v + " ")[0])
		}
		return strings.NewReplacer("%%", "%", "$$", "$").Replace(w.String()), nil
	}
	return "", fmt.Errorf("%s: no ExecStart", p)
}

// ctl runs systemctl --user; a missing user bus gets an actionable message.
func ctl(args ...string) ([]byte, error) {
	out, err := Run("systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		if noSystemd(out, err) {
			return out, errNoBus
		}
		return out, fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// Install returns *LingerError when only enabling linger failed.
func Install(binPath, home string) error {
	p, err := unitPath()
	if err != nil {
		return err
	}
	// no unit file left behind where systemd cannot run it; a degraded manager still answers
	if _, err := ctl("is-system-running"); errors.Is(err, errNoBus) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// 0600: Environment= may carry proxy credentials
	if err := os.WriteFile(p, Unit(binPath, home), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o600); err != nil { // an older install wrote 0644
		return err
	}
	// restart is SIGTERM, so the daemon can flush; never kill
	for _, a := range [][]string{{"daemon-reload"}, {"enable", Label}, {"restart", Label}} {
		if _, err := ctl(a...); err != nil {
			return err
		}
	}
	u := User()
	if _, err := Run("loginctl", "enable-linger", u); err != nil {
		return &LingerError{u}
	}
	return nil
}

func Uninstall() error {
	p, err := unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := ctl("disable", "--now", Label); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err = ctl("daemon-reload")
	return err
}

// Loaded reports whether the unit is active; ErrNoSystemd when the host has no systemd.
func Loaded() (bool, error) {
	out, err := Run("systemctl", "--user", "is-active", Label) // non-zero exit = not active
	if errors.Is(err, exec.ErrNotFound) || (err != nil && strings.Contains(string(out), "System has not been booted with systemd")) {
		return false, ErrNoSystemd
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

func Linger() (bool, error) {
	out, err := Run("loginctl", "show-user", User(), "-p", "Linger")
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "Linger=yes"), nil
}
