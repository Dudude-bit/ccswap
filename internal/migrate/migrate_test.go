package migrate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func sample() []Account {
	return []Account{
		{Name: "personal", Email: "personal@example.com", Priority: 1, Enabled: true,
			OAuthAccount: []byte(`{"emailAddress":"personal@example.com"}`),
			Credentials:  []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-AAAA","refreshToken":"sk-ant-ort01-BBBB"}}`)},
		{Name: "work", Email: "work@example.org", Priority: 2, Enabled: false,
			Credentials: []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-CCCC","refreshToken":"sk-ant-ort01-DDDD"}}`)},
	}
}

func mustSeal(t *testing.T) ([]byte, string) {
	t.Helper()
	b, code, err := Seal(sample())
	if err != nil {
		t.Fatal(err)
	}
	return b, code
}

// reencode rewrites the decoded body of a bundle through f.
func reencode(t *testing.T, bundle []byte, f func(raw []byte)) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(bundle)), "\n")
	raw, err := base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-1], ""))
	if err != nil {
		t.Fatal(err)
	}
	f(raw)
	return []byte(lines[0] + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n" + lines[len(lines)-1] + "\n")
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, code := mustSeal(t)
	got, err := Open(b, code)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sample()) {
		t.Fatalf("got %+v", got)
	}
}

func TestCodeShape(t *testing.T) {
	_, code := mustSeal(t)
	if !regexp.MustCompile(`^[A-Z2-7]{4}(-[A-Z2-7]{4}){7}$`).MatchString(code) {
		t.Fatalf("bad code %q", code)
	}
	raw, err := ParseCode(code)
	if err != nil || len(raw) != 20 {
		t.Fatalf("len %d err %v", len(raw), err)
	}
}

func TestSealIsRandom(t *testing.T) {
	b1, c1 := mustSeal(t)
	b2, c2 := mustSeal(t)
	if bytes.Equal(b1, b2) || c1 == c2 {
		t.Fatal("seal is deterministic")
	}
}

func TestBundleHidesSecrets(t *testing.T) {
	b, code := mustSeal(t)
	s := string(b)
	for _, bad := range []string{code, strings.ReplaceAll(code, "-", ""), "sk-ant", "personal", "example"} {
		if strings.Contains(s, bad) {
			t.Fatalf("bundle contains %q", bad)
		}
	}
	// the decoded body must not leak either (catches a base64-wrapped plaintext)
	var body []byte
	reencode(t, b, func(raw []byte) { body = append(body, raw...) })
	secret, _ := ParseCode(code)
	plain, _ := json.Marshal(payload{Version: 1, Accounts: sample()})
	for _, bad := range [][]byte{secret, plain, []byte("sk-ant"), []byte("@example")} {
		if bytes.Contains(body, bad) {
			t.Fatalf("decoded body contains %q", bad)
		}
	}
}

func TestWrongCode(t *testing.T) {
	b, code := mustSeal(t)
	_, other := mustSeal(t)
	if other == code {
		t.Fatal("same code")
	}
	if _, err := Open(b, other); !errors.Is(err, ErrBadCode) {
		t.Fatalf("got %v", err)
	}
}

func TestTamperedBundle(t *testing.T) {
	b, code := mustSeal(t)

	// flip one base64 char in the body (lands in the salt: key mismatch)
	lines := strings.Split(string(b), "\n")
	body := []byte(lines[1])
	if body[10] == 'A' {
		body[10] = 'B'
	} else {
		body[10] = 'A'
	}
	lines[1] = string(body)
	if _, err := Open([]byte(strings.Join(lines, "\n")), code); !errors.Is(err, ErrBadCode) {
		t.Fatalf("body flip: %v", err)
	}

	// each region flipped on its own; the nonce, ciphertext and tag keep the key correct,
	// so only GCM authentication can refuse them
	flips := map[string]func(raw []byte){
		"salt":       func(raw []byte) { raw[0] ^= 1 },
		"nonce":      func(raw []byte) { raw[saltLen] ^= 1 },
		"ciphertext": func(raw []byte) { raw[saltLen+nonceLen] ^= 1 },
		"tag":        func(raw []byte) { raw[len(raw)-1] ^= 1 },
	}
	for name, f := range flips {
		if _, err := Open(reencode(t, b, f), code); !errors.Is(err, ErrBadCode) {
			t.Fatalf("%s flip: %v", name, err)
		}
	}
}

