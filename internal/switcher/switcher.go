// Package switcher swaps which account Claude Code is logged in as.
//
// ccswap owns only the claudeAiOauth key of Claude Code's entry. Vault invariant: nothing
// refreshes a vault copy of the chain in Claude Code's entry; such a copy is parked after a switch
// until the daemon sees the switch settled and drops it.
package switcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/cclock"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

var (
	ErrNearRefresh = errors.New("access token is about to be refreshed by Claude Code")
	// ErrTargetRefreshing: the target's vault copy has no access token yet; the daemon is refreshing it.
	ErrTargetRefreshing = errors.New("the target login is still being refreshed")
	ErrUnknownActive    = errors.New("the current Claude Code login is not a known account")
	ErrNeedsLogin       = errors.New("account needs a fresh login")
	ErrNoSuchAccount    = errors.New("no such account")
	ErrCannotIdentify   = errors.New("cannot identify the current Claude Code login")
	ErrHolderChanged    = errors.New("the Claude Code login changed since the decision")
	// ErrEntryChanged: Claude Code kept rewriting its login while the switch was ready to write.
	ErrEntryChanged = errors.New("the Claude Code login kept changing during the switch")
	// ErrEntryExpired: Claude Code has not run for a while, so its access token expired and the
	// profile cannot name the login. It is an ErrCannotIdentify.
	ErrEntryExpired = fmt.Errorf("%w: Claude Code's access token has expired", ErrCannotIdentify)
	// ErrFrozen: a migrate export holds the account, so no switch may reach it.
	ErrFrozen = errors.New("the account is being moved to another machine")
)

// fingerprintAccount is the vault item of the identification fingerprint; store.ValidName
// rejects "_", so no account collides with it.
const fingerprintAccount = "_fingerprint"

// fingerprint binds a refresh token's sha256 to the account, name and email, it was identified as.
type fingerprint struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Hash  string `json:"rt_sha256"`
}

const defaultGuard = 6 * time.Minute

type Profiler interface {
	Profile(ctx context.Context, accessToken string) (anthropic.Profile, error)
}

type Deps struct {
	KC                             keychain.Store
	Store                          *store.Store
	Profiles                       Profiler
	Service, KCAccount, ClaudeJSON string
	RefreshGuard                   time.Duration
	Now                            func() time.Time
	// ClaudeDir is Claude Code's config dir, whose locks guard the entry; "" takes no locks (tests).
	ClaudeDir string
}

// lockWait bounds the wait for Claude Code's locks; past it the switch gives up with entry_changed.
var lockWait = 10 * time.Second

// errRestart: the entry must be identified again, which may call the network and so never runs
// under Claude Code's locks.
var errRestart = errors.New("entry needs identifying again")

type Switcher struct {
	d  Deps
	mu sync.Mutex
	// the last pair ccswap put in the entry or positively identified there by profile; kept in the
	// keychain so a restart keeps an idle entry identified. Independent of State.Active.
	mem fingerprint
	// UnixNano of the last landed entry write, kept even when the state write after it failed.
	lastWrite atomic.Int64
	// frozen name -> deadline; its own mutex because switches hold s.mu across network calls
	// while status paths ask Frozen.
	fmu    sync.Mutex
	frozen map[string]time.Time
}

// Freeze makes switches to names fail with ErrFrozen until the deadline.
func (s *Switcher) Freeze(names []string, until time.Time) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if s.frozen == nil {
		s.frozen = map[string]time.Time{}
	}
	for _, n := range names {
		s.frozen[n] = until
	}
}

// Thaw lifts the freeze on names.
func (s *Switcher) Thaw(names []string) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for _, n := range names {
		delete(s.frozen, n)
	}
}

// Frozen reports whether name is frozen now; an expired entry is dropped.
func (s *Switcher) Frozen(name string) bool {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	until, ok := s.frozen[name]
	if ok && !s.d.Now().Before(until) {
		delete(s.frozen, name)
		return false
	}
	return ok
}

