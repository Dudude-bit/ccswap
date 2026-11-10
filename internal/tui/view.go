package tui

import (
	"image/color"
	"math"
	"net/url"
	"regexp"
	"slices"
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

// layout is the table's arrangement by width: full has a state column in words, mid one in short words,
// slim a glyph, its legend under the table. The panel is narrow in slim only.
type layout int

const (
	slim layout = iota
	mid
	full
)

func layoutOf(w int) layout {
	switch {
	case w >= 92:
		return full
	case w >= 72:
		return mid
	}
	return slim
}

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
	none              bool // the plan has no such limit: a nil w is not missing data
}

func (m *Model) windows(a api.Account) []window {
	th := m.st.Thresholds
	five, week := th.FiveHour, weekLimit(th, a)
	if five <= 0 {
		five = 100
	}
	if week <= 0 {
		week = 100
	}
	out := []window{
		{i18n.T("tui.five_h"), i18n.T("tui.five_hours"), i18n.T("tui.five_hours"), a.FiveHour, five, a.NoFiveHourLimit},
		{i18n.T("tui.week_short"), i18n.T("tui.week"), i18n.T("tui.week"), a.SevenDay, week, false},
	}
	for _, mw := range a.Models {
		name := mw.Name
		if name != "" {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		out = append(out, window{strings.ToLower(mw.Name), i18n.T("tui.model_week", name), i18n.T("tui.model_week_short", name),
			&api.Window{Utilization: mw.Utilization, ResetsAt: mw.ResetsAt}, week, false})
	}
	return out
}

func weekLimit(th api.Thresholds, a api.Account) float64 {
	if a.WeeklyThreshold != nil {
		return *a.WeeklyThreshold
	}
	return th.SevenDay
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

// state says an account's state in the table's last column, in short words when short. An account that
// needs a login says so in place of its numbers instead.
func (m *Model) state(a api.Account, now time.Time, short, dim bool) []seg {
	p := m.pal
	st := func(s lipgloss.Style) lipgloss.Style {
		if dim {
			return p.dim
		}
		return s
	}
	pick := func(long, brief string) string {
		if short {
			return brief
		}
		return long
	}
	switch {
	case a.NeedsLogin:
		return nil
	case !a.Enabled:
		return []seg{{pick(i18n.T("tui.disabled"), i18n.T("tui.disabled_short")), p.dim}}
	}
	if end, ok := loginEnds(a); ok {
		return []seg{{"! " + pick(i18n.T("tui.login_ends", clock(end, now)), i18n.T("tui.login_ends_short", clock(end, now))), st(p.warn)}}
	}
	switch {
	case a.Exhausted:
		if a.FreeAt != nil && a.FreeAt.After(now) { // passed: the next check frees it
			return []seg{{"✕ " + pick(i18n.T("tui.out_until", clock(*a.FreeAt, now)), i18n.T("tui.out_until_short", clock(*a.FreeAt, now))), st(p.crit)}}
		}
		return []seg{{"✕ " + pick(i18n.T("tui.limit_reached"), i18n.T("tui.limit_short")), st(p.crit)}}
	case a.Error != nil && a.CheckedAt != nil:
		c := clock(*a.CheckedAt, now)
		return []seg{{"? " + pick(i18n.T("tui.numbers_stale", c), i18n.T("tui.numbers_stale_short", c)), st(p.warn)}}
	case a.Error != nil:
		return []seg{{"? " + pick(i18n.T("tui.check_failed"), i18n.T("tui.no_numbers")), st(p.warn)}}
	}
	if w := soonest(m.windows(a), now); w != nil {
		key := pad(w.key, 4) + " "
		if short {
			key = w.key + " "
		}
		return []seg{{key, p.dim}, {until(*w.w.ResetsAt, now), st(p.plain)}}
	}
	return []seg{{i18n.T("tui.no_reset"), p.dim}}
}

// glyph marks an account's state where the table has no room for words, and legend names it: ✕ out, !
// the login ends soon, ? stale numbers, ○ disabled. An account that needs a login says so in its row.
func (m *Model) glyph(a api.Account) seg {
	p := m.pal
	if m.down { // the state is as of the data on screen
		g := m.liveGlyph(a)
		return seg{g.t, p.dim}
	}
	return m.liveGlyph(a)
}

func (m *Model) liveGlyph(a api.Account) seg {
	p := m.pal
	switch {
	case a.NeedsLogin:
		return seg{" ", p.plain}
	case a.Exhausted:
		return seg{"✕", p.crit}
	case !a.Enabled:
		return seg{"○", p.dim}
	case a.LoginEnding:
		return seg{"!", p.warn}
	case a.Error != nil:
		return seg{"?", p.warn}
	}
	return seg{" ", p.plain}
}

// credits marks an account's reset credits beside its name: ↺ and how many.
func credits(a api.Account) string {
	if a.Resets == nil {
		return ""
	}
	return " ↺" + strconv.Itoa(a.Resets.Left)
}

// limited is the table window an exhausted account waits on: the 5-hour one, else the week.
func limited(a api.Account) int {
	if a.ExhaustedReason != nil && *a.ExhaustedReason == "five_hour" {
		return 0
	}
	return 1
}

// table is the accounts box; with rows above zero it shows that many accounts around the selected one and
// counts the rest in its border.
func (m *Model) table(w int, lay layout, now time.Time, rows int) []string {
	p := m.pal
	nw := 8
	for _, a := range m.accts {
		nw = max(nw, min(16, cells(a.Name)+cells(credits(a))))
	}
	stateW := map[layout]int{slim: 1, mid: 18, full: 21}[lay]
	bw := max(6, min(20, (w-4-4-nw-9-stateW-14)/2))
	head := "    " + pad(i18n.T("tui.col_account"), nw) + lpad(i18n.T("tui.col_prio"), 6) + "   " +
		pad(i18n.T("tui.five_hours"), bw+5) + "  " + pad(i18n.T("tui.week"), bw+5)
	if lay != slim {
		head += "  " + i18n.T("tui.col_reset")
	}
	lines := []row{{segs: []seg{{head, p.dim}}}}
	shown := m.accts
	if rows > 0 && rows < len(shown) {
		start := min(max(0, m.index()-rows/2), len(shown)-rows)
		shown = shown[start : start+rows]
	}
	next := m.next()
	for _, a := range shown {
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
		cr, crS := credits(a), p.acc
		switch {
		case !a.Enabled || m.down:
			crS = p.dim
		case a.Exhausted:
			crS = p.accB
		}
		nm := clip(a.Name, nw-cells(cr))
		segs := []seg{bar, {" ", p.plain}, mark, {" ", p.plain}, {nm, name}, {pad(cr, nw-cells(nm)), crS},
			{lpad(strconv.Itoa(a.Priority), 6), p.dim}, {"   ", p.plain}}
		crit := p.crit
		if m.down {
			crit = p.dim
		}
		if a.NeedsLogin { // its numbers are from before the login went: the row asks for the login instead
			say := "✕ " + i18n.T("tui.needs_login")
			if sel {
				say += ": " + i18n.T("tui.login_key")
			}
			segs = append(segs, seg{pad(say, 2*bw+12), crit}, seg{"  ", p.plain})
		}
		for i, wd := range m.windows(a)[:2] {
			switch {
			case a.NeedsLogin:
				continue
			case lay == slim && a.Exhausted && i == limited(a): // no state column: the window it waits on says until when
				say := i18n.T("tui.limit_short")
				if a.FreeAt != nil && a.FreeAt.After(now) {
					c := clock(*a.FreeAt, now)
					if say = i18n.T("tui.out_until_short", c); cells(say) > bw+6 {
						say = c
					}
				}
				segs = append(segs, seg{pad(say, bw+6), crit}, seg{" ", p.plain})
				continue
			case wd.none:
				segs = append(segs, seg{pad(i18n.T("tui.no_limit"), bw+5), p.dim}, seg{"  ", p.plain})
				continue
			}
			fill, pct := m.tone(wd, !a.Enabled || a.Error != nil || m.down)
			segs = append(append(segs, m.bar(wd.w, bw, fill)...), seg{" ", p.plain}, seg{pctText(wd.w), pct}, seg{"  ", p.plain})
		}
		if lay == slim {
			segs = append(segs, m.glyph(a))
		} else {
			st := m.state(a, now, lay == mid, m.down)
			if width(st) > w-4-width(segs) { // the long words do not fit: the short ones
				st = m.state(a, now, true, m.down)
			}
			segs = append(segs, st...)
		}
		lines = append(lines, row{segs, sel})
	}
	title := []seg{{i18n.T("tui.accounts"), p.bold}, {" · " + i18n.T("tui.left"), p.dim}}
	var foot []seg
	switch above, below := m.hidden(shown); {
	case above+below > 0:
		foot = []seg{{more(above, below), p.dim}}
	case m.down && m.loadedAt != (time.Time{}):
		foot = []seg{{i18n.T("tui.data_from", clock(m.loadedAt, now)), p.dim}}
	case lay != slim: // the border has the room
		for _, part := range m.legend(false) {
			if width(foot)+2+width(part) > w-7 {
				break
			}
			if foot != nil {
				foot = append(foot, seg{"  ", p.plain})
			}
			foot = append(foot, part...)
		}
	}
	return m.box(w, title, lines, foot)
}

// more says how many lines a scrolled box hides above and below, naming only the sides that hide some.
func more(above, below int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, "↑ "+strconv.Itoa(above))
	}
	if below > 0 {
		parts = append(parts, "↓ "+strconv.Itoa(below))
	}
	return i18n.T("tui.more_rows", strings.Join(parts, "  "))
}

