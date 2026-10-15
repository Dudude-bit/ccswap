package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

// migrateTTL is how long an export keeps its accounts frozen waiting for its commit.
const migrateTTL = 5 * time.Minute

// importBudget bounds the refreshes an import makes before it answers: past it none starts, so the CLI,
// which waits a minute, hears back; the next tick refreshes the rest. A var for tests.
var importBudget = 30 * time.Second

// The errors that name an account are wrapped in a text that names it.
var (
	// ErrMigrateActive: Claude Code uses the account, or holds its login, so it cannot move.
	ErrMigrateActive = errors.New("the account is in use in Claude Code")
	// ErrTokensChanged: a login changed since packing, so the bundle may hold a spent token.
	ErrTokensChanged = errors.New("a login changed since it was packed")
	// ErrBundleSpent: the bundle's login was used already, so it replaces no login here.
	ErrBundleSpent = errors.New("the bundle's login was used already")
	// ErrBundleUnchecked: the bundle's login was not refreshed (Anthropic failed, or no time was left), so it
	// replaces no login here yet.
	ErrBundleUnchecked = errors.New("the bundle's login could not be checked")
)

var (
	// ErrExportGone: no export has the token; a restart forgets every export.
	ErrExportGone error = i18n.Err("daemon.export_gone")
	// ErrExportDone: the export is committed, so it cannot be cancelled.
	ErrExportDone error = i18n.Err("daemon.export_done")
)

// noNamesError is an export of no account: a text of its own (an i18n.Err text would have to print
// the sentinel to wrap it) that still answers invalid_name.
type noNamesError struct{}

func (noNamesError) Error() string { return i18n.T("daemon.migrate_no_names") }
func (noNamesError) Unwrap() error { return ErrInvalidName }

// migration is an export waiting for its commit; its accounts stay frozen until then.
type migration struct {
	names []string
	rts   map[string]string // sha256 hex of each vault refresh token at packing
	until time.Time
	done  bool
}

func rtHash(rt string) string {
	h := sha256.Sum256([]byte(rt))
	return hex.EncodeToString(h[:])
}

// MigrateExport packs names into a bundle and freezes them until MigrateCommit removes them, or
// MigrateCancel, a restart or migrateTTL lifts the freeze. All under the switcher lock, so no
// refresh or switch lands between packing and freezing; no network call inside.
func (d *Daemon) MigrateExport(_ context.Context, names []string) (api.MigrateExport, error) {
	defer d.bus.publish() // a frozen account is no longer next
	var uniq []string
	for _, n := range names {
		if !slices.Contains(uniq, n) {
			uniq = append(uniq, n)
		}
	}
	if len(uniq) == 0 {
		return api.MigrateExport{}, noNamesError{}
	}
	var out api.MigrateExport
	err := d.o.Switcher.Locked(func() error {
		e, err := d.entry()
		if err != nil {
			return fmt.Errorf("read claude code entry: %w", err)
		}
		accts := make([]migrate.Account, 0, len(uniq))
		rts := map[string]string{}
		for _, n := range uniq {
			a, c, err := d.packableLocked(n, e)
			if err != nil {
				return err
			}
			b, err := c.Bytes()
			if err != nil {
				return err
			}
			accts = append(accts, migrate.Account{Name: a.Name, Email: a.Email, Priority: a.Priority, Enabled: a.Enabled,
				OAuthAccount: a.OAuthAccount, Credentials: b})
			rts[n] = rtHash(c.OAuth.RefreshToken)
		}
		bundle, code, err := migrate.Seal(accts)
		if err != nil {
			return err
		}
		t := make([]byte, 16)
		if _, err := rand.Read(t); err != nil {
			return err
		}
		token, now := hex.EncodeToString(t), d.o.Now()
		until := now.Add(migrateTTL)
		d.o.Switcher.Freeze(uniq, until)
		d.mu.Lock()
		// an expired export answers as an unknown one does, committed or not: state.json tells which
		maps.DeleteFunc(d.migrations, func(_ string, m *migration) bool { return !now.Before(m.until) })
		d.migrations[token] = &migration{names: uniq, rts: rts, until: until}
		d.mu.Unlock()
		out = api.MigrateExport{Token: token, Bundle: string(bundle), Code: code, ExpiresAt: until}
		return nil
	})
	if err != nil {
		return api.MigrateExport{}, err
	}
	slog.Info("migrate: accounts packed", "accounts", uniq)
	return out, nil
}