// LastWrite is when ccswap last wrote Claude Code's entry (zero: not since start).
func (s *Switcher) LastWrite() time.Time {
	if n := s.lastWrite.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// parseEntry reads an entry; login=false when it holds no claudeAiOauth (not logged in).
func parseEntry(b []byte) (c claudecode.Credentials, login bool, err error) {
	c, err = claudecode.ParseCredentials(b)
	if errors.Is(err, claudecode.ErrNoOAuth) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("claude code entry: %w", err)
	}
	return c, true, nil
}

func New(d Deps) *Switcher {
	if d.RefreshGuard == 0 {
		d.RefreshGuard = defaultGuard
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Switcher{d: d}
	if b, err := d.KC.Get(store.VaultService, fingerprintAccount); err == nil {
		_ = json.Unmarshal(b, &s.mem)
	}
	return s
}

// Locked runs fn under the switcher's mutex; every vault refresh must go through it.
func (s *Switcher) Locked(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

func hash(rt string) string { h := sha256.Sum256([]byte(rt)); return hex.EncodeToString(h[:]) }

func (s *Switcher) remember(name, email string, c claudecode.Credentials) {
	var f fingerprint
	if c.OAuth.RefreshToken != "" {
		f = fingerprint{name, email, hash(c.OAuth.RefreshToken)}
	}
	if f != s.mem {
		s.mem = f
		s.persist()
	}
}

// ForgetLocked drops the fingerprint when it names name (the account is being removed). The caller
// holds Locked.
func (s *Switcher) ForgetLocked(name string) {
	if s.mem.Name == name {
		s.mem = fingerprint{}
		s.persist()
	}
}

// persist saves the fingerprint; a failure only costs a profile call after a restart.
func (s *Switcher) persist() {
	var err error
	if s.mem.Name == "" {
		if err = s.d.KC.Delete(store.VaultService, fingerprintAccount); errors.Is(err, keychain.ErrNotFound) {
			err = nil
		}
	} else {
		b, _ := json.Marshal(s.mem)
		err = s.d.KC.Set(store.VaultService, fingerprintAccount, b)
	}
	if err != nil {
		slog.Warn("identification fingerprint not saved", "err", err)
	}
}

func expired(c claudecode.Credentials, now time.Time) bool {
	return c.OAuth.ExpiresAt != 0 && !c.Expiry().After(now)
}

// readEntry returns the raw entry; (nil, nil) when it does not exist.
func (s *Switcher) readEntry() ([]byte, error) {
	b, err := s.d.KC.Get(s.d.Service, s.d.KCAccount)
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// ident names the account that owns c. known=false: a login ccswap does not have. A profile match
// is remembered by fingerprint, so an idle entry (expired access token, chain not rotated) stays
// identified without the profile while the account keeps that email; force fallbacks never get here.
func (s *Switcher) ident(ctx context.Context, c claudecode.Credentials) (string, bool, error) {
	st := s.d.Store.State()
	if m := s.mem; m.Name != "" && c.OAuth.RefreshToken != "" && hash(c.OAuth.RefreshToken) == m.Hash {
		if a, ok := st.Find(m.Name); ok && a.Email != "" && strings.EqualFold(a.Email, m.Email) {
			return m.Name, true, nil
		}
	}
	if expired(c, s.d.Now()) {
		return "", false, ErrEntryExpired
	}
	p, err := s.d.Profiles.Profile(ctx, c.OAuth.AccessToken)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrCannotIdentify, err)
	}
	if p.Email == "" {
		return "", false, fmt.Errorf("%w: profile has no email", ErrCannotIdentify)
	}
	if a, ok := st.FindEmail(p.Email); ok {
		s.remember(a.Name, a.Email, c)
		return a.Name, true, nil
	}
	return "", false, nil
}

func (s *Switcher) Identify(ctx context.Context) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.readEntry()
	if err != nil {
		return "", false, err
	}
	if b == nil {
		return "", false, keychain.ErrNotFound
	}
	c, login, err := parseEntry(b)
	if err != nil {
		return "", false, err
	}
	if !login {
		return "", false, keychain.ErrNotFound
	}
	return s.ident(ctx, c)
}

