package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/daemon"
	"github.com/Dudude-bit/ccswap/internal/hook"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/launchd"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/systemd"
	"github.com/Dudude-bit/ccswap/internal/t3"
	"github.com/Dudude-bit/ccswap/internal/web"
)

type fake struct {
	accounts []api.Account
	imp      api.ImportRequest
	impSeen  bool
	dirThere bool
	force    bool
	removed  string
	strategy api.Strategy
	lang     *string
	remote   api.RemoteRequest
	payload  map[string]any
	codes    int
	listen   *string // Status.Listen
	version  string  // Status.Version
	err      error
	delay    time.Duration // Status and Activate answer this late

	mu                              sync.Mutex
	calls                           []string // migrate calls, in order
	export                          api.MigrateExport
	exportErr, commitErr, cancelErr error
	blockExport, blockCommit        chan struct{} // MigrateExport and MigrateCommit wait for these when set
	blockCancel                     chan struct{} // MigrateCancel waits for this when set
	onCommit                        func()        // runs in MigrateCommit before it answers
	imports                         []api.MigrateImportRequest
	importErrs                      []error // MigrateImport's answers in turn; past them it imports
	imported                        api.MigrateImport
	onImport                        func()        // runs in MigrateImport before it answers
	blockImport                     chan struct{} // MigrateImport waits for this when set
}

func (f *fake) call(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fake) migrateCalls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, " ")
}

func (f *fake) Status(context.Context) (api.Status, error) {
	time.Sleep(f.delay)
	return api.Status{Active: "work", Auto: true, Strategy: api.CacheAware, TargetService: "Claude Code-credentials",
		Thresholds: api.Thresholds{FiveHour: 95, SevenDay: 98}, UnknownActive: true, Listen: f.listen, Version: f.version}, f.err
}
func (f *fake) Accounts(context.Context) ([]api.Account, error) { return f.accounts, f.err }
func (f *fake) Import(_ context.Context, r api.ImportRequest) (api.Account, error) {
	f.imp, f.impSeen = r, true
	_, err := os.Stat(r.ConfigDir)
	f.dirThere = err == nil
	return api.Account{Name: r.Name, Email: "x@y.z"}, f.err
}
func (f *fake) Adopt(_ context.Context, r api.AdoptRequest) (api.Account, error) {
	return api.Account{Name: r.Name}, f.err
}
func (f *fake) Patch(_ context.Context, n string, _ api.AccountPatch) (api.Account, error) {
	return api.Account{Name: n}, f.err
}
func (f *fake) Remove(_ context.Context, n string) error { f.removed = n; return f.err }
func (f *fake) Activate(_ context.Context, n string, force bool) (api.Status, error) {
	time.Sleep(f.delay)
	f.force = force
	w := "test-warning"
	return api.Status{Active: n, Warning: &w}, f.err
}
func (f *fake) RefreshLogin(_ context.Context, n string) (api.Account, error) {
	return api.Account{Name: n}, f.err
}
func (f *fake) StartLogin(context.Context, string, bool) (api.LoginLink, error) {
	return api.LoginLink{}, f.err
}
func (f *fake) FinishLogin(_ context.Context, n, _, _ string) (api.Account, error) {
	return api.Account{Name: n}, f.err
}
func (f *fake) CancelLogin(context.Context, string, string) error { return f.err }
func (f *fake) SetAuto(context.Context, bool) error               { return f.err }
func (f *fake) Refresh(context.Context) error                     { return f.err }
func (f *fake) SetThresholds(_ context.Context, t api.Thresholds) (api.Thresholds, error) {
	return t, f.err
}
func (f *fake) Subscribe() (<-chan struct{}, func()) { return nil, func() {} }
func (f *fake) SetRemote(_ context.Context, r api.RemoteRequest) error {
	f.remote = r
	return f.err
}
func (f *fake) SetStrategy(_ context.Context, s api.Strategy) error {
	f.strategy = s
	return f.err
}
func (f *fake) SetLang(_ context.Context, lang string) error {
	f.lang = &lang
	return f.err
}
func (f *fake) Signal(_ context.Context, p map[string]any) error {
	f.payload = p
	return f.err
}
func (f *fake) WebCode(context.Context) (api.WebCode, error) {
	f.codes++
	return api.WebCode{Code: "c0de"}, f.err
}
func (f *fake) UseWebCode(context.Context, string) error { return f.err }
func (f *fake) Version() string                          { return "test" }
func (f *fake) MigrateExport(context.Context, []string) (api.MigrateExport, error) {
	f.call("export")
	if f.blockExport != nil {
		<-f.blockExport
	}
	return f.export, f.exportErr
}
func (f *fake) MigrateCommit(context.Context, string) error {
	f.call("commit")
	if f.onCommit != nil {
		f.onCommit()
	}
	if f.blockCommit != nil {
		<-f.blockCommit
	}
	return f.commitErr
}
func (f *fake) MigrateCancel(context.Context, string) error {
	f.call("cancel")
	if f.blockCancel != nil {
		<-f.blockCancel
	}
	return f.cancelErr
}
func (f *fake) MigrateImport(_ context.Context, r api.MigrateImportRequest) (api.MigrateImport, error) {
	f.call("import")
	if f.onImport != nil {
		f.onImport()
	}
	if f.blockImport != nil {
		<-f.blockImport
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, r)
	if n := len(f.imports); n <= len(f.importErrs) {
		return api.MigrateImport{}, f.importErrs[n-1]
	}
	return f.imported, nil
}

func (f *fake) importReqs() []api.MigrateImportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imports
}

