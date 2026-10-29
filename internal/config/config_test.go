package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PollInterval: 60 * time.Second, Thresholds: Thresholds{95, 98, 10, 10 * time.Minute, 5 * time.Minute, 30 * time.Minute}, T3: T3{true, "127.0.0.1:8318"}, Update: Update{true}}
	if c != want {
		t.Fatalf("got %+v", c)
	}
}

func write(t *testing.T, s string) string {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "config.toml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLoadPartialFile(t *testing.T) {
	c, err := Load(write(t, "[thresholds]\nfive_hour = 90\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Thresholds.FiveHour != 90 || c.Thresholds.Weekly != 98 || c.Thresholds.MinDwell != 10*time.Minute || c.PollInterval != time.Minute {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadFull(t *testing.T) {
	c, err := Load(write(t, "poll_interval=\"30s\"\n[thresholds]\nmin_dwell=\"1m\"\n[t3]\nenabled=false\nlisten=\"127.0.0.1:1\"\n[claude]\nconfig_dir=\"/x\"\n[update]\ncheck=false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 30*time.Second || c.Thresholds.MinDwell != time.Minute || c.T3.Enabled || c.T3.Listen != "127.0.0.1:1" || c.Claude.ConfigDir != "/x" || c.Update.Check {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	for key, body := range map[string]string{
		"poll_interval":                          "poll_interval=\"soon\"\n",
		"min_dwell":                              "[thresholds]\nmin_dwell=\"x\"\n",
		"bogus":                                  "bogus = 1\n",
		"five_hour":                              "[thresholds]\nfive_hour = 0\n",
		"weekly":                                 "[thresholds]\nweekly = 101\n",
		"hysteresis":                             "[thresholds]\nhysteresis = 95\n",
		"poll_interval ":                         "poll_interval=\"5s\"\n",
		i18n.T("config.config_dir_not_absolute"): "[claude]\nconfig_dir=\"rel/claude\"\n",
		i18n.T("config.config_dir_not_absolute") + " ": "[claude]\nconfig_dir=\"~/.claude\"\n",
	} {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(key)) {
			t.Errorf("%s: err=%v", key, err)
		}
	}
}

func TestHomeFromEnv(t *testing.T) {
	d := filepath.Join(t.TempDir(), "h")
	t.Setenv("CCSWAP_HOME", d)
	h, err := Home()
	if err != nil || h != d {
		t.Fatal(h, err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	os.Chmod(d, 0o755)
	if _, err := Home(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Fatalf("not tightened: %v", fi.Mode())
	}
}

func TestLoadRejectsNaN(t *testing.T) {
	for _, body := range []string{"[thresholds]\nfive_hour = nan\n", "[thresholds]\nweekly = nan\n", "[thresholds]\nhysteresis = nan\n"} {
		h := t.TempDir()
		os.WriteFile(filepath.Join(h, "config.toml"), []byte(body), 0o600)
		if _, err := Load(h); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
}

func TestSaveThresholdsKeepsOtherKeys(t *testing.T) {
	h := write(t, "poll_interval = \"30s\"\n[thresholds]\nfive_hour = 90\n[t3]\nlisten = \"0.0.0.0:9\"\n")
	th := Thresholds{80, 97.5, 5, 90 * time.Second, 2 * time.Minute, time.Hour}
	if err := SaveThresholds(h, th); err != nil {
		t.Fatal(err)
	}
	c, err := Load(h)
	if err != nil {
		t.Fatal(err)
	}
	if c.Thresholds != th || c.PollInterval != 30*time.Second || c.T3.Listen != "0.0.0.0:9" || !c.T3.Enabled {
		t.Fatalf("got %+v", c)
	}
}

func TestSaveThresholdsCreatesMissingFile(t *testing.T) {
	h := t.TempDir()
	th := Thresholds{90, 95, 10, 0, 5 * time.Minute, 30 * time.Minute}
	if err := SaveThresholds(h, th); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(h); err != nil || c.Thresholds != th || c.PollInterval != time.Minute {
		t.Fatal(c, err)
	}
}

func TestSaveThresholdsLeavesBrokenFile(t *testing.T) {
	h := write(t, "five_hour = = 1\n")
	if err := SaveThresholds(h, Thresholds{90, 95, 10, 0, 5 * time.Minute, 30 * time.Minute}); err == nil {
		t.Fatal("broken config.toml overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(h, "config.toml")); string(b) != "five_hour = = 1\n" {
		t.Fatalf("%s", b)
	}
}

func TestSaveThresholdsKeepsSymlink(t *testing.T) {
	real := filepath.Join(write(t, ""), "config.toml")
	h := t.TempDir()
	link := filepath.Join(h, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveThresholds(h, Thresholds{90, 95, 10, 0, 5 * time.Minute, 30 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "five_hour = 90") {
		t.Fatalf("%s", b)
	}
}

func TestRemote(t *testing.T) {
	h := write(t, "poll_interval = \"30s\"\n")
	if err := SaveRemote(h, "https://ccswap.example.com"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(h)
	if err != nil || c.Remote.URL != "https://ccswap.example.com" || c.PollInterval != 30*time.Second {
		t.Fatal(c, err)
	}
	if err := SaveRemote(h, ""); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(h); err != nil || c.Remote.URL != "" {
		t.Fatal(c, err)
	}
	for u, ok := range map[string]bool{
		"https://ccswap.example.com":       true,
		"http://127.0.0.1:8318":            true,
		"http://localhost:8318":            true,
		"http://100.100.1.2:8318":          true,
		"http://172.16.0.10:8318":          true,
		"http://203.0.113.7:8318":          false,
		"http://ccswap.example.com":        false,
		"ftp://ccswap.example.com":         false,
		"ccswap.example.com":               false,
		"https://me:pw@ccswap.example.com": false,
		"http://me@127.0.0.1:8318":         false,
	} {
		if err := CheckRemote(u); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := Load(write(t, "[remote]\nurl = \"http://203.0.113.7:8318\"\n")); err == nil {
		t.Fatal("a plain http public remote must be refused")
	}
}

func TestRelativeHomeIsRefused(t *testing.T) { // the service and a CLI run elsewhere would find other folders
	t.Chdir(t.TempDir())
	t.Setenv("CCSWAP_HOME", "rel")
	if h, err := Home(); err == nil || !strings.Contains(err.Error(), "CCSWAP_HOME") {
		t.Fatal(h, err)
	}
	if _, err := os.Stat("rel"); !os.IsNotExist(err) {
		t.Fatal("created a folder for a refused CCSWAP_HOME:", err)
	}
}

func TestSocketPathFitsASocketAddress(t *testing.T) {
	if s, err := SocketPath("/h"); err != nil || s != "/h/ccswap.sock" {
		t.Fatal(s, err)
	}
	if _, err := SocketPath("/" + strings.Repeat("h", 120)); err == nil || !strings.Contains(err.Error(), "CCSWAP_HOME") {
		t.Fatal("a socket path past sun_path must be refused in plain words:", err)
	}
}

func TestHomeDefaultsToUserConfigDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CCSWAP_HOME", "")
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	want := filepath.Join(tmp, ".config", "ccswap")
	switch runtime.GOOS {
	case "darwin":
		want = filepath.Join(tmp, "Library", "Application Support", "ccswap")
	case "linux":
		want = filepath.Join(tmp, "xdg", "ccswap")
	}
	h, err := Home()
	if err != nil || h != want {
		t.Fatal(h, err, want)
	}
	if fi, _ := os.Stat(h); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestCheckIntervals(t *testing.T) {
	c, err := Load(write(t, "[thresholds]\ncheck_active = \"2m\"\ncheck_others = \"1h\"\n"))
	if err != nil || c.Thresholds.CheckActive != 2*time.Minute || c.Thresholds.CheckOthers != time.Hour {
		t.Fatal(c.Thresholds, err)
	}
	for _, body := range []string{"check_active = \"30s\"", "check_others = \"25h\"", "check_active = \"soon\""} {
		if _, err := Load(write(t, "[thresholds]\n"+body+"\n")); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
}

func TestUILang(t *testing.T) {
	h := write(t, "poll_interval = \"30s\"\n")
	if c, err := Load(h); err != nil || c.UI.Lang != "" {
		t.Fatal(c, err)
	}
	if err := SaveLang(h, "ru"); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(h); err != nil || c.UI.Lang != "ru" || c.PollInterval != 30*time.Second {
		t.Fatal(c, err)
	}
	if err := SaveLang(h, ""); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(h); err != nil || c.UI.Lang != "" {
		t.Fatal(c, err)
	}
	if _, err := Load(write(t, "[ui]\nlang = \"qaa\"\n")); err == nil {
		t.Fatal("an unknown language must be refused")
	}
}
