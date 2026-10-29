package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/t3"
	"github.com/Dudude-bit/ccswap/internal/web"
)

// Core is what the API needs from the daemon.
type Core interface {
	Status(ctx context.Context) (api.Status, error)
	Accounts(ctx context.Context) ([]api.Account, error)
	Import(ctx context.Context, r api.ImportRequest) (api.Account, error)
	Adopt(ctx context.Context, r api.AdoptRequest) (api.Account, error)
	Patch(ctx context.Context, name string, p api.AccountPatch) (api.Account, error)
	Remove(ctx context.Context, name string) error
	Activate(ctx context.Context, name string, force bool) (api.Status, error)
	SetAuto(ctx context.Context, enabled bool) error
	SetThresholds(ctx context.Context, t api.Thresholds) (api.Thresholds, error)
	SetStrategy(ctx context.Context, s api.Strategy) error
	SetLang(ctx context.Context, lang string) error
	SetRemote(ctx context.Context, r api.RemoteRequest) error
	// Subscribe nudges the returned channel whenever status or accounts may have changed and
	// closes it when the daemon stops.
	Subscribe() (changed <-chan struct{}, cancel func())
	Refresh(ctx context.Context) error
	// RefreshLogin refreshes one account's login in the vault now.
	RefreshLogin(ctx context.Context, name string) (api.Account, error)
	// StartLogin runs claude auth login for an account and returns its sign-in link; FinishLogin hands
	// it the code the sign-in showed and stores the login; CancelLogin drops the link.
	StartLogin(ctx context.Context, name string) (api.LoginLink, error)
	FinishLogin(ctx context.Context, name, code string) (api.Account, error)
	CancelLogin(ctx context.Context, name string) error
	Signal(ctx context.Context, payload map[string]any) error
	// WebCode mints a one-time code that the TCP listener trades for its admin key; UseWebCode takes
	// it, once.
	WebCode(ctx context.Context) (api.WebCode, error)
	UseWebCode(ctx context.Context, code string) error
	// MigrateExport packs accounts into a bundle and freezes them until MigrateCommit removes them here
	// or MigrateCancel thaws them; MigrateImport adds the accounts of a bundle. Unix socket only.
	MigrateExport(ctx context.Context, names []string) (api.MigrateExport, error)
	MigrateCommit(ctx context.Context, token string) error
	MigrateCancel(ctx context.Context, token string) error
	MigrateImport(ctx context.Context, r api.MigrateImportRequest) (api.MigrateImport, error)
	// Version is the build the daemon runs.
	Version() string
}

type strict struct{ c Core }

func (s strict) GetStatus(ctx context.Context, _ api.GetStatusRequestObject) (api.GetStatusResponseObject, error) {
	st, err := s.c.Status(ctx)
	return api.GetStatus200JSONResponse(st), err
}

func (s strict) ListAccounts(ctx context.Context, _ api.ListAccountsRequestObject) (api.ListAccountsResponseObject, error) {
	a, err := s.c.Accounts(ctx)
	if a == nil {
		a = []api.Account{}
	}
	return api.ListAccounts200JSONResponse(a), err
}

func (s strict) ImportAccount(ctx context.Context, r api.ImportAccountRequestObject) (api.ImportAccountResponseObject, error) {
	a, err := s.c.Import(ctx, *r.Body)
	return api.ImportAccount200JSONResponse(a), err
}

func (s strict) AdoptAccount(ctx context.Context, r api.AdoptAccountRequestObject) (api.AdoptAccountResponseObject, error) {
	a, err := s.c.Adopt(ctx, *r.Body)
	return api.AdoptAccount200JSONResponse(a), err
}

func (s strict) PatchAccount(ctx context.Context, r api.PatchAccountRequestObject) (api.PatchAccountResponseObject, error) {
	a, err := s.c.Patch(ctx, r.Name, *r.Body)
	return api.PatchAccount200JSONResponse(a), err
}

func (s strict) RemoveAccount(ctx context.Context, r api.RemoveAccountRequestObject) (api.RemoveAccountResponseObject, error) {
	return api.RemoveAccount204Response{}, s.c.Remove(ctx, r.Name)
}

