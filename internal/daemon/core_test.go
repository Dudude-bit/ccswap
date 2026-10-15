package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/notify"
	"github.com/Dudude-bit/ccswap/internal/policy"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/t3"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type resp struct {
	status int
	body   string
}

// fakeAnthropic serves the token, usage and profile endpoints from maps keyed by token.
type fakeAnthropic struct {
	mu         sync.Mutex
	emails     map[string]string // access token -> email
	usage      map[string]resp   // access token -> usage response
	refresh    map[string]resp   // refresh token -> token response; missing = invalid_grant
	calls      []string
	scope      string
	onUsage    func(tok string) // runs under mu
	retryAfter string           // Retry-After of every 429
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/token":
		var req struct {
			RT    string `json:"refresh_token"`
			Scope string `json:"scope"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.calls = append(f.calls, "refresh "+req.RT)
		f.scope = req.Scope
		rs, ok := f.refresh[req.RT]
		if !ok {
			rs = resp{400, `{"error":"invalid_grant"}`}
		}
		w.WriteHeader(rs.status)
		io.WriteString(w, rs.body)
	case "/api/oauth/usage":
		f.calls = append(f.calls, "usage "+tok)
		if f.onUsage != nil {
			f.onUsage(tok)
		}
		rs, ok := f.usage[tok]
		if !ok {
			rs = resp{401, `{}`}
		}
		if rs.status == http.StatusTooManyRequests && f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(rs.status)
		io.WriteString(w, rs.body)
	case "/api/oauth/profile":
		f.calls = append(f.calls, "profile "+tok)
		e, ok := f.emails[tok]
		if !ok {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"account":{"uuid":"u","email":%q},"organization":{"uuid":"o"}}`, e)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeAnthropic) set(fn func(f *fakeAnthropic)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAnthropic) callsSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[n:])
}

func (f *fakeAnthropic) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func usageBody(h5 float64) resp { return usageAt(h5, t0.Add(2*time.Hour)) }

func usageAt(h5 float64, reset time.Time) resp {
	return resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":%q}}`,
		h5, reset.Format(time.RFC3339), t0.Add(72*time.Hour).Format(time.RFC3339))}
}

// usageWeekAt is usage whose weekly window resets at reset; cache-aware orders by it.
func usageWeekAt(h5 float64, reset time.Time) resp {
	return resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":%q}}`,
		h5, t0.Add(2*time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339))}
}

func init() { saveBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond} }

func (e *tenv) failVault(n int) {
	e.kc.mu.Lock()
	e.kc.failVault = n
	e.kc.mu.Unlock()
}

// spyKC counts writes to Claude Code's entry and vault writes and deletes, and can fail vault reads and writes.
type spyKC struct {
	*keychain.Fake
	mu           sync.Mutex
	entrySets    int
	failVault    int
	failAt       int    // fail the vault write that brings vaultSets to this
	failGet      string // fail vault reads of this account
	vaultSets    int
	vaultDeletes int
	slowVault    time.Duration
	onGet        func(service string) // runs on each read
}

func (s *spyKC) Get(service, account string) ([]byte, error) {
	s.mu.Lock()
	fail := service == store.VaultService && account != "" && account == s.failGet
	onGet := s.onGet
	s.mu.Unlock()
	if onGet != nil {
		onGet(service)
	}
	if fail {
		return nil, errors.New("keychain read failed")
	}
	return s.Fake.Get(service, account)
}

func (s *spyKC) Delete(service, account string) error {
	if service == store.VaultService && !strings.HasPrefix(account, "_") {
		s.mu.Lock()
		s.vaultDeletes++
		s.mu.Unlock()
	}
	return s.Fake.Delete(service, account)
}

func (s *spyKC) Set(service, account string, b []byte) error {
	if strings.HasPrefix(account, "_") { // ccswap's own items (fingerprint, T3 key), not vault logins
		return s.Fake.Set(service, account, b)
	}
	s.mu.Lock()
	if service == store.VaultService {
		s.vaultSets++
		time.Sleep(s.slowVault)
	}
	if service == store.VaultService && (s.failVault > 0 || s.vaultSets == s.failAt) {
		s.failVault = max(s.failVault-1, 0)
		s.mu.Unlock()
		return errors.New("keychain write failed")
	}
	if service == claudecode.ServiceName("") {
		s.entrySets++
	}
	s.mu.Unlock()
	return s.Fake.Set(service, account, b)
}

func (s *spyKC) failVaultLeft() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failVault
}

func cred(at, rt string, exp time.Time) []byte {
	return fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference","user:profile"]}}`,
		at, rt, exp.UnixMilli())
}

type tenv struct {
	t    *testing.T
	home string
	lock string
	kc   *spyKC
	st   *store.Store
	clk  *clock
	api  *fakeAnthropic
	rec  *notify.Recorder
	d    *Daemon
}

