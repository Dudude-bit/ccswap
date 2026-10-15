// Package hook edits Claude Code's settings.json to register ccswap's StopFailure hook.
// Only our own entries are touched: every other value keeps its original bytes and key order.
package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

var safe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Command is the hook command line for binPath; Claude Code runs it through a shell.
func Command(binPath string) string {
	if !safe.MatchString(binPath) {
		binPath = "'" + strings.ReplaceAll(binPath, "'", `'\''`) + "'"
	}
	return binPath + " signal"
}

// shellTokens splits a command line the way a POSIX shell would, ignoring expansion.
func shellTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	in := false
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case q == '"':
			if c == '"' {
				q = 0
			} else if c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			q, in = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t':
			if in {
				toks = append(toks, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		toks = append(toks, cur.String())
	}
	return toks
}

// ours: the exact command, or a stale `<path>/ccswap signal` left by a moved binary.
func ours(cmd, command string) bool {
	if cmd == command {
		return true
	}
	t := shellTokens(cmd)
	return len(t) == 2 && filepath.Base(t[0]) == "ccswap" && t[1] == "signal"
}

type kv struct {
	k string
	v json.RawMessage
}

func parseObj(raw []byte) ([]kv, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, i18n.Err("hook.not_json_object")
	}
	out := []kv{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, kv{t.(string), v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, i18n.Err("hook.extra_data")
	}
	return out, nil
}

func str(s string) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return bytes.TrimSpace(b.Bytes())
}

func encObj(o []kv) []byte {
	b := []byte{'{'}
	for i, e := range o {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, str(e.k)...)
		b = append(b, ':')
		b = append(b, e.v...)
	}
	return append(b, '}')
}

func encArr(a []json.RawMessage) []byte {
	return append(append([]byte{'['}, bytes.Join(rawStrs(a), []byte{','})...), ']')
}

func rawStrs(a []json.RawMessage) [][]byte {
	out := make([][]byte, len(a))
	for i, r := range a {
		out[i] = r
	}
	return out
}

func find(o []kv, k string) int {
	for i, e := range o {
		if e.k == k {
			return i
		}
	}
	return -1
}

func set(o []kv, k string, v []byte) []kv {
	if i := find(o, k); i >= 0 {
		o[i].v = v
		return o
	}
	return append(o, kv{k, v})
}

func del(o []kv, k string) []kv {
	if i := find(o, k); i >= 0 {
		return append(o[:i:i], o[i+1:]...)
	}
	return o
}

func hookCmd(h json.RawMessage) string {
	var x struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(h, &x)
	return x.Command
}

// strip removes our entries; it returns the new top-level object and the commands removed.
func strip(top []kv, command string) ([]kv, []string) {
	hi := find(top, "hooks")
	if hi < 0 {
		return top, nil
	}
	hooks, err := parseObj(top[hi].v)
	if err != nil {
		return top, nil
	}
	si := find(hooks, "StopFailure")
	if si < 0 {
		return top, nil
	}
	var groups []json.RawMessage
	if json.Unmarshal(hooks[si].v, &groups) != nil {
		return top, nil
	}
	var removed []string
	var keep []json.RawMessage
	for _, g := range groups {
		gm, err := parseObj(g)
		gi := find(gm, "hooks")
		var hs []json.RawMessage
		if err != nil || gi < 0 || json.Unmarshal(gm[gi].v, &hs) != nil {
			keep = append(keep, g)
			continue
		}
		var hk []json.RawMessage
		for _, h := range hs {
			if c := hookCmd(h); ours(c, command) {
				removed = append(removed, c)
			} else {
				hk = append(hk, h)
			}
		}
		switch {
		case len(hk) == len(hs):
			keep = append(keep, g)
		case len(hk) > 0:
			gm[gi].v = encArr(hk)
			keep = append(keep, encObj(gm))
		}
	}
	if len(removed) == 0 {
		return top, nil
	}
	top = append([]kv(nil), top...)
	if len(keep) > 0 {
		hooks = set(append([]kv(nil), hooks...), "StopFailure", encArr(keep))
	} else {
		hooks = del(append([]kv(nil), hooks...), "StopFailure")
	}
	if len(hooks) > 0 {
		top[hi].v = encObj(hooks)
	} else {
		top = del(top, "hooks")
	}
	return top, removed
}

// read resolves symlinks and parses the top-level object (empty when the file is missing).
func read(path string) (string, []kv, os.FileMode, error) {
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", nil, 0, errors.New(i18n.T("hook.dangling_symlink", path))
		}
		target = r
	}
	b, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		return target, []kv{}, 0o600, nil
	}
	if err != nil {
		return "", nil, 0, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", nil, 0, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return target, []kv{}, fi.Mode().Perm(), nil
	}
	top, err := parseObj(b)
	if err != nil {
		return "", nil, 0, errors.New(i18n.T("hook.settings_not_object", path, err))
	}
	return target, top, fi.Mode().Perm(), nil
}

func save(target string, top []kv, mode os.FileMode) error {
	var b bytes.Buffer
	if err := json.Indent(&b, encObj(top), "", "  "); err != nil {
		return err
	}
	b.WriteByte('\n')
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(target, b.Bytes(), mode)
}

// Installed reports whether settings.json has the exact hook command and whether it has a stale
// one (an older path to ccswap).
func Installed(path, command string) (exact, stale bool, err error) {
	_, top, _, err := read(path)
	if err != nil {
		return false, false, err
	}
	_, removed := strip(top, command)
	for _, c := range removed {
		if c == command {
			exact = true
		} else {
			stale = true
		}
	}
	return exact, stale, nil
}

func Install(path, command string) error {
	target, top, mode, err := read(path)
	if err != nil {
		return err
	}
	top, removed := strip(top, command)
	if len(removed) == 1 && removed[0] == command {
		return nil // already exactly as we want it
	}
	hooks := []kv{}
	if i := find(top, "hooks"); i >= 0 {
		if hooks, err = parseObj(top[i].v); err != nil {
			return errors.New(i18n.T("hook.hooks_not_object", path))
		}
	}
	var groups []json.RawMessage
	if i := find(hooks, "StopFailure"); i >= 0 {
		if json.Unmarshal(hooks[i].v, &groups) != nil {
			return errors.New(i18n.T("hook.stopfailure_not_array", path))
		}
	}
	cmd := encObj([]kv{{"type", []byte(`"command"`)}, {"command", str(command)}, {"timeout", []byte("5")}})
	group := encObj([]kv{{"hooks", encArr([]json.RawMessage{cmd})}})
	hooks = set(hooks, "StopFailure", encArr(append(groups, group)))
	return save(target, set(top, "hooks", encObj(hooks)), mode)
}

func Remove(path, command string) error {
	target, top, mode, err := read(path)
	if err != nil {
		return err
	}
	top, removed := strip(top, command)
	if len(removed) == 0 {
		return nil
	}
	return save(target, top, mode)
}