func (s strict) ActivateAccount(ctx context.Context, r api.ActivateAccountRequestObject) (api.ActivateAccountResponseObject, error) {
	st, err := s.c.Activate(ctx, r.Name, r.Params.Force != nil && *r.Params.Force)
	return api.ActivateAccount200JSONResponse(st), err
}

func (s strict) SetAuto(ctx context.Context, r api.SetAutoRequestObject) (api.SetAutoResponseObject, error) {
	return api.SetAuto204Response{}, s.c.SetAuto(ctx, r.Body.Enabled)
}

func (s strict) SetStrategy(ctx context.Context, r api.SetStrategyRequestObject) (api.SetStrategyResponseObject, error) {
	return api.SetStrategy204Response{}, s.c.SetStrategy(ctx, r.Body.Strategy)
}

func (s strict) SetLang(ctx context.Context, r api.SetLangRequestObject) (api.SetLangResponseObject, error) {
	return api.SetLang204Response{}, s.c.SetLang(ctx, r.Body.Lang)
}

func (s strict) SetRemote(ctx context.Context, r api.SetRemoteRequestObject) (api.SetRemoteResponseObject, error) {
	return api.SetRemote204Response{}, s.c.SetRemote(ctx, *r.Body)
}

func (s strict) SetThresholds(ctx context.Context, r api.SetThresholdsRequestObject) (api.SetThresholdsResponseObject, error) {
	t, err := s.c.SetThresholds(ctx, *r.Body)
	return api.SetThresholds200JSONResponse(t), err
}

func (s strict) Refresh(ctx context.Context, _ api.RefreshRequestObject) (api.RefreshResponseObject, error) {
	return api.Refresh204Response{}, s.c.Refresh(ctx)
}

func (s strict) RefreshLogin(ctx context.Context, r api.RefreshLoginRequestObject) (api.RefreshLoginResponseObject, error) {
	a, err := s.c.RefreshLogin(ctx, r.Name)
	return api.RefreshLogin200JSONResponse(a), err
}

func (s strict) StartLogin(ctx context.Context, r api.StartLoginRequestObject) (api.StartLoginResponseObject, error) {
	l, err := s.c.StartLogin(ctx, r.Name)
	return api.StartLogin200JSONResponse(l), err
}

func (s strict) FinishLogin(ctx context.Context, r api.FinishLoginRequestObject) (api.FinishLoginResponseObject, error) {
	a, err := s.c.FinishLogin(ctx, r.Name, r.Body.Code)
	return api.FinishLogin200JSONResponse(a), err
}

func (s strict) CancelLogin(ctx context.Context, r api.CancelLoginRequestObject) (api.CancelLoginResponseObject, error) {
	return api.CancelLogin204Response{}, s.c.CancelLogin(ctx, r.Name)
}

func (s strict) Signal(ctx context.Context, r api.SignalRequestObject) (api.SignalResponseObject, error) {
	return api.Signal204Response{}, s.c.Signal(ctx, r.Body.Payload)
}

func (s strict) CreateWebCode(ctx context.Context, _ api.CreateWebCodeRequestObject) (api.CreateWebCodeResponseObject, error) {
	c, err := s.c.WebCode(ctx)
	return api.CreateWebCode200JSONResponse(c), err
}

func (s strict) MigrateExport(ctx context.Context, r api.MigrateExportRequestObject) (api.MigrateExportResponseObject, error) {
	x, err := s.c.MigrateExport(ctx, r.Body.Names)
	return api.MigrateExport200JSONResponse(x), err
}

func (s strict) MigrateCommit(ctx context.Context, r api.MigrateCommitRequestObject) (api.MigrateCommitResponseObject, error) {
	return api.MigrateCommit204Response{}, s.c.MigrateCommit(ctx, r.Body.Token)
}

func (s strict) MigrateCancel(ctx context.Context, r api.MigrateCancelRequestObject) (api.MigrateCancelResponseObject, error) {
	return api.MigrateCancel204Response{}, s.c.MigrateCancel(ctx, r.Body.Token)
}

func (s strict) MigrateImport(ctx context.Context, r api.MigrateImportRequestObject) (api.MigrateImportResponseObject, error) {
	x, err := s.c.MigrateImport(ctx, *r.Body)
	return api.MigrateImport200JSONResponse(x), err
}

