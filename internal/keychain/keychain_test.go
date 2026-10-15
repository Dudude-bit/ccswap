package keychain

import (
	"context"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

type call struct {
	stdin string
	args  []string
}

// memRunner emulates security: stores the hex from add-generic-password (stdin or argv), serves find.
type memRunner struct {
	calls   []call
	stored  []byte
	corrupt bool
}

func (m *memRunner) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	m.calls = append(m.calls, call{string(stdin), args})
	var hx string
	if len(args) > 0 && args[0] == "find-generic-password" {
		if m.corrupt {
			return []byte("other\n"), nil
		}
		return append(append([]byte(nil), m.stored...), '\n'), nil
	}
	if len(args) > 0 && args[0] == "add-generic-password" {
		hx = args[len(args)-1]
	} else if i := strings.LastIndex(string(stdin), "-X "); i >= 0 {
		hx = strings.TrimSpace(string(stdin)[i+3:])
	}
	m.stored, _ = hex.DecodeString(hx)
	return nil, nil
}

func TestSecuritySetKeepsSecretOutOfArgv(t *testing.T) {
	m := &memRunner{}
	s := NewSecurity()
	s.Runner = m.run
	if err := s.Set("svc x", "acct", []byte("SECRET")); err != nil {
		t.Fatal(err)
	}
	got := m.calls[0]
	for _, a := range got.args {
		if strings.Contains(a, "SECRET") || strings.Contains(a, "534543524554") {
			t.Fatalf("secret in argv: %v", got.args)
		}
	}
	want := `add-generic-password -U -a "acct" -s "svc x" -X 534543524554`
	if !strings.Contains(got.stdin, want) {
		t.Fatalf("stdin %q lacks %q", got.stdin, want)
	}
}

func TestSecuritySetLargeSecretUsesArgv(t *testing.T) {
	secret := []byte(strings.Repeat("x", 10*1024))
	m := &memRunner{}
	s := NewSecurity()
	s.Runner = m.run
	if err := s.Set("svc", "acct", secret); err != nil {
		t.Fatal(err)
	}
	c := m.calls[0]
	if c.stdin != "" || c.args[0] != "add-generic-password" || c.args[len(c.args)-2] != "-X" ||
		c.args[len(c.args)-1] != hex.EncodeToString(secret) {
		t.Fatalf("want argv path, got %v", c.args[:3])
	}
}

func TestSecuritySetVerifyMismatch(t *testing.T) {
	m := &memRunner{corrupt: true}
	s := NewSecurity()
	s.Runner = m.run
	if err := s.Set("svc", "acct", []byte("v")); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestSecuritySetEscapesQuotes(t *testing.T) {
	m := &memRunner{}
	s := NewSecurity()
	s.Runner = m.run
	_ = s.Set(`a"b\c`, "x", []byte("s"))
	if !strings.Contains(m.calls[0].stdin, `-s "a\"b\\c"`) {
		t.Fatalf("bad escaping: %q", m.calls[0].stdin)
	}
}

func TestSecurityGetNotFound(t *testing.T) {
	s := NewSecurity()
	s.Runner = func(context.Context, []byte, ...string) ([]byte, error) {
		return nil, exitErr(t, 44)
	}
	if _, err := s.Get("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := s.Delete("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete got %v", err)
	}
}

func TestSecurityGetTrimsNewline(t *testing.T) {
	s := NewSecurity()
	s.Runner = func(context.Context, []byte, ...string) ([]byte, error) { return []byte("tok\n"), nil }
	b, err := s.Get("s", "a")
	if err != nil || string(b) != "tok" {
		t.Fatalf("%q %v", b, err)
	}
}

func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+string(rune('0'+code/10))+string(rune('0'+code%10))).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return err
}

func TestFake(t *testing.T) {
	f := NewFake()
	if _, err := f.Get("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	_ = f.Set("s", "a", []byte("v"))
	f.OnGet = func(_, _ string, n int) ([]byte, bool) {
		if n == 3 {
			return []byte("scripted"), true
		}
		return nil, false
	}
	b1, _ := f.Get("s", "a")
	b2, _ := f.Get("s", "a")
	if string(b1) != "v" || string(b2) != "scripted" {
		t.Fatalf("%q %q", b1, b2)
	}
	_ = f.Delete("s", "a")
	if err := f.Delete("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
