// Package launchd installs ccswap's per-user LaunchAgent.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

const Label = "com.github.dudude-bit.ccswap"

// timing knobs, variables so tests can shrink them
var (
	pollInterval  = 250 * time.Millisecond
	unloadTimeout = 35 * time.Second
	retryDelay    = 500 * time.Millisecond
)

const bootstrapTries = 3

// Dir is the LaunchAgents directory ("" = ~/Library/LaunchAgents); Run is the launchctl seam.
var (
	Dir = ""
	Run = func(args ...string) ([]byte, error) {
		return exec.Command("/bin/launchctl", args...).CombinedOutput()
	}
)

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func Plist(binPath, home string) []byte {
	var b bytes.Buffer
	s := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, esc(v)) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	s("Label", Label)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>daemon</string>\n\t\t<string>run</string>\n\t</array>\n", esc(binPath))
	// restarted only after a failure: a daemon that finds another one running, or cannot start from its
	// files and folders, exits 0 under launchd, which cannot skip one exit status as systemd does, so it
	// does not try again every 10 s
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	log := filepath.Join(home, "daemon.log")
	s("StandardOutPath", log)
	s("StandardErrorPath", log)
	// the daemon flushes rotated tokens on shutdown; launchd's default 20 s could SIGKILL it
	b.WriteString("\t<key>ExitTimeOut</key>\n\t<integer>30</integer>\n")
	// no ProcessType Background: on a loaded Mac it starves the daemon for minutes (tokens expire, CLI times out)
	// launchd gives no shell env: keep the proxy claude reaches Anthropic through (net/http reads these),
	// the locale, which picks the language of notifications, and CCSWAP_CLAUDE_BIN and PATH (launchd's is
	// /usr/bin:/bin), where a login from the page finds claude and the node an npm-installed claude runs on
	var env bytes.Buffer
	if os.Getenv("CCSWAP_HOME") != "" { // Home is absolute (a relative CCSWAP_HOME is refused), as launchd runs the job from /
		fmt.Fprintf(&env, "\t\t<key>CCSWAP_HOME</key>\n\t\t<string>%s</string>\n", esc(home))
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG", "CCSWAP_CLAUDE_BIN", "PATH"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&env, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", k, esc(v))
		}
	}
	if env.Len() > 0 {
		fmt.Fprintf(&b, "\t<key>EnvironmentVariables</key>\n\t<dict>\n%s\t</dict>\n", env.Bytes())
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func plistPath() (string, error) {
	d := Dir
	if d == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(h, "Library", "LaunchAgents")
	}
	return filepath.Join(d, Label+".plist"), nil
}

// ProgramPath is the binary the installed LaunchAgent runs (ProgramArguments[0]).
func ProgramPath() (string, error) {
	p, err := plistPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false // the DOCTYPE
	var key, last string
	for {
		t, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("%s: no ProgramArguments", p)
		}
		switch t := t.(type) {
		case xml.StartElement:
			last = t.Name.Local
		case xml.CharData:
			switch {
			case last == "key":
				key = string(t)
			case last == "string" && key == "ProgramArguments":
				return string(t), nil
			}
		case xml.EndElement:
			last = ""
		}
	}
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func Install(binPath, home string) error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// 0600: EnvironmentVariables may carry proxy credentials; launchd refuses only group/world-writable plists
	if err := os.WriteFile(p, Plist(binPath, home), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o600); err != nil { // an older install wrote 0644
		return err
	}
	if on, _ := Loaded(); on {
		_, _ = Run("bootout", domain()+"/"+Label)
		if err := waitUnloaded(); err != nil {
			return err
		}
	}
	// no kickstart -k: RunAtLoad already starts it, and -k would SIGKILL that instance
	var out []byte
	for i := range bootstrapTries {
		if i > 0 {
			time.Sleep(retryDelay)
		}
		if out, err = Run("bootstrap", domain(), p); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
}

// waitUnloaded polls until launchd forgets the job (ExitTimeOut is 30 s, so the daemon can flush).
func waitUnloaded() error {
	deadline := time.Now().Add(unloadTimeout)
	for time.Now().Before(deadline) {
		if on, _ := Loaded(); !on {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return i18n.Err("launchd.stop_timeout")
}

func Uninstall() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if out, err := Run("bootout", domain()+"/"+Label); err != nil {
		if on, _ := Loaded(); on {
			return fmt.Errorf("launchctl bootout: %w: %s", err, out)
		}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Loaded reports whether launchd knows the job; a non-zero exit means not loaded.
func Loaded() (bool, error) {
	_, err := Run("print", domain()+"/"+Label)
	var ee *exec.ExitError
	if err == nil {
		return true, nil
	}
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, err
}
