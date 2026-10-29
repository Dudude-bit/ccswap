package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlistGolden(t *testing.T) {
	for _, k := range []string{"CCSWAP_HOME", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(k, "")
	}
	got := string(Plist("/opt/a&b/ccswap", "/h/ome"))
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.github.dudude-bit.ccswap</string>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/a&amp;b/ccswap</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>/h/ome/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>/h/ome/daemon.log</string>
	<key>ExitTimeOut</key>
	<integer>30</integer>
</dict>
</plist>
`
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
	t.Setenv("CCSWAP_HOME", "x<y") // the plist carries the home it is given
	if g := string(Plist("/b", "/x<y")); !strings.Contains(g, "<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>CCSWAP_HOME</key>\n\t\t<string>/x&lt;y</string>") {
		t.Fatalf("env missing:\n%s", g)
	}
	t.Setenv("https_proxy", "http://p:1")
	if g := string(Plist("/b", "/x<y")); !strings.Contains(g, "<string>/x&lt;y</string>\n\t\t<key>https_proxy</key>\n\t\t<string>http://p:1</string>\n\t</dict>") {
		t.Fatalf("proxy missing:\n%s", g)
	}
}

func TestInstallRunsLaunchctl(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldRun := Dir, Run
	t.Cleanup(func() { Dir, Run = oldDir, oldRun })
	pollInterval, retryDelay = time.Millisecond, time.Millisecond
	Dir = dir
	var calls []string
	prints, bootstraps := 0, 0
	Run = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print": // loaded for the first 3 probes (before and while stopping), then gone
			prints++
			if prints > 3 {
				return nil, &exec.ExitError{}
			}
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return []byte("Input/output error"), fmt.Errorf("exit 5")
			}
		}
		return nil, nil
	}
	plist := filepath.Join(dir, Label+".plist")
	os.WriteFile(plist, []byte("old"), 0o644)
	if err := Install("/b/ccswap", "/h"); err != nil {
		t.Fatal(err)
	}
	uid := fmt.Sprint(os.Getuid())
	g := "gui/" + uid
	want := []string{"print " + g + "/" + Label, "bootout " + g + "/" + Label, "print " + g + "/" + Label, "print " + g + "/" + Label, "print " + g + "/" + Label,
		"bootstrap " + g + " " + plist, "bootstrap " + g + " " + plist}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q", calls)
	}
	if fi, err := os.Stat(plist); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("plist %v %v", fi, err)
	}
	calls = nil
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
	if err := Uninstall(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestProgramPath(t *testing.T) {
	oldDir := Dir
	t.Cleanup(func() { Dir = oldDir })
	Dir = t.TempDir()
	if _, err := ProgramPath(); err == nil {
		t.Fatal("no plist: want error")
	}
	os.WriteFile(filepath.Join(Dir, Label+".plist"), Plist("/opt/a&b/ccswap", "/h"), 0o644)
	if p, err := ProgramPath(); err != nil || p != "/opt/a&b/ccswap" {
		t.Fatal(p, err)
	}
}

// TestRefreshKeepsWhatThePlistHas: the daemon brings a LaunchAgent an older version wrote up to date without
// the shell that installed it: the program, log and environment stay, and launchctl is never run.
func TestRefreshKeepsWhatThePlistHas(t *testing.T) {
	oldDir, oldRun := Dir, Run
	t.Cleanup(func() { Dir, Run = oldDir, oldRun })
	Dir = t.TempDir()
	var calls []string
	Run = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	plist := filepath.Join(Dir, Label+".plist")
	if ok, err := Refresh("/b"); ok || err != nil {
		t.Fatal("no plist:", ok, err)
	}
	keys := []string{"CCSWAP_HOME", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "LC_ALL", "LC_MESSAGES", "LANG"}
	for _, k := range keys {
		t.Setenv(k, "")
	}
	t.Setenv("CCSWAP_HOME", "/h&o")
	t.Setenv("HTTPS_PROXY", "http://u:p@proxy:3128")
	t.Setenv("LANG", "ru_RU.UTF-8")
	cur := strings.Replace(string(Plist("/opt/a&b/ccswap", "/h&o")), "\t</dict>\n</dict>", "\t\t<key>SSL_CERT_FILE</key>\n\t\t<string>/c</string>\n\t</dict>\n</dict>", 1) // added by hand
	old := strings.Replace(cur, "\t<key>ExitTimeOut</key>\n\t<integer>30</integer>\n", "", 1)
	for _, k := range keys {
		t.Setenv(k, "") // the daemon's environment is not the installing shell's
	}
	os.WriteFile(plist, []byte(old), 0o600)
	if ok, err := Refresh("/usr/local/bin/ccswap"); ok || err != nil {
		t.Fatal("a plist of another binary:", ok, err)
	}
	if ok, err := Refresh("/opt/a&b/ccswap"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if b, _ := os.ReadFile(plist); string(b) != cur {
		t.Fatalf("got:\n%s\nwant:\n%s", b, cur)
	}
	if ok, err := Refresh("/opt/a&b/ccswap"); ok || err != nil {
		t.Fatal("up to date:", ok, err)
	}
	if len(calls) != 0 {
		t.Fatalf("launchctl %q", calls)
	}
}
