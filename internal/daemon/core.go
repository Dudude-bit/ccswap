package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/notify"
	"github.com/Dudude-bit/ccswap/internal/policy"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/t3"
)

const (
	refreshBefore = 10 * time.Minute // refresh vault tokens this close to expiry
	limitBackoff  = time.Hour        // an account that hit its limit counts as exhausted this long
	signalGrace   = time.Minute      // limit signals this soon after a switch belong to the previous account
	nearLimit     = 80               // utilization from which a window may be the one a limit message is about
	parkedFor     = 2 * time.Minute  // Claude Code caches its entry 30 s and may write it back over a switch
	maxBackoff    = 10 * time.Minute // slowest pace a 429 teaches; a longer check interval still wins
	calmAnswers   = 30               // answers in a row before an account slowed by a 429 is asked a bit faster
	refreshFloor  = time.Minute      // a refresh asks no account more often than this
	loginNotice   = 72 * time.Hour   // a login this close to its end is announced once and flagged on the page and in the TUI
	loginJitter   = time.Hour        // a refresh restates a login's end a few seconds off; a new login moves it by days
	usageWorkers  = 4
	title         = "ccswap"
)

// logMax is the size at which daemon.log is copied aside and truncated.
var logMax int64 = 10 << 20

// saveBackoff spaces the retries of a vault write that holds freshly rotated tokens.
var saveBackoff = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 900 * time.Millisecond}

// a vault write is retried only after a fast failure and within saveBudget: a hanging one
// (`security` waiting on an unlock prompt) must not hold the switcher lock.
var saveSlow, saveBudget = time.Second, 2 * time.Second

var (
	errNoToken = errors.New("no live access token for this account")
	errSave    = errors.New("save rotated tokens")
	errRaced   = errors.New("state changed meanwhile")
)

// skipped is why refreshOne asked Anthropic nothing: no fault of the login, so a tick does not report it.
type skipped struct{ error }

var (
	// ErrClaudeOwns: the login is the one Claude Code uses, which only Claude Code refreshes.
	ErrClaudeOwns error = i18n.Err("daemon.claude_owns")
	// ErrRefreshFailed: Anthropic did not refresh the login, for a reason other than a spent one.
	ErrRefreshFailed error = i18n.Err("daemon.refresh_failed")
)

type Options struct {
	Config   config.Config
	Store    *store.Store
	KC       keychain.Store
	API      *anthropic.Client
	Switcher *switcher.Switcher
	Notifier notify.Notifier
	Now      func() time.Time
	Home     string // the ccswap home (login dirs, daemon.log)
	Socket   string // unix socket of the daemon API
	Projects string // Claude Code's transcripts, whose writes tell when it last made a request
	LockPath string // one daemon per user, whatever CCSWAP_HOME says; default <config.DefaultHome>/daemon.lock
	Version  string
	Host     string
	Bin      string // the binary the daemon runs, which an update replaces
}

// acctUsage is the daemon's memory of one account's limits.
type acctUsage struct {
	fiveHour, sevenDay *anthropic.Window
	models             []anthropic.ModelWindow
	checkedAt          time.Time
	err                string
	refreshErr         string // err as a failed refresh set it, which the next refresh that works clears
	limitHitUntil      time.Time
	asked              time.Time     // the last ask for usage
	inFlight           chan struct{} // an ask is in flight, closed when it ends: nothing else asks meanwhile
	retryAt            time.Time     // after a 429 nothing asks before this, not even a refresh
	every              time.Duration // the slowest pace a 429 taught; usage is asked at this or the account's check interval
	calm               int           // answers in a row at that pace
	rawStatus          int
	rawBody            []byte
	rawAt              time.Time
	remote             bool             // the last data came from the other ccswap, which also answers T3 for it
	five, week         trend            // recent utilization of fiveHour and sevenDay, for leavesAt
	byModel            map[string]trend // and of each model window, by name
	token, login       time.Time        // when the login's access and refresh tokens run out, as last read; zero = unknown
	refreshed          time.Time        // the last refresh ccswap made of the login
	announced          time.Time        // the login end last announced as near
	loginStarted       time.Time        // the last start of a login from the page
}

// pendingPair is a rotated pair not yet in the vault and the consumed refresh token it replaces.
type pendingPair struct {
	creds    claudecode.Credentials
	replaces string
}

type Daemon struct {
	o           Options
	svc, kcAcct string // Claude Code's entry
	im          *Importer
	tick        sync.Mutex    // one tick at a time
	kick        chan struct{} // pending tick request; capacity 1 coalesces
	thr         atomic.Pointer[config.Thresholds]
	bus         broadcast  // tells event streams that status or accounts may have changed
	cfgMu       sync.Mutex // one config.toml rewrite at a time, so the file matches thr
	updating    atomic.Bool

	mu         sync.Mutex // guards everything below
	runCtx     context.Context
	cancel     context.CancelFunc // stops Run
	restart    bool               // an update replaced the binary: Run returns ErrRestart
	release    api.Release        // the newest release when it is newer than this build, as of the last ask
	usage      map[string]*acctUsage
	pending    map[string]pendingPair // rotated pairs whose save failed; saved before anything else
	unknown    bool                   // the entry holds a login ccswap does not know
	holder     string                 // the known account in the entry as last identified; "" = none
	last       time.Time              // end of the last usage poll
	reset      string                 // earliest reset of the last all-exhausted notice
	rem        remote
	activity   time.Time                // newest transcript write, as of the last tick
	activityOK bool                     // activity was read; a failed read never lets cache-aware climb
	logged     map[string]string        // per topic, the last line logged; repeats are not logged
	codes      map[string]time.Time     // one-time codes of `ccswap web`, by expiry
	migrations map[string]*migration    // migrate exports by token; in memory, so a restart cancels them
	unspent    map[string]bool          // imported logins whose bundle refresh token is not spent yet: refreshed by force
	kept       map[string]string        // sha256 of a bundle refresh token a failed import spent → the account keeping its pair
	listen     string                   // the TCP listener's address while it serves; "" = off
	logins     map[string]*pendingLogin // the page's logins waiting for a code, by account
}

func New(o Options) *Daemon {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Store != nil {
		o.Store.Now = o.Now // the holder log keeps the daemon's time
	}
	if o.LockPath == "" { // not in a temp dir the OS cleans up
		if h, err := config.DefaultHome(); err == nil {
			o.LockPath = filepath.Join(h, "daemon.lock")
		}
	}
	d := &Daemon{
		o:      o,
		svc:    claudecode.ServiceName(o.Config.Claude.ConfigDir),
		kcAcct: claudecode.KeychainAccount(),
		im: &Importer{KC: o.KC, Store: o.Store, Profiles: o.API, Locker: o.Switcher,
			Home: o.Home, Target: claudecode.ServiceName(o.Config.Claude.ConfigDir)},
		kick:       make(chan struct{}, 1),
		usage:      map[string]*acctUsage{},
		pending:    map[string]pendingPair{},
		logged:     map[string]string{},
		codes:      map[string]time.Time{},
		migrations: map[string]*migration{},
		unspent:    map[string]bool{},
		kept:       map[string]string{},
		logins:     map[string]*pendingLogin{},
	}
	th := o.Config.Thresholds
	d.thr.Store(&th)
	if u := o.Config.Remote.URL; u != "" {
		if key, err := o.KC.Get(store.VaultService, remoteKeyAccount); err == nil && len(key) > 0 {
			d.rem = remote{url: u, key: string(key)}
		} else {
			slog.Warn("remote: no key stored, asking Anthropic directly (ccswap remote set)", "url", u, "err", err)
		}
	}
	return d
}

// th is the thresholds in effect: config.toml's at start, then whatever SetThresholds saved.
func (d *Daemon) th() config.Thresholds { return *d.thr.Load() }

// serveAPI is serve; a test stops the API server first.
var serveAPI = serve

