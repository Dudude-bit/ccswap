package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/update"
)

// ErrRestart: Run stopped because an update replaced the daemon's binary; the caller runs it in its place.
var ErrRestart = errors.New("restart on the updated binary")

var (
	ErrNoUpdate error = i18n.Err("daemon.no_update")
	ErrUpdating error = i18n.Err("daemon.updating")
)

// updateError is an update that did not install, with why.
type updateError struct{ err error }

func (e updateError) Error() string { return e.err.Error() }
func (e updateError) Unwrap() error { return e.err }

// Test seams.
var (
	latestRelease  = update.Latest
	installRelease = update.Install
)

// releaseAfter is the wait before the first ask, so a daemon that keeps restarting does not ask GitHub on
// every start; releaseEvery is the wait between asks.
var releaseAfter, releaseEvery = time.Minute, 12 * time.Hour

const installTimeout = 2 * time.Minute

// watchReleases asks GitHub for the newest release until ctx ends.
func (d *Daemon) watchReleases(ctx context.Context) {
	t := time.NewTimer(releaseAfter)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rel, err := latestRelease(ctx, update.UserAgent(d.o.Version))
		if err != nil {
			d.logOnce("release", slog.LevelWarn, "update: cannot ask GitHub for the newest release", "err", err)
		} else {
			d.logDone("release")
			d.noteRelease(rel)
		}
		t.Reset(releaseEvery)
	}
}

// noteRelease keeps rel when it is newer than this build, and tells the page when what it shows changes.
func (d *Daemon) noteRelease(rel update.Release) {
	var up api.Release
	if update.Newer(rel.Version, d.o.Version) {
		up = api.Release{Version: rel.Version, Url: rel.URL}
	}
	d.mu.Lock()
	changed := d.release != up
	d.release = up
	d.mu.Unlock()
	if !changed {
		return
	}
	if up.Version != "" {
		slog.Info("update: a new release is out", "version", rel.Version, "running", d.o.Version)
	}
	d.bus.publish()
}

// newer is the newest release known when it is newer than this build.
func (d *Daemon) newer() *api.Release {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.release.Version == "" {
		return nil
	}
	rel := d.release
	return &rel
}

// InstallUpdate installs the newest release over the daemon's binary and stops Run with ErrRestart. It
// answers once the new binary is in place; an update that fails leaves the old binary and the daemon as
// they were.
func (d *Daemon) InstallUpdate(ctx context.Context) (api.Release, error) {
	if !d.updating.CompareAndSwap(false, true) {
		return api.Release{}, ErrUpdating
	}
	rel, err := d.install(d.tickCtx(ctx))
	if err != nil {
		d.updating.Store(false)
		return api.Release{}, err
	}
	slog.Info("update: installed, restarting on it", "from", d.o.Version, "to", rel.Version, "bin", d.o.Bin)
	d.stopToRestart()
	return api.Release{Version: rel.Version, Url: rel.URL}, nil
}

// Restart restarts the daemon on its binary as InstallUpdate does, when that binary is bin: ccswap update
// has just installed a release over it. A daemon that runs another binary carries on and says which.
func (d *Daemon) Restart(_ context.Context, bin string) (api.Restart, error) {
	if bin == "" || bin != d.o.Bin {
		return api.Restart{Bin: d.o.Bin}, nil
	}
	if !d.updating.CompareAndSwap(false, true) {
		return api.Restart{}, ErrUpdating
	}
	slog.Info("update: ccswap update replaced the binary, restarting on it", "bin", d.o.Bin)
	d.stopToRestart()
	return api.Restart{Bin: d.o.Bin, Restarting: true}, nil
}

// stopToRestart stops Run, which then returns ErrRestart.
func (d *Daemon) stopToRestart() {
	d.mu.Lock()
	d.restart = true
	stop := d.cancel
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (d *Daemon) install(ctx context.Context) (update.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	rel, err := latestRelease(ctx, update.UserAgent(d.o.Version))
	if err != nil {
		return rel, updateError{err}
	}
	d.noteRelease(rel)
	if !update.Newer(rel.Version, d.o.Version) {
		return rel, ErrNoUpdate
	}
	if d.o.Bin == "" {
		return rel, updateError{i18n.Err("daemon.update_no_bin")}
	}
	if err := installRelease(ctx, rel, d.o.Bin); err != nil {
		slog.Warn("update: not installed", "version", rel.Version, "bin", d.o.Bin, "err", err)
		return rel, updateError{err}
	}
	return rel, nil
}