// newTenv: a (priority 20) active in Claude Code's entry, b (priority 10) in the vault; both at 50%/10%.
func newTenv(t *testing.T) *tenv {
	t.Helper()
	kc := &spyKC{Fake: keychain.NewFake()}
	home := t.TempDir()
	st, err := store.Open(home, kc)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: t0}
	st.Now = clk.Now // the holder log's first entry; New wires the same clock
	err = st.Update(func(s *store.State) error {
		s.Accounts = []store.Account{
			{Name: "a", Email: "a@x", Priority: 20, Enabled: true, OAuthAccount: json.RawMessage(`{"emailAddress":"a@x"}`)},
			{Name: "b", Email: "b@x", Priority: 10, Enabled: true, OAuthAccount: json.RawMessage(`{"emailAddress":"b@x"}`)},
		}
		s.Active = "a"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(time.Hour)))
	kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(time.Hour)))
	cj := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(cj, []byte(`{"oauthAccount":{"emailAddress":"a@x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fa := &fakeAnthropic{
		emails:  map[string]string{"atA": "a@x", "atB": "b@x"},
		usage:   map[string]resp{"atA": usageBody(50), "atB": usageBody(10)},
		refresh: map[string]resp{},
	}
	srv := httptest.NewServer(fa)
	t.Cleanup(srv.Close)
	ac := &anthropic.Client{HTTP: srv.Client(), APIBase: srv.URL, TokenURL: srv.URL + "/token", ClientID: "test", UserAgent: "test", Now: clk.Now}
	sw := switcher.New(switcher.Deps{KC: kc, Store: st, Profiles: ac, Service: claudecode.ServiceName(""),
		KCAccount: claudecode.KeychainAccount(), ClaudeJSON: cj, Now: clk.Now})
	rec := &notify.Recorder{}
	sockDir, err := os.MkdirTemp("", "ccs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	d := New(Options{
		Config: config.Config{PollInterval: time.Minute,
			Thresholds: config.Thresholds{FiveHour: 95, Weekly: 98, Hysteresis: 10, MinDwell: 10 * time.Minute,
				CheckActive: time.Minute, CheckOthers: time.Minute}},
		Store: st, KC: kc, API: ac, Switcher: sw, Notifier: rec, Now: clk.Now,
		Home: home, Socket: filepath.Join(sockDir, "d.sock"), LockPath: filepath.Join(sockDir, "d.lock"), Version: "test", Host: "box",
	})
	return &tenv{t, home, filepath.Join(sockDir, "d.lock"), kc, st, clk, fa, rec, d}
}

func (e *tenv) poll() { e.d.Poll(context.Background()) }

func (e *tenv) entry() claudecode.Credentials {
	e.t.Helper()
	b, err := e.kc.Get(claudecode.ServiceName(""), claudecode.KeychainAccount())
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := claudecode.ParseCredentials(b)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *tenv) vault(name string) claudecode.Credentials {
	e.t.Helper()
	c, err := e.st.Creds(name)
	if err != nil {
		e.t.Fatal(name, err)
	}
	return c
}

func (e *tenv) account(name string) store.Account {
	e.t.Helper()
	a, ok := e.st.State().Find(name)
	if !ok {
		e.t.Fatal("no account", name)
	}
	return a
}

func (e *tenv) view(name string) api.Account {
	e.t.Helper()
	as, err := e.d.Accounts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range as {
		if a.Name == name {
			return a
		}
	}
	e.t.Fatal("no view", name)
	return api.Account{}
}

// note is a notification's text: key's text in the current language, capitalised as notify does.
func note(key string, args ...any) string { return capital(i18n.T(key, args...)) }

func (e *tenv) msgs(want ...string) {
	e.t.Helper()
	if got := e.rec.Messages(); !slices.Equal(got, want) {
		e.t.Fatalf("notifications %q, want %q", got, want)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for", what)
}

func TestPollRefreshesInactiveBeforeUsage(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	var seen string
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.usage["atB2"] = usageBody(10)
		f.onUsage = func(tok string) {
			if tok == "atB2" {
				c, _ := e.st.Creds("b")
				seen = c.OAuth.RefreshToken
			}
		}
	})
	e.poll()
	e.api.set(func(*fakeAnthropic) {})
	if seen != "rB2" {
		t.Fatalf("usage with the new token ran before the rotated pair was saved (vault had %q)", seen)
	}
	c := e.vault("b")
	if c.OAuth.AccessToken != "atB2" || c.OAuth.RefreshToken != "rB2" || !c.Expiry().Equal(t0.Add(8*time.Hour)) || len(c.OAuth.Scopes) != 2 {
		t.Fatalf("vault b %+v", c.OAuth)
	}
	if e.api.scope != "user:inference user:profile" {
		t.Fatalf("stored scopes not sent: %q", e.api.scope)
	}
	if e.api.count("usage atB") != 0 || e.api.count("refresh rA") != 0 || e.api.count("refresh rB") != 1 {
		t.Fatal(e.api.calls)
	}
	if v := e.view("b"); v.CheckedAt == nil || v.FiveHour == nil || v.FiveHour.Utilization != 10 {
		t.Fatalf("b usage %+v", v)
	}
	e.msgs()
}

// The active account and any vault copy of the entry's chain are never refreshed.
func TestRefreshNeverTouchesEntryChain(t *testing.T) {
	e := newTenv(t)
	// the entry holds b's chain but cannot be identified (profile down): State.Active stays "a".
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB", "rB", t0.Add(time.Hour)))
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(time.Minute)))
	e.kc.Fake.Set(store.VaultService, "a", cred("atA", "rA", t0.Add(time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		delete(f.emails, "atB")
		f.refresh["rA"] = resp{200, `{"access_token":"x","refresh_token":"y","expires_in":60}`}
		f.refresh["rB"] = resp{200, `{"access_token":"x","refresh_token":"y","expires_in":60}`}
	})
	e.poll()
	if e.api.count("refresh rA") != 0 || e.api.count("refresh rB") != 0 {
		t.Fatal("refreshed a chain that Claude Code owns:", e.api.calls)
	}
	if e.st.State().Active != "a" || e.entry().OAuth.RefreshToken != "rB" {
		t.Fatal("cannot-identify must keep the previous state")
	}
	e.msgs()
}

func TestRotatedPairKeptWhenSaveFails(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.usage["atB2"] = usageBody(10)
	})
	e.failVault(4)
	e.poll()
	if e.vault("b").OAuth.RefreshToken != "rB" || e.api.count("usage atB2") != 0 {
		t.Fatal("unsaved pair used or vault changed", e.api.calls)
	}
	if v := e.view("b"); v.Error == nil {
		t.Fatal("save failure not reported")
	}
	if _, err := e.d.Activate(context.Background(), "b", false); err != nil {
		t.Fatal("activate must flush the pending pair first:", err)
	}
	if e.entry().OAuth.RefreshToken != "rB2" || e.api.count("refresh rB") != 1 {
		t.Fatal("entry must get the rotated pair", e.entry().OAuth, e.api.calls)
	}
}

func TestPendingPairSavedNextTick(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.usage["atB2"] = usageBody(10)
	})
	e.failVault(4)
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB2" || e.api.count("refresh rB") != 1 || e.api.count("refresh rB2") != 0 {
		t.Fatal(c.OAuth, e.api.calls)
	}
	if v := e.view("b"); v.Error != nil {
		t.Fatal("error not cleared", *v.Error)
	}
}

func TestPendingPairNeverOverwritesNewerLogin(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	e.failVault(4)
	e.poll()
	// a re-login lands in the vault before the pending pair is saved
	e.kc.Fake.Set(store.VaultService, "b", cred("atB3", "rB3", t0.Add(8*time.Hour)))
	e.clk.Add(time.Minute)
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB3" {
		t.Fatal("pending pair overwrote a newer login", c.OAuth)
	}
}

func TestInvalidGrantMarksNeedsLogin(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	if !e.account("b").NeedsLogin || !e.view("b").NeedsLogin {
		t.Fatal("b must need a login")
	}
	if e.api.count("refresh rB") != 1 {
		t.Fatal(e.api.calls)
	}
	e.msgs(note("daemon.login_again", "b"))
}

func TestOtherRefreshErrorKeepsAccount(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) { f.refresh["rB"] = resp{500, `{}`} })
	e.poll()
	if e.account("b").NeedsLogin || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("transient refresh error must keep the account")
	}
	e.clk.Add(time.Minute)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	e.poll()
	if e.vault("b").OAuth.RefreshToken != "rB2" {
		t.Fatal("refresh not retried")
	}
	e.msgs()
}

func TestAutoSwitchOnThreshold(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
	e.poll()
	if e.entry().OAuth.RefreshToken != "rB" || e.st.State().Active != "b" {
		t.Fatal("not switched to b")
	}
	if e.vault("a").OAuth.RefreshToken != "rA" {
		t.Fatal("a not saved to the vault")
	}
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.hit_limit", "a")))
	if v := e.view("a"); !v.Exhausted || v.ExhaustedReason == nil || *v.ExhaustedReason != "five_hour" || v.Active {
		t.Fatalf("view a %+v", v)
	}
}

func TestAutoOffNoSwitch(t *testing.T) {
	e := newTenv(t)
	if err := e.d.SetAuto(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
	e.poll()
	if e.entry().OAuth.RefreshToken != "rA" || e.kc.entrySets != 0 {
		t.Fatal("switched with auto off")
	}
	e.msgs()
}

func run(t *testing.T, e *tenv) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error("Run:", err)
		}
	})
	waitFor(t, "first tick", func() bool {
		st, _ := e.d.Status(context.Background())
		return st.LastPollAt != nil
	})
}

// TestRunStopWhileTheServerStopsFirst: when Run is cancelled during a tick, the API server stops too, and the
// select after the tick finds both ready and picks either; Run returns nil either way. Each round makes both
// ready (the server stops and cancels before the tick may start), so a wrong pick fails half the rounds.
func TestRunStopWhileTheServerStopsFirst(t *testing.T) {
	t.Cleanup(func() { serveAPI = serve })
	for range 40 {
		e := newTenv(t)
		ctx, cancel := context.WithCancel(context.Background())
		e.d.tick.Lock()
		serveAPI = func(_ context.Context, ln net.Listener, _ Core) error {
			cancel()
			ln.Close()
			e.d.tick.Unlock()
			return nil
		}
		if err := e.d.Run(ctx); err != nil {
			t.Fatal("a cancelled Run must stop without an error:", err)
		}
	}
}

func TestRunFailsWhenTheServerStopsOnItsOwn(t *testing.T) {
	t.Cleanup(func() { serveAPI = serve })
	serveAPI = func(context.Context, net.Listener, Core) error { return nil }
	if err := newTenv(t).d.Run(context.Background()); err == nil {
		t.Fatal("a server that stopped by itself is a failure")
	}
}

func TestSignalLimitSwitchesNow(t *testing.T) {
	e := newTenv(t)
	run(t, e)
	if e.entry().OAuth.RefreshToken != "rA" {
		t.Fatal("switched before the signal")
	}
	err := e.d.Signal(context.Background(), map[string]any{
		"hook_event_name":        "StopFailure",
		"error":                  "rate_limit",
		"last_assistant_message": "You've hit your usage limit · resets 3pm (Europe/Moscow)",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "switch to b", func() bool {
		b, _ := e.kc.Get(claudecode.ServiceName(""), claudecode.KeychainAccount())
		return strings.Contains(string(b), `"rB"`)
	})
	waitFor(t, "notification", func() bool { return len(e.rec.Messages()) == 1 })
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.hit_limit", "a")))
	if r := e.view("a").ExhaustedReason; r == nil || *r != "limit_hit" {
		t.Fatal("a must be marked limit-hit", r)
	}
}

func TestSwitchAsksTheNewHolderAtOnce(t *testing.T) { // not on the ticker's next beat, a minute here
	ctx := context.Background()
	for _, auto := range []bool{false, true} {
		e := newTenv(t)
		th := e.d.th()
		th.CheckOthers = 30 * time.Minute
		e.d.thr.Store(&th)
		run(t, e)
		e.clk.Add(2 * time.Minute) // past b's pace once it is in use, far from its pace before
		if auto {
			e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
			e.d.SetAuto(ctx, true) // its tick finds a out and switches to b
		} else if _, err := e.d.Activate(ctx, "b", false); err != nil {
			t.Fatal(err)
		}
		waitFor(t, fmt.Sprintf("auto %v: b asked once in use", auto), func() bool { return e.api.count("usage atB") == 2 })
	}
}

func TestSignalOtherErrorIgnored(t *testing.T) {
	e := newTenv(t)
	e.poll()
	for _, p := range []map[string]any{
		{"hook_event_name": "StopFailure", "error": "overloaded", "last_assistant_message": "usage limit, resets 3pm"},
		{"hook_event_name": "StopFailure", "error": "rate_limit", "last_assistant_message": "Request rejected (429) · temporary capacity"},
		{"error": 5, "last_assistant_message": []any{1}},
		{},
	} {
		if err := e.d.Signal(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Add(time.Minute)
	e.poll()
	if e.entry().OAuth.RefreshToken != "rA" || e.view("a").Exhausted {
		t.Fatal("non-limit signal switched or marked the account")
	}
	e.msgs()
}

func TestSignalRightAfterSwitchSparesNewAccount(t *testing.T) {
	e := newTenv(t)
	if _, err := e.d.Activate(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(10 * time.Second)
	// a late signal from a session that failed on a must not burn b
	e.d.Signal(context.Background(), map[string]any{"error": "rate_limit", "last_assistant_message": "Лимит исчерпан, сброс в 15:00"})
	if e.view("b").Exhausted {
		t.Fatal("b marked by a signal that belongs to a")
	}
	e.clk.Add(2 * time.Minute)
	e.d.Signal(context.Background(), map[string]any{"error": "rate_limit", "last_assistant_message": "Лимит исчерпан, сброс в 15:00"})
	if r := e.view("b").ExhaustedReason; r == nil || *r != "limit_hit" {
		t.Fatal("Russian limit wording not recognised", r)
	}
}

// Observed: Claude Code runs hooks without CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN,
// so the hook cannot tell a session on its own token from one on the switched entry.
func TestSignalFromAnotherAccountIgnored(t *testing.T) {
	opus := func(h5, op float64) resp {
		return resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v},"seven_day":{"utilization":10},"seven_day_opus":{"utilization":%v}}`, h5, op)}
	}
	const weekly = "You've hit your weekly limit · resets 9pm (Europe/Moscow)"
	failed, atLimit := resp{500, `{}`}, resp{200, `{"five_hour":{"utilization":50},"seven_day":{"utilization":90}}`}
	for _, tc := range []struct {
		usage      resp
		every, age time.Duration // check_active, and how old a's usage is at the signal
		ask        resp          // what asking a at the signal gets; none: the usage again
		msg        string
		hit        bool
	}{
		{opus(50, 5), time.Minute, 0, resp{}, weekly, false},
		{opus(50, 5), time.Minute, 0, resp{}, "You've hit your session limit · resets 11:40pm (Europe/Moscow)", false},
		{opus(50, 5), time.Minute, 0, resp{}, "You've hit your Opus limit · resets Oct 20, 1am (Europe/Moscow)", false},
		{opus(90, 5), time.Minute, 0, resp{}, "You've hit your session limit · resets 11:40pm (Europe/Moscow)", true},
		{opus(50, 85), time.Minute, 0, resp{}, "You've hit your Opus limit · resets Oct 20, 1am (Europe/Moscow)", true},
		{opus(50, 5), time.Minute, 4 * time.Hour, resp{}, weekly, true},                     // Claude Code's token expired meanwhile: a can't be asked
		{opus(50, 5), time.Minute, 2 * time.Minute, failed, weekly, false},                  // within one check of the account in use and a tick: not asked
		{opus(50, 5), time.Minute, 2*time.Minute + time.Second, failed, weekly, true},       // older, and asking at once failed
		{opus(50, 5), time.Minute, 2*time.Minute + time.Second, resp{}, weekly, false},      // older: asked at once, the answer decides
		{opus(50, 5), time.Minute, 2*time.Minute + time.Second, atLimit, weekly, true},      // and a genuine limit still counts
		{opus(50, 5), 5 * time.Minute, 6 * time.Minute, failed, weekly, false},              // within one check and a tick
		{opus(50, 5), 30 * time.Minute, 6 * time.Minute, failed, weekly, false},             // within StaleAfter and a tick
		{opus(50, 5), 30 * time.Minute, 6*time.Minute + time.Second, failed, weekly, true},  // never older than StaleAfter and a tick
		{opus(50, 5), 30 * time.Minute, 6*time.Minute + time.Second, resp{}, weekly, false}, // asked at once, whatever check_active says
		{opus(50, 5), time.Minute, 0, resp{}, "You've hit your usage credit limit · resets 9pm (Europe/Moscow)", true},
	} {
		e := newTenv(t)
		th := e.d.th()
		th.CheckActive = tc.every
		e.d.thr.Store(&th)
		e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = tc.usage })
		e.poll()
		if tc.ask.status != 0 {
			e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = tc.ask })
		}
		e.clk.Add(tc.age)
		e.d.Signal(context.Background(), map[string]any{"error": "rate_limit", "last_assistant_message": tc.msg})
		if r := e.view("a").ExhaustedReason; (r != nil && *r == "limit_hit") != tc.hit {
			t.Errorf("%s (usage %v old, checked every %v, asked: %d): reason %v, want limit hit %v", tc.msg, tc.age, tc.every, tc.ask.status, r, tc.hit)
		}
	}
}

