package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

func grant(id string, left int) string {
	return fmt.Sprintf(`{"id":%q,"resets_left":%d,"ends_at":%q,"usable_now":true}`, id, left, t0.Add(30*24*time.Hour).Format(time.RFC3339))
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

func (e *tenv) claims() []string {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	return append([]string(nil), e.api.claims...)
}

func TestResetSpendsOneCreditAndAsksAgain(t *testing.T) {
	e := withResets(t)
	v := e.view("b")
	if r := v.Resets; r == nil || r.Left != 2 || r.Credit != "launch" || r.EndsAt == nil || !r.EndsAt.Equal(t0.Add(30*24*time.Hour)) || !v.Exhausted {
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
	if err != nil || a.Resets == nil || a.Resets.Left != 1 || a.Resets.Credit != "tenure" || a.SevenDay == nil || a.SevenDay.Utilization != 0 || a.Exhausted {
		t.Fatalf("the answer shows the cleared limits: %+v %v", a, err)
	}
	if c := e.claims(); len(c) != 1 || c[0] != "atB org-b launch "+resetRequestID("b@x", "launch", 1) {
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
	ra := api.Account{CheckedAt: &at, SevenDay: &api.Window{Utilization: 50}, Resets: &api.Resets{Left: 2, Credit: "launch", EndsAt: &ends}}
	if !e.d.takeRemote("b", ra, time.Hour) {
		t.Fatal("fresh numbers are taken")
	}
	if r := e.view("b").Resets; r == nil || r.Left != 2 || r.Credit != "launch" || !r.EndsAt.Equal(ends) {
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