// OpenWebSession hands out the admin key the TCP listener holds, which the unix socket does not know.
func (s strict) OpenWebSession(context.Context, api.OpenWebSessionRequestObject) (api.OpenWebSessionResponseObject, error) {
	return nil, fiber.NewError(fiber.StatusNotFound, i18n.T("daemon.web_session_tcp_only"))
}

// GetI18n is the page's texts in the language matching a tag; the TCP listener serves them without a key.
// A browser keeps them and asks again with their ETag, which changes with the build and the texts.
func (s strict) GetI18n(_ context.Context, r api.GetI18nRequestObject) (api.GetI18nResponseObject, error) {
	l, ok := i18n.Match(r.Lang)
	if !ok {
		return api.GetI18n404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Code: "not_found", Message: i18n.T("daemon.no_lang", r.Lang)}}, nil
	}
	langs := []api.Language{}
	for _, a := range i18n.Available() {
		langs = append(langs, api.Language{Code: string(a), Name: i18n.Name(a)})
	}
	cat := api.Catalog{Lang: string(l), Name: i18n.Name(l), Languages: langs, Messages: i18n.Web(l)}
	b, err := json.Marshal(cat)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte(s.c.Version()+"\n"), b...))
	etag := fmt.Sprintf(`"%x"`, sum[:12])
	if r.Params.IfNoneMatch != nil && etagIn(*r.Params.IfNoneMatch, etag) {
		return api.GetI18n304Response{Headers: api.GetI18n304ResponseHeaders{CacheControl: "no-cache", ETag: etag}}, nil
	}
	return api.GetI18n200JSONResponse{Body: cat, Headers: api.GetI18n200ResponseHeaders{CacheControl: "no-cache", ETag: etag}}, nil
}

// etagIn reports whether an If-None-Match list names etag; a weak tag counts, as GET compares weakly.
func etagIn(list, etag string) bool {
	for t := range strings.SplitSeq(list, ",") {
		if t = strings.TrimPrefix(strings.TrimSpace(t), "W/"); t == "*" || t == etag {
			return true
		}
	}
	return false
}