// Run serves the API and ticks every PollInterval (and on request) until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	ln, release, err := listen(d.o.Socket, d.o.LockPath)
	if err != nil {
		return err
	}
	defer release()
	SweepLogins(d.o.Home, d.im.Target, d.o.KC)
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	context.AfterFunc(ctx, d.bus.close)
	d.mu.Lock()
	d.runCtx, d.cancel = ctx, cancel
	d.mu.Unlock()
	srv := make(chan error, 1)
	go func() { srv <- serveAPI(ctx, ln, d) }()
	if d.o.Config.T3.Enabled {
		defer d.startHTTP(ctx, cancel)()
	}
	if d.o.Config.Update.Check {
		go d.watchReleases(ctx)
	}
	t := time.NewTicker(d.o.Config.PollInterval)
	defer t.Stop()
	for {
		d.Poll(ctx)
		select {
		case <-ctx.Done():
			err := <-srv
			d.stop()
			return d.ended(parent, err)
		case err := <-srv:
			stopped := ctx.Err() != nil // the server stopped because Run was asked to: not a failure
			cancel()
			d.stop()
			if stopped {
				return d.ended(parent, err)
			}
			if err == nil {
				err = errors.New("api server stopped")
			}
			return err
		case <-t.C:
		case <-d.kick:
		}
	}
}

// startHTTP serves the T3 Code usage hub and the web page; a failure to start it is logged and never
// stops the daemon. The returned func stops the listener and waits for it.
func (d *Daemon) startHTTP(ctx context.Context, cancel context.CancelFunc) func() {
	read, err := t3.Key(d.o.KC, t3.KeyAccount)
	if err != nil {
		slog.Error("http: read key unavailable, listener off", "err", err)
		return func() {}
	}
	admin, err := t3.Key(d.o.KC, t3.AdminKeyAccount)
	if err != nil {
		slog.Error("http: admin key unavailable, listener off", "err", err)
		return func() {}
	}
	ln, err := net.Listen("tcp", d.o.Config.T3.Listen)
	if err != nil {
		slog.Error("http: cannot listen, listener off", "addr", d.o.Config.T3.Listen, "err", err)
		return func() {}
	}
	k := httpKeys{read: read, admin: admin, local: ln.Addr().(*net.TCPAddr).IP.IsLoopback()}
	if !k.local {
		slog.Info("http: not on a loopback address, so the page and T3 Code only read", "addr", ln.Addr())
	}
	d.mu.Lock()
	d.listen = ln.Addr().String()
	d.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := t3.Serve(ctx, ln, newHTTPApp(d, d, k)); err != nil {
			slog.Error("http: listener stopped", "err", err)
		}
		d.mu.Lock()
		d.listen = ""
		d.mu.Unlock()
	}()
	return func() { cancel(); <-done }
}

// ended is Run's error once it stopped as asked: ErrRestart when an update asked, and not a signal or the
// caller, which stop the daemon whether or not an update is in place.
func (d *Daemon) ended(parent context.Context, err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.restart && parent.Err() == nil {
		return ErrRestart
	}
	return err
}

// stop waits for a tick an API request may still run (it may be saving rotated tokens), then
// saves every rotated pair still pending; what cannot be saved is a lost login and is logged.
func (d *Daemon) stop() {
	d.tick.Lock()
	defer d.tick.Unlock()
	d.mu.Lock()
	names := slices.Collect(maps.Keys(d.pending))
	d.mu.Unlock()
	for _, n := range names {
		if err := d.o.Switcher.Locked(func() error { return d.flushLocked(n) }); err != nil {
			slog.Error("shutdown: rotated tokens not saved, the account will need `ccswap login`", "account", n, "err", err)
		}
	}
}

func (d *Daemon) requestTick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// logOnce logs msg under topic unless it repeats the last line logged under topic.
func (d *Daemon) logOnce(topic string, lvl slog.Level, msg string, args ...any) {
	line := fmt.Sprint(lvl, msg, args)
	d.mu.Lock()
	dup := d.logged[topic] == line
	d.logged[topic] = line
	d.mu.Unlock()
	if !dup {
		slog.Log(context.Background(), lvl, msg, args...)
	}
}

// logDone ends topic's episode: its next line is logged again.
func (d *Daemon) logDone(topic string) {
	d.mu.Lock()
	delete(d.logged, topic)
	d.mu.Unlock()
}

// rotateLog keeps daemon.log under logMax. launchd appends through its own descriptor, so
// the file is copied aside and truncated in place, never renamed.
func (d *Daemon) rotateLog() {
	if d.o.Home == "" {
		return
	}
	p := filepath.Join(d.o.Home, "daemon.log")
	if fi, err := os.Stat(p); err != nil || fi.Size() <= logMax {
		return
	}
	err := func() error {
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.OpenFile(p+".1", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			dst.Close()
			return err
		}
		return dst.Close()
	}()
	if err == nil {
		err = os.Truncate(p, 0)
	}
	if err != nil {
		slog.Warn("daemon.log not rotated", "err", err)
	}
}

// notify shows msg as a sentence in the daemon's language and logs it in English.
func (d *Daemon) notify(msg *i18n.Error) {
	slog.Info("notify", "msg", capital(msg.In(i18n.EN)))
	d.o.Notifier.Notify(title, capital(msg.Error()))
}

// logWarning logs a switch's warning in English, each of several joined ones on its own line.
func logWarning(msg, to string, warn error) {
	ws := []error{warn}
	if j, ok := warn.(interface{ Unwrap() []error }); ok {
		ws = j.Unwrap()
	}
	for _, w := range ws {
		slog.Warn(msg, "to", to, "warning", w)
	}
}

// Poll runs one tick: reconcile, refresh vault tokens, fetch usage, auto-switch.
func (d *Daemon) Poll(ctx context.Context) { d.poll(ctx, false) }

// poll is a tick; force asks every account's usage now, unless a 429 asked to wait or it was asked
// under refreshFloor ago.
func (d *Daemon) poll(ctx context.Context, force bool) {
	d.tick.Lock()
	defer d.tick.Unlock()
	defer d.bus.publish()
	if ctx.Err() != nil {
		return
	}
	d.rotateLog()
	d.readActivity()
	holder, ok := d.reconcile(ctx)
	if ok { // unidentified entry: a vault copy may be the live chain's consumed token
		d.refreshVault(ctx, holder)
	}
	d.pollUsage(ctx, holder, force)
	if holder != "" && d.o.Store.State().Active == holder {
		d.autoSwitch(ctx, holder)
	}
}

// readActivity notes Claude Code's last request for cache-aware; fill-first never asks, so the
// transcripts are not walked.
func (d *Daemon) readActivity() {
	if d.o.Projects == "" || strategyOf(d.o.Store.State()) != policy.CacheAware {
		d.mu.Lock()
		d.activity, d.activityOK = time.Time{}, false
		d.mu.Unlock()
		return
	}
	at, err := claudecode.LastActivity(d.o.Projects)
	if err != nil {
		d.logOnce("activity", slog.LevelWarn, "cannot read Claude Code transcripts; cache-aware stays put", "dir", d.o.Projects, "err", err)
	} else {
		d.logDone("activity")
	}
	d.mu.Lock()
	d.activity, d.activityOK = at, err == nil
	d.mu.Unlock()
}

// idle is how long Claude Code has made no request: 0 when unknown, unbounded when it never ran.
func (d *Daemon) idle(now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case !d.activityOK:
		return 0
	case d.activity.IsZero():
		return math.MaxInt64
	}
	return now.Sub(d.activity)
}

func strategyOf(st store.State) policy.Strategy {
	if st.Strategy == string(policy.CacheAware) {
		return policy.CacheAware
	}
	return policy.FillFirst
}

func sameState(a, b store.State) bool {
	return a.Active == b.Active && a.LastSwitchAt.Equal(b.LastSwitchAt) && len(a.Accounts) == len(b.Accounts)
}

