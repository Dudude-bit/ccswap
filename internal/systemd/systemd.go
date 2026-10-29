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
	"slices"
	"strings"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
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

// service is what a unit keeps when this version rewrites it: the program, the language of its description,
// its log and its environment (KEY=value). edited marks a unit with settings ccswap never writes.
type service struct {
	bin, desc, log string
	env            []string
	edited         bool
}

// ours are the settings some version of ccswap writes in its unit.
var ours = []string{"Description", "ExecStart", "Restart", "RestartPreventExitStatus", "RestartSec", "KillSignal",
	"TimeoutStopSec", "StandardOutput", "StandardError", "Environment", "WantedBy"}

// Unit pins CCSWAP_HOME: the user manager's XDG_CONFIG_HOME may differ from the installing shell's.
func Unit(binPath, home string) []byte {
	s := service{bin: binPath, desc: i18n.T("systemd.unit_description"), log: filepath.Join(home, "daemon.log"), env: []string{"CCSWAP_HOME=" + home}}
	// the locale picks the language of the daemon's messages
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			s.env = append(s.env, k+"="+v)
		}
	}
	return s.unit()
}

func (s service) unit() []byte {
	var b bytes.Buffer
	log := "append:" + strings.ReplaceAll(s.log, "%", "%%")
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\nExecStart=%s daemon run\nRestart=on-failure\nRestartPreventExitStatus=%d %d\nRestartSec=5\nKillSignal=SIGTERM\nTimeoutStopSec=30\nStandardOutput=%s\nStandardError=%s\n",
		s.desc, quote(s.bin, true), alreadyRunning, exConfig, log, log)
	for _, e := range s.env {
		fmt.Fprintf(&b, "Environment=%s\n", quote(e, false))
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.Bytes()
}

// words are the systemd words of v, unquoted: a word may be quoted whole in " or '; % and $ stay doubled.
func words(v string) []string {
	var out []string
	for v = strings.TrimLeft(v, " \t"); v != ""; v = strings.TrimLeft(v, " \t") {
		q := v[0]
		if q != '"' && q != '\'' {
			i := strings.IndexAny(v, " \t")
			if i < 0 {
				i = len(v)
			}
			out, v = append(out, v[:i]), v[i:]
			continue
		}
		var w strings.Builder
		i := 1
		for ; i < len(v) && v[i] != q; i++ {
			if v[i] == '\\' && i+1 < len(v) {
				i++
			}
			w.WriteByte(v[i])
		}
		out, v = append(out, w.String()), v[min(i+1, len(v)):]
	}
	return out
}

// parse reads back the service a unit describes.
func parse(b []byte) service {
	var s service
	pct := strings.NewReplacer("%%", "%")
	for _, l := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(l, "=")
		switch k {
		case "Description":
			s.desc = v
		case "ExecStart":
			if w := words(v); len(w) > 0 {
				s.bin = strings.NewReplacer("%%", "%", "$$", "$").Replace(w[0])
			}
		case "StandardOutput":
			s.log = pct.Replace(strings.TrimPrefix(v, "append:"))
		case "Environment": // one line may set several
			for _, w := range words(v) {
				s.env = append(s.env, pct.Replace(w))
			}
		}
		switch t := strings.TrimSpace(l); {
		case t == "", t[0] == '#', t[0] == ';', t == "[Unit]", t == "[Service]", t == "[Install]", slices.Contains(ours, k):
		default:
			s.edited = true
		}
	}
	return s
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
	if s := parse(b); s.bin != "" {
		return s.bin, nil
	}
	return "", fmt.Errorf("%s: no ExecStart", p)
}

// Refresh rewrites the installed unit as this version writes it when it runs bin, keeping its program,
// description, log and environment, and reports whether it did. A unit edited beyond that stays as it is. Nothing is restarted: after daemon-reload
// the new unit takes effect at the service's next start.
func Refresh(bin string) (bool, error) {
	p, err := unitPath()
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := parse(b)
	want := s.unit()
	if s.bin != bin || s.edited || bytes.Equal(want, b) {
		return false, nil
	}
	if err := atomicfile.Write(p, want, 0o600); err != nil {
		return false, err
	}
	if _, err := ctl("daemon-reload"); err != nil {
		return true, err
	}
	return true, nil
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
