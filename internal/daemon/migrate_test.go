package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

// code is the API error code err maps to.
func code(err error) string {
	if err == nil {
		return ""
	}
	_, e := classify(err)
	return e.Code
}

func (e *tenv) export(names ...string) string {
	e.t.Helper()
	x, err := e.d.MigrateExport(context.Background(), names)
	if err != nil {
		e.t.Fatal(err)
	}
	return x.Token
}

func (e *tenv) frozen(name string) bool { return e.d.o.Switcher.Frozen(name) }

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func TestExportPacksAndFreezes(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	x, err := e.d.MigrateExport(ctx, []string{"b", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if x.Token == "" || !x.ExpiresAt.Equal(t0.Add(migrateTTL)) {
		t.Fatalf("token %q, expires %v", x.Token, x.ExpiresAt)
	}
	accts, err := migrate.Open([]byte(x.Bundle), x.Code)
	if err != nil || len(accts) != 1 {
		t.Fatal(accts, err)
	}
	vault, _ := e.kc.Get(store.VaultService, "b")
	if a := accts[0]; a.Name != "b" || a.Email != "b@x" || a.Priority != 10 || !a.Enabled ||
		!jsonEqual(t, a.Credentials, vault) || !jsonEqual(t, a.OAuthAccount, []byte(`{"emailAddress":"b@x"}`)) {
		t.Fatalf("%+v", a)
	}
	if !e.frozen("b") || e.frozen("a") {
		t.Fatal("b must be frozen, a not")
	}
	if _, err := e.d.Activate(ctx, "b", false); code(err) != "migrating" {
		t.Fatal("a switch to a frozen account:", err)
	}
	if e.account("b").Email != "b@x" || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("export alone must not remove b")
	}
}

func TestFrozenNeverRefreshed(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	tok := e.export("b")
	e.poll()
	if slices.ContainsFunc(e.api.callsSince(0), func(c string) bool { return strings.HasPrefix(c, "refresh") }) {
		t.Fatal("a frozen account refreshed:", e.api.callsSince(0))
	}
	if err := e.d.MigrateCommit(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenNotAutoTarget(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(99) })
	e.export("b")
	e.poll()
	if e.entry().OAuth.RefreshToken != "rA" || e.st.State().Active != "a" {
		t.Fatal("auto-switch moved to a frozen account")
	}
	if st, _ := e.d.Status(context.Background()); st.Next != nil { // the policy never picks it, not only the switcher
		t.Fatal("a frozen account is next:", *st.Next)
	}
}

func TestCommitRemovesAccounts(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	tok := e.export("b")
	if err := e.d.MigrateCommit(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.st.State().Find("b"); ok {
		t.Fatal("b left in state")
	}
	if _, err := e.st.Creds("b"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("b left in the vault:", err)
	}
	if e.frozen("b") {
		t.Fatal("b still frozen")
	}
	if err := e.d.MigrateCommit(ctx, tok); err != nil {
		t.Fatal("a second commit:", err)
	}
	if e.account("a").Email != "a@x" || e.entry().OAuth.RefreshToken != "rA" {
		t.Fatal("a touched")
	}
}

func TestCommitAfterTTL(t *testing.T) {
	e := newTenv(t)
	tok := e.export("b")
	e.clk.Add(migrateTTL + time.Second)
	if err := e.d.MigrateCommit(context.Background(), tok); code(err) != "export_gone" {
		t.Fatal(err)
	}
	// the freeze lapsed on the clock already; TestCommitRefusesChangedTokens covers the thaw of a refused commit
	if e.vault("b").OAuth.RefreshToken != "rB" || len(e.d.migrations) != 0 {
		t.Fatal("b must stay, the export dropped")
	}
	e.account("b")
}

// TestCommitRecheckOutlastsTTL: a keychain read that stalls past the deadline must not commit after it,
// when the CLI may already have told the user to trust ccswap list.
func TestCommitRecheckOutlastsTTL(t *testing.T) {
	e := newTenv(t)
	tok := e.export("b")
	e.kc.mu.Lock()
	e.kc.onGet = func(service string) {
		if service == claudecode.ServiceName("") {
			e.clk.Add(migrateTTL + time.Second)
		}
	}
	e.kc.mu.Unlock()
	if err := e.d.MigrateCommit(context.Background(), tok); code(err) != "export_gone" {
		t.Fatal(err)
	}
	if e.vault("b").OAuth.RefreshToken != "rB" || e.frozen("b") || len(e.d.migrations) != 0 {
		t.Fatal("b must stay, thawed, the export dropped")
	}
	e.account("b")
}

// TestCommitRefusedWhileStopping: a stopping daemon's socket may be gone already, and a CLI that found it gone
// trusts state.json: no commit may land after that.
func TestCommitRefusedWhileStopping(t *testing.T) {
	e := newTenv(t)
	tok := e.export("b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.d.mu.Lock()
	e.d.runCtx = ctx
	e.d.mu.Unlock()
	if err := e.d.MigrateCommit(context.Background(), tok); code(err) != "export_gone" {
		t.Fatal(err)
	}
	if e.vault("b").OAuth.RefreshToken != "rB" || e.frozen("b") || len(e.d.migrations) != 0 {
		t.Fatal("b must stay, thawed, the export dropped")
	}
	e.account("b")
}

// TestExportPrunesExpiredCommitted: a committed export is forgotten past its deadline too; a late cancel
// then answers export_gone, which the CLI settles through state.json.
func TestExportPrunesExpiredCommitted(t *testing.T) {
	e := newTenv(t)
	addC(t, e)
	ctx := context.Background()
	tok := e.export("b")
	if err := e.d.MigrateCommit(ctx, tok); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(migrateTTL + time.Second)
	e.export("c")
	if len(e.d.migrations) != 1 {
		t.Fatal("the committed export is kept:", len(e.d.migrations))
	}
	if err := e.d.MigrateCancel(ctx, tok); code(err) != "export_gone" {
		t.Fatal(err)
	}
}

func TestCommitRefusesChangedTokens(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	tok := e.export("b")
	e.kc.Fake.Set(store.VaultService, "b", cred("atB2", "rB2", t0.Add(time.Hour)))
	if err := e.d.MigrateCommit(ctx, tok); code(err) != "tokens_changed" || !strings.Contains(err.Error(), "b") {
		t.Fatal(err)
	}
	if e.vault("b").OAuth.RefreshToken != "rB2" || e.frozen("b") {
		t.Fatal("b must stay, thawed")
	}
	if err := e.d.MigrateCommit(ctx, tok); code(err) != "export_gone" {
		t.Fatal("a refused commit must drop the export:", err)
	}
}

func TestCommitRefusesAccountNowActive(t *testing.T) {
	e := newTenv(t)
	tok := e.export("b")
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), cred("atB", "rB", t0.Add(time.Hour)))
	e.poll()
	if e.st.State().Active != "b" {
		t.Fatal("setup: b must hold the entry")
	}
	if err := e.d.MigrateCommit(context.Background(), tok); code(err) != "migrate_active" {
		t.Fatal(err)
	}
	if e.account("b").Email != "b@x" || e.frozen("b") {
		t.Fatal("b must stay, thawed")
	}
}

// TestCommitRefusesHolder: the entry's identified holder differs from State.Active when a state write
// failed; it is in use all the same.
func TestCommitRefusesHolder(t *testing.T) {
	e := newTenv(t)
	tok := e.export("b")
	e.d.mu.Lock()
	e.d.holder = "b"
	e.d.mu.Unlock()
	if err := e.d.MigrateCommit(context.Background(), tok); code(err) != "migrate_active" {
		t.Fatal(err)
	}
	if e.account("b").Email != "b@x" || e.vault("b").OAuth.RefreshToken != "rB" || e.frozen("b") {
		t.Fatal("b must stay, thawed")
	}
}

func TestCancelThaws(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	tok := e.export("b")
	if err := e.d.MigrateCancel(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if e.frozen("b") || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("b must stay, thawed")
	}
	if err := e.d.MigrateCommit(ctx, tok); code(err) != "export_gone" {
		t.Fatal("commit after cancel:", err)
	}
	if _, err := e.d.Activate(ctx, "b", false); err != nil {
		t.Fatal("a thawed account is a switch target again:", err)
	}
}

func TestCancelAfterCommit(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	tok := e.export("b")
	if err := e.d.MigrateCommit(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := e.d.MigrateCancel(ctx, tok); code(err) != "export_done" {
		t.Fatal(err)
	}
}

// TestCancelUnknownTokenIsGone: a restart forgets a commit that landed just before it, so an unknown token
// is not a success the CLI could read as "the accounts stayed".
func TestCancelUnknownTokenIsGone(t *testing.T) {
	e := newTenv(t)
	if err := e.d.MigrateCancel(context.Background(), "nope"); code(err) != "export_gone" {
		t.Fatal(err)
	}
}

func TestCancelExpired(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	tok := e.export("b")
	e.clk.Add(migrateTTL + time.Second)
	if err := e.d.MigrateCancel(ctx, tok); err != nil {
		t.Fatal("an expired export that was never committed: its accounts certainly stayed:", err)
	}
	// the freeze lapsed on the clock already; TestCancelThaws covers the thaw
	if len(e.d.migrations) != 0 || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("b must stay, the export dropped")
	}
	if err := e.d.MigrateCancel(ctx, tok); code(err) != "export_gone" {
		t.Fatal("a second cancel:", err)
	}
}

func TestExportRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		setup func(e *tenv)
		want  string
	}{
		{"active", []string{"a"}, nil, "migrate_active"},
		{"holder", []string{"b"}, func(e *tenv) { e.d.mu.Lock(); e.d.holder = "b"; e.d.mu.Unlock() }, "migrate_active"},
		{"unknown", []string{"nope"}, nil, "not_found"},
		{"no names", []string{}, nil, "invalid_name"},
		{"bad name", []string{"B!"}, nil, "invalid_name"},
		{"needs login", []string{"b"}, func(e *tenv) {
			e.st.Update(func(s *store.State) error { s.Accounts[1].NeedsLogin = true; return nil })
		}, "needs_login"},
		{"no vault item", []string{"b"}, func(e *tenv) { e.kc.Fake.Delete(store.VaultService, "b") }, "needs_login"},
		{"entry chain", []string{"b"}, func(e *tenv) {
			e.kc.Fake.Set(store.VaultService, "b", cred("atA", "rA", t0.Add(time.Hour)))
		}, "migrate_active"},
		{"frozen", []string{"b"}, func(e *tenv) { e.d.o.Switcher.Freeze([]string{"b"}, t0.Add(time.Hour)) }, "migrating"},
		{"pending pair", []string{"b"}, func(e *tenv) {
			e.kc.Fake.Set(store.VaultService, "b", cred("atB", "rB", t0.Add(5*time.Minute)))
			e.api.set(func(f *fakeAnthropic) {
				f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
			})
			e.failVault(100)
			e.poll()
		}, "target_refreshing"},
		{"one of several", []string{"b", "a"}, nil, "migrate_active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			frozen := e.frozen("b")
			x, err := e.d.MigrateExport(context.Background(), tc.names)
			if code(err) != tc.want || x.Bundle != "" || x.Code != "" || x.Token != "" {
				t.Fatalf("%v (%s) %+v, want %s", err, code(err), x, tc.want)
			}
			if e.frozen("b") != frozen || e.frozen("a") {
				t.Fatal("a refused export froze an account")
			}
			if len(e.d.migrations) != 0 {
				t.Fatal("a refused export was recorded")
			}
		})
	}
}