// hidden counts the accounts above and below the ones a scrolled table shows.
func (m *Model) hidden(shown []api.Account) (above, below int) {
	if len(shown) == len(m.accts) {
		return 0, 0
	}
	above = slices.IndexFunc(m.accts, func(a api.Account) bool { return a.Name == shown[0].Name })
	return above, len(m.accts) - above - len(shown)
}

// slimLegend is the legend under a slim table, wrapped at the width.
func (m *Model) slimLegend(w int) []string {
	p := m.pal
	var out []string
	line := []seg{{"  ", p.plain}}
	for _, part := range m.legend(true) {
		if width(line) > 2 && width(line)+2+width(part) > w {
			out, line = append(out, m.paint(line, false)), []seg{{"  ", p.plain}}
		}
		if width(line) > 2 {
			line = append(line, seg{"  ", p.plain})
		}
		line = append(line, part...)
	}
	return append(out, m.paint(line, false))
}

// legend names the table's marks: the account in use and the next one, reset credits, and on a slim table,
// whose state column is glyphs, those on screen.
func (m *Model) legend(slim bool) [][]seg {
	p := m.pal
	glyphs := map[string]bool{}
	for _, a := range m.accts {
		glyphs[m.liveGlyph(a).t] = slim
	}
	items := [][3]string{{"▶", i18n.T("tui.in_use"), "acc"}, {"→", i18n.T("tui.next"), "acc"}}
	if slices.ContainsFunc(m.accts, func(a api.Account) bool { return a.Resets != nil }) {
		items = append(items, [3]string{"↺", i18n.T("tui.reset_credits_legend"), "acc"})
	}
	for _, g := range [][3]string{{"✕", i18n.T("tui.limit_short"), "crit"}, {"!", i18n.T("tui.legend_login"), "warn"},
		{"?", i18n.T("tui.legend_stale"), "warn"}, {"○", i18n.T("tui.disabled_short"), "dim"}} {
		if glyphs[g[0]] {
			items = append(items, g)
		}
	}
	style := map[string]lipgloss.Style{"acc": p.acc, "crit": p.crit, "warn": p.warn, "dim": p.dim}
	var out [][]seg
	for _, it := range items {
		out = append(out, []seg{{it[0], style[it[2]]}, {" " + it[1], p.dim}})
	}
	return out
}