func TestSignalSurvivesTickJitter(t *testing.T) { // a tick a signal requests asks off the ticker's phase
	e := newTenv(t)
	th := e.d.th()
	th.CheckActive, th.CheckOthers = 5*time.Minute, 30*time.Minute // the defaults, with a one-minute poll
	e.d.thr.Store(&th)
	at := func(d time.Duration) { e.clk.Add(t0.Add(d).Sub(e.clk.Now())) }
	weekly := map[string]any{"error": "rate_limit", "last_assistant_message": "You've hit your weekly limit · resets 9pm (Europe/Moscow)"}
	for m := range 5 {
		at(time.Duration(m) * time.Minute)
		e.poll()
	}
	at(4*time.Minute + 40*time.Second) // a session on its own CLAUDE_CODE_OAUTH_TOKEN (e.g. an agent runner) hits its limit
	e.d.Signal(context.Background(), weekly)
	e.poll() // the tick it requests asks a, so a's next ask is 5 min 20 s later
	for m := 5; m <= 9; m++ {
		at(time.Duration(m) * time.Minute)
		e.poll()
	}
	at(9*time.Minute + 41*time.Second) // a's numbers are 5 min 1 s old
	n := e.api.count("usage atA")
	e.d.Signal(context.Background(), weekly)
	if r := e.view("a").ExhaustedReason; r != nil {
		t.Fatalf("a, at 10%% of its week, marked %s by another account's signal", *r)
	}
	if e.api.count("usage atA") != n {
		t.Fatal("asked a though its numbers were recent enough")
	}
}

func TestStaleSignalAsksTheHolderFirst(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	weekly := map[string]any{"error": "rate_limit", "last_assistant_message": "You've hit your weekly limit · resets 9pm (Europe/Moscow)"}
	e.poll()
	e.clk.Add(3 * time.Minute) // no tick meanwhile: a's numbers are too old to judge by
	n := e.api.count("usage atA")
	e.d.Signal(ctx, weekly)
	if e.api.count("usage atA") != n+1 || e.view("a").Exhausted {
		t.Fatal("a not asked at once, or marked though its fresh numbers show 10% of its week")
	}
	e.d.Signal(ctx, weekly)
	if e.api.count("usage atA") != n+1 || e.view("a").Exhausted {
		t.Fatal("a second signal asked again though the first one's answer is fresh")
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"], f.retryAfter = resp{429, `{}`}, "600" })
	e.clk.Add(time.Minute)
	e.poll() // a 429: nobody asks a for 10 minutes
	e.clk.Add(2 * time.Minute)
	n = e.api.count("usage atA")
	e.d.Signal(ctx, weekly)
	if e.api.count("usage atA") != n {
		t.Fatal("a signal asked a before its Retry-After")
	}
	if r := e.view("a").ExhaustedReason; r == nil || *r != "limit_hit" {
		t.Fatal("a signal on numbers too old to judge by, with no ask allowed, must count", r)
	}
}

func TestStartupReconcile(t *testing.T) {
	e := newTenv(t)
	e.st.Update(func(s *store.State) error { s.Accounts[1].NeedsLogin = true; return nil })
	e.kc.Fake.Delete(store.VaultService, "b")
	e.kc.Fake.Set(store.VaultService, "a", cred("atA", "rA", t0.Add(time.Hour)))
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB", "rB", t0.Add(time.Hour)))
	e.d.SetAuto(context.Background(), false) // a is higher and recovered: keep the test about reconcile
	run(t, e)
	st := e.st.State()
	if st.Active != "b" || e.account("b").NeedsLogin {
		t.Fatalf("active %q, b needs login %v", st.Active, e.account("b").NeedsLogin)
	}
	if !e.view("b").Active || e.kc.entrySets != 0 {
		t.Fatal("reconcile must not write the entry")
	}
	e.msgs()
}

func TestUnknownActivePausesAuto(t *testing.T) {
	e := newTenv(t)
	off := false
	if _, err := e.d.Patch(context.Background(), "a", api.AccountPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atX", "rX", t0.Add(time.Hour)))
	e.api.set(func(f *fakeAnthropic) { f.emails["atX"] = "x@x" })
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	if e.kc.entrySets != 0 || e.entry().OAuth.RefreshToken != "rX" {
		t.Fatal("unknown login overwritten")
	}
	if st, _ := e.d.Status(context.Background()); !st.UnknownActive || st.Active != "" {
		t.Fatalf("status %+v", st)
	}
	if e.api.count("usage atX") != 0 {
		t.Fatal("stranger's usage attributed to an account")
	}
	e.msgs(note("daemon.unknown_active"))
	if _, err := e.d.Activate(context.Background(), "b", true); !errors.Is(err, switcher.ErrUnknownActive) {
		t.Fatal(err)
	}
	if _, err := e.d.Adopt(context.Background(), api.AdoptRequest{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.d.Status(context.Background()); st.UnknownActive || st.Active != "x" {
		t.Fatalf("after adopt %+v", st)
	}
	if !e.account("a").NeedsLogin {
		t.Fatal("a's login was overwritten by the adopted one: needs login")
	}
	e.msgs(note("daemon.unknown_active"), note("daemon.login_again", "a"))
}

func TestUsageErrorKeepsLastData(t *testing.T) {
	e := newTenv(t)
	e.poll()
	good := e.view("b")
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = resp{500, `{}`} })
	e.clk.Add(time.Minute)
	e.poll()
	v := e.view("b")
	if v.Error == nil || v.FiveHour == nil || v.FiveHour.Utilization != 10 || !v.CheckedAt.Equal(*good.CheckedAt) {
		t.Fatalf("b after 500: %+v", v)
	}
	// b goes stale while a runs out: b is no target, one notice at most.
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
	for _, step := range []time.Duration{5 * time.Minute, time.Minute, time.Minute} {
		e.clk.Add(step)
		e.poll()
	}
	if e.entry().OAuth.RefreshToken != "rA" || e.kc.entrySets != 0 {
		t.Fatal("switched to an account with stale data")
	}
	e.msgs() // b is unknown, not exhausted: "all at the limit" would be a guess
}

func TestFailedAskRetriedNextTick(t *testing.T) { // only an answer or a 429 waits out the pace
	for _, fail := range []string{"500", "no connection"} {
		e := newTenv(t)
		th := e.d.th()
		th.CheckOthers = 5 * time.Minute
		e.d.thr.Store(&th)
		e.poll()
		e.api.set(func(f *fakeAnthropic) {
			f.usage["atB"] = resp{500, `{}`}
			f.onUsage = func(tok string) {
				if tok == "atB" && fail == "no connection" {
					panic(http.ErrAbortHandler)
				}
			}
		})
		e.clk.Add(5 * time.Minute)
		e.poll()
		n := e.api.count("usage atB") // net/http retries a GET on a dropped connection
		if v := e.view("b"); n < 2 || v.Error == nil {
			t.Fatalf("%s: b asked %d times, view %+v", fail, n, v)
		}
		e.api.set(func(f *fakeAnthropic) { f.usage["atB"], f.onUsage = usageBody(20), nil })
		e.clk.Add(time.Minute)
		e.poll()
		if e.api.count("usage atB") != n+1 || e.view("b").FiveHour.Utilization != 20 {
			t.Fatalf("%s: b not asked again on the next tick", fail)
		}
	}
}

func TestUsage429SlowsThePace(t *testing.T) {
	e := newTenv(t)
	e.poll() // b: a real answer cached
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = resp{429, `{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`}
	})
	e.clk.Add(time.Minute)
	e.poll()
	n := e.api.count("usage atB")
	e.clk.Add(time.Minute) // within the doubled pace: neither ticks nor T3 ask again
	e.poll()
	st, body, err := e.d.UsageRaw(context.Background(), "b", 0)
	if err != nil || st != 200 || !strings.Contains(string(body), `"utilization":10`) {
		t.Fatalf("T3 must keep the last real answer: %d %s %v", st, body, err)
	}
	if got := e.api.count("usage atB"); got != n {
		t.Fatalf("asked %d more times within the doubled pace", got-n)
	}
	if v := e.view("b"); v.Error == nil || !strings.HasPrefix(*v.Error, i18n.T("daemon.rate_limited", "")) || v.FiveHour == nil {
		t.Fatalf("view %+v", v)
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = usageBody(20) })
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atB") != n+1 || e.view("b").FiveHour.Utilization != 20 || e.view("b").Error != nil {
		t.Fatal("not asked again once the doubled pace passed")
	}
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atB") != n+1 {
		t.Fatal("an answer must not bring back the pace that drew the 429")
	}
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atB") != n+2 {
		t.Fatal("not asked at the slower pace")
	}
}

