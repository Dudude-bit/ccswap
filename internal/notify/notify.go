// Package notify shows desktop notifications (macOS).
package notify

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Notifier interface {
	Notify(title, msg string)
}

// Osascript shows a notification through /usr/bin/osascript.
type Osascript struct{}

func (Osascript) Notify(title, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script(title, msg)).Run(); err != nil {
		slog.Warn("notify: osascript failed", "err", err)
	}
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)

// quote makes s an AppleScript string literal.
func quote(s string) string { return `"` + escaper.Replace(s) + `"` }

func script(title, msg string) string {
	return "display notification " + quote(msg) + " with title " + quote(title)
}

// Recorder collects messages (tests).
type Recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *Recorder) Notify(_, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
}

func (r *Recorder) Messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

// None does nothing: the daemon already logs every notification.
type None struct{}

func (None) Notify(_, _ string) {}
