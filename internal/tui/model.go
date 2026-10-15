package tui

import (
	"cmp"
	"context"
	"errors"
	"os/exec"
	"slices"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/store"
)

const (
	pollEvery = 5 * time.Second
	flashFor  = 5 * time.Second
	// reads are quick; a switch may wait on the daemon's lock through an Anthropic call.
	readBudget, writeBudget = 10 * time.Second, 60 * time.Second
)

type mode int

const (
	modeList mode = iota
	modeConfirm
	modeName
)

type (
	dataMsg struct {
		st    api.Status
		accts []api.Account
		err   error
	}
	// actionMsg is the outcome of an Activate/SetAuto/Refresh call.
	actionMsg struct {
		err     error
		name    string
		done    string // flash on success
		offerOK bool   // near_refresh may be confirmed
	}
	tickMsg  struct{}
	flashMsg struct{ seq int }
	loginMsg struct{ err error }
)

// Model is the Bubble Tea model of the account screen.
type Model struct {
	c    Client
	self string

	st         api.Status
	accts      []api.Account // in the order the screen shows them: priority, then the account in use
	loaded     bool
	loadedAt   time.Time
	down       bool
	busy       int  // polls in flight; the old data stays on screen meanwhile
	refreshing bool // a check of every account's limits asked with r
	help       bool
	pal        palette

	sel     string // selected account name; survives refreshes
	mode    mode
	pending string // account awaiting the near_refresh confirmation
	input   textinput.Model
	hint    string // validation hint under the name prompt

	flash    string
	flashErr bool
	flashSeq int
	width    int

	execProc func(*exec.Cmd, tea.ExecCallback) tea.Cmd
}

// New returns the screen; self is the ccswap binary used for `login`.
func New(c Client, self string) tea.Model {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 32
	return &Model{c: c, self: self, input: in, width: 80, pal: newPalette(true), execProc: tea.ExecProcess}
}

func (m *Model) Init() tea.Cmd { return tea.Batch(m.poll(), tick(), tea.RequestBackgroundColor) }

func tick() tea.Cmd { return tea.Tick(pollEvery, func(time.Time) tea.Msg { return tickMsg{} }) }

func withTimeout[T any](d time.Duration, f func(context.Context) T) T {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return f(ctx)
}

func (m *Model) poll() tea.Cmd {
	m.busy++
	return func() tea.Msg {
		return withTimeout(readBudget, func(ctx context.Context) tea.Msg {
			st, err := m.c.Status(ctx)
			if err != nil {
				return dataMsg{err: err}
			}
			a, err := m.c.Accounts(ctx)
			return dataMsg{st: st, accts: a, err: err}
		})
	}
}

// act runs op, then re-polls.
func (m *Model) act(name, done string, offerOK bool, op func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		return actionMsg{err: withTimeout(writeBudget, op), name: name, done: done, offerOK: offerOK}
	}
}

func (m *Model) say(s string) tea.Cmd {
	m.flash, m.flashErr = s, false
	return m.flashFor()
}

func (m *Model) sayErr(s string) tea.Cmd {
	m.flash, m.flashErr = s, true
	return m.flashFor()
}

func (m *Model) flashFor() tea.Cmd {
	m.flashSeq++
	seq := m.flashSeq
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashMsg{seq} })
}

func (m *Model) index() int {
	for i, a := range m.accts {
		if a.Name == m.sel {
			return i
		}
	}
	return 0
}

func (m *Model) order() {
	slices.SortStableFunc(m.accts, func(a, b api.Account) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		switch {
		case a.Active && !b.Active:
			return -1
		case b.Active && !a.Active:
			return 1
		}
		return 0
	})
}

// change applies an edit to the shown account at once; the daemon's answer or the next poll settles it.
func (m *Model) change(name string, f func(*api.Account)) {
	for i := range m.accts {
		if m.accts[i].Name == name {
			f(&m.accts[i])
		}
	}
	m.order()
}