// TestExportRefusesExpiredRefreshToken: a login whose refresh token has expired is dead, so it does not move.
func TestExportRefusesExpiredRefreshToken(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(store.VaultService, "b", fmt.Appendf(nil,
		`{"claudeAiOauth":{"accessToken":"atB","refreshToken":"rB","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		t0.Add(time.Hour).UnixMilli(), t0.UnixMilli()))
	_, err := e.d.MigrateExport(context.Background(), []string{"b"})
	if code(err) != "needs_login" || err.Error() != i18n.Err("daemon.migrate_needs_login", switcher.ErrNeedsLogin, "b").Error() {
		t.Fatal(err)
	}
	if e.frozen("b") || len(e.d.migrations) != 0 {
		t.Fatal("a refused export froze b")
	}
}

func TestExportNoNamesSaysSo(t *testing.T) {
	e := newTenv(t)
	_, err := e.d.MigrateExport(context.Background(), nil)
	if !errors.Is(err, ErrInvalidName) || err.Error() != i18n.T("daemon.migrate_no_names") {
		t.Fatalf("%v", err)
	}
}

func TestRemoveFrozenRefused(t *testing.T) {
	e := newTenv(t)
	e.export("b")
	if err := e.d.Remove(context.Background(), "b"); code(err) != "migrating" {
		t.Fatal(err)
	}
	if e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("b removed")
	}
	e.account("b")
}

func TestMigrateNeverLogged(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e := newTenv(t)
	ctx := context.Background()
	x, err := e.d.MigrateExport(ctx, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.MigrateCommit(ctx, x.Token); err != nil {
		t.Fatal(err)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB"] = resp{200, `{"access_token":"atB2","refresh_token":"rB2","expires_in":28800}`}
	})
	if _, err := e.migrateImport(x.Bundle, x.Code, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	log := buf.String()
	mu.Unlock()
	if !strings.Contains(log, "migrate: accounts packed") || !strings.Contains(log, "migrate: accounts moved out") ||
		!strings.Contains(log, "migrate: accounts moved in") {
		t.Fatal("the log was not captured:", log)
	}
	secrets := []string{x.Code, strings.ReplaceAll(x.Code, "-", ""), x.Token, "rB", "atB"}
	for _, l := range strings.Split(x.Bundle, "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") {
			secrets = append(secrets, l)
		}
	}
	for _, s := range secrets {
		if strings.Contains(log, s) {
			t.Fatalf("the log carries %q:\n%s", s, log)
		}
	}
}

func macct(name, email, rt string, priority int) migrate.Account {
	return migrate.Account{Name: name, Email: email, Priority: priority, Enabled: true,
		OAuthAccount: fmt.Appendf(nil, `{"emailAddress":%q}`, email), Credentials: cred("at"+rt, rt, t0.Add(time.Hour))}
}

func seal(t *testing.T, accts ...migrate.Account) (bundle, code string) {
	t.Helper()
	b, c, err := migrate.Seal(accts)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), c
}

func (e *tenv) migrateImport(bundle, code string, replace bool) (api.MigrateImport, error) {
	return e.d.MigrateImport(context.Background(), api.MigrateImportRequest{Bundle: bundle, Code: code, Replace: replace})
}

// vaultWrites is how many vault logins were set or deleted.
func (e *tenv) vaultWrites() int {
	e.kc.mu.Lock()
	defer e.kc.mu.Unlock()
	return e.kc.vaultSets + e.kc.vaultDeletes
}

// untouched fails the test when anything was written since it was called: state, or a vault login set or deleted.
func (e *tenv) untouched() func() {
	e.t.Helper()
	st, writes := e.st.State(), e.vaultWrites()
	return func() {
		e.t.Helper()
		if !reflect.DeepEqual(st, e.st.State()) || e.vaultWrites() != writes {
			e.t.Fatal("something was written")
		}
	}
}

func (e *tenv) unspent(name string) bool {
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	return e.d.unspent[name]
}

func TestImportAddsAndRefreshes(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"] = resp{200, `{"access_token":"atC2","refresh_token":"rC2","expires_in":28800}`}
	})
	bundle, code := seal(t, macct("c", "c@x", "rC", 7))
	x, err := e.migrateImport(bundle, code, false)
	if err != nil {
		t.Fatal(err)
	}
	a := e.account("c")
	if a.Email != "c@x" || a.Priority != 7 || !a.Enabled || a.NeedsLogin || a.AuthIndex != store.AuthIndex("c") ||
		!jsonEqual(t, a.OAuthAccount, []byte(`{"emailAddress":"c@x"}`)) {
		t.Fatalf("%+v", a)
	}
	// the access token is good for an hour: refreshed all the same, so the bundle's refresh token is spent
	if e.vault("c").OAuth.RefreshToken != "rC2" || e.api.count("refresh rC") != 1 || len(e.api.callsSince(0)) != 1 {
		t.Fatal(e.api.callsSince(0))
	}
	if len(x.Accounts) != 1 || x.Accounts[0].Name != "c" || x.Accounts[0].NeedsLogin {
		t.Fatalf("%+v", x)
	}
	if e.account("a").Email != "a@x" || e.vault("b").OAuth.RefreshToken != "rB" || e.entry().OAuth.RefreshToken != "rA" {
		t.Fatal("a or b touched")
	}
}

func TestImportDeadLoginNeedsLogin(t *testing.T) {
	e := newTenv(t)
	bundle, code := seal(t, macct("c", "c@x", "rC", 7))
	x, err := e.migrateImport(bundle, code, false)
	if err != nil {
		t.Fatal(err)
	}
	if !e.account("c").NeedsLogin || len(x.Accounts) != 1 || !x.Accounts[0].NeedsLogin || e.unspent("c") {
		t.Fatalf("c must need a login, no more forced refresh: %+v", x)
	}
	e.msgs(loginAgain("c").Error())
}

// TestImportRefreshFailureShown: a refresh that fails for another reason than a dead login keeps the
// account, and the answer says the refresh failed.
func TestImportRefreshFailureShown(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.refresh["rC"] = resp{500, `{}`} })
	bundle, code := seal(t, macct("c", "c@x", "rC", 7))
	x, err := e.migrateImport(bundle, code, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Accounts) != 1 || x.Accounts[0].NeedsLogin || x.Accounts[0].Error == nil ||
		*x.Accounts[0].Error != i18n.T("daemon.refresh_failed") {
		t.Fatalf("%+v", x)
	}
	if e.api.count("refresh rC") != 1 || e.vault("c").OAuth.RefreshToken != "rC" {
		t.Fatal(e.api.callsSince(0))
	}
}

// TestImportRefreshForcedUntilSpent: a forced refresh that failed is forced again on later ticks until the
// bundle's refresh token is spent, and then no more.
func TestImportRefreshForcedUntilSpent(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.refresh["rC"] = resp{500, `{}`} })
	bundle, code := seal(t, macct("c", "c@x", "rC", 7))
	x, err := e.migrateImport(bundle, code, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Accounts) != 1 || x.Accounts[0].Error == nil {
		t.Fatalf("the failed refresh must show: %+v", x)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"] = resp{200, `{"access_token":"atC2","refresh_token":"rC2","expires_in":28800}`}
	})
	e.poll() // the access token is good for an hour: refreshed all the same
	if e.api.count("refresh rC") != 2 || e.vault("c").OAuth.RefreshToken != "rC2" {
		t.Fatal(e.api.callsSince(0))
	}
	e.poll()
	if e.api.count("refresh rC2") != 0 || e.unspent("c") {
		t.Fatal("refreshed again once spent:", e.api.callsSince(0))
	}
}

// TestUnspentGoesWithTheAccount: an account removed, moved out or replaced is no longer refreshed by force,
// and one that replaces another is spent before it is written.
func TestUnspentGoesWithTheAccount(t *testing.T) {
	e := newTenv(t)
	ctx := context.Background()
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"], f.refresh["rD"] = resp{500, `{}`}, resp{200, `{"access_token":"atD2","refresh_token":"rD2","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("c", "c@x", "rC", 7))
	imp := func(bundle, code string, replace bool) {
		t.Helper()
		if _, err := e.migrateImport(bundle, code, replace); err != nil {
			t.Fatal(err)
		}
	}
	imp(bundle, c, false)
	if !e.unspent("c") {
		t.Fatal("a failed refresh must be forced again")
	}
	if err := e.d.Remove(ctx, "c"); err != nil || e.unspent("c") {
		t.Fatal("remove:", err)
	}
	imp(bundle, c, false)
	if err := e.d.MigrateCommit(ctx, e.export("c")); err != nil || e.unspent("c") {
		t.Fatal("commit:", err)
	}
	imp(bundle, c, false)
	bundle, c = seal(t, macct("d", "c@x", "rD", 7))
	imp(bundle, c, true)
	if e.unspent("c") || e.unspent("d") {
		t.Fatal("replace: c dropped, d spent already")
	}
}

func TestImportWrongCode(t *testing.T) {
	e := newTenv(t)
	bundle, _ := seal(t, macct("c", "c@x", "rC", 7))
	_, other := seal(t, macct("c", "c@x", "rC", 7))
	done := e.untouched()
	if _, err := e.migrateImport(bundle, other, false); code(err) != "bad_bundle" {
		t.Fatal(err)
	}
	done()
}

func TestImportGarbage(t *testing.T) {
	e := newTenv(t)
	_, c := seal(t, macct("c", "c@x", "rC", 7))
	done := e.untouched()
	if _, err := e.migrateImport("junk", c, false); code(err) != "not_bundle" {
		t.Fatal(err)
	}
	done()
}

func TestImportBadCodeFormat(t *testing.T) {
	e := newTenv(t)
	bundle, _ := seal(t, macct("c", "c@x", "rC", 7))
	done := e.untouched()
	if _, err := e.migrateImport(bundle, "nope", false); code(err) != "invalid_code" {
		t.Fatal(err)
	}
	done()
}

func TestImportNewerBundle(t *testing.T) {
	e := newTenv(t)
	bundle, c := seal(t, macct("c", "c@x", "rC", 7))
	done := e.untouched()
	if _, err := e.migrateImport(strings.Replace(bundle, " v1-----", " v2-----", 1), c, false); code(err) != "bundle_version" {
		t.Fatal(err)
	}
	done()
}

// TestImportBadContent: a bundle that decrypts but holds what export never packs is refused as damaged.
func TestImportBadContent(t *testing.T) {
	noRT := macct("c", "c@x", "rC", 7)
	noRT.Credentials = []byte(`{"claudeAiOauth":{"accessToken":"atC"}}`)
	noEmail := macct("c", "", "rC", 7)
	badName := macct("C!", "c@x", "rC", 7)
	for name, accts := range map[string][]migrate.Account{
		"no accounts":      {},
		"no refresh token": {noRT},
		"no email":         {noEmail},
		"bad name":         {badName},
		"same name":        {macct("c", "c@x", "rC", 7), macct("c", "d@x", "rD", 7)},
		"same email":       {macct("c", "c@x", "rC", 7), macct("d", "C@X", "rD", 7)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTenv(t)
			bundle, c := seal(t, accts...)
			done := e.untouched()
			if _, err := e.migrateImport(bundle, c, false); code(err) != "bad_bundle" {
				t.Fatal(err)
			}
			done()
		})
	}
}

func TestImportDuplicateEmail(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	_, err := e.migrateImport(bundle, c, false)
	if code(err) != "duplicate" || err.Error() != i18n.Err("daemon.migrate_email_here", store.ErrDuplicate, "b@x", "b").Error() {
		t.Fatal(err)
	}
	done()
	if _, err := e.migrateImport(bundle, c, true); err != nil {
		t.Fatal(err)
	}
	if b := e.account("b"); b.Priority != 3 || b.NeedsLogin || e.vault("b").OAuth.RefreshToken != "rB10" {
		t.Fatalf("b must be replaced: %+v", b)
	}
	if n := len(e.st.State().Accounts); n != 2 {
		t.Fatal(n, "accounts")
	}
}

// TestImportReplaceRenames: a bundle account with an email here under another name replaces that account,
// and its old vault login goes.
func TestImportReplaceRenames(t *testing.T) {
	e := newTenv(t)
	e.d.mu.Lock()
	e.d.usage["b"] = &acctUsage{err: "x"}
	e.d.mu.Unlock()
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"] = resp{200, `{"access_token":"atC2","refresh_token":"rC2","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("c", "b@x", "rC", 3))
	if _, err := e.migrateImport(bundle, c, true); err != nil {
		t.Fatal(err)
	}
	if e.vault("c").OAuth.RefreshToken != "rC2" {
		t.Fatal("c must hold the rotated pair:", e.vault("c").OAuth.RefreshToken)
	}
	if _, ok := e.st.State().Find("b"); ok {
		t.Fatal("b must be replaced by c")
	}
	if _, err := e.st.Creds("b"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("the vault login of b left:", err)
	}
	e.d.mu.Lock()
	u := e.d.usage["b"]
	e.d.mu.Unlock()
	if e.account("c").Email != "b@x" || u != nil {
		t.Fatal("c must take b's place, b's usage dropped")
	}
}

func TestImportNameTakenByOtherEmail(t *testing.T) {
	e := newTenv(t)
	bundle, c := seal(t, macct("b", "z@x", "rZ", 3))
	done := e.untouched()
	want := i18n.Err("daemon.migrate_name_taken", store.ErrDuplicate, "b", "b@x").Error() // b's email
	for _, replace := range []bool{false, true} {
		if _, err := e.migrateImport(bundle, c, replace); code(err) != "duplicate" || err.Error() != want {
			t.Fatal(replace, err)
		}
	}
	done()
	if e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("b touched")
	}
}

