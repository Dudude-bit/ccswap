// Package atomicfile replaces files so that a crash leaves either the old or the new content.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Sync flushes the temp file before it replaces the target (darwin: F_FULLFSYNC); test seam.
var Sync = (*os.File).Sync

// SyncDir makes the rename in dir durable, best effort; test seam.
var SyncDir = func(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Write replaces path with data via a temp file renamed over the symlink-resolved path, so a
// symlink stays a symlink.
func Write(path string, data []byte, mode os.FileMode) error {
	p, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		p, err = path, nil
	}
	if err != nil {
		return err
	}
	return Replace(p, data, mode)
}

// Replace renames a temp file over path itself, so a symlink at path becomes a regular file, as
// Claude Code does with its credentials. The temp name is never one of Claude Code's.
func Replace(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ccswap-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = Sync(f) // power loss must not leave a zero-length file
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}