func TestUsageRawCachesBody(t *testing.T) {
	e := newTenv(t)
	e.poll()
	st, body, err := e.d.UsageRaw(context.Background(), "b", time.Minute)
	if err != nil || st != 200 || string(body) != usageBody(10).body {
		t.Fatal(st, string(body), err)
	}
	if e.api.count("usage atB") != 1 {
		t.Fatal("fresh cache must be served")
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = resp{429, `{"error":"slow down"}`} })
	e.clk.Add(2 * time.Minute)
	st, body, err = e.d.UsageRaw(context.Background(), "b", time.Minute)
	if err != nil || st != 429 || string(body) != `{"error":"slow down"}` {
		t.Fatal(st, string(body), err)
	}
	if _, _, err := e.d.UsageRaw(context.Background(), "nobody", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if n := len(e.d.StoredAccounts()); n != 2 {
		t.Fatal(n)
	}
}

func TestActivateReturnsWarning(t *testing.T) {
	e := newTenv(t)
	e.st.Update(func(s *store.State) error { s.Accounts[1].OAuthAccount = nil; return nil })
	st, err := e.d.Activate(context.Background(), "b", false)
	if err != nil || st.Active != "b" || st.Warning == nil {
		t.Fatalf("%+v %v", st, err)
	}
	if e.st.State().LastSwitchAt.IsZero() {
		t.Fatal("manual switch must record LastSwitchAt")
	}
}

func TestRemoveAndPatch(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	if err := e.d.Remove(ctx, "a"); !errors.Is(err, ErrRemoveActive) {
		t.Fatal(err)
	}
	if err := e.d.Remove(ctx, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	p := 99
	if a, err := e.d.Patch(ctx, "b", api.AccountPatch{Priority: &p}); err != nil || a.Priority != 99 || !a.Enabled {
		t.Fatal(a, err)
	}
	if _, err := e.d.Patch(ctx, "zz", api.AccountPatch{Priority: &p}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.d.Remove(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Creds("b"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("vault item left behind")
	}
	if _, ok := e.st.State().Find("b"); ok {
		t.Fatal("account left behind")
	}
}

// The entry cannot be identified and Claude Code already rotated b's chain, so vault[b]
// holds the consumed token of the live chain while State.Active still says a.
func TestUnidentifiedEntrySkipsRefresh(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(time.Hour)))
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(time.Minute)))
	e.poll() // profile of atB2 is 401
	if n := e.api.count("refresh rB"); n != 0 {
		t.Fatal("refreshed while the entry is unidentified:", e.api.calls)
	}
	if e.api.count("usage atB2") != 0 {
		t.Fatal("unidentified entry's usage attributed to an account")
	}
	if e.account("b").NeedsLogin {
		t.Fatal("b falsely needs login")
	}
	e.msgs()
}

// The holder is skipped by name even when State.Active could not be saved.
func TestRefreshSkipsHolderWhenStateSaveFails(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB2", "rB2", t0.Add(time.Hour)))
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(time.Minute)))
	e.api.set(func(f *fakeAnthropic) { f.emails["atB2"] = "b@x"; f.usage["atB2"] = usageBody(10) })
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.home, 0o700) })
	e.poll()
	if e.st.State().Active != "a" {
		t.Fatal("state save was expected to fail")
	}
	if e.api.count("refresh rB") != 0 {
		t.Fatal("refreshed the entry holder's stale vault copy:", e.api.calls)
	}
	if v := e.view("b"); v.CheckedAt == nil {
		t.Fatal("identified holder's usage must come from the entry")
	}
}

func TestSaveRetriedWithinTick(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.usage["atB2"] = usageBody(10)
	})
	e.failVault(1)
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB2" || e.api.count("usage atB2") != 1 {
		t.Fatal("transient save failure not retried within the tick", c.OAuth, e.api.calls)
	}
}

func TestShutdownFlushesPending(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	e.failVault(4)
	e.poll()
	e.failVault(4) // the startup tick of Run fails to save as well
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.d.Run(ctx) }()
	waitFor(t, "first tick", func() bool {
		st, _ := e.d.Status(context.Background())
		return st.LastPollAt != nil && e.kc.failVaultLeft() == 0
	})
	if e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("expected the pair to be still pending")
	}
	cancel() // keychain works again
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB2" {
		t.Fatal("pending pair dropped on shutdown", c.OAuth)
	}
}

func TestReconcileMarksLostLogin(t *testing.T) {
	e := newTenv(t)
	// manual `claude /login` as b: a's chain is gone, ccswap has no copy of it
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB", "rB9", t0.Add(time.Hour)))
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	if e.st.State().Active != "b" || !e.account("a").NeedsLogin {
		t.Fatal("a must need a login", e.st.State())
	}
	e.msgs(note("daemon.login_again", "a"))
}

func TestAllExhaustedNoticePerEpisode(t *testing.T) {
	e := newTenv(t)
	reset := t0.Add(2*time.Hour - 20*time.Second) // shown as the rounded minute
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageAt(96, reset); f.usage["atB"] = usageAt(96, reset) })
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	want := note("daemon.all_limited", clockText(t0.Add(2*time.Hour), t0), "a")
	e.msgs(want)
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageAt(50, reset) })
	e.clk.Add(time.Minute)
	e.poll()
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageAt(96, reset) })
	e.clk.Add(time.Minute)
	e.poll()
	e.msgs(want, want)
}

func TestFlushDropForActiveIsLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.emails["atB9"] = "b@x"
	})
	e.failVault(4)
	e.poll()
	// b becomes the active login by hand: its unsaved pair is no longer needed
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB9", "rB9", t0.Add(time.Hour)))
	e.clk.Add(time.Minute)
	e.poll()
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	if !strings.Contains(logs, "account=b") || !strings.Contains(logs, "unsaved") {
		t.Fatal("dropped pair not logged:\n" + logs)
	}
	for _, secret := range []string{"rB2", "atB2", "rB9", "atB9"} {
		if strings.Contains(logs, secret) {
			t.Fatal("token in logs")
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestLockIsPerUserNotPerHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CCSWAP_HOME", t.TempDir())
	cfgDir, _ := os.UserConfigDir()
	if got := New(Options{}).o.LockPath; got != filepath.Join(cfgDir, "ccswap", "daemon.lock") {
		t.Fatal("lock must be per user, outside temp dirs and CCSWAP_HOME:", got)
	}
	e1, e2 := newTenv(t), newTenv(t)
	e2.d.o.LockPath = e1.lock // another CCSWAP_HOME, same user
	run(t, e1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e2.d.Run(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("a second daemon with another home must not start:", err)
	}
}

func TestIdleEntryIdentifiedByFingerprint(t *testing.T) {
	e := newTenv(t)
	e.poll() // identifies a by profile
	// Claude Code idle: its access token expired, the profile answers 401; vault upkeep must go on
	e.api.set(func(f *fakeAnthropic) {
		delete(f.emails, "atA")
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(time.Hour+5*time.Minute)))
	e.clk.Add(time.Hour)
	e.poll()
	if e.api.count("profile atA") != 1 || e.api.count("refresh rB") != 1 || e.vault("b").OAuth.RefreshToken != "rB2" {
		t.Fatal("vault upkeep stalled while Claude Code is idle:", e.api.calls)
	}
}

func TestSlowSaveNotRetried(t *testing.T) {
	old := saveSlow
	saveSlow = 30 * time.Millisecond
	t.Cleanup(func() { saveSlow = old })
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	e.kc.mu.Lock()
	e.kc.failVault, e.kc.slowVault = 4, 60*time.Millisecond // e.g. `security` waiting on an unlock prompt
	e.kc.mu.Unlock()
	e.poll()
	e.kc.mu.Lock()
	n := e.kc.vaultSets
	e.kc.mu.Unlock()
	if n != 1 {
		t.Fatalf("a slow failed save must not be retried under the lock, got %d attempts", n)
	}
}

func TestSignalDoesNotExtendLimitHit(t *testing.T) {
	e := newTenv(t)
	e.poll()
	limit := map[string]any{"error": "rate_limit", "last_assistant_message": "You've hit your limit · resets 3pm"}
	e.d.Signal(context.Background(), limit)
	e.clk.Add(10 * time.Minute)
	e.d.Signal(context.Background(), limit) // StopFailure of another session or a retry
	e.d.mu.Lock()
	until := e.d.usage["a"].limitHitUntil
	e.d.mu.Unlock()
	if !until.Equal(t0.Add(time.Hour)) {
		t.Fatal("an active limit-hit was extended:", until)
	}
}

func addC(t *testing.T, e *tenv) {
	t.Helper()
	e.st.Update(func(s *store.State) error {
		s.Accounts = append(s.Accounts, store.Account{Name: "c", Email: "c@x", Priority: 5, Enabled: true,
			OAuthAccount: json.RawMessage(`{"emailAddress":"c@x"}`)})
		return nil
	})
	e.kc.Fake.Set(store.VaultService, "c", cred("atC", "rC", t0.Add(3*time.Hour)))
	e.api.set(func(f *fakeAnthropic) { f.emails["atC"] = "c@x"; f.usage["atC"] = usageBody(10) })
}

func TestAllExhaustedSwitchWithStaleOtherWording(t *testing.T) { // stale c is unknown, not exhausted, in the switch notice too
	e := newTenv(t)
	addC(t, e)
	e.poll()
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atC"] = resp{500, `{}`}
		f.usage["atA"] = usageAt(96, t0.Add(2*time.Hour))
		f.usage["atB"] = usageAt(96, t0.Add(30*time.Minute))
	})
	e.clk.Add(6 * time.Minute) // c is stale, not known to be at its limit
	e.poll()
	if e.entry().OAuth.RefreshToken != "rB" {
		t.Fatal("expected the switch to b, which resets first")
	}
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.hit_limit", "a")))
}