// holder names the owner of the entry. A positive identification is never overridden by
// force; force only covers a failed profile call, and only when trusting State.Active
// cannot overwrite a different login in the vault.
func (s *Switcher) holder(ctx context.Context, c claudecode.Credentials, force bool) (string, error) {
	name, known, err := s.ident(ctx, c)
	if err == nil {
		if !known {
			return "", ErrUnknownActive
		}
		return name, nil
	}
	if !force || !errors.Is(err, ErrCannotIdentify) {
		return "", err
	}
	active := s.d.Store.State().Active
	if active == "" {
		return "", ErrUnknownActive
	}
	v, verr := s.d.Store.Creds(active)
	switch {
	case errors.Is(verr, keychain.ErrNotFound):
		return active, nil
	case verr == nil && v.OAuth.RefreshToken == c.OAuth.RefreshToken:
		return active, nil
	}
	return "", ErrUnknownActive
}

func (s *Switcher) setActive(to string, markLost string, stamp bool) error {
	return s.d.Store.Update(func(x *store.State) error {
		x.Active = to
		if stamp {
			x.LastSwitchAt = s.d.Now()
		}
		for i := range x.Accounts {
			if markLost != "" && x.Accounts[i].Name == markLost {
				x.Accounts[i].NeedsLogin = true
			}
		}
		return nil
	})
}

// Switch puts to's login in Claude Code's entry. A switch that landed but left something undone returns a
// warning: an *i18n.Error, or several of them joined (errors.Join).
func (s *Switcher) Switch(ctx context.Context, to string, force bool) (warning error, err error) {
	return s.switchTo(ctx, "", to, force)
}

// SwitchFrom is Switch(to, false) that writes nothing (ErrHolderChanged) unless Claude Code's
// entry still holds from: an automatic decision made for from is void once someone else switched.
func (s *Switcher) SwitchFrom(ctx context.Context, from, to string) (warning error, err error) {
	return s.switchTo(ctx, from, to, false)
}

// switchTo restarts once when the entry changed before Claude Code's locks were taken; both
// tries share one lock wait budget.
func (s *Switcher) switchTo(ctx context.Context, expect, to string, force bool) (warning error, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := lockWait
	for range 2 {
		// the restart re-runs holder() with force too: its fallback trusts State.Active by design
		if warning, err = s.trySwitch(ctx, &wait, expect, to, force); !errors.Is(err, errRestart) {
			return warning, err
		}
	}
	return nil, ErrEntryChanged
}

