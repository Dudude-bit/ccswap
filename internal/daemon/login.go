package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/store"
)

// A login made from the page: the daemon runs `claude auth login` in a login dir as `ccswap login`
// does, hands back the link it prints, and imports the login once the code its sign-in shows comes, or
// once claude logs in by itself, as a sign-in on this machine lets it.

var (
	loginTTL = 10 * time.Minute // a link with no code by then is dropped and its claude stopped
	// a start that cannot replace a link (the read key) waits this long after the account's last start:
	// each start runs a claude that may open a browser tab on this machine
	loginCooldown = 30 * time.Second
	linkWait      = 30 * time.Second // for claude to print its link
	codeWait      = 45 * time.Second // for claude to trade the code, under the page's 60 s
)

var linkRe = regexp.MustCompile(`https://\S+/oauth/authorize\?\S+`)

var (
	// ErrLoginGone: no link of the account with that id waits for a code.
	ErrLoginGone error = i18n.Err("daemon.login_link_gone")
	// ErrCodeRefused: claude got no login for the code; the link is spent.
	ErrCodeRefused error = i18n.Err("daemon.code_refused")
	// ErrEmptyCode: the page sent no code.
	ErrEmptyCode error = i18n.Err("daemon.code_empty")
	// ErrLoginFailed: claude auth login did not start, gave no link, or did not answer the code in time.
	ErrLoginFailed = errors.New("claude auth login failed")
	// ErrWrongAccount: the sign-in was another account's.
	ErrWrongAccount = errors.New("another account's login")
	// ErrLoginPending: a link of the account waits for its code, and the caller may not replace it.
	ErrLoginPending = errors.New("a login of the account is pending")
	// ErrLoginCooldown: the account's last start was under loginCooldown ago, and the caller may not replace.
	ErrLoginCooldown = errors.New("a login of the account started moments ago")
)

// pendingLogin is a `claude auth login` waiting for its code.
type pendingLogin struct {
	id      string // only who started it knows it: a code or a cancel must bring it
	dir     string
	expires time.Time
	stdin   io.WriteCloser
	stop    context.CancelFunc
	done    chan struct{} // closed once claude exits, with err its exit
	err     error
	taken   bool // FinishLogin has it, or claude's exit did; whoever took it imports the login and drops the dir; under Daemon.mu

	mu   sync.Mutex
	last string // claude's last line, for the log
}

func (p *pendingLogin) said() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func loginFailed(detail string) error { return i18n.Err("daemon.login_failed", ErrLoginFailed, detail) }

// StartLogin runs `claude auth login` for name and returns its link. A link of name that waits for its code
// makes it ErrLoginPending, unless replace: the new link then replaces it once it is up. Without replace, a
// start under loginCooldown after name's last one is ErrLoginCooldown.
func (d *Daemon) StartLogin(ctx context.Context, name string, replace bool) (api.LoginLink, error) {
	st := d.o.Store.State()
	if _, ok := st.Find(name); !ok {
		return api.LoginLink{}, ErrNotFound
	}
	if st.Active == name {
		return api.LoginLink{}, ErrActiveAccount
	}
	pctx, stop := context.WithTimeout(d.tickCtx(ctx), loginTTL)
	p := &pendingLogin{id: rand.Text(), expires: d.o.Now().Add(loginTTL), stop: stop, done: make(chan struct{})}
	// with no link waiting, p holds the account's slot before claude starts: a claude that quits at once
	// finds it to drop, and a second start spawns no claude
	now := d.o.Now()
	var refused error
	d.mu.Lock()
	prev, u := d.logins[name], d.acct(name)
	switch {
	case prev != nil && !replace:
		refused = i18n.Err("daemon.login_pending", ErrLoginPending, name, clockText(prev.expires, now))
	case !replace && now.Sub(u.loginStarted) < loginCooldown:
		refused = i18n.Err("daemon.login_cooldown", ErrLoginCooldown, name, u.loginStarted.Add(loginCooldown).Local().Format("15:04:05"))
	default:
		u.loginStarted = now
		if prev == nil {
			d.logins[name] = p
		}
	}
	d.mu.Unlock()
	if refused != nil {
		stop()
		return api.LoginLink{}, refused
	}
	dir, err := os.MkdirTemp(d.o.Home, "login-*")
	if err != nil {
		d.dropLogin(name, p)
		return api.LoginLink{}, err
	}
	p.dir = dir
	cmd := exec.CommandContext(pctx, claudecode.Bin(), "auth", "login")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = claudecode.LoginEnv(dir)
	r, w, err := os.Pipe()
	if err == nil {
		cmd.Stdout, cmd.Stderr = w, w
		p.stdin, err = cmd.StdinPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if w != nil {
		w.Close()
	}
	if err != nil {
		d.dropLogin(name, p)
		if r != nil {
			r.Close()
		}
		DropLogin(dir, d.im.Target, d.o.KC, false)
		detail := err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			detail = i18n.T("daemon.claude_not_found")
		}
		return api.LoginLink{}, loginFailed(detail)
	}
	links := make(chan string, 1)
	go func() {
		defer r.Close()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			p.mu.Lock()
			if line != "" {
				p.last = line
			}
			p.mu.Unlock()
			if l := linkRe.FindString(line); l != "" {
				select {
				case links <- l:
				default:
				}
			}
		}
	}()
	go func() {
		p.err = cmd.Wait()
		close(p.done)
		stop()
		d.mu.Lock()
		if d.logins[name] == p {
			delete(d.logins, name)
		}
		taken := p.taken
		p.taken = true
		d.mu.Unlock()
		if taken {
			return
		}
		imported := false
		if p.err == nil { // logged in with no code: signed in on this machine, in the tab claude opened
			_, err := d.renew(context.WithoutCancel(pctx), name, dir)
			imported = err == nil
			if err != nil && !errors.Is(err, ErrLoginIncomplete) {
				slog.Warn("login: the login claude got is not kept", "account", name, "err", err)
			}
		}
		DropLogin(dir, d.im.Target, d.o.KC, imported)
	}()
	t := time.NewTimer(linkWait)
	defer t.Stop()
	select {
	case l := <-links:
		// p takes the slot unless it stopped meanwhile, or another start with replace took it from p
		d.mu.Lock()
		cur := d.logins[name]
		up := pctx.Err() == nil && !p.taken && (cur == p || replace)
		if up && cur != p {
			d.logins[name] = p
			if cur != nil {
				cur.stop()
			}
		}
		d.mu.Unlock()
		if up {
			return api.LoginLink{Id: p.id, Url: l, ExpiresAt: p.expires}, nil
		}
		d.dropLogin(name, p)
		return api.LoginLink{}, ErrLoginGone
	case <-p.done:
		detail := p.said()
		if detail == "" && p.err != nil {
			detail = p.err.Error()
		}
		return api.LoginLink{}, loginFailed(detail)
	case <-t.C:
		d.dropLogin(name, p)
		return api.LoginLink{}, loginFailed(i18n.T("daemon.login_no_link"))
	}
}

