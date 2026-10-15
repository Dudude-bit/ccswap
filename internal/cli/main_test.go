package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// TestMain runs in Russian: tests compare texts with i18n.T, and in a language other than English that
// also catches text printed without the catalog.
func TestMain(m *testing.M) {
	i18n.Set("ru")
	os.Exit(mainLoop(m.Run))
}

func english(t *testing.T) {
	t.Helper()
	i18n.Set(i18n.EN)
	t.Cleanup(func() { i18n.Set("ru") })
}

var verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z]`)

// anyArgs matches key's text in the current language, whatever its arguments.
func anyArgs(key string) *regexp.Regexp {
	parts := verb.Split(i18n.T(key), -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(strings.Join(parts, ".*"))
}