// packableLocked is name with its vault login when it may move: an account here, not frozen, not
// the one Claude Code uses or holds the login of, with a saved, live vault login. e is Claude Code's
// entry. The caller holds the switcher lock.
func (d *Daemon) packableLocked(name string, e *claudecode.Credentials) (store.Account, claudecode.Credentials, error) {
	var none claudecode.Credentials
	if !store.ValidName(name) {
		return store.Account{}, none, i18n.Err("daemon.migrate_invalid_name", ErrInvalidName, name)
	}
	st := d.o.Store.State()
	a, ok := st.Find(name)
	if !ok {
		return a, none, i18n.Err("daemon.migrate_not_found", ErrNotFound, name)
	}
	if d.o.Switcher.Frozen(name) {
		return a, none, i18n.Err("daemon.migrating", switcher.ErrFrozen, name)
	}
	if st.Active == name || d.holderNow() == name {
		return a, none, i18n.Err("daemon.migrate_active", ErrMigrateActive, name)
	}
	if err := d.flushLocked(name); err != nil { // the rotated pair is still unsaved
		return a, none, i18n.Err("daemon.migrate_refreshing", switcher.ErrTargetRefreshing, name)
	}
	if a.NeedsLogin {
		return a, none, i18n.Err("daemon.migrate_needs_login", switcher.ErrNeedsLogin, name)
	}
	c, err := d.o.Store.Creds(name)
	if errors.Is(err, keychain.ErrNotFound) {
		return a, none, i18n.Err("daemon.migrate_needs_login", switcher.ErrNeedsLogin, name)
	}
	if err != nil {
		return a, none, fmt.Errorf("read vault: %w", err)
	}
	if x := c.OAuth.RefreshTokenExpiresAt; x != 0 && !time.UnixMilli(x).After(d.o.Now()) { // a dead login
		return a, none, i18n.Err("daemon.migrate_needs_login", switcher.ErrNeedsLogin, name)
	}
	if e != nil && e.OAuth.RefreshToken == c.OAuth.RefreshToken {
		return a, none, i18n.Err("daemon.migrate_active", ErrMigrateActive, name)
	}
	return a, c, nil
}

// MigrateCommit removes an export's accounts here once they are checked unchanged since packing;
// any failure cancels the export instead, so the accounts stay. A repeat commit is a no-op.
func (d *Daemon) MigrateCommit(ctx context.Context, token string) error {
	defer d.bus.publish()
	var moved []string
	err := d.o.Switcher.Locked(func() error {
		d.mu.Lock()
		m := d.migrations[token]
		done := m != nil && m.done
		d.mu.Unlock()
		switch {
		case m == nil:
			return ErrExportGone
		case done:
			return nil
		}
		err := ErrExportGone
		if d.o.Now().Before(m.until) {
			err = d.recheckLocked(m)
		}
		// the recheck reads the keychain, which can stall: no commit past the deadline the CLI names, nor
		// once the daemon is stopping, as its socket goes and a CLI that finds it gone trusts state.json.
		// Left over: a state write already under way when the stop begins, a few milliseconds.
		if err == nil && (!d.o.Now().Before(m.until) || d.tickCtx(ctx).Err() != nil) {
			err = ErrExportGone
		}
		if err == nil {
			// the commit point: once state.json drops them, the accounts are gone from here
			err = d.o.Store.Update(func(x *store.State) error {
				x.Accounts = slices.DeleteFunc(x.Accounts, func(a store.Account) bool { return slices.Contains(m.names, a.Name) })
				return nil
			})
		}
		if err != nil {
			d.dropLocked(token, m)
			slog.Info("migrate: commit refused, the accounts stay", "accounts", m.names, "err", err)
			return err
		}
		for _, n := range m.names {
			if err := d.o.Store.DeleteCreds(n); err != nil && !errors.Is(err, keychain.ErrNotFound) {
				slog.Warn("migrate: vault item of a moved account not deleted", "account", n, "err", err)
			}
		}
		d.forgetLocked(m.names...)
		d.mu.Lock()
		m.done = true
		d.mu.Unlock()
		d.o.Switcher.Thaw(m.names)
		moved = m.names
		return nil
	})
	if moved != nil {
		slog.Info("migrate: accounts moved out", "accounts", moved)
	}
	return err
}

