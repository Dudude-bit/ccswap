package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	dir := t.TempDir()
	runs, bin := filepath.Join(dir, "runs"), filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho x >> " + runs + "\necho '2.1.319 (Claude Code)'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	v := Versions()
	if got := v(context.Background()); got != "2.1.319" {
		t.Fatal(got)
	}
	v(context.Background())
	if b, _ := os.ReadFile(runs); strings.Count(string(b), "x") != 1 {
		t.Fatalf("claude runs once an hour, not per ask: %q", b)
	}
	t.Setenv("CCSWAP_CLAUDE_BIN", filepath.Join(dir, "missing"))
	if got := Versions()(context.Background()); got != "" {
		t.Fatal("no claude, no version:", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("CCSWAP_CLAUDE_BIN", bin)
	v = Versions()
	if v(ctx); v(context.Background()) != "2.1.319" {
		t.Fatal("a caller that gave up leaves nothing cached")
	}
}
