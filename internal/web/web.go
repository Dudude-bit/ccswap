// Package web is the browser page of the daemon's HTTP listener.
package web

import (
	"embed"
	"path"

	"github.com/gofiber/fiber/v3"
)

//go:embed index.html app.css app.js i18n.js fonts/geologica.woff2 fonts/jetbrains-mono.woff2 fonts/OFL-Geologica.txt fonts/OFL-JetBrainsMono.txt
var files embed.FS

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Page is where the page lives, as the CLIProxyAPI management panel does.
const Page = "/management.html"

// Register serves the page without a key: it holds no data, and its script asks the API with one.
func Register(app fiber.Router) {
	app.Get("/", func(c fiber.Ctx) error { return c.Redirect().To(Page) })
	for route, name := range map[string]string{Page: "index.html", "/app.css": "app.css", "/app.js": "app.js", "/i18n.js": "i18n.js",
		"/fonts/geologica.woff2": "fonts/geologica.woff2", "/fonts/jetbrains-mono.woff2": "fonts/jetbrains-mono.woff2",
		"/fonts/OFL-Geologica.txt": "fonts/OFL-Geologica.txt", "/fonts/OFL-JetBrainsMono.txt": "fonts/OFL-JetBrainsMono.txt"} {
		b, err := files.ReadFile(name)
		if err != nil {
			panic(err)
		}
		app.Get(route, func(c fiber.Ctx) error {
			c.Set("Content-Security-Policy", csp)
			c.Set("X-Content-Type-Options", "nosniff")
			c.Set("Referrer-Policy", "no-referrer")
			c.Set("Cache-Control", "no-cache")
			switch path.Ext(name) {
			case ".woff2":
				c.Set("Content-Type", "font/woff2")
				return c.Send(b)
			case ".txt":
				c.Set("Content-Type", "text/plain; charset=utf-8")
				return c.Send(b)
			}
			return c.Type(path.Ext(name)).Send(b)
		})
	}
}
