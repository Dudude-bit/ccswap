package switcher

import (
	"os"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// TestMain runs in Russian: tests compare texts with i18n.T, and in a language other than English that
// also catches text printed without the catalog.
func TestMain(m *testing.M) {
	i18n.Set("ru")
	os.Exit(m.Run())
}
