package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setGOOS(t *testing.T, v string) {
	old := GOOS
	GOOS = v
	t.Cleanup(func() { GOOS = old })
}

func TestServiceNameLinuxIsCredentialsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setGOOS(t, "linux")
	if got, want := ServiceName(""), filepath.Join(home, ".claude", ".credentials.json"); got != want {
		t.Fatal(got)
	}
	if got := ServiceName("/x/cfg"); got != "/x/cfg/.credentials.json" {
		t.Fatal(got)
	}
	setGOOS(t, "darwin")
	if ServiceName("") != "Claude Code-credentials" || ServiceName("/x/cfg") == "/x/cfg/.credentials.json" {
		t.Fatal("darwin changed")
	}
}

func TestConfigDirIsNFCLikeServiceName(t *testing.T) {
	setGOOS(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	nfd, nfc := "/x/cafe\u0301", "/x/caf\u00e9"
	if ConfigDir(home, nfd) != nfc || filepath.Dir(ServiceName(nfd)) != ConfigDir(home, nfd) {
		t.Fatalf("%q %q", ConfigDir(home, nfd), ServiceName(nfd))
	}
	if ConfigDir(home, "") != filepath.Join(home, ".claude") {
		t.Fatal(ConfigDir(home, ""))
	}
}

func TestLoginServicesRefuseLiveLogin(t *testing.T) {
	setGOOS(t, "linux")
	home := t.TempDir() // on macOS a symlinked path (/var -> /private/var): only the same-dir check sees it
	t.Setenv("HOME", home)
	live := filepath.Join(home, ".claude")
	os.Mkdir(live, 0o700)
	target := ServiceName("")
	link := filepath.Join(home, "login-link")
	os.Symlink(live, link)
	plain := filepath.Join(home, "login-plain")
	os.Mkdir(plain, 0o700)
	for dir, want := range map[string]bool{link: false, live: false, plain: true} {
		if svcs, ok := LoginServices(dir, target); ok != want || svcs[0] != filepath.Join(dir, ".credentials.json") {
			t.Fatalf("%s: %v %v", dir, svcs, ok)
		}
	}
	setGOOS(t, "darwin")
	real, _ := filepath.EvalSymlinks(live)
	if _, ok := LoginServices(link, ServiceName(real)); ok {
		t.Fatal("darwin: the resolved path's hash is the live entry")
	}
	if _, ok := LoginServices(plain, ServiceName(real)); !ok {
		t.Fatal("darwin: plain dir refused")
	}
}

func TestServiceName(t *testing.T) {
	setGOOS(t, "darwin")
	if ServiceName("") != "Claude Code-credentials" {
		t.Fatal("default")
	}
	if got := ServiceName("/Users/someone/.claude"); got != "Claude Code-credentials-b38b2c3b" {
		t.Fatal(got)
	}
}

func TestCredentialsRoundTripKeepsUnknownOAuthKeys(t *testing.T) {
	in := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1791600000000,"scopes":["user:inference"],"futureField":7},"mcpOAuth":{"x":1}}`
	c, err := ParseCredentials([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.OAuth.AccessToken != "a" || c.OAuth.RefreshToken != "r" {
		t.Fatal("fields")
	}
	out, err := c.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(in), &a)
	if err := json.Unmarshal(out, &b); err != nil || len(b) != 1 || !reflect.DeepEqual(a["claudeAiOauth"], b["claudeAiOauth"]) {
		t.Fatalf("vault form must hold claudeAiOauth only, with all its keys: %s", out)
	}
	if !c.Expiry().Equal(time.UnixMilli(1791600000000)) {
		t.Fatal("expiry")
	}
	c.OAuth.AccessToken = "new"
	out, _ = c.Bytes()
	c2, _ := ParseCredentials(out)
	if c2.OAuth.AccessToken != "new" {
		t.Fatal("edit lost")
	}
}

func TestWithOAuthKeepsOtherKeysByteFaithful(t *testing.T) {
	entry := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"},"mcpOAuth":{"srv|1":{"accessToken":"m&<>","serverUrl":"https://x/?a=1&b=2"}},"zz":[1, 2.50],"claudeAiOauthX":{}}`
	c, _ := ParseCredentials([]byte(`{"claudeAiOauth":{"accessToken":"b","refreshToken":"rb","futureField":7},"mcpOAuth":{"stale":1}}`))
	out, err := WithOAuth([]byte(entry), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{`"mcpOAuth":{"srv|1":{"accessToken":"m&<>","serverUrl":"https://x/?a=1&b=2"}}`, `"zz":[1, 2.50]`, `"claudeAiOauthX":{}`} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("%s not kept byte for byte: %s", keep, out)
		}
	}
	got, err := ParseCredentials(out)
	if err != nil || got.OAuth.AccessToken != "b" || got.OAuth.RefreshToken != "rb" || strings.Contains(string(out), "stale") ||
		!strings.Contains(string(out), `"futureField":7`) {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := WithOAuth(nil, c); err != nil || string(out) != `{"claudeAiOauth":{"accessToken":"b","futureField":7,"refreshToken":"rb"}}` {
		t.Fatalf("empty entry: %v %s", err, out)
	}
	for _, bad := range []string{`[]`, `{"claudeAiOauth":{}`} {
		if _, err := WithOAuth([]byte(bad), c); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestParseCredentialsErrors(t *testing.T) {
	for _, in := range []string{`nope`, `{}`, `{"claudeAiOauth":{}}`} {
		if _, err := ParseCredentials([]byte(in)); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestKeychainAccountSanitizes(t *testing.T) {
	t.Setenv("USER", "a b")
	if got := KeychainAccount(); got != "claude-code-user" {
		t.Fatal(got)
	}
	t.Setenv("USER", "someone")
	if got := KeychainAccount(); got != "someone" {
		t.Fatal(got)
	}
}

func TestGlobalConfigPath(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	if got := GlobalConfigPath(home, ""); got != filepath.Join(home, ".claude.json") {
		t.Fatal(got)
	}
	if got := GlobalConfigPath(home, cfg); got != filepath.Join(cfg, ".claude.json") {
		t.Fatal(got)
	}
	// legacy wins, default dir
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	legacy := filepath.Join(home, ".claude", ".config.json")
	_ = os.WriteFile(legacy, nil, 0o600)
	if got := GlobalConfigPath(home, ""); got != legacy {
		t.Fatal(got)
	}
	// legacy wins, custom dir
	cl := filepath.Join(cfg, ".config.json")
	_ = os.WriteFile(cl, nil, 0o600)
	if got := GlobalConfigPath(home, cfg); got != cl {
		t.Fatal(got)
	}
}

func TestSettingsPath(t *testing.T) {
	if SettingsPath("/h", "") != "/h/.claude/settings.json" || SettingsPath("/h", "/d") != "/d/settings.json" {
		t.Fatal("settings")
	}
}

func TestBytesClearsKnownFields(t *testing.T) {
	in := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","refreshTokenExpiresAt":5,"scopes":["x"],"subscriptionType":"max","clientId":"c","futureField":7},"mcpOAuth":{"x":1}}`
	c, _ := ParseCredentials([]byte(in))
	c.OAuth.RefreshTokenExpiresAt, c.OAuth.Scopes, c.OAuth.SubscriptionType, c.OAuth.ClientID = 0, nil, "", ""
	out, _ := c.Bytes()
	var m map[string]map[string]any
	_ = json.Unmarshal(out, &m)
	o := m["claudeAiOauth"]
	for _, k := range []string{"refreshTokenExpiresAt", "scopes", "subscriptionType", "clientId"} {
		if _, ok := o[k]; ok {
			t.Errorf("%s not cleared", k)
		}
	}
	if o["futureField"] != float64(7) || o["accessToken"] != "a" {
		t.Fatalf("lost keys: %s", out)
	}
}

func TestWithOAuthAddsMissingKey(t *testing.T) {
	c := Credentials{OAuth: OAuth{AccessToken: "a", RefreshToken: "r"}}
	for _, in := range []string{`{"mcpOAuth":{"k": 1}}`, `{ }`, `{"claudeAiOauth":null,"z":[1, 2]}`} {
		out, err := WithOAuth([]byte(in), c)
		if err != nil {
			t.Fatal(in, err)
		}
		got, err := ParseCredentials(out)
		if err != nil || got.OAuth.RefreshToken != "r" || !json.Valid(out) {
			t.Fatalf("%s -> %s %v", in, out, err)
		}
	}
	if _, err := ParseCredentials([]byte(`{"claudeAiOauth":null}`)); !errors.Is(err, ErrNoOAuth) {
		t.Fatal(err)
	}
}