// lockClaude takes Claude Code's credential locks, spending the wait budget; a busy lock is an
// ErrEntryChanged.
func (s *Switcher) lockClaude(ctx context.Context, wait *time.Duration) (release func(), err error) {
	if s.d.ClaudeDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(s.d.ClaudeDir, 0o700); err != nil { // Claude Code creates D before locking too
		return nil, err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	release, err = cclock.Claude(ctx, s.d.ClaudeDir)
	*wait -= time.Since(start)
	if errors.Is(err, cclock.ErrBusy) {
		return nil, fmt.Errorf("%w: %w", ErrEntryChanged, err)
	}
	return release, err
}

// trySwitch identifies the holder and checks outside Claude Code's locks; under them it only
// reads and writes the entry and the vault, never the network.
func (s *Switcher) trySwitch(ctx context.Context, wait *time.Duration, expect, to string, force bool) (warning error, err error) {
	st := s.d.Store.State()
	target, ok := st.Find(to)
	if !ok {
		return nil, ErrNoSuchAccount
	}
	if target.NeedsLogin {
		return nil, ErrNeedsLogin
	}
	if s.Frozen(to) {
		return nil, ErrFrozen
	}

	first, err := s.readEntry()
	if err != nil {
		return nil, err
	}
	var cur claudecode.Credentials
	var from string
	var login bool // the entry holds a claudeAiOauth
	if first != nil {
		if cur, login, err = parseEntry(first); err != nil {
			return nil, err
		}
	}
	if login {
		if from, err = s.holder(ctx, cur, force); err != nil {
			return nil, err
		}
	}
	if expect != "" && from != expect {
		return nil, ErrHolderChanged
	}
	if login {
		if from == to {
			// the entry already holds the target: no write, only bookkeeping; a vault copy of to
			// is the daemon's to drop once the entry has settled.
			if st.Active == to {
				return nil, nil // not verified (force may have trusted Active): no fingerprint
			}
			s.remember(to, target.Email, cur)
			return nil, s.setActive(to, "", true)
		}
		if left := cur.Expiry().Sub(s.d.Now()); !force && left <= 0 {
			return nil, ErrEntryExpired
		} else if !force && left < s.d.RefreshGuard {
			return nil, ErrNearRefresh
		}
	}
	prevName := s.mem.Name
	toCreds, err := s.d.Store.Creds(to)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, ErrNeedsLogin
		}
		return nil, fmt.Errorf("vault creds of %s: %w", to, err)
	}
	if toCreds.OAuth.AccessToken == "" {
		return nil, ErrTargetRefreshing // the daemon refreshes it on its next tick
	}

	// ponytail: hold time = ~5 keychain calls (macOS: a `security` exec each, typically 100–300 ms,
	// up to its 30 s timeout) under Claude Code's locks, whose writers give up after ~7.5 s. If that
	// bites: do the vault writes before the locks and touch the lock dirs every stale/2 like proper-lockfile.
	release, err := s.lockClaude(ctx, wait)
	if err != nil {
		return nil, err
	}
	defer release()
	if login {
		if err := s.d.Store.PutCreds(from, cur); err != nil {
			return nil, fmt.Errorf("save %s to vault: %w", from, err)
		}
	}
	// last look right before the write: its blob is the base of the write, so Claude Code's
	// other keys in the entry (MCP logins) keep their newest bytes.
	latest, err := s.lastLook(login, from, cur)
	if err != nil {
		return nil, err
	}
	b, err := claudecode.WithOAuth(latest, toCreds)
	if err != nil {
		return nil, fmt.Errorf("claude code entry: %w", err)
	}
	if serr := s.d.KC.Set(s.d.Service, s.d.KCAccount, b); serr != nil {
		// the write may have landed even though Set reported a failure.
		if got, rerr := s.readEntry(); rerr != nil || !bytes.Equal(got, b) {
			return nil, fmt.Errorf("write claude code entry: %w", serr)
		}
	}
	release() // Claude Code's writes give up after ~7.5 s: hold only around the entry
	s.lastWrite.Store(s.d.Now().UnixNano())
	s.remember(to, target.Email, toCreds)

	// vault[to] stays parked: Claude Code's unlocked read-modify-write may still put from's
	// login back over this write; the daemon drops the copy once to has settled in the entry.
	if len(target.OAuthAccount) == 0 {
		warning = i18n.Err("switcher.no_oauth_account", to)
	} else if err := claudecode.SwapOAuthAccount(s.d.ClaudeJSON, target.OAuthAccount); err != nil {
		warning = i18n.Err("switcher.oauth_not_updated", err)
	}
	// entry was empty: the previous holder's login is lost unless the vault still has it.
	lost := ""
	if !login {
		lost = st.Active
		if _, ok := st.Find(prevName); ok {
			lost = prevName
		}
		if _, cerr := s.d.Store.Creds(lost); lost == to || !errors.Is(cerr, keychain.ErrNotFound) {
			lost = ""
		}
	}
	if serr := s.setActive(to, lost, true); serr != nil {
		// the entry already holds to: the switch happened, only the bookkeeping failed.
		warning = errors.Join(warning, i18n.Err("switcher.state_not_saved", serr))
	}
	return warning, nil
}