// reconcile identifies the login in Claude Code's entry and points State.Active at it. ok=false:
// identification failed or raced a switch, so nothing may rely on the entry this tick. holder is
// the known account in the entry; "" for an empty entry or a stranger's login.
func (d *Daemon) reconcile(ctx context.Context) (holder string, ok bool) {
	before := d.o.Store.State()
	name, known, entry, err := d.o.Switcher.Identify(ctx)
	empty := errors.Is(err, keychain.ErrNotFound)
	d.noteEntry(before.Active, name, known, entry)
	if err != nil && !empty { // profile or network down, unreadable entry: keep the previous state
		d.logOnce("reconcile", slog.LevelWarn, "reconcile: cannot identify the Claude Code login", "err", err)
		d.mu.Lock()
		d.holder = ""
		d.mu.Unlock()
		return "", false
	}
	d.logDone("reconcile")
	var notices []*i18n.Error
	// under the switcher lock no switch, adopt or import can land between the check and the update.
	err = d.o.Switcher.Locked(func() error {
		if !sameState(before, d.o.Store.State()) {
			return errRaced
		}
		stranger := !known && !empty
		d.mu.Lock()
		if stranger && !d.unknown {
			notices = append(notices, i18n.Err("daemon.unknown_active"))
		}
		d.unknown, d.holder = stranger, name
		d.mu.Unlock()
		if stranger || known && before.Active == name { // the holder log follows the entry, not only switches
			if err := d.o.Store.Hold(name); err != nil {
				d.logOnce("hold", slog.LevelWarn, "reconcile: holder log not saved", "err", err)
			} else {
				d.logDone("hold")
			}
		}
		if known {
			last := before.LastSwitchAt // a switch whose state write failed still counts
			if w := d.o.Switcher.LastWrite(); w.After(last) {
				last = w
			}
			d.dropParkedLocked(name, last)
		}
		if a, _ := before.Find(name); !known || (before.Active == name && !a.NeedsLogin) {
			return nil
		}
		err := d.o.Store.Update(func(x *store.State) error {
			x.Active = name
			if w := d.o.Switcher.LastWrite(); w.After(x.LastSwitchAt) {
				x.LastSwitchAt = w // the switch whose state write failed: MinDwell counts from it
			}
			for i := range x.Accounts {
				if x.Accounts[i].Name == name {
					x.Accounts[i].NeedsLogin = false // a fresh `claude /login` heals it
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		slog.Info("reconcile: Claude Code is logged in as", "account", name, "was", before.Active)
		if p := before.Active; p != "" && p != name && d.markLostLocked(p) {
			notices = append(notices, loginAgain(p))
		}
		return nil
	})
	for _, m := range notices {
		d.notify(m)
	}
	switch {
	case errors.Is(err, errRaced):
		return "", false
	case err != nil: // identified all the same; State.Active stays stale, auto-switch waits
		slog.Error("reconcile: save state", "err", err)
	}
	return name, true
}

// dropParkedLocked deletes name's vault copy of the chain in Claude Code's entry once name, positively
// identified there, has held the entry for parkedFor since the last switch. A copy with
// another refresh token (a newer login or a consumed token) is kept. The caller holds the switcher lock.
func (d *Daemon) dropParkedLocked(name string, lastSwitch time.Time) {
	if d.o.Now().Sub(lastSwitch) < parkedFor {
		return
	}
	v, err := d.o.Store.Creds(name)
	if err != nil {
		return
	}
	if e, err := d.entry(); err != nil || e == nil || e.OAuth.RefreshToken != v.OAuth.RefreshToken {
		return
	}
	if err := d.o.Store.DeleteCreds(name); err != nil && !errors.Is(err, keychain.ErrNotFound) {
		slog.Warn("reconcile: vault copy of the login in Claude Code's entry not dropped", "account", name, "err", err)
		return
	}
	slog.Info("reconcile: vault copy of the login in Claude Code's entry dropped", "account", name)
}

// markLostLocked flags p NeedsLogin when ccswap has no copy of its login: Claude Code's entry holds
// another one now, so p's chain is gone. The caller holds the switcher lock.
func (d *Daemon) markLostLocked(p string) bool {
	if _, err := d.o.Store.Creds(p); !errors.Is(err, keychain.ErrNotFound) {
		return false
	}
	return d.needsLogin(p)
}

// entry reads Claude Code's entry; nil when there is none.
func (d *Daemon) entry() (*claudecode.Credentials, error) {
	b, err := d.o.KC.Get(d.svc, d.kcAcct)
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := claudecode.ParseCredentials(b)
	if errors.Is(err, claudecode.ErrNoOAuth) {
		return nil, nil // not logged in
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *Daemon) acct(name string) *acctUsage { // caller holds d.mu
	u := d.usage[name]
	if u == nil {
		u = &acctUsage{}
		d.usage[name] = u
	}
	return u
}

func (d *Daemon) setErr(name, msg string) {
	d.mu.Lock()
	d.acct(name).err = msg
	d.mu.Unlock()
}

// setRefreshErr sets name's error to a failed refresh's.
func (d *Daemon) setRefreshErr(name, msg string) {
	d.mu.Lock()
	u := d.acct(name)
	u.err, u.refreshErr = msg, msg
	d.mu.Unlock()
}

// holderNow is the known account in Claude Code's entry as last identified; "" = none.
func (d *Daemon) holderNow() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holder
}

// forgetLocked drops what the daemon and the switcher remember of names, accounts no longer here. The
// caller holds the switcher lock.
func (d *Daemon) forgetLocked(names ...string) {
	for _, n := range names {
		d.o.Switcher.ForgetLocked(n)
	}
	d.mu.Lock()
	for _, n := range names {
		if u := d.usage[n]; u != nil {
			u.endAsk() // a T3 request waiting for it must not sit out the timeout
		}
		delete(d.usage, n)
		delete(d.pending, n)
		delete(d.unspent, n)
	}
	d.mu.Unlock()
}

// flushLocked saves name's pending rotated pair over the consumed one it replaces; a vault that
// holds anything else (a newer login) or an active account keeps what it has. The caller holds
// the switcher lock.
func (d *Daemon) flushLocked(name string) error {
	d.mu.Lock()
	p, ok := d.pending[name]
	d.mu.Unlock()
	if !ok {
		return nil
	}
	if d.o.Store.State().Active == name || d.holderNow() == name {
		slog.Info("refresh: unsaved rotated pair dropped, the account is in Claude Code's entry now", "account", name)
	} else {
		cur, err := d.o.Store.Creds(name)
		switch {
		case err != nil && !errors.Is(err, keychain.ErrNotFound):
			return fmt.Errorf("%w: %v", errSave, err)
		case err == nil && cur.OAuth.RefreshToken != p.replaces:
			slog.Info("refresh: newer vault login supersedes the unsaved pair", "account", name)
		default:
			if err := d.putCreds(name, p.creds); err != nil {
				return fmt.Errorf("%w: %v", errSave, err)
			}
		}
	}
	d.mu.Lock()
	delete(d.pending, name)
	d.mu.Unlock()
	return nil
}

// putCreds saves freshly rotated tokens, retrying fast keychain failures (wall clock, not d.o.Now).
func (d *Daemon) putCreds(name string, c claudecode.Credentials) error {
	start := time.Now()
	for i := 0; ; i++ {
		t := time.Now()
		err := d.o.Store.PutCreds(name, c)
		if err == nil || i == len(saveBackoff) || time.Since(t) > saveSlow || time.Since(start)+saveBackoff[i] > saveBudget {
			return err
		}
		time.Sleep(saveBackoff[i])
	}
}

func (d *Daemon) refreshVault(ctx context.Context, holder string) {
	for _, a := range d.o.Store.State().Accounts {
		d.refreshOne(ctx, a.Name, holder, false)
	}
	d.announceEndingLogins(holder)
}

// noteLogin keeps when c's tokens run out, for name's view.
func (d *Daemon) noteLogin(name string, c claudecode.Credentials) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u := d.acct(name)
	u.token, u.login = time.Time{}, time.Time{}
	if c.OAuth.AccessToken != "" {
		u.token = c.Expiry()
	}
	if c.OAuth.RefreshTokenExpiresAt > 0 {
		u.login = time.UnixMilli(c.OAuth.RefreshTokenExpiresAt)
	}
}

// noteEntry keeps the times of the login in Claude Code's entry, as the tick read it, for name, the account
// identified there. With no account identified there, active's times go: they would be an older chain's.
func (d *Daemon) noteEntry(active, name string, known bool, c claudecode.Credentials) {
	if known {
		d.noteLogin(name, c)
		return
	}
	if active != "" {
		d.mu.Lock()
		u := d.acct(active)
		u.token, u.login = time.Time{}, time.Time{}
		d.mu.Unlock()
	}
}

// ending tells a login's end loginNotice ahead or past: no refresh moves it, only a new login does.
func ending(login, now time.Time) bool { return !login.IsZero() && login.Sub(now) <= loginNotice }

// announceEndingLogins says once per login, loginNotice ahead, that it ends: no refresh moves its end, only
// a new login does, `claude /login` for the account in Claude Code.
func (d *Daemon) announceEndingLogins(holder string) {
	st, now := d.o.Store.State(), d.o.Now()
	var notices []*i18n.Error
	d.mu.Lock()
	for _, a := range st.Accounts {
		u := d.usage[a.Name]
		if a.NeedsLogin || u == nil || !now.Before(u.login) || !ending(u.login, now) || u.login.Sub(u.announced).Abs() < loginJitter {
			continue
		}
		u.announced = u.login
		m := i18n.Err("daemon.login_ends", a.Name, clockText(u.login, now))
		if a.Name == holder {
			m = i18n.Err("daemon.login_ends_in_claude", a.Name, clockText(u.login, now))
		}
		notices = append(notices, m)
	}
	d.mu.Unlock()
	for _, m := range notices {
		d.notify(m)
	}
}

// refreshOne refreshes name's vault token when it is about to expire, or at once with force or while
// an imported login's bundle refresh token is unspent. Rules: never the active account nor the
// identified holder of Claude Code's entry, never a vault copy of the chain in the entry, never an
// account a migrate export holds, read → refresh → save under the switcher's lock, and the rotated pair
// is saved before its access token is used. A spent login comes back as switcher.ErrNeedsLogin, one it
// did not ask Anthropic about as skipped.
func (d *Daemon) refreshOne(ctx context.Context, name, holder string, force bool) error {
	invalid, flipped, rotated := false, false, false
	// unspent and NeedsLogin are set under the switcher lock: an import of name in between brings a new login
	spent := func() {
		d.mu.Lock()
		delete(d.unspent, name)
		d.acct(name).refreshed = d.o.Now()
		d.mu.Unlock()
	}
	err := d.o.Switcher.Locked(func() error {
		if d.o.Switcher.Frozen(name) { // its bundle holds this refresh token
			return skipped{i18n.Err("daemon.migrating", switcher.ErrFrozen, name)}
		}
		if err := d.flushLocked(name); err != nil {
			return err
		}
		st := d.o.Store.State()
		a, ok := st.Find(name)
		switch {
		case !ok:
			return skipped{ErrNotFound}
		case a.NeedsLogin:
			return skipped{i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, name)}
		case st.Active == name || name == holder:
			return skipped{ErrClaudeOwns}
		case ctx.Err() != nil:
			return skipped{ctx.Err()}
		}
		v, err := d.o.Store.Creds(name)
		if errors.Is(err, keychain.ErrNotFound) { // ccswap keeps no login of it
			return skipped{i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, name)}
		}
		if err != nil {
			return fmt.Errorf("read vault: %w", err)
		}
		d.noteLogin(name, v)
		d.mu.Lock()
		force = force || d.unspent[name]
		d.mu.Unlock()
		if !force && v.OAuth.AccessToken != "" && v.Expiry().Sub(d.o.Now()) >= refreshBefore {
			return nil
		}
		e, err := d.entry()
		if err != nil {
			return fmt.Errorf("read claude code entry: %w", err)
		}
		if e != nil && e.OAuth.RefreshToken == v.OAuth.RefreshToken {
			return skipped{ErrClaudeOwns}
		}
		old := v.OAuth.RefreshToken
		v, ok, err = d.rotate(ctx, v)
		if !ok {
			if invalid = errors.Is(err, anthropic.ErrInvalidGrant); invalid {
				spent()
				flipped = d.needsLogin(name) // the vault still holds the dead token: no vault write without this lock
			}
			return err
		}
		// ErrNoAccessToken with a rotated refresh token: save it all the same, the old one is spent.
		rerr := err
		spent()
		d.noteLogin(name, v)
		rotated = rerr == nil
		if err := d.putCreds(name, v); err != nil {
			d.mu.Lock()
			d.pending[name] = pendingPair{v, old}
			d.mu.Unlock()
			return fmt.Errorf("%w: %v", errSave, err)
		}
		return rerr
	})
	topic := "refresh:" + name
	var sk skipped
	switch {
	case invalid:
		if flipped {
			d.notify(loginAgain(name))
		}
		return i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, name)
	case errors.As(err, &sk):
		d.logDone(topic)
	case errors.Is(err, errSave):
		d.logOnce(topic, slog.LevelError, "refresh: unsaved rotated tokens kept in memory, retrying next tick", "account", name, "err", err)
		d.setRefreshErr(name, i18n.T("daemon.refresh_not_saved"))
	case err != nil:
		d.logOnce(topic, slog.LevelWarn, "refresh failed, retrying next tick", "account", name, "err", err)
		d.setRefreshErr(name, i18n.T("daemon.refresh_failed"))
	default:
		d.logDone(topic)
		if rotated {
			d.clearRefreshErr(name)
		}
	}
	return err
}

