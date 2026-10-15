// Package anthropic is a small client for Claude's OAuth token, usage and profile endpoints.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrInvalidGrant = errors.New("anthropic: invalid_grant")
	// ErrNoAccessToken: a 2xx refresh without an access token. The Tokens returned with it still
	// carry a rotated refresh token, if any, which must be saved: refresh tokens are single-use,
	// so the old one is spent.
	ErrNoAccessToken = errors.New("anthropic: refresh response has no access token")
)

// Timeout bounds each request; a GET whose reused connection was dropped is sent once more.
const Timeout = 15 * time.Second

type Client struct {
	HTTP                        *http.Client
	APIBase, TokenURL, ClientID string
	UserAgent                   string
	Now                         func() time.Time
}

func New() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: Timeout},
		APIBase:   "https://api.anthropic.com",
		TokenURL:  "https://platform.claude.com/v1/oauth/token",
		ClientID:  "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		UserAgent: "ccswap",
		Now:       time.Now,
	}
}

type Tokens struct {
	AccessToken, RefreshToken        string
	ExpiresAt, RefreshTokenExpiresAt time.Time
	Scopes                           []string
}

type Profile struct{ Email string }

func (c *Client) do(ctx context.Context, method, url string, body []byte, hdr map[string]string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil && method == http.MethodGet && body == nil && ctx.Err() == nil {
		resp, err = c.HTTP.Do(req) // once more: the server may have dropped the idle connection we reused
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("anthropic: %s request failed: %w", method, scrub(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

// scrub drops the *url.Error wrapper's URL (it can't hold tokens, but keep errors terse).
func scrub(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		return ue.Unwrap()
	}
	return err
}

func (c *Client) Refresh(ctx context.Context, refreshToken string, scopes []string) (Tokens, error) {
	req := map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": c.ClientID}
	if len(scopes) > 0 {
		req["scope"] = strings.Join(scopes, " ")
	}
	body, _ := json.Marshal(req)
	status, _, b, err := c.do(ctx, http.MethodPost, c.TokenURL, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return Tokens{}, err
	}
	if status/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if status == http.StatusBadRequest && json.Unmarshal(b, &e) == nil && e.Error == "invalid_grant" {
			return Tokens{}, fmt.Errorf("refresh: status %d: %w", status, ErrInvalidGrant)
		}
		return Tokens{}, fmt.Errorf("refresh: unexpected status %d", status)
	}
	var r struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Scope            string `json:"scope"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Tokens{}, fmt.Errorf("refresh: bad response: %w", err)
	}
	if r.AccessToken == "" && r.RefreshToken == "" {
		return Tokens{}, ErrNoAccessToken
	}
	now := c.Now()
	// no expires_in: expired at once, so the next tick refreshes again; never an error here.
	t := Tokens{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: now.Add(time.Duration(r.ExpiresIn) * time.Second)}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	if r.RefreshExpiresIn > 0 {
		t.RefreshTokenExpiresAt = now.Add(time.Duration(r.RefreshExpiresIn) * time.Second)
	}
	if f := strings.Fields(r.Scope); len(f) > 0 {
		t.Scopes = f
	}
	if t.AccessToken == "" {
		t.ExpiresAt = now // never "fresh": the next tick refreshes it again
		return t, ErrNoAccessToken
	}
	return t, nil
}

func (c *Client) Profile(ctx context.Context, accessToken string) (Profile, error) {
	status, _, b, err := c.do(ctx, http.MethodGet, c.APIBase+"/api/oauth/profile", nil, map[string]string{"Authorization": "Bearer " + accessToken})
	if err != nil {
		return Profile{}, err
	}
	if status/100 != 2 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return Profile{}, fmt.Errorf("profile: unexpected status %d %.200s", status, e.Error.Message)
	}
	var r struct {
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Profile{}, fmt.Errorf("profile: bad response: %w", err)
	}
	return Profile{Email: r.Account.Email}, nil
}