func TestDisabledHolderNotice(t *testing.T) {
	e := newTenv(t)
	off := false
	for _, n := range []string{"a", "b"} {
		if _, err := e.d.Patch(context.Background(), n, api.AccountPatch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
	}
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	e.msgs(note("daemon.active_disabled_no_other", "a"))
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestT3ListenerServesAndStops(t *testing.T) {
	e := newTenv(t)
	addr := freeAddr(t)
	e.d.o.Config.T3 = config.T3{Enabled: true, Listen: addr}
	run(t, e)
	key, err := t3.Key(e.kc, t3.KeyAccount)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/v0/management/auth-files", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	waitFor(t, "t3 up", func() bool {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	if st, _ := e.d.Status(context.Background()); st.Listen == nil || *st.Listen != addr {
		t.Fatalf("status must name the address the listener holds: %v", st.Listen)
	}
}

func TestT3BindFailureKeepsDaemon(t *testing.T) {
	e := newTenv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	e.d.o.Config.T3 = config.T3{Enabled: true, Listen: busy.Addr().String()}
	run(t, e)
	waitFor(t, "first tick", func() bool {
		st, _ := e.d.Status(context.Background())
		return st.LastPollAt != nil
	})
	if st, _ := e.d.Status(context.Background()); st.Listen != nil {
		t.Fatal("no listener, yet status names one:", *st.Listen)
	}
}

// Claude Code's own read-modify-write wrote a's login back over the switch to b.
func TestReconcileAfterRevertKeepsParkedTarget(t *testing.T) {
	e := newTenv(t)
	if _, err := e.d.Activate(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(time.Hour)))
	e.clk.Add(3 * time.Minute)
	e.poll()
	if e.st.State().Active != "a" || e.account("b").NeedsLogin {
		t.Fatal("reconcile must follow the entry back to a without losing b", e.st.State())
	}
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB" {
		t.Fatal("b's parked login must survive the revert", c.OAuth)
	}
	if _, err := e.st.Creds("a"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("a's vault copy of the chain in the entry must go:", err)
	}
	e.msgs()
}

func TestReconcileDropsParkedCopy(t *testing.T) {
	e := newTenv(t)
	if _, err := e.d.Activate(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.poll() // right after the switch Claude Code may still write a back: keep the copy
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB" {
		t.Fatal("parked copy dropped too early", c.OAuth)
	}
	e.clk.Add(3 * time.Minute)
	e.poll()
	if _, err := e.st.Creds("b"); !errors.Is(err, keychain.ErrNotFound) || e.st.State().Active != "b" {
		t.Fatal("parked copy of the identified holder must go:", err)
	}
	if e.vault("a").OAuth.RefreshToken != "rA" {
		t.Fatal("a's login lost")
	}
}