// recheckLocked confirms an export's accounts are as packed: still here, not in Claude Code's entry,
// with the refresh token packed. Active and holder go first: a login Claude Code took over may have
// lost its vault copy meanwhile. The caller holds the switcher lock.
func (d *Daemon) recheckLocked(m *migration) error {
	st, holder := d.o.Store.State(), d.holderNow()
	e, err := d.entry()
	if err != nil {
		return fmt.Errorf("read claude code entry: %w", err)
	}
	for _, n := range m.names {
		if _, ok := st.Find(n); !ok {
			return i18n.Err("daemon.tokens_changed", ErrTokensChanged, n)
		}
		if st.Active == n || holder == n {
			return i18n.Err("daemon.migrate_active", ErrMigrateActive, n)
		}
		v, err := d.o.Store.Creds(n)
		switch {
		case errors.Is(err, keychain.ErrNotFound):
			return i18n.Err("daemon.tokens_changed", ErrTokensChanged, n)
		case err != nil:
			return fmt.Errorf("read vault: %w", err)
		case e != nil && e.OAuth.RefreshToken == v.OAuth.RefreshToken:
			return i18n.Err("daemon.migrate_active", ErrMigrateActive, n)
		case rtHash(v.OAuth.RefreshToken) != m.rts[n]:
			return i18n.Err("daemon.tokens_changed", ErrTokensChanged, n)
		}
	}
	return nil
}

// MigrateCancel drops an export that is not committed, expired or not; its accounts stay and thaw. An
// unknown token is ErrExportGone, not a success: a restart forgets a commit that landed just before it.
func (d *Daemon) MigrateCancel(_ context.Context, token string) error {
	defer d.bus.publish()
	return d.o.Switcher.Locked(func() error {
		d.mu.Lock()
		m := d.migrations[token]
		done := m != nil && m.done
		d.mu.Unlock()
		switch {
		case m == nil:
			return ErrExportGone
		case done:
			return ErrExportDone
		}
		d.dropLocked(token, m)
		slog.Info("migrate: export cancelled, the accounts stay", "accounts", m.names)
		return nil
	})
}

// dropLocked forgets an export and thaws its accounts. The caller holds the switcher lock.
func (d *Daemon) dropLocked(token string, m *migration) {
	d.o.Switcher.Thaw(m.names)
	d.mu.Lock()
	delete(d.migrations, token)
	d.mu.Unlock()
}

// MigrateImport adds the accounts of a bundle, all or none, then refreshes each at once: the bundle's
// refresh token is spent here, so a second import of the same bundle gets a dead login. A refresh that
// fails is forced again on each tick until one spends it. An account that replaces one here is refreshed
// first, before anything is written, so a spent bundle never replaces a working login.
func (d *Daemon) MigrateImport(ctx context.Context, r api.MigrateImportRequest) (api.MigrateImport, error) {
	defer d.bus.publish()
	start := time.Now()
	late := func() bool { return time.Since(start) >= importBudget }
	accts, err := migrate.Open([]byte(r.Bundle), r.Code)
	if err != nil {
		return api.MigrateImport{}, err
	}
	creds, err := bundleCreds(accts)
	if err != nil {
		return api.MigrateImport{}, err
	}
	var added []string
	err = d.o.Switcher.Locked(func() error {
		repl, err := d.replacedLocked(accts, r.Replace)
		if err != nil {
			return err
		}
		logins, spent, err := d.spendLocked(ctx, accts, creds, repl, late)
		if err == nil {
			err = d.addLocked(accts, logins, repl)
		}
		if err != nil {
			for _, i := range spent {
				d.keepLocked(repl[i], logins[i], creds[i].OAuth.RefreshToken)
			}
			return err
		}
		d.mu.Lock()
		for i, a := range accts {
			delete(d.kept, rtHash(creds[i].OAuth.RefreshToken))
			if repl[i].name == "" {
				added = append(added, a.Name)
			}
		}
		d.mu.Unlock()
		return nil
	})
	if err != nil {
		return api.MigrateImport{}, err
	}
	names := make([]string, len(accts))
	for i, a := range accts {
		names[i] = a.Name
	}
	slog.Info("migrate: accounts moved in", "accounts", names)
	holder, rctx := d.holderNow(), d.tickCtx(ctx)
	for _, n := range added {
		if late() { // the next tick refreshes the rest: they stay unspent
			break
		}
		d.refreshOne(rctx, n, holder, true) // a dead login is marked NeedsLogin and notified
	}
	d.requestTick()
	st := d.o.Store.State()
	out := api.MigrateImport{Accounts: make([]api.Account, 0, len(names))}
	for _, n := range names {
		if a, ok := st.Find(n); ok {
			out.Accounts = append(out.Accounts, d.view(st, a))
		}
	}
	return out, nil
}

