package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// names lists dir, so a leftover temp file shows.
func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestReplaceWritesThroughAPrivateTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := Sync
	t.Cleanup(func() { Sync = old })
	var tmp string
	Sync = func(f *os.File) error {
		tmp = f.Name()
		if fi, err := f.Stat(); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("temp file %v %v: the secret must never sit in a readable file", fi.Mode(), err)
		}
		return old(f)
	}
	if err := Replace(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := filepath.Match(filepath.Join(dir, ".ccswap-*.tmp"), tmp); !ok {
		t.Fatalf("temp %q: not ccswap's own name next to the target", tmp)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if n := names(t, dir); !slices.Equal(n, []string{"f"}) {
		t.Fatalf("temp left behind: %v", n)
	}
}

func TestSymlinks(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("written"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Write replaced the symlink instead of its target")
	}
	if b, _ := os.ReadFile(target); string(b) != "written" {
		t.Fatalf("target %q", b)
	}
	if err := Replace(link, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); !fi.Mode().IsRegular() {
		t.Fatalf("Replace must turn the symlink into a regular file, as Claude Code does: %v", fi.Mode())
	}
	if b, _ := os.ReadFile(link); string(b) != "replaced" {
		t.Fatalf("link %q", b)
	}
	if b, _ := os.ReadFile(target); string(b) != "written" {
		t.Fatalf("Replace wrote through the symlink: target %q", b)
	}
	if err := Write(filepath.Join(dir, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal("a missing file must be created:", err)
	}
}

func TestFailureLeavesTargetAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := Sync
	t.Cleanup(func() { Sync = old })
	Sync = func(*os.File) error { return errors.New("disk gone") }
	if err := Replace(p, []byte("new"), 0o600); err == nil {
		t.Fatal("a failed sync must fail the write")
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Fatalf("target changed: %q", b)
	}
	Sync = old
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(sub, []byte("new"), 0o600); err == nil { // rename cannot replace a non-empty directory
		t.Fatal("a failed rename must fail the write")
	}
	if n := names(t, dir); !slices.Equal(n, []string{"f", "sub"}) {
		t.Fatalf("temp left behind: %v", n)
	}
}
