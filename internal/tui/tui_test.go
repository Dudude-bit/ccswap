package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// TestMain runs in Russian: tests compare texts with i18n.T, and in a language other than English that
// also catches text printed without the catalog.
func TestMain(m *testing.M) {
	i18n.Set("ru")
	os.Exit(m.Run())
}

func english(t *testing.T) {
	t.Helper()
	i18n.Set(i18n.EN)
	t.Cleanup(func() { i18n.Set("ru") })
}

type call struct {
	name  string
	force bool
}

type fake struct {
	st       api.Status
	accts    []api.Account
	actErr   []error // consumed per Activate call
	acts     []call
	autos    []bool
	statErr  error
	refreshN int
	prios    map[string]int
	enabled  map[string]bool
	strategy api.Strategy
	editErr  error
}

func (f *fake) Status(context.Context) (api.Status, error) { return f.st, f.statErr }
func (f *fake) Accounts(context.Context) ([]api.Account, error) {
	return f.accts, f.statErr
}
func (f *fake) Activate(_ context.Context, name string, force bool) error {
	f.acts = append(f.acts, call{name, force})
	if len(f.actErr) > 0 {
		e := f.actErr[0]
		f.actErr = f.actErr[1:]
		return e
	}
	return nil
}
func (f *fake) SetAuto(_ context.Context, on bool) error { f.autos = append(f.autos, on); return nil }
func (f *fake) Refresh(context.Context) error            { f.refreshN++; return nil }
func (f *fake) SetPriority(_ context.Context, name string, p int) error {
	if f.prios == nil {
		f.prios = map[string]int{}
	}
	f.prios[name] = p
	return f.editErr
}
func (f *fake) SetEnabled(_ context.Context, name string, on bool) error {
	if f.enabled == nil {
		f.enabled = map[string]bool{}
	}
	f.enabled[name] = on
	return f.editErr
}
func (f *fake) SetStrategy(_ context.Context, s api.Strategy) error { f.strategy = s; return f.editErr }

var ansi = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func text(m tea.Model) string { return ansi.ReplaceAllString(m.View().Content, "") }

func win(u float64, reset time.Time) *api.Window {
	return &api.Window{Utilization: u, ResetsAt: &reset}
}

