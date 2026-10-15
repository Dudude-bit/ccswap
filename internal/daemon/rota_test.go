package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

// shiftsText is shifts as "a 0s-30m0s" from t0, "-" for no end.
func shiftsText(ss []api.Shift) string {
	var out []string
	for _, s := range ss {
		to := "-"
		if s.To != nil {
			to = s.To.Sub(t0).String()
		}
		out = append(out, fmt.Sprintf("%s %v-%s", s.Account, s.From.Sub(t0), to))
	}
	return strings.Join(out, ", ")
}

func TestStatusShifts(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	status := func() api.Status {
		t.Helper()
		s, err := e.d.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	other, _ := store.Open(t.TempDir(), keychain.NewFake())
	if New(Options{Store: other, Now: e.clk.Now}); !other.Now().Equal(t0) {
		t.Fatal("the daemon must give the store its clock")
	}

	e.st.Update(func(x *store.State) error { x.Holders = nil; return nil })
	if b, _ := json.Marshal(status()); !strings.Contains(string(b), `"shifts":[]`) || strings.Contains(string(b), "last_switch_at") {
		t.Fatalf("no history: %s", b)
	}
	e.st.Update(func(x *store.State) error { x.Holders = []store.Holder{{Account: "a", Since: t0}}; return nil })

	e.clk.Add(30 * time.Minute)
	if _, err := e.d.Activate(ctx, "b", false); err != nil {
		t.Fatal(err)
	}
	s := status()
	if s.LastSwitchAt == nil || !s.LastSwitchAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatal("last_switch_at", s.LastSwitchAt)
	}
	if got := shiftsText(s.Shifts); got != "a 0s-30m0s, b 30m0s--" {
		t.Fatal(got)
	}

	e.clk.Add(10 * time.Minute)
	e.st.Update(func(x *store.State) error { x.Active = ""; return nil }) // no known account holds Claude Code
	e.clk.Add(10 * time.Minute)
	e.st.Update(func(x *store.State) error { x.Active = "b"; return nil })
	if got := shiftsText(status().Shifts); got != "a 0s-30m0s, b 30m0s-40m0s, b 50m0s--" {
		t.Fatal("a gap with no account must have no shift:", got)
	}

	e.clk.Add(24*time.Hour - 15*time.Minute) // the day back starts at 35m: a's shift is older
	if got := shiftsText(status().Shifts); got != "b 30m0s-40m0s, b 50m0s--" {
		t.Fatal("shifts of the last day:", got)
	}
}

func TestShiftsFromBeforeTheHolderLog(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "state.json"), fmt.Appendf(nil, `{"accounts":[{"name":"a"},{"name":"b"}],"active":"a","auto":false,"last_switch_at":%q}`,
		t0.Add(-2*time.Hour).Format(time.RFC3339)), 0o600)
	st, err := store.Open(home, keychain.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	d := New(Options{Store: st, Now: (&clock{t: t0}).Now})
	got := func() string {
		s, _ := d.Status(context.Background())
		return shiftsText(s.Shifts)
	}
	if g := got(); g != "a -2h0m0s--" {
		t.Fatal("the holder since the last switch:", g)
	}
	st.Update(func(x *store.State) error { x.Active = "b"; return nil }) // any switch writes Active so
	if g := got(); g != "a -2h0m0s-0s, b 0s--" {
		t.Fatal("after the first change:", g)
	}
}

