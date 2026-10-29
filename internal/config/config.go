// Package config loads the user-edited config.toml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

type Thresholds struct {
	FiveHour, Weekly, Hysteresis float64
	MinDwell                     time.Duration
	// how often Anthropic is asked for the usage of the account in use, and of every other one
	CheckActive, CheckOthers time.Duration
}

type T3 struct {
	Enabled bool
	Listen  string
}

type Claude struct{ ConfigDir string }

// Remote is another ccswap that asks Anthropic for usage; this one takes the limits from it.
type Remote struct{ URL string }

// UI is how ccswap talks: Lang is a pinned language's BCP 47 tag ("ru", "pt-BR"), or "" to follow the locale.
type UI struct{ Lang string }

// Update: Check has the daemon ask GitHub for ccswap's newest release.
type Update struct{ Check bool }

type Config struct {
	PollInterval time.Duration
	Thresholds   Thresholds
	T3           T3
	Claude       Claude
	Remote       Remote
	UI           UI
	Update       Update
}

// DefaultHome is the app dir without $CCSWAP_HOME: ~/Library/Application Support/ccswap on macOS,
// $XDG_CONFIG_HOME/ccswap (~/.config/ccswap) on Linux.
func DefaultHome() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ccswap"), nil
}

// Home returns the app dir ($CCSWAP_HOME or DefaultHome), created/tightened to 0700. A relative
// $CCSWAP_HOME is refused: the service and a CLI run elsewhere would each find another folder.
func Home() (string, error) {
	h := os.Getenv("CCSWAP_HOME")
	switch {
	case h == "":
		var err error
		if h, err = DefaultHome(); err != nil {
			return "", err
		}
	case !filepath.IsAbs(h):
		return "", i18n.Err("config.home_relative", h)
	}
	if err := os.MkdirAll(h, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(h, 0o700); err != nil {
		return "", err
	}
	return h, nil
}

// SocketPath is the daemon's Unix socket in home, refused in plain words when it is too long for a
// socket address (104 bytes on macOS), which would otherwise fail as "invalid argument".
func SocketPath(home string) (string, error) {
	sock, room := filepath.Join(home, "ccswap.sock"), len(syscall.RawSockaddrUnix{}.Path)
	if len(sock) >= room {
		return "", i18n.Err("config.socket_too_long", sock, len(sock), room-1)
	}
	return sock, nil
}

type file struct {
	PollInterval string `toml:"poll_interval"`
	Thresholds   struct {
		FiveHour    float64 `toml:"five_hour"`
		Weekly      float64 `toml:"weekly"`
		Hysteresis  float64 `toml:"hysteresis"`
		MinDwell    string  `toml:"min_dwell"`
		CheckActive string  `toml:"check_active"`
		CheckOthers string  `toml:"check_others"`
	} `toml:"thresholds"`
	T3 struct {
		Enabled bool   `toml:"enabled"`
		Listen  string `toml:"listen"`
	} `toml:"t3"`
	Claude struct {
		ConfigDir string `toml:"config_dir"`
	} `toml:"claude"`
	Remote struct {
		URL string `toml:"url"`
	} `toml:"remote"`
	UI struct {
		Lang string `toml:"lang"`
	} `toml:"ui"`
	Update struct {
		Check bool `toml:"check"`
	} `toml:"update"`
}

func Load(home string) (Config, error) {
	f := file{PollInterval: "60s"}
	f.Thresholds.FiveHour, f.Thresholds.Weekly, f.Thresholds.Hysteresis, f.Thresholds.MinDwell = 95, 98, 10, "10m"
	f.Thresholds.CheckActive, f.Thresholds.CheckOthers = "5m", "30m"
	f.T3.Enabled, f.T3.Listen = true, "127.0.0.1:8318"
	f.Update.Check = true

	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, err
	}
	if err == nil {
		d := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields()
		if err := d.Decode(&f); err != nil {
			var se *toml.StrictMissingError
			if errors.As(err, &se) {
				return Config{}, i18n.Err("config.unknown_key", se.String())
			}
			return Config{}, fmt.Errorf("config.toml: %w", err)
		}
	}

	poll, err := time.ParseDuration(f.PollInterval)
	if err != nil {
		return Config{}, fmt.Errorf("config.toml: poll_interval: %w", err)
	}
	dwell, err := time.ParseDuration(f.Thresholds.MinDwell)
	if err != nil {
		return Config{}, fmt.Errorf("config.toml: thresholds.min_dwell: %w", err)
	}
	active, err := time.ParseDuration(f.Thresholds.CheckActive)
	if err != nil {
		return Config{}, fmt.Errorf("config.toml: thresholds.check_active: %w", err)
	}
	others, err := time.ParseDuration(f.Thresholds.CheckOthers)
	if err != nil {
		return Config{}, fmt.Errorf("config.toml: thresholds.check_others: %w", err)
	}
	t := Thresholds{f.Thresholds.FiveHour, f.Thresholds.Weekly, f.Thresholds.Hysteresis, dwell, active, others}
	if err := t.Check(); err != nil {
		return Config{}, i18n.Err("config.file_error", err)
	}
	if poll < 10*time.Second {
		return Config{}, i18n.Err("config.poll_interval_too_short")
	}
	if f.Claude.ConfigDir != "" && !filepath.IsAbs(f.Claude.ConfigDir) {
		return Config{}, i18n.Err("config.config_dir_not_absolute")
	}
	if f.Remote.URL != "" {
		if err := CheckRemote(f.Remote.URL); err != nil {
			return Config{}, i18n.Err("config.remote_url_error", err)
		}
	}
	if _, ok := i18n.Pin(f.UI.Lang); !ok {
		var codes []string
		for _, l := range i18n.Available() {
			codes = append(codes, strconv.Quote(string(l)))
		}
		return Config{}, i18n.Err("config.invalid_lang", strings.Join(codes, ", "))
	}
	return Config{
		PollInterval: poll,
		Thresholds:   t,
		T3:           T3{f.T3.Enabled, f.T3.Listen},
		Claude:       Claude{f.Claude.ConfigDir},
		Remote:       Remote{f.Remote.URL},
		UI:           UI{f.UI.Lang},
		Update:       Update{f.Update.Check},
	}, nil
}

