package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
)

const cmd = "/bin/ccswap signal"

func load(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestHookInstallIntoMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "settings.json")
	if err := Install(p, cmd); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := Installed(p, cmd); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	g := load(t, p)["hooks"].(map[string]any)["StopFailure"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if g["type"] != "command" || g["command"] != cmd || g["timeout"] != float64(5) {
		t.Fatalf("%v", g)
	}
}

const other = `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x"}]}],
"StopFailure":[{"hooks":[{"type":"command","command":"/other/hook"}]}]}}`

func TestHookInstallPreservesOtherHooks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write(t, p, other)
	if err := Install(p, cmd); err != nil {
		t.Fatal(err)
	}
	m := load(t, p)
	h := m["hooks"].(map[string]any)
	if m["model"] != "opus" || h["PreToolUse"] == nil || len(h["StopFailure"].([]any)) != 2 {
		t.Fatalf("%v", m)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatal("mode not kept", fi.Mode())
	}
}

func TestHookInstallIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	for range 2 {
		if err := Install(p, cmd); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(load(t, p)["hooks"].(map[string]any)["StopFailure"].([]any)); n != 1 {
		t.Fatal(n)
	}
}

func TestHookRemoveOnlyOurs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write(t, p, other)
	_ = Install(p, cmd)
	if err := Remove(p, cmd); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := Installed(p, cmd); ok {
		t.Fatal("still installed")
	}
	m := load(t, p)
	if len(m["hooks"].(map[string]any)["StopFailure"].([]any)) != 1 || m["hooks"].(map[string]any)["PreToolUse"] == nil {
		t.Fatalf("%v", m)
	}
	// only ours: everything collapses
	p2 := filepath.Join(t.TempDir(), "s.json")
	write(t, p2, `{"a":1}`)
	_ = Install(p2, cmd)
	_ = Remove(p2, cmd)
	m = load(t, p2)
	if _, ok := m["hooks"]; ok || m["a"] != float64(1) {
		t.Fatalf("%v", m)
	}
	if err := Remove(filepath.Join(t.TempDir(), "none.json"), cmd); err != nil {
		t.Fatal(err)
	}
}

func TestHookPathWithSpaces(t *testing.T) {
	if g := Command("/Applications/My Tools/ccswap"); g != "'/Applications/My Tools/ccswap' signal" {
		t.Fatal(g)
	}
	if g := Command("/a/it's/ccswap"); g != `'/a/it'\''s/ccswap' signal` {
		t.Fatal(g)
	}
	if g := Command("/usr/local/bin/ccswap"); g != "/usr/local/bin/ccswap signal" {
		t.Fatal(g)
	}
}

func TestHookSymlinkAndMalformed(t *testing.T) {
	d := t.TempDir()
	target, link := filepath.Join(d, "real.json"), filepath.Join(d, "settings.json")
	write(t, target, `{}`)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Install(link, cmd); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if ok, _, _ := Installed(target, cmd); !ok {
		t.Fatal("target not updated")
	}
	bad := filepath.Join(d, "bad.json")
	write(t, bad, `{oops`)
	if Install(bad, cmd) == nil {
		t.Fatal("want error")
	}
	if b, _ := os.ReadFile(bad); string(b) != `{oops` {
		t.Fatal("touched")
	}
}

const foreign = `{
  "zeta": 9007199254740993,
  "alpha": 1e3,
  "hooks": {
    "StopFailure": [{"hooks": [{"type": "command", "command": "a && <x> > y", "timeout": 1e1}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "b && c"}]}]
  },
  "model": "opus"
}`

func indented(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(s), "", "  "); err != nil {
		t.Fatal(err)
	}
	return b.String() + "\n"
}

func TestHookFaithfulRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write(t, p, foreign)
	if err := Install(p, cmd); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p, cmd); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != indented(t, foreign) {
		t.Fatalf("not faithful:\n%s", got)
	}
}

func TestHookStaleEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write(t, p, `{"hooks":{"StopFailure":[{"hooks":[{"type":"command","command":"'/old dir/ccswap' signal","timeout":5}]},{"hooks":[{"type":"command","command":"/x/other signal"}]}]}}`)
	if exact, stale, _ := Installed(p, cmd); exact || !stale {
		t.Fatal("stale not told apart:", exact, stale)
	}
	if err := Install(p, cmd); err != nil {
		t.Fatal(err)
	}
	if exact, stale, _ := Installed(p, cmd); !exact || stale {
		t.Fatal("after install:", exact, stale)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "old dir") || strings.Count(string(b), cmd) != 1 || !strings.Contains(string(b), "/x/other signal") {
		t.Fatalf("%s", b)
	}
	write(t, p, `{"hooks":{"StopFailure":[{"hooks":[{"type":"command","command":"/old/ccswap signal"}]}]}}`)
	if err := Remove(p, cmd); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "ccswap") {
		t.Fatalf("%s", b)
	}
}

func TestHookDanglingSymlink(t *testing.T) {
	d := t.TempDir()
	link := filepath.Join(d, "settings.json")
	if err := os.Symlink(filepath.Join(d, "gone.json"), link); err != nil {
		t.Fatal(err)
	}
	if Install(link, cmd) == nil {
		t.Fatal("want error")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
}

func TestSaveSyncsBeforeRename(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write(t, p, `{"model":"opus"}`)
	old := atomicfile.Sync
	t.Cleanup(func() { atomicfile.Sync = old })
	synced := false
	atomicfile.Sync = func(f *os.File) error {
		b, _ := os.ReadFile(f.Name())
		cur, _ := os.ReadFile(p)
		synced = bytes.Contains(b, []byte("StopFailure")) && !bytes.Contains(cur, []byte("StopFailure"))
		return f.Sync()
	}
	if err := Install(p, cmd); err != nil || !synced {
		t.Fatal("settings.json must be flushed to disk before the rename", err)
	}
	atomicfile.Sync = func(*os.File) error { return errors.New("disk gone") }
	if err := Remove(p, cmd); err == nil {
		t.Fatal("sync failure ignored")
	}
	if b, _ := os.ReadFile(p); !bytes.Contains(b, []byte("StopFailure")) {
		t.Fatal("unsynced copy replaced the file")
	}
}
