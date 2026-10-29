// Package launchd installs ccswap's per-user LaunchAgent.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
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

// job is what a LaunchAgent keeps when this version rewrites it: the program, its log and its environment.
// edited marks a plist with keys ccswap never writes.
type job struct {
	bin, log string
	env      [][2]string
	edited   bool
}

// ours are the keys some version of ccswap writes in its plist.
var ours = []string{"Label", "ProgramArguments", "RunAtLoad", "KeepAlive", "StandardOutPath", "StandardErrorPath",
	"ExitTimeOut", "ProcessType", "EnvironmentVariables"}

func Plist(binPath, home string) []byte {
	j := job{bin: binPath, log: filepath.Join(home, "daemon.log")}
	// launchd gives no shell env: keep the proxy claude reaches Anthropic through (net/http reads these),
	// the locale, which picks the language of notifications, and CCSWAP_CLAUDE_BIN and PATH (launchd's is
	// /usr/bin:/bin), where a login from the page finds claude and the node an npm-installed claude runs on
	if os.Getenv("CCSWAP_HOME") != "" { // Home is absolute (a relative CCSWAP_HOME is refused), as launchd runs the job from /
		j.env = append(j.env, [2]string{"CCSWAP_HOME", home})
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG", "CCSWAP_CLAUDE_BIN", "PATH"} {
		if v := os.Getenv(k); v != "" {
			j.env = append(j.env, [2]string{k, v})
		}
	}
	return j.plist()
}

func (j job) plist() []byte {
	var b bytes.Buffer
	s := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, esc(v)) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	s("Label", Label)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>daemon</string>\n\t\t<string>run</string>\n\t</array>\n", esc(j.bin))
	// restarted only after a failure: a daemon that finds another one running, or cannot start from its
	// files and folders, exits 0 under launchd, which cannot skip one exit status as systemd does, so it
	// does not try again every 10 s
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	s("StandardOutPath", j.log)
	s("StandardErrorPath", j.log)
	// the daemon flushes rotated tokens on shutdown; launchd's default 20 s could SIGKILL it
	b.WriteString("\t<key>ExitTimeOut</key>\n\t<integer>30</integer>\n")
	// no ProcessType Background: on a loaded Mac it starves the daemon for minutes (tokens expire, CLI times out)
	if len(j.env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, kv := range j.env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", esc(kv[0]), esc(kv[1]))
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// parse reads back the job a plist describes.
func parse(b []byte) (job, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false // the DOCTYPE
	var j job
	var open []string      // the elements open, from <plist> in
	var key, envKey string // the last key of the top dict, and of EnvironmentVariables
	for {
		t, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return j, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			open = append(open, t.Name.Local)
		case xml.EndElement:
			open = open[:max(len(open)-1, 0)]
		case xml.CharData:
			switch v := string(t); strings.Join(open, "/") {
			case "plist/dict/key":
				key = v
				j.edited = j.edited || !slices.Contains(ours, v)
			case "plist/dict/string":
				if key == "StandardOutPath" {
					j.log = v
				}
			case "plist/dict/array/string":
				if key == "ProgramArguments" && j.bin == "" {
					j.bin = v
				}
			case "plist/dict/dict/key":
				envKey = v
			case "plist/dict/dict/string":
				if key == "EnvironmentVariables" {
					j.env = append(j.env, [2]string{envKey, v})
				}
			}
		}
	}
	if j.bin == "" {
		return j, errors.New("no ProgramArguments")
	}
	return j, nil
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
	j, err := parse(b)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return j.bin, nil
}

// Refresh rewrites the installed LaunchAgent as this version writes it when it runs bin, keeping its
// program, log and environment, and reports whether it did; a plist edited beyond that stays as it is.
// Nothing is loaded or stopped: launchd reads the file when the job is next bootstrapped, at a login or
// by ccswap daemon install.
func Refresh(bin string) (bool, error) {
	p, err := plistPath()
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
	j, err := parse(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", p, err)
	}
	want := j.plist()
	if j.bin != bin || j.edited || bytes.Equal(want, b) {
		return false, nil
	}
	if err := atomicfile.Write(p, want, 0o600); err != nil {
		return false, err
	}
	return true, nil
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
