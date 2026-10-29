package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/update"
)

// releases stands in for GitHub: Latest answers latest (or err), Install records what it put where.
type releases struct {
	mu         sync.Mutex
	latest     string
	err        error
	asks       int
	ua         string
	installed  []string
	installErr error
}

func (r *releases) seam(t *testing.T) {
	t.Helper()
	oldLatest, oldInstall, oldAfter := latestRelease, installRelease, releaseAfter
	t.Cleanup(func() { latestRelease, installRelease, releaseAfter = oldLatest, oldInstall, oldAfter })
	releaseAfter = time.Millisecond
	latestRelease = func(_ context.Context, ua string) (update.Release, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.asks++
		r.ua = ua
		return update.Release{Version: r.latest, URL: "https://github.com/Dudude-bit/ccswap/releases/tag/" + r.latest}, r.err
	}
	installRelease = func(_ context.Context, rel update.Release, bin string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.installErr != nil {
			return r.installErr
		}
		r.installed = append(r.installed, rel.Version+" "+bin)
		return nil
	}
}

func (r *releases) set(f func(*releases)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func released(e *tenv, check bool) {
	e.d.o.Version, e.d.o.Bin, e.d.o.Config.Update.Check = "v0.1.0", "/opt/ccswap/ccswap", check
}

func TestStatusShowsNewerRelease(t *testing.T) {
	r := &releases{latest: "v0.2.0"}
	r.seam(t)
	e := newTenv(t)
	released(e, true)
	run(t, e)
	waitFor(t, "the release in status", func() bool {
		st, _ := e.d.Status(context.Background())
		return st.Update != nil
	})
	st, _ := e.d.Status(context.Background())
	if st.Update.Version != "v0.2.0" || st.Update.Url != "https://github.com/Dudude-bit/ccswap/releases/tag/v0.2.0" {
		t.Fatalf("%+v", st.Update)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ua != "ccswap/v0.1.0" {
		t.Fatalf("user agent %q", r.ua)
	}
}

func TestStatusQuietWithoutNewerRelease(t *testing.T) {
	for _, c := range []struct {
		name, latest, version string
		err                   error
	}{
		{"latest", "v0.1.0", "v0.1.0", nil},
		{"ahead", "v0.1.0", "v0.1.1-0.20261023061355-3b0e643abcde", nil},
		{"dev build", "v0.2.0", "dev", nil},
		{"github down", "", "v0.1.0", i18n.Err("update.github_status", 503)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &releases{latest: c.latest, err: c.err}
			r.seam(t)
			e := newTenv(t)
			released(e, true)
			e.d.o.Version = c.version
			run(t, e)
			waitFor(t, "an ask", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.asks > 0 })
			if st, _ := e.d.Status(context.Background()); st.Update != nil {
				t.Fatalf("update shown: %+v", st.Update)
			}
		})
	}
}

func TestNoAsksWhenCheckIsOff(t *testing.T) {
	r := &releases{latest: "v0.2.0"}
	r.seam(t)
	e := newTenv(t)
	released(e, false)
	run(t, e)
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.asks != 0 {
		t.Fatalf("asked GitHub %d times with [update] check off", r.asks)
	}
}

func TestInstallUpdateRestarts(t *testing.T) {
	r := &releases{latest: "v0.2.0"}
	r.seam(t)
	e := newTenv(t)
	released(e, false)
	done := make(chan error, 1)
	go func() { done <- e.d.Run(context.Background()) }()
	waitFor(t, "first tick", func() bool {
		st, _ := e.d.Status(context.Background())
		return st.LastPollAt != nil
	})
	c, err := Dial(e.d.o.Socket)
	if err != nil {
		t.Fatal(err)
	}
	up, err := c.InstallUpdateWithResponse(context.Background()) // the answer gets out before the daemon stops
	if err != nil || up.StatusCode() != 200 || up.JSON200.Version != "v0.2.0" {
		t.Fatalf("%v %v", err, up)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestart) {
			t.Fatal("Run:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop for the restart")
	}
	if len(r.installed) != 1 || r.installed[0] != "v0.2.0 /opt/ccswap/ccswap" {
		t.Fatalf("installed %v", r.installed)
	}
	if _, err := e.d.InstallUpdate(context.Background()); !errors.Is(err, ErrUpdating) {
		t.Fatal("a second update while restarting:", err)
	}
}

func TestInstallUpdateRefusals(t *testing.T) {
	r := &releases{latest: "v0.1.0"}
	r.seam(t)
	e := newTenv(t)
	released(e, false)
	run(t, e)
	for range 2 { // a refusal leaves no update running
		if _, err := e.d.InstallUpdate(context.Background()); !errors.Is(err, ErrNoUpdate) {
			t.Fatal("no newer release:", err)
		}
	}
	r.set(func(r *releases) { r.latest, r.installErr = "v0.2.0", i18n.Err("update.checksum_mismatch", "x.tar.gz") })
	_, err := e.d.InstallUpdate(context.Background())
	if c, body := classify(err); c != 502 || body.Code != "update_failed" || body.Message != i18n.T("update.checksum_mismatch", "x.tar.gz") {
		t.Fatalf("%v: %d %+v", err, c, body)
	}
	r.set(func(r *releases) { r.err = i18n.Err("update.github_status", 503) })
	if _, err := e.d.InstallUpdate(context.Background()); !errors.As(err, new(updateError)) {
		t.Fatal("GitHub down:", err)
	}
	e.d.o.Bin = ""
	r.set(func(r *releases) { r.err, r.installErr = nil, nil })
	if _, err := e.d.InstallUpdate(context.Background()); !errors.As(err, new(updateError)) {
		t.Fatal("no binary path:", err)
	}
	if len(r.installed) != 0 {
		t.Fatalf("installed %v", r.installed)
	}
	if st, _ := e.d.Status(context.Background()); st.LastPollAt == nil {
		t.Fatal("the daemon stopped")
	}
}