// FinishLogin hands the claude of name's link id the code and imports the login it gets. The link is spent
// either way.
func (d *Daemon) FinishLogin(ctx context.Context, name, id, code string) (api.Account, error) {
	code = claudecode.AuthCode(code)
	if code == "" {
		return api.Account{}, ErrEmptyCode
	}
	d.mu.Lock()
	p := d.logins[name]
	if p != nil && p.id == id {
		delete(d.logins, name)
		p.taken = true
	} else {
		p = nil
	}
	d.mu.Unlock()
	if p == nil {
		return api.Account{}, ErrLoginGone
	}
	imported := false
	defer func() {
		p.stop()
		<-p.done
		DropLogin(p.dir, d.im.Target, d.o.KC, imported)
	}()
	sent := false
	select {
	case <-p.done: // it ended just now: expired, or logged in with no code
	default:
		_, _ = io.WriteString(p.stdin, code+"\n")
		p.stdin.Close()
		sent = true
		t := time.NewTimer(codeWait)
		defer t.Stop()
		select {
		case <-p.done:
		case <-t.C:
			return api.Account{}, loginFailed(i18n.T("daemon.login_no_answer"))
		}
	}
	switch {
	case p.err != nil && !sent:
		return api.Account{}, ErrLoginGone
	case p.err != nil:
		slog.Warn("login: claude took no login for the code", "account", name, "claude", p.said(), "err", p.err)
		return api.Account{}, ErrCodeRefused
	}
	a, err := d.renew(context.WithoutCancel(ctx), name, p.dir) // the code is spent: keep its login whatever the page does
	imported = err == nil
	return a, err
}

// renew stores the login in dir as name's new one. It never adds an account: the read key, which may
// start a login, must not.
func (d *Daemon) renew(ctx context.Context, name, dir string) (api.Account, error) {
	defer d.bus.publish()
	_, err := d.im.Renew(ctx, name, dir)
	if errors.Is(err, store.ErrDuplicate) {
		email := ""
		if x, ok := d.o.Store.State().Find(name); ok {
			email = x.Email
		}
		return api.Account{}, i18n.Err("daemon.wrong_account", ErrWrongAccount, name, email)
	}
	if err != nil {
		return api.Account{}, err
	}
	slog.Info("login: renewed from the page", "account", name)
	d.requestTick()
	return d.viewOf(name)
}

// CancelLogin drops name's link id and stops its claude, if there is one.
func (d *Daemon) CancelLogin(_ context.Context, name, id string) error {
	d.mu.Lock()
	p := d.logins[name]
	d.mu.Unlock()
	if p != nil && p.id == id {
		d.dropLogin(name, p)
	}
	return nil
}

// dropLogin stops p, name's link unless someone took it; claude's exit drops the dir.
func (d *Daemon) dropLogin(name string, p *pendingLogin) {
	d.mu.Lock()
	if d.logins[name] == p {
		delete(d.logins, name)
	}
	taken := p.taken
	d.mu.Unlock()
	if !taken {
		p.stop()
	}
}
