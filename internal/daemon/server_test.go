package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/t3"
)

type fakeCore struct {
	calls   []string
	imp     api.ImportRequest
	patch   api.AccountPatch
	force   bool
	auto    bool
	th      api.Thresholds
	bus     broadcast
	payload map[string]any
	names   []string
	token   string
	mimp    api.MigrateImportRequest
	err     error
}

func (f *fakeCore) rec(s string) { f.calls = append(f.calls, s) }
func (f *fakeCore) Status(context.Context) (api.Status, error) {
	f.rec("status")
	return api.Status{Version: "1", Active: "a", Auto: true}, f.err
}
func (f *fakeCore) Accounts(context.Context) ([]api.Account, error) {
	f.rec("accounts")
	return []api.Account{{Name: "a", Models: []api.ModelWindow{}}}, f.err
}
func (f *fakeCore) Import(_ context.Context, r api.ImportRequest) (api.Account, error) {
	f.rec("import")
	f.imp = r
	return api.Account{Name: r.Name}, f.err
}
func (f *fakeCore) Adopt(_ context.Context, r api.AdoptRequest) (api.Account, error) {
	f.rec("adopt " + r.Name)
	return api.Account{Name: r.Name}, f.err
}
func (f *fakeCore) Patch(_ context.Context, n string, p api.AccountPatch) (api.Account, error) {
	f.rec("patch " + n)
	f.patch = p
	return api.Account{Name: n}, f.err
}
func (f *fakeCore) Remove(_ context.Context, n string) error { f.rec("remove " + n); return f.err }
func (f *fakeCore) RefreshLogin(_ context.Context, n string) (api.Account, error) {
	f.rec("refresh login " + n)
	return api.Account{Name: n}, f.err
}
func (f *fakeCore) StartLogin(_ context.Context, n string, replace bool) (api.LoginLink, error) {
	f.rec(fmt.Sprintf("start login %s replace=%v", n, replace))
	return api.LoginLink{Id: "L1", Url: "https://claude.example/oauth/authorize?state=s"}, f.err
}
func (f *fakeCore) FinishLogin(_ context.Context, n, id, code string) (api.Account, error) {
	f.rec("finish login " + n + " " + id + " " + code)
	return api.Account{Name: n}, f.err
}
func (f *fakeCore) CancelLogin(_ context.Context, n, id string) error {
	f.rec("cancel login " + n + " " + id)
	return f.err
}
func (f *fakeCore) Activate(_ context.Context, n string, force bool) (api.Status, error) {
	f.rec("activate " + n)
	f.force = force
	return api.Status{Active: n}, f.err
}
func (f *fakeCore) SetAuto(_ context.Context, e bool) error { f.rec("auto"); f.auto = e; return f.err }
func (f *fakeCore) Refresh(context.Context) error           { f.rec("refresh"); return f.err }
func (f *fakeCore) Subscribe() (<-chan struct{}, func())    { return f.bus.subscribe() }
func (f *fakeCore) SetRemote(_ context.Context, r api.RemoteRequest) error {
	f.rec("remote " + r.Url)
	return f.err
}
func (f *fakeCore) SetStrategy(_ context.Context, s api.Strategy) error {
	f.rec("strategy " + string(s))
	return f.err
}

func (f *fakeCore) SetLang(_ context.Context, lang string) error {
	f.rec("lang " + lang)
	return f.err
}
func (f *fakeCore) SetThresholds(_ context.Context, t api.Thresholds) (api.Thresholds, error) {
	f.rec("thresholds")
	f.th = t
	return t, f.err
}
func (f *fakeCore) Signal(_ context.Context, p map[string]any) error {
	f.rec("signal")
	f.payload = p
	return f.err
}
func (f *fakeCore) WebCode(context.Context) (api.WebCode, error) {
	f.rec("code")
	return api.WebCode{Code: "c0de"}, f.err
}
func (f *fakeCore) Version() string { return "v1.2.3" }
func (f *fakeCore) MigrateExport(_ context.Context, names []string) (api.MigrateExport, error) {
	f.rec("migrate export")
	f.names = names
	return api.MigrateExport{Token: "t0k", Bundle: "bundle", Code: "c0de"}, f.err
}
func (f *fakeCore) MigrateCommit(_ context.Context, token string) error {
	f.rec("migrate commit")
	f.token = token
	return f.err
}
func (f *fakeCore) MigrateCancel(_ context.Context, token string) error {
	f.rec("migrate cancel " + token)
	return f.err
}
func (f *fakeCore) MigrateImport(_ context.Context, r api.MigrateImportRequest) (api.MigrateImport, error) {
	f.rec("migrate import")
	f.mimp = r
	return api.MigrateImport{Accounts: []api.Account{{Name: "m", Models: []api.ModelWindow{}}}}, f.err
}
func (f *fakeCore) UseWebCode(_ context.Context, code string) error {
	f.rec("session")
	if code != "c0de" {
		return ErrBadCode
	}
	return f.err
}