// classify maps an error to an HTTP status and API error. Unknown errors are logged, never echoed.
func classify(err error) (int, api.Error) {
	var fe *fiber.Error
	switch {
	case errors.As(err, &fe):
		return fe.Code, api.Error{Code: "bad_request", Message: fe.Message}
	case errors.Is(err, ErrInvalidName):
		return 400, api.Error{Code: "invalid_name", Message: err.Error()}
	case errors.Is(err, ErrInvalidThresholds):
		return 400, api.Error{Code: "invalid_thresholds", Message: err.Error()}
	case errors.Is(err, ErrInvalidStrategy):
		return 400, api.Error{Code: "invalid_strategy", Message: err.Error()}
	case errors.Is(err, ErrInvalidLang):
		return 400, api.Error{Code: "invalid_lang", Message: err.Error()}
	case errors.Is(err, ErrInvalidRemote):
		return 400, api.Error{Code: "invalid_remote", Message: err.Error()}
	case errors.Is(err, ErrInvalidConfigDir):
		return 400, api.Error{Code: "invalid_config_dir", Message: err.Error()}
	case errors.Is(err, ErrLoginIncomplete):
		return 400, api.Error{Code: "login_incomplete", Message: err.Error()}
	case errors.Is(err, ErrEmptyCode):
		return 400, api.Error{Code: "bad_request", Message: err.Error()}
	case errors.Is(err, ErrCodeRefused):
		return 400, api.Error{Code: "code_refused", Message: err.Error()}
	case errors.Is(err, migrate.ErrNotBundle):
		return 400, api.Error{Code: "not_bundle", Message: i18n.T("daemon.not_bundle")}
	case errors.Is(err, migrate.ErrNewerVersion):
		return 400, api.Error{Code: "bundle_version", Message: i18n.T("daemon.bundle_version")}
	case errors.Is(err, migrate.ErrCodeFormat):
		return 400, api.Error{Code: "invalid_code", Message: i18n.T("daemon.invalid_code")}
	case errors.Is(err, migrate.ErrBadCode):
		return 400, api.Error{Code: "bad_bundle", Message: i18n.T("daemon.bad_bundle")}
	case errors.Is(err, ErrNoEntry):
		return 404, api.Error{Code: "no_entry", Message: err.Error()}
	case errors.Is(err, ErrNotFound), errors.Is(err, switcher.ErrNoSuchAccount):
		return 404, api.Error{Code: "not_found", Message: err.Error()}
	case errors.Is(err, store.ErrDuplicate):
		return 409, api.Error{Code: "duplicate", Message: err.Error()}
	case errors.Is(err, ErrRemoveActive):
		return 409, api.Error{Code: "remove_active", Message: err.Error()}
	case errors.Is(err, ErrActiveAccount):
		return 409, api.Error{Code: "active_account", Message: err.Error()}
	case errors.Is(err, ErrClaudeOwns):
		return 409, api.Error{Code: "claude_owns", Message: err.Error()}
	case errors.Is(err, ErrLoginGone):
		return 409, api.Error{Code: "login_gone", Message: err.Error()}
	case errors.Is(err, ErrWrongAccount):
		return 409, api.Error{Code: "wrong_account", Message: err.Error()}
	case errors.Is(err, switcher.ErrNearRefresh):
		return 409, api.Error{Code: "near_refresh", Message: err.Error()}
	case errors.Is(err, switcher.ErrTargetRefreshing):
		return 409, api.Error{Code: "target_refreshing", Message: err.Error()}
	case errors.Is(err, switcher.ErrEntryChanged):
		return 409, api.Error{Code: "entry_changed", Message: err.Error()}
	case errors.Is(err, switcher.ErrUnknownActive):
		return 409, api.Error{Code: "unknown_active", Message: err.Error()}
	case errors.Is(err, switcher.ErrEntryExpired):
		return 409, api.Error{Code: "entry_expired", Message: err.Error()}
	case errors.Is(err, switcher.ErrCannotIdentify):
		slog.Warn("api: cannot identify login", "err", err)
		return 409, api.Error{Code: "cannot_identify", Message: err.Error()}
	case errors.Is(err, switcher.ErrNeedsLogin):
		return 409, api.Error{Code: "needs_login", Message: err.Error()}
	case errors.Is(err, switcher.ErrFrozen):
		return 409, api.Error{Code: "migrating", Message: err.Error()}
	case errors.Is(err, ErrMigrateActive):
		return 409, api.Error{Code: "migrate_active", Message: err.Error()}
	case errors.Is(err, ErrExportGone):
		return 409, api.Error{Code: "export_gone", Message: err.Error()}
	case errors.Is(err, ErrExportDone):
		return 409, api.Error{Code: "export_done", Message: err.Error()}
	case errors.Is(err, ErrTokensChanged):
		return 409, api.Error{Code: "tokens_changed", Message: err.Error()}
	case errors.Is(err, ErrBundleSpent):
		return 409, api.Error{Code: "bundle_spent", Message: err.Error()}
	case errors.Is(err, ErrBundleUnchecked):
		return 502, api.Error{Code: "bundle_unchecked", Message: err.Error()}
	case errors.Is(err, ErrRefreshFailed):
		return 502, api.Error{Code: "refresh_failed", Message: err.Error()}
	case errors.Is(err, ErrLoginFailed):
		slog.Warn("api: login failed", "err", err)
		return 502, api.Error{Code: "login_failed", Message: err.Error()}
	case errors.Is(err, ErrBadCode):
		return 403, api.Error{Code: "bad_code", Message: err.Error()}
	}
	slog.Error("api: internal error", "err", err)
	return 500, api.Error{Code: "internal", Message: "internal error"}
}

func errorJSON(c fiber.Ctx, err error) error {
	code, e := classify(err)
	return c.Status(code).JSON(e)
}

func newApp(core Core) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: errorJSON})
	api.RegisterHandlers(app, api.NewStrictHandler(strict{core}, nil))
	return app
}

// access is what the TCP listener asks of a request to one of its routes.
type access int

const (
	readKey  access = iota // the read key (T3 Code, ccswap remote, the page's view) or the admin key
	adminKey               // the admin key, and a listener on a loopback address
	codeOnly               // no key, a one-time code from `ccswap web` in the body; a loopback listener
	anyKey                 // either key, on any listener and Host: a new login, which only the account's own sign-in finishes
	noKey                  // nothing: the page's texts, which its login screen needs first and which hold no secret
)