// bundleCreds checks a decrypted bundle before anything is written and returns each account's login:
// valid names, emails, logins with a refresh token, no name or email twice. A bundle export could not
// have made is as damaged as one that does not decrypt.
func bundleCreds(accts []migrate.Account) ([]claudecode.Credentials, error) {
	if len(accts) == 0 {
		return nil, migrate.ErrBadCode
	}
	out := make([]claudecode.Credentials, len(accts))
	for i, a := range accts {
		c, err := claudecode.ParseCredentials(a.Credentials)
		twice := slices.ContainsFunc(accts[:i], func(b migrate.Account) bool {
			return b.Name == a.Name || strings.EqualFold(b.Email, a.Email)
		})
		if err != nil || c.OAuth.RefreshToken == "" || a.Email == "" || !store.ValidName(a.Name) || twice {
			return nil, migrate.ErrBadCode
		}
		out[i] = c
	}
	return out, nil
}

// replacement is the account here that a bundle account replaces and the refresh token of its login ("" when
// it has none); the zero value for a bundle account that is added.
type replacement struct{ name, rt string }

// replacedLocked checks every bundle account against the accounts here and returns, by bundle account, the
// account it replaces: one with the same email, only with replace, and never one Claude Code uses or holds
// the login of, nor one an export holds. A name here with another email is never replaced. The caller
// holds the switcher lock.
func (d *Daemon) replacedLocked(accts []migrate.Account, replace bool) ([]replacement, error) {
	st, holder := d.o.Store.State(), d.holderNow()
	entry := sync.OnceValues(d.entry) // read only when something is to be replaced
	repl := make([]replacement, len(accts))
	for i, a := range accts {
		if x, ok := st.Find(a.Name); ok && !strings.EqualFold(x.Email, a.Email) {
			return nil, i18n.Err("daemon.migrate_name_taken", store.ErrDuplicate, x.Name, x.Email)
		}
		x, ok := st.FindEmail(a.Email)
		if !ok {
			continue
		}
		if !replace {
			return nil, i18n.Err("daemon.migrate_email_here", store.ErrDuplicate, a.Email, x.Name)
		}
		if st.Active == x.Name || holder == x.Name {
			return nil, i18n.Err("daemon.migrate_active", ErrMigrateActive, x.Name)
		}
		e, err := entry()
		if err != nil {
			return nil, fmt.Errorf("read claude code entry: %w", err)
		}
		v, err := d.o.Store.Creds(x.Name)
		switch {
		case err != nil && !errors.Is(err, keychain.ErrNotFound):
			return nil, fmt.Errorf("read vault: %w", err)
		case err == nil && e != nil && e.OAuth.RefreshToken == v.OAuth.RefreshToken:
			return nil, i18n.Err("daemon.migrate_active", ErrMigrateActive, x.Name)
		}
		if d.o.Switcher.Frozen(x.Name) {
			return nil, i18n.Err("daemon.migrating", switcher.ErrFrozen, x.Name)
		}
		repl[i] = replacement{name: x.Name, rt: v.OAuth.RefreshToken}
	}
	return repl, nil
}

// spendLocked refreshes the bundle login of each account that replaces one here, so that only a working
// login replaces one: logins is creds with the rotated pairs, spent the bundle accounts whose login it
// refreshed. A login a failed import spent is taken from the account that keeps its pair (keepLocked)
// instead. Any refusal leaves the pairs of spent for the caller to keep. The caller holds the switcher lock.
func (d *Daemon) spendLocked(ctx context.Context, accts []migrate.Account, creds []claudecode.Credentials, repl []replacement,
	late func() bool) (logins []claudecode.Credentials, spent []int, err error) {
	logins = slices.Clone(creds)
	for i, x := range repl {
		if x.name == "" {
			continue
		}
		d.mu.Lock()
		keeper := d.kept[rtHash(creds[i].OAuth.RefreshToken)]
		d.mu.Unlock()
		if keeper == x.name {
			if err := d.flushLocked(x.name); err != nil {
				return logins, spent, i18n.Err("daemon.migrate_refreshing", switcher.ErrTargetRefreshing, x.name)
			}
			v, err := d.o.Store.Creds(x.name)
			switch {
			case errors.Is(err, keychain.ErrNotFound):
				return logins, spent, i18n.Err("daemon.migrate_spent", ErrBundleSpent, accts[i].Name, x.name)
			case err != nil:
				return logins, spent, fmt.Errorf("read vault: %w", err)
			}
			logins[i] = v
			continue
		}
		// a refresh the exit cuts short loses its rotated pair, as refreshOne knows
		if late() || d.tickCtx(ctx).Err() != nil {
			slog.Info("migrate: no time left to refresh the bundle's login, the import is refused", "account", accts[i].Name)
			return logins, spent, i18n.Err("daemon.migrate_unchecked_late", ErrBundleUnchecked, accts[i].Name)
		}
		c, ok, err := d.rotate(ctx, creds[i])
		switch {
		case errors.Is(err, anthropic.ErrInvalidGrant):
			return logins, spent, i18n.Err("daemon.migrate_spent", ErrBundleSpent, accts[i].Name, x.name)
		case !ok:
			slog.Warn("migrate: the bundle's login not refreshed, the import is refused", "account", accts[i].Name, "err", err)
			return logins, spent, i18n.Err("daemon.migrate_unchecked", ErrBundleUnchecked, accts[i].Name)
		}
		logins[i], spent = c, append(spent, i)
	}
	return logins, spent, nil
}

