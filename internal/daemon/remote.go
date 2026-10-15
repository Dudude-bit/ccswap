package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

// remoteKeyAccount holds the other ccswap's key in the vault; the leading _ keeps it clear of account names.
const remoteKeyAccount = "_remote-key"

var ErrInvalidRemote = errors.New("invalid remote")

// remote is another ccswap that asks Anthropic for usage, so this one does not ask it again.
type remote struct {
	url, key string
	ok       bool
	err      string
	at       time.Time
}

var remoteHTTP = &http.Client{Timeout: 10 * time.Second}

func getRemote(ctx context.Context, base, key string) ([]api.Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/accounts", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := remoteHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("the server rejected the key")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	var accts []api.Account
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&accts); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	return accts, nil
}

// remoteUsage is the other ccswap's accounts by email; nil when remote usage is off or the ask failed.
func (d *Daemon) remoteUsage(ctx context.Context) map[string]api.Account {
	d.mu.Lock()
	r := d.rem
	d.mu.Unlock()
	if r.url == "" {
		return nil
	}
	accts, err := getRemote(ctx, r.url, r.key)
	d.mu.Lock()
	if d.rem.url == r.url {
		d.rem.ok, d.rem.err = err == nil, ""
		if err != nil {
			d.rem.err = err.Error()
		} else {
			d.rem.at = d.o.Now()
		}
	}
	d.mu.Unlock()
	if err != nil {
		d.logOnce("remote", slog.LevelWarn, "remote: no usage from the other ccswap, asking Anthropic", "url", r.url, "err", err)
		return nil
	}
	d.logDone("remote")
	m := make(map[string]api.Account, len(accts))
	for _, a := range accts {
		if a.Email != "" { // an account without one matches nothing here
			m[strings.ToLower(a.Email)] = a
		}
	}
	return m
}

// takeRemote copies name's usage from the other ccswap, unless the numbers here are newer; false
// when its numbers are older than maxAge, or missing, so the account is asked directly.
func (d *Daemon) takeRemote(name string, ra api.Account, maxAge time.Duration) bool {
	now := d.o.Now()
	if ra.NeedsLogin || ra.CheckedAt == nil {
		return false
	}
	at := *ra.CheckedAt
	if at.After(now) { // the other computer's clock runs ahead: its numbers are from now at the latest
		at = now
	}
	if now.Sub(at) > maxAge {
		return false
	}
	win := func(w *api.Window) *anthropic.Window {
		if w == nil {
			return nil
		}
		r := &anthropic.Window{Utilization: w.Utilization}
		if w.ResetsAt != nil {
			r.ResetsAt = *w.ResetsAt
		}
		return r
	}
	us := anthropic.Usage{FiveHour: win(ra.FiveHour), SevenDay: win(ra.SevenDay)}
	for _, m := range ra.Models {
		us.Models = append(us.Models, anthropic.ModelWindow{Name: m.Name, Window: *win(&api.Window{Utilization: m.Utilization, ResetsAt: m.ResetsAt})})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	u := d.acct(name)
	if u.checkedAt.After(at) { // asked here since: keep the newer numbers
		return true
	}
	u.fiveHour, u.sevenDay, u.models, u.checkedAt, u.err = us.FiveHour, us.SevenDay, us.Models, at, ""
	if ra.Error != nil {
		u.err = *ra.Error
	}
	u.sample()
	u.rawStatus, u.rawBody, u.rawAt, u.remote = http.StatusOK, anthropic.Body(us), now, true
	return true
}

func (d *Daemon) remoteStatus() *api.Remote {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rem.url == "" {
		return nil
	}
	r := &api.Remote{Url: d.rem.url, Ok: d.rem.ok, CheckedAt: timePtr(d.rem.at)}
	if d.rem.err != "" {
		r.Error = &d.rem.err
	}
	return r
}

// SetRemote checks that url answers to key before keeping either; an empty url turns remote usage off.
func (d *Daemon) SetRemote(ctx context.Context, r api.RemoteRequest) error {
	defer d.bus.publish()
	if r.Url == "" {
		d.cfgMu.Lock()
		defer d.cfgMu.Unlock()
		if err := config.SaveRemote(d.o.Home, ""); err != nil {
			return err
		}
		if err := d.o.KC.Delete(store.VaultService, remoteKeyAccount); err != nil && !errors.Is(err, keychain.ErrNotFound) {
			return err
		}
		d.mu.Lock()
		d.rem = remote{}
		d.mu.Unlock()
		d.requestTick()
		return nil
	}
	if err := config.CheckRemote(r.Url); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRemote, err)
	}
	key := ""
	if r.Key != nil {
		key = *r.Key
	}
	if key == "" {
		b, err := d.o.KC.Get(store.VaultService, remoteKeyAccount)
		if err != nil || len(b) == 0 {
			return fmt.Errorf("%w: no key given and none stored", ErrInvalidRemote)
		}
		key = string(b)
	}
	if _, err := getRemote(ctx, r.Url, key); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidRemote, r.Url, err)
	}
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if err := d.o.KC.Set(store.VaultService, remoteKeyAccount, []byte(key)); err != nil {
		return err
	}
	if err := config.SaveRemote(d.o.Home, r.Url); err != nil {
		return err
	}
	d.mu.Lock()
	d.rem = remote{url: r.Url, key: key}
	d.mu.Unlock()
	slog.Info("remote: usage now comes from another ccswap", "url", r.Url)
	d.requestTick()
	return nil
}
