package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c := New()
	c.APIBase, c.TokenURL = s.URL, s.URL+"/token"
	return c
}

func TestRefreshMapsExpiresIn(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		json.NewDecoder(r.Body).Decode(&m)
		if m["grant_type"] != "refresh_token" || m["refresh_token"] != "rt" || m["client_id"] != c0.ClientID || m["scope"] != "a b" {
			t.Errorf("bad body %v", m)
		}
		if r.Header.Get("User-Agent") != "ccswap" {
			t.Errorf("ua %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"access_token":"at","expires_in":3600,"refresh_token_expires_in":7200,"scope":"a b"}`))
	})
	c.Now = func() time.Time { return now }
	tk, err := c.Refresh(context.Background(), "rt", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.AccessToken != "at" || tk.RefreshToken != "rt" || !tk.ExpiresAt.Equal(now.Add(time.Hour)) ||
		!tk.RefreshTokenExpiresAt.Equal(now.Add(2*time.Hour)) || len(tk.Scopes) != 2 {
		t.Errorf("%+v", tk)
	}
}

var c0 = New()

func TestRefreshOmitsEmptyScope(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		json.NewDecoder(r.Body).Decode(&m)
		if _, ok := m["scope"]; ok {
			t.Error("scope sent")
		}
		w.Write([]byte(`{"access_token":"at","refresh_token":"new","expires_in":1}`))
	})
	tk, err := c.Refresh(context.Background(), "rt", nil)
	if err != nil || tk.RefreshToken != "new" || tk.Scopes != nil || !tk.RefreshTokenExpiresAt.IsZero() {
		t.Errorf("%+v %v", tk, err)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	if _, err := c.Refresh(context.Background(), "rt", nil); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("%v", err)
	}
}

func TestRefreshOtherErrorHidesSecrets(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	_, err := c.Refresh(context.Background(), "secret-rt", nil)
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "secret-rt") {
		t.Errorf("%v", err)
	}
}

func TestUsageSendsHeaders(t *testing.T) {
	const body = `{"five_hour":{"utilization":1,"resets_at":null}}`
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || r.URL.Path != "/api/oauth/usage" {
			t.Errorf("bad request %v", r.Header)
		}
		w.Write([]byte(body))
	})
	res, err := c.Usage(context.Background(), "tok")
	if err != nil || string(res.Body) != body || res.Status != 200 || res.Usage.FiveHour == nil {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUsageNon2xxKeepsBody(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte("slow down"))
	})
	res, err := c.Usage(context.Background(), "tok")
	if err != nil || res.Status != 429 || string(res.Body) != "slow down" || res.Usage.FiveHour != nil {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUsageBadJSON2xx(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("nope")) })
	res, err := c.Usage(context.Background(), "tok")
	if err == nil || res.Status != 200 || string(res.Body) != "nope" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestParseUsage(t *testing.T) {
	b, _ := os.ReadFile("testdata/usage.json")
	u, err := ParseUsage(b)
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHour.Utilization != 42 || !u.FiveHour.ResetsAt.Equal(time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)) || u.SevenDay.Utilization != 17.5 {
		t.Errorf("%+v %+v", u.FiveHour, u.SevenDay)
	}
	if len(u.Models) != 2 || u.Models[0].Name != "opus" || u.Models[0].Utilization != 98 || !u.Models[0].ResetsAt.IsZero() ||
		u.Models[1].Name != "Fable" || u.Models[1].Utilization != 60 {
		t.Errorf("%+v", u.Models)
	}
}

func TestProfile(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/profile" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") != "" {
			t.Errorf("bad request %v", r.Header)
		}
		w.Write([]byte(`{"account":{"uuid":"a1","email":"x@y.z"},"organization":{"uuid":"o1"}}`))
	})
	p, err := c.Profile(context.Background(), "tok")
	if err != nil || p != (Profile{"x@y.z"}) {
		t.Errorf("%+v %v", p, err)
	}
	c2 := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth access token is invalid."}}`))
	})
	if _, err := c2.Profile(context.Background(), "tok"); err == nil || !strings.Contains(err.Error(), "401 OAuth access token is invalid.") {
		t.Errorf("%v", err)
	}
}

