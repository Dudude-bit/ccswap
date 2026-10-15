// Package tui is the interactive ccswap screen; it talks to the daemon only through Client.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"syscall"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// Client is the slice of the daemon API the screen needs.
type Client interface {
	Status(ctx context.Context) (api.Status, error)
	Accounts(ctx context.Context) ([]api.Account, error)
	Activate(ctx context.Context, name string, force bool) error
	SetAuto(ctx context.Context, on bool) error
	Refresh(ctx context.Context) error
	SetPriority(ctx context.Context, name string, priority int) error
	SetEnabled(ctx context.Context, name string, on bool) error
	SetStrategy(ctx context.Context, s api.Strategy) error
}

var (
	// ErrDown means the daemon socket is missing or refuses connections.
	ErrDown error = i18n.Err("tui.daemon_down")
	// ErrNearRefresh means the active token is about to refresh; Activate may be retried with force.
	ErrNearRefresh error = i18n.Err("tui.token_refreshing")
	// ErrSlow means the call timed out; a mutating one may still be running in the daemon.
	ErrSlow    error = i18n.Err("tui.daemon_slow")
	errBadResp error = i18n.Err("tui.bad_reply")
)

// NameRule describes a valid account name.
func NameRule() string {
	return i18n.T("tui.invalid_name")
}

// Hint explains a daemon error code in the current language, or "" when the code has none. Hints never
// offer --force: only `ccswap use` has it, and it adds that itself.
func Hint(code, name string) string {
	who := name
	if who == "" {
		who = i18n.T("tui.name_placeholder")
	}
	switch code {
	case "unknown_active":
		return i18n.T("tui.unknown_active")
	case "cannot_identify":
		return i18n.T("tui.no_login_account")
	case "entry_expired":
		return i18n.T("tui.claude_idle")
	case "entry_changed":
		return i18n.T("tui.claude_refreshing")
	case "target_refreshing":
		return i18n.T("tui.login_refreshing", who)
	case "near_refresh":
		return i18n.T("tui.active_token_refreshing")
	case "needs_login":
		return i18n.T("tui.must_login_again", who)
	case "not_found":
		return i18n.T("tui.no_account", who)
	case "migrating":
		return i18n.T("tui.account_migrating", who)
	case "no_entry":
		return i18n.T("tui.claude_not_logged_in")
	case "duplicate":
		return i18n.T("tui.account_exists")
	case "remove_active":
		return i18n.T("tui.remove_active")
	case "active_account":
		return i18n.T("tui.relogin_active")
	case "invalid_name":
		return NameRule()
	case "login_incomplete":
		return i18n.T("tui.login_incomplete", who)
	case "invalid_config_dir":
		return i18n.T("tui.invalid_config_dir")
	case "bad_request":
		return i18n.T("tui.daemon_bad_request")
	case "internal":
		return i18n.T("tui.daemon_internal_error")
	}
	return ""
}

// Describe is Hint plus the daemon's reason where the hint alone can't say what went wrong.
func Describe(e api.Error, name string) string {
	h := Hint(e.Code, name)
	if h != "" && e.Code == "cannot_identify" && e.Message != "" {
		h += i18n.T("tui.reason", e.Message)
	}
	return h
}

// Timeout reports whether err is a call that ran out of time.
func Timeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

type adapter struct{ c *api.ClientWithResponses }

// NewClient wraps the generated client.
func NewClient(c *api.ClientWithResponses) Client { return adapter{c} }

func down(err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED):
		return ErrDown
	case Timeout(err):
		return ErrSlow
	}
	return err
}

func fail(code int, body []byte, name string, ok int) error {
	if code == ok {
		return nil
	}
	var e api.Error
	_ = json.Unmarshal(body, &e)
	if e.Code == "near_refresh" {
		return ErrNearRefresh
	}
	if h := Describe(e, name); h != "" {
		return errors.New(h)
	}
	if e.Message == "" {
		return errors.New(i18n.T("tui.daemon_replied", code))
	}
	return errors.New(e.Message)
}

func (a adapter) Status(ctx context.Context) (api.Status, error) {
	r, err := a.c.GetStatusWithResponse(ctx)
	if err != nil {
		return api.Status{}, down(err)
	}
	if err := fail(r.StatusCode(), r.Body, "", 200); err != nil {
		return api.Status{}, err
	}
	if r.JSON200 == nil {
		return api.Status{}, errBadResp
	}
	return *r.JSON200, nil
}

func (a adapter) Accounts(ctx context.Context) ([]api.Account, error) {
	r, err := a.c.ListAccountsWithResponse(ctx)
	if err != nil {
		return nil, down(err)
	}
	if err := fail(r.StatusCode(), r.Body, "", 200); err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, errBadResp
	}
	return *r.JSON200, nil
}

func (a adapter) Activate(ctx context.Context, name string, force bool) error {
	r, err := a.c.ActivateAccountWithResponse(ctx, name, &api.ActivateAccountParams{Force: &force})
	if err != nil {
		return down(err)
	}
	return fail(r.StatusCode(), r.Body, name, 200)
}

func (a adapter) SetAuto(ctx context.Context, on bool) error {
	r, err := a.c.SetAutoWithResponse(ctx, api.AutoRequest{Enabled: on})
	if err != nil {
		return down(err)
	}
	return fail(r.StatusCode(), r.Body, "", 204)
}

func (a adapter) Refresh(ctx context.Context) error {
	r, err := a.c.RefreshWithResponse(ctx)
	if err != nil {
		return down(err)
	}
	return fail(r.StatusCode(), r.Body, "", 204)
}

func (a adapter) patch(ctx context.Context, name string, p api.AccountPatch) error {
	r, err := a.c.PatchAccountWithResponse(ctx, name, p)
	if err != nil {
		return down(err)
	}
	return fail(r.StatusCode(), r.Body, name, 200)
}

func (a adapter) SetPriority(ctx context.Context, name string, priority int) error {
	return a.patch(ctx, name, api.AccountPatch{Priority: &priority})
}

func (a adapter) SetEnabled(ctx context.Context, name string, on bool) error {
	return a.patch(ctx, name, api.AccountPatch{Enabled: &on})
}

func (a adapter) SetStrategy(ctx context.Context, s api.Strategy) error {
	r, err := a.c.SetStrategyWithResponse(ctx, api.StrategyRequest{Strategy: s})
	if err != nil {
		return down(err)
	}
	return fail(r.StatusCode(), r.Body, "", 204)
}
