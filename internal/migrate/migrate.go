// Package migrate packs accounts into an encrypted bundle for moving them
// between machines. The bundle is useless without the one-time code.
package migrate

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const (
	bundleVersion = 1
	beginSuffix   = "-----"
	hkdfInfo      = "ccswap migrate v1"
	saltLen       = 16
	nonceLen      = 12
	tagLen        = 16
	codeLen       = 20
	keyLen        = 32
	lineWidth     = 64
)

// BeginPrefix starts the first line of a bundle, before its version; EndLine is its last line. A reader
// keeps what lies between them, whatever text surrounds them.
const (
	BeginPrefix = "-----BEGIN CCSWAP BUNDLE v"
	EndLine     = "-----END CCSWAP BUNDLE-----"
)

var (
	ErrNotBundle    = errors.New("not a ccswap bundle")
	ErrNewerVersion = errors.New("bundle made by a newer ccswap")
	ErrCodeFormat   = errors.New("malformed code")
	ErrBadCode      = errors.New("wrong code or damaged bundle")
)

// Account is one migrated account. Credentials is the vault form
// {"claudeAiOauth":{...}}, preserved semantically (JSON is compacted).
type Account struct {
	Name         string          `json:"name"`
	Email        string          `json:"email"`
	Priority     int             `json:"priority"`
	Enabled      bool            `json:"enabled"`
	OAuthAccount json.RawMessage `json:"oauth_account,omitempty"`
	Credentials  json.RawMessage `json:"credentials"`
}

type payload struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Seal encrypts accts under a fresh random code. The code is returned in
// display form (8 groups of 4) and is not stored anywhere in the bundle.
func Seal(accts []Account) (bundle []byte, code string, err error) {
	var pb bytes.Buffer
	enc := json.NewEncoder(&pb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload{Version: bundleVersion, Accounts: accts}); err != nil {
		return nil, "", err
	}
	plain := bytes.TrimSuffix(pb.Bytes(), []byte("\n"))
	secret := make([]byte, codeLen)
	salt := make([]byte, saltLen)
	nonce := make([]byte, nonceLen)
	for _, b := range [][]byte{secret, salt, nonce} {
		if _, err := rand.Read(b); err != nil {
			return nil, "", err
		}
	}
	gcm, err := newGCM(secret, salt)
	if err != nil {
		return nil, "", err
	}
	begin := BeginPrefix + strconv.Itoa(bundleVersion) + beginSuffix
	raw := append(append(salt, nonce...), gcm.Seal(nil, nonce, plain, aad(begin, salt))...)
	body := base64.StdEncoding.EncodeToString(raw)

	var buf bytes.Buffer
	buf.WriteString(begin + "\n")
	for len(body) > lineWidth {
		buf.WriteString(body[:lineWidth] + "\n")
		body = body[lineWidth:]
	}
	buf.WriteString(body + "\n" + EndLine + "\n")

	s := b32.EncodeToString(secret)
	groups := make([]string, 0, len(s)/4)
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return buf.Bytes(), strings.Join(groups, "-"), nil
}

// Open decrypts a bundle. A wrong code and a damaged body are
// indistinguishable by design.
func Open(bundle []byte, code string) ([]Account, error) {
	begin, raw, err := parse(bundle)
	if err != nil {
		return nil, err
	}
	secret, err := ParseCode(code)
	if err != nil {
		return nil, err
	}
	salt, nonce, ct := raw[:saltLen], raw[saltLen:saltLen+nonceLen], raw[saltLen+nonceLen:]
	gcm, err := newGCM(secret, salt)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, aad(begin, salt))
	if err != nil {
		return nil, ErrBadCode
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil || p.Version != bundleVersion {
		return nil, ErrBadCode
	}
	return p.Accounts, nil
}

// Check validates the armor and version without a code.
func Check(bundle []byte) error {
	_, _, err := parse(bundle)
	return err
}

// ParseCode accepts a code in any case, with spaces or dashes (or neither),
// and returns its 20 bytes.
func ParseCode(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(s))
	if len(s) != b32.EncodedLen(codeLen) {
		return nil, ErrCodeFormat
	}
	raw, err := b32.DecodeString(s)
	if err != nil || len(raw) != codeLen {
		return nil, ErrCodeFormat
	}
	return raw, nil
}

func newGCM(secret, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, secret, salt, hkdfInfo, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// aad binds the header line and the salt to the ciphertext.
func aad(begin string, salt []byte) []byte {
	return append([]byte(begin), salt...)
}

// parse returns the BEGIN line and the decoded body (salt‖nonce‖ciphertext).
func parse(bundle []byte) (begin string, raw []byte, err error) {
	var lines []string
	for _, l := range strings.Split(string(bundle), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 3 || !strings.HasPrefix(lines[0], BeginPrefix) || !strings.HasSuffix(lines[0], beginSuffix) {
		return "", nil, ErrNotBundle
	}
	num := strings.TrimSuffix(strings.TrimPrefix(lines[0], BeginPrefix), beginSuffix)
	if num == "" || strings.Trim(num, "0123456789") != "" {
		return "", nil, ErrNotBundle
	}
	// An overflowing number is certainly newer than ours.
	if v, err := strconv.Atoi(num); err != nil || v > bundleVersion {
		return "", nil, ErrNewerVersion
	} else if v < bundleVersion {
		return "", nil, ErrNotBundle
	}
	if lines[len(lines)-1] != EndLine {
		return "", nil, ErrNotBundle
	}
	raw, err = base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-1], ""))
	if err != nil || len(raw) < saltLen+nonceLen+tagLen {
		return "", nil, ErrNotBundle
	}
	return lines[0], raw, nil
}