func TestImportReplaceActiveRefused(t *testing.T) {
	for _, tc := range []struct {
		name, acct, email string
		setup             func(e *tenv)
		want              string
	}{
		{"active", "a", "a@x", nil, "migrate_active"},
		{"holder", "b", "b@x", func(e *tenv) { e.d.mu.Lock(); e.d.holder = "b"; e.d.mu.Unlock() }, "migrate_active"},
		{"entry chain", "b", "b@x", func(e *tenv) {
			e.kc.Fake.Set(store.VaultService, "b", cred("atA", "rA", t0.Add(time.Hour)))
		}, "migrate_active"},
		{"frozen", "b", "b@x", func(e *tenv) { e.d.o.Switcher.Freeze([]string{"b"}, t0.Add(time.Hour)) }, "migrating"},
		// renamed, so only the guard reads b's vault item: an unreadable one is not taken as free to replace
		{"unreadable vault", "c", "b@x", func(e *tenv) { e.kc.mu.Lock(); e.kc.failGet = "b"; e.kc.mu.Unlock() }, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			bundle, c := seal(t, macct(tc.acct, tc.email, "rN", 3))
			done := e.untouched()
			if _, err := e.migrateImport(bundle, c, true); code(err) != tc.want {
				t.Fatal(err)
			}
			done()
		})
	}
}