func (m *Model) move(d int) {
	if len(m.accts) == 0 {
		return
	}
	m.sel = m.accts[max(0, min(len(m.accts)-1, m.index()+d))].Name
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.BackgroundColorMsg:
		m.pal = newPalette(msg.IsDark())
	case tickMsg:
		return m, tea.Batch(m.poll(), tick())
	case flashMsg:
		if msg.seq == m.flashSeq {
			m.flash = ""
		}
	case dataMsg:
		m.busy = max(0, m.busy-1)
		if msg.err != nil {
			m.down = errors.Is(msg.err, ErrDown)
			if m.down {
				return m, nil
			}
			return m, m.sayErr(msg.err.Error())
		}
		m.down, m.loaded, m.loadedAt = false, true, time.Now()
		m.st, m.accts = msg.st, msg.accts
		m.order()
		if m.sel == "" {
			m.sel = m.st.Active
		}
		if !m.has(m.sel) && len(m.accts) > 0 {
			m.sel = m.accts[0].Name
		}
	case actionMsg:
		m.refreshing = false
		switch {
		case msg.err == nil && msg.done == "":
			return m, m.poll()
		case msg.err == nil:
			return m, tea.Batch(m.say(msg.done), m.poll())
		case msg.offerOK && errors.Is(msg.err, ErrNearRefresh):
			m.mode, m.pending, m.flash = modeConfirm, msg.name, ""
		case errors.Is(msg.err, ErrDown):
			m.down = true
		default:
			return m, tea.Batch(m.sayErr(msg.err.Error()), m.poll())
		}
	case loginMsg:
		if msg.err != nil {
			return m, tea.Batch(m.sayErr(i18n.T("tui.login_failed", msg.err)), m.poll())
		}
		return m, m.poll()
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) has(name string) bool {
	for _, a := range m.accts {
		if a.Name == name {
			return true
		}
	}
	return false
}

func (m *Model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeConfirm:
		name := m.pending
		switch s {
		case "y", "Y", "н":
			m.mode, m.pending = modeList, ""
			return m, m.act(name, i18n.T("tui.active", name), false, func(ctx context.Context) error { return m.c.Activate(ctx, name, true) })
		case "n", "N", "esc", "т":
			m.mode, m.pending = modeList, ""
		}
		return m, nil
	case modeName:
		switch s {
		case "esc":
			m.mode, m.hint = modeList, ""
			m.input.Blur()
			return m, nil
		case "enter":
			name := m.input.Value()
			if !store.ValidName(name) {
				m.hint = NameRule()
				return m, nil
			}
			m.mode, m.hint = modeList, ""
			m.input.Blur()
			m.input.Reset()
			return m, m.execProc(exec.Command(m.self, "login", name), func(err error) tea.Msg { return loginMsg{err} })
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		m.hint = ""
		return m, cmd
	}
	if m.help && (s == "?" || s == "esc") {
		m.help = false
		return m, nil
	}
	a, ok := m.selected()
	switch s {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
	case "+", "=", "-", "_":
		if !ok {
			return m, nil
		}
		p := a.Priority + 10
		if s == "-" || s == "_" {
			p = a.Priority - 10
		}
		m.change(a.Name, func(x *api.Account) { x.Priority = p })
		return m, m.act(a.Name, i18n.T("tui.priority_set", a.Name, p), false, func(ctx context.Context) error { return m.c.SetPriority(ctx, a.Name, p) })
	case "space":
		if !ok {
			return m, nil
		}
		on := !a.Enabled
		done := i18n.T("tui.auto_switch_skips", a.Name)
		if on {
			done = i18n.T("tui.auto_back", a.Name)
		}
		m.change(a.Name, func(x *api.Account) { x.Enabled = on })
		return m, m.act(a.Name, done, false, func(ctx context.Context) error { return m.c.SetEnabled(ctx, a.Name, on) })
	case "s":
		next := api.CacheAware
		if m.st.Strategy == api.CacheAware {
			next = api.FillFirst
		}
		m.st.Strategy = next
		return m, m.act("", i18n.T("tui.strategy_set", next), false, func(ctx context.Context) error { return m.c.SetStrategy(ctx, next) })
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		if len(m.accts) == 0 || m.accts[m.index()].Active {
			return m, nil
		}
		name := m.accts[m.index()].Name
		return m, m.act(name, i18n.T("tui.active", name), true, func(ctx context.Context) error { return m.c.Activate(ctx, name, false) })
	case "a":
		on := !m.st.Auto
		m.st.Auto = on
		done := i18n.T("tui.auto_off")
		if on {
			done = i18n.T("tui.auto_on")
		}
		return m, m.act("", done, false, func(ctx context.Context) error { return m.c.SetAuto(ctx, on) })
	case "r":
		m.refreshing = true
		return m, m.act("", "", false, m.c.Refresh)
	case "l":
		m.mode, m.hint = modeName, ""
		m.input.Reset()
		return m, m.input.Focus()
	}
	return m, nil
}