func TestShiftsEndWhenAStrangerHoldsClaudeCode(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	got := func(c Core) string {
		t.Helper()
		s, _ := c.Status(ctx)
		return shiftsText(s.Shifts)
	}
	e.poll()
	e.clk.Add(10 * time.Minute)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atX", "rX", t0.Add(time.Hour)))
	e.api.set(func(f *fakeAnthropic) { f.emails["atX"] = "x@x" })
	e.poll() // seen at 10m
	n := len(e.st.State().Holders)
	for range 3 {
		e.clk.Add(time.Minute)
		e.poll()
	}
	if s, _ := e.d.Status(ctx); !s.UnknownActive || shiftsText(s.Shifts) != "a 0s-10m0s" || len(e.st.State().Holders) != n {
		t.Fatalf("a's shift ends once, when the stranger's login is seen: unknown %v, %s, %+v", s.UnknownActive, shiftsText(s.Shifts), e.st.State().Holders)
	}
	d := New(e.d.o) // a restart keeps it
	d.Poll(ctx)
	if g := got(d); g != "a 0s-10m0s" {
		t.Fatal("after a restart:", g)
	}
	e.clk.Add(7 * time.Minute)
	if _, err := e.d.Adopt(ctx, api.AdoptRequest{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if g := got(e.d); g != "a 0s-10m0s, x 20m0s--" {
		t.Fatal("adopted at 20m:", g)
	}
	st, err := store.Open(e.home, e.kc)
	if err != nil {
		t.Fatal(err)
	}
	if g := shiftsText(shifts(st.State().Holders, e.clk.Now())); g != "a 0s-10m0s, x 20m0s--" {
		t.Fatal("reopened:", g)
	}
}

func TestShiftResumesWhenTheKnownLoginReturns(t *testing.T) { // State.Active stayed a meanwhile
	e := newTenv(t)
	e.poll()
	e.clk.Add(10 * time.Minute)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atX", "rX", t0.Add(time.Hour)))
	e.api.set(func(f *fakeAnthropic) { f.emails["atX"] = "x@x" })
	e.poll()
	e.clk.Add(5 * time.Minute)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(time.Hour)))
	e.poll()
	if s, _ := e.d.Status(context.Background()); s.UnknownActive || shiftsText(s.Shifts) != "a 0s-10m0s, a 15m0s--" {
		t.Fatalf("unknown %v, %s", s.UnknownActive, shiftsText(s.Shifts))
	}
}

// usage5w is usage whose five-hour window is at h5 until r5 and weekly at w until t0+72h.
func usage5w(h5 float64, r5 time.Time, w float64) resp {
	return resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":%v,"resets_at":%q}}`,
		h5, r5.Format(time.RFC3339), w, t0.Add(72*time.Hour).Format(time.RFC3339))}
}

func TestLeavesAt(t *testing.T) {
	type answer struct {
		h5 float64
		r5 time.Duration // five-hour reset, from t0
		w  float64
	}
	rising := []answer{{50, 2 * time.Hour, 10}, {55, 2 * time.Hour, 10}, {60, 2 * time.Hour, 10}, {65, 2 * time.Hour, 10}}
	for _, c := range []struct {
		name string
		ans  []answer      // five minutes apart from t0
		want time.Duration // from t0; 0 = absent
		then func(e *tenv) // before the check
	}{
		{name: "rising: a point a minute, 95 at 45m", ans: rising, want: 45 * time.Minute},
		{name: "under 15 minutes of samples", ans: rising[:3]},
		{name: "flat", ans: []answer{{50, 2 * time.Hour, 10}, {50, 2 * time.Hour, 10}, {50, 2 * time.Hour, 10}, {50, 2 * time.Hour, 10}}},
		{name: "the weekly window first", ans: []answer{{50, 2 * time.Hour, 90}, {55, 2 * time.Hour, 91}, {60, 2 * time.Hour, 92}, {65, 2 * time.Hour, 93}},
			want: 40 * time.Minute}, // 0.2 a minute from 93 at 15m: 98 at 40m
		{name: "the five-hour window resets first", ans: []answer{{50, 30 * time.Minute, 10}, {55, 30 * time.Minute, 10}, {60, 30 * time.Minute, 10}, {65, 30 * time.Minute, 10}}},
		{name: "the weekly window while the five-hour resets first", ans: []answer{{50, 30 * time.Minute, 90}, {55, 30 * time.Minute, 91}, {60, 30 * time.Minute, 92}, {65, 30 * time.Minute, 93}},
			want: 40 * time.Minute},
		{name: "a drop starts over", ans: []answer{{50, 2 * time.Hour, 10}, {55, 2 * time.Hour, 10}, {60, 2 * time.Hour, 10}, {40, 2 * time.Hour, 10}, {45, 2 * time.Hour, 10}, {50, 2 * time.Hour, 10}}},
		{name: "15 minutes after a drop", ans: []answer{{50, 2 * time.Hour, 10}, {55, 2 * time.Hour, 10}, {60, 2 * time.Hour, 10}, {40, 2 * time.Hour, 10}, {45, 2 * time.Hour, 10}, {50, 2 * time.Hour, 10}, {55, 2 * time.Hour, 10}},
			want: 70 * time.Minute}, // from 40 at 15m to 55 at 30m: 95 at 70m
		{name: "a new resets_at starts over", ans: append(rising[:4:4], answer{70, 5 * time.Hour, 10})},
		{name: "at the threshold already", ans: []answer{{80, 2 * time.Hour, 10}, {85, 2 * time.Hour, 10}, {90, 2 * time.Hour, 10}, {96, 2 * time.Hour, 10}}},
		{name: "numbers an hour old", ans: rising, then: func(e *tenv) { e.clk.Add(time.Hour) }},
		{name: "a projection already past", ans: rising, then: func(e *tenv) { e.clk.Add(40 * time.Minute) }}, // every sample within the hour
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t)
			if err := e.d.SetAuto(context.Background(), false); err != nil { // a stays in use whatever its numbers
				t.Fatal(err)
			}
			for i, a := range c.ans {
				if i > 0 {
					e.clk.Add(5 * time.Minute)
				}
				e.api.set(func(f *fakeAnthropic) {
					f.usage["atA"] = usage5w(a.h5, t0.Add(a.r5), a.w)
					f.usage["atB"] = f.usage["atA"]
				})
				e.poll()
			}
			if c.then != nil {
				c.then(e)
			}
			got := e.view("a").LeavesAt
			if c.want == 0 && got != nil || c.want != 0 && (got == nil || !got.Equal(t0.Add(c.want))) {
				t.Fatalf("leaves_at %v, want t0+%v", got, c.want)
			}
			if l := e.view("b").LeavesAt; l != nil {
				t.Fatal("only the account in use leaves; b has", l)
			}
		})
	}
}