func fixture() *fake {
	t := time.Now().Add(2 * time.Hour)
	free := t.Add(time.Hour) // a limit hit outlasts the window
	return &fake{
		st: api.Status{Active: "work", Auto: true, Strategy: api.CacheAware, Thresholds: api.Thresholds{FiveHour: 95, SevenDay: 98}},
		accts: []api.Account{
			{Name: "work", Email: "work@example.com", Active: true, Enabled: true, FiveHour: win(71, t), SevenDay: win(20, t)},
			{Name: "home", Email: "home@example.com", Enabled: true, FiveHour: win(96, t), SevenDay: win(50, t), Exhausted: true, FreeAt: &free},
			{Name: "side", Email: "side@example.com", Enabled: true, NeedsLogin: true},
			{Name: "old", Email: "old@example.com", Enabled: false, FiveHour: win(5, t), SevenDay: win(8, t)},
		},
	}
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func send(m tea.Model, msg tea.Msg) (tea.Model, tea.Cmd) { return m.Update(msg) }

// loaded returns a model that has received the fixture's first poll.
func loaded(t *testing.T, f *fake) tea.Model { return loadedAt(t, f, 80) }

func loadedAt(t *testing.T, f *fake, width int) tea.Model {
	t.Helper()
	m := New(f, "/bin/ccswap")
	m, _ = send(m, tea.WindowSizeMsg{Width: width, Height: 40})
	m, _ = send(m, m.(*Model).poll()())
	return m
}

func fits(t *testing.T, v string, width int) {
	t.Helper()
	for _, l := range strings.Split(v, "\n") {
		if len([]rune(l)) > width {
			t.Errorf("line over %d cols: %q", width, l)
		}
	}
}

// run executes cmd and feeds its message back.
func run(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	m, _ = send(m, cmd())
	return m
}

func TestPassedFreeAtShowsNoTime(t *testing.T) { // the daemon is down, or its next check has not come yet
	f := fixture()
	past := time.Now().Add(-time.Minute)
	f.accts[1].FreeAt = &past
	v := text(loadedAt(t, f, 100))
	if !strings.Contains(v, "✕ "+i18n.T("tui.limit_reached")) || strings.Contains(v, i18n.T("tui.out_until", "")) {
		t.Fatal("a passed free_at shown as a time:\n" + v)
	}
}

func TestWideViewShowsEveryState(t *testing.T) {
	f := fixture()
	v := text(loadedAt(t, f, 100))
	t.Log("\n" + v)
	for _, want := range []string{"ccswap", i18n.T("tui.auto") + " " + i18n.T("tui.on"), i18n.T("tui.strategy") + " cache-aware",
		i18n.T("tui.accounts"), i18n.T("tui.five_hours_left"), i18n.T("tui.col_reset"), "▶ work", "29%", i18n.T("tui.in_hours", 2),
		"✕ " + i18n.T("tui.out_until", clock(*f.accts[1].FreeAt, time.Now())), "✕ " + i18n.T("tui.needs_login"), i18n.T("tui.disabled"), "▶ " + i18n.T("tui.in_use"),
		"→ " + i18n.T("tui.next"), "work  " + i18n.T("tui.in_use"), "work@example.com", "↑↓ " + i18n.T("tui.select"),
		"+ - " + i18n.T("tui.priority"), "? " + i18n.T("tui.all_keys")} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	fits(t, v, 100)
}

func TestNarrowViewMarksStatesWithoutWords(t *testing.T) {
	v := text(loadedAt(t, fixture(), 64))
	t.Log("\n" + v)
	if strings.Contains(v, i18n.T("tui.needs_login")) {
		t.Errorf("a narrow table has no state column:\n%s", v)
	}
	for _, name := range []string{"home", "side"} {
		if !regexp.MustCompile(name + `.*✕`).MatchString(v) {
			t.Errorf("%s has no state glyph:\n%s", name, v)
		}
	}
	if !regexp.MustCompile(`old.*○`).MatchString(v) || !strings.Contains(v, i18n.T("tui.in_hours", 2)) {
		t.Errorf("narrow view:\n%s", v)
	}
	fits(t, v, 64)
}

func TestRowsFollowPriorityAndNavigation(t *testing.T) {
	f := fixture()
	f.accts[3].Priority = 50 // old
	f.accts[1].Priority = 20 // home
	m := loaded(t, f)
	var names []string
	for _, a := range m.(*Model).accts {
		names = append(names, a.Name)
	}
	if strings.Join(names, " ") != "old home work side" {
		t.Fatalf("order %v", names)
	}
	m, _ = send(m, key("down"))
	if m.(*Model).sel != "side" {
		t.Fatalf("down from work must reach the row below it, got %s", m.(*Model).sel)
	}
}

func TestPriorityKeysMoveTheRowAtOnce(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	m, _ = send(m, key("down"))
	m, cmd := send(m, key("+"))
	if m.(*Model).accts[0].Name != "home" || m.(*Model).sel != "home" {
		t.Fatalf("home must lead before the daemon answers: %v", m.(*Model).accts[0].Name)
	}
	m = run(t, m, cmd)
	if f.prios["home"] != 10 || !strings.Contains(text(m), i18n.T("tui.priority_set", "home", 10)) {
		t.Fatalf("prios %v\n%s", f.prios, text(m))
	}
	m, cmd = send(m, key("-"))
	run(t, m, cmd)
	if f.prios["home"] != 0 {
		t.Fatalf("prios %v", f.prios)
	}
}

func TestSpaceAndStrategyKeys(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	for range 3 {
		m, _ = send(m, key("down"))
	}
	m, cmd := send(m, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = run(t, m, cmd)
	if !f.enabled["old"] || !strings.Contains(text(m), i18n.T("tui.auto_back", "old")) {
		t.Fatalf("enabled %v\n%s", f.enabled, text(m))
	}
	m, cmd = send(m, key("s"))
	if m.(*Model).st.Strategy != api.FillFirst {
		t.Fatal("the strategy must flip at once")
	}
	run(t, m, cmd)
	if f.strategy != api.FillFirst {
		t.Fatalf("strategy %q", f.strategy)
	}
}

func TestFailedEditShowsTheErrorAndPolls(t *testing.T) {
	f := fixture()
	f.editErr = errors.New("no such account")
	m := loaded(t, f)
	m, cmd := send(m, key("+"))
	m, cmd = send(m, cmd())
	if !strings.Contains(text(m), "no such account") || !m.(*Model).flashErr || cmd == nil {
		t.Fatalf("a failed edit must say why and poll to undo it:\n%s", text(m))
	}
}

func TestHelpReplacesThePanel(t *testing.T) {
	m := loadedAt(t, fixture(), 100)
	m, _ = send(m, key("?"))
	v := text(m)
	if !strings.Contains(v, i18n.T("tui.keys")) || !strings.Contains(v, i18n.T("tui.help_priority")) || strings.Contains(v, "work@example.com") {
		t.Fatalf("help:\n%s", v)
	}
	m, _ = send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if strings.Contains(text(m), i18n.T("tui.keys")) {
		t.Fatal("esc must close help")
	}
}

func TestBackgroundPicksThePalette(t *testing.T) {
	m := loaded(t, fixture())
	dark := m.(*Model).pal.sel
	m, _ = send(m, tea.BackgroundColorMsg{Color: color.White})
	if m.(*Model).pal.sel == dark {
		t.Fatal("a light terminal must get the light palette")
	}
}

func TestEnterActivatesSelected(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	m, _ = send(m, key("down"))
	m, cmd := send(m, key("enter"))
	m = run(t, m, cmd)
	if len(f.acts) != 1 || f.acts[0] != (call{"home", false}) {
		t.Fatalf("acts = %v", f.acts)
	}
	_ = m
}

func TestEnterOnActiveIsNoop(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	_, cmd := send(m, key("enter"))
	if cmd != nil || len(f.acts) != 0 {
		t.Fatal("enter on the active row must do nothing")
	}
}

func TestNearRefreshConfirmThenForce(t *testing.T) {
	f := fixture()
	f.actErr = []error{ErrNearRefresh}
	m := loaded(t, f)
	m, _ = send(m, key("down"))
	m, cmd := send(m, key("enter"))
	m = run(t, m, cmd)
	if v := text(m); !strings.Contains(v, i18n.T("tui.token_refreshing_confirm")) || !strings.Contains(v, "y "+i18n.T("tui.yes")) {
		t.Fatalf("no confirm prompt:\n%s", text(m))
	}
	m, cmd = send(m, key("y"))
	m = run(t, m, cmd)
	want := []call{{"home", false}, {"home", true}}
	if len(f.acts) != 2 || f.acts[0] != want[0] || f.acts[1] != want[1] {
		t.Fatalf("acts = %v", f.acts)
	}
	if strings.Contains(text(m), i18n.T("tui.token_refreshing_confirm")) {
		t.Fatal("prompt must be gone")
	}
}

func TestNearRefreshDeclined(t *testing.T) {
	f := fixture()
	f.actErr = []error{ErrNearRefresh}
	m := loaded(t, f)
	m, _ = send(m, key("down"))
	m, cmd := send(m, key("enter"))
	m = run(t, m, cmd)
	m, cmd = send(m, key("n"))
	if cmd != nil || len(f.acts) != 1 || strings.Contains(text(m), i18n.T("tui.token_refreshing_confirm")) {
		t.Fatal("n must cancel without retry")
	}
}

func TestToggleAuto(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	m, cmd := send(m, key("a"))
	run(t, m, cmd)
	if len(f.autos) != 1 || f.autos[0] != false {
		t.Fatalf("autos = %v", f.autos)
	}
}

func TestLoginPromptsForName(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	var got *exec.Cmd
	m.(*Model).execProc = func(c *exec.Cmd, _ tea.ExecCallback) tea.Cmd { got = c; return func() tea.Msg { return nil } }
	m, _ = send(m, key("l"))
	if !strings.Contains(text(m), i18n.T("tui.new_account_name")) {
		t.Fatalf("no name prompt:\n%s", text(m))
	}
	for _, r := range "Bad name" {
		m, _ = send(m, key(string(r)))
	}
	m, cmd := send(m, key("enter"))
	if cmd != nil && got != nil {
		t.Fatal("invalid name must not run login")
	}
	if !strings.Contains(text(m), i18n.T("tui.invalid_name")) {
		t.Fatalf("no validation hint:\n%s", text(m))
	}
	// clear and type a valid name
	for range "Bad name" {
		m, _ = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	for _, r := range "work2" {
		m, _ = send(m, key(string(r)))
	}
	_, cmd = send(m, key("enter"))
	if cmd == nil || got == nil {
		t.Fatal("expected exec command")
	}
	if len(got.Args) != 3 || got.Args[0] != "/bin/ccswap" || got.Args[1] != "login" || got.Args[2] != "work2" {
		t.Fatalf("args = %v", got.Args)
	}
}

func TestSelectionSurvivesRefresh(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	m, _ = send(m, key("down"))
	f.accts = append([]api.Account{{Name: "new", Email: "n@example.com", Enabled: true}}, f.accts...)
	m, cmd := send(m, key("r"))
	m, cmd = send(m, cmd()) // refresh done -> re-poll
	m = run(t, m, cmd)
	_, cmd = send(m, key("enter"))
	run(t, m, cmd)
	if len(f.acts) != 1 || f.acts[0].name != "home" {
		t.Fatalf("selection moved: %v", f.acts)
	}
	if f.refreshN != 1 {
		t.Fatalf("refresh calls = %d", f.refreshN)
	}
}

func TestDaemonDownKeepsDataAndEmptyState(t *testing.T) {
	f := fixture()
	m := loaded(t, f)
	m, _ = send(m, dataMsg{err: ErrDown})
	v := text(m)
	if !strings.Contains(v, i18n.T("tui.daemon_not_running")) || !strings.Contains(v, i18n.T("tui.daemon_not_responding")) ||
		!strings.Contains(v, "work") || !strings.Contains(v, i18n.T("tui.data_from", "")) {
		t.Fatalf("down view must keep last data:\n%s", v)
	}
	e := loaded(t, &fake{})
	if !strings.Contains(text(e), i18n.T("tui.no_accounts_yet")) || !strings.Contains(text(e), i18n.T("tui.adopt_command")) {
		t.Fatalf("empty:\n%s", text(e))
	}
}

func TestQuit(t *testing.T) {
	m := loaded(t, fixture())
	_, cmd := send(m, key("q"))
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q must quit")
	}
	_, cmd = send(m, key("ctrl+c"))
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c must quit")
	}
}

func TestResetText(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	same := now.Add(2 * time.Hour)
	other := now.Add(48 * time.Hour)
	if g := ResetText(api.Account{FiveHour: win(1, same)}, now); g != "12:00" {
		t.Fatal(g)
	}
	if g := ResetText(api.Account{FiveHour: win(1, other), SevenDay: win(1, other.Add(time.Hour))}, now); g != i18n.T("tui.thu")+" 10:00" {
		t.Fatal(g)
	}
	_ = errors.New
}

func TestHintEveryCode(t *testing.T) {
	cyr := regexp.MustCompile(`\p{Cyrillic}`)
	for _, c := range []string{"unknown_active", "cannot_identify", "entry_expired", "near_refresh", "needs_login", "not_found",
		"no_entry", "duplicate", "remove_active", "active_account", "invalid_name", "login_incomplete", "invalid_config_dir",
		"bad_request", "internal", "entry_changed", "target_refreshing", "migrating"} {
		h := Hint(c, "x")
		if !cyr.MatchString(h) || strings.Contains(h, "--force") {
			t.Errorf("%s: %q", c, h)
		}
	}
	if err := fail(409, []byte(`{"code":"cannot_identify","message":"cannot identify"}`), "x", 200); err == nil ||
		strings.Contains(err.Error(), "--force") || !cyr.MatchString(err.Error()) {
		t.Fatal("the TUI has no --force:", err)
	} else if !strings.Contains(err.Error(), i18n.T("tui.reason", "cannot identify")) {
		t.Fatal("the daemon's reason is lost:", err)
	}
	if err := fail(409, []byte(`{"code":"target_refreshing"}`), "b", 200); errors.Is(err, ErrNearRefresh) || !strings.Contains(err.Error(), i18n.T("tui.login_refreshing", "b")) {
		t.Fatal(err)
	}
	if Hint("entry_expired", "") != i18n.T("tui.claude_idle") {
		t.Fatal(Hint("entry_expired", ""))
	}
}

func TestTimeoutIsSlowDaemon(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("Post: %w", context.DeadlineExceeded)} {
		if !errors.Is(down(err), ErrSlow) {
			t.Fatal(err)
		}
	}
	if !strings.Contains(ErrSlow.Error(), "ccswap status") {
		t.Fatal(ErrSlow)
	}
}

