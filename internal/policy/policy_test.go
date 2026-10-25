package policy

import (
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/config"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

var th = config.Thresholds{FiveHour: 95, Weekly: 98, Hysteresis: 10, MinDwell: 10 * time.Minute}

type opt func(*AccountState)

func acct(name string, prio int, opts ...opt) AccountState {
	a := AccountState{Name: name, Priority: prio, Enabled: true, CheckedAt: now.Add(-time.Minute)}
	for _, o := range opts {
		o(&a)
	}
	return a
}

func win(u float64, resetIn time.Duration) *anthropic.Window {
	w := &anthropic.Window{Utilization: u}
	if resetIn != 0 {
		w.ResetsAt = now.Add(resetIn)
	}
	return w
}

func h5(u float64) opt { return func(a *AccountState) { a.FiveHour = win(u, time.Hour) } }
func h5r(u float64, d time.Duration) opt {
	return func(a *AccountState) { a.FiveHour = win(u, d) }
}
func wk(u float64) opt { return func(a *AccountState) { a.SevenDay = win(u, 48*time.Hour) } }
func wkr(u float64, d time.Duration) opt {
	return func(a *AccountState) { a.SevenDay = win(u, d) }
}
func model(n string, u float64) opt {
	return func(a *AccountState) {
		a.Models = append(a.Models, anthropic.ModelWindow{Name: n, Window: *win(u, 48*time.Hour)})
	}
}
func limitUntil(d time.Duration) opt { return func(a *AccountState) { a.LimitHitUntil = now.Add(d) } }
func needsLogin() opt                { return func(a *AccountState) { a.NeedsLogin = true } }
func disabled() opt                  { return func(a *AccountState) { a.Enabled = false } }
func checked(ago time.Duration) opt  { return func(a *AccountState) { a.CheckedAt = now.Add(-ago) } }

func TestDecide(t *testing.T) {
	tests := []struct {
		name        string
		accts       func() []AccountState
		active      string
		sinceSwitch time.Duration
		want        Decision
	}{
		{"active below thresholds, nothing higher recovered",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(90)), acct("cur", 20, h5(50), wk(50))}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"active 5h=95 -> next usable by priority",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(10)), acct("mid", 30, h5(10)), acct("cur", 20, h5(95))}
			}, "cur", time.Hour, Decision{"high", ReasonExhausted}},
		{"active weekly=98",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, h5(10), wk(98))}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"active opus model=98, 5h/weekly low",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, h5(10), wk(10), model("opus", 98))}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"two usable at same priority, more headroom wins",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5(50)), acct("b", 30, h5(20)), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"b", ReasonExhausted}},
		{"tie on headroom -> earlier position",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5(20)), acct("b", 30, h5(20)), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"a", ReasonExhausted}},
		{"targets NeedsLogin, disabled, stale, exhausted all skipped",
			func() []AccountState {
				return []AccountState{
					acct("nl", 50, needsLogin()), acct("dis", 45, disabled()), acct("stale", 44, checked(6*time.Minute)),
					acct("exh", 40, h5(96)), acct("mid", 30, h5(10)), acct("cur", 20, h5(99)),
				}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"stale higher target ignored",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(10), checked(10*time.Minute)), acct("mid", 30, h5(10)), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"higher recovered, dwell 11m",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(80), wk(50)), acct("cur", 20, h5(10))}
			}, "cur", 11 * time.Minute, Decision{"high", ReasonRecovered}},
		{"higher recovered, dwell 5m",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(80), wk(50)), acct("cur", 20, h5(10))}
			}, "cur", 5 * time.Minute, Decision{"", ReasonNone}},
		{"higher at 5h 90 is above 85",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(90)), acct("cur", 20, h5(10))}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"higher with weekly 89 not recovered",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(10), wk(89)), acct("cur", 20, h5(10))}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"recovery picks highest priority recovered",
			func() []AccountState {
				return []AccountState{acct("low", 10, h5(10)), acct("mid", 30, h5(10)), acct("high", 40, h5(10)), acct("cur", 20, h5(10))}
			}, "cur", time.Hour, Decision{"high", ReasonRecovered}},
		{"higher with passed reset does not block recovery",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5r(99, -time.Minute)), acct("cur", 20, h5(10))}
			}, "cur", time.Hour, Decision{"high", ReasonRecovered}},
		{"active 5h 99 but ResetsAt in the past",
			func() []AccountState {
				return []AccountState{acct("low", 10, h5(10)), acct("cur", 20, h5r(99, -time.Minute))}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"active LimitHitUntil in future, windows low",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, h5(10), limitUntil(30*time.Minute))}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"LimitHitUntil in the past is ignored",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, h5(10), limitUntil(-time.Minute))}
			}, "cur", 11 * time.Minute, Decision{"mid", ReasonRecovered}},
		{"all exhausted, another resets earlier",
			func() []AccountState {
				return []AccountState{
					acct("a", 30, h5r(99, 30*time.Minute)),
					acct("cur", 20, h5r(99, 2*time.Hour)),
				}
			}, "cur", time.Hour, Decision{"a", ReasonAllExhausted}},
		{"all exhausted, active resets earliest",
			func() []AccountState {
				return []AccountState{
					acct("a", 30, h5r(99, 3*time.Hour)),
					acct("cur", 20, h5r(99, time.Hour)),
				}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"all exhausted, equal free-at not strictly earlier",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5r(99, time.Hour)), acct("cur", 20, h5r(99, time.Hour))}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"all exhausted, free-at tie among others -> higher priority",
			func() []AccountState {
				return []AccountState{
					acct("lo", 10, h5r(99, time.Hour)), acct("hi", 30, h5r(99, time.Hour)),
					acct("cur", 20, h5r(99, 5*time.Hour)),
				}
			}, "cur", time.Hour, Decision{"hi", ReasonAllExhausted}},
		{"all exhausted, blocking window with unknown reset is +Inf",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5r(99, 0)), acct("cur", 20, h5r(99, 2*time.Hour))}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"all exhausted, limit-hit backoff extends free-at",
			func() []AccountState {
				return []AccountState{
					acct("a", 30, h5r(99, time.Hour), limitUntil(4*time.Hour)),
					acct("cur", 20, h5r(99, 2*time.Hour)),
				}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"all exhausted, stale other is not a target",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5(10), checked(6*time.Minute)), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"all exhausted, others NeedsLogin/disabled",
			func() []AccountState {
				return []AccountState{acct("a", 30, needsLogin()), acct("b", 25, disabled()), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"", ReasonAllExhausted}},
		{"active NeedsLogin",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, needsLogin())}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"active disabled",
			func() []AccountState {
				return []AccountState{acct("mid", 30, h5(10)), acct("cur", 20, disabled())}
			}, "cur", time.Hour, Decision{"mid", ReasonExhausted}},
		{"active stale, others usable",
			func() []AccountState {
				return []AccountState{acct("high", 40, h5(10)), acct("cur", 20, h5(99), checked(6*time.Minute))}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"active CheckedAt zero is stale",
			func() []AccountState {
				a := acct("cur", 20, h5(99))
				a.CheckedAt = time.Time{}
				return []AccountState{acct("high", 40, h5(10)), a}
			}, "cur", time.Hour, Decision{"", ReasonNone}},
		{"active stale but limit-hit still switches",
			func() []AccountState {
				a := acct("cur", 20, h5(50), limitUntil(time.Hour))
				a.CheckedAt = now.Add(-time.Hour)
				return []AccountState{acct("high", 40, h5(10)), a}
			}, "cur", time.Hour, Decision{"high", ReasonExhausted}},
		{"active not in accts",
			func() []AccountState { return []AccountState{acct("high", 40, h5(10))} },
			"ghost", time.Hour, Decision{"", ReasonNone}},
		{"account with no windows but fresh CheckedAt is usable",
			func() []AccountState {
				return []AccountState{acct("empty", 30), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"empty", ReasonExhausted}},
		{"no-window account has infinite headroom",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5(0)), acct("empty", 30), acct("cur", 20, h5(99))}
			}, "cur", time.Hour, Decision{"empty", ReasonExhausted}},
		{"active exhausted, no usable target but others in backoff -> all exhausted switch",
			func() []AccountState {
				return []AccountState{acct("a", 30, h5(10), limitUntil(10*time.Minute)), acct("cur", 20, wkr(99, 5*time.Hour))}
			}, "cur", time.Hour, Decision{"a", ReasonAllExhausted}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(now, tc.accts(), tc.active, now.Add(-tc.sinceSwitch), th, FillFirst, 0)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExhaustedIgnoresStaleness(t *testing.T) {
	if !Exhausted(acct("x", 1, h5(96), checked(time.Hour)), th, now) {
		t.Error("stale data at limit must still be exhausted")
	}
	if Exhausted(acct("x", 1, h5(94)), th, now) {
		t.Error("94 < 95 must not be exhausted")
	}
}

func TestExhaustedBy(t *testing.T) {
	for _, tc := range []struct {
		a    AccountState
		want string
	}{
		{acct("x", 1, h5(10), wk(10)), ""},
		{acct("x", 1, h5(95), wk(99)), "five_hour"},
		{acct("x", 1, h5r(99, -time.Minute), wk(98)), "seven_day"},
		{acct("x", 1, h5(10), model("opus", 98)), "model:opus"},
		{acct("x", 1, h5(10), limitUntil(time.Minute)), "limit_hit"},
	} {
		if got := ExhaustedBy(tc.a, th, now); got != tc.want || Exhausted(tc.a, th, now) != (tc.want != "") {
			t.Errorf("%+v: got %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestFreeAt(t *testing.T) {
	if at, ok := FreeAt(acct("x", 1, h5r(99, time.Hour), limitUntil(2*time.Hour)), th, now); !ok || !at.Equal(now.Add(2*time.Hour)) {
		t.Errorf("%v %v", at, ok)
	}
	if _, ok := FreeAt(acct("x", 1, h5r(99, 0)), th, now); ok {
		t.Error("unknown reset must not be ok")
	}
	if at, ok := FreeAt(acct("x", 1, h5(10)), th, now); !ok || !at.Equal(now) {
		t.Errorf("not blocked: %v %v", at, ok)
	}
	exact := 3*time.Hour + 123*time.Millisecond // the page matches it to the window's own reset
	if at, ok := FreeAt(acct("x", 1, h5r(99, exact)), th, now); !ok || !at.Equal(now.Add(exact)) {
		t.Errorf("reset at %v, free at %v", now.Add(exact), at)
	}
}

func TestFirstFree(t *testing.T) { // what the page names; Decide's all-exhausted cases cover the switch
	for _, tc := range []struct {
		name  string
		accts []AccountState
		want  string
	}{
		{"staying names the active", []AccountState{acct("a", 30, h5r(99, 3*time.Hour)), acct("cur", 20, h5r(99, time.Hour))}, "cur"},
		{"another sooner", []AccountState{acct("a", 30, h5r(99, 30*time.Minute)), acct("cur", 20, h5r(99, time.Hour))}, "a"},
		{"active unknown, another known", []AccountState{acct("a", 30, h5r(99, 3*time.Hour)), acct("cur", 20, h5r(99, 0))}, "a"},
		{"nothing known", []AccountState{acct("a", 30, h5r(99, 0)), acct("cur", 20, h5r(99, 0))}, ""},
		{"a stale one is no pick", []AccountState{acct("a", 30, h5r(99, time.Minute), checked(6*time.Minute)), acct("cur", 20, h5r(99, time.Hour))}, "cur"},
		{"no active", []AccountState{acct("a", 30, h5r(99, 3*time.Hour)), acct("b", 20, h5r(99, time.Hour))}, "b"},
	} {
		if got := FirstFree(now, tc.accts, "cur", th); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDecideCacheAware(t *testing.T) {
	cur := func(opts ...opt) AccountState { return acct("b", 20, opts...) }
	warm, cold := time.Duration(0), CacheTTL
	for _, tc := range []struct {
		name  string
		accts []AccountState
		s     Strategy
		idle  time.Duration
		since time.Duration
		want  Decision
	}{
		{"warm: same priority, room resets 2 h sooner within a day -> switch",
			[]AccountState{acct("a", 20, wkr(20, 2*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, warm, time.Hour, Decision{"a", ReasonResetsSooner}},
		{"warm: same priority, resets only 30 min sooner -> stay",
			[]AccountState{acct("a", 20, wkr(20, 210*time.Minute)), cur(wkr(30, 4*time.Hour))}, CacheAware, warm, time.Hour, Decision{}},
		{"cold: same priority, resets 30 min sooner -> switch, it is free",
			[]AccountState{acct("a", 20, wkr(20, 210*time.Minute)), cur(wkr(30, 4*time.Hour))}, CacheAware, cold, time.Hour, Decision{"a", ReasonResetsSooner}},
		{"cold: same priority, resets later -> stay",
			[]AccountState{acct("a", 20, wkr(20, 72*time.Hour)), cur(wkr(30, 2*time.Hour))}, CacheAware, cold, time.Hour, Decision{}},
		{"warm: same priority, sooner but days away -> stay",
			[]AccountState{acct("a", 20, wkr(20, 72*time.Hour)), cur()}, CacheAware, warm, time.Hour, Decision{}},
		{"cold: same priority, current has no known reset -> switch",
			[]AccountState{acct("a", 20, wkr(20, 72*time.Hour)), cur()}, CacheAware, cold, time.Hour, Decision{"a", ReasonResetsSooner}},
		{"warm: higher priority recovered, resets in days -> climb at once, priority beats the cache",
			[]AccountState{acct("a", 30, wkr(20, 72*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, warm, time.Hour, Decision{"a", ReasonRecovered}},
		{"cold: higher priority recovered -> climb",
			[]AccountState{acct("a", 30, wkr(20, 72*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, cold, time.Hour, Decision{"a", ReasonRecovered}},
		{"warm: higher priority resets within a day -> climb, even if later than current",
			[]AccountState{acct("a", 30, wkr(20, 10*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, warm, time.Hour, Decision{"a", ReasonRecovered}},
		{"warm: sooner but not recovered (above threshold minus hysteresis) -> stay",
			[]AccountState{acct("a", 20, wkr(90, 2*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, warm, time.Hour, Decision{}},
		{"within min_dwell -> stay",
			[]AccountState{acct("a", 20, wkr(20, 2*time.Hour)), cur(wkr(30, 4*time.Hour))}, CacheAware, cold, 5 * time.Minute, Decision{}},
		{"lower priority never pulls, however soon it resets",
			[]AccountState{acct("a", 10, h5r(20, 10*time.Minute)), cur(wkr(30, 4*time.Hour))}, CacheAware, cold, time.Hour, Decision{}},
		{"exhausted: cache-aware takes the sooner reset within the priority",
			[]AccountState{acct("c", 20, wkr(10, 72*time.Hour)), acct("a", 20, wkr(50, 5*time.Hour)), acct("b", 30, h5(99))}, CacheAware, warm, time.Hour, Decision{"a", ReasonExhausted}},
		{"exhausted: fill-first takes the most headroom within the priority",
			[]AccountState{acct("c", 20, wkr(10, 72*time.Hour)), acct("a", 20, wkr(50, 5*time.Hour)), acct("b", 30, h5(99))}, FillFirst, warm, time.Hour, Decision{"c", ReasonExhausted}},
		{"fill-first ignores a sooner reset at the same priority",
			[]AccountState{acct("a", 20, wkr(20, 2*time.Hour)), cur(wkr(30, 4*time.Hour))}, FillFirst, cold, time.Hour, Decision{}},
		{"fill-first climbs at once",
			[]AccountState{acct("a", 30, wkr(20, 72*time.Hour)), cur(wkr(30, 4*time.Hour))}, FillFirst, warm, time.Hour, Decision{"a", ReasonRecovered}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(now, tc.accts, "b", now.Add(-tc.since), th, tc.s, tc.idle); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNext(t *testing.T) {
	accts := []AccountState{
		acct("cur", 40, h5(50)),
		acct("roomy", 30, wkr(10, 72*time.Hour)),
		acct("soon", 30, wkr(60, 5*time.Hour)),
		acct("full", 30, h5(99)),
		acct("high-but-off", 50, h5(1), func(a *AccountState) { a.Enabled = false }),
		acct("low", 10, h5(1)),
	}
	if got := Next(now, accts, "cur", th, FillFirst); got != "roomy" {
		t.Errorf("fill-first: %q, want the most room within the top usable priority", got)
	}
	if got := Next(now, accts, "cur", th, CacheAware); got != "soon" {
		t.Errorf("cache-aware: %q, want the soonest reset within the top usable priority", got)
	}
	if got := Next(now, []AccountState{acct("cur", 40), acct("full", 30, h5(99))}, "cur", th, FillFirst); got != "" {
		t.Errorf("nothing usable: %q", got)
	}
}

func TestCacheAwareOrdersByWeeklyResets(t *testing.T) { // the 5-hour window resets anyway: it only exhausts
	a := acct("a", 20, h5r(3, 2*time.Hour), wkr(40, 5*24*time.Hour))
	b := acct("b", 20, wkr(30, 10*time.Hour))
	if got := Next(now, []AccountState{a, b, acct("cur", 30, h5(99))}, "cur", th, CacheAware); got != "b" {
		t.Errorf("next %q, want b: its week resets first", got)
	}
	if got := Decide(now, []AccountState{a, b}, "b", now.Add(-time.Hour), th, CacheAware, 2*time.Hour); got != (Decision{}) {
		t.Errorf("idle on b: %+v, want to stay", got)
	}
}

func TestFreshFor(t *testing.T) {
	freshFor := func(d time.Duration) opt { return func(a *AccountState) { a.FreshFor = d } }
	for _, tc := range []struct {
		ago, freshFor time.Duration
		want          bool
	}{
		{4 * time.Minute, 0, true},
		{6 * time.Minute, 0, false},
		{6 * time.Minute, time.Minute, false}, // never less than StaleAfter
		{20 * time.Minute, 0, false},
		{20 * time.Minute, time.Hour, true}, // an account checked every 30 minutes stays a candidate
		{time.Hour + time.Second, time.Hour, false},
	} {
		s := acct("slow", 30, checked(tc.ago), freshFor(tc.freshFor))
		if Fresh(s, now) != tc.want || (Next(now, []AccountState{s, acct("cur", 40, h5(99))}, "cur", th, FillFirst) == "slow") != tc.want {
			t.Errorf("answer %v old, fresh for %v: want fresh=%v", tc.ago, tc.freshFor, tc.want)
		}
	}
}
