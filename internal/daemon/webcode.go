package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"maps"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// webCodeTTL is how long a code of `ccswap web` can be traded.
const webCodeTTL = time.Minute

var ErrBadCode error = i18n.Err("daemon.bad_code")

// WebCode mints a one-time code that the page `ccswap web` opens trades for the admin key, so no key
// lands in a link, the browser's history or a process list.
func (d *Daemon) WebCode(context.Context) (api.WebCode, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return api.WebCode{}, err
	}
	code, now := hex.EncodeToString(b), d.o.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	maps.DeleteFunc(d.codes, func(_ string, exp time.Time) bool { return !now.Before(exp) })
	d.codes[code] = now.Add(webCodeTTL)
	return api.WebCode{Code: code, ExpiresAt: now.Add(webCodeTTL)}, nil
}

// UseWebCode takes a code from WebCode, once and within webCodeTTL. Each code is compared in
// constant time, so how long a guess takes tells nothing of the codes held.
func (d *Daemon) UseWebCode(_ context.Context, code string) error {
	now := d.o.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for c, exp := range d.codes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			delete(d.codes, c)
			if now.Before(exp) {
				return nil
			}
		}
	}
	return ErrBadCode
}