// detail is the panel of the selected account; trim leaves out, for a short terminal, its blank lines (1),
// then its check and login lines (2), then its windows, which the table shows too (3).
func (m *Model) detail(w int, narrow bool, a api.Account, now time.Time, trim int) []string {
	p := m.pal
	var tag *seg
	switch next := m.next(); {
	case a.Active:
		tag = &seg{i18n.T("tui.in_use"), p.acc}
	case a.Name == next:
		tag = &seg{i18n.T("tui.next"), p.acc}
	case a.NeedsLogin:
		tag = &seg{"✕ " + i18n.T("tui.needs_login"), p.crit}
	case !a.Enabled:
		tag = &seg{i18n.T("tui.disabled"), p.dim}
	case a.Exhausted:
		tag = &m.state(a, now, false, false)[0]
	case a.Error != nil:
		tag = &m.state(a, now, false, false)[0]
	}
	first := []seg{{a.Email, p.dim}, {"   " + i18n.T("tui.priority") + " ", p.dim}, {strconv.Itoa(a.Priority), p.plain}}
	if a.WeeklyThreshold != nil {
		first = append(first, seg{"   " + i18n.T("weekly.leaves_at", *a.WeeklyThreshold), p.dim})
	}
	lines := flow(first, w-4)
	th := m.st.Thresholds
	five, week := strconv.FormatFloat(100-th.FiveHour, 'f', -1, 64)+"%", strconv.FormatFloat(100-weekLimit(th, a), 'f', -1, 64)+"%"
	atLeft, whenLeft := i18n.T("tui.next_at_left", five, week), i18n.T("tui.next_when_left", five, week)
	if a.NoFiveHourLimit {
		atLeft, whenLeft = i18n.T("tui.next_at_week_left", week), i18n.T("tui.next_when_week_left", week)
	}
	switch next := m.next(); {
	case a.Active && next != "" && narrow:
		lines = append(lines, flow([]seg{{i18n.T("tui.next_label"), p.dim}, {next, p.bold}, {atLeft, p.dim}}, w-4)...)
	case a.Active && next != "":
		lines = append(lines, flow([]seg{{i18n.T("tui.moves_to"), p.dim}, {next, p.bold}, {whenLeft, p.dim}}, w-4)...)
	case a.Name == next && m.st.Active != "":
		lines = append(lines, flow([]seg{{i18n.T("tui.moves_here_once"), p.dim}, {m.st.Active, p.bold}, {i18n.T("tui.runs_out"), p.dim}}, w-4)...)
	}
	if r := a.Resets; r != nil {
		segs := []seg{{"↺ ", p.acc}, {i18n.N("tui.reset_credits", r.Left), p.plain}}
		if r.EndsAt != nil {
			segs = append(segs, seg{", " + i18n.N("tui.credit_ends", r.Left, date(*r.EndsAt, now)), p.dim})
		}
		lines = append(lines, flow(segs, w-4)...)
	}
	if a.NeedsLogin {
		lines = append(lines, flow([]seg{{"✕ " + i18n.T("tui.login_again", a.Name), p.crit}}, w-4)...)
	}
	if a.Error != nil {
		lines = append(lines, flow([]seg{{"? " + *a.Error, p.warn}}, w-4)...)
	}
	if end, ok := loginEnds(a); ok && a.Active {
		lines = append(lines, flow([]seg{{"! " + i18n.T("tui.login_ends_claude", clock(end, now)), p.warn}}, w-4)...)
	} else if ok {
		lines = append(lines, flow([]seg{{"! " + i18n.T("tui.login_ends_run", clock(end, now), a.Name), p.warn}}, w-4)...)
	}
	if trim < 1 {
		lines = append(lines, row{})
	}
	lw, resetW := 15, 31
	if narrow {
		lw, resetW = 11, 15
	}
	bw := max(6, min(40, w-4-lw-8-resetW))
	stale := a.Error != nil || a.NeedsLogin // numbers from before: muted, and no reset to count down to
	for _, wd := range m.windows(a) {
		if trim >= 3 {
			break
		}
		fill, pct := m.tone(wd, stale)
		label := wd.label
		if narrow {
			label = wd.short
		}
		if wd.none {
			lines = append(lines, row{segs: []seg{{pad(label, lw), p.dim}, {i18n.T("tui.no_limit_plan"), p.dim}}})
			continue
		}
		segs := cat([]seg{{pad(label, lw), p.dim}}, m.bar(wd.w, bw, fill), []seg{{" ", p.plain}, {pctText(wd.w), pct}, {"   ", p.plain}})
		switch {
		case wd.w == nil:
			segs = append(segs, seg{i18n.T("tui.no_data_yet"), p.dim})
		case a.NeedsLogin:
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
	if trim < 1 {
		lines = append(lines, row{})
	}
	switch {
	case trim >= 2:
	case a.CheckedAt == nil:
		lines = append(lines, flow([]seg{{i18n.T("tui.limits_not_checked_yet"), p.dim}}, w-4)...)
	case a.NeedsLogin:
		lines = append(lines, flow([]seg{{i18n.T("tui.numbers_before_login", clock(*a.CheckedAt, now)), p.dim}}, w-4)...)
	default:
		at := a.CheckedAt.Local().Format("15:04:05")
		lines = append(lines, flow([]seg{{i18n.T("tui.checked_at", at, ago(*a.CheckedAt, now)), p.dim}}, w-4)...)
	}
	if t := tokens(a, now, narrow); t != "" && trim < 2 {
		lines = append(lines, flow([]seg{{t, p.dim}}, w-4)...)
	}
	title := []seg{{a.Name, p.bold}}
	if tag != nil {
		title = append(title, seg{"  ", p.plain}, *tag)
	}
	return m.box(w, title, lines, nil)
}

// bound are what a wrapped line must not split: a number and its unit (62% недели, 2 д), a short weekday
// and its time; oneLetter is a one-letter word, which goes with the next.
var (
	bound     = regexp.MustCompile(`\d%? \pL|\pL{2,3} \d{1,2}:\d\d`)
	oneLetter = regexp.MustCompile(`(^|\s)(\pL) `)
)

// joined keeps on one line what reads as one word, for laying text out: a no-break space joins "Claude
// Code", each pair bound matches and a one-letter word to the next.
func joined(s string) string {
	s = strings.ReplaceAll(s, "Claude Code", "Claude\u00a0Code")
	s = bound.ReplaceAllStringFunc(s, func(m string) string { return strings.Replace(m, " ", "\u00a0", 1) })
	return oneLetter.ReplaceAllString(s, "$1$2\u00a0")
}

// flow wraps a line of styled segments at n cells between words, each word keeping its style; segments
// with no space between them stay together.
func flow(segs []seg, n int) []row {
	type word struct {
		space, t, look string // look tells styles apart: a sample the style renders
		s              lipgloss.Style
	}
	var words []word
	space := ""
	for _, sg := range segs {
		look := sg.s.Render("x")
		for _, r := range joined(sg.t) {
			switch {
			case r == ' ':
				space += " "
			case len(words) > 0 && space == "" && words[len(words)-1].look == look:
				words[len(words)-1].t += string(r)
			default:
				words, space = append(words, word{space, string(r), look, sg.s}), ""
			}
		}
	}
	var rows []row
	var line []seg
	used := 0
	for _, wd := range words {
		if wd.space != "" && used > 0 && used+cells(wd.space)+cells(wd.t) > n {
			rows, line, used, wd.space = append(rows, row{segs: line}), nil, 0, ""
		}
		if used == 0 {
			wd.space = ""
		}
		line, used = append(line, seg{wd.space + strings.ReplaceAll(wd.t, "\u00a0", " "), wd.s}), used+cells(wd.space)+cells(wd.t)
	}
	return append(rows, row{segs: line})
}

// wrap breaks s into lines of at most n cells between words; a word longer than n gets a line of its own.
func wrap(s string, n int) []string {
	var out []string
	line := ""
	for _, word := range strings.FieldsFunc(joined(s), func(r rune) bool { return r == ' ' }) {
		if line != "" && cells(line)+1+cells(word) > n {
			out = append(out, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	out = append(out, line)
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], "\u00a0", " ")
	}
	return out
}

// pairs lays out keys or commands beside what they do, the text wrapping under itself; with under 30
// cells left for the text, or a key wider than its column, the text goes under the key.
func (m *Model) pairs(w, keyW int, list [][2]string) []row {
	p := m.pal
	var lines []row
	inner := w - 4
	for _, k := range list {
		if inner-keyW < 30 || cells(k[0]) >= keyW {
			if len(lines) > 0 {
				lines = append(lines, row{})
			}
			lines = append(lines, row{segs: []seg{{k[0], p.bold}}})
			for _, l := range wrap(k[1], inner-2) {
				lines = append(lines, row{segs: []seg{{"  " + l, p.plain}}})
			}
			continue
		}
		for i, l := range wrap(k[1], inner-keyW) {
			key := ""
			if i == 0 {
				key = k[0]
			}
			lines = append(lines, row{segs: []seg{{pad(key, keyW), p.bold}, {l, p.plain}}})
		}
	}
	return lines
}

// helpRows are the help's lines.
func (m *Model) helpRows(w int) []row {
	return m.pairs(w, 10, [][2]string{
		{"↑ ↓  k j", i18n.T("tui.help_select")},
		{"enter", i18n.T("tui.help_use")},
		{"+  -", i18n.T("tui.help_priority")},
		{"space", i18n.T("tui.help_enable")},
		{"a", i18n.T("tui.help_auto")},
		{"s", i18n.T("tui.help_strategy")},
		{"r", i18n.T("tui.help_check")},
		{"t", i18n.T("tui.help_token")},
		{"w", i18n.T("tui.help_weekly")},
		{"R", i18n.T("tui.help_reset")},
		{"l", i18n.T("tui.help_login")},
		{"?  esc", i18n.T("tui.close_help")},
		{"q", i18n.T("tui.quit")},
	})
}

// helpStarts are the lines where a help entry begins, the lines its pages start at, so a page always opens
// on a key; past the last that still fills a page, only that one.
func (m *Model) helpStarts(w int) []int {
	rows, fits := m.helpRows(w), max(1, m.helpRoom()-2)
	var starts []int
	for i, r := range rows {
		if len(r.segs) > 0 && !strings.HasPrefix(r.segs[0].t, " ") {
			starts = append(starts, i)
			if i >= len(rows)-fits {
				break
			}
		}
	}
	return starts
}

// helpPages reports whether the help does not fit the screen on its own, so it pages.
func (m *Model) helpPages(w int) bool { return len(m.helpRows(w)) > m.helpRoom()-2 }

// helpBox is the help in a box of at most rows lines (all with rows at zero or less), from line m.helpTop
// when it pages.
func (m *Model) helpBox(w, rows int) []string {
	p := m.pal
	lines, foot := m.helpRows(w), []seg(nil)
	if fits := max(1, rows-2); rows > 0 && fits < len(lines) {
		top := min(m.helpTop, len(lines)-1)
		end := min(len(lines), top+fits)
		foot = []seg{{more(top, len(lines)-end), p.dim}}
		lines = lines[top:end]
	}
	return m.box(w, []seg{{i18n.T("tui.keys"), p.bold}}, lines, foot)
}

// said lays out a sentence under a mark, wrapping it at the width between words; its last two words stay
// together, so no line holds a lone word.
func (m *Model) said(w int, mark, text seg) []string {
	lead := "  " + mark.t + " "
	body := text.t
	if i := strings.LastIndex(body, " "); strings.Count(body, " ") >= 2 && i > 0 {
		body = body[:i] + "\u00a0" + body[i+1:]
	}
	var out []string
	for i, l := range wrap(body, w-cells(lead)) {
		if i > 0 {
			mark.t = strings.Repeat(" ", cells(lead))
		} else {
			mark.t = lead
		}
		out = append(out, m.paint([]seg{mark, {l, text.s}}, false))
	}
	return out
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

// atLimit reports whether a window of a is spent, or Anthropic said a hit its limit: where a reset credit
// gives back all it can.
func atLimit(a api.Account) bool {
	if a.ExhaustedReason != nil && *a.ExhaustedReason == "limit_hit" {
		return true
	}
	ws := []*api.Window{a.FiveHour, a.SevenDay}
	for _, mw := range a.Models {
		ws = append(ws, &api.Window{Utilization: mw.Utilization})
	}
	return slices.ContainsFunc(ws, func(w *api.Window) bool { return w != nil && w.Utilization >= 100 })
}

// givesBack says how much of its most used window a reset gives back, as "62% of its week"; "" without numbers.
func givesBack(a api.Account) string {
	type part struct {
		w   *api.Window
		say func(string) string
	}
	parts := []part{{a.FiveHour, func(v string) string { return i18n.T("tui.of_five", v) }},
		{a.SevenDay, func(v string) string { return i18n.T("tui.of_week", v) }}}
	for _, mw := range a.Models {
		parts = append(parts, part{&api.Window{Utilization: mw.Utilization}, func(v string) string { return i18n.T("tui.of_model", v, mw.Name) }})
	}
	var best *part
	for i := range parts {
		if pt := &parts[i]; pt.w != nil && (best == nil || pt.w.Utilization > best.w.Utilization) {
			best = pt
		}
	}
	if best == nil {
		return ""
	}
	return best.say(strconv.Itoa(int(math.Round(min(100, max(0, best.w.Utilization))))) + "%")
}

// resetAsk is the question a reset puts: what it buys when the account waits for a time, and what it costs.
func (m *Model) resetAsk(now time.Time) string {
	s := i18n.T("tui.reset_ask", m.pending)
	if a, ok := m.selected(); ok && a.Name == m.pending && a.Exhausted && a.FreeAt != nil && a.FreeAt.After(now) {
		s += " " + i18n.T("tui.reset_gain", clock(*a.FreeAt, now))
	}
	if m.left == 1 {
		return s + " " + i18n.T("tui.reset_spends_last")
	}
	return s + " " + i18n.N("tui.reset_spends", m.left)
}

// tail is what the screen always ends with: the prompt or the flash, and the keys; gap keeps a blank line in
// place of a flash.
func (m *Model) tail(w int, now time.Time, gap bool) []string {
	p := m.pal
	var out []string
	switch {
	case m.mode == modeConfirm:
		out = append(out, m.said(w, seg{"!", p.warnB}, seg{i18n.T("tui.token_refreshing_confirm"), p.warn})...)
		return append(out, m.keys(w, [2]string{"y", i18n.T("tui.yes")}, [2]string{"n", i18n.T("tui.no")})...)
	case m.mode == modeReset:
		out = append(out, m.said(w, seg{"↺", p.accB}, seg{m.resetAsk(now), p.plain})...)
		if a, ok := m.selected(); ok && a.Name == m.pending && !atLimit(a) {
			if back := givesBack(a); back != "" {
				out = append(out, m.said(w, seg{"!", p.warnB}, seg{i18n.T("tui.reset_not_limited", a.Name, back), p.warn})...)
			}
		}
		return append(out, m.keys(w, [2]string{"y", i18n.T("tui.reset_yes")}, [2]string{"n", i18n.T("tui.no")})...)
	case m.mode == modeName:
		prompt := i18n.T("tui.new_account_name")
		if a, ok := m.selected(); ok && relogin(a) && m.input.Value() == a.Name {
			prompt = i18n.T("tui.relogin_name")
		}
		out = append(out, m.paint([]seg{{"  " + prompt, p.plain}}, false)+m.input.View())
		if m.hint != "" {
			out = append(out, m.said(w, seg{"✕", p.crit}, seg{m.hint, p.crit})...)
		}
		return append(out, m.keys(w, [2]string{"enter", i18n.T("tui.log_in")}, [2]string{"esc", i18n.T("tui.cancel")})...)
	case m.mode == modeWeekly:
		global := strconv.FormatFloat(m.st.Thresholds.SevenDay, 'f', -1, 64) + "%"
		for _, l := range wrap(i18n.T("tui.weekly_prompt", m.pending, global), w-2) {
			out = append(out, m.paint([]seg{{"  " + l, p.plain}}, false))
		}
		out[len(out)-1] += " " + m.input.View()
		if m.hint != "" {
			out = append(out, m.said(w, seg{"✕", p.crit}, seg{m.hint, p.crit})...)
		}
		return append(out, m.keys(w, [2]string{"enter", i18n.T("tui.save")}, [2]string{"esc", i18n.T("tui.cancel")})...)
	case m.down:
		out = append(out, m.said(w, seg{"✕", p.critB}, seg{i18n.T("tui.daemon_down_body"), p.plain})...)
		return append(out, m.keys(w, [2]string{"r", i18n.T("tui.check_now")}, [2]string{"q", i18n.T("tui.quit")})...)
	}
	switch {
	case m.flash != "" && m.flashErr:
		out = append(out, m.said(w, seg{"✕", p.crit}, seg{m.flash, p.crit})...)
	case m.flash != "":
		out = append(out, m.said(w, seg{"✓", p.acc}, seg{m.flash, p.plain})...)
	case gap:
		out = append(out, "")
	}
	switch {
	case !m.loaded || len(m.accts) == 0:
		return append(out, m.keys(w, [2]string{"l", i18n.T("tui.log_in")}, [2]string{"q", i18n.T("tui.quit")})...)
	case m.help:
		return append(out, m.helpKeys(w, m.helpPages(w))...)
	}
	list := [][2]string{{"↑↓", i18n.T("tui.select")}, {"enter", i18n.T("tui.use")}}
	if a, ok := m.selected(); ok && relogin(a) {
		list = append(list, [2]string{"l", i18n.T("tui.log_in_again")})
	}
	if a, ok := m.selected(); ok && a.Resets != nil {
		list = append(list, [2]string{"R", i18n.T("tui.reset_limits")})
	}
	list = append(list, [2]string{"+ -", i18n.T("tui.priority")}, [2]string{"space", i18n.T("tui.on_off")},
		[2]string{"r", i18n.T("tui.check")}, [2]string{"?", i18n.T("tui.all_keys")}, [2]string{"q", i18n.T("tui.quit")})
	return append(out, m.keys(w, list...)...)
}

// screenHeight is the terminal's height, or no limit before it is known.
func (m *Model) screenHeight() int {
	if m.height <= 0 {
		return math.MaxInt32
	}
	return m.height
}

// helpKeys are the keys under help; scrolling is offered only when it pages.
func (m *Model) helpKeys(w int, pages bool) []string {
	list := [][2]string{{"?", i18n.T("tui.close")}, {"q", i18n.T("tui.quit")}}
	if pages {
		list = append([][2]string{{"↑↓", i18n.T("tui.scroll")}}, list...)
	}
	return m.keys(w, list...)
}

// helpRoom is how many lines the help box may take: the screen less the header, a flash and the keys.
func (m *Model) helpRoom() int {
	w := max(40, m.width)
	flash := 0
	if m.flash != "" {
		flash = len(m.said(w, seg{"✓", m.pal.plain}, seg{m.flash, m.pal.plain}))
	}
	return m.screenHeight() - 1 - flash - len(m.helpKeys(w, true))
}

// render lays the screen out to the terminal's height: the prompt and the keys always show, and on a short
// terminal blank lines, the slim legend, the panel's check lines, its windows and then the panel give way,
// least needed first; past that the table scrolls around the selected account, and help pages.
func (m *Model) render() string {
	p := m.pal
	w := max(40, m.width)
	lay, now, h := layoutOf(w), time.Now(), m.screenHeight()
	join := func(parts ...[]string) string {
		var out []string
		for _, part := range parts {
			out = append(out, part...)
		}
		return strings.Join(out, "\n") + "\n"
	}
	head := []string{m.header(w)}
	switch {
	case !m.loaded && m.down:
		return join(head, []string{""}, m.tail(w, now, true))
	case !m.loaded:
		return join(head, []string{"", m.paint([]seg{{"  " + i18n.T("tui.loading"), p.dim}}, false)}, m.tail(w, now, true))
	case len(m.accts) == 0:
		lines := []row{{segs: []seg{{i18n.T("tui.no_accounts_yet"), p.bold}}}, {}}
		lines = append(lines, m.pairs(w, 23, [][2]string{
			{"l", i18n.T("tui.empty_login")},
			{i18n.T("tui.adopt_command"), i18n.T("tui.help_adopt")},
		})...)
		return join(head, []string{""}, m.box(w, []seg{{i18n.T("tui.accounts"), p.bold}}, lines, nil), m.tail(w, now, true))
	}
	legend := func(on bool) []string {
		if on && lay == slim {
			return m.slimLegend(w)
		}
		return nil
	}
	if m.help {
		if s := join(head, []string{""}, m.table(w, lay, now, 0), legend(true), m.helpBox(w, 0), m.tail(w, now, true)); strings.Count(s, "\n") <= h {
			return s
		}
		return join(head, m.helpBox(w, m.helpRoom()), m.tail(w, now, false))
	}
	a, ok := m.selected()
	for _, step := range []struct {
		gap, legend bool
		trim        int
	}{{true, true, 0}, {false, true, 1}, {false, false, 1}, {false, false, 2}, {false, false, 3}, {false, false, 4}} {
		var panel []string
		if ok && !m.down && step.trim < 4 {
			panel = m.detail(w, lay == slim, a, now, step.trim)
		}
		gap := []string(nil)
		if step.gap {
			gap = []string{""}
		}
		if s := join(head, gap, m.table(w, lay, now, 0), legend(step.legend), panel, m.tail(w, now, step.gap)); strings.Count(s, "\n") <= h {
			return s
		}
	}
	tail := m.tail(w, now, false)
	return join(head, m.table(w, lay, now, max(1, h-len(head)-len(tail)-3)), tail)
}
