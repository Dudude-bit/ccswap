package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Dudude-bit/ccswap/internal/cclock"
)

const acctKey = "oauthAccount"

// beforeRename is a test seam called after the temp file is written.
var beforeRename func()

// configLockWait bounds the wait for Claude Code's config lock.
var configLockWait = 10 * time.Second

type snapshot struct {
	data []byte
	size int64
	mod  time.Time
}

func snap(path string) (snapshot, os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return snapshot{}, nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return snapshot{}, nil, err
	}
	return snapshot{b, fi.Size(), fi.ModTime()}, fi, nil
}

// ReadOAuthAccount returns the oauthAccount value, or nil when the key is absent.
func ReadOAuthAccount(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m[acctKey], nil
}

// SwapOAuthAccount replaces oauthAccount in the file atomically under Claude Code's config lock,
// restarting from a fresh read if the file changes while the new copy is being prepared.
func SwapOAuthAccount(orig string, acct json.RawMessage) error {
	var obj map[string]json.RawMessage
	if json.Unmarshal(acct, &obj) != nil || obj == nil {
		return errors.New("oauthAccount must be a JSON object")
	}
	ctx, cancel := context.WithTimeout(context.Background(), configLockWait)
	defer cancel()
	release, err := cclock.ConfigFile(ctx, orig)
	if err != nil {
		return err
	}
	defer release()
	for range 5 {
		path, err := filepath.EvalSymlinks(orig)
		if err != nil {
			return err
		}
		s, fi, err := snap(path)
		if err != nil {
			return err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(s.data, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if m == nil {
			return fmt.Errorf("%s: not a JSON object", path)
		}
		m[acctKey] = acct
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false) // keep "<", ">" and "&" in Claude Code's other keys as they are
		enc.SetIndent("", "  ")
		if err := enc.Encode(m); err != nil {
			return err
		}
		out := buf.Bytes()

		tmp, err := os.CreateTemp(filepath.Dir(path), ".ccswap-*.tmp")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(out)
		if werr == nil {
			werr = syncFile(tmp) // power loss must not leave a zero-length ~/.claude.json
		}
		cerr := tmp.Close()
		if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), fi.Mode().Perm())); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if beforeRename != nil {
			beforeRename()
		}
		cur, _, err := snap(path)
		if err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if cur.size != s.size || !cur.mod.Equal(s.mod) || !bytes.Equal(cur.data, s.data) {
			os.Remove(tmp.Name())
			continue
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if d, err := os.Open(filepath.Dir(path)); err == nil { // the rename itself, best effort
			_ = d.Sync()
			d.Close()
		}
		return nil
	}
	return fmt.Errorf("%s kept changing during swap, gave up after 5 attempts", orig)
}

// syncFile flushes a temp file to disk before it replaces a file (darwin: F_FULLFSYNC); test seam.
var syncFile = (*os.File).Sync
