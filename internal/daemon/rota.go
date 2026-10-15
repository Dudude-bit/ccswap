package daemon

import (
	"slices"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/store"
)

const (
	shiftsFor = 24 * time.Hour   // the day of shifts Status shows
	trendSpan = time.Hour        // leavesAt's pace is the rise over the samples this recent
	trendMin  = 15 * time.Minute // and over this long at least
	trendMax  = 12               // samples kept per window
)

// shifts is who held Claude Code over the last shiftsFor, oldest first; the holder's shift has no end.
// An entry whose end is not after its start has no shift.
func shifts(hs []store.Holder, now time.Time) []api.Shift {
	out := []api.Shift{}
	for i, h := range hs {
		var to *time.Time
		if i+1 < len(hs) {
			to = &hs[i+1].Since
			if !to.After(h.Since) || !to.After(now.Add(-shiftsFor)) {
				continue
			}
		}
		if h.Account != "" { // no known account held it
			out = append(out, api.Shift{Account: h.Account, From: h.Since, To: to})
		}
	}
	return out
}

// trend is a window's utilization since its last reset or drop, oldest first.
type trend struct {
	resetsAt time.Time
	pts      []sample
}

type sample struct {
	at   time.Time
	util float64
}

// add keeps w's utilization as of at. Samples stay trendSpan/trendMax apart, the newest replaced
// until then, so a short check interval still spans the hour. A drop or another resets_at starts
// over: the pace before a reset says nothing of the pace after.
func (t *trend) add(w *anthropic.Window, at time.Time) {
	n := len(t.pts)
	switch {
	case w == nil:
		*t = trend{}
		return
	case n > 0 && !at.After(t.pts[n-1].at): // the same answer again: the other ccswap's is taken every tick
		return
	case n > 0 && (w.Utilization < t.pts[n-1].util || w.ResetsAt.Sub(t.resetsAt).Abs() > time.Minute): // a new window resets hours later; seconds are jitter
		t.pts = nil
	case n > 1 && t.pts[n-1].at.Sub(t.pts[n-2].at) < trendSpan/trendMax:
		t.pts = t.pts[:n-1]
	}
	t.resetsAt = w.ResetsAt
	if t.pts = append(t.pts, sample{at, w.Utilization}); len(t.pts) > trendMax {
		t.pts = t.pts[1:]
	}
}

// leavesAt is when t's window reaches threshold at the pace of the last trendSpan since since; zero
// when the pace is unknown or flat, the window resets first, or the numbers are too old to tell.
// A window with no reset date may reset before it gets there, so it has no projection.
func (t trend) leavesAt(threshold float64, now, since time.Time) time.Time {
	from := now.Add(-trendSpan)
	if since.After(from) {
		from = since
	}
	i := slices.IndexFunc(t.pts, func(s sample) bool { return !s.at.Before(from) })
	if i < 0 {
		return time.Time{}
	}
	first, last := t.pts[i], t.pts[len(t.pts)-1]
	resets := t.resetsAt
	if resets.IsZero() {
		return time.Time{}
	}
	span, rise := last.at.Sub(first.at), last.util-first.util
	if span < trendMin || rise <= 0 || last.util >= threshold {
		return time.Time{}
	}
	left := (threshold - last.util) / rise * float64(span) // a float: a slow pace overflows a Duration
	if left >= float64(resets.Sub(last.at)) {
		return time.Time{}
	}
	if at := last.at.Add(time.Duration(left)).Round(time.Second); at.After(now) && at.Before(resets) { // a float's last digits are noise
		return at
	}
	return time.Time{}
}

// shiftStart is when st.Active's shift began: its holder entry, else the last switch.
func shiftStart(st store.State) time.Time {
	if n := len(st.Holders); n > 0 && st.Holders[n-1].Account == st.Active {
		return st.Holders[n-1].Since
	}
	return st.LastSwitchAt
}

// sample keeps the windows' utilization as of the last answer, the daemon's own or the other ccswap's.
// A model the answer no longer lists takes its trend with it.
func (u *acctUsage) sample() {
	u.five.add(u.fiveHour, u.checkedAt)
	u.week.add(u.sevenDay, u.checkedAt)
	byModel := make(map[string]trend, len(u.models))
	for i := range u.models {
		t := u.byModel[u.models[i].Name]
		t.add(&u.models[i].Window, u.checkedAt)
		byModel[u.models[i].Name] = t
	}
	u.byModel = byModel
}

// leavesAt is when the account reaches a leave threshold at its pace in use since since, the earliest
// window's, as policy.ExhaustedBy counts them; zero = unknown. Its numbers from before, idle, are not that pace.
func (u *acctUsage) leavesAt(th config.Thresholds, now, since time.Time) time.Time {
	at := u.five.leavesAt(th.FiveHour, now, since)
	earliest := func(b time.Time) {
		if !b.IsZero() && (at.IsZero() || b.Before(at)) {
			at = b
		}
	}
	earliest(u.week.leavesAt(th.Weekly, now, since))
	for _, t := range u.byModel {
		earliest(t.leavesAt(th.Weekly, now, since))
	}
	return at
}
