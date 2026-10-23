// Package policy decides when the daemon should switch accounts. Pure: no I/O, no clock.
package policy

import (
	"math"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/config"
)

// StaleAfter is how old usage may be and still count, unless the account is asked less often (FreshFor).
const StaleAfter = 5 * time.Minute

// CacheTTL is how long a prompt cache outlives the last request of a subscription session.
const CacheTTL = time.Hour

// SoonReset: room this close to its reset is worth a prompt-cache rewrite to spend before it is lost.
const SoonReset = 24 * time.Hour

// Strategy orders the switches; both put a higher priority first.
type Strategy string

const (
	// FillFirst takes the account with the most headroom within a priority and returns to a higher
	// priority as soon as min_dwell allows.
	FillFirst Strategy = "fill-first"
	// CacheAware returns to a higher priority like FillFirst, and within a priority takes the account
	// whose weekly room resets soonest. Every switch makes each session rewrite its prompt cache, so within a
	// priority and with a warm cache it moves only for room that resets within SoonReset; once Claude
	// Code has been idle for CacheTTL the switch is free.
	CacheAware Strategy = "cache-aware"
)

type Reason string

const (
	ReasonNone         Reason = ""
	ReasonExhausted    Reason = "exhausted"
	ReasonRecovered    Reason = "recovered"
	ReasonResetsSooner Reason = "resets_sooner"
	ReasonAllExhausted Reason = "all_exhausted"
)

type AccountState struct {
	Name          string
	Priority      int
	Enabled       bool
	NeedsLogin    bool
	FiveHour      *anthropic.Window
	SevenDay      *anthropic.Window
	Models        []anthropic.ModelWindow
	LimitHitUntil time.Time
	CheckedAt     time.Time
	FreshFor      time.Duration // how old CheckedAt may be for the data to count, at least StaleAfter
}

type Decision struct {
	SwitchTo string
	Reason   Reason
}

type limited struct {
	w         anthropic.Window
	threshold float64
}

// counting returns the windows that still matter at now, with their thresholds.
func counting(a AccountState, th config.Thresholds, now time.Time) []limited {
	var out []limited
	add := func(w *anthropic.Window, t float64) {
		if w != nil && (w.ResetsAt.IsZero() || w.ResetsAt.After(now)) {
			out = append(out, limited{*w, t})
		}
	}
	add(a.FiveHour, th.FiveHour)
	add(a.SevenDay, th.Weekly)
	for _, m := range a.Models {
		add(&m.Window, th.Weekly)
	}
	return out
}

func Exhausted(a AccountState, th config.Thresholds, now time.Time) bool {
	return ExhaustedBy(a, th, now) != ""
}

// ExhaustedBy names what blocks a: "five_hour", "seven_day", "model:<name>" or "limit_hit"; "" when nothing does.
func ExhaustedBy(a AccountState, th config.Thresholds, now time.Time) string {
	at := func(w *anthropic.Window, t float64) bool {
		return w != nil && (w.ResetsAt.IsZero() || w.ResetsAt.After(now)) && w.Utilization >= t
	}
	switch {
	case at(a.FiveHour, th.FiveHour):
		return "five_hour"
	case at(a.SevenDay, th.Weekly):
		return "seven_day"
	}
	for _, m := range a.Models {
		if at(&m.Window, th.Weekly) {
			return "model:" + m.Name
		}
	}
	if now.Before(a.LimitHitUntil) {
		return "limit_hit"
	}
	return ""
}

// Fresh reports whether a's usage still counts: no older than FreshFor, and never less than StaleAfter.
func Fresh(a AccountState, now time.Time) bool {
	return !a.CheckedAt.IsZero() && now.Sub(a.CheckedAt) <= max(a.FreshFor, StaleAfter)
}

func usable(a AccountState, th config.Thresholds, now time.Time) bool {
	return a.Enabled && !a.NeedsLogin && Fresh(a, now) && !Exhausted(a, th, now)
}

func recovered(a AccountState, th config.Thresholds, now time.Time) bool {
	if !usable(a, th, now) {
		return false
	}
	for _, l := range counting(a, th, now) {
		if l.w.Utilization > l.threshold-th.Hysteresis {
			return false
		}
	}
	return true
}

func headroom(a AccountState, th config.Thresholds, now time.Time) float64 {
	h := math.Inf(1)
	for _, l := range counting(a, th, now) {
		h = min(h, l.threshold-l.w.Utilization)
	}
	return h
}

// deadline is the earliest known reset among a's weekly windows (the week, per-model weeks), when its
// room there is lost; zero when none is known. The 5-hour window resets every few hours whatever
// happens, so it counts only for exhaustion.
func deadline(a AccountState, now time.Time) time.Time {
	var d time.Time
	weekly := func(w *anthropic.Window) {
		if w != nil && w.ResetsAt.After(now) && sooner(w.ResetsAt, d) {
			d = w.ResetsAt
		}
	}
	weekly(a.SevenDay)
	for _, m := range a.Models {
		weekly(&m.Window)
	}
	return d
}

