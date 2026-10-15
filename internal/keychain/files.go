package keychain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
)

// Files is the Linux Store: an absolute service is the file itself (Claude Code's
// .credentials.json, account ignored); otherwise Root/service/account.
type Files struct{ Root string }

var _ Store = (*Files)(nil)

func NewFiles(root string) *Files { return &Files{Root: root} }

func (f *Files) path(service, account string) (string, error) {
	if filepath.IsAbs(service) {
		return service, nil
	}
	for _, s := range []string{service, account} {
		if s == "" || s == "." || s == ".." || filepath.Base(s) != s || filepath.ToSlash(s) != s {
			return "", fmt.Errorf("keychain: bad name %q", s)
		}
	}
	return filepath.Join(f.Root, service, account), nil
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (f *Files) Get(service, account string) ([]byte, error) {
	p, err := f.path(service, account)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	return b, notFound(err)
}

func (f *Files) Delete(service, account string) error {
	p, err := f.path(service, account)
	if err != nil {
		return err
	}
	return notFound(os.Remove(p))
}

// Set writes like Claude Code: 0600 temp, fsync, rename (replaces a symlink), dir fsync, so a crash
// cannot bring back a consumed refresh token.
func (f *Files) Set(service, account string, secret []byte) error {
	p, err := f.path(service, account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return atomicfile.Replace(p, secret, 0o600)
}
