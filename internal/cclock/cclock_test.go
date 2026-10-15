package cclock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ctxT(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestAcquireCreatesEmptyDirAndReleaseRemovesIt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(ctxT(t, 5*time.Second), p, StorageStale)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		t.Fatalf("want dir, got %v %v", fi, err)
	}
	if ents, _ := os.ReadDir(p); len(ents) != 0 {
		t.Fatalf("lock dir not empty: %v", ents)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if exists(p) {
		t.Fatal("lock not removed")
	}
}

func TestAcquireWaitsForHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(ctxT(t, 5*time.Second), p, StorageStale)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(200 * time.Millisecond)
		first.Release()
	}()
	defer func() { <-released }()
	start := time.Now()
	second, err := Acquire(ctxT(t, 5*time.Second), p, StorageStale)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("second acquire did not wait")
	}
}

func TestAcquireTakesOverStaleLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(ctxT(t, 2*time.Second), p, StorageStale)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
}

func TestAcquireNeverRemovesFreshForeignLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Acquire(ctxT(t, 300*time.Millisecond), p, StorageStale)
	if !errors.Is(err, ErrBusy) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), p) {
		t.Fatalf("want ErrBusy+DeadlineExceeded+path, got %v", err)
	}
	if !exists(p) {
		t.Fatal("foreign lock removed")
	}
}

func TestReleaseSparesLockReplacedByOther(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(ctxT(t, 5*time.Second), p, StorageStale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	other := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, other, other); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if !exists(p) {
		t.Fatal("release removed someone else's lock")
	}
}

func TestClaudeOrderAndLegacyPath(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	release, err := Claude(ctxT(t, 5*time.Second), link)
	if err != nil {
		t.Fatal(err)
	}
	locks := []string{
		filepath.Join(link, ".oauth_refresh.lock"),
		dir + ".lock",
		filepath.Join(link, ".storage-write.lock"),
	}
	for _, p := range locks {
		if !exists(p) {
			t.Fatalf("missing %s", p)
		}
	}
	release()
	for _, p := range locks {
		if exists(p) {
			t.Fatalf("not released: %s", p)
		}
	}
}

func TestClaudeFailsWholeWhenOneLockBusy(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	busy := filepath.Join(dir, ".storage-write.lock")
	if err := os.Mkdir(busy, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = Claude(ctxT(t, 300*time.Millisecond), dir)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	for _, p := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock"} {
		if exists(p) {
			t.Fatalf("leaked %s", p)
		}
	}
	if !exists(busy) {
		t.Fatal("foreign lock removed")
	}
}

func TestConfigFileLock(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".claude.json")
	release, err := ConfigFile(ctxT(t, 5*time.Second), f)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(f + ".lock") {
		t.Fatal("missing config lock")
	}
	release()
	if exists(f + ".lock") {
		t.Fatal("config lock not released")
	}
}

func mkStale(t *testing.T, p string, age time.Duration) {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireStaleNonEmptyRespectsCtx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	mkStale(t, p, 20*time.Second)
	f := filepath.Join(p, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := Acquire(ctxT(t, 300*time.Millisecond), p, StorageStale)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ignored ctx")
	}
	if !exists(f) {
		t.Fatal("contents touched")
	}
}

func TestAcquireStaleRegularFileNotUnlinked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(ctxT(t, 300*time.Millisecond), p, StorageStale); err == nil {
		t.Fatal("acquired over a regular file")
	}
	if !exists(p) {
		t.Fatal("regular file unlinked")
	}
}

func TestReleaseFuncIdempotent(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".claude.json")
	release, err := ConfigFile(ctxT(t, 5*time.Second), f)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.Mkdir(f+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	release()
	if !exists(f + ".lock") {
		t.Fatal("second release removed a foreign lock")
	}
}

func TestClaudeBusyRefreshTakesNothingElse(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Claude(ctxT(t, 300*time.Millisecond), dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	for _, p := range []string{dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
		if exists(p) {
			t.Fatalf("should not exist: %s", p)
		}
	}
}

func TestClaudeReleaseIsReverseOrder(t *testing.T) {
	var got []string
	orig := rmdir
	rmdir = func(p string) error { got = append(got, filepath.Base(p)); return orig(p) }
	t.Cleanup(func() { rmdir = orig })
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := Claude(ctxT(t, 5*time.Second), dir)
	if err != nil {
		t.Fatal(err)
	}
	release()
	want := []string{".storage-write.lock", ".claude.lock", ".oauth_refresh.lock"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestStaleThresholds(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// ConfigFile: 10 s
	f := filepath.Join(tmp, ".claude.json")
	mkStale(t, f+".lock", 12*time.Second)
	release, err := ConfigFile(ctxT(t, 2*time.Second), f)
	if err != nil {
		t.Fatalf("12s-old config lock not taken over: %v", err)
	}
	release()
	mkStale(t, f+".lock", 5*time.Second)
	if _, err := ConfigFile(ctxT(t, 300*time.Millisecond), f); !errors.Is(err, ErrBusy) {
		t.Fatalf("5s-old config lock: want ErrBusy, got %v", err)
	}
	// Claude: legacy lock uses 60 s, storage 15 s
	dir := filepath.Join(tmp, ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mkStale(t, dir+".lock", 30*time.Second)
	if _, err := Claude(ctxT(t, 300*time.Millisecond), dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("30s-old legacy lock: want ErrBusy, got %v", err)
	}
	if err := os.Remove(dir + ".lock"); err != nil {
		t.Fatal(err)
	}
	mkStale(t, dir+".lock", 90*time.Second)
	mkStale(t, filepath.Join(dir, ".storage-write.lock"), 20*time.Second)
	release, err = Claude(ctxT(t, 2*time.Second), dir)
	if err != nil {
		t.Fatalf("stale legacy+storage not taken over: %v", err)
	}
	release()
	storage := filepath.Join(dir, ".storage-write.lock")
	mkStale(t, storage, 10*time.Second)
	fi, _ := os.Stat(storage)
	if _, err := Claude(ctxT(t, 300*time.Millisecond), dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("10s-old storage lock: want ErrBusy, got %v", err)
	}
	if fi2, err := os.Stat(storage); err != nil || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("fresh storage lock touched")
	}
}