// env makes a temp CCSWAP_HOME (short path: unix socket limit) and optionally serves f on its socket.
func env(t *testing.T, f *fake) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CCSWAP_HOME", home)
	old := newKeychain // a failed login deletes its temp item: never in the real keychain
	newKeychain = func(string) keychain.Store { return keychain.NewFake() }
	t.Cleanup(func() { newKeychain = old })
	oldAsk := askCode // a test that does not answer never reads the real terminal
	askCode = func(string) (string, error) { return "", errNoTTY }
	t.Cleanup(func() { askCode = oldAsk })
	if f == nil {
		return home
	}
	sock := filepath.Join(home, "ccswap.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx, sock, sock+".lock", f) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; ; i++ { // the socket file exists before it listens: wait for an answer, not the file
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if i == 1000 {
			t.Fatal("fake daemon did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return home
}

// syncBuf is a bytes.Buffer safe for concurrent writes (the login child's output copier races the paste echo).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := NewRoot("test")
	var out syncBuf
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := execute(context.Background(), root)
	return out.String(), err
}

// runTo runs ccswap with its stdout going to out and returns its stderr.
func runTo(t *testing.T, out io.Writer, stdin string, args ...string) (string, error) {
	t.Helper()
	root := NewRoot("test")
	var errb syncBuf
	root.SetOut(out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := execute(t.Context(), root)
	return errb.String(), err
}

// runSplit is run with stdout and stderr apart.
func runSplit(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out syncBuf
	stderr, err = runTo(t, &out, stdin, args...)
	return out.String(), stderr, err
}

func TestListTable(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	f := &fake{accounts: []api.Account{
		{Name: "work", Email: "w@x.y", Priority: 1, Enabled: true, Active: true,
			FiveHour: &api.Window{Utilization: 42, ResetsAt: &reset}, SevenDay: &api.Window{Utilization: 7}, Models: []api.ModelWindow{}},
		{Name: "old", Email: "o@x.y", Enabled: true, NeedsLogin: true, Models: []api.ModelWindow{}},
		{Name: "full", Enabled: false, Exhausted: true, Models: []api.ModelWindow{}},
	}}
	env(t, f)
	out, err := run(t, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	header := strings.Split(i18n.T("list.header"), "\t")
	if len(lines) != 5 || !strings.Contains(lines[0], header[len(header)-1]) || lines[4] != i18n.N("list.accounts", 3) {
		t.Fatalf("table:\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "▶") || !strings.Contains(lines[1], "42%") || !strings.Contains(lines[1], "7%") ||
		!strings.Contains(lines[1], reset.Local().Format("15:04")) {
		t.Fatalf("active row: %q", lines[1])
	}
	if !strings.Contains(lines[2], i18n.T("list.needs_login")) || !strings.Contains(lines[3], i18n.T("list.limit")+", "+i18n.T("list.off")) {
		t.Fatalf("states:\n%s", out)
	}
}

func TestListJSONIsAPIShape(t *testing.T) {
	env(t, &fake{accounts: []api.Account{{Name: "a", Models: []api.ModelWindow{}}}})
	out, err := run(t, "", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `[{"active":false`) || !strings.Contains(out, `"name":"a"`) || !strings.Contains(out, `"models":[]`) {
		t.Fatalf("json: %s", out)
	}
}

func TestLoginRunsClaudeWithTempConfigDir(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	rec := filepath.Join(home, "rec")
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR $*\" > "+rec+"\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	out, err := run(t, "", "login", "work", "--priority", "5")
	if err != nil {
		t.Fatal(err, out)
	}
	b, _ := os.ReadFile(rec)
	dir, args, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	if args != "auth login" || filepath.Dir(dir) != home || !strings.HasPrefix(filepath.Base(dir), "login-") {
		t.Fatalf("claude ran as %q", b)
	}
	if !f.impSeen || f.imp.ConfigDir != dir || f.imp.Name != "work" || f.imp.Priority == nil || *f.imp.Priority != 5 {
		t.Fatalf("import %+v", f.imp)
	}
	if !f.dirThere {
		t.Fatal("dir was gone during import")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("temp dir not removed")
	}
	if !strings.Contains(out, "Paste code here") {
		t.Fatalf("no hint: %s", out)
	}
}

func TestLoginTakesPastedCallbackURL(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	rec := filepath.Join(home, "rec")
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nread a; read b; read c; printf '%s|%s|%s' \"$a\" \"$b\" \"$c\" > "+rec+"\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	in := "https://platform.claude.com/oauth/code/callback?code=C1&state=S-1_x\n  C2#S2 \n\n"
	out, err := run(t, in, "login", "work")
	if err != nil {
		t.Fatal(err, out)
	}
	if b, _ := os.ReadFile(rec); string(b) != "C1#S-1_x|C2#S2|" {
		t.Fatalf("claude read %q", b)
	}
	if strings.Count(out, i18n.T("login.checking_code")) != 2 || !strings.Contains(out, i18n.T("login.saving")) {
		t.Fatalf("no progress: %s", out)
	}
}

func TestLoginFailureSkipsImport(t *testing.T) {
	f := &fake{}
	env(t, f)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	_, err := run(t, "", "login", "work")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || f.impSeen {
		t.Fatalf("err %v import %v", err, f.impSeen)
	}
}

func TestUseForwardsForce(t *testing.T) {
	f := &fake{}
	env(t, f)
	out, err := run(t, "", "use", "b", "--force")
	if err != nil || !f.force || !strings.Contains(out, "test-warning") {
		t.Fatalf("%v force=%v %s", err, f.force, out)
	}
	if _, err := run(t, "", "use", "b"); err != nil || f.force {
		t.Fatalf("%v force=%v", err, f.force)
	}
}

func TestUseHints(t *testing.T) {
	f := &fake{err: switcher.ErrNearRefresh}
	env(t, f)
	_, err := run(t, "", "use", "b")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("%v", err)
	}
	f.err = switcher.ErrNoSuchAccount
	_, err = run(t, "", "use", "b")
	if err == nil || err.Error() != i18n.T("tui.no_account", "b") {
		t.Fatalf("%v", err)
	}
}

func TestRemoveConfirmation(t *testing.T) {
	f := &fake{}
	env(t, f)
	if _, err := run(t, "n\n", "remove", "a"); err != nil || f.removed != "" {
		t.Fatalf("declined: %v %q", err, f.removed)
	}
	out, err := run(t, "y\n", "remove", "a")
	if err != nil || f.removed != "a" || !strings.Contains(out, i18n.T("remove.confirm", "a")) {
		t.Fatalf("%v %q %s", err, f.removed, out)
	}
	f.removed = ""
	if _, err := run(t, "", "remove", "b", "--yes"); err != nil || f.removed != "b" {
		t.Fatal(err, f.removed)
	}
}

func TestStatusAndSimpleCommands(t *testing.T) {
	env(t, &fake{})
	setGOOS(t, "darwin")
	out, err := run(t, "", "status")
	if err != nil || !strings.Contains(out, "work") || !strings.Contains(out, "adopt") || !strings.Contains(out, i18n.T("status.keychain", "Claude Code-credentials")) {
		t.Fatalf("%v %s", err, out)
	}
	setGOOS(t, "linux")
	if out, err := run(t, "", "status"); err != nil || !strings.Contains(out, i18n.T("status.cred_file", "Claude Code-credentials")) || strings.Contains(out, i18n.T("status.keychain", "")) {
		t.Fatalf("linux: %v %s", err, out)
	}
	for _, a := range [][]string{{"priority", "a", "3"}, {"enable", "a"}, {"disable", "a"}, {"auto", "on"}, {"auto", "off"}, {"adopt", "a"}} {
		if _, err := run(t, "", a...); err != nil {
			t.Fatalf("%v: %v", a, err)
		}
	}
	if _, err := run(t, "", "auto", "maybe"); err == nil {
		t.Fatal("auto maybe accepted")
	}
}

var sessionEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR"}

// plainSession clears what would make the hook's session one that does not use the managed entry.
func plainSession(t *testing.T) {
	for _, k := range sessionEnv {
		t.Setenv(k, "")
	}
}

func TestSignalPostsPayload(t *testing.T) {
	f := &fake{}
	env(t, f)
	plainSession(t)
	out, err := run(t, `{"hook_event_name":"Stop"}`, "signal")
	if err != nil || out != "" || f.payload["hook_event_name"] != "Stop" {
		t.Fatalf("%v %q %v", err, out, f.payload)
	}
}

func TestSignalIgnoresForeignSessions(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	for _, k := range sessionEnv {
		plainSession(t)
		t.Setenv(k, "/elsewhere")
		f.payload = nil
		if _, err := run(t, `{"error":"rate_limit"}`, "signal"); err != nil || f.payload != nil {
			t.Fatalf("%s set: %v %v", k, err, f.payload)
		}
	}
	// the configured dir is the managed one, whatever the spelling of the default
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[claude]\nconfig_dir = \"/d\"\n"), 0o600)
	plainSession(t)
	if run(t, `{"error":"rate_limit"}`, "signal"); f.payload != nil {
		t.Fatal("default config dir is not the configured /d")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/d")
	if run(t, `{"error":"rate_limit"}`, "signal"); f.payload == nil {
		t.Fatal("session on the configured dir ignored")
	}
}

func TestSignalExitsZeroWithoutDaemon(t *testing.T) {
	env(t, nil)
	start := time.Now()
	out, err := run(t, `{"a":1}`, "signal")
	if err != nil || out != "" || time.Since(start) > 3*time.Second {
		t.Fatalf("%v %q %v", err, out, time.Since(start))
	}
	if out, err := run(t, "garbage", "signal"); err != nil || out != "" {
		t.Fatal(err, out)
	}
}

func TestDaemonDownMessage(t *testing.T) {
	env(t, nil)
	for _, a := range []string{"list", "status"} {
		_, err := run(t, "", a)
		if err == nil || err.Error() != i18n.T("cli.daemon_down") {
			t.Fatalf("%s: %v", a, err)
		}
	}
}

func TestT3(t *testing.T) {
	home := env(t, nil)
	kc := keychain.NewFake()
	old := newKeychain
	newKeychain = func(string) keychain.Store { return kc }
	t.Cleanup(func() { newKeychain = old })
	if _, err := run(t, "", "t3"); err == nil || !strings.Contains(err.Error(), i18n.T("t3.no_key")) {
		t.Fatalf("missing key: %v", err)
	}
	kc.Set("ccswap", "_t3-management-key", nil) // the daemon replaces an empty key
	if _, err := run(t, "", "t3"); err == nil || !strings.Contains(err.Error(), i18n.T("t3.no_key")) {
		t.Fatalf("empty key: %v", err)
	}
	kc.Set("ccswap", "_t3-management-key", []byte("sekret"))
	out, err := run(t, "", "t3")
	if err != nil || !strings.Contains(out, i18n.T("t3.address", "http://127.0.0.1:8318")+i18n.T("t3.key", "sekret")) ||
		!strings.Contains(out, "Add hub") {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[t3]\nlisten = \"0.0.0.0:8318\"\n"), 0o600)
	want := i18n.T("t3.address", "http://127.0.0.1:8318")
	if links, _, _ := listenLinks("0.0.0.0:8318", ""); len(links) > 1 { // another machine needs this one
		want = i18n.T("t3.address", links[1])
	}
	if out, err := run(t, "", "t3"); err != nil || !strings.Contains(out, want) || strings.Contains(out, "0.0.0.0") {
		t.Fatalf("on all interfaces: %v %s", err, out)
	}
}

func TestWeb(t *testing.T) {
	english(t)
	listen := "127.0.0.1:8318"
	f := &fake{listen: &listen, version: "test"}
	home := env(t, f)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[t3]\nlisten = \"0.0.0.0:9999\"\n"), 0o600) // edited since the daemon started
	old := openURL
	var opened []string
	openURL = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openURL = old })
	setGOOS(t, "darwin")
	out, err := run(t, "", "web")
	if err != nil || len(opened) != 1 || opened[0] != "http://127.0.0.1:8318/management.html#code=c0de" ||
		!strings.HasPrefix(out, opened[0]+"\n") || !strings.Contains(out, "works once") || strings.Contains(out, "#key") {
		t.Fatalf("%v %q %s", err, opened, out)
	}
	for _, l := range []string{"0.0.0.0:8318", "[::]:8318"} {
		listen, f.codes = l, 0
		out, err = run(t, "", "web")
		if err != nil || f.codes != 0 || opened[len(opened)-1] != "http://127.0.0.1:8318/management.html" || !strings.Contains(out, "ccswap t3") {
			t.Fatalf("%s: a listener open to the network must get no code: %v %d %q %s", l, err, f.codes, opened, out)
		}
	}
	f.listen = nil
	if _, err := run(t, "", "web"); err == nil || !strings.Contains(err.Error(), "enabled = true") {
		t.Fatalf("listener off: %v", err)
	}
	f.version = "v0.3.0" // a daemon started before an upgrade does not say where it listens
	if _, err := run(t, "", "web"); err == nil || !strings.Contains(err.Error(), "runs ccswap v0.3.0, not this test: restart it with ccswap daemon install") {
		t.Fatalf("old daemon: %v", err)
	}
	env(t, nil)
	if _, err := run(t, "", "web"); err == nil || !strings.Contains(err.Error(), "The daemon is not running") {
		t.Fatalf("daemon down: %v", err)
	}
}

