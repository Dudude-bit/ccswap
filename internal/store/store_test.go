package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
)

func creds(t *testing.T) claudecode.Credentials {
	c, err := claudecode.ParseCredentials([]byte(`{"claudeAiOauth":{"accessToken":"SECRET-AT","refreshToken":"SECRET-RT","expiresAt":1,"scopes":["a"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	kc := keychain.NewFake()
	s, err := Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	if !s.State().Auto {
		t.Fatal("fresh state must be Auto")
	}
	err = s.Update(func(st *State) error {
		st.Accounts = append(st.Accounts,
			Account{Name: "b", Email: "b@x", Priority: 2, Enabled: true, OAuthAccount: json.RawMessage(`{"k":1}`), AuthIndex: AuthIndex("b")},
			Account{Name: "a", Email: "a@x", Priority: 1})
		st.Active = "b"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutCreds("b", creds(t)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, "state.json")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatal(err, fi)
	}
	raw, _ := os.ReadFile(p)
	if bytes.Contains(raw, []byte("SECRET")) {
		t.Fatal("token in state.json")
	}
	s2, err := Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	st := s2.State()
	if st.Active != "b" || len(st.Accounts) != 2 || st.Accounts[0].Name != "b" || compact(st.Accounts[0].OAuthAccount) != `{"k":1}` {
		t.Fatalf("%+v", st)
	}
	if a, ok := st.Find("a"); !ok || a.Email != "a@x" {
		t.Fatal("Find")
	}
	if _, ok := st.FindEmail("b@x"); !ok {
		t.Fatal("FindEmail")
	}
}

func TestUpdateErrorAndCopy(t *testing.T) {
	s, _ := Open(t.TempDir(), keychain.NewFake())
	s.Update(func(st *State) error { st.Accounts = []Account{{Name: "a"}}; return nil })
	boom := errors.New("boom")
	if err := s.Update(func(st *State) error { st.Accounts[0].Name = "z"; st.Active = "z"; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if s.State().Accounts[0].Name != "a" || s.State().Active != "" {
		t.Fatal("failed update leaked")
	}
	got := s.State()
	got.Accounts[0].Name = "mut"
	if s.State().Accounts[0].Name != "a" {
		t.Fatal("State not a copy")
	}
}

func TestCorruptStateIsError(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "state.json"), []byte("{nope"), 0o600)
	if _, err := Open(home, keychain.NewFake()); err == nil {
		t.Fatal("want error")
	}
}

func TestWriteThroughSymlink(t *testing.T) {
	home, real := t.TempDir(), t.TempDir()
	target := filepath.Join(real, "s.json")
	os.WriteFile(target, []byte(`{"auto":false}`), 0o600)
	os.Symlink(target, filepath.Join(home, "state.json"))
	s, err := Open(home, keychain.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Active = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(home, "state.json")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if b, _ := os.ReadFile(target); !bytes.Contains(b, []byte(`"x"`)) {
		t.Fatal("target not written")
	}
}

func TestCredsLiveInVault(t *testing.T) {
	kc := keychain.NewFake()
	s, _ := Open(t.TempDir(), kc)
	if _, err := s.Creds("work"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal(err)
	}
	c := creds(t)
	if err := s.PutCreds("work", c); err != nil {
		t.Fatal(err)
	}
	if _, err := kc.Get("ccswap", "work"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Creds("work")
	if err != nil || got.OAuth.AccessToken != "SECRET-AT" {
		t.Fatal(got, err)
	}
	if err := s.DeleteCreds("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := kc.Get("ccswap", "work"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal(err)
	}
	if s.PutCreds("Bad", c) == nil {
		t.Fatal("name not validated")
	}
	if _, err := s.Creds("Bad"); err == nil {
		t.Fatal("name not validated")
	}
}

func TestValidName(t *testing.T) {
	for n, want := range map[string]bool{"team": true, "a-1": true, "Bad": false, "-x": false, "": false} {
		if ValidName(n) != want {
			t.Errorf("%q", n)
		}
	}
	if len(AuthIndex("a")) != 16 {
		t.Fatal("AuthIndex")
	}
}

func compact(b []byte) string {
	var o bytes.Buffer
	json.Compact(&o, b)
	return o.String()
}

func TestFindEmailCaseInsensitive(t *testing.T) {
	s := State{Accounts: []Account{{Name: "blank"}, {Name: "a", Email: "Me@Example.net"}}}
	if a, ok := s.FindEmail("me@example.NET"); !ok || a.Name != "a" {
		t.Fatal(a, ok)
	}
	if a, ok := s.FindEmail(""); ok {
		t.Fatal("empty email matched", a)
	}
}

func TestWriteSyncsBeforeRename(t *testing.T) {
	home := t.TempDir()
	s, _ := Open(home, keychain.NewFake())
	s.Update(func(st *State) error { st.Active = "old"; return nil })
	old := atomicfile.Sync
	t.Cleanup(func() { atomicfile.Sync = old })
	synced := false
	atomicfile.Sync = func(f *os.File) error {
		b, _ := os.ReadFile(f.Name())
		cur, _ := os.ReadFile(filepath.Join(home, "state.json"))
		synced = bytes.Contains(b, []byte(`"new"`)) && bytes.Contains(cur, []byte(`"old"`))
		return f.Sync()
	}
	if err := s.Update(func(st *State) error { st.Active = "new"; return nil }); err != nil || !synced {
		t.Fatal("state.json must be flushed to disk before it replaces the old one", err)
	}
	atomicfile.Sync = func(*os.File) error { return errors.New("disk gone") }
	if err := s.Update(func(st *State) error { st.Active = "lost"; return nil }); err == nil || s.State().Active != "new" {
		t.Fatal("an unsynced write must not replace state.json", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "state.json")); !bytes.Contains(b, []byte(`"new"`)) {
		t.Fatalf("%s", b)
	}
}

func TestHoldersLogEachChangeOfActive(t *testing.T) {
	home, kc := t.TempDir(), keychain.NewFake()
	os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"accounts":[{"name":"a"},{"name":"b"},{"name":"c"}],"active":"a","auto":true}`), 0o600) // before holders
	s, err := Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	now := t0
	s.Now = func() time.Time { return now }
	set := func(active string) {
		t.Helper()
		if err := s.Update(func(st *State) error { st.Active = active; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	set("a") // unchanged
	if h := s.State().Holders; len(h) != 0 {
		t.Fatalf("no change of Active, yet %+v", h)
	}
	set("b")
	now = t0.Add(time.Hour)
	s.Update(func(st *State) error { st.Auto = false; return nil })
	set("b")
	now = t0.Add(2 * time.Hour)
	set("")
	now = t0.Add(3 * time.Hour)
	set("c")
	want := []Holder{{"b", t0}, {"", t0.Add(2 * time.Hour)}, {"c", t0.Add(3 * time.Hour)}}
	if h := s.State().Holders; !slices.Equal(h, want) {
		t.Fatalf("holders %+v, want %+v", h, want)
	}
	s.State().Holders[0].Account = "mut"
	if s.State().Holders[0].Account != "b" {
		t.Fatal("State not a copy")
	}
	s2, err := Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	if h := s2.State().Holders; !slices.Equal(h, want) {
		t.Fatalf("reopened: holders %+v, want %+v", h, want)
	}

	now = t0.Add(50*time.Hour + 30*time.Minute) // b's and the gap's entries are past 48 h; the gap's is the last such
	set("b")
	want = []Holder{{"", t0.Add(2 * time.Hour)}, {"c", t0.Add(3 * time.Hour)}, {"b", now}}
	if h := s.State().Holders; !slices.Equal(h, want) {
		t.Fatalf("trimmed: holders %+v, want %+v", h, want)
	}

	s3, _ := Open(t.TempDir(), kc) // the default clock
	s3.Update(func(st *State) error { st.Active = "a"; return nil })
	if h := s3.State().Holders; len(h) != 1 || time.Since(h[0].Since).Abs() > time.Minute {
		t.Fatalf("holders %+v, want one entry from now", h)
	}
}

func TestHoldersSeededFromBeforeTheLog(t *testing.T) { // the holder since the last switch survives the first change
	home, kc := t.TempDir(), keychain.NewFake()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"accounts":[{"name":"a"},{"name":"b"}],"active":"a","auto":true,"last_switch_at":"2026-10-13T10:00:00Z"}`), 0o600)
	s, err := Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return t0 }
	want := []Holder{{"a", t0.Add(-2 * time.Hour)}}
	if h := s.State().Holders; !slices.Equal(h, want) {
		t.Fatalf("holders %+v, want %+v", h, want)
	}
	if err := s.Update(func(st *State) error { st.Active = "b"; return nil }); err != nil {
		t.Fatal(err)
	}
	want = append(want, Holder{"b", t0})
	if s2, _ := Open(home, kc); !slices.Equal(s2.State().Holders, want) {
		t.Fatalf("holders %+v, want %+v", s2.State().Holders, want)
	}
}

func TestHoldLogsWhoHoldsClaudeCode(t *testing.T) { // whatever State.Active says: a stranger's login, or a's back
	home, kc := t.TempDir(), keychain.NewFake()
	s, _ := Open(home, kc)
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	now := t0
	s.Now = func() time.Time { return now }
	s.Update(func(st *State) error { st.Active = "a"; return nil })
	hold := func(name string) {
		t.Helper()
		now = now.Add(time.Minute)
		if err := s.Hold(name); err != nil {
			t.Fatal(err)
		}
	}
	hold("a") // says so already
	hold("")
	hold("") // still
	hold("a")
	want := []Holder{{"a", t0}, {"", t0.Add(2 * time.Minute)}, {"a", t0.Add(4 * time.Minute)}}
	if s2, _ := Open(home, kc); !slices.Equal(s2.State().Holders, want) || s2.State().Active != "a" {
		t.Fatalf("holders %+v, want %+v; active %q", s2.State().Holders, want, s2.State().Active)
	}
}

func TestHoldersForgetARemovedAccount(t *testing.T) { // a later account of its name starts with no shifts
	s, _ := Open(t.TempDir(), keychain.NewFake())
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	now := t0
	s.Now = func() time.Time { return now }
	update := func(fn func(*State)) {
		t.Helper()
		if err := s.Update(func(st *State) error { fn(st); return nil }); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	update(func(st *State) { st.Accounts, st.Active = []Account{{Name: "a"}, {Name: "b"}}, "b" })
	update(func(st *State) { st.Active = "a" })
	update(func(st *State) { st.Accounts = st.Accounts[:1] })
	update(func(st *State) { st.Accounts = append(st.Accounts, Account{Name: "b", Email: "other@example.com"}) })
	want := []Holder{{"", t0}, {"a", t0.Add(time.Hour)}}
	if h := s.State().Holders; !slices.Equal(h, want) {
		t.Fatalf("holders %+v, want %+v", h, want)
	}
}