func TestRemoveForgetsFingerprint(t *testing.T) {
	e := newTenv(t)
	if _, err := e.d.Activate(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	e.st.Update(func(s *store.State) error { s.Active = "a"; return nil }) // stale state: b removable
	if err := e.d.Remove(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.kc.Get(store.VaultService, "_fingerprint"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("fingerprint of the removed account kept:", err)
	}
}

func TestRefreshWithoutAccessTokenSavesRotatedToken(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"refresh_token":"rB2"}`}
		f.refresh["rB2"] = resp{200, `{"access_token":"atB3","refresh_token":"rB3","expires_in":28800}`}
	})
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB2" || e.account("b").NeedsLogin {
		t.Fatal("the rotated refresh token must be saved although the response had no access token", c.OAuth)
	}
	e.clk.Add(time.Minute)
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB3" || c.OAuth.AccessToken != "atB3" {
		t.Fatal("next tick must refresh with the saved token", c.OAuth, e.api.calls)
	}
}

func TestUsageRawServesLastBodyWhenIdle(t *testing.T) {
	e := newTenv(t)
	e.poll()
	// Claude Code idle: the entry's access token expired, the chain did not rotate
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(30*time.Second)))
	e.clk.Add(2 * time.Minute)
	st, body, err := e.d.UsageRaw(context.Background(), "a", time.Minute)
	if err != nil || st != 200 || string(body) != usageBody(50).body {
		t.Fatal("T3 must get the last body, not lose the account:", st, string(body), err)
	}
	addC(t, e)
	e.kc.Fake.Set(store.VaultService, "c", cred("atC", "rC", t0)) // expired, never fetched
	if _, _, err := e.d.UsageRaw(context.Background(), "c", time.Minute); err == nil {
		t.Fatal("nothing cached and no token: an error")
	}
}

func TestLogRotation(t *testing.T) {
	old := logMax
	logMax = 64
	t.Cleanup(func() { logMax = old })
	e := newTenv(t)
	p := filepath.Join(e.home, "daemon.log")
	big := strings.Repeat("x", 100)
	os.WriteFile(p, []byte(big), 0o600)
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0) // launchd's descriptor
	defer f.Close()
	e.poll()
	if b, _ := os.ReadFile(p + ".1"); string(b) != big {
		t.Fatalf("daemon.log.1 = %q", b)
	}
	f.WriteString("next\n")
	if b, _ := os.ReadFile(p); string(b) != "next\n" {
		t.Fatalf("daemon.log must be truncated in place and keep taking appends: %q", b)
	}
	e.poll()
	if b, _ := os.ReadFile(p); string(b) != "next\n" {
		t.Fatal("rotated under the limit")
	}
}

func TestRepeatedLinesLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := newTenv(t)
	addC(t, e)
	// a must switch to b but Claude Code is about to refresh its token; c's usage fails
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96); f.usage["atC"] = resp{500, `{}`} })
	for range 3 {
		e.poll()
		e.clk.Add(time.Minute)
	}
	// then the profile goes away for an unknown login
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atX", "rX", t0.Add(time.Hour)))
	for range 3 {
		e.poll()
		e.clk.Add(time.Minute)
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for _, line := range []string{"auto-switch postponed", "msg=usage account=c", "reconcile: cannot identify"} {
		if n := strings.Count(logs, line); n != 1 {
			t.Errorf("%q logged %d times:\n%s", line, n, logs)
		}
	}
}

func TestIdleHolderExplainsMissingData(t *testing.T) {
	e := newTenv(t)
	e.poll()
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(30*time.Second)))
	e.clk.Add(time.Minute)
	e.poll()
	if v := e.view("a"); !v.Active || v.Error == nil || *v.Error != i18n.T("daemon.claude_idle") || v.FiveHour == nil {
		t.Fatalf("the idle active account must keep its data and say why it is not fresh: %+v", v)
	}
}

func TestFailedStateWriteStillParksTarget(t *testing.T) {
	e := newTenv(t)
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	st, err := e.d.Activate(context.Background(), "b", false)
	os.Chmod(e.home, 0o700)
	if err != nil || st.Warning == nil || e.st.State().Active != "a" {
		t.Fatalf("the switch happened: a warning, no error: %v %+v", err, st.Warning)
	}
	e.clk.Add(time.Minute) // within the 2 minutes of the landed write, though LastSwitchAt was never stamped
	e.poll()
	if c := e.vault("b"); c.OAuth.RefreshToken != "rB" {
		t.Fatal("vault[b] dropped before the parked-copy wait", c.OAuth)
	}
}

func TestRefreshVaultCopyWithoutAccessTokenRegardlessOfExpiry(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("", "rB", t0.Add(8*time.Hour)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		f.usage["atB2"] = usageBody(10)
	})
	e.poll()
	if c := e.vault("b"); c.OAuth.AccessToken != "atB2" || c.OAuth.RefreshToken != "rB2" {
		t.Fatal(c.OAuth, e.api.calls)
	}
	e.msgs()
}

func TestLandedSwitchWithFailedStateWrite(t *testing.T) {
	e := newTenv(t)
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(time.Minute)
	switchedAt := e.clk.Now()
	st, err := e.d.Activate(context.Background(), "b", false)
	os.Chmod(e.home, 0o700)
	if err != nil || st.Active != "b" || st.Warning == nil || e.st.State().Active != "a" {
		t.Fatalf("%+v %v %s", st, err, e.st.State().Active)
	}
	e.clk.Add(3 * time.Minute)
	e.poll()
	if s := e.st.State(); s.Active != "b" || !s.LastSwitchAt.Equal(switchedAt) {
		t.Fatalf("reconcile must stamp the real switch time: %+v want %v", s, switchedAt)
	}
	e.msgs()
}

func TestSetThresholdsSavesAndApplies(t *testing.T) {
	e := newTenv(t)
	e.poll()
	if e.view("a").Exhausted {
		t.Fatal("a at 50% is not exhausted at 95%")
	}
	ctx := context.Background()
	got, err := e.d.SetThresholds(ctx, api.Thresholds{FiveHour: 50, SevenDay: 97, Hysteresis: 5, MinDwellSeconds: 120,
		CheckActiveSeconds: 300, CheckOthersSeconds: 1800})
	if err != nil {
		t.Fatal(err)
	}
	want := api.Thresholds{FiveHour: 50, SevenDay: 97, Hysteresis: 5, MinDwellSeconds: 120, CheckActiveSeconds: 300, CheckOthersSeconds: 1800}
	if st, _ := e.d.Status(ctx); got != want || st.Thresholds != want {
		t.Fatalf("got %+v, status %+v", got, st.Thresholds)
	}
	if v := e.view("a"); !v.Exhausted || v.ExhaustedReason == nil || *v.ExhaustedReason != "five_hour" {
		t.Fatalf("new threshold not applied: %+v", v)
	}
	c, err := config.Load(e.home)
	if err != nil || c.Thresholds != (config.Thresholds{FiveHour: 50, Weekly: 97, Hysteresis: 5, MinDwell: 2 * time.Minute,
		CheckActive: 5 * time.Minute, CheckOthers: 30 * time.Minute}) {
		t.Fatal("config.toml", c.Thresholds, err)
	}
}

func TestSetThresholdsRejectsInvalid(t *testing.T) {
	e := newTenv(t)
	for _, th := range []api.Thresholds{
		{FiveHour: 0, SevenDay: 98, Hysteresis: 10},
		{FiveHour: 95, SevenDay: 101, Hysteresis: 10},
		{FiveHour: 95, SevenDay: 98, Hysteresis: 95},
		{FiveHour: 95, SevenDay: 98, Hysteresis: 10, MinDwellSeconds: -1},
		{FiveHour: 95, SevenDay: 98, Hysteresis: 10, MinDwellSeconds: math.MaxInt64},
	} {
		if _, err := e.d.SetThresholds(context.Background(), th); !errors.Is(err, ErrInvalidThresholds) {
			t.Errorf("%+v: %v", th, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected thresholds were written", err)
	}
	if st, _ := e.d.Status(context.Background()); st.Thresholds.FiveHour != 95 {
		t.Fatal("rejected thresholds applied", st.Thresholds)
	}
}

// warmTranscript gives e a Claude Code transcript last written ago before the fake clock.
func warmTranscript(e *tenv) func(ago time.Duration) {
	p := filepath.Join(e.t.TempDir(), "-Users-x-p", "s.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("{}\n"), 0o600)
	e.d.o.Projects = filepath.Dir(filepath.Dir(p))
	return func(ago time.Duration) {
		at := e.clk.Now().Add(-ago)
		os.Chtimes(p, at, at)
	}
}

func TestOnlyCacheAwareReadsTranscripts(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	warmTranscript(e)(time.Minute)
	activity := func() *time.Time { st, _ := e.d.Status(ctx); return st.LastActivityAt }
	e.poll()
	if activity() != nil {
		t.Fatal("fill-first walked the transcripts")
	}
	e.d.SetStrategy(ctx, api.CacheAware)
	e.poll()
	if activity() == nil {
		t.Fatal("cache-aware did not read the transcripts")
	}
	e.d.SetStrategy(ctx, api.FillFirst)
	e.poll()
	if activity() != nil {
		t.Fatal("the last activity outlived cache-aware")
	}
}

func TestCacheAwareClimbsToAHigherPriorityAtOnce(t *testing.T) {
	e := newTenv(t)
	e.d.SetStrategy(context.Background(), api.CacheAware)
	touch := warmTranscript(e)
	touch(time.Minute)
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
	e.poll()
	if e.st.State().Active != "b" {
		t.Fatal("cache-aware must leave an exhausted account")
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageAt(10, t0.Add(72*time.Hour)) }) // a's room resets in days
	e.clk.Add(20 * time.Minute)
	touch(time.Minute)
	e.poll()
	if e.st.State().Active != "a" {
		t.Fatal("stayed on the lower priority to keep the cache warm")
	}
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.hit_limit", "a")), note("daemon.switched", "a", i18n.Err("daemon.available_again", "a")))
}

func TestCacheAwareWaitsForAColdCacheWithinAPriority(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	e.d.SetStrategy(ctx, api.CacheAware)
	touch := warmTranscript(e)
	prio := 20
	e.d.Patch(ctx, "b", api.AccountPatch{Priority: &prio})
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atA"] = usageWeekAt(10, t0.Add(72*time.Hour)) // a: room resets in 72 h
		f.usage["atB"] = usageWeekAt(10, t0.Add(30*time.Hour)) // b: in 30 h, sooner but not within a day
	})
	touch(time.Minute)
	e.poll()
	if e.st.State().Active != "a" {
		t.Fatal("left a warm cache for room that resets in more than a day")
	}
	st, _ := e.d.Status(ctx)
	if st.Strategy != api.CacheAware || st.LastActivityAt == nil || !st.LastActivityAt.Equal(e.clk.Now().Add(-time.Minute)) {
		t.Fatalf("status %+v", st)
	}
	e.clk.Add(time.Minute)
	touch(policy.CacheTTL + time.Minute)
	e.poll()
	if e.st.State().Active != "b" {
		t.Fatal("did not move to the sooner reset once the cache was cold")
	}
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.limit_resets_sooner", "b")))
	if err := e.d.SetStrategy(ctx, "round-robin"); !errors.Is(err, ErrInvalidStrategy) {
		t.Fatal(err)
	}
}

func TestCacheAwareSpendsTheSoonerReset(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	e.d.SetStrategy(ctx, api.CacheAware)
	warmTranscript(e)(0)
	prio := 20
	if _, err := e.d.Patch(ctx, "b", api.AccountPatch{Priority: &prio}); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atA"] = usageWeekAt(50, t0.Add(2*time.Hour))
		f.usage["atB"] = usageWeekAt(10, t0.Add(30*time.Minute))
	})
	e.poll()
	if e.st.State().Active != "b" {
		t.Fatal("did not move to b, whose room resets 90 min before a's")
	}
	e.msgs(note("daemon.switched", "b", i18n.Err("daemon.limit_resets_sooner", "b")))
}

func TestStatusNext(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	next := func() string {
		st, _ := e.d.Status(ctx)
		if st.Next == nil {
			return ""
		}
		return *st.Next
	}
	if st, _ := e.d.Status(ctx); st.Host != "box" {
		t.Fatalf("host %q, want the one the daemon was started with", st.Host)
	}
	e.poll()
	if got := next(); got != "b" {
		t.Fatalf("next %q, want b", got)
	}
	e.d.SetAuto(ctx, false)
	if got := next(); got != "" {
		t.Fatalf("auto off: next %q", got)
	}
	e.d.SetAuto(ctx, true)
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = usageBody(99) })
	e.clk.Add(time.Minute)
	e.poll()
	if got := next(); got != "" {
		t.Fatalf("b exhausted: next %q", got)
	}
}

func TestUsageKeepsThePaceAnthropicAllows(t *testing.T) { // the usage endpoint takes about one ask per account every 100 s
	e := newTenv(t)
	var lastOK time.Time
	throttled, ok := 0, 0
	e.api.set(func(f *fakeAnthropic) {
		f.onUsage = func(tok string) {
			if tok != "atB" {
				return
			}
			if now := e.clk.Now(); lastOK.IsZero() || now.Sub(lastOK) >= 100*time.Second {
				lastOK, ok = now, ok+1
				f.usage["atB"] = usageBody(10)
				return
			}
			throttled++
			f.usage["atB"] = resp{429, `{}`}
		}
	})
	for range 60 { // an hour of one-minute ticks
		e.poll()
		e.clk.Add(time.Minute)
	}
	if throttled > 1 || ok < 25 {
		t.Fatalf("%d asks throttled, %d answered: after a 429 the account must be asked at a pace the endpoint takes", throttled, ok)
	}
}

func TestSlowPaceKeepsUsageKnown(t *testing.T) { // usage counts for two paces, a 429's pace included
	e := newTenv(t)
	e.poll()
	e.d.mu.Lock()
	e.d.acct("b").every = 8 * time.Minute
	e.d.mu.Unlock()
	for i := range 17 {
		e.clk.Add(time.Minute)
		e.poll()
		if got := statusNext(e); got != "b" {
			t.Fatalf("minute %d: next %q; b, asked every 8 min, went unknown between answers", i+1, got)
		}
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = resp{500, `{}`} })
	for range 17 {
		e.clk.Add(time.Minute)
		e.poll()
	}
	if got := statusNext(e); got != "" {
		t.Fatalf("next %q: b's last answer is older than two paces", got)
	}
}

func TestRetryAfterKeepsUsageKnown(t *testing.T) { // an account nobody may ask yet is not unknown
	e := newTenv(t)
	e.poll()
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"], f.retryAfter = resp{429, `{}`}, "1800" })
	e.clk.Add(time.Minute)
	e.poll()
	n := e.api.count("usage atB")
	for i := range 29 { // b's test token lasts an hour
		e.clk.Add(time.Minute)
		e.poll()
		e.d.Refresh(context.Background())
		if got := statusNext(e); got != "b" {
			t.Fatalf("minute %d of b's 30-minute Retry-After: next %q", i+2, got)
		}
	}
	if e.api.count("usage atB") != n {
		t.Fatal("asked b before its Retry-After")
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = usageBody(10) })
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atB") != n+1 {
		t.Fatal("b not asked once its Retry-After was over")
	}
	e.clk.Add(time.Minute)
	e.d.Refresh(context.Background())
	if e.api.count("usage atB") != n+1 {
		t.Fatal("a refresh asked b sooner than the 2-minute pace its 429 taught")
	}
	e.clk.Add(time.Minute)
	e.d.Refresh(context.Background())
	if e.api.count("usage atB") != n+2 {
		t.Fatal("a refresh must ask b once its pace allows")
	}
}

func TestA429NeverRevivesStaleUsage(t *testing.T) {
	long := func(e *tenv) { // tokens that outlive the test, so b can be asked hours later
		e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(8*time.Hour)))
		e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(8*time.Hour)))
	}
	e := newTenv(t)
	long(e)
	e.poll()
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"], f.retryAfter = resp{429, `{}`}, "3600" })
	e.clk.Add(6 * time.Hour) // asleep: b's numbers are 6 h old when the 429 comes
	e.poll()
	if e.api.count("usage atB") != 2 {
		t.Fatal("b not asked after the sleep")
	}
	if got := statusNext(e); got != "" {
		t.Fatalf("next %q: a 429 made b's 6-hour-old numbers count again", got)
	}

	e = newTenv(t)
	long(e)
	e.poll()
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"], f.retryAfter = resp{429, `{}`}, "3600" })
	for range 150 {
		e.clk.Add(time.Minute)
		e.poll()
	}
	if n := e.api.count("usage atB"); n < 4 {
		t.Fatalf("b asked %d times: want 429s in a row", n)
	}
	if got := statusNext(e); got != "" {
		t.Fatalf("next %q: 429s in a row kept b's 2.5-hour-old numbers counting", got)
	}
}

func statusNext(e *tenv) string {
	st, _ := e.d.Status(context.Background())
	if st.Next == nil {
		return ""
	}
	return *st.Next
}

func TestSettingsChangeDoesNotAskButRefreshDoes(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	e.poll()
	n := len(e.api.callsSince(0))
	prio := 30
	if _, err := e.d.Patch(ctx, "b", api.AccountPatch{Priority: &prio}); err != nil {
		t.Fatal(err)
	}
	e.d.SetAuto(ctx, true)
	e.poll() // the tick those changes request
	if got := e.api.callsSince(n); len(got) != 0 {
		t.Fatalf("a settings change asked Anthropic within the check interval: %v", got)
	}
	e.d.Refresh(ctx)
	if got := e.api.callsSince(n); len(got) != 0 {
		t.Fatalf("a refresh asked accounts asked under a minute ago: %v", got)
	}
	e.clk.Add(time.Minute)
	e.d.Refresh(ctx)
	if e.api.count("usage atA") != 2 || e.api.count("usage atB") != 2 {
		t.Fatal("a refresh must ask every account now")
	}
	e.clk.Add(55 * time.Second) // a tick's 10% allowance does not apply
	e.d.Refresh(ctx)
	if e.api.count("usage atA") != 2 || e.api.count("usage atB") != 2 {
		t.Fatal("a refresh asked accounts asked 55 s ago")
	}
	e.clk.Add(5 * time.Second)
	e.d.Refresh(ctx)
	if e.api.count("usage atA") != 3 || e.api.count("usage atB") != 3 {
		t.Fatal("a refresh a minute after the last ask must ask")
	}
}

func TestUsageIsNeverAskedTwiceAtOnce(t *testing.T) { // a T3 request during a tick's ask of the same account
	e := newTenv(t)
	e.poll()
	in, release := make(chan struct{}), make(chan struct{})
	e.api.set(func(f *fakeAnthropic) {
		f.onUsage = func(tok string) {
			if tok == "atB" {
				f.onUsage = nil
				close(in)
				<-release
			}
		}
	})
	e.clk.Add(30 * time.Minute)
	ticked := make(chan struct{})
	go func() { e.poll(); close(ticked) }()
	<-in
	raw := make(chan struct{})
	go func() { e.d.UsageRaw(context.Background(), "b", 0); close(raw) }()
	select {
	case <-raw:
	case <-time.After(300 * time.Millisecond): // it asked, and waits on the fake the tick's ask holds
	}
	close(release)
	<-ticked
	<-raw
	if n := e.api.count("usage atB"); n != 2 {
		t.Fatalf("b asked %d times, want 2: the T3 request asked while the tick's ask was in flight", n)
	}
}

func TestUsageRawWaitsForTheAskInFlight(t *testing.T) { // the first ask after a start: nothing cached yet
	e := newTenv(t)
	in, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // runs before newTenv's srv.Close, which waits for the held request
	e.api.set(func(f *fakeAnthropic) {
		f.onUsage = func(tok string) {
			if tok == "atB" {
				f.onUsage = nil
				close(in)
				<-hold
			}
		}
	})
	ticked := make(chan struct{})
	go func() { e.poll(); close(ticked) }()
	<-in
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.d.UsageRaw(cancelled, "b", 0); err == nil {
		t.Fatal("a request that stopped waiting has nothing to answer with")
	}
	var st int
	var body []byte
	got := make(chan error, 1)
	go func() {
		var err error
		st, body, err = e.d.UsageRaw(context.Background(), "b", 0)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("answered before the ask in flight did: %d %v", st, err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	<-ticked
	if err := <-got; err != nil || st != 200 || string(body) != usageBody(10).body || e.api.count("usage atB") != 1 {
		t.Fatalf("T3 must get the tick's answer: %d %s %v, b asked %d times", st, body, err, e.api.count("usage atB"))
	}
}

func TestUsageRawNeverAsksForAccountsLeftOut(t *testing.T) { // disabled, or waiting for a login
	e := newTenv(t)
	e.poll()
	addC(t, e)
	e.st.Update(func(s *store.State) error {
		s.Accounts[1].Enabled = false
		s.Accounts[2].Enabled = false
		return nil
	})
	e.clk.Add(2 * time.Minute)
	if st, body, err := e.d.UsageRaw(context.Background(), "b", 0); err != nil || st != 200 || string(body) != usageBody(10).body {
		t.Fatalf("a disabled account gets the last answer: %d %s %v", st, body, err)
	}
	if _, _, err := e.d.UsageRaw(context.Background(), "c", 0); err == nil {
		t.Fatal("a disabled account never asked has nothing to answer with")
	}
	e.st.Update(func(s *store.State) error { s.Accounts[1].Enabled, s.Accounts[1].NeedsLogin = true, true; return nil })
	if st, _, err := e.d.UsageRaw(context.Background(), "b", 0); err != nil || st != 200 {
		t.Fatalf("an account waiting for a login gets the last answer: %d %v", st, err)
	}
	if n, c := e.api.count("usage atB"), e.api.count("usage atC"); n != 1 || c != 0 {
		t.Fatalf("T3 asked Anthropic for accounts ccswap never asks for: b %d times, c %d", n, c)
	}
}

func TestSignalNeverAsksForADisabledHolder(t *testing.T) {
	e := newTenv(t)
	e.poll()
	e.st.Update(func(s *store.State) error { s.Accounts[0].Enabled = false; return nil })
	e.clk.Add(3 * time.Minute) // a's numbers are too old to judge by
	n := e.api.count("usage atA")
	e.d.Signal(context.Background(), map[string]any{"error": "rate_limit", "last_assistant_message": "You've hit your weekly limit · resets 9pm (Europe/Moscow)"})
	if e.api.count("usage atA") != n {
		t.Fatal("a signal asked for a disabled account")
	}
	if r := e.view("a").ExhaustedReason; r == nil || *r != "limit_hit" {
		t.Fatal("a signal on numbers too old to judge by, with no ask allowed, must count", r)
	}
}

// fakeRemote is another ccswap: it answers /v1/accounts to key with accts().
func fakeRemote(t *testing.T, key string, accts func() []api.Account) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(accts())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func remoteAcct(email string, h5 float64, checked time.Time) api.Account {
	return api.Account{Email: email, FiveHour: &api.Window{Utilization: h5}, SevenDay: &api.Window{Utilization: 5},
		Models: []api.ModelWindow{{Name: "Fable", Utilization: 7}}, CheckedAt: &checked}
}

func TestRemoteUsageReplacesAskingAnthropic(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	var mu sync.Mutex
	accts := []api.Account{remoteAcct("A@X", 33, e.clk.Now().Add(-30*time.Second)), remoteAcct("b@x", 44, e.clk.Now().Add(-30*time.Second))}
	srv := fakeRemote(t, "srv-key", func() []api.Account { mu.Lock(); defer mu.Unlock(); return accts })
	key := "srv-key"
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if n := e.api.count("usage atA") + e.api.count("usage atB"); n != 0 {
		t.Fatalf("asked Anthropic %d times though the other ccswap answered", n)
	}
	if a, b := e.view("a"), e.view("b"); a.FiveHour.Utilization != 33 || b.FiveHour.Utilization != 44 || len(a.Models) != 1 || !a.CheckedAt.Equal(e.clk.Now().Add(-30*time.Second)) {
		t.Fatalf("views %+v %+v", a, b)
	}
	if st, body, err := e.d.UsageRaw(ctx, "a", 0); err != nil || st != 200 || !strings.Contains(string(body), `"utilization":33`) || e.api.count("usage atA") != 0 {
		t.Fatalf("T3 must get the other ccswap's numbers without asking Anthropic: %d %s %v", st, body, err)
	}
	if st, _ := e.d.Status(ctx); st.Remote == nil || !st.Remote.Ok || st.Remote.Url != srv.URL {
		t.Fatalf("status %+v", st.Remote)
	}
	if c, err := config.Load(e.home); err != nil || c.Remote.URL != srv.URL {
		t.Fatal(c.Remote, err)
	}
	if b, _ := e.kc.Get(store.VaultService, remoteKeyAccount); string(b) != "srv-key" {
		t.Fatal("key not kept in the vault")
	}

	mu.Lock()
	accts = accts[:1] // b is not on the other ccswap
	accts[0] = remoteAcct("a@x", 35, e.clk.Now().Add(-10*time.Minute))
	mu.Unlock()
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atB") != 1 || e.api.count("usage atA") != 1 {
		t.Fatal("an account missing there, or with stale numbers, must be asked directly")
	}

	srv.Close()
	e.clk.Add(time.Minute)
	e.poll()
	if st, _ := e.d.Status(ctx); st.Remote == nil || st.Remote.Ok || st.Remote.Error == nil {
		t.Fatalf("an unreachable remote must show: %+v", st.Remote)
	}
	if e.api.count("usage atA") != 2 {
		t.Fatal("not asked directly while the other ccswap is down")
	}
}

func TestRemoteNumbersFollowTheCheckIntervals(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	th := e.d.th()
	th.CheckActive, th.CheckOthers = 5*time.Minute, 30*time.Minute
	e.d.thr.Store(&th)
	var mu sync.Mutex
	at := e.clk.Now().Add(-4 * time.Minute)
	accts := []api.Account{remoteAcct("a@x", 33, at), remoteAcct("b@x", 44, at)}
	srv := fakeRemote(t, "k", func() []api.Account { mu.Lock(); defer mu.Unlock(); return slices.Clone(accts) })
	key := "k"
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	for range 40 { // the other ccswap's numbers age: a in use is asked here after 5 min, b not before an hour
		e.poll()
		e.clk.Add(time.Minute)
	}
	if a, b := e.api.count("usage atA"), e.api.count("usage atB"); a < 7 || b != 0 {
		t.Fatalf("asked a %d and b %d times: a's numbers there are older than its 5 minutes, b's are within two of its 30", a, b)
	}
	if statusNext(e) != "b" {
		t.Fatal("b, fresh by the other ccswap's numbers, must stay a candidate")
	}
	mu.Lock()
	accts[1] = remoteAcct("b@x", 44, e.clk.Now().Add(-time.Minute))
	mu.Unlock()
	e.d.Refresh(ctx)
	if e.api.count("usage atB") != 1 || e.view("b").FiveHour.Utilization != 10 {
		t.Fatal("a refresh must ask b here: the other ccswap takes no refresh from a read key")
	}
	e.clk.Add(time.Minute)
	e.poll()
	if e.view("b").FiveHour.Utilization != 10 || e.api.count("usage atB") != 1 {
		t.Fatal("older numbers from the other ccswap replaced the ones just asked here")
	}
}

func TestRemoteNumbersOfTheAccountInUseGetATick(t *testing.T) { // the other ccswap asks on its own ticks
	e := newTenv(t)
	ctx := context.Background()
	th := e.d.th()
	th.CheckActive = 5 * time.Minute
	e.d.thr.Store(&th)
	var mu sync.Mutex
	age := 5*time.Minute + 30*time.Second
	srv := fakeRemote(t, "k", func() []api.Account {
		mu.Lock()
		defer mu.Unlock()
		return []api.Account{remoteAcct("a@x", 33, e.clk.Now().Add(-age)), remoteAcct("b@x", 44, e.clk.Now())}
	})
	key := "k"
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if e.api.count("usage atA") != 0 || e.view("a").FiveHour.Utilization != 33 {
		t.Fatal("a asked directly though the other ccswap's numbers are within its check and a tick")
	}
	mu.Lock()
	age = 6*time.Minute + time.Second
	mu.Unlock()
	e.clk.Add(time.Minute)
	e.poll()
	if e.api.count("usage atA") != 1 {
		t.Fatal("a not asked directly once the other ccswap's numbers are older than its check and a tick")
	}
}

func TestRemoteClockAheadCountsAsNow(t *testing.T) { // clocks drift between machines
	e := newTenv(t)
	srv := fakeRemote(t, "k", func() []api.Account { return []api.Account{remoteAcct("b@x", 44, e.clk.Now().Add(time.Hour))} })
	key := "k"
	if err := e.d.SetRemote(context.Background(), api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if v := e.view("b"); v.FiveHour.Utilization != 44 || !v.CheckedAt.Equal(e.clk.Now()) {
		t.Fatalf("b's numbers from an hour ahead must count as taken now: %+v", v)
	}
}

func TestRemoteNeverMatchesAMissingEmail(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	e.st.Update(func(s *store.State) error { s.Accounts[1].Email = ""; return nil })
	srv := fakeRemote(t, "k", func() []api.Account { return []api.Account{remoteAcct("", 44, e.clk.Now())} })
	key := "k"
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if e.api.count("usage atB") != 1 || e.view("b").FiveHour.Utilization != 10 {
		t.Fatal("b, which has no email, took the numbers of the other ccswap's account without one")
	}
}

func TestConfigWritesTakeTheConfigLock(t *testing.T) { // they all rewrite the same config.toml
	e := newTenv(t)
	ctx := context.Background()
	t.Cleanup(func() { i18n.Set("ru") })
	srv := fakeRemote(t, "k", func() []api.Account { return nil })
	key := "k"
	writes := map[string]func() error{
		"remote set":   func() error { return e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}) },
		"remote clear": func() error { return e.d.SetRemote(ctx, api.RemoteRequest{}) },
		"lang":         func() error { return e.d.SetLang(ctx, "en") },
	}
	for name, write := range writes {
		e.d.cfgMu.Lock()
		done := make(chan error, 1)
		go func() { done <- write() }()
		select {
		case err := <-done:
			t.Fatalf("%s: wrote config.toml past the config lock: %v", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		e.d.cfgMu.Unlock()
		if err := <-done; err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestWebCodeIsOneTimeAndShortLived(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	c, err := e.d.WebCode(ctx)
	if err != nil || len(c.Code) != 32 || !c.ExpiresAt.Equal(e.clk.Now().Add(time.Minute)) {
		t.Fatalf("%+v %v", c, err)
	}
	for _, wrong := range []string{c.Code + "0", c.Code[:31], "", strings.ToUpper(c.Code)} {
		if err := e.d.UseWebCode(ctx, wrong); !errors.Is(err, ErrBadCode) {
			t.Fatalf("a wrong code %q was taken: %v", wrong, err)
		}
	}
	if err := e.d.UseWebCode(ctx, c.Code); err != nil {
		t.Fatal(err)
	}
	if err := e.d.UseWebCode(ctx, c.Code); !errors.Is(err, ErrBadCode) {
		t.Fatal("a code was taken twice:", err)
	}
	c, _ = e.d.WebCode(ctx)
	e.clk.Add(time.Minute)
	if other, _ := e.d.WebCode(ctx); other.Code == c.Code || len(e.d.codes) != 1 {
		t.Fatal("an expired code is kept, or a code repeats:", e.d.codes)
	}
	if err := e.d.UseWebCode(ctx, c.Code); !errors.Is(err, ErrBadCode) {
		t.Fatal("an expired code was taken:", err)
	}
}

func TestSetRemoteChecksBeforeKeeping(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	srv := fakeRemote(t, "srv-key", func() []api.Account { return nil })
	wrong, right := "nope", "srv-key"
	for _, r := range []api.RemoteRequest{
		{Url: "http://203.0.113.7:8318", Key: &right},
		{Url: srv.URL, Key: &wrong},
		{Url: srv.URL},
	} {
		if err := e.d.SetRemote(ctx, r); !errors.Is(err, ErrInvalidRemote) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused remote was written")
	}
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &right}); err != nil {
		t.Fatal(err)
	}
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL}); err != nil {
		t.Fatal("the stored key must be reused:", err)
	}
	if err := e.d.SetRemote(ctx, api.RemoteRequest{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.d.Status(ctx); st.Remote != nil {
		t.Fatal("still remote after turning it off")
	}
	if _, err := e.kc.Get(store.VaultService, remoteKeyAccount); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("key kept after turning remote off")
	}
}

func TestCheckIntervals(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	th := e.d.th()
	th.CheckActive, th.CheckOthers = 5*time.Minute, 30*time.Minute
	e.d.thr.Store(&th)
	for i := range 40 { // 40 one-minute ticks with a in use (b's test token lasts an hour)
		e.poll()
		if i == 20 {
			if st, _ := e.d.Status(ctx); st.Next == nil || *st.Next != "b" {
				t.Fatal("b's 20-minute-old numbers must keep it a candidate:", st.Next)
			}
		}
		e.clk.Add(time.Minute)
	}
	if a, b := e.api.count("usage atA"), e.api.count("usage atB"); a < 7 || a > 9 || b != 2 {
		t.Fatalf("asked a %d and b %d times in 40 minutes; want about 8 (every 5 min) and 2 (every 30)", a, b)
	}
	if _, err := e.d.Activate(ctx, "b", true); err != nil {
		t.Fatal(err)
	}
	n := e.api.count("usage atB")
	e.poll()
	if e.api.count("usage atB") != n+1 {
		t.Fatal("an account just put in use must be asked at once, not on its 30-minute pace")
	}
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = resp{429, `{}`} })
	e.d.Refresh(ctx)
	n = e.api.count("usage atA")
	e.d.Refresh(ctx)
	if e.api.count("usage atA") != n {
		t.Fatal("a refresh must not ask an account a 429 told to wait")
	}
}

func TestSetLang(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Cleanup(func() { i18n.Set("ru") })
	if err := e.d.SetLang(ctx, "ru"); err != nil || i18n.Current() != "ru" {
		t.Fatal(err, i18n.Current())
	}
	if c, err := config.Load(e.home); err != nil || c.UI.Lang != "ru" {
		t.Fatal(c.UI, err)
	}
	if err := e.d.SetLang(ctx, "qaa"); !errors.Is(err, ErrInvalidLang) || i18n.Current() != "ru" {
		t.Fatal("an unknown language must be refused and change nothing:", err)
	}
	if err := e.d.SetLang(ctx, "RU_ru"); err != nil || i18n.Current() != "ru" {
		t.Fatal(err, i18n.Current())
	}
	if c, err := config.Load(e.home); err != nil || c.UI.Lang != "ru-RU" {
		t.Fatal("config.toml keeps the canonical tag:", c.UI, err)
	}
	if err := e.d.SetLang(ctx, ""); err != nil || i18n.Current() != i18n.EN {
		t.Fatal("unpinned, the locale decides:", err, i18n.Current())
	}
	if c, err := config.Load(e.home); err != nil || c.UI.Lang != "" {
		t.Fatal(c.UI, err)
	}
}

// TestForcedRefreshKeepsGuards: force skips only the check that the access token is still good.
func TestForcedRefreshKeepsGuards(t *testing.T) {
	for _, tc := range []struct {
		name, acct, holder string
		setup              func(e *tenv)
	}{
		{"active", "a", "", func(e *tenv) { e.kc.Fake.Set(store.VaultService, "a", cred("atA9", "rA9", t0.Add(time.Hour))) }},
		{"holder", "b", "b", nil},
		{"entry chain", "b", "", func(e *tenv) { e.kc.Fake.Set(store.VaultService, "b", cred("atA", "rA", t0.Add(time.Hour))) }},
		{"frozen", "b", "", func(e *tenv) { e.d.o.Switcher.Freeze([]string{"b"}, t0.Add(time.Hour)) }},
		{"needs login", "b", "", func(e *tenv) {
			e.st.Update(func(s *store.State) error { s.Accounts[1].NeedsLogin = true; return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenv(t)
			e.api.set(func(f *fakeAnthropic) {
				f.refresh["rA"] = resp{200, `{"access_token":"atA2","refresh_token":"rA2","expires_in":28800}`}
				f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
			})
			if tc.setup != nil {
				tc.setup(e)
			}
			done := e.untouched()
			e.d.refreshOne(context.Background(), tc.acct, tc.holder, true)
			if calls := e.api.callsSince(0); len(calls) != 0 {
				t.Fatal("refreshed:", calls)
			}
			done()
		})
	}
	t.Run("live token", func(t *testing.T) {
		e := newTenv(t)
		e.api.set(func(f *fakeAnthropic) {
			f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
		})
		e.d.refreshOne(context.Background(), "b", "", true) // good for an hour
		if e.api.count("refresh rB") != 1 || e.vault("b").OAuth.RefreshToken != "rB2" {
			t.Fatal(e.api.callsSince(0))
		}
	})
}

func TestRemoveWakesT3WaitingForTheAsk(t *testing.T) {
	e := newTenv(t)
	in, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	e.api.set(func(f *fakeAnthropic) {
		f.onUsage = func(tok string) {
			if tok == "atB" {
				f.onUsage = nil
				close(in)
				<-hold
			}
		}
	})
	ticked := make(chan struct{})
	go func() { e.poll(); close(ticked) }()
	<-in
	got := make(chan error, 1)
	go func() { _, _, err := e.d.UsageRaw(context.Background(), "b", 0); got <- err }()
	time.Sleep(100 * time.Millisecond) // let it start waiting
	if err := e.d.Remove(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, ErrNotFound) {
			t.Fatal("want ErrNotFound, got", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the T3 wait outlived the removed account")
	}
	release()
	<-ticked
}