func TestStrategy(t *testing.T) {
	english(t)
	f := &fake{}
	env(t, f)
	out, err := run(t, "", "strategy")
	if err != nil || !strings.HasPrefix(out, "cache-aware: back to a higher priority as soon as it recovers") {
		t.Fatalf("%v %q", err, out)
	}
	if out, err := run(t, "", "strategy", "fill-first"); err != nil || f.strategy != api.FillFirst || !strings.Contains(out, "fill-first") {
		t.Fatalf("%v %q %q", err, out, f.strategy)
	}
	if _, err := run(t, "", "strategy", "round-robin"); err == nil || !strings.Contains(err.Error(), "fill-first or cache-aware") {
		t.Fatal(err)
	}
}

func TestRemote(t *testing.T) {
	english(t)
	f := &fake{}
	env(t, f)
	out, err := run(t, "srv-key\n", "remote", "set", "https://ccswap.example.com")
	if err != nil || f.remote.Url != "https://ccswap.example.com" || f.remote.Key == nil || *f.remote.Key != "srv-key" || !strings.Contains(out, "now come from") {
		t.Fatalf("%v %q %+v", err, out, f.remote)
	}
	if _, err := run(t, "\n", "remote", "set", "https://ccswap.example.com"); err != nil || f.remote.Key != nil {
		t.Fatal("an empty key must keep the stored one:", err, f.remote)
	}
	if out, err := run(t, "", "remote", "off"); err != nil || f.remote.Url != "" || !strings.Contains(out, "Off") {
		t.Fatalf("%v %q", err, out)
	}
	if out, err := run(t, "", "remote"); err != nil || !strings.Contains(out, "asks Anthropic") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestDaemonRunFindingAnotherIsNotRestarted(t *testing.T) { // the supervisor must not loop on it
	offTerminal(t, true)
	env(t, nil)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XPC_SERVICE_NAME", "0")
	dir, err := config.DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	lf, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var ee *ExitError
	if _, err := run(t, "", "daemon", "run"); !errors.As(err, &ee) || ee.Code != daemon.ExitAlreadyRunning || !errors.Is(ee.Err, daemon.ErrAlreadyRunning) {
		t.Fatalf("in a terminal: %v", err)
	}
	if got := ee.Err.Error(); got != "already running: another daemon holds "+filepath.Join(dir, "daemon.lock") {
		t.Fatalf("off a terminal the error goes to daemon.log, in English: %q", got)
	}
	if !strings.Contains(string(systemd.Unit("/b", "/h")), fmt.Sprintf("Restart=on-failure\nRestartPreventExitStatus=%d %d\n", daemon.ExitAlreadyRunning, daemon.ExitConfig)) {
		t.Fatal("the systemd unit restarts a daemon that found another one, or files that do not load")
	}
	t.Setenv("XPC_SERVICE_NAME", launchd.Label)
	if _, err := run(t, "", "daemon", "run"); !errors.As(err, &ee) || ee.Code != 0 {
		t.Fatalf("under launchd, which restarts on any failure: %v", err)
	}
}

func TestDaemonRunThatCannotStartIsNotRestarted(t *testing.T) { // every start would fail the same way
	offTerminal(t, true)
	if daemon.ExitConfig != 78 {
		t.Fatal("EX_CONFIG, which systemd shows as 78/CONFIG")
	}
	write := func(file, body string) func(string) {
		return func(home string) { os.WriteFile(filepath.Join(home, file), []byte(body), 0o600) }
	}
	for _, c := range []struct {
		name, want string
		setup      func(home string)
	}{
		{"config.toml", "poll_interval", write("config.toml", `poll_interval = "1s"`)},
		{"state.json", "state.json", write("state.json", "{")},
		{"a folder too long for the socket", "CCSWAP_HOME", func(home string) { t.Setenv("CCSWAP_HOME", filepath.Join(home, strings.Repeat("h", 110))) }},
		{"a relative CCSWAP_HOME", "CCSWAP_HOME", func(string) { t.Setenv("CCSWAP_HOME", "rel") }},
		{"a folder that cannot be made", "not a directory", func(home string) {
			os.WriteFile(filepath.Join(home, "file"), nil, 0o600)
			t.Setenv("CCSWAP_HOME", filepath.Join(home, "file", "ccswap"))
		}},
		{"no home folder", "HOME", func(string) { t.Setenv("HOME", "") }},
	} {
		c.setup(env(t, nil))
		t.Setenv("XPC_SERVICE_NAME", "0")
		var ee *ExitError
		if _, err := run(t, "", "daemon", "run"); !errors.As(err, &ee) || ee.Code != daemon.ExitConfig || !strings.Contains(err.Error(), c.want) ||
			!strings.Contains(err.Error(), "will not restart on its own: fix this, then run ccswap daemon install") {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Setenv("XPC_SERVICE_NAME", launchd.Label)
		if _, err := run(t, "", "daemon", "run"); !errors.As(err, &ee) || ee.Code != 0 || ee.Err == nil {
			t.Fatalf("%s: under launchd, which restarts on any failure: %v", c.name, err)
		}
	}
}

func TestStatusNamesWhatStopsTheDaemon(t *testing.T) {
	setGOOS(t, "linux")
	t.Setenv("HOME", t.TempDir())
	linuxService(t, nil)
	home := env(t, nil)
	if out, _ := run(t, "", "daemon", "status"); anyArgs("daemon.cannot_start").MatchString(out) {
		t.Fatalf("a daemon merely down blamed on its files:\n%s", out)
	}
	os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600)
	_, serr := store.Open(home, nil)
	for _, args := range [][]string{{"daemon", "status"}, {"doctor"}} {
		if out, _ := run(t, "", args...); !strings.Contains(out, "✗ "+i18n.T("doctor.daemon_down")+"\n✗ "+i18n.T("daemon.cannot_start", serr)+"\n") {
			t.Fatalf("%v: a corrupt state.json not named:\n%s", args, out)
		}
	}
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(`poll_interval = "1s"`), 0o600)
	for _, args := range [][]string{{"daemon", "status"}, {"doctor"}} {
		if _, err := run(t, "", args...); err == nil || !anyArgs("daemon.cannot_start").MatchString(err.Error()) || !strings.Contains(err.Error(), "poll_interval") {
			t.Fatalf("%v: a config.toml that does not load not named: %v", args, err)
		}
	}
	t.Setenv("CCSWAP_HOME", filepath.Join(home, strings.Repeat("h", 110)))
	if out, _ := run(t, "", "daemon", "status"); !strings.Contains(out, "✗ "+i18n.T("daemon.cannot_start", "")) || !strings.Contains(out, "CCSWAP_HOME") {
		t.Fatalf("a folder too long for the socket not named:\n%s", out)
	}
}