// clearRefreshErr drops name's error when a refresh set it: the login works again, and the next ask for
// its usage, maybe half an hour off, would only then say so.
func (d *Daemon) clearRefreshErr(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if u := d.acct(name); u.err != "" && u.err == u.refreshErr {
		u.err = ""
	}
}

// rotate spends v's refresh token for a new pair, never cancelled midway: a lost response is a lost login.
// ok tells the token is spent and c is the rotated pair; err is then nil or ErrNoAccessToken.
func (d *Daemon) rotate(ctx context.Context, v claudecode.Credentials) (c claudecode.Credentials, ok bool, err error) {
	t, err := d.o.API.Refresh(context.WithoutCancel(ctx), v.OAuth.RefreshToken, v.OAuth.Scopes)
	if err != nil && (!errors.Is(err, anthropic.ErrNoAccessToken) || t.RefreshToken == "") {
		return v, false, err
	}
	v.OAuth.AccessToken, v.OAuth.RefreshToken, v.OAuth.ExpiresAt = t.AccessToken, t.RefreshToken, t.ExpiresAt.UnixMilli()
	if !t.RefreshTokenExpiresAt.IsZero() {
		v.OAuth.RefreshTokenExpiresAt = t.RefreshTokenExpiresAt.UnixMilli()
	}
	if len(t.Scopes) > 0 {
		v.OAuth.Scopes = t.Scopes
	}
	return v, true, err
}

