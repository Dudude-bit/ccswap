package t3

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

// The vault accounts of the HTTP listener's keys; the leading "_", which store.ValidName rejects,
// keeps them clear of account names.
const (
	// KeyAccount holds the read key: T3 Code, `ccswap remote` on another machine and the page's
	// read-only view send it. `ccswap t3` prints it.
	KeyAccount = "_t3-management-key"
	// AdminKeyAccount holds the key the page changes settings with. It is never printed: the page
	// trades a one-time code from `ccswap web` for it.
	AdminKeyAccount = "_web-admin-key"
)

// Key returns the key kept in the vault under account, creating it on first use; an empty stored
// key counts as missing.
func Key(kc keychain.Store, account string) (string, error) {
	b, err := kc.Get(store.VaultService, account)
	if err == nil && len(b) > 0 {
		return string(b), nil
	}
	if err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	k := hex.EncodeToString(raw)
	return k, kc.Set(store.VaultService, account, []byte(k))
}
