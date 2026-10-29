package daemon

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
)

// fakeClaude is a claude whose auth login prints a link naming its login dir, takes C1#S1 as the
// only good code and writes each code it reads to rec.
func fakeClaude(t *testing.T, e *tenv) (rec string) {
	t.Helper()
	rec = filepath.Join(t.TempDir(), "codes")
	useClaude(t, e, "echo 'Opening browser to sign in'\n"+
		"echo \"If the browser didn't open, visit: https://claude.example/oauth/authorize?code=true&dir=$CLAUDE_CONFIG_DIR\"\n"+
		"printf 'Paste code here if prompted > '\nread code\necho \"$code\" >> "+rec+"\n"+
		"[ \"$code\" = 'C1#S1' ] || { echo \"Login failed: That code wasn't accepted.\"; exit 1; }\necho 'Login successful.'\n")
	return rec
}

// useClaude makes script the claude logins run, and stops the logins it left when the test ends.
func useClaude(t *testing.T, e *tenv, script string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	t.Cleanup(func() {
		e.d.mu.Lock()
		ls := maps.Clone(e.d.logins)
		e.d.mu.Unlock()
		for n, p := range ls {
			e.d.dropLogin(n, p)
		}
	})
}

// startLogin starts b's login and returns its link and login dir, read off the link.
func startLogin(t *testing.T, e *tenv) (api.LoginLink, string) {
	t.Helper()
	l, err := e.d.StartLogin(context.Background(), "b", false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(l.Url)
	if err != nil || u.Host != "claude.example" || !l.ExpiresAt.Equal(t0.Add(loginTTL)) || l.Id == "" {
		t.Fatalf("link %+v %v", l, err)
	}
	dir := u.Query().Get("dir")
	if filepath.Dir(dir) != e.home || !strings.HasPrefix(filepath.Base(dir), "login-") {
		t.Fatalf("login dir %q", dir)
	}
	return l, dir
}

func gone(t *testing.T, dir string) {
	t.Helper()
	waitFor(t, "the login dir dropped", func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
}

func TestLoginFromThePage(t *testing.T) {
	e := newTenv(t)
	rec := fakeClaude(t, e)
	l, dir := startLogin(t, e)
	e.api.set(func(f *fakeAnthropic) { f.emails["atB2"] = "b@x" })
	e.kc.Fake.Set(claudecode.ServiceName(dir), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(8*time.Hour)))
	a, err := e.d.FinishLogin(context.Background(), "b", l.Id, " https://platform.claude.com/oauth/code/callback?code=C1&state=S1 ")
	if err != nil || a.Name != "b" || a.NeedsLogin {
		t.Fatalf("%+v %v", a, err)
	}
	if c := e.vault("b"); c.OAuth.AccessToken != "atB2" || c.OAuth.RefreshToken != "rB2" {
		t.Fatalf("vault %+v", c.OAuth)
	}
	if b, _ := os.ReadFile(rec); string(b) != "C1#S1\n" {
		t.Fatalf("claude read %q", b)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("login dir left")
	}
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a used link: %v", err)
	}
}

// TestLoginWithNoCode: signing in on the daemon's machine, in the tab claude opens, ends claude auth
// login by itself; its login is kept all the same.
func TestLoginWithNoCode(t *testing.T) {
	e := newTenv(t)
	useClaude(t, e, "echo \"visit: https://claude.example/oauth/authorize?code=true&dir=$CLAUDE_CONFIG_DIR\"\n"+
		"while [ ! -e \"$CLAUDE_CONFIG_DIR/signed-in\" ]; do sleep 0.02; done\necho 'Login successful.'\n")
	l, dir := startLogin(t, e)
	e.api.set(func(f *fakeAnthropic) { f.emails["atB2"] = "b@x" })
	e.kc.Fake.Set(claudecode.ServiceName(dir), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(8*time.Hour)))
	os.WriteFile(filepath.Join(dir, "signed-in"), nil, 0o600)
	gone(t, dir)
	if c := e.vault("b"); c.OAuth.AccessToken != "atB2" {
		t.Fatalf("the login claude got is not kept: vault %+v", c.OAuth)
	}
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a link claude ended: %v", err)
	}
}

func TestLoginCodeRefusedSpendsTheLink(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	l, dir := startLogin(t, e)
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C9#S9"); !errors.Is(err, ErrCodeRefused) {
		t.Fatal(err)
	}
	gone(t, dir)
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a spent link: %v", err)
	}
	if c := e.vault("b"); c.OAuth.AccessToken != "atB" {
		t.Fatalf("vault %+v", c.OAuth)
	}
}