// tcpRoutes is the part of the API the TCP listener serves, by what each route asks for; the rest
// (login, adopt, remove, the hook signal, remote, web codes, migrate) is served on the unix socket only.
var tcpRoutes = map[string]access{
	"GET /v1/status":                     readKey,
	"GET /v1/accounts":                   readKey,
	"GET /v1/events":                     readKey,
	"PATCH /v1/accounts/:name":           adminKey,
	"POST /v1/accounts/:name/activate":   adminKey,
	"POST /v1/accounts/:name/refresh":    adminKey,
	"POST /v1/accounts/:name/login":      anyKey,
	"DELETE /v1/accounts/:name/login":    anyKey,
	"POST /v1/accounts/:name/login/code": anyKey,
	"PUT /v1/auto":                       adminKey,
	"PUT /v1/strategy":                   adminKey,
	"PUT /v1/thresholds":                 adminKey,
	"POST /v1/refresh":                   adminKey,
	"POST /v1/web/session":               codeOnly,
	"GET /v1/i18n/:lang":                 noKey,
}

// httpKeys are the TCP listener's keys; local: it listens on a loopback address.
type httpKeys struct {
	read, admin string
	local       bool
}

// gate lets a request through to a route that asks for a. A change needs the admin key on a loopback
// listener: the read key in T3 Code or another ccswap, and a listener open to the network, only look.
// A change must also name a loopback host, so a site whose name a DNS rebinding pointed at
// 127.0.0.1 cannot trade a code or use a key it got. A new login is the exception: the store keeps it
// only as the account's own email's, so whoever holds a key can at most start one the owner finishes.
func (k httpKeys) gate(a access) fiber.Handler {
	return func(c fiber.Ctx) error {
		admin := t3.Carries(c, k.admin)
		switch {
		case a == noKey:
		case a != codeOnly && !admin && !t3.Carries(c, k.read):
			return c.Status(fiber.StatusUnauthorized).JSON(api.Error{Code: "unauthorized", Message: i18n.T("daemon.bad_key")})
		case a != readKey && a != anyKey && (!k.local || !loopbackHost(string(c.Request().Host()))):
			return c.Status(fiber.StatusForbidden).JSON(api.Error{Code: "local_only", Message: i18n.T("daemon.settings_local_only")})
		case a == adminKey && !admin:
			return c.Status(fiber.StatusForbidden).JSON(api.Error{Code: "read_only", Message: i18n.T("daemon.read_only_key")})
		}
		return c.Next()
	}
}

// loopbackHost reports whether a Host header names a loopback address or localhost, with any port.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

// tcpRouter takes the generated routes and registers those tcpRoutes names, each behind its gate.
type tcpRouter struct {
	fiber.Router
	keys httpKeys
	seen map[string]bool
}

func (r tcpRouter) add(method, path string, h any, hs []any) fiber.Router {
	route := method + " " + path
	if a, ok := tcpRoutes[route]; ok {
		r.seen[route] = true
		r.Router.Add([]string{method}, path, r.keys.gate(a), append([]any{h}, hs...)...)
	}
	return r
}

func (r tcpRouter) Get(p string, h any, hs ...any) fiber.Router {
	return r.add(fiber.MethodGet, p, h, hs)
}
func (r tcpRouter) Post(p string, h any, hs ...any) fiber.Router {
	return r.add(fiber.MethodPost, p, h, hs)
}
func (r tcpRouter) Put(p string, h any, hs ...any) fiber.Router {
	return r.add(fiber.MethodPut, p, h, hs)
}
func (r tcpRouter) Patch(p string, h any, hs ...any) fiber.Router {
	return r.add(fiber.MethodPatch, p, h, hs)
}
func (r tcpRouter) Delete(p string, h any, hs ...any) fiber.Router {
	return r.add(fiber.MethodDelete, p, h, hs)
}

// tcpStrict is the API as the TCP listener serves it: a switch is never forced there, as forcing
// one may undo the token refresh Claude Code is about to make; admin is the listener's admin key.
type tcpStrict struct {
	strict
	admin string
}

func (s tcpStrict) ActivateAccount(ctx context.Context, r api.ActivateAccountRequestObject) (api.ActivateAccountResponseObject, error) {
	if r.Params.Force != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, i18n.T("daemon.force_socket_only"))
	}
	return s.strict.ActivateAccount(ctx, r)
}