func TestInstallRefusesCorruptState(t *testing.T) { // the daemon it starts would stop at once
	home := env(t, nil)
	setGOOS(t, "linux")
	t.Setenv("HOME", t.TempDir())
	calls := linuxService(t, nil)
	os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600)
	if _, err := run(t, "", "daemon", "install"); err == nil || !strings.Contains(err.Error(), "state.json") || len(*calls) != 0 {
		t.Fatalf("%v %q", err, *calls)
	}
}

func TestUninstallWithBrokenFiles(t *testing.T) { // the service needs neither file
	home := env(t, nil)
	setGOOS(t, "linux")
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	calls := linuxService(t, nil)
	bin, _ := selfBin()
	unit, settings := filepath.Join(systemd.Dir, systemd.Label), claudecode.SettingsPath(uh, "")
	os.WriteFile(unit, systemd.Unit(bin, home), 0o600)
	os.MkdirAll(filepath.Dir(settings), 0o700)
	if err := hook.Install(settings, hook.Command(bin)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(`poll_interval = "1s"`), 0o600)
	os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600)
	_, cerr := config.Load(home)
	out, err := run(t, "", "daemon", "uninstall")
	if err != nil || !strings.Contains(out, i18n.T("daemon.uninstall_bad_config", cerr)) || !strings.Contains(out, i18n.T("daemon.uninstall_kept")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) || !strings.Contains(strings.Join(*calls, "|"), "systemctl --user disable --now "+systemd.Label) {
		t.Fatalf("the service stays: %v %q", err, *calls)
	}
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), "StopFailure") {
		t.Fatalf("hook left: %s", b)
	}
}

