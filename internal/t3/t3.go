// Package t3 makes ccswap look like a CLIProxyAPI usage hub so T3 Code lists its accounts.
package t3

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/store"
)

type Source interface {
	StoredAccounts() []store.Account
	UsageRaw(ctx context.Context, name string, maxAge time.Duration) (int, []byte, error)
}

const usageURL = "https://api.anthropic.com/api/oauth/usage"

type file struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled"`
}

// Carries reports whether c carries key as a Bearer token; an empty key matches nobody.
func Carries(c fiber.Ctx, key string) bool {
	got, ok := strings.CutPrefix(c.Get("Authorization"), "Bearer ")
	return ok && key != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

// Auth lets through only requests that carry one of keys as a Bearer token.
func Auth(keys ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !slices.ContainsFunc(keys, func(k string) bool { return Carries(c, k) }) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		return c.Next()
	}
}

// Register adds the hub routes to app; they are meant to sit behind Auth.
func Register(app fiber.Router, src Source) {
	app.Get("/v0/management/auth-files", func(c fiber.Ctx) error {
		accts := src.StoredAccounts()
		files := make([]file, 0, len(accts))
		for _, a := range accts {
			files = append(files, file{a.Name, store.AuthIndex(a.Name), "claude", a.Email, !a.Enabled || a.NeedsLogin})
		}
		return c.JSON(fiber.Map{"files": files})
	})
	app.Post("/v0/management/api-call", func(c fiber.Ctx) error {
		var req struct {
			AuthIndex string `json:"auth_index"`
			Method    string `json:"method"`
			URL       string `json:"url"`
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported request"})
		}
		name := ""
		for _, a := range src.StoredAccounts() {
			if req.AuthIndex != "" && store.AuthIndex(a.Name) == req.AuthIndex {
				name = a.Name
			}
		}
		if name == "" {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unknown auth_index"})
		}
		if req.Method != "GET" || req.URL != usageURL {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported request"})
		}
		status, body, err := src.UsageRaw(c.Context(), name, 60*time.Second)
		if err != nil { // T3 drops just this account on a non-2xx status_code
			status, body = 502, nil
		}
		return c.JSON(fiber.Map{"status_code": status, "header": fiber.Map{}, "body": string(body)})
	})
	app.Post("/v0/management/reset-quota", func(c fiber.Ctx) error { return c.JSON(fiber.Map{}) })
}

// Serve runs app on ln until ctx is done, then shuts it down.
func Serve(ctx context.Context, ln net.Listener, app *fiber.App) error {
	errc := make(chan error, 1)
	go func() { errc <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.ShutdownWithContext(sctx)
		return <-errc
	}
}