func TestLeavesAtCountsOnlyTheShift(t *testing.T) { // b's numbers from before it came into use are not its pace in use
	ctx := context.Background()
	for _, c := range []struct {
		name string
		into func(e *tenv)
	}{
		{"a switch", func(e *tenv) {
			if _, err := e.d.Activate(ctx, "b", false); err != nil {
				t.Fatal(err)
			}
		}},
		{"claude /login", func(e *tenv) { // LastSwitchAt stays: only the holder log dates the shift
			e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB", "rB", t0.Add(24*time.Hour)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t)
			e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(24*time.Hour)))
			e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(24*time.Hour)))
			if err := e.d.SetAuto(ctx, false); err != nil { // a, first by priority, would take over again
				t.Fatal(err)
			}
			b := func(h5 float64) {
				e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = usage5w(h5, t0.Add(5*time.Hour), 10) })
				e.poll()
			}
			for i := range 9 { // idle at 40 until 40m
				if i > 0 {
					e.clk.Add(5 * time.Minute)
				}
				b(40)
			}
			e.clk.Add(5 * time.Minute)
			c.into(e)
			for i, h5 := range []float64{40, 42, 44, 46} { // in use from 45m, 0.4 a minute
				if i > 0 {
					e.clk.Add(5 * time.Minute)
				}
				b(h5)
				if l := e.view("b").LeavesAt; i == 2 && l != nil {
					t.Fatal("10 minutes in use is too short a pace:", l)
				}
			}
			if st := e.st.State(); st.Active != "b" {
				t.Fatal("b not in use:", st.Active)
			}
			if l := e.view("b").LeavesAt; l == nil || !l.Equal(t0.Add(182*time.Minute+30*time.Second)) { // 46 at 60m: 95 at 182m30s
				t.Fatalf("leaves_at %v, want t0+3h2m30s", l)
			}
		})
	}
}

func TestLeavesAtNotWhenExhausted(t *testing.T) { // a model window at the weekly threshold already sends a away
	e := newTenv(t)
	if err := e.d.SetAuto(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for i, h5 := range []float64{50, 55, 60, 65} {
		if i > 0 {
			e.clk.Add(5 * time.Minute)
		}
		e.api.set(func(f *fakeAnthropic) {
			f.usage["atA"] = resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":%[3]q},`+
				`"seven_day_opus":{"utilization":99,"resets_at":%[3]q}}`, h5, t0.Add(2*time.Hour).Format(time.RFC3339), t0.Add(72*time.Hour).Format(time.RFC3339))}
		})
		e.poll()
	}
	if v := e.view("a"); !v.Exhausted || v.LeavesAt != nil {
		t.Fatalf("exhausted %v, leaves_at %v", v.Exhausted, v.LeavesAt)
	}
}

func TestLeavesAtSpansTheHourAtAShortCheck(t *testing.T) { // a sample a minute would fill 12 in 11 minutes
	e := newTenv(t)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(24*time.Hour))) // asked all along
	for i := range 21 {
		if i > 0 {
			e.clk.Add(time.Minute)
		}
		e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usage5w(50+float64(i), t0.Add(2*time.Hour), 10) })
		e.poll()
	}
	if l := e.view("a").LeavesAt; l == nil || !l.Equal(t0.Add(45*time.Minute)) { // a point a minute from 70 at 20m
		t.Fatalf("leaves_at %v, want t0+45m", l)
	}
	for range 90 {
		e.clk.Add(time.Minute)
		e.poll()
	}
	e.d.mu.Lock()
	n := len(e.d.usage["a"].five.pts)
	e.d.mu.Unlock()
	if n > trendMax {
		t.Fatalf("%d samples kept", n)
	}
}