// CheckRemote accepts an https URL, or http to a loopback or private address: the server's key
// travels with every request. A user:password in the URL is refused: it would sit in config.toml.
func CheckRemote(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return err
	case u.Host == "" || u.Scheme != "https" && u.Scheme != "http":
		return i18n.Err("config.remote_bad_url")
	case u.User != nil:
		return i18n.Err("config.remote_has_userinfo")
	case u.Scheme == "https":
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)) || u.Hostname() == "localhost" {
		return nil
	}
	return i18n.Err("config.remote_plain_http")
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)} // Tailscale and other CGNAT ranges

func (t Thresholds) Check() error {
	switch { // written as !(in range): NaN is in no range
	case !(t.FiveHour > 0 && t.FiveHour <= 100):
		return i18n.Err("config.five_hour_range")
	case !(t.Weekly > 0 && t.Weekly <= 100):
		return i18n.Err("config.weekly_range")
	case !(t.Hysteresis >= 0 && t.Hysteresis < min(t.FiveHour, t.Weekly)):
		return i18n.Err("config.hysteresis_range")
	case t.MinDwell < 0:
		return i18n.Err("config.min_dwell_range")
	case t.CheckActive < time.Minute || t.CheckActive > 24*time.Hour:
		return i18n.Err("config.check_active_range")
	case t.CheckOthers < time.Minute || t.CheckOthers > 24*time.Hour:
		return i18n.Err("config.check_others_range")
	}
	return nil
}

// SaveThresholds rewrites the [thresholds] table of config.toml.
func SaveThresholds(home string, t Thresholds) error {
	return saveTable(home, "thresholds", map[string]any{
		"five_hour": t.FiveHour, "weekly": t.Weekly, "hysteresis": t.Hysteresis, "min_dwell": t.MinDwell.String(),
		"check_active": t.CheckActive.String(), "check_others": t.CheckOthers.String(),
	})
}

// SaveRemote sets [remote] url in config.toml; "" drops the table.
func SaveRemote(home, url string) error {
	if url == "" {
		return saveTable(home, "remote", nil)
	}
	return saveTable(home, "remote", map[string]any{"url": url})
}

// SaveLang sets [ui] lang in config.toml; "" drops the table, so the locale decides.
func SaveLang(home, lang string) error {
	if lang == "" {
		return saveTable(home, "ui", nil)
	}
	return saveTable(home, "ui", map[string]any{"lang": lang})
}

// saveTable replaces one table of config.toml (nil removes it) and keeps the other keys; comments do
// not survive the rewrite. A config.toml that does not parse is left as it is.
func saveTable(home, name string, table map[string]any) error {
	path := filepath.Join(home, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	m := map[string]any{}
	if err := toml.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("config.toml: %w", err)
	}
	if table == nil {
		delete(m, name)
	} else {
		m[name] = table
	}
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, out, 0o600)
}
