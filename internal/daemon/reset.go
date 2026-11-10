package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

var (
	// ErrNoResetCredit: the account has no reset credit left, as ccswap last saw or as Anthropic says.
	ErrNoResetCredit = errors.New("no reset credit left")
	// ErrResetChanged: the caller showed another count of reset credits than the daemon has.
	ErrResetChanged = errors.New("reset credits changed")
	// ErrNotLimited: Anthropic spends the credit only at a limit.
	ErrNotLimited = errors.New("not at a limit")
	// ErrResetCooldown: Anthropic wants a pause between resets.
	ErrResetCooldown error = i18n.Err("daemon.reset_cooldown")
)

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

// spent is credits after one reset of the grant id, in a new slice: views read the old one unlocked.
func spent(credits []anthropic.ResetCredit, id string) []anthropic.ResetCredit {
	var out []anthropic.ResetCredit
	for _, c := range credits {
		if c.ID == id {
			c.Left--
		}
		if c.Left > 0 {
			out = append(out, c)
		}
	}
	return out
}

// resetRequestID names a claim of a grant at the resets it has left. Anthropic takes one claim per id, so
// a retry after a lost answer names the same one and spends nothing more; a grant's count only falls, so
// no later claim names one spent before.
func resetRequestID(email, credit string, left int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "ccswap:%s:%s:%d", strings.ToLower(email), credit, left))
	return hex.EncodeToString(h[:16])
}

// UseReset spends one of name's reset credits when the daemon has left of them, as the caller showed,
// then asks the account's usage again.
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
	d.mu.Lock()
	credits := liveCredits(d.acct(name).resets, d.o.Now())
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
	credit := credits[0].ID
	status, result, err := d.o.API.Reset(d.tickCtx(ctx), tok, oa.Org, credit, resetRequestID(a.Email, credit, credits[0].Left))
	if err != nil || result != "reset" && result != "already_used" {
		slog.Warn("reset: not confirmed", "account", name, "credit", credit, "status", status, "result", result, "err", err)
	}
	if _, ok := d.o.Store.State().Find(name); !ok { // removed meanwhile: nothing here to update
		return api.Account{}, ErrNotFound
	}
	// unsure is a claim that may have landed: the account's usage, asked again, shows whether it did.
	unsure := func(why *i18n.Error) (api.Account, error) {
		go d.reask(d.tickCtx(ctx), name, holder)
		return api.Account{}, resetFailed{why}
	}
	switch {
	case status == 0:
		return unsure(i18n.Err("daemon.reset_no_answer"))
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_login_refused", name)}
	case status == http.StatusTooManyRequests:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_rate_limited")}
	case status/100 == 5:
		return unsure(i18n.Err("daemon.reset_status", status))
	case status/100 != 2:
		return api.Account{}, resetFailed{i18n.Err("daemon.reset_status", status)}
	case result == "not_limited":
		return api.Account{}, i18n.Err("daemon.not_limited", ErrNotLimited, name)
	case result == "cooldown":
		return api.Account{}, ErrResetCooldown
	case result == "ineligible":
		d.mu.Lock()
		d.acct(name).resets = nil // until the next ask says otherwise
		d.mu.Unlock()
		return api.Account{}, i18n.Err("daemon.no_reset_credit", ErrNoResetCredit, name)
	case result != "reset" && result != "already_used": // unavailable, a word ccswap does not know, or none
		return unsure(i18n.Err("daemon.reset_unconfirmed"))
	}
	slog.Info("reset credit spent", "account", name, "credit", credit, "left", left-1)
	d.mu.Lock()
	u := d.acct(name)
	u.resets = spent(u.resets, credit)
	u.limitHitUntil = time.Time{} // the limit it hit is cleared
	d.mu.Unlock()
	d.reask(ctx, name, holder)
	d.requestTick()
	return d.viewOf(name)
}

// reask asks name's usage now, after any ask in flight, whose numbers may be from before.
func (d *Daemon) reask(ctx context.Context, name, holder string) {
	d.awaitAsk(ctx, name)
	d.mu.Lock()
	d.acct(name).asked = time.Time{}
	d.mu.Unlock()
	if tok, ok := d.accessToken(name, holder); ok && !d.askLater(name, true) {
		d.fetch(d.tickCtx(ctx), name, tok)
	}
}
