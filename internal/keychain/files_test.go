package keychain

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestFilesVaultRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	f := NewFiles(root)
	b := []byte("secret")
	if err := f.Set("ccswap", "work", b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "ccswap", "work")
	if mode(t, p) != 0o600 || mode(t, root) != 0o700 || mode(t, filepath.Dir(p)) != 0o700 {
		t.Fatal("modes")
	}
	got, err := f.Get("ccswap", "work")
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("%q %v", got, err)
	}
	if err := f.Delete("ccswap", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("ccswap", "work"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.Delete("ccswap", "work"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestFilesRejectsBadSegments(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(filepath.Join(dir, "vault"))
	for _, c := range [][2]string{{"", "a"}, {"a", ""}, {".", "a"}, {"a", ".."}, {"..", "a"}, {"a/b", "c"}, {"a", "b/c"}} {
		if err := f.Set(c[0], c[1], []byte("x")); err == nil {
			t.Fatalf("accepted %q", c)
		}
		if _, err := f.Get(c[0], c[1]); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("get accepted %q", c)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("wrote %v", ents)
	}
}

func TestFilesClaudeEntryByPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude")
	p := filepath.Join(dir, ".credentials.json")
	f := NewFiles(t.TempDir())
	if err := f.Set(p, "ignored", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if mode(t, p) != 0o600 || mode(t, dir) != 0o700 {
		t.Fatal("modes")
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	if err := f.Set(p, "other", []byte("two")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if !fi.ModTime().After(old) {
		t.Fatal("mtime unchanged")
	}
	if b, _ := f.Get(p, "x"); string(b) != "two" {
		t.Fatalf("%q", b)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != ".credentials.json" {
			t.Fatalf("leftover %s", e.Name())
		}
	}
	if err := f.Delete(p, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestFilesSetSyncsDirAfterRename(t *testing.T) {
	old := atomicfile.SyncDir
	t.Cleanup(func() { atomicfile.SyncDir = old })
	dir := filepath.Join(t.TempDir(), ".claude")
	p := filepath.Join(dir, ".credentials.json")
	var synced []string
	atomicfile.SyncDir = func(d string) {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			synced = append(synced, e.Name())
		}
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(p+".tmp.0a1b2c3d", nil, 0o600) // Claude's in-flight temp: never ours to touch
	if err := NewFiles(t.TempDir()).Set(p, "", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(synced, ",") != ".credentials.json,.credentials.json.tmp.0a1b2c3d" {
		t.Fatalf("dir synced holding %v", synced)
	}
	if b, err := os.ReadFile(p + ".tmp.0a1b2c3d"); err != nil || len(b) != 0 {
		t.Fatal("Claude's temp touched")
	}
}

func TestFilesSetFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "busy")
	os.MkdirAll(filepath.Join(p, "x"), 0o700) // rename onto a non-empty dir fails
	if err := NewFiles(t.TempDir()).Set(p, "", []byte("s")); err == nil {
		t.Fatal("renamed onto a dir")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 || ents[0].Name() != "busy" {
		t.Fatalf("leftover %v", ents)
	}
}

func TestFilesReplacesSymlinkLikeClaude(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real.json")
	os.WriteFile(real, []byte("orig"), 0o600)
	p := filepath.Join(tmp, ".credentials.json")
	if err := os.Symlink(real, p); err != nil {
		t.Fatal(err)
	}
	f := NewFiles(t.TempDir())
	if b, err := f.Get(p, ""); err != nil || string(b) != "orig" {
		t.Fatalf("%q %v", b, err)
	}
	if err := f.Set(p, "", []byte("new")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(p)
	if !fi.Mode().IsRegular() {
		t.Fatal("still symlink")
	}
	if b, _ := os.ReadFile(real); string(b) != "orig" {
		t.Fatalf("real changed: %q", b)
	}
}

func TestFilesMissingClaudeDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "none")
	f := NewFiles(t.TempDir())
	if _, err := f.Get(filepath.Join(dir, ".credentials.json"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("dir created")
	}
}