// offTerminal makes stderr a terminal or not for the test, which starts in Russian.
func offTerminal(t *testing.T, off bool) {
	old := stderrIsTerminal
	stderrIsTerminal = func() bool { return !off }
	t.Cleanup(func() { stderrIsTerminal = old; i18n.Set("ru") })
	i18n.Set("ru")
}

// TestDaemonRunErrorsInEnglish: launchd and systemd keep daemon run's stderr in daemon.log, so off a terminal
// its errors, its own and cobra's, are English whatever language ccswap speaks; in a terminal they are not.
func TestDaemonRunErrorsInEnglish(t *testing.T) {
	home := env(t, nil)
	for _, c := range []struct{ config, args, en, ru string }{
		{"[thresholds]\nfive_hour = 200\n", "", "config.toml: thresholds.five_hour must be in (0, 100]", i18n.In("ru", "config.file_error", i18n.Err("config.five_hour_range"))},
		{"[remote]\nurl = 'ftp://x'\n", "", "config.toml: remote.url: want http(s)://host[:port]", i18n.In("ru", "config.remote_url_error", i18n.Err("config.remote_bad_url"))},
		{"[bogus]\nx = 1\n", "", "config.toml: unknown key:\n", i18n.In("ru", "config.unknown_key", "")},
		{"", "extra", `unknown command "extra" for "ccswap daemon run"`, i18n.In("ru", "cobra.unknown_command", "extra", "ccswap daemon run", "")},
		{"", "--bogus", "unknown flag: --bogus", i18n.In("ru", "cobra.unknown_flag", "--bogus")},
	} {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(c.config), 0o600); err != nil {
			t.Fatal(err)
		}
		for off, want := range map[bool]string{true: c.en, false: c.ru} {
			offTerminal(t, off)
			if _, err := run(t, "", append([]string{"daemon", "run"}, strings.Fields(c.args)...)...); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("%q %q, off a terminal %v: %v, want %q", c.config, c.args, off, err, want)
			}
		}
	}
	offTerminal(t, true)
	if _, err := run(t, "", "list", "extra"); err == nil || !strings.Contains(err.Error(), i18n.In("ru", "cobra.unknown_command", "extra", "ccswap list", "")) {
		t.Errorf("only daemon run speaks English off a terminal: %v", err)
	}
}

func TestPageLinks(t *testing.T) {
	for listen, want := range map[string]struct {
		first string
		local bool
	}{
		"127.0.0.1:8318":   {"http://127.0.0.1:8318/management.html", true},
		"[::1]:8318":       {"http://[::1]:8318/management.html", true},
		"localhost:8318":   {"http://localhost:8318/management.html", true},
		"100.64.0.7:9":     {"http://100.64.0.7:9/management.html", false},
		"0.0.0.0:8318":     {"http://127.0.0.1:8318/management.html", false},
		":8318":            {"http://127.0.0.1:8318/management.html", false},
		"[::]:8318":        {"http://127.0.0.1:8318/management.html", false},
		"mac.tailnet:8318": {"http://mac.tailnet:8318/management.html", false},
	} {
		links, local, err := listenLinks(listen, web.Page)
		if err != nil || links[0] != want.first || local != want.local {
			t.Errorf("%s: %v %v %v", listen, links, local, err)
		}
		if ip := net.ParseIP(strings.Trim(listen[:strings.LastIndex(listen, ":")], "[]")); ip != nil && !ip.IsUnspecified() && len(links) != 1 {
			t.Errorf("%s: a specific address must give one link: %v", listen, links)
		}
	}
	if _, _, err := listenLinks("8318", web.Page); err == nil {
		t.Fatal("bad listen accepted")
	}
}

