package switcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

const (
	svc  = "Claude Code-credentials"
	kacc = "tester"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

type fakeProf struct {
	byAT  map[string]string
	err   error
	calls int
}

func (f *fakeProf) Profile(_ context.Context, at string) (anthropic.Profile, error) {
	f.calls++
	if f.err != nil {
		return anthropic.Profile{}, f.err
	}
	e, ok := f.byAT[at]
	if !ok {
		return anthropic.Profile{}, errors.New("401")
	}
	return anthropic.Profile{Email: e}, nil
}

func credJSON(at, rt string, exp time.Time) []byte {
	return fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, at, rt, exp.UnixMilli())
}

type env struct {
	kc   *keychain.Fake
	st   *store.Store
	prof *fakeProf
	sw   *Switcher
	cj   string
	home string
}

func setup(t *testing.T, entryExp time.Duration) *env {
	t.Helper()
	kc := keychain.NewFake()
	home := t.TempDir()
	st, err := store.Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	cj := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(cj, []byte(`{"other":1,"oauthAccount":{"who":"A","emailAddress":"a@x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = st.Update(func(s *store.State) error {
		s.Accounts = []store.Account{
			{Name: "a", Email: "a@x", Enabled: true, OAuthAccount: json.RawMessage(`{"who":"A"}`)},
			{Name: "b", Email: "b@x", Enabled: true, OAuthAccount: json.RawMessage(`{"who":"B"}`)},
		}
		s.Active = "a"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kc.Set(svc, kacc, credJSON("atA", "rA", now.Add(entryExp)))
	kc.Set(store.VaultService, "b", credJSON("atB", "rB", now.Add(time.Hour)))
	prof := &fakeProf{byAT: map[string]string{"atA": "a@x", "atB": "b@x", "atA2": "A@X"}}
	sw := New(Deps{KC: kc, Store: st, Profiles: prof, Service: svc, KCAccount: kacc, ClaudeJSON: cj, Now: func() time.Time { return now }})
	return &env{kc, st, prof, sw, cj, home}
}

func (e *env) entry(t *testing.T) claudecode.Credentials {
	t.Helper()
	b, err := e.kc.Get(svc, kacc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := claudecode.ParseCredentials(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *env) vault(name string) (claudecode.Credentials, error) { return e.st.Creds(name) }

func TestSwitchHappyPath(t *testing.T) {
	e := setup(t, time.Hour)
	w, err := e.sw.Switch(context.Background(), "b", false)
	if err != nil || w != nil {
		t.Fatal(w, err)
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA" {
		t.Fatal("vault[a]", c.OAuth, err)
	}
	if e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal("entry not B")
	}
	acct, _ := claudecode.ReadOAuthAccount(e.cj)
	if c, _ := json.Marshal(acct); string(c) != `{"who":"B"}` {
		t.Fatal(string(c))
	}
	s := e.st.State()
	if s.Active != "b" || !s.LastSwitchAt.Equal(now) {
		t.Fatal(s)
	}
}

func TestSwitchKeepsTargetVaultCopy(t *testing.T) { // Claude Code may still write the old login back
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	if c, err := e.vault("b"); err != nil || c.OAuth.RefreshToken != "rB" {
		t.Fatal("vault[b] must stay parked until reconcile", c.OAuth, err)
	}
}

func TestSwitchKeepsEntryTopLevelKeys(t *testing.T) {
	e := setup(t, time.Hour)
	mcp := func(v string) string {
		return `,"mcpOAuth":{"cloudflare|1":{"accessToken":"` + v + `","serverUrl":"https://x/?a=1&b=2"}},"future":[1, 2]}`
	}
	a := strings.TrimSuffix(string(credJSON("atA", "rA", now.Add(time.Hour))), "}")
	e.kc.Set(svc, kacc, []byte(a+mcp("m1")))
	// Claude Code refreshes an MCP login between ccswap's reads: the latest blob is the base.
	e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
		if s == svc && n == 2 {
			return []byte(a + mcp("m2")), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.kc.OnGet = nil
	raw, _ := e.kc.Get(svc, kacc)
	if !strings.HasSuffix(string(raw), mcp("m2")) || e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatalf("entry must keep Claude Code's other keys byte for byte: %s", raw)
	}
	v, _ := e.kc.Get(store.VaultService, "a")
	var top map[string]json.RawMessage
	if json.Unmarshal(v, &top) != nil || len(top) != 1 || top["claudeAiOauth"] == nil {
		t.Fatalf("vault[a] must hold claudeAiOauth only: %s", v)
	}
	// a vault item from older versions (whole entry JSON) is stripped to claudeAiOauth on its next write
	e.kc.Set(store.VaultService, "a", []byte(a+mcp("old")))
	if _, err := e.sw.Switch(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	raw, _ = e.kc.Get(svc, kacc)
	if !strings.HasSuffix(string(raw), mcp("m2")) || e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatalf("switch back: %s", raw)
	}
	if v, _ := e.kc.Get(store.VaultService, "b"); strings.Contains(string(v), "mcpOAuth") {
		t.Fatalf("vault[b] took Claude Code's MCP logins: %s", v)
	}
}

func TestSwitchEmptyEntryGetsOAuthOnly(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Delete(svc, kacc)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	raw, _ := e.kc.Get(svc, kacc)
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || len(top) != 1 || e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatalf("%s", raw)
	}
}

func TestSwitchRefusesNearRefresh(t *testing.T) {
	e := setup(t, 2*time.Minute)
	_, err := e.sw.Switch(context.Background(), "b", false)
	if !errors.Is(err, ErrNearRefresh) {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" || e.st.State().Active != "a" {
		t.Fatal("touched")
	}
	if _, err := e.vault("a"); err == nil {
		t.Fatal("vault written")
	}
	if _, err := e.sw.Switch(context.Background(), "b", true); err != nil {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal("force did not switch")
	}
}

func TestSwitchRefusesUnknownActive(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.byAT["atA"] = "stranger@x"
	_, err := e.sw.Switch(context.Background(), "b", false)
	if !errors.Is(err, ErrUnknownActive) {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("entry touched")
	}
	if _, err := e.vault("a"); err == nil {
		t.Fatal("vault written")
	}
}

func TestSwitchKeepsTokenRefreshedMidSwap(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.OnGet = func(s, a string, n int) ([]byte, bool) {
		if s == svc && n >= 2 { // Claude Code refreshed a's token after the first read
			return credJSON("atA2", "rA2", now.Add(time.Hour)), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.kc.OnGet = nil
	if e.entry(t).OAuth.RefreshToken != "rB" || e.st.State().Active != "b" {
		t.Fatal("not switched")
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA2" {
		t.Fatal("vault[a] must hold r2", c.OAuth, err)
	}
}

func TestSwitchIdentityChangedMidSwapRestarts(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.byAT["atZ"] = "b@x"
	e.kc.OnGet = func(s, a string, n int) ([]byte, bool) {
		if s == svc && n >= 2 {
			return credJSON("atZ", "rZ", now.Add(time.Hour)), true
		}
		return nil, false
	}
	// the restart identifies the new holder: an automatic decision made for a is void
	if _, err := e.sw.SwitchFrom(context.Background(), "a", "b"); !errors.Is(err, ErrHolderChanged) {
		t.Fatal(err)
	}
	// a manual switch finds the entry already holding b
	if w, err := e.sw.Switch(context.Background(), "b", false); w != nil || err != nil {
		t.Fatal(w, err)
	}
	e.kc.OnGet = nil
	if e.entry(t).OAuth.RefreshToken != "rA" { // only the read was scripted
		t.Fatal("entry overwritten")
	}
	if e.st.State().Active != "b" {
		t.Fatal("Active must follow the new holder")
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA" {
		t.Fatal("vault[a] must survive", err)
	}
	if _, err := e.vault("b"); err != nil {
		t.Fatal("vault[b] must survive", err)
	}
}

func TestSwitchWithMalformedClaudeJSON(t *testing.T) {
	e := setup(t, time.Hour)
	os.WriteFile(e.cj, []byte(`{not json`), 0o600)
	w, err := e.sw.Switch(context.Background(), "b", false)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	if e.entry(t).OAuth.RefreshToken != "rB" || e.st.State().Active != "b" {
		t.Fatal("not switched")
	}
	if got, _ := os.ReadFile(e.cj); string(got) != `{not json` {
		t.Fatal("claude.json changed")
	}
}

func TestSwitchToNeedsLoginAccount(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts[1].NeedsLogin = true; return nil })
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrNeedsLogin) {
		t.Fatal(err)
	}
	e.st.Update(func(s *store.State) error { s.Accounts[1].NeedsLogin = false; return nil })
	e.kc.Delete(store.VaultService, "b")
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrNeedsLogin) {
		t.Fatal("missing vault creds:", err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("entry touched")
	}
}

func TestSwitchNoSuchAndNoop(t *testing.T) {
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "zz", false); !errors.Is(err, ErrNoSuchAccount) {
		t.Fatal(err)
	}
	if w, err := e.sw.Switch(context.Background(), "a", false); w != nil || err != nil {
		t.Fatal(w, err)
	}
}

func TestIdentifyFingerprintSkipsProfile(t *testing.T) {
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	before := e.prof.calls
	name, known, err := e.sw.Identify(context.Background())
	if err != nil || !known || name != "b" || e.prof.calls != before {
		t.Fatal(name, known, err, e.prof.calls, before)
	}
	// a refreshed token no longer matches: profile is consulted.
	e.kc.Set(svc, kacc, credJSON("atB", "rB-new", now.Add(time.Hour)))
	name, known, err = e.sw.Identify(context.Background())
	if err != nil || !known || name != "b" || e.prof.calls != before+1 {
		t.Fatal(name, known, err, e.prof.calls)
	}
}

func TestSwitchProfileFailureNeedsForce(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.err = errors.New("network down")
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrCannotIdentify) {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("entry touched")
	}
	if _, err := e.sw.Switch(context.Background(), "b", true); err != nil {
		t.Fatal(err)
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA" {
		t.Fatal("force must save under Active", err)
	}
}

func TestSwitchForceWithoutActiveRefuses(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.err = errors.New("x")
	e.st.Update(func(s *store.State) error { s.Active = ""; return nil })
	if _, err := e.sw.Switch(context.Background(), "b", true); !errors.Is(err, ErrUnknownActive) {
		t.Fatal(err)
	}
}

func TestAdoptImportsCurrentEntry(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts = nil; s.Active = ""; return nil })
	if err := e.sw.Adopt(context.Background(), "work", 20); err != nil {
		t.Fatal(err)
	}
	s := e.st.State()
	a, ok := s.Find("work")
	if !ok || s.Active != "work" || a.Email != "a@x" || a.Priority != 20 || !a.Enabled ||
		compact(a.OAuthAccount) != `{"who":"A","emailAddress":"a@x"}` || a.AuthIndex != store.AuthIndex("work") {
		t.Fatal(s)
	}
}

func TestAdoptWritesNoVaultItem(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts = nil; s.Active = ""; return nil })
	e.sw.Adopt(context.Background(), "work", 1)
	if _, err := e.vault("work"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAdoptDuplicatesAndBadName(t *testing.T) {
	e := setup(t, time.Hour)
	if err := e.sw.Adopt(context.Background(), "a", 1); !errors.Is(err, store.ErrDuplicate) {
		t.Fatal("name:", err)
	}
	if err := e.sw.Adopt(context.Background(), "c", 1); !errors.Is(err, store.ErrDuplicate) {
		t.Fatal("email:", err)
	}
	if err := e.sw.Adopt(context.Background(), "Bad Name", 1); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestNoTokensInErrors(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.err = errors.New("boom")
	_, err := e.sw.Switch(context.Background(), "b", false)
	if err == nil || strings.Contains(err.Error(), "atA") || strings.Contains(err.Error(), "rA") {
		t.Fatal(err)
	}
}

func compact(r json.RawMessage) string { b, _ := json.Marshal(r); return string(b) }

func TestFingerprintBoundToAccountNotActive(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Set(store.VaultService, "c", credJSON("atC", "rC", now.Add(time.Hour)))
	e.st.Update(func(s *store.State) error {
		s.Accounts = append(s.Accounts, store.Account{Name: "c", Email: "c@x", Enabled: true})
		return nil
	})
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(e.home, 0o700)
	w, err := e.sw.Switch(context.Background(), "b", false)
	if err == nil {
		t.Skip("state dir writable (root?)")
	}
	_ = w
	if e.entry(t).OAuth.RefreshToken != "rB" || e.st.State().Active != "a" {
		t.Fatal("setup of failure scenario wrong")
	}
	before := e.prof.calls
	if name, known, err := e.sw.Identify(context.Background()); err != nil || !known || name != "b" || e.prof.calls != before {
		t.Fatal(name, known, err)
	}
	os.Chmod(e.home, 0o700)
	if _, err := e.sw.Switch(context.Background(), "c", false); err != nil {
		t.Fatal(err)
	}
	if c, err := e.vault("b"); err != nil || c.OAuth.RefreshToken != "rB" {
		t.Fatal("rB must be filed under b", err)
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA" {
		t.Fatal("a's copy must survive", err)
	}
}

func TestForceNeverOverridesStranger(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.byAT["atA"] = "stranger@x"
	if _, err := e.sw.Switch(context.Background(), "b", true); !errors.Is(err, ErrUnknownActive) {
		t.Fatal(err)
	}
	if _, err := e.vault("a"); err == nil {
		t.Fatal("vault[a] written")
	}
	if e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("entry touched")
	}
}

func TestForceOnProfileFailureRefusedWhenVaultDiffers(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Set(store.VaultService, "a", credJSON("atOld", "rOld", now.Add(time.Hour)))
	e.prof.err = errors.New("down")
	if _, err := e.sw.Switch(context.Background(), "b", true); !errors.Is(err, ErrUnknownActive) {
		t.Fatal(err)
	}
	if c, _ := e.vault("a"); c.OAuth.RefreshToken != "rOld" || e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("something was overwritten")
	}
	// same token in vault: force is fine.
	e.kc.Set(store.VaultService, "a", credJSON("atOld", "rA", now.Add(time.Hour)))
	if _, err := e.sw.Switch(context.Background(), "b", true); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchKeepsRotatedVaultTarget(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.OnGet = func(s, a string, n int) ([]byte, bool) {
		if s == store.VaultService && a == "b" && n == 2 {
			return credJSON("atB2", "rB2", now.Add(time.Hour)), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.kc.OnGet = nil
	if _, err := e.vault("b"); err != nil {
		t.Fatal("rotated vault[b] must be kept", err)
	}
}

func TestLockedSerializes(t *testing.T) {
	e := setup(t, time.Hour)
	ran := false
	err := e.sw.Locked(func() error {
		ran = true
		if e.sw.mu.TryLock() {
			t.Error("mutex not held inside Locked")
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatal(err)
	}
}

func TestEntryAlreadyHoldsTarget(t *testing.T) {
	e := setup(t, time.Minute) // near expiry must not block: no write happens
	e.kc.Set(svc, kacc, credJSON("atB", "rB-live", now.Add(time.Minute)))
	if w, err := e.sw.Switch(context.Background(), "b", false); err != nil || w != nil {
		t.Fatal(w, err)
	}
	if _, err := e.vault("b"); err != nil {
		t.Fatal("vault[b] deleted")
	}
	if e.st.State().Active != "b" {
		t.Fatal("Active not fixed")
	}
}

func TestEntryAlreadyHoldsTargetKeepsEqualVault(t *testing.T) { // only the daemon's reconcile drops it
	e := setup(t, time.Hour)
	e.kc.Set(svc, kacc, credJSON("atB", "rB", now.Add(time.Hour)))
	if w, err := e.sw.Switch(context.Background(), "b", false); err != nil || w != nil {
		t.Fatal(w, err)
	}
	if _, err := e.vault("b"); err != nil {
		t.Fatal("a copy of the chain in the entry may be the only one left if Claude Code writes back:", err)
	}
}

type setThenFail struct {
	*keychain.Fake
	store bool
}

func (f setThenFail) Set(service, account string, b []byte) error {
	if service == svc {
		if f.store {
			f.Fake.Set(service, account, b)
		}
		return errors.New("read-back failed")
	}
	return f.Fake.Set(service, account, b)
}

func TestSetErrorAfterLandedWriteCompletes(t *testing.T) {
	for _, landed := range []bool{true, false} {
		e := setup(t, time.Hour)
		e.sw = New(Deps{KC: setThenFail{e.kc, landed}, Store: e.st, Profiles: e.prof, Service: svc,
			KCAccount: kacc, ClaudeJSON: e.cj, Now: func() time.Time { return now }})
		_, err := e.sw.Switch(context.Background(), "b", false)
		if landed {
			if err != nil || e.st.State().Active != "b" {
				t.Fatal(err)
			}
		} else if err == nil || e.st.State().Active != "a" || e.entry(t).OAuth.RefreshToken != "rA" {
			t.Fatal("failed write must change nothing", err)
		}
	}
}

func TestEmptyProfileEmailIsUnidentifiable(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts[0].Email = ""; return nil })
	e.prof.byAT["atA"] = ""
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrCannotIdentify) {
		t.Fatal(err)
	}
	if err := e.sw.Adopt(context.Background(), "zz", 1); !errors.Is(err, ErrCannotIdentify) {
		t.Fatal(err)
	}
}

func TestRefreshGuardBoundary(t *testing.T) {
	e := setup(t, 5*time.Minute+59*time.Second)
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrNearRefresh) {
		t.Fatal(err)
	}
	e = setup(t, 6*time.Minute+time.Second)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
}

func TestMissingEntryMarksPreviousActiveNeedsLogin(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Delete(svc, kacc)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	s := e.st.State()
	a, _ := s.Find("a")
	if !a.NeedsLogin || s.Active != "b" || e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal(s)
	}
}

func TestAdoptDropsForeignOAuthAccount(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts = nil; s.Active = ""; return nil })
	os.WriteFile(e.cj, []byte(`{"oauthAccount":{"emailAddress":"other@x"}}`), 0o600)
	if err := e.sw.Adopt(context.Background(), "work", 1); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.st.State().Find("work"); len(a.OAuthAccount) != 0 {
		t.Fatal("foreign blob stored")
	}
}

func TestForcedNoopDoesNotBindFingerprint(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Set(svc, kacc, credJSON("atS", "rS", now.Add(time.Hour)))
	e.prof.err = errors.New("down")
	if w, err := e.sw.Switch(context.Background(), "a", true); w != nil || err != nil {
		t.Fatal(w, err)
	}
	e.prof.err = nil
	e.prof.byAT["atS"] = "stranger@x"
	before := e.prof.calls
	if name, known, err := e.sw.Identify(context.Background()); err != nil || known || name != "" || e.prof.calls != before+1 {
		t.Fatal(name, known, err, e.prof.calls)
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrUnknownActive) {
		t.Fatal(err)
	}
	if _, err := e.vault("a"); err == nil {
		t.Fatal("stranger filed under a")
	}
}

func TestMissingEntryKeepsValidVaultAccount(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Set(store.VaultService, "a", credJSON("atA", "rA", now.Add(time.Hour)))
	e.kc.Set(store.VaultService, "c", credJSON("atC", "rC", now.Add(time.Hour)))
	e.st.Update(func(s *store.State) error {
		s.Accounts = append(s.Accounts, store.Account{Name: "c", Email: "c@x", Enabled: true})
		return nil
	})
	e.kc.Delete(svc, kacc)
	if _, err := e.sw.Switch(context.Background(), "c", false); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.st.State().Find("a"); a.NeedsLogin {
		t.Fatal("a has a valid vault copy")
	}
	if _, err := e.sw.Switch(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" {
		t.Fatal("not switched back")
	}
}

func TestAbortDoesNotStampLastSwitch(t *testing.T) {
	e := setup(t, time.Hour)
	e.prof.byAT["atZ"] = "stranger@x"
	e.kc.OnGet = func(s, a string, n int) ([]byte, bool) {
		if s == svc && n >= 2 {
			return credJSON("atZ", "rZ", now.Add(time.Hour)), true
		}
		return nil, false
	}
	e.sw.Switch(context.Background(), "b", false)
	if !e.st.State().LastSwitchAt.IsZero() {
		t.Fatal("abort stamped LastSwitchAt")
	}
}

func TestSwitchFromGivesUpWhenHolderChanged(t *testing.T) {
	e := setup(t, time.Hour)
	before, _ := e.kc.Get(svc, kacc)
	if _, err := e.sw.SwitchFrom(context.Background(), "b", "a"); !errors.Is(err, ErrHolderChanged) {
		t.Fatal(err)
	}
	if after, _ := e.kc.Get(svc, kacc); string(after) != string(before) {
		t.Fatal("entry written although the holder changed")
	}
	if _, err := e.vault("a"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("vault touched", err)
	}
	if _, err := e.sw.SwitchFrom(context.Background(), "a", "b"); err != nil || e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal("SwitchFrom with the right holder must switch:", err)
	}
	e.kc.Delete(svc, kacc)
	if _, err := e.sw.SwitchFrom(context.Background(), "b", "a"); !errors.Is(err, ErrHolderChanged) {
		t.Fatal("empty entry is not the expected holder:", err)
	}
}

func TestWarningsAreLocalized(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts[1].OAuthAccount = nil; return nil })
	w, err := e.sw.Switch(context.Background(), "b", false)
	if err != nil || w == nil || w.Error() != i18n.T("switcher.no_oauth_account", "b") {
		t.Fatalf("%v %v", w, err)
	}
	e = setup(t, time.Hour)
	os.WriteFile(e.cj, []byte(`{not json`), 0o600)
	w, err = e.sw.Switch(context.Background(), "b", false)
	if err != nil || w == nil || !strings.HasPrefix(w.Error(), i18n.T("switcher.oauth_not_updated", "")) {
		t.Fatalf("%v %v", w, err)
	}
}

func TestIdentifyRemembersProfileMatchOnly(t *testing.T) {
	e := setup(t, time.Hour)
	if n, known, err := e.sw.Identify(context.Background()); err != nil || !known || n != "a" {
		t.Fatal(n, known, err)
	}
	e.prof.err = errors.New("401: access token expired") // Claude Code idle, entry unchanged
	if n, known, err := e.sw.Identify(context.Background()); err != nil || !known || n != "a" {
		t.Fatal("a positive identification must be remembered by fingerprint:", n, known, err)
	}
	// a rotated entry is a new chain: the profile is asked again
	e.kc.Set(svc, kacc, credJSON("atA2", "rA2", now.Add(time.Hour)))
	if _, _, err := e.sw.Identify(context.Background()); !errors.Is(err, ErrCannotIdentify) {
		t.Fatal(err)
	}
	// a forced fallback is no identification and is not remembered
	f := setup(t, time.Hour)
	f.prof.err = errors.New("down")
	if _, err := f.sw.Switch(context.Background(), "a", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.sw.Identify(context.Background()); !errors.Is(err, ErrCannotIdentify) {
		t.Fatal("force fallback remembered:", err)
	}
}

func TestFingerprintSurvivesRestart(t *testing.T) {
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	raw, err := e.kc.Get(store.VaultService, "_fingerprint")
	if err != nil || strings.Contains(string(raw), "rB") {
		t.Fatalf("fingerprint must be persisted as a hash only: %s %v", raw, err)
	}
	sw := New(Deps{KC: e.kc, Store: e.st, Profiles: e.prof, Service: svc, KCAccount: kacc, ClaudeJSON: e.cj, Now: func() time.Time { return now }})
	e.prof.err = errors.New("401: access token expired")
	if name, known, err := sw.Identify(context.Background()); err != nil || !known || name != "b" {
		t.Fatal("a restarted daemon must still know the idle entry:", name, known, err)
	}
}

func TestFingerprintNeedsSameEmail(t *testing.T) {
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	// b removed and added again with a different login
	e.st.Update(func(s *store.State) error { s.Accounts[1].Email = "other@x"; return nil })
	before := e.prof.calls
	if name, known, err := e.sw.Identify(context.Background()); err != nil || known || name != "" || e.prof.calls != before+1 {
		t.Fatal("fingerprint must not name an account whose email changed:", name, known, err)
	}
}

func TestForgetLockedClearsFingerprint(t *testing.T) {
	e := setup(t, time.Hour)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.sw.Locked(func() error { e.sw.ForgetLocked("a"); return nil })
	if _, err := e.kc.Get(store.VaultService, "_fingerprint"); err != nil {
		t.Fatal("fingerprint of b dropped for a:", err)
	}
	e.sw.Locked(func() error { e.sw.ForgetLocked("b"); return nil })
	if _, err := e.kc.Get(store.VaultService, "_fingerprint"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("fingerprint of a removed account kept:", err)
	}
	before := e.prof.calls
	e.sw.Identify(context.Background())
	if e.prof.calls != before+1 {
		t.Fatal("forgotten fingerprint still used")
	}
}

func TestExpiredEntry(t *testing.T) {
	e := setup(t, -time.Minute) // Claude Code idle: its access token expired, nothing refreshed it
	e.prof.err = errors.New("401")
	if _, _, err := e.sw.Identify(context.Background()); !errors.Is(err, ErrEntryExpired) || !errors.Is(err, ErrCannotIdentify) {
		t.Fatal(err)
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryExpired) {
		t.Fatal(err)
	}
	f := setup(t, -time.Minute)
	f.st.Update(func(s *store.State) error { s.Accounts = nil; s.Active = ""; return nil })
	if err := f.sw.Adopt(context.Background(), "work", 1); !errors.Is(err, ErrEntryExpired) {
		t.Fatal(err)
	}
	// identified by fingerprint, but the guard sees an expiry already in the past
	g := setup(t, time.Hour)
	if _, _, err := g.sw.Identify(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.kc.Set(svc, kacc, credJSON("atA", "rA", now.Add(-time.Minute)))
	if _, err := g.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryExpired) {
		t.Fatal("past expiry must not read as «about to refresh»:", err)
	}
	if _, err := g.sw.Switch(context.Background(), "b", true); err != nil || g.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal("force switches an idle entry:", err)
	}
}

func TestAdoptRemembersFingerprint(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error { s.Accounts = nil; s.Active = ""; return nil })
	if err := e.sw.Adopt(context.Background(), "work", 1); err != nil {
		t.Fatal(err)
	}
	e.prof.err = errors.New("401")
	if name, known, err := e.sw.Identify(context.Background()); err != nil || !known || name != "work" {
		t.Fatal(name, known, err)
	}
}

func TestSwitchRefusesTargetWithoutAccessToken(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.Set(store.VaultService, "b", credJSON("", "rB", now))
	before, _ := e.kc.Get(svc, kacc)
	if _, err := e.sw.Switch(context.Background(), "b", true); !errors.Is(err, ErrTargetRefreshing) || errors.Is(err, ErrNearRefresh) {
		t.Fatal(err)
	}
	if after, _ := e.kc.Get(svc, kacc); !bytes.Equal(before, after) {
		t.Fatal("entry touched")
	}
}

func TestStateWriteFailureAfterLandedWriteIsAWarning(t *testing.T) {
	e := setup(t, time.Hour)
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	w, err := e.sw.Switch(context.Background(), "b", false)
	os.Chmod(e.home, 0o700)
	if err != nil || w == nil || e.entry(t).OAuth.RefreshToken != "rB" || !e.sw.LastWrite().Equal(now) {
		t.Fatalf("%v %v %v", w, err, e.sw.LastWrite())
	}
}

func TestEntryWithoutLoginKeepsOtherKeys(t *testing.T) {
	for _, entry := range []string{`{"mcpOAuth":{"x|1":{"accessToken":"m","u":"a&b"}}}`, `{"claudeAiOauth": null,"mcpOAuth":{"x":1}}`, `{}`} {
		e := setup(t, time.Hour)
		e.kc.Set(svc, kacc, []byte(entry))
		if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		raw, _ := e.kc.Get(svc, kacc)
		var top map[string]json.RawMessage
		var old map[string]json.RawMessage
		json.Unmarshal([]byte(entry), &old)
		if json.Unmarshal(raw, &top) != nil || e.entry(t).OAuth.RefreshToken != "rB" || string(top["mcpOAuth"]) != string(old["mcpOAuth"]) {
			t.Fatalf("%s -> %s", entry, raw)
		}
		if _, _, err := e.sw.Identify(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.kc.Set(svc, kacc, []byte(entry))
		if _, _, err := e.sw.Identify(context.Background()); !errors.Is(err, keychain.ErrNotFound) {
			t.Fatalf("no login must read as a missing entry: %v", err)
		}
	}
}

func TestLastLookGivesUpWithEntryChanged(t *testing.T) {
	e := setup(t, time.Hour)
	e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
		if s == svc && n >= 2 {
			return credJSON("atA", "rA", now.Add(time.Hour+time.Duration(n)*time.Second)), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryChanged) {
		t.Fatal(err)
	}
}

func TestLastLookLoginAppearingMidSwitch(t *testing.T) { // a missing entry is an empty one
	for _, tc := range []struct {
		entry []byte // nil: no entry (Linux: no file)
		at    string
		want  error
	}{{[]byte(`{}`), "atA", nil}, {[]byte(`{}`), "atX", ErrUnknownActive}, {nil, "atA", nil}, {nil, "atX", ErrUnknownActive}} { // the restart identifies the login that appeared
		e := setup(t, time.Hour)
		e.prof.byAT["atX"] = "x@x"
		e.kc.Delete(svc, kacc)
		if tc.entry != nil {
			e.kc.Set(svc, kacc, tc.entry)
		}
		gets := 0
		e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
			if s == svc {
				gets = n
			}
			if s == svc && n >= 2 {
				return credJSON(tc.at, "rZ", now.Add(time.Hour)), true
			}
			return nil, false
		}
		if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, tc.want) {
			t.Fatalf("%q %s: %v", tc.entry, tc.at, err)
		}
		if gets < 3 {
			t.Fatalf("%q %s: no restart (%d reads)", tc.entry, tc.at, gets)
		}
		if c, err := e.vault("a"); tc.want == nil && (err != nil || c.OAuth.RefreshToken != "rZ") {
			t.Fatal("a's login that appeared must be saved", err)
		}
	}
}

func TestLastLookEmptyEntryVanishingIsStillEmpty(t *testing.T) {
	for _, tc := range [][2][]byte{{nil, []byte(`{"mcpOAuth":{"x":1}}`)}, {[]byte(`{}`), nil}} { // first read, last look; nil: no entry
		e := setup(t, time.Hour)
		e.kc.Delete(svc, kacc)
		e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
			if s == svc && n <= 2 && tc[n-1] != nil {
				return tc[n-1], true
			}
			return nil, false
		}
		if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
			t.Fatalf("%q -> %q: %v", tc[0], tc[1], err)
		}
		if e.entry(t).OAuth.RefreshToken != "rB" {
			t.Fatal("not switched")
		}
	}
}

// withClaudeDir rebuilds the switcher with Claude Code's locks in a temp config dir D.
func (e *env) withClaudeDir(t *testing.T, kc keychain.Store, prof Profiler) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), ".claude")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	e.sw = New(Deps{KC: kc, Store: e.st, Profiles: prof, Service: svc, KCAccount: kacc, ClaudeJSON: e.cj,
		ClaudeDir: d, Now: func() time.Time { return now }})
	return d
}

func claudeLocks(t *testing.T, d string) []string {
	t.Helper()
	real, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return []string{filepath.Join(d, ".oauth_refresh.lock"), real + ".lock", filepath.Join(d, ".storage-write.lock")}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

type setSpy struct {
	*keychain.Fake
	onSet func(service string)
}

func (s setSpy) Set(service, account string, b []byte) error {
	s.onSet(service)
	return s.Fake.Set(service, account, b)
}

func TestSwitchHoldsClaudeLocks(t *testing.T) {
	e := setup(t, time.Hour)
	var locks []string
	set := false
	d := e.withClaudeDir(t, setSpy{e.kc, func(service string) {
		if service != svc {
			return
		}
		set = true
		for _, l := range locks {
			if !exists(l) {
				t.Errorf("entry written without %s", l)
			}
		}
	}}, e.prof)
	locks = claudeLocks(t, d)
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil || !set {
		t.Fatal(err, set)
	}
	for _, l := range locks {
		if exists(l) {
			t.Errorf("%s left behind", l)
		}
	}
}

func TestSwitchWaitsForClaudeWrite(t *testing.T) {
	e := setup(t, time.Hour)
	var released atomic.Bool
	d := e.withClaudeDir(t, setSpy{e.kc, func(service string) {
		if service == svc && !released.Load() {
			t.Error("entry written while Claude Code held its storage lock")
		}
	}}, e.prof)
	lock := filepath.Join(d, ".storage-write.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		released.Store(true)
		os.Remove(lock)
	}()
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rB" {
		t.Fatal("entry not B")
	}
}

func TestSwitchBusyClaudeIsEntryChanged(t *testing.T) {
	old := lockWait
	lockWait = 300 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	e := setup(t, time.Hour)
	d := e.withClaudeDir(t, e.kc, e.prof)
	lock := filepath.Join(d, ".oauth_refresh.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(lock)
	beforeB, _ := e.kc.Get(store.VaultService, "b")
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryChanged) {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" || e.st.State().Active != "a" {
		t.Fatal("entry or state touched")
	}
	if afterB, _ := e.kc.Get(store.VaultService, "b"); !bytes.Equal(beforeB, afterB) {
		t.Fatal("vault[b] touched")
	}
	if _, err := e.vault("a"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("vault[a] written without the locks", err)
	}
	if fi2, err := os.Stat(lock); err != nil || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("foreign lock touched", err)
	}
}

func TestSwitchTakesOverStaleClaudeLock(t *testing.T) {
	e := setup(t, time.Hour)
	d := e.withClaudeDir(t, e.kc, e.prof)
	lock := filepath.Join(d, ".storage-write.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rB" || exists(lock) {
		t.Fatal("stale lock not taken over")
	}
}

type lockCheckProf struct {
	*fakeProf
	t    *testing.T
	lock *string
}

func (p lockCheckProf) Profile(ctx context.Context, at string) (anthropic.Profile, error) {
	if exists(*p.lock) {
		p.t.Error("profile called under Claude Code's locks")
	}
	return p.fakeProf.Profile(ctx, at)
}

func TestSwitchNoNetworkUnderLocks(t *testing.T) {
	e := setup(t, time.Hour)
	var lock string
	d := e.withClaudeDir(t, e.kc, lockCheckProf{e.prof, t, &lock})
	lock = filepath.Join(d, ".storage-write.lock")
	// Claude Code refreshed a's token right after ccswap's first read
	e.kc.Set(svc, kacc, credJSON("atA2", "rA2", now.Add(time.Hour)))
	e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
		if s == svc && n == 1 {
			return credJSON("atA", "rA", now.Add(time.Hour)), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.kc.OnGet = nil
	if e.entry(t).OAuth.RefreshToken != "rB" || e.st.State().Active != "b" {
		t.Fatal("not switched")
	}
	if c, err := e.vault("a"); err != nil || c.OAuth.RefreshToken != "rA2" {
		t.Fatal("vault[a] must hold the refreshed chain", c.OAuth, err)
	}
	if e.prof.calls != 2 {
		t.Fatal("refreshed entry must be identified again:", e.prof.calls)
	}
}

func TestSwitchRestartsOnlyOnce(t *testing.T) {
	e := setup(t, time.Hour)
	e.withClaudeDir(t, e.kc, e.prof)
	e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) { // Claude Code refreshes on every read
		if s == svc {
			return credJSON("atA", fmt.Sprintf("rA%d", n), now.Add(time.Hour)), true
		}
		return nil, false
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryChanged) {
		t.Fatal(err)
	}
	e.kc.OnGet = nil
	if e.entry(t).OAuth.RefreshToken != "rA" || e.st.State().Active != "a" {
		t.Fatal("entry or state touched")
	}
}

type hookProf struct {
	*fakeProf
	hook func(call int)
}

func (p hookProf) Profile(ctx context.Context, at string) (anthropic.Profile, error) {
	p.hook(p.calls + 1)
	return p.fakeProf.Profile(ctx, at)
}

func TestSwitchRestartSharesLockWait(t *testing.T) {
	old := lockWait
	lockWait = time.Second
	t.Cleanup(func() { lockWait = old })
	e := setup(t, time.Hour)
	var lock string
	d := e.withClaudeDir(t, e.kc, hookProf{e.prof, func(call int) {
		if call == 2 { // the restarted try finds Claude Code writing again
			os.Mkdir(lock, 0o700)
		}
	}})
	lock = filepath.Join(d, ".storage-write.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		os.Remove(lock)
	}()
	e.kc.Set(svc, kacc, credJSON("atA2", "rA2", now.Add(time.Hour)))
	e.kc.OnGet = func(s, _ string, n int) ([]byte, bool) {
		if s == svc && n == 1 {
			return credJSON("atA", "rA", now.Add(time.Hour)), true
		}
		return nil, false
	}
	start := time.Now()
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrEntryChanged) {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 1450*time.Millisecond {
		t.Fatal("each try waited a full budget:", el)
	}
}

// vaultBytes snapshots the vault items of a and b; a missing item counts as nil.
func (e *env) vaultBytes() [2][]byte {
	a, _ := e.kc.Get(store.VaultService, "a")
	b, _ := e.kc.Get(store.VaultService, "b")
	return [2][]byte{a, b}
}

func (e *env) sameVault(t *testing.T, before [2][]byte) {
	t.Helper()
	after := e.vaultBytes()
	if !bytes.Equal(before[0], after[0]) || !bytes.Equal(before[1], after[1]) {
		t.Fatal("vault written")
	}
}

func TestSwitchToFrozenRefused(t *testing.T) {
	e := setup(t, time.Hour)
	entry, _ := e.kc.Get(svc, kacc)
	vault := e.vaultBytes()
	e.sw.Freeze([]string{"b"}, now.Add(time.Minute))
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrFrozen) {
		t.Fatal(err)
	}
	after, _ := e.kc.Get(svc, kacc)
	if !bytes.Equal(entry, after) || e.st.State().Active != "a" || !e.sw.LastWrite().IsZero() {
		t.Fatal("entry or state touched")
	}
	e.sameVault(t, vault)
}

func TestSwitchFromFrozenRefused(t *testing.T) {
	e := setup(t, time.Hour)
	vault := e.vaultBytes()
	e.sw.Freeze([]string{"b"}, now.Add(time.Minute))
	if _, err := e.sw.SwitchFrom(context.Background(), "a", "b"); !errors.Is(err, ErrFrozen) {
		t.Fatal(err)
	}
	if e.entry(t).OAuth.RefreshToken != "rA" || e.st.State().Active != "a" {
		t.Fatal("entry or state touched")
	}
	e.sameVault(t, vault)
}

func TestFreezeEndsAtDeadline(t *testing.T) {
	e := setup(t, time.Hour)
	// The shared clock is the package var `now`; safe to move because no test here is parallel.
	saved := now
	t.Cleanup(func() { now = saved })
	e.sw.Freeze([]string{"b"}, now.Add(time.Minute))
	if !e.sw.Frozen("b") || e.sw.Frozen("a") {
		t.Fatal("freeze not scoped")
	}
	now = saved.Add(time.Minute)
	if e.sw.Frozen("b") {
		t.Fatal("still frozen at deadline")
	}
	if len(e.sw.frozen) != 0 {
		t.Fatal("expired entry kept")
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
}

func TestThawLiftsFreeze(t *testing.T) {
	e := setup(t, time.Hour)
	e.sw.Freeze([]string{"a", "b"}, now.Add(time.Hour))
	e.sw.Thaw([]string{"b"})
	if e.sw.Frozen("b") || !e.sw.Frozen("a") {
		t.Fatal("thaw not scoped")
	}
	if _, err := e.sw.Switch(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenDoesNotWaitOnSwitchLock(t *testing.T) {
	e := setup(t, time.Hour)
	e.sw.Freeze([]string{"b"}, now.Add(time.Hour))
	e.sw.mu.Lock()
	defer e.sw.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- e.sw.Frozen("b") }()
	select {
	case v := <-done:
		if !v {
			t.Fatal("not frozen")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Frozen waits on the switch lock")
	}
}

func TestNeedsLoginBeatsFrozen(t *testing.T) {
	e := setup(t, time.Hour)
	e.st.Update(func(s *store.State) error {
		for i := range s.Accounts {
			if s.Accounts[i].Name == "b" {
				s.Accounts[i].NeedsLogin = true
			}
		}
		return nil
	})
	e.sw.Freeze([]string{"b"}, now.Add(time.Hour))
	if _, err := e.sw.Switch(context.Background(), "b", false); !errors.Is(err, ErrNeedsLogin) {
		t.Fatal(err)
	}
}