// needsLogin flags name NeedsLogin; true when the flag flipped (notify once per transition).
func (d *Daemon) needsLogin(name string) bool {
	flipped := false
	err := d.o.Store.Update(func(x *store.State) error {
		for i := range x.Accounts {
			if a := &x.Accounts[i]; a.Name == name && !a.NeedsLogin && x.Active != name {
				a.NeedsLogin, flipped = true, true
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("mark needs login", "account", name, "err", err)
		return false
	}
	return flipped
}

// accessToken returns a live access token: Claude Code's entry for its identified holder (read
// only, Claude Code refreshes it), the vault for the others.
func (d *Daemon) accessToken(name, holder string) (string, bool) {
	var c *claudecode.Credentials
	if name == holder {
		e, err := d.entry()
		if err != nil || e == nil {
			return "", false
		}
		c = e
	} else {
		d.mu.Lock()
		_, pend := d.pending[name]
		d.mu.Unlock()
		if pend { // not saved yet: not used for anything
			return "", false
		}
		v, err := d.o.Store.Creds(name)
		if err != nil {
			return "", false
		}
		c = &v
	}
	if c.OAuth.AccessToken == "" || !c.Expiry().After(d.o.Now()) {
		return "", false
	}
	return c.OAuth.AccessToken, true
}

func (d *Daemon) pollUsage(ctx context.Context, holder string, force bool) {
	st := d.o.Store.State()
	served := d.remoteUsage(ctx)
	sem := make(chan struct{}, usageWorkers)
	var wg sync.WaitGroup
	for _, a := range st.Accounts {
		if !a.Enabled || a.NeedsLogin {
			continue
		}
		// a refresh asks here: the other ccswap takes no refresh from a read key
		if ra, ok := served[strings.ToLower(a.Email)]; ok && !force && d.takeRemote(a.Name, ra, maxAge(a.Name == holder, d.th(), d.o.Config.PollInterval)) {
			continue
		}
		if d.askLater(a.Name, force) {
			continue
		}
		tok, ok := d.accessToken(a.Name, holder)
		if !ok {
			d.endAsk(a.Name)
			if a.Name == holder { // Claude Code owns the token and has not refreshed it
				d.setErr(a.Name, i18n.T("daemon.claude_idle"))
			}
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			d.fetch(ctx, a.Name, tok)
		})
	}
	wg.Wait()
	d.mu.Lock()
	d.last = d.o.Now()
	d.mu.Unlock()
}

// askLater reports whether name's usage was asked too recently or is being asked: any tick,
// settings change or T3 request in between is answered from what the daemon already has. force (a
// refresh) skips the check interval for refreshFloor, with no allowance for an early tick, never a
// wait Anthropic imposed: a 429's Retry-After or the slower pace it taught. Otherwise it marks the ask
// in flight; the caller fetches, or ends it with endAsk.
func (d *Daemon) askLater(name string, force bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, now := d.acct(name), d.o.Now()
	pace, early := max(u.every, refreshFloor), time.Duration(0)
	if !force {
		pace = d.paceLocked(name)
		early = pace / 10 // the next tick may come a little early
	}
	if u.inFlight != nil || now.Before(u.retryAt) || now.Sub(u.asked) < pace-early {
		return true
	}
	u.inFlight = make(chan struct{})
	return false
}

func (d *Daemon) endAsk(name string) {
	d.mu.Lock()
	d.acct(name).endAsk()
	d.mu.Unlock()
}

// endAsk ends the ask in flight and wakes whoever waits for its answer.
func (u *acctUsage) endAsk() {
	if u.inFlight != nil {
		close(u.inFlight)
		u.inFlight = nil
	}
}

// awaitAsk waits for an ask of name in flight, if any, as long as a request to Anthropic may take.
func (d *Daemon) awaitAsk(ctx context.Context, name string) {
	d.mu.Lock()
	done := d.acct(name).inFlight
	d.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(anthropic.Timeout):
	}
}

// paceLocked is how often name's usage is asked: its check interval, or the slower pace a 429 taught.
func (d *Daemon) paceLocked(name string) time.Duration {
	return max(d.acct(name).every, d.checkEveryLocked(name))
}

// freshForLocked is how old name's usage may be and still count: one pace past the next ask, so a
// missed answer is fine and two are not. A wait Anthropic imposed puts the next ask off, and the
// window with it, by an hour at most: an account nobody may ask yet is not unknown, but a 429 never
// revives numbers hours old, nor do 429s in a row keep them counting.
func (d *Daemon) freshForLocked(name string) time.Duration {
	u, pace := d.acct(name), d.paceLocked(name)
	return max(pace, min(u.retryAt.Sub(u.checkedAt), pace+time.Hour)) + pace
}

// checkEveryLocked is how often name's usage is asked: the account in use more often.
func (d *Daemon) checkEveryLocked(name string) time.Duration {
	if th := d.th(); name != d.holder {
		return th.CheckOthers
	}
	return d.th().CheckActive
}

// maxAge is how old the other ccswap's numbers may be for an account: the account in use is asked
// directly once they are older than its check interval and a tick, as the other ccswap asks on its
// own ticks.
func maxAge(active bool, th config.Thresholds, poll time.Duration) time.Duration {
	if active {
		return th.CheckActive + poll
	}
	return 2 * th.CheckOthers
}

// fetch asks for name's usage, after askLater let it, and ends the ask. Failures keep the last
// windows and CheckedAt and set the error.
func (d *Daemon) fetch(ctx context.Context, name, tok string) (anthropic.UsageResult, error) {
	asked := d.o.Now()
	res, err := d.o.API.Usage(ctx, tok)
	now := d.o.Now()
	d.mu.Lock()
	u := d.acct(name)
	u.remote = false
	if res.Status/100 == 2 || res.Status == http.StatusTooManyRequests { // no answer, or a 5xx: the next tick asks again
		u.asked = asked
	}
	pace := d.paceLocked(name)
	switch {
	case err != nil && res.Status == 0:
		u.err = i18n.T("daemon.no_connection")
	case err != nil:
		u.err = i18n.T("daemon.unreadable_answer")
	case res.Status == http.StatusTooManyRequests: // the usage endpoint throttles each account: slow down for good
		u.every, u.calm = min(2*pace, maxBackoff), 0
		u.retryAt = now.Add(max(u.every, min(res.RetryAfter, time.Hour)))
		u.err = i18n.T("daemon.rate_limited", u.retryAt.Local().Format("15:04"))
	case res.Status/100 != 2:
		u.err = i18n.T("daemon.anthropic_answered", res.Status)
	default:
		u.fiveHour, u.sevenDay, u.models = res.Usage.FiveHour, res.Usage.SevenDay, res.Usage.Models
		u.checkedAt, u.err = now, ""
		u.sample()
		if u.calm++; u.calm >= calmAnswers && u.every > 0 { // probe whether the endpoint eased up
			if u.every, u.calm = u.every*3/4, 0; u.every <= d.checkEveryLocked(name) {
				u.every = 0 // no slower than the interval: nothing left to wait for
			}
		}
	}
	// a 429 is transient: T3 keeps the last real answer
	if res.Status != 0 && (res.Status != http.StatusTooManyRequests || u.rawStatus == 0) {
		u.rawStatus, u.rawBody, u.rawAt = res.Status, res.Body, now
	}
	failed := u.err != ""
	u.endAsk()
	d.mu.Unlock()
	if failed {
		d.logOnce("usage:"+name, slog.LevelWarn, "usage", "account", name, "status", res.Status, "err", err)
	} else {
		d.logDone("usage:" + name)
	}
	return res, err
}

// UsageRaw returns name's upstream usage response, from cache when younger than maxAge, else live,
// or from the ask of name in flight, waited for; when no live fetch is possible (an idle entry's
// expired token, no connection) or allowed (a disabled account or one waiting for a login, which
// ccswap never asks for) the last cached response, however old.
func (d *Daemon) UsageRaw(ctx context.Context, name string, maxAge time.Duration) (int, []byte, error) {
	a, ok := d.o.Store.State().Find(name)
	if !ok {
		return 0, nil, ErrNotFound
	}
	cached := func(maxAge time.Duration) (int, []byte, bool) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if u := d.usage[name]; u != nil && u.rawStatus != 0 && (maxAge < 0 || d.o.Now().Sub(u.rawAt) <= maxAge) {
			return u.rawStatus, bytes.Clone(u.rawBody), true
		}
		return 0, nil, false
	}
	if s, b, ok := cached(maxAge); ok {
		return s, b, nil
	}
	d.mu.Lock()
	holder, remote := d.holder, d.acct(name).remote
	d.mu.Unlock()
	err := errNoToken
	if a.Enabled && !a.NeedsLogin && !remote {
		if tok, ok := d.accessToken(name, holder); ok && !d.askLater(name, false) {
			var res anthropic.UsageResult
			if res, err = d.fetch(ctx, name, tok); res.Status != 0 {
				return res.Status, res.Body, nil
			}
		} else {
			d.awaitAsk(ctx, name) // e.g. a tick's: its answer is this one's
			if _, ok := d.o.Store.State().Find(name); !ok {
				return 0, nil, ErrNotFound // removed while waiting
			}
		}
	}
	if s, b, ok := cached(-1); ok {
		return s, b, nil
	}
	return 0, nil, err
}

func (d *Daemon) StoredAccounts() []store.Account { return d.o.Store.State().Accounts }

func stateOf(a store.Account, u *acctUsage) policy.AccountState {
	s := policy.AccountState{Name: a.Name, Priority: a.Priority, Enabled: a.Enabled, NeedsLogin: a.NeedsLogin}
	if u != nil {
		s.FiveHour, s.SevenDay, s.Models = u.fiveHour, u.sevenDay, u.models
		s.LimitHitUntil, s.CheckedAt = u.limitHitUntil, u.checkedAt
	}
	return s
}

