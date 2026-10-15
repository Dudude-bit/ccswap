// Package store holds account metadata (state.json) and the keychain vault of inactive accounts' credentials.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/keychain"
)

const VaultService = "ccswap"

// ErrDuplicate means an account with the same name or email already exists.
var ErrDuplicate = errors.New("account already exists")

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func ValidName(s string) bool { return nameRe.MatchString(s) }

func AuthIndex(name string) string {
	h := sha256.Sum256([]byte("ccswap:" + name))
	return hex.EncodeToString(h[:])[:16]
}

type Account struct {
	Name         string          `json:"name"`
	Email        string          `json:"email"`
	Priority     int             `json:"priority"`
	Enabled      bool            `json:"enabled"`
	NeedsLogin   bool            `json:"needs_login"`
	OAuthAccount json.RawMessage `json:"oauth_account,omitempty"`
	AuthIndex    string          `json:"auth_index"`
}

type State struct {
	Accounts     []Account `json:"accounts"`
	Active       string    `json:"active"`
	Auto         bool      `json:"auto"`
	Strategy     string    `json:"strategy,omitempty"` // "" is fill-first, the order before strategies existed
	LastSwitchAt time.Time `json:"last_switch_at"`
	Holders      []Holder  `json:"holders,omitempty"` // who held Claude Code's login, oldest first
}

// Holder holds Claude Code's login from Since until the next entry: an account, or "" for a login
// ccswap does not know or an account since removed. It follows the login, not only Active: a stranger's login does not change Active.
type Holder struct {
	Account string    `json:"account"`
	Since   time.Time `json:"since"`
}

// holdersFor is how far back the holder log reaches: entries older than this go, but the last such,
// which says who held Claude Code from before then.
const holdersFor = 48 * time.Hour

func (s State) Find(name string) (Account, bool) {
	for _, a := range s.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

// FindEmail finds the account with email, ignoring case; an empty email matches nothing.
func (s State) FindEmail(email string) (Account, bool) {
	for _, a := range s.Accounts {
		if email != "" && strings.EqualFold(a.Email, email) {
			return a, true
		}
	}
	return Account{}, false
}

func (s State) clone() State {
	s.Accounts = slices.Clone(s.Accounts)
	s.Holders = slices.Clone(s.Holders)
	for i := range s.Accounts {
		s.Accounts[i].OAuthAccount = bytes.Clone(s.Accounts[i].OAuthAccount)
	}
	return s
}

type Store struct {
	Now  func() time.Time // the clock of the holder log
	path string
	kc   keychain.Store
	mu   sync.Mutex
	st   State
}

func Open(home string, kc keychain.Store) (*Store, error) {
	s := &Store{Now: time.Now, path: filepath.Join(home, "state.json"), kc: kc, st: State{Auto: true}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.st = State{}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, fmt.Errorf("state.json is corrupt (not reset, fix or remove it): %w", err)
	}
	if st := &s.st; len(st.Holders) == 0 && st.Active != "" && !st.LastSwitchAt.IsZero() { // from before the log
		st.Holders = []Holder{{st.Active, st.LastSwitchAt}}
	}
	return s, nil
}

func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.clone()
}

func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.st.clone()
	if err := fn(&c); err != nil {
		return err
	}
	for i, h := range c.Holders { // a removed account's shifts are not those of a later one of its name
		if _, ok := c.Find(h.Account); !ok && h.Account != c.Active {
			c.Holders[i].Account = ""
		}
	}
	if c.Active != s.st.Active {
		c.hold(c.Active, s.Now())
	}
	if err := s.write(c); err != nil {
		return err
	}
	s.st = c
	return nil
}

// Hold logs name as the holder of Claude Code from now on, "" for a login ccswap does not know,
// unless the log says so already. State.Active, the account ccswap switched to, stays.
func (s *Store) Hold(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.st.clone()
	if !c.hold(name, s.Now()) {
		return nil
	}
	if err := s.write(c); err != nil {
		return err
	}
	s.st = c
	return nil
}

// hold appends name to the holder log unless its last entry names it, dropping what is past holdersFor.
func (st *State) hold(name string, now time.Time) bool {
	if n := len(st.Holders); n > 0 && st.Holders[n-1].Account == name {
		return false
	}
	st.Holders = append(st.Holders, Holder{name, now})
	i := 0
	for i+1 < len(st.Holders) && !st.Holders[i+1].Since.After(now.Add(-holdersFor)) {
		i++
	}
	st.Holders = st.Holders[i:]
	return true
}

// write persists state.json; a symlinked state.json stays a symlink.
func (s *Store) write(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, b, 0o600)
}

func checkName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid account name %q", name)
	}
	return nil
}

func (s *Store) Creds(name string) (claudecode.Credentials, error) {
	if err := checkName(name); err != nil {
		return claudecode.Credentials{}, err
	}
	b, err := s.kc.Get(VaultService, name)
	if err != nil {
		return claudecode.Credentials{}, err
	}
	return claudecode.ParseCredentials(b)
}

func (s *Store) PutCreds(name string, c claudecode.Credentials) error {
	if err := checkName(name); err != nil {
		return err
	}
	b, err := c.Bytes()
	if err != nil {
		return err
	}
	return s.kc.Set(VaultService, name, b)
}

func (s *Store) DeleteCreds(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return s.kc.Delete(VaultService, name)
}
