package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

var (
	// ErrNoResetCredit: the account has no reset credit left, as ccswap last saw or as Anthropic says.
	ErrNoResetCredit = errors.New("no reset credit left")
	// ErrResetChanged: the caller showed another count of reset credits than the daemon has, or another
	// claim spent the credit first.
	ErrResetChanged = errors.New("reset credits changed")
	// ErrNotLimited: Anthropic spends the credit only at a limit.
	ErrNotLimited = errors.New("not at a limit")
	// ErrResetCooldown: Anthropic wants a pause between resets.
	ErrResetCooldown = errors.New("reset cooldown")
)

// claimRetry is how long a retry names again a claim Anthropic gave no sure answer to, as Claude Code
// does: within it, a retry of a claim that landed spends nothing more.
const claimRetry = 10 * time.Minute

// resetClaim is a reset claim Anthropic gave no sure answer to; the zero value is none.
type resetClaim struct {
	credit, id string
	left       int       // the resets the grant had when it was first sent
	at         time.Time // when it was first sent
}

// resetFailed is a reset Anthropic did not confirm, with why; trying again is safe.
type resetFailed struct{ err error }

func (e resetFailed) Error() string { return e.err.Error() }
func (e resetFailed) Unwrap() error { return e.err }

// liveCredits is credits without the grants that ran out by now; the one a reset spends next stays first.
func liveCredits(credits []anthropic.ResetCredit, now time.Time) []anthropic.ResetCredit {
	return slices.DeleteFunc(slices.Clone(credits), func(c anthropic.ResetCredit) bool {
		return !c.EndsAt.IsZero() && !c.EndsAt.After(now)
	})
}

func creditsLeft(credits []anthropic.ResetCredit) int {
	n := 0
	for _, c := range credits {
		n += c.Left
	}
	return n
}

// withLeft is credits with n resets left of the grant id, in a new slice: views read the old one unlocked.
func withLeft(credits []anthropic.ResetCredit, id string, n int) []anthropic.ResetCredit {
	var out []anthropic.ResetCredit
	for _, c := range credits {
		if c.ID == id {
			c.Left = n
		}
		if c.Left > 0 {
			out = append(out, c)
		}
	}
	return out
}