// TestImportReadsEntryOnlyToReplace: an unreadable Claude Code entry fails only an import that replaces.
func TestImportReadsEntryOnlyToReplace(t *testing.T) {
	e := newTenv(t)
	e.kc.Fake.Set(claudecode.ServiceName(""), claudecode.KeychainAccount(), []byte("{"))
	bundle, c := seal(t, macct("c", "c@x", "rC", 7))
	if _, err := e.migrateImport(bundle, c, true); err != nil {
		t.Fatal(err)
	}
	e.account("c")
	bundle, c = seal(t, macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	if _, err := e.migrateImport(bundle, c, true); code(err) != "internal" {
		t.Fatal(err)
	}
	done()
}

func TestImportAllOrNothing(t *testing.T) {
	e := newTenv(t)
	bundle, c := seal(t, macct("c", "c@x", "rC", 7), macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	if _, err := e.migrateImport(bundle, c, false); code(err) != "duplicate" {
		t.Fatal(err)
	}
	done()
	if _, ok := e.st.State().Find("c"); ok {
		t.Fatal("c added")
	}
	if _, err := e.st.Creds("c"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("c left in the vault:", err)
	}
}

// TestImportRollsBackVault: a failed vault write undoes the import; the login it spent to replace b is kept
// by b, as the bundle's copy is dead.
func TestImportRollsBackVault(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	st := e.st.State()
	e.kc.mu.Lock()
	e.kc.failAt = e.kc.vaultSets + 2
	e.kc.mu.Unlock()
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3), macct("c", "c@x", "rC", 7))
	if _, err := e.migrateImport(bundle, c, true); err == nil {
		t.Fatal("the import must fail")
	}
	if e.vault("b").OAuth.RefreshToken != "rB10" {
		t.Fatal("b must keep the rotated pair:", e.vault("b").OAuth.RefreshToken)
	}
	if _, err := e.st.Creds("c"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("c left in the vault:", err)
	}
	if !reflect.DeepEqual(st, e.st.State()) || !slices.Equal(e.api.callsSince(0), []string{"refresh rB9"}) {
		t.Fatal("state changed, or another refresh ran:", e.api.callsSince(0))
	}
}