func start(t *testing.T, core Core) (*api.ClientWithResponses, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, sock+".lock", core) }()
	t.Cleanup(func() { cancel(); <-done })
	for range 200 {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	return c, sock
}

func TestRoutesOverSocket(t *testing.T) {
	f := &fakeCore{}
	c, _ := start(t, f)
	ctx := context.Background()
	want := func(code, got int) {
		t.Helper()
		if code != got {
			t.Fatalf("status %d, want %d", got, code)
		}
	}
	st, err := c.GetStatusWithResponse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want(200, st.StatusCode())
	if st.JSON200.Active != "a" || !st.JSON200.Auto {
		t.Fatalf("status body %+v", st.JSON200)
	}
	ac, _ := c.ListAccountsWithResponse(ctx)
	want(200, ac.StatusCode())
	if len(*ac.JSON200) != 1 {
		t.Fatal("accounts")
	}
	prio := 3
	im, _ := c.ImportAccountWithResponse(ctx, api.ImportAccountJSONRequestBody{Name: "n", ConfigDir: "/tmp/x", Priority: &prio})
	want(200, im.StatusCode())
	if f.imp.Name != "n" || f.imp.ConfigDir != "/tmp/x" || *f.imp.Priority != 3 {
		t.Fatalf("import args %+v", f.imp)
	}
	ad, _ := c.AdoptAccountWithResponse(ctx, api.AdoptAccountJSONRequestBody{Name: "z"})
	want(200, ad.StatusCode())
	en := false
	pa, _ := c.PatchAccountWithResponse(ctx, "n", api.AccountPatch{Enabled: &en})
	want(200, pa.StatusCode())
	if f.patch.Enabled == nil || *f.patch.Enabled || f.patch.Priority != nil {
		t.Fatalf("patch %+v", f.patch)
	}
	force := true
	act, _ := c.ActivateAccountWithResponse(ctx, "n", &api.ActivateAccountParams{Force: &force})
	want(200, act.StatusCode())
	if !f.force || act.JSON200.Active != "n" {
		t.Fatal("activate")
	}
	au, _ := c.SetAutoWithResponse(ctx, api.AutoRequest{Enabled: true})
	want(204, au.StatusCode())
	if !f.auto {
		t.Fatal("auto")
	}
	th, _ := c.SetThresholdsWithResponse(ctx, api.Thresholds{FiveHour: 90, SevenDay: 97, Hysteresis: 5, MinDwellSeconds: 60})
	want(200, th.StatusCode())
	if f.th.SevenDay != 97 || th.JSON200.MinDwellSeconds != 60 {
		t.Fatalf("thresholds %+v", f.th)
	}
	rf, _ := c.RefreshWithResponse(ctx)
	want(204, rf.StatusCode())
	sg, _ := c.SignalWithResponse(ctx, api.SignalRequest{Payload: map[string]any{"k": "v"}})
	want(204, sg.StatusCode())
	if f.payload["k"] != "v" {
		t.Fatalf("payload %v", f.payload)
	}
	rm, _ := c.RemoveAccountWithResponse(ctx, "n")
	want(204, rm.StatusCode())
	ex, _ := c.MigrateExportWithResponse(ctx, api.MigrateExportRequest{Names: []string{"x", "y"}})
	want(200, ex.StatusCode())
	if !slices.Equal(f.names, []string{"x", "y"}) || ex.JSON200.Token != "t0k" || ex.JSON200.Bundle != "bundle" || ex.JSON200.Code != "c0de" {
		t.Fatalf("export %v %+v", f.names, ex.JSON200)
	}
	co, _ := c.MigrateCommitWithResponse(ctx, api.MigrateTokenRequest{Token: "t0k"})
	want(204, co.StatusCode())
	if f.token != "t0k" {
		t.Fatal("commit token", f.token)
	}
	ca, _ := c.MigrateCancelWithResponse(ctx, api.MigrateTokenRequest{Token: "t1k"})
	want(204, ca.StatusCode())
	mi, _ := c.MigrateImportWithResponse(ctx, api.MigrateImportRequest{Bundle: "bundle", Code: "c0de", Replace: true})
	want(200, mi.StatusCode())
	if f.mimp.Bundle != "bundle" || f.mimp.Code != "c0de" || !f.mimp.Replace || len(mi.JSON200.Accounts) != 1 {
		t.Fatalf("import %+v %+v", f.mimp, mi.JSON200)
	}
	if len(f.calls) != 15 || f.calls[13] != "migrate cancel t1k" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestActivateConflictBody(t *testing.T) {
	f := &fakeCore{err: switcher.ErrNearRefresh}
	c, _ := start(t, f)
	r, err := c.ActivateAccountWithResponse(context.Background(), "n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != 409 || r.JSON409 == nil || r.JSON409.Code != "near_refresh" {
		t.Fatalf("%d %s", r.StatusCode(), r.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		name string
	}{
		{ErrNotFound, 404, "not_found"},
		{ErrNoEntry, 404, "no_entry"},
		{switcher.ErrNoSuchAccount, 404, "not_found"},
		{store.ErrDuplicate, 409, "duplicate"},
		{ErrRemoveActive, 409, "remove_active"},
		{ErrActiveAccount, 409, "active_account"},
		{ErrClaudeOwns, 409, "claude_owns"},
		{ErrRefreshFailed, 502, "refresh_failed"},
		{i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, "b"), 409, "needs_login"},
		{ErrLoginIncomplete, 400, "login_incomplete"},
		{ErrInvalidName, 400, "invalid_name"},
		{fmt.Errorf("%w: x", ErrInvalidThresholds), 400, "invalid_thresholds"},
		{fmt.Errorf("%w: x", ErrInvalidStrategy), 400, "invalid_strategy"},
		{ErrInvalidConfigDir, 400, "invalid_config_dir"},
		{switcher.ErrUnknownActive, 409, "unknown_active"},
		{switcher.ErrCannotIdentify, 409, "cannot_identify"},
		{switcher.ErrEntryExpired, 409, "entry_expired"},
		{switcher.ErrNeedsLogin, 409, "needs_login"},
		{switcher.ErrTargetRefreshing, 409, "target_refreshing"},
		{ErrMigrateActive, 409, "migrate_active"},
		{i18n.Err("daemon.migrate_active", ErrMigrateActive, "b"), 409, "migrate_active"},
		{ErrExportGone, 409, "export_gone"},
		{ErrExportDone, 409, "export_done"},
		{ErrTokensChanged, 409, "tokens_changed"},
		{i18n.Err("daemon.tokens_changed", ErrTokensChanged, "b"), 409, "tokens_changed"},
		{switcher.ErrFrozen, 409, "migrating"},
		{i18n.Err("daemon.migrating", switcher.ErrFrozen, "b"), 409, "migrating"},
		{migrate.ErrNotBundle, 400, "not_bundle"},
		{migrate.ErrNewerVersion, 400, "bundle_version"},
		{migrate.ErrCodeFormat, 400, "invalid_code"},
		{migrate.ErrBadCode, 400, "bad_bundle"},
		{i18n.Err("daemon.migrate_email_here", store.ErrDuplicate, "b@x", "b"), 409, "duplicate"},
		{i18n.Err("daemon.migrate_name_taken", store.ErrDuplicate, "b", "b@x"), 409, "duplicate"},
		{i18n.Err("daemon.migrate_spent", ErrBundleSpent, "b", "b"), 409, "bundle_spent"},
		{i18n.Err("daemon.migrate_unchecked", ErrBundleUnchecked, "b"), 502, "bundle_unchecked"},
		{os.ErrPermission, 500, "internal"},
	} {
		c, e := classify(tc.err)
		if c != tc.code || e.Code != tc.name {
			t.Errorf("%v -> %d %s", tc.err, c, e.Code)
		}
	}
	if _, e := classify(os.ErrPermission); e.Message != "internal error" {
		t.Fatal("internal message leaks")
	}
	if _, e := classify(migrate.ErrBadCode); e.Message != i18n.T("daemon.bad_bundle") { // the sentinel is English
		t.Fatal("bad_bundle message:", e.Message)
	}
}

func TestBadBody(t *testing.T) {
	_, sock := start(t, &fakeCore{})
	c, _ := Dial(sock)
	r, err := c.ImportAccountWithBodyWithResponse(context.Background(), "application/json", strings.NewReader("{"))
	if err != nil || r.StatusCode() != http.StatusBadRequest {
		t.Fatalf("%v %v", err, r)
	}
	// the contract declares 400 wherever a body can be malformed
	ctx := context.Background()
	p, _ := c.PatchAccountWithBodyWithResponse(ctx, "a", "application/json", strings.NewReader("{"))
	a, _ := c.SetAutoWithBodyWithResponse(ctx, "application/json", strings.NewReader("{"))
	sg, _ := c.SignalWithBodyWithResponse(ctx, "application/json", strings.NewReader("{"))
	for _, e := range []*api.Error{p.JSON400, a.JSON400, sg.JSON400} {
		if e == nil || e.Code != "bad_request" {
			t.Fatalf("400 not in the contract: %+v", e)
		}
	}
}

func TestSocketMode(t *testing.T) {
	f := &fakeCore{}
	_, sock := start(t, f)
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	// a live daemon blocks a second one
	if err := Serve(context.Background(), sock, sock+".lock", f); err == nil {
		t.Fatal("second Serve on a live socket must fail")
	}
	// a stale socket does not
	stale := filepath.Join(shortDir(t), "stale.sock")
	staleSocket(t, stale)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, stale, stale+".lock", f) }()
	c, _ := Dial(stale)
	var ok bool
	for range 200 {
		if r, err := c.GetStatusWithResponse(ctx); err == nil && r.StatusCode() == 200 {
			ok = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("stale socket not replaced")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("socket file left behind")
	}
}