func meterColumns(v string) (cols []int) {
	for _, l := range strings.Split(v, "\n") {
		if i := strings.IndexRune(l, '━'); i >= 0 && !strings.Contains(l, "╭") && !strings.Contains(l, "╰") {
			cols = append(cols, len([]rune(l[:i])))
		}
	}
	return cols
}

func TestEnglishScreen(t *testing.T) {
	ru := text(loadedAt(t, fixture(), 100))
	english(t)
	m := loadedAt(t, fixture(), 100)
	v := text(m)
	for _, want := range []string{"auto on", "strategy cache-aware", "Accounts", "5 hours, left", "week, left", "reset",
		"in 2h", "✕ out until", "✕ needs login", "disabled", "▶ in use", "→ next", "work  in use",
		"↑↓ select", "enter use", "+ - priority", "space on/off", "? all keys", "q quit"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if regexp.MustCompile(`\p{Cyrillic}`).MatchString(v) {
		t.Errorf("Russian text in the English view:\n%s", v)
	}
	if strings.ContainsAny(v, "\u2013\u2014") {
		t.Errorf("dash in the English view:\n%s", v)
	}
	en, rus := meterColumns(v), meterColumns(ru)
	if len(en) < 4 || len(rus) < 4 || fmt.Sprint(en[:4]) != fmt.Sprint(rus[:4]) {
		t.Errorf("bar columns differ: en %v, ru %v", en, rus)
	}
	fits(t, v, 100)
	m, _ = send(m, key("l"))
	if v := text(m); !strings.Contains(v, "New account name: ") || !strings.Contains(v, "enter log in") || !strings.Contains(v, "esc cancel") {
		t.Errorf("name prompt:\n%s", v)
	}
	if e := text(loaded(t, &fake{})); !strings.Contains(e, "No accounts yet.") {
		t.Errorf("empty:\n%s", e)
	}
}

func TestEnglishErrorsAndResetText(t *testing.T) {
	english(t)
	if g := Describe(api.Error{Code: "cannot_identify", Message: "boom"}, ""); !strings.HasSuffix(g, "try again later (reason: boom)") {
		t.Error(g)
	}
	if g := Hint("needs_login", "work"); g != "You need to log in again: ccswap login work" {
		t.Error(g)
	}
	if g := Hint("not_found", ""); g != "No account <name>" {
		t.Error(g)
	}
	if g := NameRule(); strings.ContainsAny(g, "\u2013\u2014") || !strings.HasPrefix(g, "Name: a-z, 0-9") {
		t.Error(g)
	}
	if !strings.Contains(ErrSlow.Error(), "ccswap status") || !strings.HasPrefix(ErrDown.Error(), "The daemon is not running") {
		t.Error(ErrSlow, ErrDown)
	}
	if err := fail(500, nil, "", 200); err.Error() != "the daemon replied 500" {
		t.Error(err)
	}
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	if g := ResetText(api.Account{FiveHour: win(1, now.Add(48*time.Hour))}, now); g != "Thu 10:00" {
		t.Error(g)
	}
	if g := ResetText(api.Account{}, now); g != "-" {
		t.Error(g)
	}
}
