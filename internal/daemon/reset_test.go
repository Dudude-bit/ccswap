package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

func grant(id string, left int) string { return grantEnds(id, left, t0.Add(30*24*time.Hour)) }

func grantEnds(id string, left int, end time.Time) string {
	return fmt.Sprintf(`{"id":%q,"resets_left":%d,"ends_at":%q,"usable_now":true}`, id, left, end.Format(time.RFC3339))
}

// usageResets is usage at week % of the weekly window with reset credits: next, then grants.
func usageResets(week float64, next string, grants ...string) resp {
	return resp{200, fmt.Sprintf(`{"five_hour":{"utilization":5,"resets_at":%q},"seven_day":{"utilization":%v,"resets_at":%q},`+
		`"cedar_ember":{"eligible":true,"grants":[%s],"next_grant_id":%q}}`,
		t0.Add(2*time.Hour).Format(time.RFC3339), week, t0.Add(72*time.Hour).Format(time.RFC3339), strings.Join(grants, ","), next)}
}

// withResets gives b two reset credits and its organization, at its weekly limit.
func withResets(t *testing.T) *tenv {
	e := newTenv(t)
	if err := e.st.Update(func(s *store.State) error {
		s.Accounts[1].OAuthAccount = json.RawMessage(`{"emailAddress":"b@x","organizationUuid":"org-b"}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = usageResets(100, "launch", grant("launch", 1), grant("tenure", 1))
	})
	e.poll()
	return e
}

// claimRe is a claim with a request id Anthropic takes: Claude Code's are 1 to 64 of [A-Za-z0-9_-].
var claimRe = regexp.MustCompile(`^\S+ \S+ \S+ [A-Za-z0-9_-]{1,64}$`)

// claimID is the request id of a claim.
func claimID(c string) string { return c[strings.LastIndexByte(c, ' ')+1:] }

func (e *tenv) claims() []string {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	return append([]string(nil), e.api.claims...)
}

func TestResetSpendsOneCreditAndAsksAgain(t *testing.T) {
	e := withResets(t)
	v := e.view("b")
	if r := v.Resets; r == nil || r.Left != 2 || len(r.Grants) != 2 || r.Grants[0].Id != "launch" || r.EndsAt == nil || !r.EndsAt.Equal(t0.Add(30*24*time.Hour)) || !v.Exhausted {
		t.Fatalf("%+v exhausted=%v", v.Resets, v.Exhausted)
	}
	if _, err := e.d.UseReset(context.Background(), "b", 1); !errors.Is(err, ErrResetChanged) || len(e.claims()) != 0 {
		t.Fatal("a count the daemon does not have spends nothing:", err, e.claims())
	}
	e.api.set(func(f *fakeAnthropic) {
		f.resets = []resp{{200, `{"result":"reset"}`}}
		f.onReset = func(string) { f.usage["atB"] = usageResets(0, "tenure", grant("tenure", 1)) }
	})
	e.d.mu.Lock()
	e.d.acct("b").limitHitUntil = e.clk.Now().Add(time.Hour)
	e.d.mu.Unlock()
	a, err := e.d.UseReset(context.Background(), "b", 2)
	if err != nil || a.Resets == nil || a.Resets.Left != 1 || a.Resets.Grants[0].Id != "tenure" || a.SevenDay == nil || a.SevenDay.Utilization != 0 || a.Exhausted {
		t.Fatalf("the answer shows the cleared limits: %+v %v", a, err)
	}
	if c := e.claims(); len(c) != 1 || !claimRe.MatchString(c[0]) || !strings.HasPrefix(c[0], "atB org-b launch ") {
		t.Fatal(c)
	}
	if _, err := e.d.UseReset(context.Background(), "b", 2); !errors.Is(err, ErrResetChanged) || len(e.claims()) != 1 {
		t.Fatal("a second click with the count it showed spends no second credit:", err, e.claims())
	}
}

func TestResetRetryNamesTheSameClaim(t *testing.T) {
	e := withResets(t)
	e.api.set(func(f *fakeAnthropic) { f.resets = []resp{{502, ""}, {200, `{"result":"already_used"}`}} })
	if _, err := e.d.UseReset(context.Background(), "b", 2); !errors.As(err, new(resetFailed)) {
		t.Fatal(err)
	}
	if v := e.view("b"); v.Resets == nil || v.Resets.Left != 2 {
		t.Fatal("an unconfirmed reset spends nothing here:", v.Resets)
	}
	if _, err := e.d.UseReset(context.Background(), "b", 2); err != nil {
		t.Fatal("a claim that landed before counts as done:", err)
	}
	if c := e.claims(); len(c) != 2 || c[0] != c[1] {
		t.Fatal("a retry names the same claim, so Anthropic spends one credit:", c)
	}
}

func TestResetClaimIDs(t *testing.T) {
	e := withResets(t)
	claim := func(answer resp) error {
		t.Helper()
		e.api.set(func(f *fakeAnthropic) { f.resets = []resp{answer} })
		_, err := e.d.UseReset(context.Background(), "b", 2)
		return err
	}
	last := func() string { c := e.claims(); return claimID(c[len(c)-1]) }

	if err := claim(resp{200, `{"result":"not_limited"}`}); !errors.Is(err, ErrNotLimited) {
		t.Fatal(err)
	}
	first := last()
	if err := claim(resp{502, ""}); !errors.As(err, new(resetFailed)) || last() == first {
		t.Fatal("a claim Anthropic answered for sure is not named again:", err, e.claims())
	}
	unsure := last()
	if err := claim(resp{200, `{"result":"unavailable"}`}); !errors.As(err, new(resetFailed)) || last() != unsure {
		t.Fatal("a retry names the claim with no sure answer:", err, e.claims())
	}
	e.clk.Add(claimRetry)
	if err := claim(resp{200, `{"result":"cooldown","cooldown_until":"` + e.clk.Now().Add(90*time.Minute).UTC().Format(time.RFC3339) + `"}`}); !errors.Is(err, ErrResetCooldown) ||
		last() == unsure || !strings.Contains(err.Error(), clockText(e.clk.Now().Add(90*time.Minute), e.clk.Now())) {
		t.Fatal("past claimRetry a claim is new, and a cooldown says until when:", err, e.claims())
	}
	cooled := last()

	e.d.mu.Lock()
	e.d.acct("b").limitHitUntil = e.clk.Now().Add(time.Hour)
	e.d.mu.Unlock()
	err := claim(resp{200, `{"result":"already_used"}`})
	if !errors.Is(err, ErrResetChanged) || last() == cooled || !strings.Contains(err.Error(), "b") {
		t.Fatal("already_used on a claim of its own is another's spend, not this one:", err)
	}
	e.d.mu.Lock()
	hit := e.d.acct("b").limitHitUntil
	e.d.mu.Unlock()
	if hit.IsZero() {
		t.Fatal("nothing was reset here: the limit it hit stays")
	}
}

func TestResetRetryCountsTheFirstSpendOnce(t *testing.T) {
	e := withResets(t)
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = usageResets(100, "launch", grant("launch", 2))
	})
	e.poll()
	e.api.set(func(f *fakeAnthropic) { f.resets = []resp{{502, ""}, {200, `{"result":"already_used"}`}} })
	if _, err := e.d.UseReset(context.Background(), "b", 2); !errors.As(err, new(resetFailed)) {
		t.Fatal(err)
	}
	e.d.awaitAsk(context.Background(), "b")
	e.api.set(func(f *fakeAnthropic) { f.usage["atB"] = usageResets(0, "launch", grant("launch", 1)) }) // the first claim landed
	if _, err := e.d.fetch(context.Background(), "b", "atB"); err != nil {
		t.Fatal(err)
	}
	a, err := e.d.UseReset(context.Background(), "b", 1)
	if err != nil || a.Resets == nil || a.Resets.Left != 1 {
		t.Fatalf("a count that shows the first claim's spend keeps it, once: %+v %v", a.Resets, err)
	}

	e = withResets(t)
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = usageResets(100, "launch", grant("launch", 2))
	})
	e.poll()
	e.api.set(func(f *fakeAnthropic) {
		f.resets = []resp{{502, ""}, {200, `{"result":"already_used"}`}}
		f.usage["atB"] = resp{429, ""} // no ask shows the spend
	})
	if _, err := e.d.UseReset(context.Background(), "b", 2); !errors.As(err, new(resetFailed)) {
		t.Fatal(err)
	}
	if a, err := e.d.UseReset(context.Background(), "b", 2); err != nil || a.Resets == nil || a.Resets.Left != 1 {
		t.Fatalf("a count that does not show it yet takes it off: %+v %v", a.Resets, err)
	}
}

func TestUsageAskedBeforeAResetDoesNotUndoIt(t *testing.T) {
	e := withResets(t)
	e.api.set(func(f *fakeAnthropic) {
		f.onUsage = func(string) { // a reset lands while this ask is out
			e.d.mu.Lock()
			u := e.d.acct("b")
			u.resets, u.spentAt = withLeft(u.resets, "launch", 0), e.clk.Now().Add(time.Second)
			e.d.mu.Unlock()
		}
	})
	if _, err := e.d.fetch(context.Background(), "b", "atB"); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) { f.onUsage = nil })
	if r := e.view("b").Resets; r == nil || r.Left != 1 || r.Grants[0].Id != "tenure" {
		t.Fatal("an answer asked before the reset keeps the spend:", r)
	}
	e.clk.Add(2 * time.Second)
	if _, err := e.d.fetch(context.Background(), "b", "atB"); err != nil {
		t.Fatal(err)
	}
	if r := e.view("b").Resets; r == nil || r.Left != 2 {
		t.Fatal("an ask after the reset says what Anthropic has:", r)
	}
}

func TestRemoteNumbersFromBeforeAResetAreNotTaken(t *testing.T) {
	e := withResets(t)
	e.api.set(func(f *fakeAnthropic) {
		f.resets = []resp{{200, `{"result":"reset"}`}}
		f.onReset = func(string) { f.usage["atB"] = usageResets(0, "tenure", grant("tenure", 1)) }
	})
	if _, err := e.d.UseReset(context.Background(), "b", 2); err != nil {
		t.Fatal(err)
	}
	remote := func(left int, credit string) bool {
		at := e.clk.Now().Add(time.Hour) // the other computer's clock runs ahead: its numbers look newest
		ra := api.Account{CheckedAt: &at, SevenDay: &api.Window{Utilization: 100}, Resets: &api.Resets{Left: left, Grants: []api.ResetGrant{{Id: credit, Left: left}}}}
		return e.d.takeRemote("b", ra, 2*time.Hour)
	}
	e.clk.Add(time.Minute) // the other ccswap has not asked since, and its clock labels its numbers now
	if remote(2, "launch") {
		t.Fatal("numbers with more resets than the reset here left predate it, whatever their clock: this one asks Anthropic")
	}
	if v := e.view("b"); v.Resets == nil || v.Resets.Left != 1 || v.SevenDay.Utilization != 0 {
		t.Fatal("numbers from before the reset keep neither its credit nor its limits:", v.Resets, v.SevenDay)
	}
	if !remote(1, "tenure") || e.view("b").SevenDay.Utilization != 100 {
		t.Fatal("once the other ccswap shows the reset, its numbers are taken")
	}
	if !remote(2, "launch2") || e.view("b").Resets.Left != 2 {
		t.Fatal("past that, a new grant is taken from it as any number")
	}
}

func TestRemoteNumbersBeforeAResetWhateverOtherGrants(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, wait time.Duration // when old runs out, from the reset; how long after it the other ccswap answers
	}{
		{"old ran out before the reset", -time.Minute, 0},
		{"old runs out after it", time.Minute, 2 * time.Minute},
		{"the other clock runs ahead past old's end", time.Minute, 0}, // it no longer names old, live here
	} {
		e := withResets(t)
		ends := e.clk.Now().Add(time.Minute + tc.before)
		e.api.set(func(f *fakeAnthropic) {
			f.usage["atB"] = usageResets(100, "launch", grant("launch", 2), grantEnds("old", 2, ends))
			f.resets = []resp{{200, `{"result":"reset"}`}}
			f.onReset = func(string) { f.usage["atB"] = resp{500, ""} } // the ask after the reset fails
		})
		e.d.fetch(context.Background(), "b", "atB")
		e.clk.Add(time.Minute)
		if _, err := e.d.UseReset(context.Background(), "b", e.view("b").Resets.Left); err != nil {
			t.Fatal(tc.name, err)
		}
		e.clk.Add(tc.wait + time.Second)
		remote := func(left int) bool {
			at := e.clk.Now()
			ra := api.Account{CheckedAt: &at, SevenDay: &api.Window{Utilization: 100}, Resets: &api.Resets{Left: left, Grants: []api.ResetGrant{{Id: "launch", Left: left}}}}
			return e.d.takeRemote("b", ra, time.Hour)
		}
		if remote(2) {
			t.Fatal(tc.name, ": numbers from before the reset are not taken")
		}
		if !remote(1) {
			t.Fatal(tc.name, ": numbers that show the reset are taken")
		}
	}
}

func TestRemoteGrantsKeepTheirOwnEnds(t *testing.T) {
	e := withResets(t)
	remote := func(gs ...api.ResetGrant) bool {
		e.clk.Add(time.Second) // each newer than the last
		at := e.clk.Now()
		ra := api.Account{CheckedAt: &at, SevenDay: &api.Window{Utilization: 100}, Resets: &api.Resets{Grants: gs}}
		return e.d.takeRemote("b", ra, time.Hour)
	}
	soon := e.clk.Now().Add(time.Minute)
	launch, tenure := api.ResetGrant{Id: "launch", Left: 1, EndsAt: &soon}, api.ResetGrant{Id: "tenure", Left: 1}
	if !remote(launch, tenure) {
		t.Fatal("fresh numbers are taken")
	}
	e.api.set(func(f *fakeAnthropic) {
		f.resets = []resp{{200, `{"result":"reset"}`}} // no resets_left
		f.onReset = func(string) { f.usage["atB"] = resp{500, ""} }
	})
	if _, err := e.d.UseReset(context.Background(), "b", 2); err != nil {
		t.Fatal(err)
	}
	if remote(launch, tenure) {
		t.Fatal("numbers from before the reset are not taken")
	}
	e.clk.Add(2 * time.Minute) // launch runs out; tenure, which ends later, is left
	if !remote(tenure) || e.view("b").Resets == nil || e.view("b").Resets.Left != 1 {
		t.Fatal("numbers that show the reset are taken, with the grant left:", e.view("b").Resets)
	}
}

func TestResetRefusals(t *testing.T) {
	e := withResets(t)
	for _, tc := range []struct {
		answer resp
		want   func(error) bool
	}{
		{resp{200, `{"result":"not_limited"}`}, func(err error) bool { return errors.Is(err, ErrNotLimited) }},
		{resp{200, `{"result":"cooldown"}`}, func(err error) bool { return errors.Is(err, ErrResetCooldown) }},
		{resp{200, `{"result":"unavailable"}`}, func(err error) bool { return errors.As(err, new(resetFailed)) }},
		{resp{200, `{}`}, func(err error) bool { return errors.As(err, new(resetFailed)) }},
		{resp{429, ""}, func(err error) bool { return errors.As(err, new(resetFailed)) }},
		{resp{401, ""}, func(err error) bool { return errors.As(err, new(resetFailed)) }},
		{resp{200, `{"result":"ineligible"}`}, func(err error) bool { return errors.Is(err, ErrNoResetCredit) }},
	} {
		e.api.set(func(f *fakeAnthropic) { f.resets = []resp{tc.answer} })
		if _, err := e.d.UseReset(context.Background(), "b", 2); !tc.want(err) {
			t.Errorf("%+v: %v", tc.answer, err)
		}
	}
	if v := e.view("b"); v.Resets != nil {
		t.Fatal("Anthropic says it has none: none shows until the next ask", v.Resets)
	}
}

func TestResetNeedsACreditAndALogin(t *testing.T) {
	e := newTenv(t)
	if _, err := e.d.UseReset(context.Background(), "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := e.d.UseReset(context.Background(), "b", 1); !errors.Is(err, ErrNoResetCredit) {
		t.Fatal("no credit before Anthropic names one:", err)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atA"] = usageResets(100, "launch", grant("launch", 1))
		f.usage["atB"] = usageResets(100, "launch", grant("launch", 1))
		f.resets = []resp{{200, `{"result":"reset"}`}}
	})
	e.poll()
	if _, err := e.d.UseReset(context.Background(), "b", 1); !errors.As(err, new(resetFailed)) || len(e.claims()) != 0 {
		t.Fatal("no organization, no claim:", err)
	}
	if err := e.st.Update(func(s *store.State) error {
		s.Accounts[0].OAuthAccount = json.RawMessage(`{"emailAddress":"a@x","organizationUuid":"org-a"}`)
		s.Accounts[1].NeedsLogin = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v := e.view("b"); v.Resets != nil {
		t.Fatal("an account that needs a login shows no credit to spend:", v.Resets)
	}
	if _, err := e.d.UseReset(context.Background(), "b", 1); !errors.Is(err, switcher.ErrNeedsLogin) {
		t.Fatal(err)
	}
	if _, err := e.d.UseReset(context.Background(), "a", 1); err != nil {
		t.Fatal("the account in Claude Code spends with Claude Code's token:", err)
	}
	if c := e.claims(); len(c) != 1 || !strings.HasPrefix(c[0], "atA org-a launch ") {
		t.Fatal(c)
	}
}

func TestRemoteResetsCarryOver(t *testing.T) {
	e := newTenv(t)
	at, ends := e.clk.Now(), t0.Add(24*time.Hour)
	ra := api.Account{CheckedAt: &at, SevenDay: &api.Window{Utilization: 50}, Resets: &api.Resets{Left: 2, EndsAt: &ends, Grants: []api.ResetGrant{{Id: "launch", Left: 2, EndsAt: &ends}}}}
	if !e.d.takeRemote("b", ra, time.Hour) {
		t.Fatal("fresh numbers are taken")
	}
	if r := e.view("b").Resets; r == nil || r.Left != 2 || r.Grants[0].Id != "launch" || !r.EndsAt.Equal(ends) {
		t.Fatal(r)
	}
	_, body, err := e.d.UsageRaw(context.Background(), "b", time.Hour)
	if err != nil || !strings.Contains(string(body), `"next_grant_id":"launch"`) {
		t.Fatalf("T3 gets the credits in the body: %s %v", body, err)
	}
	e.clk.Add(25 * time.Hour)
	if r := e.view("b").Resets; r != nil {
		t.Fatal("a grant that ran out shows no more:", r)
	}
}
