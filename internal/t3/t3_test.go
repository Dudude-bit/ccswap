package t3

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
)

type fakeSrc struct {
	accts  []store.Account
	status int
	body   string
	err    error
}

func (f *fakeSrc) StoredAccounts() []store.Account { return f.accts }
func (f *fakeSrc) UsageRaw(context.Context, string, time.Duration) (int, []byte, error) {
	return f.status, []byte(f.body), f.err
}

const key = "k3y"

func newSrc() *fakeSrc {
	return &fakeSrc{
		accts:  []store.Account{{Name: "a", Email: "a@example.net", Enabled: true}, {Name: "b", Enabled: true, NeedsLogin: true}},
		status: 200,
		body:   `{"five_hour":{"utilization":12,"resets_at":null},"seven_day":{"utilization":3,"resets_at":"2026-10-20T00:00:00Z"}}`,
	}
}

func hub(s Source, key string) *fiber.App {
	app := fiber.New()
	app.Use(Auth(key))
	Register(app, s)
	return app
}

func do(t *testing.T, s Source, method, path, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hub(s, key).Test(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

var auth = map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}

func call(t *testing.T, s Source, idx, method, url string) (int, []byte) {
	b, _ := json.Marshal(map[string]any{"auth_index": idx, "method": method, "url": url,
		"header": map[string]string{"Authorization": "Bearer $TOKEN$"}})
	return do(t, s, "POST", "/v0/management/api-call", string(b), auth)
}

func TestAuthRequired(t *testing.T) {
	for _, h := range []map[string]string{nil, {"Authorization": "Bearer nope"}, {"X-Management-Key": key}} {
		code, b := do(t, newSrc(), "GET", "/v0/management/auth-files", "", h)
		if code != 401 || string(b) != `{"error":"unauthorized"}` {
			t.Fatalf("%v: %d %s", h, code, b)
		}
	}
}

func TestAuthFilesShape(t *testing.T) {
	code, b := do(t, newSrc(), "GET", "/v0/management/auth-files", "", auth)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var got struct{ Files []map[string]any }
	if err := json.Unmarshal(b, &got); err != nil || len(got.Files) != 2 {
		t.Fatal(err, string(b))
	}
	a, c := got.Files[0], got.Files[1]
	if a["id"] != "a" || a["auth_index"] != store.AuthIndex("a") || a["provider"] != "claude" || a["email"] != "a@example.net" || a["disabled"] != false {
		t.Fatalf("a %v", a)
	}
	if _, has := c["email"]; has || c["disabled"] != true {
		t.Fatalf("b %v", c)
	}
}

func TestAPICallReturnsRawBodyString(t *testing.T) {
	s := newSrc()
	code, b := call(t, s, store.AuthIndex("a"), "GET", usageURL)
	var r struct {
		StatusCode int            `json:"status_code"`
		Header     map[string]any `json:"header"`
		Body       string         `json:"body"`
	}
	if code != 200 || json.Unmarshal(b, &r) != nil || r.StatusCode != 200 || r.Body != s.body || r.Header == nil {
		t.Fatalf("%d %s", code, b)
	}
	s.status, s.body = 429, `{"error":"rate"}`
	code, b = call(t, s, store.AuthIndex("a"), "GET", usageURL)
	if err := json.Unmarshal(b, &r); code != 200 || err != nil || r.StatusCode != 429 || r.Body != s.body {
		t.Fatalf("%d %s", code, b)
	}
}

func TestAPICallRejectsOtherURLs(t *testing.T) {
	s := newSrc()
	idx := store.AuthIndex("a")
	if code, _ := call(t, s, idx, "POST", usageURL); code != 400 {
		t.Fatal(code)
	}
	if code, _ := call(t, s, idx, "GET", "https://evil.example/x"); code != 400 {
		t.Fatal(code)
	}
	if code, _ := call(t, s, "nope", "GET", usageURL); code != 404 {
		t.Fatal(code)
	}
	if code, _ := call(t, s, "", "GET", usageURL); code != 404 {
		t.Fatal(code)
	}
}

func TestAPICallUsageErrorIs502(t *testing.T) {
	s := newSrc()
	s.err = errors.New("boom")
	code, b := call(t, s, store.AuthIndex("a"), "GET", usageURL)
	var r struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if code != 200 || json.Unmarshal(b, &r) != nil || r.StatusCode != 502 || r.Body != "" {
		t.Fatalf("%d %s", code, b)
	}
}

func TestDecodesLikeT3(t *testing.T) {
	_, b := call(t, newSrc(), store.AuthIndex("a"), "GET", usageURL)
	var r struct{ Body string }
	_ = json.Unmarshal(b, &r)
	var u map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.Body), &u); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"five_hour", "seven_day"} {
		if _, ok := u[w]["resets_at"]; !ok {
			t.Fatalf("%s lost resets_at", w)
		}
	}
}

func TestResetQuotaAndUnknownPath(t *testing.T) {
	if code, b := do(t, newSrc(), "POST", "/v0/management/reset-quota", "{}", auth); code != 200 || string(b) != "{}" {
		t.Fatal(code, string(b))
	}
	if code, _ := do(t, newSrc(), "GET", "/v0/other", "", auth); code != http.StatusNotFound {
		t.Fatal(code)
	}
}

func TestKeyStable(t *testing.T) {
	kc := keychain.NewFake()
	a, err := Key(kc, KeyAccount)
	if err != nil || len(a) != 64 {
		t.Fatal(a, err)
	}
	b, _ := Key(kc, KeyAccount)
	if a != b {
		t.Fatal("key changed")
	}
	if admin, err := Key(kc, AdminKeyAccount); err != nil || len(admin) != 64 || admin == a {
		t.Fatal("the admin key must be its own:", admin, err)
	}
	if store.ValidName(KeyAccount) || store.ValidName(AdminKeyAccount) {
		t.Fatal("key accounts must not be valid account names")
	}
}

func TestEmptyKeyNeverAuthorizes(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/management/auth-files", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, err := hub(newSrc(), "").Test(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatal("an empty key must authorize nobody", resp.StatusCode, err)
	}
	kc := keychain.NewFake()
	kc.Set(store.VaultService, KeyAccount, []byte{})
	k, err := Key(kc, KeyAccount)
	if err != nil || len(k) != 64 {
		t.Fatal("an empty stored key must be replaced:", k, err)
	}
	if b, _ := kc.Get(store.VaultService, KeyAccount); string(b) != k {
		t.Fatal("new key not stored")
	}
}