func TestRefreshWithoutAccessToken(t *testing.T) { // the rotated refresh token is still returned
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	body := `{"access_token":"","refresh_token":"new","refresh_token_expires_in":60}`
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	c.Now = func() time.Time { return now }
	tk, err := c.Refresh(context.Background(), "rt", nil)
	if !errors.Is(err, ErrNoAccessToken) || tk.RefreshToken != "new" || tk.AccessToken != "" || !tk.ExpiresAt.Equal(now) ||
		!tk.RefreshTokenExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("%+v %v", tk, err)
	}
	body = `{"access_token":"","refresh_token":"new","expires_in":3600}` // no access token is never "fresh"
	if tk, err := c.Refresh(context.Background(), "rt", nil); !errors.Is(err, ErrNoAccessToken) || !tk.ExpiresAt.Equal(now) {
		t.Fatalf("%+v %v", tk, err)
	}
	body = `{}`
	if tk, err := c.Refresh(context.Background(), "rt", nil); !errors.Is(err, ErrNoAccessToken) || tk.RefreshToken != "" {
		t.Fatalf("nothing rotated, nothing to save: %+v %v", tk, err)
	}
	body = `{"access_token":"at"}` // no expires_in: expired at once, never an error after a rotation
	if tk, err := c.Refresh(context.Background(), "rt", nil); err != nil || tk.AccessToken != "at" || !tk.ExpiresAt.Equal(now) {
		t.Fatalf("%+v %v", tk, err)
	}
}

func TestParseUsageSkipsBadParts(t *testing.T) {
	b := `{"five_hour":{"utilization":"x","resets_at":null},"seven_day":{"utilization":10,"resets_at":"2026-10-20T00:00:00Z"},
		"seven_day_opus":{"utilization":50,"resets_at":"soon"},"seven_day_sonnet":7,"unknown":{"a":[1]},
		"limits":[{"kind":"weekly_scoped","percent":"x","scope":{"model":{"display_name":"Bad"}}},5,
			{"kind":"weekly_scoped","percent":60,"scope":{"model":{"display_name":"Fable"}}}]}`
	u, err := ParseUsage([]byte(b))
	if err != nil || u.FiveHour != nil || u.SevenDay == nil || u.SevenDay.Utilization != 10 {
		t.Fatalf("%+v %v", u, err)
	}
	if len(u.Models) != 1 || u.Models[0].Name != "Fable" || u.Models[0].Utilization != 60 {
		t.Fatalf("%+v", u.Models)
	}
	if _, err := ParseUsage([]byte(`{"limits":{"x":1}}`)); err != nil {
		t.Fatal("malformed limits must not reject the payload:", err)
	}
	if _, err := ParseUsage([]byte(`[]`)); err == nil {
		t.Fatal("a non-object payload is an error")
	}
}

func TestGetRetriesDroppedConnection(t *testing.T) {
	n := 0
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if n++; n == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Write([]byte(`{"account":{"uuid":"a1","email":"x@y.z"},"organization":{"uuid":"o1"}}`))
	})
	if p, err := c.Profile(context.Background(), "tok"); err != nil || p.Email != "x@y.z" || n != 2 {
		t.Fatalf("%+v %v after %d requests", p, err, n)
	}
}

func TestUsageRetryAfter(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	})
	if res, err := c.Usage(context.Background(), "tok"); err != nil || res.Status != 429 || res.RetryAfter != 2*time.Minute {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestBodyRoundTrips(t *testing.T) {
	reset := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	u := Usage{
		FiveHour: &Window{Utilization: 42},
		SevenDay: &Window{Utilization: 17.5, ResetsAt: reset},
		Models:   []ModelWindow{{"opus", Window{98, time.Time{}}}, {"Fable", Window{60, reset}}},
	}
	got, err := ParseUsage(Body(u))
	if err != nil || !reflect.DeepEqual(got, u) {
		t.Fatalf("%+v %v\n%s", got, err, Body(u))
	}
	if string(Body(Usage{})) != "{}" {
		t.Fatal(string(Body(Usage{})))
	}
}