// lastLook re-reads the entry until vault[from] holds the claudeAiOauth it carries, and returns
// that blob. Claude Code may have rewritten its MCP logins meanwhile; a missing entry is one
// without a login (Linux: no file yet). A login that vanished aborts the switch, and a new refresh
// token or a login that appeared needs identifying, so the switch restarts (errRestart) outside the locks.
func (s *Switcher) lastLook(hadLogin bool, from string, saved claudecode.Credentials) ([]byte, error) {
	for range 3 {
		latest, err := s.readEntry()
		if err != nil {
			return nil, err
		}
		var c2 claudecode.Credentials
		login := false
		if latest != nil {
			if c2, login, err = parseEntry(latest); err != nil {
				return nil, err
			}
		}
		if login != hadLogin {
			if login { // a login appeared mid-switch
				return nil, errRestart
			}
			return nil, ErrUnknownActive
		}
		if !login {
			return latest, nil
		}
		if sameOAuth(c2, saved) {
			return latest, nil
		}
		if c2.OAuth.RefreshToken != saved.OAuth.RefreshToken {
			// refreshed, or another login; vault[from] stays: it may be from's only copy.
			return nil, errRestart
		}
		if err := s.d.Store.PutCreds(from, c2); err != nil {
			return nil, fmt.Errorf("save %s to vault: %w", from, err)
		}
		saved = c2
	}
	return nil, ErrEntryChanged // Claude Code keeps rewriting its login: retry in a moment
}

func sameOAuth(a, b claudecode.Credentials) bool {
	x, err := a.Bytes()
	y, err2 := b.Bytes()
	return err == nil && err2 == nil && bytes.Equal(x, y)
}

// Adopt registers the login currently in Claude Code's entry as account name and marks it active.
// It writes metadata only: the active account has no vault item.
func (s *Switcher) Adopt(ctx context.Context, name string, priority int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !store.ValidName(name) {
		return fmt.Errorf("invalid account name %q", name)
	}
	b, err := s.readEntry()
	if err != nil {
		return err
	}
	if b == nil {
		return keychain.ErrNotFound
	}
	c, login, err := parseEntry(b)
	if err != nil {
		return err
	}
	if !login {
		return keychain.ErrNotFound
	}
	if expired(c, s.d.Now()) {
		return ErrEntryExpired
	}
	p, err := s.d.Profiles.Profile(ctx, c.OAuth.AccessToken)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCannotIdentify, err)
	}
	if p.Email == "" {
		return fmt.Errorf("%w: profile has no email", ErrCannotIdentify)
	}
	blob, err := claudecode.ReadOAuthAccount(s.d.ClaudeJSON)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var meta struct {
		Email string `json:"emailAddress"`
	}
	if json.Unmarshal(blob, &meta) != nil || !strings.EqualFold(meta.Email, p.Email) {
		blob = nil // belongs to some other login
	}
	err = s.d.Store.Update(func(x *store.State) error {
		if _, ok := x.Find(name); ok {
			return fmt.Errorf("%w: name %q", store.ErrDuplicate, name)
		}
		if a, ok := x.FindEmail(p.Email); ok {
			return fmt.Errorf("%w: email of %q", store.ErrDuplicate, a.Name)
		}
		x.Accounts = append(x.Accounts, store.Account{Name: name, Email: p.Email, Priority: priority,
			Enabled: true, OAuthAccount: blob, AuthIndex: store.AuthIndex(name)})
		x.Active = name
		return nil
	})
	if err == nil {
		s.remember(name, p.Email, c)
	}
	return err
}