// sooner orders deadlines; a zero one is never.
func sooner(a, b time.Time) bool { return !a.IsZero() && (b.IsZero() || a.Before(b)) }

// ahead orders candidates: priority, then for CacheAware the sooner deadline, then headroom.
func ahead(a, b AccountState, s Strategy, th config.Thresholds, now time.Time) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if da, db := deadline(a, now), deadline(b, now); s == CacheAware && !da.Equal(db) {
		return sooner(da, db)
	}
	return headroom(a, th, now) > headroom(b, th, now)
}

// best picks the first of accts by ahead; position in accts breaks ties.
func best(accts []AccountState, ok func(AccountState) bool, s Strategy, th config.Thresholds, now time.Time) (AccountState, bool) {
	var b AccountState
	found := false
	for _, a := range accts {
		if ok(a) && (!found || ahead(a, b, s, th, now)) {
			b, found = a, true
		}
	}
	return b, found
}

// worthLeaving reports whether a CacheAware switch from cur to a pays for itself. A higher priority
// always does: priority says whose limit to spend. At equal priority a must reset sooner, and with a
// warm cache within SoonReset and at least CacheTTL before cur, or two accounts would trade places
// every few minutes.
func worthLeaving(a, cur AccountState, now time.Time, idle time.Duration) bool {
	da, dc := deadline(a, now), deadline(cur, now)
	switch {
	case a.Priority > cur.Priority:
		return true
	case a.Priority < cur.Priority, !sooner(da, dc):
		return false
	case idle >= CacheTTL:
		return true
	case da.IsZero() || da.Sub(now) > SoonReset:
		return false
	}
	return dc.IsZero() || dc.Sub(da) >= CacheTTL
}

// FreeAt is when a stops being blocked (now when it is not); ok=false when a blocking window has no reset time.
func FreeAt(a AccountState, th config.Thresholds, now time.Time) (time.Time, bool) {
	t := now
	if now.Before(a.LimitHitUntil) {
		t = a.LimitHitUntil
	}
	for _, l := range counting(a, th, now) {
		if l.w.Utilization < l.threshold {
			continue
		}
		if l.w.ResetsAt.IsZero() {
			return time.Time{}, false
		}
		if l.w.ResetsAt.After(t) {
			t = l.w.ResetsAt
		}
	}
	return t, true
}

// Decide picks the next switch; idle is how long Claude Code has made no request (0 when unknown).
func Decide(now time.Time, accts []AccountState, active string, lastSwitch time.Time, th config.Thresholds, s Strategy, idle time.Duration) Decision {
	var cur AccountState
	found := false
	for _, a := range accts {
		if a.Name == active {
			cur, found = a, true
			break
		}
	}
	if !found {
		return Decision{}
	}
	loginOK := cur.Enabled && !cur.NeedsLogin
	// stale usage hides nothing a limit signal says: the endpoint often 429s right at the wall
	if loginOK && !Fresh(cur, now) && !now.Before(cur.LimitHitUntil) {
		return Decision{}
	}

	if !loginOK || Exhausted(cur, th, now) {
		if n := Next(now, accts, active, th, s); n != "" {
			return Decision{n, ReasonExhausted}
		}
		return allExhausted(now, accts, cur, loginOK, th)
	}

	if now.Sub(lastSwitch) < th.MinDwell {
		return Decision{}
	}
	better := func(a AccountState) bool { return a.Priority > cur.Priority }
	if s == CacheAware {
		better = func(a AccountState) bool { return worthLeaving(a, cur, now, idle) }
	}
	b, ok := best(accts, func(a AccountState) bool { return a.Name != active && recovered(a, th, now) && better(a) }, s, th, now)
	switch {
	case !ok:
		return Decision{}
	case b.Priority > cur.Priority:
		return Decision{b.Name, ReasonRecovered}
	}
	return Decision{b.Name, ReasonResetsSooner}
}

// Next is the account Decide switches to once active runs out, as things stand now; "" when none is usable.
func Next(now time.Time, accts []AccountState, active string, th config.Thresholds, s Strategy) string {
	b, ok := best(accts, func(a AccountState) bool { return a.Name != active && usable(a, th, now) }, s, th, now)
	if !ok {
		return ""
	}
	return b.Name
}

func allExhausted(now time.Time, accts []AccountState, cur AccountState, loginOK bool, th config.Thresholds) Decision {
	var own time.Time // zero: never, as with an unknown reset
	if loginOK {
		own, _ = FreeAt(cur, th, now)
	}
	var pick AccountState
	var pickAt time.Time
	for _, a := range accts {
		if a.Name == cur.Name || !a.Enabled || a.NeedsLogin || !Fresh(a, now) {
			continue
		}
		if at, ok := FreeAt(a, th, now); ok && (pickAt.IsZero() || at.Before(pickAt) || at.Equal(pickAt) && a.Priority > pick.Priority) {
			pick, pickAt = a, at
		}
	}
	if sooner(pickAt, own) {
		return Decision{pick.Name, ReasonAllExhausted}
	}
	return Decision{"", ReasonAllExhausted}
}
