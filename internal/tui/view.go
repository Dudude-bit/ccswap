package tui

import (
	"image/color"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// palette is the web page's lavender, picked for the terminal's background once it reports it. Words
// and glyphs carry the meaning too, so colour is never the only signal.
type palette struct {
	plain, dim, bold, acc, accB, warn, warnB, crit, critB, trk, bd, pill lipgloss.Style
	sel                                                                  color.Color
}

func newPalette(dark bool) palette {
	pick := lipgloss.LightDark(dark)
	c := func(light, dark string) color.Color { return pick(lipgloss.Color(light), lipgloss.Color(dark)) }
	fg := func(light, dark string) lipgloss.Style { return lipgloss.NewStyle().Foreground(c(light, dark)) }
	accent := c("#6f5a9a", "#b3a1d6")
	return palette{
		plain: lipgloss.NewStyle(),
		dim:   lipgloss.NewStyle().Faint(true),
		bold:  lipgloss.NewStyle().Bold(true),
		acc:   lipgloss.NewStyle().Foreground(accent),
		accB:  lipgloss.NewStyle().Foreground(accent).Bold(true),
		warn:  fg("#9c6f08", "#e3c26b"),
		warnB: fg("#9c6f08", "#e3c26b").Bold(true),
		crit:  fg("#c65746", "#ec8a7a"),
		critB: fg("#c65746", "#ec8a7a").Bold(true),
		trk:   fg("#ddd6ea", "#3a3147"),
		bd:    fg("#d3cbe3", "#443952"),
		pill:  lipgloss.NewStyle().Background(accent).Foreground(c("#ffffff", "#15111c")).Bold(true),
		sel:   c("#f0ebf8", "#241d2e"),
	}
}

// wide is the width from which the table has a reset column and the panel the full reset text.
const wide = 92

// seg is a run of text in one style; a line is built from segments so a selected row can carry its
// background through every one of them.
type seg struct {
	t string
	s lipgloss.Style
}

type row struct {
	segs []seg
	sel  bool
}

func cells(s string) int { return utf8.RuneCountInString(s) }

func width(segs []seg) int {
	n := 0
	for _, s := range segs {
		n += cells(s.t)
	}
	return n
}

func cat(parts ...[]seg) []seg {
	var out []seg
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func pad(s string, n int) string  { return s + strings.Repeat(" ", max(0, n-cells(s))) }
func lpad(s string, n int) string { return strings.Repeat(" ", max(0, n-cells(s))) + s }

func clip(s string, n int) string {
	r := []rune(s)
	if n < 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fit cuts segs to n cells and pads them out to n.
func fit(segs []seg, n int) []seg {
	out := make([]seg, 0, len(segs)+1)
	for _, s := range segs {
		if n <= 0 {
			break
		}
		r := []rune(s.t)
		r = r[:min(len(r), n)]
		out = append(out, seg{string(r), s.s})
		n -= len(r)
	}
	if n > 0 {
		out = append(out, seg{strings.Repeat(" ", n), lipgloss.NewStyle()})
	}
	return out
}

func (m *Model) paint(segs []seg, sel bool) string {
	var b strings.Builder
	for _, s := range segs {
		st := s.s
		if sel {
			st = st.Background(m.pal.sel)
		}
		b.WriteString(st.Render(s.t))
	}
	return b.String()
}

func (m *Model) box(w int, title []seg, lines []row, foot []seg) []string {
	p := m.pal
	top := cat([]seg{{"╭─ ", p.bd}}, title, []seg{{" ", p.bd}})
	out := []string{m.paint(cat(top, []seg{{strings.Repeat("─", max(0, w-1-width(top))) + "╮", p.bd}}), false)}
	edge := m.paint([]seg{{"│", p.bd}}, false)
	for _, l := range lines {
		out = append(out, edge+m.paint(cat([]seg{{" ", p.plain}}, fit(l.segs, w-4), []seg{{" ", p.plain}}), l.sel)+edge)
	}
	bot := []seg{{"╰", p.bd}}
	if foot != nil {
		bot = cat([]seg{{"╰─ ", p.bd}}, foot, []seg{{" ", p.bd}})
	}
	return append(out, m.paint(cat(bot, []seg{{strings.Repeat("─", max(0, w-1-width(bot))) + "╯", p.bd}}), false))
}

func weekday(d time.Weekday) string {
	return [...]string{i18n.T("tui.sun"), i18n.T("tui.mon"), i18n.T("tui.tue"), i18n.T("tui.wed"), i18n.T("tui.thu"), i18n.T("tui.fri"), i18n.T("tui.sat")}[d]
}

// clock is HH:MM today, else a short weekday and HH:MM, in local time.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return weekday(t.Weekday()) + " " + t.Format("15:04")
}

// date is clock for a time within six days, else the date and time: a weekday alone could be another week's.
func date(t, now time.Time) string {
	if t.Sub(now).Abs() < 6*24*time.Hour {
		return clock(t, now)
	}
	return t.Local().Format("02.01 15:04")
}

// loginEnds is when a's login ends, if the daemon flags it as ending: no refresh extends it, a new login must.
func loginEnds(a api.Account) (time.Time, bool) {
	if a.NeedsLogin || !a.LoginEnding || a.LoginExpiresAt == nil {
		return time.Time{}, false
	}
	return *a.LoginExpiresAt, true
}

// tokens says when a's login ends and until when its token lasts; "" when neither is known. A narrow
// panel leaves out who renews the token.
func tokens(a api.Account, now time.Time, narrow bool) string {
	var parts []string
	switch {
	case a.NeedsLogin:
		return ""
	case a.LoginExpiresAt != nil:
		parts = append(parts, i18n.T("tui.login_until", date(*a.LoginExpiresAt, now)))
	case a.TokenExpiresAt != nil:
		parts = append(parts, i18n.T("tui.login_unknown"))
	}
	if t := a.TokenExpiresAt; t != nil {
		tok := i18n.T("tui.token_expired")
		if t.After(now) {
			tok = i18n.T("tui.token_until", clock(*t, now))
		}
		if a.Active && !narrow {
			tok += ", " + i18n.T("tui.token_by_claude")
		}
		parts = append(parts, tok)
	}
	return strings.Join(parts, " · ")
}

func until(t, now time.Time) string {
	n := max(1, int(t.Sub(now).Round(time.Minute)/time.Minute))
	d, h, mins := n/1440, n%1440/60, n%60
	switch {
	case d > 0 && h == 0:
		return i18n.T("tui.in_days", d)
	case d > 0:
		return i18n.T("tui.in_days_hours", d, h)
	case h > 0 && mins == 0:
		return i18n.T("tui.in_hours", h)
	case h > 0:
		return i18n.T("tui.in_hours_minutes", h, mins)
	}
	return i18n.T("tui.in_minutes", mins)
}

func ago(t, now time.Time) string {
	n := int(now.Sub(t).Round(time.Minute) / time.Minute)
	switch {
	case n < 1:
		return i18n.T("tui.just_now")
	case n < 60:
		return i18n.T("tui.minutes_ago", n)
	}
	return i18n.T("tui.hours_ago", n/60)
}

// ResetText is the earliest reset of the account's windows: HH:MM today, else a short weekday and
// HH:MM; "-" when unknown.
func ResetText(a api.Account, now time.Time) string {
	var first *time.Time
	pick := func(t *time.Time) {
		if t != nil && (first == nil || t.Before(*first)) {
			first = t
		}
	}
	if a.FiveHour != nil {
		pick(a.FiveHour.ResetsAt)
	}
	if a.SevenDay != nil {
		pick(a.SevenDay.ResetsAt)
	}
	for _, mw := range a.Models {
		pick(mw.ResetsAt)
	}
	if first == nil {
		return "-"
	}
	return clock(*first, now)
}

type window struct {
	key, label, short string
	w                 *api.Window
	limit             float64
}

func (m *Model) windows(a api.Account) []window {
	th := m.st.Thresholds
	five, week := th.FiveHour, th.SevenDay
	if five <= 0 {
		five = 100
	}
	if week <= 0 {
		week = 100
	}
	out := []window{
		{i18n.T("tui.five_h"), i18n.T("tui.five_hours"), i18n.T("tui.five_hours"), a.FiveHour, five},
		{i18n.T("tui.week_short"), i18n.T("tui.week"), i18n.T("tui.week"), a.SevenDay, week},
	}
	for _, mw := range a.Models {
		name := mw.Name
		if name != "" {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		out = append(out, window{strings.ToLower(mw.Name), i18n.T("tui.model_week", name), i18n.T("tui.model_week_short", name),
			&api.Window{Utilization: mw.Utilization, ResetsAt: mw.ResetsAt}, week})
	}
	return out
}

// soonest is the window whose reset comes first after now.
func soonest(ws []window, now time.Time) *window {
	var best *window
	for i := range ws {
		w := &ws[i]
		if w.w == nil || w.w.ResetsAt == nil || !w.w.ResetsAt.After(now) {
			continue
		}
		if best == nil || w.w.ResetsAt.Before(*best.w.ResetsAt) {
			best = w
		}
	}
	return best
}

// tone styles a window's bar fill and percentage: what is left shades to amber three quarters of the
// way to the switch threshold and to red at it.
func (m *Model) tone(w window, dimmed bool) (fill, pct lipgloss.Style) {
	p := m.pal
	switch {
	case w.w == nil || dimmed:
		return p.dim, p.dim
	case w.w.Utilization >= w.limit:
		return p.crit, p.critB
	case w.w.Utilization >= w.limit*0.75:
		return p.warn, p.warn
	}
	return p.acc, p.plain
}

func pctText(w *api.Window) string {
	if w == nil {
		return "    "
	}
	return lpad(strconv.Itoa(int(math.Round(100-min(100, max(0, w.Utilization)))))+"%", 4)
}

// bar draws what is left of a window in half cells: ━ filled, ╸ a half, then the track.
func (m *Model) bar(w *api.Window, n int, fill lipgloss.Style) []seg {
	p := m.pal
	if w == nil {
		return []seg{{strings.Repeat("━", n), p.trk}}
	}
	halves := int(math.Round((100 - min(100, max(0, w.Utilization))) / 100 * float64(n) * 2))
	full, half := halves/2, halves%2
	var out []seg
	if full > 0 {
		out = append(out, seg{strings.Repeat("━", full), fill})
	}
	if half == 1 {
		out = append(out, seg{"╸", fill})
	}
	rest := n - full - half
	if rest > 0 && half == 0 && full > 0 {
		out = append(out, seg{"╺", p.trk})
		rest--
	}
	if rest > 0 {
		out = append(out, seg{strings.Repeat("━", rest), p.trk})
	}
	return out
}

func (m *Model) next() string {
	if m.st.Next == nil || !m.st.Auto {
		return ""
	}
	return *m.st.Next
}

func (m *Model) header(w int) string {
	p := m.pal
	auto := []seg{{" ccswap ", p.pill}, {"  " + i18n.T("tui.auto") + " ", p.dim}, {i18n.T("tui.off"), p.dim}}
	if m.st.Auto {
		auto[2] = seg{i18n.T("tui.on"), p.accB}
	}
	var right []seg
	switch {
	case m.down:
		right = []seg{{"✕ " + i18n.T("tui.daemon_not_responding"), p.critB}}
	case m.refreshing:
		right = []seg{{i18n.T("tui.checking_limits"), p.acc}}
	case m.st.LastPollAt != nil:
		right = []seg{{i18n.T("tui.checked"), p.dim}, {clock(*m.st.LastPollAt, time.Now()), p.plain}}
	}
	strategy := string(m.st.Strategy)
	var hub, hubShort []seg
	if r := m.st.Remote; r != nil {
		host := r.Url
		if u, err := url.Parse(r.Url); err == nil && u.Host != "" {
			host = u.Host
		}
		if r.Ok {
			hub = []seg{{"   " + i18n.T("tui.limits") + " ", p.dim}, {i18n.T("tui.from_hub_host", host), p.plain}}
			hubShort = []seg{{"  " + i18n.T("tui.from_hub"), p.plain}}
		} else {
			hub = []seg{{"   " + i18n.T("tui.hub_down_asking_anthropic", host), p.warn}}
			hubShort = []seg{{"  " + i18n.T("tui.hub_unreachable"), p.warn}}
		}
	}
	for _, left := range [][]seg{
		cat(auto, []seg{{"   " + i18n.T("tui.strategy") + " ", p.dim}, {strategy, p.plain}}, hub),
		cat(auto, []seg{{"  " + strategy, p.plain}}, hubShort),
		cat(auto, []seg{{"  " + strategy, p.plain}}),
		auto,
	} {
		if gap := w - width(left) - width(right); gap >= 1 {
			return m.paint(cat(left, []seg{{strings.Repeat(" ", gap), p.plain}}, right), false)
		}
	}
	return m.paint(fit(auto, w), false)
}

func (m *Model) state(a api.Account, now time.Time, dim bool) []seg {
	p := m.pal
	st := func(s lipgloss.Style) lipgloss.Style {
		if dim {
			return p.dim
		}
		return s
	}
	switch {
	case a.NeedsLogin:
		return []seg{{"✕ " + i18n.T("tui.needs_login"), st(p.crit)}}
	case !a.Enabled:
		return []seg{{i18n.T("tui.disabled"), p.dim}}
	}
	if end, ok := loginEnds(a); ok {
		return []seg{{"! " + i18n.T("tui.login_ends", clock(end, now)), st(p.warn)}}
	}
	switch {
	case a.Exhausted:
		if a.FreeAt != nil && a.FreeAt.After(now) { // passed: the next check frees it
			return []seg{{"✕ " + i18n.T("tui.out_until", clock(*a.FreeAt, now)), st(p.crit)}}
		}
		return []seg{{"✕ " + i18n.T("tui.limit_reached"), st(p.crit)}}
	case a.Error != nil:
		return []seg{{"! " + i18n.T("tui.check_failed"), st(p.warn)}}
	}
	if w := soonest(m.windows(a), now); w != nil {
		return []seg{{pad(w.key, 4) + " ", p.dim}, {until(*w.w.ResetsAt, now), st(p.plain)}}
	}
	return []seg{{i18n.T("tui.no_reset"), p.dim}}
}

// glyph marks an account's state where a narrow table has no room for the words; the panel says them.
func (m *Model) glyph(a api.Account) seg {
	p := m.pal
	switch {
	case a.NeedsLogin || a.Exhausted:
		return seg{"✕", p.crit}
	case a.Error != nil:
		return seg{"!", p.warn}
	case !a.Enabled:
		return seg{"○", p.dim}
	case a.LoginEnding:
		return seg{"!", p.warn}
	}
	return seg{" ", p.plain}
}

func (m *Model) table(w int, narrow bool, now time.Time) []string {
	p := m.pal
	nw := 8
	for _, a := range m.accts {
		nw = max(nw, min(16, cells(a.Name)))
	}
	stateW := 1
	if !narrow {
		stateW = 21
	}
	bw := max(6, min(20, (w-4-4-nw-9-stateW-14)/2))
	fiveH, weekH := i18n.T("tui.five_hours_left"), i18n.T("tui.week_left")
	if narrow {
		fiveH, weekH = i18n.T("tui.five_hours"), i18n.T("tui.week")
	}
	head := "    " + pad(i18n.T("tui.col_account"), nw) + lpad(i18n.T("tui.col_prio"), 6) + "   " + pad(fiveH, bw+5) + "  " + pad(weekH, bw+5)
	if !narrow {
		head += "  " + i18n.T("tui.col_reset")
	}
	lines := []row{{segs: []seg{{head, p.dim}}}}
	next := m.next()
	for _, a := range m.accts {
		sel := a.Name == m.sel
		bar, mark, name := seg{" ", p.plain}, seg{" ", p.plain}, p.plain
		if sel {
			bar, name = seg{"▌", p.acc}, p.bold
		}
		switch {
		case a.Active:
			mark = seg{"▶", p.accB}
		case a.Name == next:
			mark = seg{"→", p.acc}
		}
		if !a.Enabled || m.down {
			name = p.dim
		}
		if m.down {
			bar.s, mark.s = p.dim, p.dim
		}
		segs := []seg{bar, {" ", p.plain}, mark, {" ", p.plain}, {pad(clip(a.Name, nw), nw), name}, {lpad(strconv.Itoa(a.Priority), 6), p.dim}, {"   ", p.plain}}
		for _, wd := range m.windows(a)[:2] {
			fill, pct := m.tone(wd, !a.Enabled || a.Error != nil || m.down)
			segs = append(append(segs, m.bar(wd.w, bw, fill)...), seg{" ", p.plain}, seg{pctText(wd.w), pct}, seg{"  ", p.plain})
		}
		if narrow {
			segs = append(segs, m.glyph(a))
		} else {
			segs = append(segs, m.state(a, now, m.down)...)
		}
		lines = append(lines, row{segs, sel})
	}
	foot := []seg{{"▶", p.acc}, {" " + i18n.T("tui.in_use") + "  ", p.dim}, {"→", p.acc}, {" " + i18n.T("tui.next"), p.dim}}
	if m.down && m.loadedAt != (time.Time{}) {
		foot = []seg{{i18n.T("tui.data_from", clock(m.loadedAt, now)), p.dim}}
	}
	return m.box(w, []seg{{i18n.T("tui.accounts"), p.bold}}, lines, foot)
}

func (m *Model) detail(w int, narrow bool, a api.Account, now time.Time) []string {
	p := m.pal
	var tag *seg
	switch next := m.next(); {
	case a.Active:
		tag = &seg{i18n.T("tui.in_use"), p.acc}
	case a.Name == next:
		tag = &seg{i18n.T("tui.next"), p.acc}
	case a.NeedsLogin:
		tag = &seg{i18n.T("tui.needs_login"), p.crit}
	case !a.Enabled:
		tag = &seg{i18n.T("tui.disabled"), p.dim}
	}
	lines := []row{{segs: []seg{{a.Email, p.dim}, {"   " + i18n.T("tui.priority") + " ", p.dim}, {strconv.Itoa(a.Priority), p.plain}}}}
	th := m.st.Thresholds
	five, week := strconv.FormatFloat(100-th.FiveHour, 'f', -1, 64)+"%", strconv.FormatFloat(100-th.SevenDay, 'f', -1, 64)+"%"
	switch next := m.next(); {
	case a.Active && next != "" && narrow:
		lines = append(lines, row{segs: []seg{{i18n.T("tui.next_label"), p.dim}, {next, p.bold}, {i18n.T("tui.next_at_left", five, week), p.dim}}})
	case a.Active && next != "":
		lines = append(lines, row{segs: []seg{{i18n.T("tui.moves_to"), p.dim}, {next, p.bold},
			{i18n.T("tui.next_when_left", five, week), p.dim}}})
	case a.Name == next && m.st.Active != "":
		lines = append(lines, row{segs: []seg{{i18n.T("tui.moves_here_once"), p.dim}, {m.st.Active, p.bold}, {i18n.T("tui.runs_out"), p.dim}}})
	}
	if a.NeedsLogin {
		lines = append(lines, row{segs: []seg{{"✕ " + i18n.T("tui.login_again", a.Name), p.crit}}})
	}
	if a.Error != nil {
		lines = append(lines, row{segs: []seg{{"! " + *a.Error, p.warn}}})
	}
	if end, ok := loginEnds(a); ok && a.Active {
		lines = append(lines, row{segs: []seg{{"! " + i18n.T("tui.login_ends_claude", clock(end, now)), p.warn}}})
	} else if ok {
		lines = append(lines, row{segs: []seg{{"! " + i18n.T("tui.login_ends_run", clock(end, now), a.Name), p.warn}}})
	}
	lines = append(lines, row{})
	lw, resetW := 15, 31
	if narrow {
		lw, resetW = 11, 15
	}
	bw := max(6, min(40, w-4-lw-8-resetW))
	for _, wd := range m.windows(a) {
		fill, pct := m.tone(wd, a.Error != nil)
		label := wd.label
		if narrow {
			label = wd.short
		}
		segs := cat([]seg{{pad(label, lw), p.dim}}, m.bar(wd.w, bw, fill), []seg{{" ", p.plain}, {pctText(wd.w), pct}, {"   ", p.plain}})
		switch {
		case wd.w == nil:
			segs = append(segs, seg{i18n.T("tui.no_data_yet"), p.dim})
		case wd.w.ResetsAt != nil && wd.w.ResetsAt.After(now) && narrow:
			segs = append(segs, seg{until(*wd.w.ResetsAt, now), p.plain})
		case wd.w.ResetsAt != nil && wd.w.ResetsAt.After(now):
			segs = append(segs, seg{i18n.T("tui.resets", clock(*wd.w.ResetsAt, now)), p.dim}, seg{until(*wd.w.ResetsAt, now), p.plain})
		case wd.w.ResetsAt != nil:
			segs = append(segs, seg{i18n.T("tui.reset_waiting"), p.dim})
		case wd.w.Utilization == 0 && narrow:
			segs = append(segs, seg{i18n.T("tui.not_started"), p.dim})
		case wd.w.Utilization == 0:
			segs = append(segs, seg{i18n.T("tui.window_not_started"), p.dim})
		}
		lines = append(lines, row{segs: segs})
	}
	lines = append(lines, row{})
	switch {
	case a.CheckedAt == nil:
		lines = append(lines, row{segs: []seg{{i18n.T("tui.limits_not_checked_yet"), p.dim}}})
	case a.Error != nil:
		lines = append(lines, row{segs: []seg{{i18n.T("tui.numbers_from", clock(*a.CheckedAt, now), ago(*a.CheckedAt, now)), p.warn}}})
	default:
		at := a.CheckedAt.Local().Format("15:04:05")
		lines = append(lines, row{segs: []seg{{i18n.T("tui.checked_at", at, ago(*a.CheckedAt, now)), p.dim}}})
	}
	if t := tokens(a, now, narrow); t != "" {
		lines = append(lines, row{segs: []seg{{t, p.dim}}})
	}
	title := []seg{{a.Name, p.bold}}
	if tag != nil {
		title = append(title, seg{"  ", p.plain}, *tag)
	}
	return m.box(w, title, lines, nil)
}

func (m *Model) helpBox(w int) []string {
	p := m.pal
	var lines []row
	for _, k := range [][2]string{
		{"↑ ↓  k j", i18n.T("tui.help_select")},
		{"enter", i18n.T("tui.help_use")},
		{"+  -", i18n.T("tui.help_priority")},
		{"space", i18n.T("tui.help_enable")},
		{"a", i18n.T("tui.help_auto")},
		{"s", i18n.T("tui.help_strategy")},
		{"r", i18n.T("tui.help_check")},
		{"t", i18n.T("tui.help_token")},
		{"l", i18n.T("tui.help_login")},
		{"?  esc", i18n.T("tui.close_help")},
		{"q", i18n.T("tui.quit")},
	} {
		lines = append(lines, row{segs: []seg{{pad(k[0], 10), p.bold}, {k[1], p.plain}}})
	}
	return m.box(w, []seg{{i18n.T("tui.keys"), p.bold}}, lines, nil)
}

// keys lays out key hints, wrapping to the next line at the width.
func (m *Model) keys(w int, list ...[2]string) []string {
	p := m.pal
	var out []string
	line := []seg{{"  ", p.plain}}
	for _, k := range list {
		part := []seg{{k[0], p.bold}, {" " + k[1], p.dim}}
		if width(line) > 2 && width(line)+3+width(part) > w {
			out = append(out, m.paint(line, false))
			line = []seg{{"  ", p.plain}}
		}
		if width(line) > 2 {
			line = append(line, seg{"   ", p.plain})
		}
		line = append(line, part...)
	}
	return append(out, m.paint(line, false))
}

func (m *Model) selected() (api.Account, bool) {
	for _, a := range m.accts {
		if a.Name == m.sel {
			return a, true
		}
	}
	return api.Account{}, false
}

func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *Model) render() string {
	p := m.pal
	w := max(40, m.width)
	narrow, now := w < wide, time.Now()
	out := []string{m.header(w), ""}
	switch {
	case !m.loaded && m.down:
	case !m.loaded:
		out = append(out, m.paint([]seg{{"  " + i18n.T("tui.loading"), p.dim}}, false))
	case len(m.accts) == 0:
		out = append(out, m.box(w, []seg{{i18n.T("tui.accounts"), p.bold}}, []row{
			{segs: []seg{{i18n.T("tui.no_accounts_yet"), p.bold}}},
			{},
			{segs: []seg{{pad("l", 23), p.bold}, {i18n.T("tui.help_login"), p.dim}}},
			{segs: []seg{{pad(i18n.T("tui.adopt_command"), 23), p.bold},
				{i18n.T("tui.help_adopt"), p.dim}}},
		}, nil)...)
	default:
		out = append(out, m.table(w, narrow, now)...)
		if a, ok := m.selected(); m.help {
			out = append(out, m.helpBox(w)...)
		} else if ok && !m.down {
			out = append(out, m.detail(w, narrow, a, now)...)
		}
	}
	switch {
	case m.mode == modeConfirm:
		out = append(out, m.paint([]seg{{"  ! ", p.warnB}, {i18n.T("tui.token_refreshing_confirm"), p.warn}}, false))
		out = append(out, m.keys(w, [2]string{"y", i18n.T("tui.yes")}, [2]string{"n", i18n.T("tui.no")})...)
	case m.mode == modeName:
		out = append(out, m.paint([]seg{{"  " + i18n.T("tui.new_account_name"), p.plain}}, false)+m.input.View())
		if m.hint != "" {
			out = append(out, m.paint([]seg{{"  " + m.hint, p.crit}}, false))
		}
		out = append(out, m.keys(w, [2]string{"enter", i18n.T("tui.log_in")}, [2]string{"esc", i18n.T("tui.cancel")})...)
	case m.down:
		out = append(out, m.paint([]seg{{"  ✕ " + i18n.T("tui.daemon_not_running"), p.critB}, {i18n.T("tui.run"), p.dim},
			{"ccswap daemon install", p.plain}, {i18n.T("tui.retrying"), p.dim}}, false))
		out = append(out, m.keys(w, [2]string{"r", i18n.T("tui.check_now")}, [2]string{"q", i18n.T("tui.quit")})...)
	default:
		switch {
		case m.flash != "" && m.flashErr:
			out = append(out, m.paint([]seg{{"  " + m.flash, p.crit}}, false))
		case m.flash != "":
			out = append(out, m.paint([]seg{{"  ✓ ", p.acc}, {m.flash, p.plain}}, false))
		default:
			out = append(out, "")
		}
		switch {
		case !m.loaded || len(m.accts) == 0:
			out = append(out, m.keys(w, [2]string{"l", i18n.T("tui.log_in")}, [2]string{"q", i18n.T("tui.quit")})...)
		case m.help:
			out = append(out, m.keys(w, [2]string{"?", i18n.T("tui.close")}, [2]string{"q", i18n.T("tui.quit")})...)
		default:
			out = append(out, m.keys(w, [2]string{"↑↓", i18n.T("tui.select")}, [2]string{"enter", i18n.T("tui.use")},
				[2]string{"+ -", i18n.T("tui.priority")}, [2]string{"space", i18n.T("tui.on_off")},
				[2]string{"r", i18n.T("tui.check")}, [2]string{"?", i18n.T("tui.all_keys")}, [2]string{"q", i18n.T("tui.quit")})...)
		}
	}
	return strings.Join(out, "\n") + "\n"
}
