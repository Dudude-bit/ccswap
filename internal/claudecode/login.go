package claudecode

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Bin is the claude binary ccswap runs: CCSWAP_CLAUDE_BIN, else claude from PATH.
func Bin() string {
	if b := os.Getenv("CCSWAP_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

// LoginEnv is the environment of a `claude auth login` that keeps its login in dir.
func LoginEnv(dir string) []string {
	env := slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") })
	return append(env, "CLAUDE_CONFIG_DIR="+dir)
}

// AuthCode is a pasted code as `claude auth login` wants it, CODE#STATE: a pasted callback URL is
// turned into that.
func AuthCode(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		if q := u.Query(); q.Get("code") != "" && q.Get("state") != "" {
			return q.Get("code") + "#" + q.Get("state")
		}
	}
	return s
}

var versionRe = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)

// Versions reads the version `claude --version` prints, such as 2.1.319, and asks again after an hour,
// as Claude Code updates itself, or after a minute when claude did not run or printed none; "" then. A
// failure is logged once until its cause changes: without a version Anthropic shows no reset credits.
func Versions() func(context.Context) string {
	var (
		mu     sync.Mutex
		v      string
		until  time.Time
		failed string // the last failure logged, "" after a lookup that worked
	)
	return func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		if time.Now().Before(until) {
			return v
		}
		run, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(run, Bin(), "--version").Output()
		if ctx.Err() != nil { // the caller gave up, which says nothing of claude
			return v
		}
		v, until = "", time.Now().Add(time.Minute)
		if err == nil {
			v = versionRe.FindString(string(out))
		}
		why := ""
		switch {
		case err != nil:
			why = err.Error()
		case v == "":
			why = fmt.Sprintf("no version in %.80q", strings.TrimSpace(string(out)))
		}
		if why != "" && why != failed {
			slog.Warn("claude --version failed: Anthropic shows no reset credits without it", "bin", Bin(), "err", why)
		}
		failed = why
		if v != "" {
			until = time.Now().Add(time.Hour)
		}
		return v
	}
}
