package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/store"
)

func inEnglish(t *testing.T) {
	i18n.Set(i18n.EN)
	t.Cleanup(func() { i18n.Set("ru") })
}

func TestNotificationInEnglish(t *testing.T) {
	inEnglish(t)
	e := newTenv(t)
	off := false
	for _, n := range []string{"a", "b"} {
		if _, err := e.d.Patch(context.Background(), n, api.AccountPatch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
	}
	e.poll()
	e.clk.Add(time.Minute)
	e.poll()
	e.msgs("The current account a is disabled and no other is available")
}

func TestUsage429ErrorFollowsLanguage(t *testing.T) {
	e := newTenv(t)
	e.poll()
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = resp{429, `{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`}
	})
	e.clk.Add(time.Minute)
	e.poll()
	if v := e.view("b"); v.Error == nil || !strings.HasPrefix(*v.Error, "Anthropic просит реже спрашивать лимиты; повторю в ") {
		t.Fatalf("ru: %+v", v.Error)
	}
	inEnglish(t)
	e.api.set(func(f *fakeAnthropic) {
		f.usage["atB"] = resp{429, `{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`}
	})
	e.clk.Add(11 * time.Minute)
	e.poll()
	if v := e.view("b"); v.Error == nil || !strings.HasPrefix(*v.Error, "Anthropic asks to check limits less often; will retry at ") {
		t.Fatalf("en: %v", v.Error)
	}
}

func TestSetLangChangesNextMessage(t *testing.T) {
	e := newTenv(t)
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "ru_RU.UTF-8")
	t.Cleanup(func() { i18n.Set("ru") })
	if err := e.d.SetLang(context.Background(), "en"); err != nil {
		t.Fatal(err)
	}
	e.d.notify(loginAgain("a"))
	e.msgs("Log in again: ccswap login a")
	if err := e.d.SetLang(context.Background(), "ru"); err != nil {
		t.Fatal(err)
	}
	e.d.notify(loginAgain("a"))
	e.msgs("Log in again: ccswap login a", "Нужно войти заново: ccswap login a")
}

// TestLogsInEnglish: the daemon speaks Russian, daemon.log stays English: notifications and switch warnings,
// joined ones too.
func TestLogsInEnglish(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := newTenv(t)
	e.api.set(func(f *fakeAnthropic) { f.usage["atA"] = usageBody(96) })
	e.poll()
	e.msgs("Переключил на b: кончился лимит у a")

	e = newTenv(t)
	e.st.Update(func(s *store.State) error { s.Accounts[1].OAuthAccount = nil; return nil })
	if err := os.Chmod(e.home, 0o500); err != nil {
		t.Fatal(err)
	}
	st, err := e.d.Activate(context.Background(), "b", false)
	os.Chmod(e.home, 0o700)
	if err != nil || st.Warning == nil || !strings.HasPrefix(*st.Warning, "у b нет сохранённого oauthAccount, .claude.json не обновлён\nпереключено, но состояние не сохранено: ") {
		t.Fatalf("the warning shown is Russian: %v %v", err, st.Warning)
	}

	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for _, want := range []string{
		`msg="Switched to b: a hit its limit"`,
		`warning="b has no saved oauthAccount, .claude.json not updated"`,
		`warning="switched, but the state was not saved: `,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %s:\n%s", want, logs)
		}
	}
	if regexp.MustCompile(`\p{Cyrillic}`).MatchString(logs) {
		t.Errorf("Russian in the log:\n%s", logs)
	}
}