// OpenWebSession trades a code from `ccswap web` for the admin key this listener checks.
func (s tcpStrict) OpenWebSession(ctx context.Context, r api.OpenWebSessionRequestObject) (api.OpenWebSessionResponseObject, error) {
	if err := s.c.UseWebCode(ctx, r.Body.Code); err != nil {
		return nil, err
	}
	return api.OpenWebSession200JSONResponse{Key: s.admin}, nil
}

// newHTTPApp is the TCP listener: the web page, the tcpRoutes part of the API and the T3 Code hub.
// No WriteTimeout: /v1/events streams for as long as the page is open.
func newHTTPApp(core Core, hub t3.Source, k httpKeys) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: errorJSON, ReadTimeout: 10 * time.Second, IdleTimeout: time.Minute})
	web.Register(app)
	r := tcpRouter{app, k, map[string]bool{}}
	api.RegisterHandlers(r, api.NewStrictHandler(tcpStrict{strict{core}, k.admin}, nil))
	if len(r.seen) != len(tcpRoutes) {
		panic("tcpRoutes names a route the API does not have")
	}
	app.Use(t3.Auth(k.read, k.admin))
	t3.Register(app, hub)
	return app
}

// Serve answers the API on a unix socket until ctx is cancelled, holding the flock on lockPath.
func Serve(ctx context.Context, socketPath, lockPath string, core Core) error {
	ln, release, err := listen(socketPath, lockPath)
	if err != nil {
		return err
	}
	defer release()
	return serve(ctx, ln, core)
}

// ErrAlreadyRunning: another daemon holds the lock or answers on the socket.
var ErrAlreadyRunning error = i18n.Err("daemon.already_running")

// ExitAlreadyRunning is the exit status of `ccswap daemon run` that finds another daemon; the systemd
// unit does not restart on it, or it would try again every few seconds for as long as the other runs.
const ExitAlreadyRunning = 3

// ExitConfig is the exit status of `ccswap daemon run` that cannot start from its files and folders:
// $CCSWAP_HOME is relative, ccswap's folder or the user's home folder cannot be found, ccswap's folder
// cannot be created or made 0700, it is too long a path for the socket, or config.toml or state.json
// does not load. Every start fails the same way, so the systemd unit does not restart on it either.
// It is EX_CONFIG of sysexits.h, which systemd shows as 78/CONFIG.
const ExitConfig = 78

// listen claims socketPath for this daemon: an exclusive flock on lockPath held until release,
// then the socket. A leftover socket file is unlinked only when dialing it is refused or finds
// nothing; any other dial error means it may be live, so the daemon refuses to start.
func listen(socketPath, lockPath string) (net.Listener, func(), error) {
	if lockPath == "" {
		return nil, nil, errors.New("no daemon lock path: home directory unknown")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, nil, err
	}
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (net.Listener, func(), error) {
		lf.Close()
		return nil, nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fail(i18n.Err("daemon.another_daemon_holds", ErrAlreadyRunning, lockPath))
		}
		return fail(fmt.Errorf("lock %s: %w", lockPath, err))
	}
	c, err := net.DialTimeout("unix", socketPath, time.Second)
	switch {
	case err == nil:
		c.Close()
		return fail(i18n.Err("daemon.another_daemon_answers", ErrAlreadyRunning, socketPath))
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOENT):
		if err := removeStaleSocket(socketPath); err != nil {
			return fail(err)
		}
	default:
		return fail(i18n.Err("daemon.socket_unknown", socketPath, err))
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return fail(err)
	}
	return ln, func() {
		ln.Close()
		os.Remove(socketPath)
		lf.Close() // releases the flock
	}, nil
}

// removeStaleSocket unlinks a socket nobody answers on. ECONNREFUSED is also what Linux returns
// for a regular file, so anything but a socket (or nothing) is refused.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return i18n.Err("daemon.socket_not_socket", path, fi.Mode().Type())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func serve(ctx context.Context, ln net.Listener, core Core) error {
	app := newApp(core)
	done := make(chan error, 1)
	go func() { done <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := app.ShutdownWithContext(sctx)
		ln.Close() // a server cancelled before it started accepting would block forever
		<-done
		return err
	}
}
