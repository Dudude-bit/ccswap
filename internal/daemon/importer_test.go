package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

type fakeProfiles map[string]string // access token -> email

func (f fakeProfiles) Profile(_ context.Context, tok string) (anthropic.Profile, error) {
	e, ok := f[tok]
	if !ok {
		return anthropic.Profile{}, errors.New("unauthorized")
	}
	return anthropic.Profile{Email: e}, nil
}

type noLock struct{}

func (noLock) Locked(fn func() error) error { return fn() }

func credJSON(tok string) []byte {
	return []byte(`{"claudeAiOauth":{"accessToken":"` + tok + `","refreshToken":"r-` + tok + `"}}`)
}

type env struct {
	kc   *keychain.Fake
	st   *store.Store
	im   *Importer
	home string
	dir  string // a login dir as `ccswap login` makes it
}

func newEnv(t *testing.T, profiles fakeProfiles) *env {
	t.Helper()
	kc := keychain.NewFake()
	home := t.TempDir()
	st, err := store.Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, "login-*")
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{KC: kc, Store: st, Profiles: profiles, Locker: noLock{}, Home: home, Target: claudecode.ServiceName("")}
	return &env{kc, st, im, home, dir}
}

func (e *env) login(t *testing.T, tok string) {
	t.Helper()
	if err := e.kc.Set(claudecode.ServiceName(e.dir), claudecode.KeychainAccount(), credJSON(tok)); err != nil {
		t.Fatal(err)
	}
}

func (e *env) tempGone(t *testing.T) {
	t.Helper()
	if _, err := e.kc.Get(claudecode.ServiceName(e.dir), claudecode.KeychainAccount()); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("temp entry still there")
	}
}