func TestLoginInterruptCleansUp(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	kc := keychain.NewFake()
	old := newKeychain
	newKeychain = func(string) keychain.Store { return kc }
	t.Cleanup(func() { newKeychain = old })
	rec := filepath.Join(home, "rec")
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > "+rec+"\nexec sleep 30\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	stale := filepath.Join(home, "login-stale")
	os.Mkdir(stale, 0o700)
	os.Chtimes(stale, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	done := make(chan struct{})
	go func() { // a SIGINT once claude runs; none after the test, when nothing catches it
		defer close(done)
		for range 500 {
			if b, _ := os.ReadFile(rec); strings.HasSuffix(string(b), "\n") {
				kc.Set(claudecode.ServiceName(strings.TrimSpace(string(b))), claudecode.KeychainAccount(), []byte("x"))
				interruptSelf(t)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("claude never started")
	}()
	_, err := run(t, "", "login", "work")
	<-done
	if err == nil || f.impSeen {
		t.Fatalf("err %v import %v", err, f.impSeen)
	}
	b, _ := os.ReadFile(rec)
	dir := strings.TrimSpace(string(b))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("login dir left behind")
	}
	if _, err := kc.Get(claudecode.ServiceName(dir), claudecode.KeychainAccount()); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("keychain item left: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale dir not swept")
	}
}

func TestCleanupSparesLiveLogin(t *testing.T) {
	setGOOS(t, "linux")
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	home := t.TempDir()
	kc := keychain.NewFiles(filepath.Join(home, "vault"))
	target := claudecode.ServiceName("")
	os.MkdirAll(filepath.Dir(target), 0o700)
	os.WriteFile(target, []byte("live"), 0o600)
	link := filepath.Join(home, "login-link")
	os.Symlink(filepath.Dir(target), link)
	stale := filepath.Join(home, "login-stale")
	os.Mkdir(stale, 0o700)
	os.WriteFile(filepath.Join(stale, ".credentials.json"), []byte("tmp"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	for _, d := range []string{stale, filepath.Dir(target)} {
		os.Chtimes(d, old, old)
	}
	daemon.SweepLogins(home, target, kc)
	daemon.DropLogin(link, target, kc, false)
	if b, err := os.ReadFile(target); err != nil || string(b) != "live" {
		t.Fatalf("live login deleted: %q %v", b, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale login dir not swept")
	}
}

func TestSignalDoesNotHangOnStdin(t *testing.T) {
	env(t, nil)
	pr, pw := io.Pipe()
	defer pw.Close()
	root := NewRoot("test")
	root.SetIn(pr)
	root.SetArgs([]string{"signal"})
	start := time.Now()
	if err := root.ExecuteContext(context.Background()); err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("%v %v", err, time.Since(start))
	}
}

func TestDoctorAndInstall(t *testing.T) {
	setGOOS(t, "darwin")
	env(t, &fake{})
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/elsewhere")
	oldRun, oldDir, oldKC := launchd.Run, launchd.Dir, keychainHas
	t.Cleanup(func() { launchd.Run, launchd.Dir, keychainHas = oldRun, oldDir, oldKC })
	launchd.Dir = t.TempDir()
	loaded := false
	launchd.Run = func(args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			loaded = true
		}
		if args[0] == "print" && !loaded {
			return nil, &exec.ExitError{}
		}
		return nil, nil
	}
	keychainHas = func(string) bool { return true }
	out, err := run(t, "", "doctor")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, "✗ "+i18n.T("doctor.claude_not_found")) || !strings.Contains(out, "CLAUDE_CONFIG_DIR") ||
		!strings.Contains(out, "✓ "+i18n.T("doctor.daemon_up")) || !strings.Contains(out, "✗ LaunchAgent "+i18n.T("launchd.not_loaded")) {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if out, _ = run(t, "", "doctor"); strings.Contains(out, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("false alarm:\n%s", out)
	}
	if out, err = run(t, "", "daemon", "install"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	settings := claudecode.SettingsPath(uh, "")
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "StopFailure") {
		t.Fatalf("hook missing: %s", b)
	}
	if out, _ = run(t, "", "daemon", "status"); strings.Contains(out, "✗") {
		t.Fatalf("status:\n%s", out)
	}
	if out, err = run(t, "", "daemon", "uninstall"); err != nil || !strings.Contains(out, i18n.T("daemon.uninstall_kept")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), "StopFailure") {
		t.Fatalf("hook left: %s", b)
	}
}

func TestLoginChecksDaemonFirst(t *testing.T) {
	home := env(t, nil)
	rec := filepath.Join(home, "rec")
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+rec+"\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	if _, err := run(t, "", "login", "work"); err == nil || err.Error() != errDown.Error() {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec); err == nil {
		t.Fatal("claude ran although the daemon is down: the login code would be wasted")
	}
}

func TestHintsForEveryCode(t *testing.T) {
	f := &fake{}
	env(t, f)
	for _, tc := range []struct {
		err  error
		args []string
		want string
		bad  string
	}{
		{switcher.ErrCannotIdentify, []string{"use", "b"}, "ccswap use b --force", ""},
		{switcher.ErrEntryExpired, []string{"use", "b"}, i18n.T("tui.claude_idle"), "--force"},
		{switcher.ErrCannotIdentify, []string{"adopt", "b"}, i18n.T("tui.reason", switcher.ErrCannotIdentify), "--force"},
		{store.ErrDuplicate, []string{"adopt", "b"}, i18n.T("tui.account_exists"), ""},
		{daemon.ErrRemoveActive, []string{"remove", "b", "--yes"}, i18n.T("tui.remove_active"), ""},
		{daemon.ErrInvalidName, []string{"adopt", "B"}, "a-z", ""},
	} {
		f.err = tc.err
		_, err := run(t, "", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || (tc.bad != "" && strings.Contains(err.Error(), tc.bad)) {
			t.Errorf("%v %v: %v", tc.args, tc.err, err)
		}
	}
}

func TestSlowDaemon(t *testing.T) {
	oldR, oldW := readTimeout, writeTimeout
	t.Cleanup(func() { readTimeout, writeTimeout = oldR, oldW })
	f := &fake{delay: 300 * time.Millisecond}
	env(t, f)
	readTimeout, writeTimeout = 100*time.Millisecond, 5*time.Second
	if _, err := run(t, "", "use", "b"); err != nil {
		t.Fatal("a mutating call must wait longer than a read:", err)
	}
	if _, err := run(t, "", "status"); err == nil || !strings.Contains(err.Error(), i18n.T("tui.daemon_slow")) {
		t.Fatal(err)
	}
	writeTimeout = 100 * time.Millisecond
	if _, err := run(t, "", "use", "b"); err == nil || !strings.Contains(err.Error(), i18n.T("tui.daemon_slow")) {
		t.Fatal(err)
	}
}

func TestDoctorFlagsStaleInstall(t *testing.T) {
	setGOOS(t, "darwin")
	env(t, &fake{})
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	oldRun, oldDir, oldKC := launchd.Run, launchd.Dir, keychainHas
	t.Cleanup(func() { launchd.Run, launchd.Dir, keychainHas = oldRun, oldDir, oldKC })
	launchd.Dir = t.TempDir()
	launchd.Run = func(args ...string) ([]byte, error) { return nil, nil }
	keychainHas = func(string) bool { return true }
	os.WriteFile(filepath.Join(launchd.Dir, launchd.Label+".plist"), launchd.Plist("/old/ccswap", uh), 0o644)
	settings := claudecode.SettingsPath(uh, "")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	os.WriteFile(settings, []byte(`{"hooks":{"StopFailure":[{"hooks":[{"type":"command","command":"/old/ccswap signal"}]}]}}`), 0o600)
	out, _ := run(t, "", "daemon", "status")
	bin, _ := selfBin()
	if !strings.Contains(out, "✗ "+i18n.T("doctor.hook_old_path")) || !strings.Contains(out, "✗ "+i18n.T("doctor.service_runs_other", "LaunchAgent", "/old/ccswap", bin)) {
		t.Fatalf("a moved binary must be flagged:\n%s", out)
	}
}

func TestUseForceHintOnlyWhereForceHelps(t *testing.T) {
	f := &fake{err: switcher.ErrNearRefresh}
	env(t, f)
	if _, err := run(t, "", "use", "b", "--force"); err == nil || strings.Contains(err.Error(), "--force") {
		t.Fatalf("already forced: %v", err)
	}
	f.err = switcher.ErrTargetRefreshing
	if _, err := run(t, "", "use", "b"); err == nil || strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), i18n.T("tui.login_refreshing", "b")) {
		t.Fatalf("%v", err)
	}
	f.err = switcher.ErrEntryChanged
	if _, err := run(t, "", "use", "b"); err == nil || strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), i18n.T("tui.claude_refreshing")) {
		t.Fatalf("%v", err)
	}
}

