package daemon

import (
	"bufio"
	"context"
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
// does, hands back the link it prints, and imports the login once the code its sign-in shows comes.

var (
	loginTTL = 10 * time.Minute // a link with no code by then is dropped and its claude stopped
	linkWait = 30 * time.Second // for claude to print its link
	codeWait = 45 * time.Second // for claude to trade the code, under the page's 60 s
)

var linkRe = regexp.MustCompile(`https://\S+/oauth/authorize\?\S+`)

var (
	// ErrLoginGone: no link of the account waits for a code.
	ErrLoginGone error = i18n.Err("daemon.login_link_gone")
	// ErrCodeRefused: claude got no login for the code; the link is spent.
	ErrCodeRefused error = i18n.Err("daemon.code_refused")
	// ErrEmptyCode: the page sent no code.
	ErrEmptyCode error = i18n.Err("daemon.code_empty")
	// ErrLoginFailed: claude auth login did not start, gave no link, or did not answer the code in time.
	ErrLoginFailed = errors.New("claude auth login failed")
	// ErrWrongAccount: the sign-in was another account's.
	ErrWrongAccount = errors.New("another account's login")
)

// pendingLogin is a `claude auth login` waiting for its code.
type pendingLogin struct {
	dir   string
	stdin io.WriteCloser
	stop  context.CancelFunc
	done  chan struct{} // closed once claude exits, with err its exit
	err   error
	taken bool // FinishLogin has it, and drops the dir; under Daemon.mu

	mu   sync.Mutex
	last string // claude's last line, for the log
}

func (p *pendingLogin) said() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func loginFailed(detail string) error { return i18n.Err("daemon.login_failed", ErrLoginFailed, detail) }

// StartLogin runs `claude auth login` for name and returns its link; a link name had is dropped.
func (d *Daemon) StartLogin(ctx context.Context, name string) (api.LoginLink, error) {
	st := d.o.Store.State()
	if _, ok := st.Find(name); !ok {
		return api.LoginLink{}, ErrNotFound
	}
	if st.Active == name {
		return api.LoginLink{}, ErrActiveAccount
	}
	dir, err := os.MkdirTemp(d.o.Home, "login-*")
	if err != nil {
		return api.LoginLink{}, err
	}
	pctx, stop := context.WithTimeout(d.tickCtx(ctx), loginTTL)
	p := &pendingLogin{dir: dir, stop: stop, done: make(chan struct{})}
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
		stop()
		if r != nil {
			r.Close()
		}
		DropLogin(dir, d.im.Target, d.o.KC, false)
		return api.LoginLink{}, loginFailed(err.Error())
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
		d.mu.Unlock()
		if !taken {
			DropLogin(dir, d.im.Target, d.o.KC, false)
		}
	}()
	d.mu.Lock()
	prev := d.logins[name]
	d.logins[name] = p
	d.mu.Unlock()
	if prev != nil {
		prev.stop()
	}
	t := time.NewTimer(linkWait)
	defer t.Stop()
	select {
	case l := <-links:
		return api.LoginLink{Url: l, ExpiresAt: d.o.Now().Add(loginTTL)}, nil
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

// FinishLogin hands name's claude the code and imports the login it gets. The link is spent either way.
func (d *Daemon) FinishLogin(ctx context.Context, name, code string) (api.Account, error) {
	code = claudecode.AuthCode(code)
	if code == "" {
		return api.Account{}, ErrEmptyCode
	}
	d.mu.Lock()
	p := d.logins[name]
	if p != nil {
		delete(d.logins, name)
		p.taken = true
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
	select {
	case <-p.done: // expired just now
		return api.Account{}, ErrLoginGone
	default:
	}
	_, _ = io.WriteString(p.stdin, code+"\n")
	p.stdin.Close()
	t := time.NewTimer(codeWait)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
		return api.Account{}, loginFailed(i18n.T("daemon.login_no_answer"))
	}
	if p.err != nil {
		slog.Warn("login: claude took no login for the code", "account", name, "claude", p.said(), "err", p.err)
		return api.Account{}, ErrCodeRefused
	}
	a, err := d.Import(context.WithoutCancel(ctx), api.ImportRequest{Name: name, ConfigDir: p.dir}) // the code is spent: keep its login whatever the page does
	imported = err == nil
	if errors.Is(err, store.ErrDuplicate) {
		email := ""
		if x, ok := d.o.Store.State().Find(name); ok {
			email = x.Email
		}
		return api.Account{}, i18n.Err("daemon.wrong_account", ErrWrongAccount, name, email)
	}
	if err == nil {
		slog.Info("login: renewed from the page", "account", name)
	}
	return a, err
}

// CancelLogin drops name's link and stops its claude, if there is one.
func (d *Daemon) CancelLogin(_ context.Context, name string) error {
	d.mu.Lock()
	p := d.logins[name]
	d.mu.Unlock()
	if p != nil {
		d.dropLogin(name, p)
	}
	return nil
}

// dropLogin stops p, name's link unless FinishLogin took it; claude's exit drops the dir.
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