func TestImportRejectsDuplicateEmail(t *testing.T) {
	e := newEnv(t, fakeProfiles{"t1": "me@example.net", "t2": "ME@example.net"})
	e.login(t, "t1")
	a, err := e.im.Import(context.Background(), "first", e.dir, 2)
	if err != nil || !a.Enabled || a.Priority != 2 || a.AuthIndex != store.AuthIndex("first") {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := e.st.Creds("first"); err != nil {
		t.Fatal("vault creds not stored")
	}
	e.tempGone(t)
	e.login(t, "t2")
	_, err = e.im.Import(context.Background(), "second", e.dir, 0)
	if !errors.Is(err, store.ErrDuplicate) || !strings.Contains(err.Error(), "first") {
		t.Fatalf("err %v", err)
	}
	e.tempGone(t)
	if _, err := e.st.Creds("second"); err == nil {
		t.Fatal("second stored")
	}
}

func TestImportLoginIncomplete(t *testing.T) {
	e := newEnv(t, fakeProfiles{})
	if _, err := e.im.Import(context.Background(), "x", e.dir, 0); !errors.Is(err, ErrLoginIncomplete) {
		t.Fatal(err)
	}
}

func TestImportInvalidName(t *testing.T) {
	e := newEnv(t, fakeProfiles{})
	if _, err := e.im.Import(context.Background(), "Bad Name", e.dir, 0); !errors.Is(err, ErrInvalidName) {
		t.Fatal(err)
	}
}

func TestImportSymlinkedConfigDir(t *testing.T) {
	e := newEnv(t, fakeProfiles{"t1": "a@example.net"})
	link := filepath.Join(e.home, "login-link")
	if err := os.Symlink(e.dir, link); err != nil {
		t.Fatal(err)
	}
	// Claude Code hashed the resolved path; we were given the link.
	resolved, _ := filepath.EvalSymlinks(link)
	svc := claudecode.ServiceName(resolved)
	e.kc.Set(svc, claudecode.KeychainAccount(), credJSON("t1"))
	if _, err := e.im.Import(context.Background(), "a", link, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.kc.Get(svc, claudecode.KeychainAccount()); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("temp entry left")
	}
}

func TestImportOAuthAccountMatching(t *testing.T) {
	e := newEnv(t, fakeProfiles{"t1": "a@example.net", "t2": "b@example.net"})
	cfg := claudecode.GlobalConfigPath("", e.dir)
	os.WriteFile(cfg, []byte(`{"oauthAccount":{"emailAddress":"A@example.net","k":1}}`), 0o600)
	e.login(t, "t1")
	a, err := e.im.Import(context.Background(), "a", e.dir, 0)
	if err != nil || a.OAuthAccount == nil {
		t.Fatalf("%+v %v", a, err)
	}
	e.login(t, "t2") // config still names a@example.net
	b, err := e.im.Import(context.Background(), "b", e.dir, 0)
	if err != nil || b.OAuthAccount != nil {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestReLogin(t *testing.T) {
	e := newEnv(t, fakeProfiles{"t1": "a@example.net", "t2": "a@example.net", "t3": "other@example.net"})
	ctx := context.Background()
	e.login(t, "t1")
	if _, err := e.im.Import(ctx, "a", e.dir, 0); err != nil {
		t.Fatal(err)
	}
	e.st.Update(func(s *store.State) error { s.Accounts[0].NeedsLogin = true; return nil })

	e.login(t, "t2")
	a, err := e.im.Import(ctx, "a", e.dir, 5)
	if err != nil || a.NeedsLogin {
		t.Fatalf("%+v %v", a, err)
	}
	c, _ := e.st.Creds("a")
	if c.OAuth.AccessToken != "t2" || e.st.State().Accounts[0].NeedsLogin || len(e.st.State().Accounts) != 1 {
		t.Fatal("re-login did not replace creds")
	}

	e.login(t, "t3") // same name, different email
	if _, err := e.im.Import(ctx, "a", e.dir, 0); !errors.Is(err, store.ErrDuplicate) {
		t.Fatal(err)
	}

	e.st.Update(func(s *store.State) error { s.Active = "a"; return nil })
	e.login(t, "t2")
	if _, err := e.im.Import(ctx, "a", e.dir, 0); !errors.Is(err, ErrActiveAccount) {
		t.Fatal(err)
	}
	e.tempGone(t)
}

func TestImportRejectsForeignConfigDir(t *testing.T) {
	e := newEnv(t, fakeProfiles{"live": "me@example.net", "t1": "me@example.net"})
	live := credJSON("live")
	other, _ := filepath.EvalSymlinks(t.TempDir())
	e.im.Target = claudecode.ServiceName(other) // config.toml points Claude Code at other
	evil := filepath.Join(e.home, "login-evil")
	os.Symlink(other, evil)
	plain := filepath.Join(e.home, "plain")
	os.Mkdir(plain, 0o700)
	for _, dir := range []string{"", other, evil, plain, t.TempDir(), e.home + "/login-x/../plain"} {
		for _, svc := range []string{claudecode.ServiceName(dir), e.im.Target} {
			e.kc.Set(svc, claudecode.KeychainAccount(), live)
		}
		if _, err := e.im.Import(context.Background(), "a", dir, 0); !errors.Is(err, ErrInvalidConfigDir) {
			t.Fatalf("%q: %v", dir, err)
		}
		for _, svc := range []string{claudecode.ServiceName(dir), e.im.Target} {
			if b, _ := e.kc.Get(svc, claudecode.KeychainAccount()); string(b) != string(live) {
				t.Fatalf("%q: entry %s deleted", dir, svc)
			}
		}
	}
	e.im.Target = claudecode.ServiceName(e.dir) // the login dir itself is Claude Code's dir
	e.login(t, "t1")
	if _, err := e.im.Import(context.Background(), "a", e.dir, 0); !errors.Is(err, ErrInvalidConfigDir) {
		t.Fatal(err)
	}
}

func TestImportFromCredentialsFile(t *testing.T) { // Linux: the login lives in .credentials.json (keychain.Files)
	old := claudecode.GOOS
	claudecode.GOOS = "linux"
	t.Cleanup(func() { claudecode.GOOS = old })
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	home := t.TempDir()
	kc := keychain.NewFiles(filepath.Join(home, "vault"))
	st, err := store.Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	live := claudecode.ServiceName("")
	im := &Importer{KC: kc, Store: st, Profiles: fakeProfiles{"t1": "a@example.net", "live": "me@example.net"}, Locker: noLock{}, Home: home, Target: live}
	dir, _ := os.MkdirTemp(home, "login-*")
	tmp := filepath.Join(dir, ".credentials.json")
	os.WriteFile(tmp, credJSON("t1"), 0o600)
	a, err := im.Import(context.Background(), "a", dir, 3)
	if err != nil || a.Email != "a@example.net" || a.Priority != 3 || len(st.State().Accounts) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	vault := filepath.Join(home, "vault", store.VaultService, "a")
	if fi, err := os.Stat(vault); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("vault file %v %v", fi, err)
	}
	if c, err := st.Creds("a"); err != nil || c.OAuth.AccessToken != "t1" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temporary login left")
	}

	os.MkdirAll(filepath.Dir(live), 0o700)
	os.WriteFile(live, credJSON("live"), 0o600)
	link := filepath.Join(home, "login-live")
	os.Symlink(filepath.Dir(live), link)
	if _, err := im.Import(context.Background(), "me", link, 0); !errors.Is(err, ErrInvalidConfigDir) {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(live); err != nil || string(b) != string(credJSON("live")) {
		t.Fatalf("live login touched: %q %v", b, err)
	}
}