func TestLeavesAtPaceOfTheLastHour(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atA", "rA", t0.Add(24*time.Hour)))
	for _, a := range []struct {
		wait time.Duration
		h5   float64
	}{{0, 10}, {45 * time.Minute, 40}, {5 * time.Minute, 45}, {5 * time.Minute, 50}, {5 * time.Minute, 55}, {5 * time.Minute, 60}} {
		e.clk.Add(a.wait)
		e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usage5w(a.h5, t0.Add(2*time.Hour), 10) })
		e.poll()
	}
	if l := e.view("a").LeavesAt; l == nil || !l.Equal(t0.Add(100*time.Minute)) { // from 40 at 45m, not 10 at 0: 95 at 100m
		t.Fatalf("leaves_at %v, want t0+1h40m", l)
	}
}

func TestLeavesAtFromTheOtherCcswap(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	var mu sync.Mutex
	h5 := 50.0
	srv := fakeRemote(t, "k", func() []api.Account {
		mu.Lock()
		defer mu.Unlock()
		checked, reset := e.clk.Now().Add(-30*time.Second), t0.Add(2*time.Hour)
		return []api.Account{{Email: "a@x", CheckedAt: &checked, FiveHour: &api.Window{Utilization: h5, ResetsAt: &reset}}}
	})
	key := "k"
	if err := e.d.SetRemote(ctx, api.RemoteRequest{Url: srv.URL, Key: &key}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		e.clk.Add(5 * time.Minute) // a in use since t0: its numbers checked before do not count
		mu.Lock()
		h5 = 50 + 5*float64(i)
		mu.Unlock()
		e.poll()
		e.poll() // the same numbers again add no sample
	}
	e.d.mu.Lock()
	n := len(e.d.usage["a"].five.pts)
	e.d.mu.Unlock()
	if e.api.count("usage atA") != 0 || n != 4 {
		t.Fatalf("a asked directly %d times, %d samples", e.api.count("usage atA"), n)
	}
	if l := e.view("a").LeavesAt; l == nil || !l.Equal(t0.Add(50*time.Minute-30*time.Second)) { // 65 at 19m30s: 95 at 49m30s
		t.Fatalf("leaves_at %v, want t0+49m30s", l)
	}
}

func TestLeavesAtModelWindows(t *testing.T) { // a model window at the weekly threshold sends a away too
	week := t0.Add(72 * time.Hour).Format(time.RFC3339)
	opus := func(u float64) string {
		return fmt.Sprintf(`"seven_day_opus":{"utilization":%v,"resets_at":%q}`, u, week)
	}
	scoped := func(u float64) string { // a limits[] entry may have no reset date
		return fmt.Sprintf(`"limits":[{"kind":"weekly_scoped","percent":%v,"scope":{"model":{"display_name":"Fable"}}}]`, u)
	}
	for _, c := range []struct {
		name  string
		extra []string      // the model windows of answers five minutes apart from t0, whose five-hour window is at 50, 55, ...
		want  time.Duration // from t0
	}{
		{"a model rising faster", []string{opus(90), opus(91), opus(92), opus(93)}, 40 * time.Minute},              // 0.2 a minute from 93 at 15m: 98 at 40m
		{"a model with no reset date", []string{scoped(90), scoped(91), scoped(92), scoped(93)}, 45 * time.Minute}, // not projected: the five-hour window's
		{"a model no longer listed", []string{opus(90), opus(91), opus(92), opus(93), ""}, 45 * time.Minute},       // the five-hour window's: 70 at 20m, 95 at 45m
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t)
			if err := e.d.SetAuto(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			for i, x := range c.extra {
				if i > 0 {
					e.clk.Add(5 * time.Minute)
				}
				if x != "" {
					x = "," + x
				}
				e.api.set(func(f *fakeAnthropic) {
					f.usage["atA"] = resp{200, fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":%q}%s}`,
						50+5*float64(i), t0.Add(2*time.Hour).Format(time.RFC3339), week, x)}
				})
				e.poll()
			}
			if l := e.view("a").LeavesAt; l == nil || !l.Equal(t0.Add(c.want)) {
				t.Fatalf("leaves_at %v, want t0+%v", l, c.want)
			}
		})
	}
}