func TestLoginOfAnotherAccountRefused(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	l, dir := startLogin(t, e)
	e.api.set(func(f *fakeAnthropic) { f.emails["atC"] = "c@x" })
	svc := claudecode.ServiceName(dir)
	e.kc.Fake.Set(svc, claudecode.KeychainAccount(), cred("atC", "rC", t0.Add(8*time.Hour)))
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C1#S1"); !errors.Is(err, ErrWrongAccount) || !strings.Contains(err.Error(), "b@x") {
		t.Fatal(err)
	}
	if c := e.vault("b"); c.OAuth.AccessToken != "atB" {
		t.Fatalf("vault %+v", c.OAuth)
	}
	if _, err := e.kc.Fake.Get(svc, claudecode.KeychainAccount()); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("the other login left: %v", err)
	}
	gone(t, dir)
}

// TestLoginNeverAddsAnAccount: the read key may start a login, so one of an account removed meanwhile
// adds nothing.
func TestLoginNeverAddsAnAccount(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	l, dir := startLogin(t, e)
	if err := e.d.Remove(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) { f.emails["atB2"] = "b@x" })
	e.kc.Fake.Set(claudecode.ServiceName(dir), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(8*time.Hour)))
	if _, err := e.d.FinishLogin(context.Background(), "b", l.Id, "C1#S1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, ok := e.st.State().Find("b"); ok {
		t.Fatal("a login from the page added an account")
	}
	gone(t, dir)
}

func TestLoginRefusals(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	ctx := context.Background()
	if _, err := e.d.StartLogin(ctx, "nobody", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such account: %v", err)
	}
	if _, err := e.d.StartLogin(ctx, "a", true); !errors.Is(err, ErrActiveAccount) {
		t.Errorf("in Claude Code: %v", err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", "x", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Errorf("none started: %v", err)
	}
	l, _ := startLogin(t, e)
	if _, err := e.d.FinishLogin(ctx, "b", l.Id, "  "); !errors.Is(err, ErrEmptyCode) {
		t.Errorf("empty code: %v", err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", l.Id, "C9#S9"); !errors.Is(err, ErrCodeRefused) {
		t.Errorf("an empty code spent the link: %v", err)
	}

	t.Setenv("CCSWAP_CLAUDE_BIN", filepath.Join(t.TempDir(), "none"))
	if _, err := e.d.StartLogin(ctx, "b", false); !errors.Is(err, ErrLoginFailed) {
		t.Errorf("no claude: %v", err)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := e.d.StartLogin(ctx, "b", false); !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), i18n.T("daemon.claude_not_found")) {
		t.Errorf("no claude in PATH: %v", err)
	}
	quits := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(quits, []byte("#!/bin/sh\necho 'Not logged in: network unreachable'\nexit 1\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", quits)
	if _, err := e.d.StartLogin(ctx, "b", false); !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), "network unreachable") {
		t.Errorf("no link: %v", err)
	}
	waitFor(t, "failed logins dropped", func() bool { ds, _ := filepath.Glob(filepath.Join(e.home, "login-*")); return len(ds) == 0 })
	e.d.mu.Lock()
	left := len(e.d.logins)
	e.d.mu.Unlock()
	if left != 0 {
		t.Fatal("a claude that quit left its link")
	}
}

// TestLoginLinkIsTheStarters: a link waiting for its code is replaced only by a start with replace (the
// admin key, the unix socket), and only its id finishes or cancels it.
func TestLoginLinkIsTheStarters(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	ctx := context.Background()
	first, firstDir := startLogin(t, e)
	if _, err := e.d.StartLogin(ctx, "b", false); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("a second start: %v", err)
	}
	if ds, _ := filepath.Glob(filepath.Join(e.home, "login-*")); len(ds) != 1 {
		t.Fatalf("a refused start ran claude: %v", ds)
	}
	if err := e.d.CancelLogin(ctx, "b", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", "other", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("another id: %v", err)
	}
	quits := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(quits, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	bin := os.Getenv("CCSWAP_CLAUDE_BIN")
	t.Setenv("CCSWAP_CLAUDE_BIN", quits)
	if _, err := e.d.StartLogin(ctx, "b", true); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("a claude that quits: %v", err)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	if _, err := os.Stat(firstDir); err != nil {
		t.Fatal("a failed replacement ended the link:", err)
	}
	second, err := e.d.StartLogin(ctx, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	gone(t, firstDir)
	if _, err := e.d.FinishLogin(ctx, "b", first.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a replaced link: %v", err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", second.Id, "C9#S9"); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("the new link: %v", err)
	}
}

func TestLoginLinkEnds(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	ctx := context.Background()
	l, dir := startLogin(t, e)
	if err := e.d.CancelLogin(ctx, "b", l.Id); err != nil {
		t.Fatal(err)
	}
	gone(t, dir)
	if _, err := e.d.FinishLogin(ctx, "b", l.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("cancelled: %v", err)
	}
	if err := e.d.CancelLogin(ctx, "b", l.Id); err != nil {
		t.Fatalf("nothing to cancel: %v", err)
	}

	old := loginTTL
	loginTTL = 300 * time.Millisecond
	t.Cleanup(func() { loginTTL = old })
	l, dir = startLogin(t, e)
	gone(t, dir)
	if _, err := e.d.FinishLogin(ctx, "b", l.Id, "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("expired: %v", err)
	}
}