// A header that still parses as v1 but differs in bytes must fail on the AAD alone.
func TestHeaderIsAuthenticated(t *testing.T) {
	b, code := mustSeal(t)
	for _, v := range []string{"v01", "v001"} {
		h := bytes.Replace(b, []byte(" v1-----"), []byte(" "+v+"-----"), 1)
		if err := Check(h); err != nil {
			t.Fatalf("%s check: %v", v, err)
		}
		if _, err := Open(h, code); !errors.Is(err, ErrBadCode) {
			t.Fatalf("%s open: %v", v, err)
		}
	}
}

func TestTruncatedBundle(t *testing.T) {
	b, code := mustSeal(t)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")

	noEnd := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	if _, err := Open([]byte(noEnd), code); !errors.Is(err, ErrNotBundle) {
		t.Fatalf("no END: %v", err)
	}

	body := strings.Join(lines[1:len(lines)-1], "")
	cut := lines[0] + "\n" + body[:len(body)/2] + "\n" + lines[len(lines)-1] + "\n"
	if _, err := Open([]byte(cut), code); !errors.Is(err, ErrNotBundle) && !errors.Is(err, ErrBadCode) {
		t.Fatalf("half body: %v", err)
	}

	for _, in := range []string{"", "\n", lines[0], lines[0] + "\n" + lines[len(lines)-1]} {
		if _, err := Open([]byte(in), code); !errors.Is(err, ErrNotBundle) {
			t.Fatalf("%q: %v", in, err)
		}
		if err := Check([]byte(in)); !errors.Is(err, ErrNotBundle) {
			t.Fatalf("check %q: %v", in, err)
		}
	}
}

func TestNewerVersion(t *testing.T) {
	b, code := mustSeal(t)
	v2 := bytes.Replace(b, []byte(" v1-----"), []byte(" v2-----"), 1)
	if err := Check(v2); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("check: %v", err)
	}
	if _, err := Open(v2, code); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("open: %v", err)
	}
	if _, err := Open(v2, "bad"); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("v2 with bad code: %v", err)
	}
	if _, err := Open([]byte("junk"), "bad"); !errors.Is(err, ErrNotBundle) {
		t.Fatalf("junk with bad code: %v", err)
	}
	if err := Check(b); err != nil {
		t.Fatal(err)
	}
}

func TestParseCodeTolerance(t *testing.T) {
	_, code := mustSeal(t)
	want, err := ParseCode(code)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.ReplaceAll(code, "-", "")
	for _, in := range []string{strings.ToLower(code), strings.ReplaceAll(code, "-", " "), flat, " " + strings.ToLower(flat) + "\n"} {
		got, err := ParseCode(in)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	for _, in := range []string{flat[:31], flat + "A", "0" + flat[1:], "1" + flat[1:], "8" + flat[1:], ""} {
		if _, err := ParseCode(in); !errors.Is(err, ErrCodeFormat) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

func TestOpenToleratesPasteNoise(t *testing.T) {
	b, code := mustSeal(t)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	noisy := "\r\n  \r\n  " + lines[0] + "  \r\n" + lines[1][:20] + "\r\n\r\n" + lines[1][20:] + "\r\n" +
		strings.Join(lines[2:], "\r\n") + "\r\n  \r\n\r\n"
	got, err := Open([]byte(noisy), code)
	if err != nil || !reflect.DeepEqual(got, sample()) {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestHugeInputDoesNotPanic(t *testing.T) {
	_, code := mustSeal(t)
	in := "-----BEGIN CCSWAP BUNDLE v99999999999999999999-----\n" + strings.Repeat("A", 1<<16) + "\n-----END CCSWAP BUNDLE-----\n"
	if err := Check([]byte(in)); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("check: %v", err)
	}
	if _, err := Open([]byte(in), code); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("open: %v", err)
	}
}

func TestCredentialsCompactedNotEscaped(t *testing.T) {
	in := []Account{{Name: "a", Credentials: []byte("{\n  \"claudeAiOauth\": {\"x\": \"a&b<c>\"}\n}")}}
	b, code, err := Seal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(b, code)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"claudeAiOauth":{"x":"a&b<c>"}}`; string(got[0].Credentials) != want {
		t.Fatalf("got %s", got[0].Credentials)
	}
}