// keepLocked keeps the rotated pair of a bundle login a failed import spent, whose bundle copy is dead now:
// the account it was to replace, of the same email, takes it over its own login, as an unsaved refresh
// pair, saved at once or on a later tick; and a retry of the import takes the login from there. The
// caller holds the switcher lock.
func (d *Daemon) keepLocked(x replacement, c claudecode.Credentials, bundleRT string) {
	d.mu.Lock()
	d.pending[x.name] = pendingPair{c, x.rt}
	d.kept[rtHash(bundleRT)] = x.name
	d.mu.Unlock()
	if err := d.flushLocked(x.name); err != nil {
		slog.Error("migrate: the refreshed login of a failed import kept in memory, retrying next tick", "account", x.name, "err", err)
		return
	}
	slog.Info("migrate: the import failed, the account keeps the bundle's refreshed login", "account", x.name)
}

// addLocked saves the bundle's logins, then in one state write drops the replaced accounts and adds the
// bundle's. On any failure every vault item it touched gets its previous bytes back, or goes if it had
// none, so nothing is added. The caller holds the switcher lock.
func (d *Daemon) addLocked(accts []migrate.Account, logins []claudecode.Credentials, repl []replacement) (err error) {
	var touched []string
	prev := map[string][]byte{} // the previous bytes of the touched items that had any
	defer func() {
		if err == nil {
			return
		}
		for _, n := range touched {
			var rerr error
			if b, ok := prev[n]; ok {
				rerr = d.o.KC.Set(store.VaultService, n, b)
			} else if rerr = d.o.Store.DeleteCreds(n); errors.Is(rerr, keychain.ErrNotFound) {
				rerr = nil
			}
			if rerr != nil {
				slog.Error("migrate: vault item not restored after a failed import", "account", n, "err", rerr)
			}
		}
	}()
	for i, a := range accts {
		b, err := d.o.KC.Get(store.VaultService, a.Name)
		switch {
		case err == nil:
			prev[a.Name] = b
		case !errors.Is(err, keychain.ErrNotFound):
			return fmt.Errorf("read vault: %w", err)
		}
		touched = append(touched, a.Name)
		if err := d.o.Store.PutCreds(a.Name, logins[i]); err != nil {
			return err
		}
	}
	var replaced []string
	for _, x := range repl {
		if x.name != "" {
			replaced = append(replaced, x.name)
		}
	}
	err = d.o.Store.Update(func(x *store.State) error {
		x.Accounts = slices.DeleteFunc(x.Accounts, func(a store.Account) bool { return slices.Contains(replaced, a.Name) })
		for _, a := range accts {
			x.Accounts = append(x.Accounts, store.Account{Name: a.Name, Email: a.Email, Priority: a.Priority,
				Enabled: a.Enabled, OAuthAccount: a.OAuthAccount, AuthIndex: store.AuthIndex(a.Name)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, n := range replaced {
		if !slices.Contains(touched, n) { // renamed: its old login goes
			if err := d.o.Store.DeleteCreds(n); err != nil && !errors.Is(err, keychain.ErrNotFound) {
				slog.Warn("migrate: vault item of a replaced account not deleted", "account", n, "err", err)
			}
		}
	}
	d.forgetLocked(replaced...)
	d.mu.Lock()
	for i, a := range accts {
		if repl[i].name == "" { // a replacing login was spent already
			d.unspent[a.Name] = true // until a refresh spends the bundle's refresh token
		}
	}
	d.mu.Unlock()
	return nil
}