func TestImportRollsBackOnStateWrite(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	st := e.st.State()
	if err := os.Chmod(e.home, 0o500); err != nil { // state.json cannot be rewritten
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.home, 0o700) })
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3), macct("c", "c@x", "rC", 7))
	if _, err := e.migrateImport(bundle, c, true); err == nil {
		t.Fatal("the import must fail")
	}
	if e.vault("b").OAuth.RefreshToken != "rB10" || !reflect.DeepEqual(st, e.st.State()) {
		t.Fatal("b must keep the rotated pair, state unchanged:", e.vault("b").OAuth.RefreshToken)
	}
	if _, err := e.st.Creds("c"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("c left in the vault:", err)
	}
}

func TestImportSameBundleTwice(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"] = resp{200, `{"access_token":"atC2","refresh_token":"rC2","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("c", "c@x", "rC", 7))
	if _, err := e.migrateImport(bundle, c, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.migrateImport(bundle, c, false); code(err) != "duplicate" {
		t.Fatal(err)
	}
	if e.vault("c").OAuth.RefreshToken != "rC2" {
		t.Fatal("the second import touched c")
	}
}

// TestImportReplaceSpentRefused: --replace with a bundle whose login was used already (imported before) never
// puts that dead login over the working one here.
func TestImportReplaceSpentRefused(t *testing.T) {
	e := newTenv(t)
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3)) // the fake answers rB9 with invalid_grant
	done := e.untouched()
	_, err := e.migrateImport(bundle, c, true)
	if code(err) != "bundle_spent" || err.Error() != i18n.Err("daemon.migrate_spent", ErrBundleSpent, "b", "b").Error() {
		t.Fatal(err)
	}
	done()
	if e.vault("b").OAuth.RefreshToken != "rB" || e.api.count("refresh rB9") != 1 || e.account("b").NeedsLogin {
		t.Fatal("b touched:", e.api.callsSince(0))
	}
}

