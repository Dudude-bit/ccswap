// Package cclock takes the same proper-lockfile mkdir locks Claude Code uses
// for its credential store, so a swap never races its token refresh or writes.
package cclock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	RefreshStale = 60 * time.Second
	StorageStale = 15 * time.Second
	ConfigStale  = 10 * time.Second
)

var ErrBusy = errors.New("claude code lock busy")

// rmdir is a seam for tests; rmdir semantics never unlink files or symlinks.
var rmdir = syscall.Rmdir

// Lock is a held lock dir; mtime identifies it so Release spares a replacement.
type Lock struct {
	path  string
	mtime time.Time
}

// Acquire creates the empty lock dir at path, waiting until ctx is done.
// A lock older than stale is removed; a fresh one is never touched.
func Acquire(ctx context.Context, path string, stale time.Duration) (*Lock, error) {
	delay := 25 * time.Millisecond
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			fi, err := os.Stat(path)
			if err != nil {
				_ = rmdir(path)
				return nil, err
			}
			return &Lock{path: path, mtime: fi.ModTime()}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Stat -> rmdir is racy: another process may take over first (same as proper-lockfile).
		if time.Since(fi.ModTime()) > stale {
			err := rmdir(path)
			if err == nil || errors.Is(err, syscall.ENOENT) {
				continue
			}
			// ENOTEMPTY, ENOTDIR, ...: not ours to clear; back off until ctx ends.
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %s: %w", ErrBusy, path, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, 250*time.Millisecond)
	}
}

// Release removes the lock unless it was replaced (taken over as stale).
func (l *Lock) Release() error {
	fi, err := os.Stat(l.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !fi.ModTime().Equal(l.mtime) {
		return nil
	}
	if err := rmdir(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Claude takes the refresh, legacy and storage-write locks of Claude config
// dir in Claude Code's order; release frees them in reverse.
func Claude(ctx context.Context, dir string) (release func(), err error) {
	legacy := dir
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		legacy = real
	}
	return acquireAll(ctx, []spec{
		{filepath.Join(dir, ".oauth_refresh.lock"), RefreshStale},
		{legacy + ".lock", RefreshStale},
		{filepath.Join(dir, ".storage-write.lock"), StorageStale},
	})
}

// ConfigFile takes Claude Code's lock for the config file (file + ".lock").
func ConfigFile(ctx context.Context, file string) (release func(), err error) {
	return acquireAll(ctx, []spec{{file + ".lock", ConfigStale}})
}

type spec struct {
	path  string
	stale time.Duration
}

func acquireAll(ctx context.Context, specs []spec) (func(), error) {
	var held []*Lock
	done := false
	release := func() {
		if done {
			return
		}
		done = true
		for i := len(held) - 1; i >= 0; i-- {
			if err := held[i].Release(); err != nil {
				slog.Warn("cclock: release failed", "path", held[i].path, "err", err)
			}
		}
	}
	for _, s := range specs {
		l, err := Acquire(ctx, s.path, s.stale)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, l)
	}
	return release, nil
}