// staleSocket leaves a real socket file nobody listens on.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ccs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d, 0o700); os.RemoveAll(d) })
	return d
}

func TestServeKeepsSocketOnOtherDialErrors(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	staleSocket(t, sock)
	if err := os.Chmod(sock, 0); err != nil { // dial now fails with EACCES, not ECONNREFUSED
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Serve(ctx, sock, sock+".lock", &fakeCore{}); err == nil {
		t.Fatal("Serve must refuse when the socket cannot be probed")
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("socket file must not be unlinked:", err)
	}
	// a regular file is no socket of ours (ENOTSOCK): kept, daemon refuses
	file := filepath.Join(shortDir(t), "f.sock")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(ctx, file, file+".lock", &fakeCore{}); err == nil {
		t.Fatal("Serve must refuse to replace a non-socket file")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep" {
		t.Fatal("non-socket file touched")
	}
}

func TestServeReplacesRefusedSocket(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	staleSocket(t, sock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, sock+".lock", &fakeCore{}) }()
	c, _ := Dial(sock)
	ok := false
	for range 200 {
		if r, err := c.GetStatusWithResponse(ctx); err == nil && r.StatusCode() == 200 {
			ok = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestServeLockBlocksSecondDaemon(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	staleSocket(t, sock)
	f, err := os.OpenFile(sock+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Serve(ctx, sock, sock+".lock", &fakeCore{}); err == nil {
		t.Fatal("Serve must refuse while another daemon holds the lock")
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("a locked daemon's socket must not be unlinked:", err)
	}

	// two daemons starting at once: exactly one runs.
	sock2 := filepath.Join(dir, "t.sock")
	ctx2, cancel2 := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- Serve(ctx2, sock2, sock2+".lock", &fakeCore{}) }()
	}
	first := <-errs // the loser returns at once
	if first == nil {
		t.Fatal("one of two concurrent daemons must refuse")
	}
	cancel2()
	if err := <-errs; err != nil {
		t.Fatal("the winner must shut down cleanly:", err)
	}
}

func TestServeCreatesLockDir(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "app", "daemon.lock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, filepath.Join(dir, "s.sock"), lock, &fakeCore{}) }()
	waitFor(t, "lock file", func() bool { _, err := os.Stat(lock); return err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(lock)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatal(fi, err)
	}
}

type fakeHub struct{}

func (fakeHub) StoredAccounts() []store.Account { return []store.Account{{Name: "a", Enabled: true}} }
func (fakeHub) UsageRaw(context.Context, string, time.Duration) (int, []byte, error) {
	return 200, []byte("{}"), nil
}

var keys = httpKeys{read: "r3ad", admin: "adm1n", local: true}

func httpDo(t *testing.T, app interface {
	Test(*http.Request, ...fiber.TestConfig) (*http.Response, error)
}, method, path, key, body string) *http.Response {
	t.Helper()
	return httpDoHost(t, app, "127.0.0.1:8318", method, path, key, body)
}

func httpDoHost(t *testing.T, app interface {
	Test(*http.Request, ...fiber.TestConfig) (*http.Response, error)
}, host, method, path, key, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func errCode(t *testing.T, r *http.Response) string {
	t.Helper()
	var e api.Error
	json.NewDecoder(r.Body).Decode(&e)
	return e.Code
}

type route struct{ method, path, body string }

var (
	readRoutes  = []route{{"GET", "/v1/status", ""}, {"GET", "/v1/accounts", ""}, {"GET", "/v0/management/auth-files", ""}}
	adminRoutes = []route{
		{"PATCH", "/v1/accounts/a", `{"priority":3}`},
		{"POST", "/v1/accounts/a/activate", ""},
		{"POST", "/v1/accounts/a/refresh", ""},
		{"PUT", "/v1/auto", `{"enabled":true}`},
		{"PUT", "/v1/strategy", `{"strategy":"cache-aware"}`},
		{"PUT", "/v1/thresholds", `{"five_hour":90,"seven_day":97,"hysteresis":5,"min_dwell_seconds":60}`},
		{"POST", "/v1/refresh", ""},
	}
	socketRoutes = []route{
		{"POST", "/v1/accounts", `{"name":"n","config_dir":"/tmp/x"}`},
		{"POST", "/v1/accounts/adopt", `{"name":"n"}`},
		{"DELETE", "/v1/accounts/a", ""},
		{"POST", "/v1/signal", `{"payload":{}}`},
		{"PUT", "/v1/remote", `{"url":""}`},
		{"POST", "/v1/web/code", ""},
	}
)

func TestHTTPApp(t *testing.T) {
	f := &fakeCore{}
	app := newHTTPApp(f, fakeHub{}, keys)
	if c := app.Config(); c.ReadTimeout == 0 || c.IdleTimeout == 0 || c.WriteTimeout != 0 { // /v1/events streams
		t.Errorf("timeouts: read %v, idle %v, write %v", c.ReadTimeout, c.IdleTimeout, c.WriteTimeout)
	}
	if r := httpDo(t, app, "GET", "/", "", ""); r.StatusCode/100 != 3 || r.Header.Get("Location") != "/management.html" {
		t.Errorf("/: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for path, ctype := range map[string]string{"/management.html": "text/html", "/app.js": "javascript", "/app.css": "text/css"} {
		r := httpDo(t, app, "GET", path, "", "")
		if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), ctype) ||
			!strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: %d %q %q", path, r.StatusCode, r.Header.Get("Content-Type"), r.Header.Get("Content-Security-Policy"))
		}
	}
	for _, key := range []string{"", "wrong"} {
		for _, c := range append(readRoutes, adminRoutes...) {
			if r := httpDo(t, app, c.method, c.path, key, c.body); r.StatusCode != 401 {
				t.Errorf("key %q, %s %s: %d", key, c.method, c.path, r.StatusCode)
			}
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("unauthorized calls reached the core: %v", f.calls)
	}
	for _, key := range []string{keys.read, keys.admin} {
		for _, c := range readRoutes {
			if r := httpDo(t, app, c.method, c.path, key, c.body); r.StatusCode != 200 {
				t.Errorf("key %q, %s %s: %d", key, c.method, c.path, r.StatusCode)
			}
		}
	}
	for _, c := range adminRoutes {
		if r := httpDo(t, app, c.method, c.path, keys.read, c.body); r.StatusCode != 403 || errCode(t, r) != "read_only" {
			t.Errorf("read key changed something: %s %s: %d", c.method, c.path, r.StatusCode)
		}
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return c != "status" && c != "accounts" }) {
		t.Fatalf("the read key reached a change: %v", f.calls)
	}
	for _, c := range adminRoutes {
		if r := httpDo(t, app, c.method, c.path, keys.admin, c.body); r.StatusCode/100 != 2 {
			t.Errorf("admin key on loopback: %s %s: %d", c.method, c.path, r.StatusCode)
		}
	}
	served := len(f.calls)
	for _, q := range []string{"?force=true", "?force=false"} {
		if r := httpDo(t, app, "POST", "/v1/accounts/a/activate"+q, keys.admin, ""); r.StatusCode != 400 {
			t.Errorf("force over TCP%s: %d", q, r.StatusCode)
		}
	}
	for _, c := range socketRoutes {
		if r := httpDo(t, app, c.method, c.path, keys.admin, c.body); r.StatusCode/100 != 4 {
			t.Errorf("%s %s must stay on the unix socket: %d", c.method, c.path, r.StatusCode)
		}
	}
	if len(f.calls) != served {
		t.Fatalf("forced or socket-only calls reached the core: %v", f.calls[served:])
	}
}

// TestMigrateNotOnTCP: moving accounts is for the unix socket only, whatever key or Host a request carries.
func TestMigrateNotOnTCP(t *testing.T) {
	f := &fakeCore{}
	app := newHTTPApp(f, fakeHub{}, keys)
	for _, c := range []route{
		{"POST", "/v1/migrate/export", `{"names":["b"]}`},
		{"POST", "/v1/migrate/commit", `{"token":"t"}`},
		{"POST", "/v1/migrate/cancel", `{"token":"t"}`},
		{"POST", "/v1/migrate/import", `{"bundle":"b","code":"c","replace":false}`},
	} {
		if r := httpDo(t, app, c.method, c.path, keys.admin, c.body); r.StatusCode != 404 {
			t.Errorf("%s %s over TCP: %d", c.method, c.path, r.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("migrate calls reached the core over TCP: %v", f.calls)
	}
	for route := range tcpRoutes {
		if strings.Contains(route, "/migrate/") {
			t.Errorf("tcpRoutes names %s", route)
		}
	}
}

func TestHTTPAppOffLoopbackOnlyReads(t *testing.T) {
	inEnglish(t)
	f := &fakeCore{}
	k := keys
	k.local = false
	app := newHTTPApp(f, fakeHub{}, k)
	for _, c := range readRoutes {
		if r := httpDo(t, app, c.method, c.path, k.read, c.body); r.StatusCode != 200 {
			t.Errorf("%s %s: %d", c.method, c.path, r.StatusCode)
		}
	}
	for _, c := range adminRoutes {
		r := httpDo(t, app, c.method, c.path, k.admin, c.body)
		var e api.Error
		json.NewDecoder(r.Body).Decode(&e)
		if r.StatusCode != 403 || e.Code != "local_only" || e.Message != "settings can be changed only from this machine" {
			t.Errorf("admin key off loopback: %s %s: %d %+v", c.method, c.path, r.StatusCode, e)
		}
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return c != "status" && c != "accounts" }) {
		t.Fatalf("a change reached the core off loopback: %v", f.calls)
	}
}

func TestWebSessionOverTCP(t *testing.T) {
	f := &fakeCore{}
	app := newHTTPApp(f, fakeHub{}, keys)
	r := httpDo(t, app, "POST", "/v1/web/session", "", `{"code":"c0de"}`)
	var s api.WebSession
	if json.NewDecoder(r.Body).Decode(&s); r.StatusCode != 200 || s.Key != keys.admin {
		t.Fatalf("a code from ccswap web, no key, must get the listener's admin key: %d %+v", r.StatusCode, s)
	}
	f.calls = nil
	if r := httpDo(t, newApp(f), "POST", "/v1/web/session", "", `{"code":"c0de"}`); r.StatusCode != 404 || len(f.calls) != 0 {
		t.Fatalf("the unix socket holds no listener key: %d %v", r.StatusCode, f.calls)
	}
	if r := httpDo(t, app, "POST", "/v1/web/session", "", `{"code":"nope"}`); r.StatusCode != 403 || errCode(t, r) != "bad_code" {
		t.Fatalf("wrong code: %d", r.StatusCode)
	}
	k := keys
	k.local = false
	f.calls = nil
	r = httpDo(t, newHTTPApp(f, fakeHub{}, k), "POST", "/v1/web/session", keys.admin, `{"code":"c0de"}`)
	if r.StatusCode != 403 || errCode(t, r) != "local_only" || len(f.calls) != 0 {
		t.Fatalf("a session off loopback: %d %v", r.StatusCode, f.calls)
	}
}

func TestChangesNeedALoopbackHost(t *testing.T) { // DNS rebinding: a foreign name resolved to 127.0.0.1
	f := &fakeCore{}
	app := newHTTPApp(f, fakeHub{}, keys)
	for _, host := range []string{"evil.example.com:8318", "evil.example.com", "127.0.0.1.example.com:8318", "[::2]:8318"} {
		for _, c := range append(adminRoutes, route{"POST", "/v1/web/session", `{"code":"c0de"}`}) {
			if r := httpDoHost(t, app, host, c.method, c.path, keys.admin, c.body); r.StatusCode != 403 || errCode(t, r) != "local_only" {
				t.Errorf("Host %q, %s %s: %d", host, c.method, c.path, r.StatusCode)
			}
		}
		for _, c := range readRoutes {
			if r := httpDoHost(t, app, host, c.method, c.path, keys.read, c.body); r.StatusCode != 200 {
				t.Errorf("Host %q, read %s %s: %d", host, c.method, c.path, r.StatusCode)
			}
		}
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return c != "status" && c != "accounts" }) {
		t.Fatalf("a change under a foreign Host reached the core: %v", f.calls)
	}
	if loopbackHost("") { // HTTP/1.0 without Host; app.Test always sends one
		t.Error("no Host taken for a loopback one")
	}
	for _, host := range []string{"127.0.0.1:8318", "127.0.0.1", "localhost:9", "LocalHost", "[::1]:8318", "[::1]"} {
		if r := httpDoHost(t, app, host, "POST", "/v1/refresh", keys.admin, ""); r.StatusCode != 204 {
			t.Errorf("Host %q: %d", host, r.StatusCode)
		}
	}
}

// TestLoginTakesEitherKeyAnywhere: a new login of an account needs a key, but not the admin key, a
// loopback listener or a loopback Host: only that account's own sign-in can finish it. Only the admin
// key, and the unix socket, may replace a link that waits.
func TestLoginTakesEitherKeyAnywhere(t *testing.T) {
	logins := []route{
		{"POST", "/v1/accounts/b/login", ""},
		{"POST", "/v1/accounts/b/login/code", `{"id":"L1","code":"C1#S1"}`},
		{"DELETE", "/v1/accounts/b/login?id=L1", ""},
	}
	f := &fakeCore{}
	off := keys
	off.local = false
	for _, app := range []*fiber.App{newHTTPApp(f, fakeHub{}, keys), newHTTPApp(f, fakeHub{}, off)} {
		for _, key := range []string{"", "wrong"} {
			for _, c := range logins {
				if r := httpDo(t, app, c.method, c.path, key, c.body); r.StatusCode != 401 {
					t.Errorf("key %q, %s %s: %d", key, c.method, c.path, r.StatusCode)
				}
			}
		}
		if len(f.calls) != 0 {
			t.Fatalf("unauthorized logins reached the core: %v", f.calls)
		}
		for _, key := range []string{keys.read, keys.admin} {
			for _, c := range logins {
				if r := httpDoHost(t, app, "ccswap.example.com", c.method, c.path, key, c.body); r.StatusCode/100 != 2 {
					t.Errorf("key %q, %s %s under another Host: %d", key, c.method, c.path, r.StatusCode)
				}
			}
		}
		rest := []string{"finish login b L1 C1#S1", "cancel login b L1"}
		want := slices.Concat([]string{"start login b replace=false"}, rest, []string{"start login b replace=true"}, rest)
		if !slices.Equal(f.calls, want) {
			t.Fatalf("calls %v", f.calls)
		}
		f.calls = nil
	}
	for _, c := range logins {
		httpDo(t, newApp(f), c.method, c.path, "", c.body)
	}
	if want := []string{"start login b replace=true", "finish login b L1 C1#S1", "cancel login b L1"}; !slices.Equal(f.calls, want) {
		t.Fatalf("unix socket calls %v", f.calls)
	}
}

func TestHTTPAppEmptyKeysAuthorizeNobody(t *testing.T) {
	app := newHTTPApp(&fakeCore{}, fakeHub{}, httpKeys{local: true})
	for _, c := range append(readRoutes, adminRoutes...) {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer ")
		if r, err := app.Test(req); err != nil || r.StatusCode != 401 {
			t.Errorf("%s %s: %v %v", c.method, c.path, r.StatusCode, err)
		}
	}
}

func TestI18nEndpoint(t *testing.T) {
	i18n.Set("ru") // the daemon's own language never changes what a page asks for
	c, _ := start(t, &fakeCore{})
	ctx := context.Background()
	for tag, want := range map[string]struct{ lang, title string }{
		"en": {"en", "Log in to ccswap"}, "ru": {"ru", "Вход в ccswap"},
		"en-US": {"en", "Log in to ccswap"}, "ru_RU": {"ru", "Вход в ccswap"},
	} {
		r, err := c.GetI18nWithResponse(ctx, tag, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := r.HTTPResponse.Header
		if r.StatusCode() != 200 || h.Get("Cache-Control") != "no-cache" || !strings.HasPrefix(h.Get("ETag"), `"`) {
			t.Fatalf("%s: %d %q %q", tag, r.StatusCode(), h.Get("Cache-Control"), h.Get("ETag"))
		}
		cat := r.JSON200
		if cat.Lang != want.lang || cat.Name != i18n.Name(i18n.Lang(want.lang)) || cat.Messages["auth.title"] != want.title || cat.Messages["web.auth.title"] != nil {
			t.Fatalf("%s: %s %s %v", tag, cat.Lang, cat.Name, cat.Messages["auth.title"])
		}
		if !slices.Contains(cat.Languages, api.Language{Code: "en", Name: "English"}) || !slices.Contains(cat.Languages, api.Language{Code: "ru", Name: "Русский"}) {
			t.Fatalf("%s: languages %v", tag, cat.Languages)
		}
		if _, ok := cat.Messages["accts.count"].(map[string]any)["other"]; !ok {
			t.Fatalf("%s: a plural message comes as its forms: %v", tag, cat.Messages["accts.count"])
		}
	}
	r, err := c.GetI18nWithResponse(ctx, "xx", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != 404 || r.JSON404 == nil || r.JSON404.Code != "not_found" {
		t.Fatalf("unknown language: %d %s", r.StatusCode(), r.Body)
	}
}

// TestI18nETag: a browser that has the texts asks with their ETag and gets 304 while they and the build
// are the same.
func TestI18nETag(t *testing.T) {
	app := newApp(&fakeCore{})
	get := func(app *fiber.App, lang, inm string) *http.Response {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/i18n/"+lang, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		r, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	en, ru := get(app, "en", "").Header.Get("ETag"), get(app, "ru", "").Header.Get("ETag")
	if en == "" || en == ru || get(app, "en-GB", "").Header.Get("ETag") != en {
		t.Fatalf("ETags: en %q, ru %q", en, ru)
	}
	for _, inm := range []string{en, "W/" + en, `"other", ` + en, "*"} {
		r := get(app, "en", inm)
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 304 || len(b) != 0 || r.Header.Get("ETag") != en || r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("If-None-Match %s: %d %q, %d bytes", inm, r.StatusCode, r.Header.Get("ETag"), len(b))
		}
	}
	for _, inm := range []string{ru, `"stale"`} {
		if r := get(app, "en", inm); r.StatusCode != 200 {
			t.Errorf("If-None-Match %s: %d", inm, r.StatusCode)
		}
	}
	if r := get(newApp(&otherBuild{}), "en", en); r.StatusCode != 200 || r.Header.Get("ETag") == en {
		t.Fatalf("another build kept the ETag: %d %q", r.StatusCode, r.Header.Get("ETag"))
	}
}

type otherBuild struct{ fakeCore }

func (*otherBuild) Version() string { return "v1.2.4" }

// TestI18nEndpointOnTCPWithoutKey: the login screen needs the texts before it has a key, and they hold no
// secret; everything else the listener serves still asks for one.
func TestI18nEndpointOnTCPWithoutKey(t *testing.T) {
	f := &fakeCore{}
	off := keys
	off.local = false
	for _, app := range []*fiber.App{newHTTPApp(f, fakeHub{}, keys), newHTTPApp(f, fakeHub{}, off)} {
		for _, host := range []string{"127.0.0.1:8318", "evil.example.com"} {
			r := httpDoHost(t, app, host, "GET", "/v1/i18n/en", "", "")
			var cat api.Catalog
			if json.NewDecoder(r.Body).Decode(&cat); r.StatusCode != 200 || cat.Messages["auth.title"] != "Log in to ccswap" {
				t.Fatalf("Host %q, no key: %d %v", host, r.StatusCode, cat.Messages["auth.title"])
			}
		}
	}
	app := newHTTPApp(f, fakeHub{}, keys)
	for route := range tcpRoutes {
		if route == "GET /v1/i18n/:lang" || route == "POST /v1/web/session" { // the code is its key
			continue
		}
		method, path, _ := strings.Cut(route, " ")
		if r := httpDo(t, app, method, strings.ReplaceAll(path, ":name", "a"), "", ""); r.StatusCode != 401 {
			t.Errorf("%s without a key: %d", route, r.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls without a key reached the core: %v", f.calls)
	}
}

// liveCore answers Status from a value the test changes while a stream reads it.
type liveCore struct {
	fakeCore
	active atomic.Value
}

func (l *liveCore) Status(context.Context) (api.Status, error) {
	return api.Status{Active: l.active.Load().(string)}, nil
}
func (l *liveCore) Accounts(context.Context) ([]api.Account, error) { return nil, nil }

func TestEventsStream(t *testing.T) {
	old := heartbeat
	heartbeat = 20 * time.Millisecond
	t.Cleanup(func() { heartbeat = old })
	l := &liveCore{}
	l.active.Store("a")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- t3.Serve(ctx, ln, newHTTPApp(l, fakeHub{}, keys)) }()

	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+keys.read)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal(resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(resp.Body)
	pings := 0
	next := func() api.Snapshot {
		t.Helper()
		var data string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal("stream ended:", err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == ": ping":
				pings++
			case strings.HasPrefix(line, "data: "):
				data = line[len("data: "):]
			case line == "" && data != "":
				var s api.Snapshot
				if err := json.Unmarshal([]byte(data), &s); err != nil {
					t.Fatal(err)
				}
				return s
			}
		}
	}
	if s := next(); s.Status.Active != "a" || s.Accounts == nil {
		t.Fatalf("first event %+v", s)
	}
	l.bus.publish() // nothing changed: the next event must already be the change
	time.Sleep(5 * heartbeat)
	l.active.Store("b")
	l.bus.publish()
	if s := next(); s.Status.Active != "b" {
		t.Fatalf("want the change, got %+v", s)
	}
	if pings == 0 {
		t.Fatal("no heartbeat while idle")
	}

	l.bus.close()
	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("an open stream held up the shutdown")
	}
}

func TestRemoveStaleSocket(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "s.sock")
	staleSocket(t, sock)
	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("precondition: %s is not a socket: %v %v", sock, fi, err)
	}
	file := filepath.Join(d, "f")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(d, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path          string
		wantErr, wantRemain bool
	}{
		{"missing", filepath.Join(d, "none"), false, false},
		{"stale socket", sock, false, false},
		{"regular file", file, true, true},
		{"directory", dir, true, true},
	} {
		err := removeStaleSocket(c.path)
		_, serr := os.Lstat(c.path)
		if (err != nil) != c.wantErr || (serr == nil) != c.wantRemain {
			t.Errorf("%s: err=%v remains=%v", c.name, err, serr == nil)
		}
	}
}
