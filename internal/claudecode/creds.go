// Package claudecode knows how Claude Code names and stores its credentials.
package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"time"

	"golang.org/x/text/unicode/norm"
)

const DefaultService = "Claude Code-credentials"

// GOOS is the platform seam (tests flip it).
var GOOS = runtime.GOOS

// CredentialsPath is Claude Code's credentials file on Linux.
func CredentialsPath(home, configDir string) string {
	return filepath.Join(ConfigDir(home, configDir), ".credentials.json")
}

// ServiceName is the keychain service Claude Code uses for configDir ("" = default);
// on Linux it is the credentials file path.
func ServiceName(configDir string) string {
	if GOOS == "linux" {
		home, _ := os.UserHomeDir()
		return CredentialsPath(home, configDir)
	}
	if configDir == "" {
		return DefaultService
	}
	sum := sha256.Sum256([]byte(norm.NFC.String(configDir)))
	return DefaultService + "-" + hex.EncodeToString(sum[:])[:8]
}

var accountRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// KeychainAccount is the keychain account Claude Code uses.
func KeychainAccount() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if !accountRe.MatchString(name) {
		return "claude-code-user"
	}
	return name
}

// ConfigDir is configDir, or ~/.claude when empty, NFC-normalised as Claude Code does.
func ConfigDir(home, configDir string) string {
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	return norm.NFC.String(configDir)
}

// LoginServices are the services Claude Code may keep the login of config dir under: the path as
// given and symlink-resolved. ok=false when dir is the live login's (target): never read it as a
// temporary login or delete it. On Linux any path to target's dir counts.
func LoginServices(dir, target string) (svcs []string, ok bool) {
	svcs = []string{ServiceName(dir)}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		if s := ServiceName(r); s != svcs[0] {
			svcs = append(svcs, s)
		}
	}
	if slices.Contains(svcs, target) {
		return svcs, false
	}
	if GOOS == "linux" {
		a, err := os.Stat(dir)
		b, err2 := os.Stat(filepath.Dir(target))
		if err == nil && err2 == nil && os.SameFile(a, b) {
			return svcs, false
		}
	}
	return svcs, true
}

// GlobalConfigPath is Claude Code's global config file; a legacy .config.json wins.
func GlobalConfigPath(home, configDir string) string {
	legacy := filepath.Join(ConfigDir(home, configDir), ".config.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	if configDir != "" {
		return filepath.Join(configDir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

func SettingsPath(home, configDir string) string {
	return filepath.Join(ConfigDir(home, configDir), "settings.json")
}

type OAuth struct {
	AccessToken           string   `json:"accessToken,omitempty"`
	RefreshToken          string   `json:"refreshToken,omitempty"`
	ExpiresAt             int64    `json:"expiresAt,omitempty"` // ms
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`
	ClientID              string   `json:"clientId,omitempty"`
}

// Credentials is the claudeAiOauth part of the keychain JSON, the only part ccswap owns;
// unknown keys inside it survive a round trip.
type Credentials struct {
	OAuth OAuth
	raw   map[string]json.RawMessage
}

const oauthKey = "claudeAiOauth"

// ErrNoOAuth: the entry has no (or a null) claudeAiOauth, so Claude Code is not logged in; its
// other keys (mcpOAuth) may still be there.
var ErrNoOAuth = errors.New("no claudeAiOauth in credentials")

func ParseCredentials(b []byte) (Credentials, error) {
	var c Credentials
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return c, err
	}
	rawOAuth, ok := top[oauthKey]
	if !ok || bytes.Equal(bytes.TrimSpace(rawOAuth), []byte("null")) {
		return c, ErrNoOAuth
	}
	if err := json.Unmarshal(rawOAuth, &c.raw); err != nil {
		return c, err
	}
	if err := json.Unmarshal(rawOAuth, &c.OAuth); err != nil {
		return c, err
	}
	if c.OAuth.AccessToken == "" && c.OAuth.RefreshToken == "" {
		return c, errors.New("credentials have no tokens")
	}
	return c, nil
}

func (c Credentials) oauthJSON() ([]byte, error) {
	known, err := json.Marshal(c.OAuth)
	if err != nil {
		return nil, err
	}
	var kv map[string]json.RawMessage
	_ = json.Unmarshal(known, &kv)
	oauth := map[string]json.RawMessage{}
	for k, v := range c.raw {
		oauth[k] = v
	}
	// the struct is the source of truth for known keys (omitted = cleared).
	for _, k := range []string{"accessToken", "refreshToken", "expiresAt", "refreshTokenExpiresAt", "scopes", "subscriptionType", "rateLimitTier", "clientId"} {
		delete(oauth, k)
	}
	for k, v := range kv {
		oauth[k] = v
	}
	return json.Marshal(oauth)
}

// Bytes is the vault form: {"claudeAiOauth": …} and nothing else.
func (c Credentials) Bytes() ([]byte, error) {
	o, err := c.oauthJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{oauthKey: o})
}

// WithOAuth returns entry with its claudeAiOauth value replaced by c's; every other byte of entry
// (Claude Code's mcpOAuth logins, unknown keys) stays as it is. A nil entry gives the vault form.
func WithOAuth(entry []byte, c Credentials) ([]byte, error) {
	if entry == nil {
		return c.Bytes()
	}
	o, err := c.oauthJSON()
	if err != nil {
		return nil, err
	}
	if !json.Valid(entry) {
		return nil, errors.New("claude code entry is not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(entry))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("claude code entry is not a JSON object")
	}
	start, end := int64(-1), int64(-1)
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		at := dec.InputOffset() // right after the key: the value's ':' follows
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if k == oauthKey { // the last one wins, as in JSON.parse
			start, end = at, dec.InputOffset()
		}
	}
	if start < 0 { // no login yet: add the key, keep the rest
		if t, err := dec.Token(); err != nil || t != json.Delim('}') {
			return nil, errors.New("claude code entry is not a JSON object")
		}
		closing := dec.InputOffset() - 1
		sep := ","
		if head := bytes.TrimSpace(entry[:closing]); head[len(head)-1] == '{' {
			sep = ""
		}
		out := append([]byte(nil), entry[:closing]...)
		out = append(out, sep+`"`+oauthKey+`":`...)
		return append(append(out, o...), entry[closing:]...), nil
	}
	out := append([]byte(nil), entry[:start]...)
	out = append(append(out, ':'), o...)
	return append(out, entry[end:]...), nil
}

func (c Credentials) Expiry() time.Time { return time.UnixMilli(c.OAuth.ExpiresAt) }