// TestImportReplaceUncheckedRefused: a bundle login Anthropic did not refresh may be spent: it replaces
// nothing, and the import can be tried again.
func TestImportReplaceUncheckedRefused(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.refresh["rB9"] = resp{500, `{}`} })
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	_, err := e.migrateImport(bundle, c, true)
	if code(err) != "bundle_unchecked" || err.Error() != i18n.Err("daemon.migrate_unchecked", ErrBundleUnchecked, "b").Error() {
		t.Fatal(err)
	}
	done()
	if e.vault("b").OAuth.RefreshToken != "rB" || e.unspent("b") {
		t.Fatal("b touched")
	}
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	if _, err := e.migrateImport(bundle, c, true); err != nil || e.vault("b").OAuth.RefreshToken != "rB10" {
		t.Fatal("the retry:", err)
	}
}

// TestImportReplaceWritesRotated: the login that replaces b is refreshed once, before it is written, and what
// is written is the rotated pair: the bundle's copy is spent.
func TestImportReplaceWritesRotated(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3))
	x, err := e.migrateImport(bundle, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := e.vault("b"); v.OAuth.RefreshToken != "rB10" || v.OAuth.AccessToken != "atB10" || e.unspent("b") {
		t.Fatalf("b holds %+v", v.OAuth)
	}
	if !slices.Equal(e.api.callsSince(0), []string{"refresh rB9"}) || len(x.Accounts) != 1 || x.Accounts[0].NeedsLogin {
		t.Fatalf("%v %+v", e.api.callsSince(0), x)
	}
	e.poll()
	if e.api.count("refresh rB10") != 0 {
		t.Fatal("refreshed again:", e.api.callsSince(0))
	}
}

