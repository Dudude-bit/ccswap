package claudecode

import (
	"net/url"
	"os"
	"slices"
	"strings"
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