func setGOOS(t *testing.T, g string) {
	old := claudecode.GOOS
	claudecode.GOOS = g
	t.Cleanup(func() { claudecode.GOOS = old })
}

func TestLoginStripsSecureStorageDir(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/x")
	rec := filepath.Join(home, "rec")
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nenv > "+rec+"\n"), 0o755)
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	if out, err := run(t, "", "login", "work"); err != nil {
		t.Fatal(err, out)
	}
	b, _ := os.ReadFile(rec)
	if strings.Contains(string(b), "CLAUDE_SECURESTORAGE_CONFIG_DIR") || !strings.Contains(string(b), "CLAUDE_CONFIG_DIR="+f.imp.ConfigDir+"\n") {
		t.Fatalf("env:\n%s", b)
	}
}

// linuxService points the systemd seams at a temp dir and a spy (nil reply = success).
func linuxService(t *testing.T, reply func(name string, args ...string) ([]byte, error)) *[]string {
	t.Helper()
	oldRun, oldDir, oldUser := systemd.Run, systemd.Dir, systemd.User
	t.Cleanup(func() { systemd.Run, systemd.Dir, systemd.User = oldRun, oldDir, oldUser })
	systemd.Dir = t.TempDir()
	systemd.User = func() string { return "bob" }
	var calls []string
	systemd.Run = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if reply != nil {
			return reply(name, args...)
		}
		return nil, nil
	}
	return &calls
}

func TestCLIInstallLinuxUsesSystemd(t *testing.T) {
	env(t, &fake{})
	setGOOS(t, "linux")
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	t.Setenv("PATH", t.TempDir())
	linger := true
	calls := linuxService(t, func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "loginctl" && args[0] == "enable-linger":
			return nil, errors.New("denied")
		case name == "loginctl":
			return []byte(map[bool]string{true: "Linger=yes", false: "Linger=no"}[linger]), nil
		case args[1] == "is-active":
			return []byte("active"), nil
		}
		return nil, nil
	})
	out, err := run(t, "", "daemon", "install")
	if err != nil || !strings.Contains(out, i18n.T("systemd.service")+" "+systemd.Label+" "+i18n.T("systemd.installed_running")) ||
		!strings.Contains(out, i18n.T("systemd.linger_failed", "bob")+"\n") {
		t.Fatalf("%v\n%s\n%q", err, out, *calls)
	}
	if _, err := os.Stat(filepath.Join(systemd.Dir, systemd.Label)); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, "", "doctor"); !strings.Contains(out, "✓ "+i18n.T("doctor.linger_on")) || !strings.Contains(out, "✓ "+i18n.T("systemd.service")+" "+i18n.T("systemd.loaded")) {
		t.Fatalf("doctor:\n%s", out)
	}
	linger = false
	if out, _ := run(t, "", "doctor"); !strings.Contains(out, "✗ "+i18n.T("doctor.linger_off", "bob")) {
		t.Fatalf("doctor:\n%s", out)
	}
	if out, err := run(t, "", "daemon", "uninstall"); err != nil || !strings.Contains(out, i18n.T("systemd.service")+" "+i18n.T("systemd.removed")) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestNoSystemd(t *testing.T) {
	env(t, &fake{})
	setGOOS(t, "linux")
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	calls := linuxService(t, func(name string, _ ...string) ([]byte, error) {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	})
	if out, err := run(t, "", "daemon", "install"); err == nil || !strings.Contains(err.Error(), i18n.T("systemd.no_user_session")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(systemd.Dir, systemd.Label)); !os.IsNotExist(err) {
		t.Fatal("orphan unit file")
	}
	for _, args := range [][]string{{"doctor"}, {"daemon", "status"}} {
		*calls = nil
		out, _ := run(t, "", args...)
		if strings.Count(out, "✗ "+i18n.T("systemd.not_found")+"\n") != 1 ||
			strings.Contains(out, "linger") || strings.Contains(out, i18n.T("systemd.service")) || strings.Contains(strings.Join(*calls, "|"), "loginctl") {
			t.Fatalf("%v:\n%s\n%q", args, out, *calls)
		}
	}
}