// UseReset spends one of name's reset credits when the daemon has left of them, as the caller showed,
// then asks the account's usage again. Each claim has an id of its own, as Claude Code's do; only a retry
// of a claim Anthropic gave no sure answer to names it again, so Anthropic spends one credit for both.
func (d *Daemon) UseReset(ctx context.Context, name string, left int) (api.Account, error) {
	defer d.bus.publish()
	d.resetMu.Lock() // a second click waits, then finds the count changed
	defer d.resetMu.Unlock()
	st, holder := d.o.Store.State(), d.holderNow()
	a, ok := st.Find(name)
	switch {
	case !ok:
		return api.Account{}, ErrNotFound
	case a.NeedsLogin:
		return api.Account{}, i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, name)
	}
	now := d.o.Now()
	d.mu.Lock()
	credits := liveCredits(d.acct(name).resets, now)
	d.mu.Unlock()
	switch n := creditsLeft(credits); {
	case n == 0:
		return api.Account{}, i18n.Err("daemon.no_reset_credit", ErrNoResetCredit, name)
	case n != left:
		return api.Account{}, i18n.Err("daemon.reset_changed", ErrResetChanged, name, n)
	}
	var oa struct {
		Org string `json:"organizationUuid"`
	}
	if json.Unmarshal(a.OAuthAccount, &oa) != nil || oa.Org == "" {
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_no_org", name)}
	}
	tok, ok := d.accessToken(name, holder)
	switch {
	case !ok && name == holder:
		return api.Account{}, switcher.ErrEntryExpired
	case !ok:
		return api.Account{}, switcher.ErrTargetRefreshing
	}
	credit := credits[0]
	d.mu.Lock()
	u := d.acct(name)
	retry := u.claim.credit == credit.ID && now.Sub(u.claim.at) < claimRetry
	if !retry {
		u.claim = resetClaim{credit.ID, rand.Text(), credit.Left, now}
	}
	claim := u.claim
	d.mu.Unlock()
	status, res, err := d.o.API.Reset(d.tickCtx(ctx), tok, oa.Org, credit.ID, claim.id)
	answered := d.o.Now()
	if err != nil || res.Result != "reset" && res.Result != "already_used" {
		slog.Warn("reset: not confirmed", "account", name, "credit", credit.ID, "retry", retry, "status", status, "result", res.Result, "err", err)
	}
	if _, ok := d.o.Store.State().Find(name); !ok { // removed meanwhile: nothing here to update
		return api.Account{}, ErrNotFound
	}
	// settled forgets the claim once Anthropic answered it for sure; the next one gets an id of its own.
	settled := func() {
		d.mu.Lock()
		if u := d.acct(name); u.claim.id == claim.id {
			u.claim = resetClaim{}
		}
		d.mu.Unlock()
	}
	// reask shows, once the account's usage was asked again, whether a claim landed.
	reask := func() { go d.reask(d.tickCtx(ctx), name) }
	switch {
	case status == 0:
		reask()
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_no_answer")}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_login_refused", name)}
	case status == http.StatusTooManyRequests:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_rate_limited")}
	case status/100 == 5:
		reask()
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_status", status)}
	case status/100 != 2:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_status", status)}
	}
	switch res.Result {
	case "reset":
	case "already_used":
		if !retry { // a claim of another, such as Claude Code at its limit, spent it first
			settled()
			reask()
			return api.Account{}, i18n.Err("daemon.reset_used", ErrResetChanged, name)
		}
	case "not_limited":
		settled()
		if retry { // its first claim may have cleared the limit
			reask()
		}
		return api.Account{}, i18n.Err("daemon.not_limited", ErrNotLimited, name)
	case "cooldown":
		if !retry { // after a retry, it may be that claim still landing: a retry names it again
			settled()
		}
		reask()
		if res.CooldownUntil.After(now) {
			return api.Account{}, i18n.Err("daemon.reset_cooldown_until", ErrResetCooldown, name, clockText(res.CooldownUntil, now))
		}
		return api.Account{}, i18n.Err("daemon.reset_cooldown", ErrResetCooldown, name)
	case "ineligible":
		settled()
		d.mu.Lock()
		d.acct(name).resets = nil // until the next ask says otherwise
		d.mu.Unlock()
		return api.Account{}, i18n.Err("daemon.no_reset_credit", ErrNoResetCredit, name)
	default: // unavailable, a word ccswap does not know, or none
		reask()
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_unconfirmed")}
	}
	settled()
	n := credit.Left - 1              // this claim's spend
	if res.Result == "already_used" { // a retry: the first claim's spend, which the count may show already
		n = min(credit.Left, claim.left-1)
	}
	if res.Left != nil {
		n = *res.Left
	}
	slog.Info("reset credit spent", "account", name, "credit", credit.ID, "retry", retry, "grant_left", n)
	d.mu.Lock()
	u = d.acct(name)
	u.resets = withLeft(u.resets, credit.ID, n)
	u.spentAt = answered          // an ask in flight from before the reset must not undo it
	u.limitHitUntil = time.Time{} // the limit it hit is cleared
	d.mu.Unlock()
	d.reask(ctx, name)
	d.requestTick()
	return d.viewOf(name)
}

// reask asks name's usage now, after any ask in flight, whose numbers may be from before. It reads which
// account Claude Code holds after that wait, as a switch may have landed meanwhile.
func (d *Daemon) reask(ctx context.Context, name string) {
	d.awaitAsk(ctx, name)
	holder := d.holderNow()
	d.mu.Lock()
	d.acct(name).asked = time.Time{}
	d.mu.Unlock()
	if tok, ok := d.accessToken(name, holder); ok && !d.askLater(name, true) {
		d.fetch(d.tickCtx(ctx), name, tok)
	}
}
