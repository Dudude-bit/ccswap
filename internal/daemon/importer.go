// Package daemon holds the ccswap daemon's API server, client and account importer.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

var (
	ErrNotFound          = errors.New("account not found")
	ErrRemoveActive      = errors.New("cannot remove the active account")
	ErrLoginIncomplete   = errors.New("login was not completed")
	ErrActiveAccount     = errors.New("this is the active account: re-login it with `claude /login`")
	ErrInvalidName       = errors.New("invalid account name")
	ErrInvalidThresholds = errors.New("invalid thresholds")
	ErrInvalidStrategy   = errors.New("invalid strategy")
	ErrInvalidLang       = errors.New("invalid language")
	ErrNoEntry           = errors.New("Claude Code has no login") //lint:ignore ST1005 "Claude Code" is a proper noun
	// ErrInvalidConfigDir: config_dir is not a login dir `ccswap login` made in the ccswap home.
	ErrInvalidConfigDir = errors.New("config_dir is not a ccswap login directory")
)

// Locker serialises vault writes with the switcher (*switcher.Switcher implements it).
type Locker interface {
	Locked(func() error) error
}

// Importer turns a fresh `claude auth login` in a temp config dir into a stored account.
type Importer struct {
	KC       keychain.Store
	Store    *store.Store
	Profiles switcher.Profiler
	Locker   Locker
	Home     string // the ccswap home: login dirs are <Home>/login-*
	Target   string // Claude Code's entry ccswap switches; never read or deleted here
}

// Import stores the login of configDir as account name. An existing name with the same email is a re-login.
func (im *Importer) Import(ctx context.Context, name, configDir string, priority int) (store.Account, error) {
	return im.put(ctx, name, configDir, priority, false)
}

// Renew stores the login of configDir as the new login of account name, which must be here with its
// email: it never adds an account (ErrNotFound).
func (im *Importer) Renew(ctx context.Context, name, configDir string) (store.Account, error) {
	return im.put(ctx, name, configDir, 0, true)
}

func (im *Importer) put(ctx context.Context, name, configDir string, priority int, renew bool) (store.Account, error) {
	if !store.ValidName(name) {
		return store.Account{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	// the temp entry is read and then deleted: anything but a login dir could be a live login.
	if im.Home == "" || filepath.Dir(configDir) != filepath.Clean(im.Home) || !strings.HasPrefix(filepath.Base(configDir), "login-") {
		return store.Account{}, ErrInvalidConfigDir
	}
	// Claude Code may hash the path as given or symlink-resolved (/var vs /private/var): try both.
	svcs, ok := claudecode.LoginServices(configDir, im.Target)
	if !ok {
		return store.Account{}, ErrInvalidConfigDir
	}
	kcAcct := claudecode.KeychainAccount()
	defer func() {
		for _, s := range svcs {
			_ = im.KC.Delete(s, kcAcct)
		}
	}()
	var raw []byte
	for _, s := range svcs {
		b, err := im.KC.Get(s, kcAcct)
		if errors.Is(err, keychain.ErrNotFound) {
			continue
		}
		if err != nil {
			return store.Account{}, fmt.Errorf("read temporary login: %w", err)
		}
		raw = b
		break
	}
	if raw == nil {
		return store.Account{}, ErrLoginIncomplete
	}
	creds, err := claudecode.ParseCredentials(raw)
	if err != nil {
		return store.Account{}, fmt.Errorf("%w: %v", ErrLoginIncomplete, err)
	}
	p, err := im.Profiles.Profile(ctx, creds.OAuth.AccessToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("%w: %v", switcher.ErrCannotIdentify, err)
	}
	if p.Email == "" {
		return store.Account{}, fmt.Errorf("%w: profile has no email", switcher.ErrCannotIdentify)
	}
	blob, _ := claudecode.ReadOAuthAccount(claudecode.GlobalConfigPath("", configDir))
	var meta struct {
		Email string `json:"emailAddress"`
	}
	if json.Unmarshal(blob, &meta) != nil || !strings.EqualFold(meta.Email, p.Email) {
		blob = nil // belongs to some other login
	}

	var out store.Account
	err = im.Locker.Locked(func() error {
		created := false
		err := im.Store.Update(func(x *store.State) error {
			if i := slices.IndexFunc(x.Accounts, func(a store.Account) bool { return a.Name == name }); i >= 0 {
				if !strings.EqualFold(x.Accounts[i].Email, p.Email) {
					return fmt.Errorf("%w: name %q belongs to another email", store.ErrDuplicate, name)
				}
				if x.Active == name {
					return ErrActiveAccount
				}
				if err := im.Store.PutCreds(name, creds); err != nil {
					return err
				}
				x.Accounts[i].NeedsLogin = false
				if blob != nil {
					x.Accounts[i].OAuthAccount = blob
				}
				out = x.Accounts[i]
				return nil
			}
			if renew {
				return ErrNotFound
			}
			if a, ok := x.FindEmail(p.Email); ok {
				return fmt.Errorf("%w: email of %q", store.ErrDuplicate, a.Name)
			}
			if err := im.Store.PutCreds(name, creds); err != nil {
				return err
			}
			created = true
			out = store.Account{Name: name, Email: p.Email, Priority: priority, Enabled: true,
				OAuthAccount: blob, AuthIndex: store.AuthIndex(name)}
			x.Accounts = append(x.Accounts, out)
			return nil
		})
		if err != nil && created {
			_ = im.Store.DeleteCreds(name)
		}
		return err
	})
	return out, err
}

// DropLogin removes a login dir; unless the import succeeded (the importer then deletes them itself)
// it also deletes any keychain item claude created under the dir's hashed service. A dir that is the
// live login's (target) is left alone.
func DropLogin(dir, target string, kc keychain.Store, imported bool) {
	svcs, ok := claudecode.LoginServices(dir, target)
	if !ok {
		return
	}
	if !imported {
		for _, s := range svcs {
			_ = kc.Delete(s, claudecode.KeychainAccount())
		}
	}
	_ = os.RemoveAll(dir)
}

// SweepLogins drops the login dirs of home left over an hour ago by a killed ccswap login or daemon.
func SweepLogins(home, target string, kc keychain.Store) {
	ds, _ := filepath.Glob(filepath.Join(home, "login-*"))
	for _, d := range ds {
		if st, err := os.Stat(d); err == nil && time.Since(st.ModTime()) > time.Hour {
			DropLogin(d, target, kc, false)
		}
	}
}