func TestDoctorLinuxCredentialsFile(t *testing.T) {
	env(t, &fake{})
	setGOOS(t, "linux")
	uh := t.TempDir()
	t.Setenv("HOME", uh)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(uh, "xdg"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/elsewhere")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	linuxService(t, nil)
	cred := filepath.Join(uh, ".claude", ".credentials.json")
	mismatch := i18n.T("doctor.config_dir_mismatch", "/elsewhere", i18n.T("doctor.the_cred_file"), claudecode.ServiceName("/elsewhere"), cred, "")
	if out, _ := run(t, "", "doctor"); !strings.Contains(out, mismatch) {
		t.Fatalf("mismatch wording:\n%s", out)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	doctor := func() string { out, _ := run(t, "", "doctor"); return out }

	if out := doctor(); !strings.Contains(out, i18n.T("doctor.cred_file", cred, i18n.T("doctor.missing"))) {
		t.Fatalf("missing:\n%s", out)
	}
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte("{}"), 0o600)
	out := doctor()
	if !strings.Contains(out, i18n.T("doctor.cred_file", cred, i18n.T("doctor.present"))) ||
		anyArgs("doctor.cred_file_mode").MatchString(out) || anyArgs("doctor.cred_file_symlink").MatchString(out) {
		t.Fatalf("ok:\n%s", out)
	}
	if anyArgs("doctor.cred_file_owner").MatchString(out) {
		t.Fatalf("own file flagged:\n%s", out)
	}
	oldUID := getuid
	t.Cleanup(func() { getuid = oldUID })
	getuid = func() int { return os.Getuid() + 1 }
	if out := doctor(); !strings.Contains(out, "✗ "+i18n.T("doctor.cred_file_owner", os.Getuid())) {
		t.Fatalf("owner:\n%s", out)
	}
	getuid = oldUID
	os.Chmod(cred, 0o644)
	if out := doctor(); !strings.Contains(out, i18n.T("doctor.cred_file_mode", os.FileMode(0o644))) {
		t.Fatalf("mode:\n%s", out)
	}
	os.Remove(cred)
	target := filepath.Join(uh, "real.json")
	os.WriteFile(target, []byte("{}"), 0o600)
	os.Symlink(target, cred)
	if out := doctor(); !strings.Contains(out, i18n.T("doctor.cred_file_symlink", cred)) {
		t.Fatalf("symlink:\n%s", out)
	}
}

func TestDoctorWarnsSecureStorageDir(t *testing.T) {
	env(t, &fake{})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/x")
	old, oldRun, oldDir := keychainHas, launchd.Run, launchd.Dir
	t.Cleanup(func() { keychainHas, launchd.Run, launchd.Dir = old, oldRun, oldDir })
	keychainHas = func(string) bool { return true }
	launchd.Dir = t.TempDir()
	launchd.Run = func(...string) ([]byte, error) { return nil, nil }
	linuxService(t, nil)
	out, err := run(t, "", "doctor")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, "✗ "+i18n.T("doctor.securestorage_set")) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestPlatformKeychainLinuxIsFiles(t *testing.T) {
	setGOOS(t, "linux")
	f, ok := platformKeychain("/h").(*keychain.Files)
	if !ok || f.Root != filepath.Join("/h", "vault") {
		t.Fatalf("%#v", f)
	}
	setGOOS(t, "darwin")
	if _, ok := platformKeychain("/h").(*keychain.Security); !ok {
		t.Fatal("darwin is not Security")
	}
}

// 6eb169c (the first Linux build) kept items at <home>/vault/<PathEscape(service)>/<PathEscape(account)>
// and Claude Code's entry in ~/.claude/.credentials.json: an upgrade must read them where they are.
func TestLinuxReadsEarlierLinuxVault(t *testing.T) {
	setGOOS(t, "linux")
	uh, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", uh)
	at := map[[2]string]string{
		{claudecode.ServiceName(""), claudecode.KeychainAccount()}: filepath.Join(uh, ".claude", ".credentials.json"),
	}
	for _, a := range []string{"work", "acc-2", t3.KeyAccount, "_fingerprint", "_remote-key"} {
		at[[2]string{store.VaultService, a}] = filepath.Join(home, "vault", url.PathEscape(store.VaultService), url.PathEscape(a))
	}
	kc := platformKeychain(home)
	for k, p := range at {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(p), 0o600)
		if b, err := kc.Get(k[0], k[1]); err != nil || string(b) != p {
			t.Errorf("%q: %q %v", k, b, err)
		}
	}
}

func TestEnglishOutput(t *testing.T) {
	english(t)
	f := &fake{accounts: []api.Account{{Name: "work", Email: "w@x.y", Enabled: true}}}
	env(t, f)
	out, err := run(t, "", "list")
	if err != nil || !strings.Contains(out, "name") || !strings.Contains(out, "email") || !strings.Contains(out, "resets") || strings.ContainsFunc(out, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
		t.Fatalf("%v %s", err, out)
	}
	out, err = run(t, "", "status")
	if err != nil || !strings.Contains(out, "Active account: work") || !strings.Contains(out, "Auto-switching: on") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err = run(t, "", "use", "work"); err != nil || !strings.Contains(out, "Active account: work") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestEnglishDaemonDown(t *testing.T) {
	english(t)
	env(t, nil)
	_, err := run(t, "", "list")
	if err == nil || err.Error() != "The daemon is not running: ccswap daemon install (or ccswap daemon run)" {
		t.Fatal(err)
	}
}

func TestLang(t *testing.T) {
	english(t)
	f := &fake{}
	env(t, f)
	out, err := run(t, "", "lang", "ru")
	if err != nil || f.lang == nil || *f.lang != "ru" || !strings.Contains(out, i18n.In("ru", "lang.pinned", i18n.Name("ru"))) {
		t.Fatalf("%v %q %v", err, out, f.lang)
	}
	if _, err := run(t, "", "lang", "qaa"); err == nil || err.Error() != i18n.T("lang.unknown", "qaa", codes(", ")) {
		t.Fatal(err)
	}
	if _, err := run(t, "", "lang", "RU_ru"); err != nil || *f.lang != "ru-RU" {
		t.Fatal("a pin goes to the daemon as a canonical tag:", err, *f.lang)
	}
}

func TestLangWithoutDaemonWritesConfig(t *testing.T) {
	english(t)
	home := env(t, nil)
	out, err := run(t, "", "lang", "EN_us")
	if err != nil || !strings.Contains(out, "English, pinned in config.toml") {
		t.Fatalf("%v %q", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if !strings.Contains(string(b), "lang = 'en-US'") {
		t.Fatalf("config.toml keeps the canonical tag: %s", b)
	}
}
