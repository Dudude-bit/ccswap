package daemon

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
)

// fakeClaude is a claude whose auth login prints a link naming its login dir, takes C1#S1 as the
// only good code and writes each code it reads to rec.
func fakeClaude(t *testing.T, e *tenv) (rec string) {
	t.Helper()
	rec = filepath.Join(t.TempDir(), "codes")
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\necho 'Opening browser to sign in'\n" +
		"echo \"If the browser didn't open, visit: https://claude.example/oauth/authorize?code=true&dir=$CLAUDE_CONFIG_DIR\"\n" +
		"printf 'Paste code here if prompted > '\nread code\necho \"$code\" >> " + rec + "\n" +
		"[ \"$code\" = 'C1#S1' ] || { echo \"Login failed: That code wasn't accepted.\"; exit 1; }\necho 'Login successful.'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	t.Cleanup(func() {
		for _, n := range []string{"a", "b"} {
			e.d.CancelLogin(context.Background(), n)
		}
	})
	return rec
}

// startLogin starts b's login and returns its login dir, read off the link.
func startLogin(t *testing.T, e *tenv) string {
	t.Helper()
	l, err := e.d.StartLogin(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(l.Url)
	if err != nil || u.Host != "claude.example" || !l.ExpiresAt.Equal(t0.Add(loginTTL)) {
		t.Fatalf("link %+v %v", l, err)
	}
	dir := u.Query().Get("dir")
	if filepath.Dir(dir) != e.home || !strings.HasPrefix(filepath.Base(dir), "login-") {
		t.Fatalf("login dir %q", dir)
	}
	return dir
}

func gone(t *testing.T, dir string) {
	t.Helper()
	waitFor(t, "the login dir dropped", func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
}

func TestLoginFromThePage(t *testing.T) {
	e := newTenv(t)
	rec := fakeClaude(t, e)
	dir := startLogin(t, e)
	e.api.set(func(f *fakeAnthropic) { f.emails["atB2"] = "b@x" })
	e.kc.Fake.Set(claudecode.ServiceName(dir), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(8*time.Hour)))
	a, err := e.d.FinishLogin(context.Background(), "b", " https://platform.claude.com/oauth/code/callback?code=C1&state=S1 ")
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
	if _, err := e.d.FinishLogin(context.Background(), "b", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a used link: %v", err)
	}
}

func TestLoginCodeRefusedSpendsTheLink(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	dir := startLogin(t, e)
	if _, err := e.d.FinishLogin(context.Background(), "b", "C9#S9"); !errors.Is(err, ErrCodeRefused) {
		t.Fatal(err)
	}
	gone(t, dir)
	if _, err := e.d.FinishLogin(context.Background(), "b", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("a spent link: %v", err)
	}
	if c := e.vault("b"); c.OAuth.AccessToken != "atB" {
		t.Fatalf("vault %+v", c.OAuth)
	}
}

func TestLoginOfAnotherAccountRefused(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	dir := startLogin(t, e)
	e.api.set(func(f *fakeAnthropic) { f.emails["atC"] = "c@x" })
	svc := claudecode.ServiceName(dir)
	e.kc.Fake.Set(svc, claudecode.KeychainAccount(), cred("atC", "rC", t0.Add(8*time.Hour)))
	if _, err := e.d.FinishLogin(context.Background(), "b", "C1#S1"); !errors.Is(err, ErrWrongAccount) || !strings.Contains(err.Error(), "b@x") {
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

func TestLoginRefusals(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	ctx := context.Background()
	if _, err := e.d.StartLogin(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such account: %v", err)
	}
	if _, err := e.d.StartLogin(ctx, "a"); !errors.Is(err, ErrActiveAccount) {
		t.Errorf("in Claude Code: %v", err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Errorf("none started: %v", err)
	}
	startLogin(t, e)
	if _, err := e.d.FinishLogin(ctx, "b", "  "); !errors.Is(err, ErrEmptyCode) {
		t.Errorf("empty code: %v", err)
	}
	if _, err := e.d.FinishLogin(ctx, "b", "C9#S9"); !errors.Is(err, ErrCodeRefused) {
		t.Errorf("an empty code spent the link: %v", err)
	}

	t.Setenv("CCSWAP_CLAUDE_BIN", filepath.Join(t.TempDir(), "none"))
	if _, err := e.d.StartLogin(ctx, "b"); !errors.Is(err, ErrLoginFailed) {
		t.Errorf("no claude: %v", err)
	}
	quits := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(quits, []byte("#!/bin/sh\necho 'Not logged in: network unreachable'\nexit 1\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", quits)
	if _, err := e.d.StartLogin(ctx, "b"); !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), "network unreachable") {
		t.Errorf("no link: %v", err)
	}
	waitFor(t, "failed logins dropped", func() bool { ds, _ := filepath.Glob(filepath.Join(e.home, "login-*")); return len(ds) == 0 })
}

func TestLoginLinkEnds(t *testing.T) {
	e := newTenv(t)
	fakeClaude(t, e)
	ctx := context.Background()
	first := startLogin(t, e)
	second := startLogin(t, e)
	gone(t, first)
	if err := e.d.CancelLogin(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	gone(t, second)
	if _, err := e.d.FinishLogin(ctx, "b", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("cancelled: %v", err)
	}
	if err := e.d.CancelLogin(ctx, "b"); err != nil {
		t.Fatalf("nothing to cancel: %v", err)
	}

	old := loginTTL
	loginTTL = 300 * time.Millisecond
	t.Cleanup(func() { loginTTL = old })
	gone(t, startLogin(t, e))
	if _, err := e.d.FinishLogin(ctx, "b", "C1#S1"); !errors.Is(err, ErrLoginGone) {
		t.Fatalf("expired: %v", err)
	}
}