// TestImportReplaceKeepsRotatedOnLaterFailure: once a replace spent the bundle's login of b, a later failure
// in the same import must not lose the rotated pair: b keeps it, and a retry takes it from there instead
// of the dead bundle copy.
func TestImportReplaceKeepsRotatedOnLaterFailure(t *testing.T) {
	e := newTenv(t)
	addC(t, e)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
		f.refresh["rC9"] = resp{500, `{}`}
	})
	st := e.st.State()
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3), macct("c", "c@x", "rC9", 4))
	if _, err := e.migrateImport(bundle, c, true); code(err) != "bundle_unchecked" {
		t.Fatal(err)
	}
	if e.vault("b").OAuth.RefreshToken != "rB10" || e.vault("c").OAuth.RefreshToken != "rC" || !reflect.DeepEqual(st, e.st.State()) {
		t.Fatal("b must keep the rotated pair, c and state untouched:", e.vault("b").OAuth.RefreshToken)
	}
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC9"] = resp{200, `{"access_token":"atC10","refresh_token":"rC10","expires_in":28800}`}
	})
	if _, err := e.migrateImport(bundle, c, true); err != nil {
		t.Fatal("the retry:", err)
	}
	if e.api.count("refresh rB9") != 1 || e.vault("b").OAuth.RefreshToken != "rB10" || e.vault("c").OAuth.RefreshToken != "rC10" {
		t.Fatal(e.api.callsSince(0))
	}
	if e.account("b").Priority != 3 || e.account("c").Priority != 4 {
		t.Fatal("the retry must import the bundle")
	}
}

// TestImportKeptPairSurvivesFailedSave: a rotated pair the vault does not take stays in memory, saved on a
// later tick, as a refresh's unsaved pair is.
func TestImportKeptPairSurvivesFailedSave(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	e.failVault(100)
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3))
	if _, err := e.migrateImport(bundle, c, true); err == nil {
		t.Fatal("the import must fail")
	}
	e.d.mu.Lock()
	p, ok := e.d.pending["b"]
	e.d.mu.Unlock()
	if !ok || p.creds.OAuth.RefreshToken != "rB10" || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("the rotated pair must wait in memory")
	}
	e.failVault(0)
	e.poll()
	if e.vault("b").OAuth.RefreshToken != "rB10" {
		t.Fatal("the rotated pair not saved:", e.vault("b").OAuth.RefreshToken)
	}
}

