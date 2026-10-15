package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/cclock"
)

func writeCfg(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func decode(t *testing.T, p string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func compact(t *testing.T, r json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSwapPreservesOtherKeys(t *testing.T) {
	p := writeCfg(t, `{"a":1,"b":{"x":[1,2]},"oauthAccount":{"email":"old"}}`, 0o600)
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err != nil {
		t.Fatal(err)
	}
	m := decode(t, p)
	if compact(t, m["a"]) != `1` || compact(t, m["b"]) != `{"x":[1,2]}` {
		t.Fatalf("other keys changed: %v", m)
	}
	got, err := ReadOAuthAccount(p)
	if err != nil || compact(t, got) != `{"email":"new"}` {
		t.Fatalf("got %s, %v", got, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestSwapKeepsOtherKeysUnescaped(t *testing.T) {
	p := writeCfg(t, `{"history":[{"display":"a <b> && c"}],"oauthAccount":{"email":"old"}}`, 0o600)
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"display": "a <b> && c"`) || !strings.HasSuffix(string(b), "}\n") {
		t.Fatalf("other keys were re-escaped:\n%s", b)
	}
}

func TestSwapMalformedLeavesFileUntouched(t *testing.T) {
	p := writeCfg(t, "{not json", 0o600)
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err == nil {
		t.Fatal("want error")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "{not json" {
		t.Fatalf("changed: %s", b)
	}
}

func TestSwapRetriesOnConcurrentWrite(t *testing.T) {
	p := writeCfg(t, `{"other":1,"oauthAccount":{"email":"old"}}`, 0o600)
	n := 0
	beforeRename = func() {
		if n++; n == 1 {
			os.WriteFile(p, []byte(`{"other":2,"oauthAccount":{"email":"old"}}`), 0o600)
		}
	}
	t.Cleanup(func() { beforeRename = nil })
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err != nil {
		t.Fatal(err)
	}
	m := decode(t, p)
	if compact(t, m["other"]) != `2` || compact(t, m["oauthAccount"]) != `{"email":"new"}` {
		t.Fatalf("got %v", m)
	}
}

func TestSwapGivesUpAfterFiveRaces(t *testing.T) {
	p := writeCfg(t, `{"other":0}`, 0o600)
	n := 0
	beforeRename = func() {
		n++
		os.WriteFile(p, fmt.Appendf(nil, `{"other":%d}`, n), 0o600)
	}
	t.Cleanup(func() { beforeRename = nil })
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err == nil {
		t.Fatal("want error")
	}
	if n != 5 {
		t.Fatalf("attempts %d", n)
	}
	m := decode(t, p)
	if compact(t, m["other"]) != `5` || m["oauthAccount"] != nil {
		t.Fatalf("got %v", m)
	}
	if es, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*")); len(es) != 1 {
		t.Fatalf("temp files left: %v", es)
	}
}

func TestMissingFileAndAbsentKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.json")
	if _, err := ReadOAuthAccount(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read: %v", err)
	}
	if err := SwapOAuthAccount(p, json.RawMessage(`{}`)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("swap: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("created")
	}
	q := writeCfg(t, `{"a":1}`, 0o600)
	if got, err := ReadOAuthAccount(q); got != nil || err != nil {
		t.Fatalf("%s %v", got, err)
	}
	if err := SwapOAuthAccount(q, json.RawMessage(`[1]`)); err == nil {
		t.Fatal("non-object accepted")
	}
}

func TestSwapThroughSymlink(t *testing.T) {
	target := writeCfg(t, `{"a":1,"oauthAccount":{"email":"old"}}`, 0o600)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := SwapOAuthAccount(link, json.RawMessage(`{"email":"new"}`)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi, err)
	}
	m := decode(t, target)
	if compact(t, m["a"]) != `1` || compact(t, m["oauthAccount"]) != `{"email":"new"}` {
		t.Fatalf("got %v", m)
	}
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if es, _ := os.ReadDir(d); len(es) != 1 {
			t.Fatalf("leftovers in %s: %v", d, es)
		}
	}
}

func TestSwapKeepsMode0644(t *testing.T) {
	p := writeCfg(t, `{}`, 0o644)
	if err := SwapOAuthAccount(p, json.RawMessage(`{"e":1}`)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestSwapSyncsBeforeRename(t *testing.T) {
	p := writeCfg(t, `{"oauthAccount":{"a":1}}`, 0o600)
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	synced := false
	syncFile = func(f *os.File) error {
		b, _ := os.ReadFile(f.Name())
		cur, _ := os.ReadFile(p)
		synced = strings.Contains(string(b), `"b"`) && strings.Contains(string(cur), `"a"`)
		return f.Sync()
	}
	if err := SwapOAuthAccount(p, json.RawMessage(`{"b":2}`)); err != nil || !synced {
		t.Fatal("~/.claude.json must be flushed to disk before the rename", err)
	}
	syncFile = func(*os.File) error { return errors.New("disk gone") }
	if err := SwapOAuthAccount(p, json.RawMessage(`{"c":3}`)); err == nil {
		t.Fatal("sync failure ignored")
	}
	if m := decode(t, p); compact(t, m["oauthAccount"]) != `{"b":2}` {
		t.Fatal("unsynced copy replaced the file")
	}
}

func TestSwapOAuthAccountHoldsConfigLock(t *testing.T) {
	p := writeCfg(t, `{"oauthAccount":{"email":"old"}}`, 0o600)
	lock := p + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	var released atomic.Bool
	go func() {
		time.Sleep(200 * time.Millisecond)
		released.Store(true)
		os.Remove(lock)
	}()
	ran := false
	beforeRename = func() {
		ran = true
		if !released.Load() {
			t.Error("swap ran while Claude Code held the config lock")
		}
		if _, err := os.Stat(lock); err != nil {
			t.Error("swap ran without the config lock")
		}
	}
	t.Cleanup(func() { beforeRename = nil })
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"new"}`)); err != nil || !ran {
		t.Fatal(err, ran)
	}
	if m := decode(t, p); compact(t, m["oauthAccount"]) != `{"email":"new"}` {
		t.Fatalf("got %v", m)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("config lock left behind")
	}

	// a lock held past the wait budget: nothing is written, the lock stays
	old := configLockWait
	configLockWait = 200 * time.Millisecond
	t.Cleanup(func() { configLockWait = old })
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SwapOAuthAccount(p, json.RawMessage(`{"email":"newer"}`)); !errors.Is(err, cclock.ErrBusy) {
		t.Fatal(err)
	}
	if m := decode(t, p); compact(t, m["oauthAccount"]) != `{"email":"new"}` {
		t.Fatalf("written under a foreign lock: %v", m)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("foreign lock removed")
	}
}