func (d *Daemon) states(st store.State) []policy.AccountState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]policy.AccountState, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		s := stateOf(a, d.usage[a.Name])
		if !s.CheckedAt.IsZero() { // never asked: nothing to keep fresh, and no entry to make
			s.FreshFor = d.freshForLocked(a.Name)
		}
		if _, p := d.pending[a.Name]; p && a.Name != st.Active {
			s.NeedsLogin = true // its vault copy is stale until the rotated pair is saved
		}
		if a.Name != st.Active && d.o.Switcher.Frozen(a.Name) {
			s.NeedsLogin = true // a migrate export holds it: no switch may reach it
		}
		out = append(out, s)
	}
	return out
}

func loginAgain(name string) *i18n.Error { return i18n.Err("daemon.login_again", name) }

func capital(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

func clockText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

// allExhausted says when name, the account that frees up first, resets; key dedups notices.
func (d *Daemon) allExhausted(states []policy.AccountState, name string, now time.Time) (why *i18n.Error, key string) {
	for _, s := range states {
		if s.Name != name || !s.Enabled || s.NeedsLogin {
			continue
		}
		if at, ok := policy.FreeAt(s, d.th(), now); ok {
			at = at.Round(time.Minute)
			return i18n.Err("daemon.all_limited", clockText(at, now), name), at.UTC().Format(time.RFC3339)
		}
	}
	return i18n.Err("daemon.all_limited_no_reset"), "unknown"
}

// firstNotice reports whether key differs from the last all-exhausted notice and records it.
func (d *Daemon) firstNotice(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reset == key {
		return false
	}
	d.reset = key
	return true
}

// unknownOthers reports whether another candidate has only stale data that does not show it at its
// limit: then "all accounts are at their limit" is not known.
func unknownOthers(states []policy.AccountState, active string, th config.Thresholds, now time.Time) bool {
	for _, s := range states {
		if s.Name == active || !s.Enabled || s.NeedsLogin {
			continue
		}
		if !policy.Fresh(s, now) && !policy.Exhausted(s, th, now) {
			return true
		}
	}
	return false
}

// blockedWhy says why holder cannot stay when its limit is not the reason; nil when it is.
func blockedWhy(st store.State, holder string) *i18n.Error {
	cur, _ := st.Find(holder)
	switch {
	case !cur.Enabled:
		return i18n.Err("daemon.disabled", holder)
	case cur.NeedsLogin:
		return i18n.Err("daemon.needs_login", holder)
	}
	return nil
}

// stayNotice tells, once per episode, that no account can take over from holder.
func (d *Daemon) stayNotice(st store.State, states []policy.AccountState, holder string, now time.Time) {
	cur, _ := st.Find(holder)
	var text *i18n.Error
	var key string
	switch {
	case !cur.Enabled:
		text, key = i18n.Err("daemon.active_disabled_no_other", holder), "disabled:"+holder
	case cur.NeedsLogin, unknownOthers(states, holder, d.th(), now):
		return // the login notice went out already / other accounts' data is unknown
	default:
		text, key = d.allExhausted(states, holder, now)
	}
	if d.firstNotice(key) {
		d.notify(text)
	}
}

// claimHolder records name as the entry's holder after a switch to it, unless another switch
// landed since (State.Active names another account). Under the switcher lock, like every switch.
func (d *Daemon) claimHolder(name string) {
	d.o.Switcher.Locked(func() error {
		d.claimHolderLocked(name)
		return nil
	})
}

func (d *Daemon) claimHolderLocked(name string) {
	if d.o.Store.State().Active == name {
		d.mu.Lock()
		d.unknown, d.holder = false, name
		d.mu.Unlock()
	}
}

// autoSwitch acts on the policy for holder, the account identified in Claude Code's entry.
func (d *Daemon) autoSwitch(ctx context.Context, holder string) {
	st := d.o.Store.State()
	if !st.Auto {
		return
	}
	now := d.o.Now()
	th := d.th()
	states := d.states(st)
	dec := policy.Decide(now, states, holder, st.LastSwitchAt, th, strategyOf(st), d.idle(now))
	if i := slices.IndexFunc(states, func(s policy.AccountState) bool { return s.Name == holder }); dec.Reason != policy.ReasonAllExhausted &&
		i >= 0 && !policy.Exhausted(states[i], th, now) {
		d.mu.Lock()
		d.reset = "" // the all-exhausted episode is over: the next one notifies again
		d.mu.Unlock()
	}
	var why *i18n.Error
	var key string
	blocked := blockedWhy(st, holder)
	switch {
	case dec.Reason == policy.ReasonNone:
		d.logDone("auto")
		return
	case dec.Reason == policy.ReasonRecovered:
		why = i18n.Err("daemon.available_again", dec.SwitchTo)
	case dec.Reason == policy.ReasonResetsSooner:
		why = i18n.Err("daemon.limit_resets_sooner", dec.SwitchTo)
	case dec.SwitchTo == "": // all exhausted, staying
		d.stayNotice(st, states, holder, now)
		return
	case blocked != nil:
		why = blocked
	case dec.Reason == policy.ReasonExhausted || unknownOthers(states, holder, th, now):
		why = i18n.Err("daemon.hit_limit", holder) // not "all": some account is merely unknown
	default:
		why, key = d.allExhausted(states, dec.SwitchTo, now)
	}
	// under the switcher's lock: void if someone switched since this decision.
	warn, err := d.o.Switcher.SwitchFrom(ctx, holder, dec.SwitchTo)
	switch {
	case errors.Is(err, switcher.ErrHolderChanged):
		slog.Info("auto-switch dropped: Claude Code's login changed meanwhile", "from", holder, "to", dec.SwitchTo)
		return
	case errors.Is(err, switcher.ErrNearRefresh), errors.Is(err, switcher.ErrTargetRefreshing), errors.Is(err, switcher.ErrEntryChanged), errors.Is(err, switcher.ErrEntryExpired):
		d.logOnce("auto", slog.LevelInfo, "auto-switch postponed: Claude Code is about to refresh its token or has not run for a while",
			"to", dec.SwitchTo, "err", err)
		return
	case err != nil:
		d.logOnce("auto", slog.LevelError, "auto-switch failed", "to", dec.SwitchTo, "err", err)
		return
	case warn != nil:
		logWarning("auto-switch warning", dec.SwitchTo, warn)
	}
	d.logDone("auto")
	d.claimHolder(dec.SwitchTo)
	d.requestTick() // the account now in use is asked at its own pace, at once if that allows
	if key != "" && !d.firstNotice(key) {
		return
	}
	d.notify(i18n.Err("daemon.switched", dec.SwitchTo, why))
}

// Signal handles a StopFailure hook payload: a subscription limit marks the active account
// and ticks at once; every other error is ignored.
func (d *Daemon) Signal(ctx context.Context, p map[string]any) error {
	if e, _ := p["error"].(string); e != "rate_limit" {
		return nil
	}
	if msg, _ := p["last_assistant_message"].(string); usageLimit(msg) {
		d.limitHit(d.tickCtx(ctx), msg)
	}
	d.requestTick()
	return nil
}

// limitHit marks the account in Claude Code's entry as at its limit, unless the signal came right
// after a switch (it belongs to the previous account) or the usage shows the window msg names far
// from its limit or reset since. Usage too old to tell is asked at once, unless the account is disabled or waits for
// a login, Anthropic made it wait, a T3 request's ask of it is in flight, or there is no live token:
// then the signal counts.
func (d *Daemon) limitHit(ctx context.Context, msg string) {
	d.tick.Lock() // no tick asks meanwhile, and the next signal judges by what this one asked
	defer d.tick.Unlock()
	if h := d.staleHolder(msg); h != "" && !d.askLater(h, true) {
		if tok, ok := d.accessToken(h, h); ok {
			d.fetch(ctx, h, tok)
		} else {
			d.endAsk(h)
		}
	}
	st, now := d.o.Store.State(), d.o.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.holder != "" && now.Sub(st.LastSwitchAt) >= signalGrace { // the account in the entry
		u := d.acct(d.holder)
		if w := named(u, msg); !now.Before(u.limitHitUntil) && !d.farFromLimit(u, w, now) { // retries must not extend it
			u.limitHitUntil = now.Add(limitBackoff)
			if r := limitEnd(u, w, msg, now); r.After(now) && r.Before(u.limitHitUntil) {
				u.limitHitUntil = r // longer, it would hide the reset from the all-exhausted pick
			}
		}
	}
}

// staleHolder is the account in Claude Code's entry when limitHit would judge msg by its usage but
// that is older than signalAge; "" when asking would change nothing.
func (d *Daemon) staleHolder(msg string) string {
	st, now := d.o.Store.State(), d.o.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.holder == "" || now.Sub(st.LastSwitchAt) < signalGrace || !limitName.MatchString(msg) {
		return ""
	}
	if a, ok := st.Find(d.holder); ok && (!a.Enabled || a.NeedsLogin) { // never asked for
		return ""
	}
	if u := d.acct(d.holder); now.Before(u.limitHitUntil) || now.Sub(u.checkedAt) <= d.signalAge() {
		return ""
	}
	return d.holder
}

// signalAge is how old the usage of the account in use may be to judge a limit signal by: one check
// of it (at most StaleAfter) and a tick, as a tick a signal requests lands off the ticker's phase and
// puts the next ask up to a tick later.
func (d *Daemon) signalAge() time.Duration {
	return min(d.th().CheckActive, policy.StaleAfter) + d.o.Config.PollInterval
}

var limitName = regexp.MustCompile(`^You've hit your (\w+) limit`)

// farFromLimit reports whether fresh usage shows w, the window the limit message names, well below its
// limit: then the failing session ran on another account. The hook can't tell itself: Claude Code is
// observed to run hooks without CLAUDE_CODE_OAUTH_TOKEN (a session on its own token, e.g. an agent
// runner's). So does a reset of w since the check: the message is from before it (a late hook, a clock
// ahead), as no window runs out within signalAge of its start.
// Unknown wording, or usage older than signalAge: false, and the signal counts.
func (d *Daemon) farFromLimit(u *acctUsage, w *anthropic.Window, now time.Time) bool {
	if w == nil || now.Sub(u.checkedAt) > d.signalAge() {
		return false
	}
	return w.Utilization < nearLimit || w.ResetsAt.After(u.checkedAt) && !w.ResetsAt.After(now)
}

// limitEnd is when the limit msg reports ends by u's numbers: the reset of w, the window msg names, or
// with wording that names none, the soonest coming reset of a window at nearLimit or more; zero when unknown.
func limitEnd(u *acctUsage, w *anthropic.Window, msg string, now time.Time) time.Time {
	if limitName.MatchString(msg) {
		if w == nil {
			return time.Time{}
		}
		return w.ResetsAt
	}
	ws := []*anthropic.Window{u.fiveHour, u.sevenDay}
	for i := range u.models {
		ws = append(ws, &u.models[i].Window)
	}
	var end time.Time
	for _, x := range ws {
		if x != nil && x.Utilization >= nearLimit && x.ResetsAt.After(now) && (end.IsZero() || x.ResetsAt.Before(end)) {
			end = x.ResetsAt
		}
	}
	return end
}

// named is u's window that msg says is at its limit; nil for unknown wording or a window u has no numbers for.
func named(u *acctUsage, msg string) *anthropic.Window {
	m := limitName.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	switch m[1] {
	case "session":
		return u.fiveHour
	case "weekly":
		return u.sevenDay
	}
	for i := range u.models {
		if strings.EqualFold(u.models[i].Name, m[1]) {
			return &u.models[i].Window
		}
	}
	return nil
}

// usageLimit tells a subscription limit ("…limit… resets 3pm") from other 429s ("temporary capacity").
func usageLimit(msg string) bool {
	m := strings.ToLower(msg)
	return (strings.Contains(m, "limit") || strings.Contains(m, "лимит")) &&
		(strings.Contains(m, "reset") || strings.Contains(m, "сброс"))
}

func window(w *anthropic.Window) *api.Window {
	if w == nil {
		return nil
	}
	return &api.Window{Utilization: w.Utilization, ResetsAt: timePtr(w.ResetsAt)}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (d *Daemon) view(st store.State, a store.Account) api.Account {
	d.mu.Lock()
	var u acctUsage
	if p := d.usage[a.Name]; p != nil {
		u = *p
	}
	active := a.Name == st.Active && !d.unknown
	var leaves time.Time
	if active {
		leaves = u.leavesAt(d.th(), d.o.Now(), shiftStart(st))
	}
	d.mu.Unlock()
	v := api.Account{Name: a.Name, Email: a.Email, Priority: a.Priority, Enabled: a.Enabled, NeedsLogin: a.NeedsLogin,
		Active: active, Models: []api.ModelWindow{}, CheckedAt: timePtr(u.checkedAt), LeavesAt: timePtr(leaves),
		FiveHour: window(u.fiveHour), SevenDay: window(u.sevenDay)}
	s, now := stateOf(a, &u), d.o.Now()
	if r := policy.ExhaustedBy(s, d.th(), now); r != "" {
		v.Exhausted, v.ExhaustedReason, v.LeavesAt = true, &r, nil // leaving already
		if at, ok := policy.FreeAt(s, d.th(), now); ok {
			v.FreeAt = &at
		}
	}
	if u.err != "" {
		v.Error = &u.err
	}
	if !a.NeedsLogin {
		v.TokenExpiresAt, v.LoginExpiresAt, v.LoginEnding = timePtr(u.token), timePtr(u.login), ending(u.login, now)
	}
	for _, m := range u.models {
		v.Models = append(v.Models, api.ModelWindow{Name: m.Name, Utilization: m.Utilization, ResetsAt: timePtr(m.ResetsAt)})
	}
	return v
}

func (d *Daemon) viewOf(name string) (api.Account, error) {
	st := d.o.Store.State()
	a, ok := st.Find(name)
	if !ok {
		return api.Account{}, ErrNotFound
	}
	return d.view(st, a), nil
}

func (d *Daemon) Subscribe() (<-chan struct{}, func()) { return d.bus.subscribe() }

func (d *Daemon) Version() string { return d.o.Version }

func (d *Daemon) Status(context.Context) (api.Status, error) {
	st := d.o.Store.State()
	d.mu.Lock()
	unknown, last, activity, listen := d.unknown, d.last, d.activity, d.listen
	d.mu.Unlock()
	s := api.Status{Version: d.o.Version, Host: d.o.Host, Active: st.Active, Auto: st.Auto, Strategy: api.Strategy(strategyOf(st)),
		UnknownActive: unknown, TargetService: d.svc, LastPollAt: timePtr(last), LastActivityAt: timePtr(activity),
		LastSwitchAt: timePtr(st.LastSwitchAt), Shifts: shifts(st.Holders, d.o.Now()),
		Thresholds: thresholdsOf(d.th()), Remote: d.remoteStatus(), Update: d.newer()}
	if listen != "" {
		s.Listen = &listen
	}
	if unknown {
		s.Active = ""
	}
	states, now := d.states(st), d.o.Now()
	if st.Auto && s.Active != "" {
		if n := policy.Next(now, states, s.Active, d.th(), strategyOf(st)); n != "" {
			s.Next = &n
		}
	}
	usable := func(a policy.AccountState) bool {
		return a.Enabled && !a.NeedsLogin && !policy.Exhausted(a, d.th(), now)
	}
	if n := policy.FirstFree(now, states, s.Active, d.th()); n != "" && !slices.ContainsFunc(states, usable) {
		s.FirstFree = &n
	}
	return s, nil
}

func (d *Daemon) Accounts(context.Context) ([]api.Account, error) {
	st := d.o.Store.State()
	out := make([]api.Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		out = append(out, d.view(st, a))
	}
	return out, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (d *Daemon) Import(ctx context.Context, r api.ImportRequest) (api.Account, error) {
	defer d.bus.publish()
	a, err := d.im.Import(ctx, r.Name, r.ConfigDir, deref(r.Priority))
	if err != nil {
		return api.Account{}, err
	}
	d.requestTick()
	return d.viewOf(a.Name)
}

func (d *Daemon) Adopt(ctx context.Context, r api.AdoptRequest) (api.Account, error) {
	defer d.bus.publish()
	if !store.ValidName(r.Name) {
		return api.Account{}, fmt.Errorf("%w: %q", ErrInvalidName, r.Name)
	}
	prev := d.o.Store.State().Active
	err := d.o.Switcher.Adopt(ctx, r.Name, deref(r.Priority))
	if errors.Is(err, keychain.ErrNotFound) {
		err = ErrNoEntry
	}
	if err != nil {
		return api.Account{}, err
	}
	lost := false
	d.o.Switcher.Locked(func() error {
		d.claimHolderLocked(r.Name)
		lost = prev != "" && prev != r.Name && d.markLostLocked(prev) // the adopted login replaced prev's
		return nil
	})
	if lost {
		d.notify(loginAgain(prev))
	}
	d.requestTick()
	return d.viewOf(r.Name)
}

func (d *Daemon) Patch(_ context.Context, name string, p api.AccountPatch) (api.Account, error) {
	defer d.bus.publish()
	err := d.o.Switcher.Locked(func() error {
		return d.o.Store.Update(func(x *store.State) error {
			for i := range x.Accounts {
				if x.Accounts[i].Name == name {
					if p.Enabled != nil {
						x.Accounts[i].Enabled = *p.Enabled
					}
					if p.Priority != nil {
						x.Accounts[i].Priority = *p.Priority
					}
					return nil
				}
			}
			return ErrNotFound
		})
	})
	if err != nil {
		return api.Account{}, err
	}
	d.requestTick()
	return d.viewOf(name)
}

func (d *Daemon) Remove(_ context.Context, name string) error {
	defer d.bus.publish()
	return d.o.Switcher.Locked(func() error {
		err := d.o.Store.Update(func(x *store.State) error {
			i := slices.IndexFunc(x.Accounts, func(a store.Account) bool { return a.Name == name })
			switch {
			case i < 0:
				return ErrNotFound
			case x.Active == name:
				return ErrRemoveActive
			case d.o.Switcher.Frozen(name):
				return i18n.Err("daemon.migrating", switcher.ErrFrozen, name)
			}
			if err := d.o.Store.DeleteCreds(name); err != nil && !errors.Is(err, keychain.ErrNotFound) {
				return err
			}
			x.Accounts = slices.Delete(x.Accounts, i, i+1)
			return nil
		})
		if err == nil {
			d.forgetLocked(name)
		}
		return err
	})
}

// Activate is a manual switch; the switcher's non-fatal warning goes into Status.warning.
func (d *Daemon) Activate(ctx context.Context, name string, force bool) (api.Status, error) {
	defer d.bus.publish()
	if err := d.o.Switcher.Locked(func() error { return d.flushLocked(name) }); err != nil {
		return api.Status{}, err
	}
	warn, err := d.o.Switcher.Switch(ctx, name, force)
	if err != nil {
		return api.Status{}, err
	}
	d.claimHolder(name)
	d.requestTick() // the account now in use is asked at its own pace, at once if that allows
	s, _ := d.Status(ctx)
	s.Active = name // the switch landed even if its state write did not
	if warn != nil {
		logWarning("switch warning", name, warn)
		w := warn.Error()
		s.Warning = &w
	}
	return s, nil
}

func (d *Daemon) SetAuto(_ context.Context, on bool) error {
	defer d.bus.publish()
	if err := d.o.Store.Update(func(x *store.State) error { x.Auto = on; return nil }); err != nil {
		return err
	}
	if on {
		d.requestTick()
	}
	return nil
}

func (d *Daemon) SetStrategy(_ context.Context, s api.Strategy) error {
	defer d.bus.publish()
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidStrategy, s)
	}
	if err := d.o.Store.Update(func(x *store.State) error { x.Strategy = string(s); return nil }); err != nil {
		return err
	}
	d.requestTick()
	return nil
}

// SetLang pins the language of the daemon's messages and notifications in config.toml, or with "" lets
// its locale pick it, and speaks it from now on.
func (d *Daemon) SetLang(_ context.Context, lang string) error {
	defer d.bus.publish()
	pin, ok := i18n.Pin(lang)
	if !ok {
		return fmt.Errorf("%w: %q", ErrInvalidLang, lang)
	}
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if err := config.SaveLang(d.o.Home, pin); err != nil {
		return err
	}
	i18n.Set(i18n.Resolve(pin, os.Getenv))
	return nil
}

func thresholdsOf(th config.Thresholds) api.Thresholds {
	return api.Thresholds{FiveHour: th.FiveHour, SevenDay: th.Weekly, Hysteresis: th.Hysteresis,
		MinDwellSeconds: int(th.MinDwell / time.Second), CheckActiveSeconds: int(th.CheckActive / time.Second),
		CheckOthersSeconds: int(th.CheckOthers / time.Second)}
}

// SetThresholds saves t to config.toml, then switches by it from the next tick, which it requests.
func (d *Daemon) SetThresholds(_ context.Context, t api.Thresholds) (api.Thresholds, error) {
	defer d.bus.publish()
	secs := func(n int) time.Duration { return time.Duration(min(max(n, 0), 1<<30)) * time.Second }
	th := config.Thresholds{FiveHour: t.FiveHour, Weekly: t.SevenDay, Hysteresis: t.Hysteresis,
		MinDwell: secs(t.MinDwellSeconds), CheckActive: secs(t.CheckActiveSeconds), CheckOthers: secs(t.CheckOthersSeconds)}
	if t.MinDwellSeconds < 0 || t.MinDwellSeconds > 1<<30 {
		return api.Thresholds{}, fmt.Errorf("%w: min_dwell_seconds out of range", ErrInvalidThresholds)
	}
	if err := th.Check(); err != nil {
		return api.Thresholds{}, fmt.Errorf("%w: %v", ErrInvalidThresholds, err)
	}
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if err := config.SaveThresholds(d.o.Home, th); err != nil {
		return api.Thresholds{}, err
	}
	d.thr.Store(&th)
	slog.Info("thresholds changed", "five_hour", th.FiveHour, "weekly", th.Weekly, "hysteresis", th.Hysteresis, "min_dwell", th.MinDwell,
		"check_active", th.CheckActive, "check_others", th.CheckOthers)
	d.requestTick()
	return thresholdsOf(th), nil
}

// RefreshLogin refreshes name's login in the vault now, unless ccswap refreshed it under refreshFloor ago,
// and returns the account as it is then.
func (d *Daemon) RefreshLogin(ctx context.Context, name string) (api.Account, error) {
	defer d.bus.publish()
	st, holder := d.o.Store.State(), d.holderNow()
	a, ok := st.Find(name)
	switch {
	case !ok:
		return api.Account{}, ErrNotFound
	case a.NeedsLogin:
		return api.Account{}, i18n.Err("daemon.login_gone", switcher.ErrNeedsLogin, name)
	case st.Active == name || name == holder:
		return api.Account{}, ErrClaudeOwns
	case d.o.Switcher.Frozen(name):
		return api.Account{}, i18n.Err("daemon.migrating", switcher.ErrFrozen, name)
	}
	d.mu.Lock()
	recent := d.o.Now().Sub(d.acct(name).refreshed) < refreshFloor
	d.mu.Unlock()
	if !recent {
		var sk skipped
		switch err := d.refreshOne(d.tickCtx(ctx), name, holder, true); {
		case errors.As(err, &sk): // the state changed since the checks above, or ccswap keeps no login of it
			return api.Account{}, sk.error
		case errors.Is(err, switcher.ErrNeedsLogin), errors.Is(err, errSave):
			return api.Account{}, err
		case err != nil:
			return api.Account{}, ErrRefreshFailed
		}
	}
	return d.viewOf(name)
}

// Refresh runs a tick now (after the running one, if any) that asks the usage of every enabled
// account that needs no login, except one asked under refreshFloor ago or told to wait by a 429, and
// returns when it is done.
func (d *Daemon) Refresh(ctx context.Context) error {
	d.poll(d.tickCtx(ctx), true)
	return nil
}

// tickCtx is the daemon's run context, for the asks of an API request: API requests carry no
// cancellation, and ticks stop with the daemon.
func (d *Daemon) tickCtx(ctx context.Context) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runCtx != nil {
		return d.runCtx
	}
	return ctx
}
