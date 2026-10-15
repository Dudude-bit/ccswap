package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"unicode"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// russian are the log lines in Russian: the tests run the daemon in Russian, and daemon.log stays English
// (an *i18n.Error wrapped by fmt.Errorf would keep the Russian of the moment it was made).
var russian struct {
	sync.Mutex
	lines [][]byte
}

type logWatch struct{}

func (logWatch) Write(p []byte) (int, error) {
	if bytes.ContainsFunc(p, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
		russian.Lock()
		russian.lines = append(russian.lines, bytes.Clone(p))
		russian.Unlock()
	}
	return os.Stderr.Write(p)
}

func TestMain(m *testing.M) {
	i18n.Set("ru")
	slog.SetDefault(slog.New(slog.NewTextHandler(logWatch{}, nil)))
	code := m.Run()
	russian.Lock()
	lines := russian.lines
	russian.Unlock()
	if len(lines) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: Russian in the log, which stays English:\n%s", bytes.Join(lines, nil))
		code = 1
	}
	os.Exit(code)
}