// TestImportRefreshesWithinBudget: refreshes that would keep the answer past importBudget are left to the
// next tick, so the CLI hears back in time; a replace that cannot be checked in it is refused.
func TestImportRefreshesWithinBudget(t *testing.T) {
	e := newTenv(t)
	old := importBudget
	importBudget = 0
	t.Cleanup(func() { importBudget = old })
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rC"] = resp{200, `{"access_token":"atC2","refresh_token":"rC2","expires_in":28800}`}
	})
	bundle, c := seal(t, macct("c", "c@x", "rC", 7))
	x, err := e.migrateImport(bundle, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.api.callsSince(0)) != 0 || len(x.Accounts) != 1 || x.Accounts[0].NeedsLogin || x.Accounts[0].Error != nil || !e.unspent("c") {
		t.Fatalf("%v %+v", e.api.callsSince(0), x)
	}
	e.poll()
	if e.vault("c").OAuth.RefreshToken != "rC2" || e.unspent("c") {
		t.Fatal("the next tick must refresh c:", e.api.callsSince(0))
	}
	bundle, c = seal(t, macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	if _, err = e.migrateImport(bundle, c, true); code(err) != "bundle_unchecked" || err.Error() != i18n.Err("daemon.migrate_unchecked_late", ErrBundleUnchecked, "b").Error() {
		t.Fatal(err) // no refresh ran: Anthropic is not to blame, and nothing is spent
	}
	done()
}

// TestImportReplaceRefusedWhileStopping: a stopping daemon starts no refresh of a bundle login: one cut short
// by the exit would lose its rotated pair.
func TestImportReplaceRefusedWhileStopping(t *testing.T) {
	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) {
		f.refresh["rB9"] = resp{200, `{"access_token":"atB10","refresh_token":"rB10","expires_in":28800}`}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.d.mu.Lock()
	e.d.runCtx = ctx
	e.d.mu.Unlock()
	bundle, c := seal(t, macct("b", "b@x", "rB9", 3))
	done := e.untouched()
	if _, err := e.migrateImport(bundle, c, true); code(err) != "bundle_unchecked" || err.Error() != i18n.Err("daemon.migrate_unchecked_late", ErrBundleUnchecked, "b").Error() {
		t.Fatal(err)
	}
	done()
	if len(e.api.callsSince(0)) != 0 || e.vault("b").OAuth.RefreshToken != "rB" {
		t.Fatal("a refresh ran, or b touched:", e.api.callsSince(0))
	}
}

// TestImportFinishAfterRestart: the README's way to finish a move whose retry a restarted daemon refuses as
// spent: the account holding the refreshed login moves out and back, around the bundle's import.
func TestImportFinishAfterRestart(t *testing.T) {
	e := newTenv(t)
	addC(t, e)
	ok := func(at, rt string) resp {
		return resp{200, fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":28800}`, at, rt)}
	}
	e.api.set(func(f *fakeAnthropic) { f.refresh["rB9"], f.refresh["rC9"] = ok("atB10", "rB10"), resp{500, `{}`} })
	bundle, code9 := seal(t, macct("b", "b@x", "rB9", 3), macct("c", "c@x", "rC9", 4), macct("d", "d@x", "rD", 5))
	if _, err := e.migrateImport(bundle, code9, true); code(err) != "bundle_unchecked" {
		t.Fatal(err)
	}
	e.d.mu.Lock()
	e.d.kept = map[string]string{} // a restart forgets
	e.d.mu.Unlock()
	e.api.set(func(f *fakeAnthropic) {
		delete(f.refresh, "rB9") // spent
		f.refresh["rC9"], f.refresh["rD"], f.refresh["rB10"] = ok("atC10", "rC10"), ok("atD2", "rD2"), ok("atB11", "rB11")
	})
	if _, err := e.migrateImport(bundle, code9, true); code(err) != "bundle_spent" {
		t.Fatal("the retry:", err)
	}

	x, err := e.d.MigrateExport(context.Background(), []string{"b"}) // ccswap migrate export b -o b.ccswap
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.MigrateCommit(context.Background(), x.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.migrateImport(bundle, code9, true); err != nil { // b arrives dead, c and d working
		t.Fatal(err)
	}
	if !e.account("b").NeedsLogin || e.vault("c").OAuth.RefreshToken != "rC10" || e.vault("d").OAuth.RefreshToken != "rD2" {
		t.Fatal("b dead, c and d imported:", e.api.callsSince(0))
	}
	if _, err := e.migrateImport(x.Bundle, x.Code, true); err != nil { // ccswap migrate import b.ccswap --replace
		t.Fatal(err)
	}
	if e.account("b").NeedsLogin || e.vault("b").OAuth.RefreshToken != "rB11" {
		t.Fatal("b must work again:", e.vault("b").OAuth.RefreshToken)
	}
}
