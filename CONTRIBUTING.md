# Contributing to ccswap

Thanks for helping. Bug reports, fixes, translations and ideas are all welcome. For anything larger than
a small fix, open an issue first so we can agree on the approach before you write the code.

ccswap switches between Claude accounts that one person owns. Changes that share an account between
people, make ccswap look like Claude Code to Anthropic (its user agent or headers), or proxy model
traffic will not be accepted. Report vulnerabilities privately, as [SECURITY.md](SECURITY.md) says, not
in an issue.

## Build and test

You need Go (`go.mod`'s `go` line pins the version; with the default `GOTOOLCHAIN=auto` the `go` command
fetches it), and Node.js (LTS) for the web page's script test. ccswap builds on macOS and Linux.

- `make build` builds `bin/ccswap`.
- `make install` installs it into `$PREFIX/bin`, `~/.local/bin` by default.
- `make vet` runs `go vet ./...`.
- `make test` runs `go test ./...` and, with `node` on `PATH`, `internal/web/i18n.test.js`.

`bash scripts/linux-test.sh` runs the tests on Linux in Docker, as a non-root user, then smoke-tests
`ccswap status` and `ccswap doctor` against a running daemon in English and Russian. Run it when you
touch anything platform-specific (keychain, launchd, systemd, paths, signals) from a Mac.

CI runs the tests on Linux and macOS, plus these checks: that the generated files are current, that
`go.mod` and `go.sum` are tidy, gofmt, and staticcheck and govulncheck at the versions
`.github/workflows/ci.yml` pins. Run them before you open a pull request:

```sh
make check-gen
go mod tidy -diff
test -z "$(gofmt -l .)"
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

After editing `api/openapi.yaml` or changing a dependency, run `make generate` and commit what it
changes: `internal/api/api.gen.go` follows the spec, and `THIRD_PARTY_LICENSES` (written by
`scripts/third-party-licenses.sh`) follows `go.mod`.

Tests must not touch your real keychain, LaunchAgents, systemd units or `~/.claude`. Use the seams the
packages already have: `keychain.NewFake()`, the `Run` and `Dir` variables of `launchd` and `systemd`,
`claudecode.GOOS` for the other platform's code paths, and `t.TempDir()` for every home folder.

## Code

- `gofmt`, `go vet` and staticcheck clean. No new dependencies without an issue that agrees on them.
- Comments say why, not what, and match the density of the code around them.
- English in code, comments, tests and docs. Russian lives only in `internal/i18n/locales/ru.json` and
  `README.ru.md`.
- Keep a change to what ccswap does in step with the docs: `README.md`, `README.ru.md`, and
  `SECURITY.md` when it touches storage, keys or the listener.

## Texts and languages

Every text a user sees goes through the catalog: `i18n.T("key", args…)` in Go, keys under `web.` for
the page. A new or changed text needs its key in `internal/i18n/locales/en.json` (the source, which has
every key) and in every other locale file, today `ru.json`; the tests fail on a key missing from any of
them. If you cannot translate a text, put the English one into `ru.json` (for a plural message, under
every Russian form: `one`, `few`, `many` and `other`) and say so in the pull request: CI stays green and
we translate it before the merge. Tests compare against `i18n.T(key)`, not literal strings, so they read
the same in any language.

### Adding a language

Every text is in `internal/i18n/locales/<tag>.json`, one file per language, built into the binary. A
new language needs no code changes.

1. Copy `en.json` to `<tag>.json` and translate the values. The name is the language's canonical BCP 47
   tag: `de.json`, `pt-BR.json`, `zh-Hant.json` (language in lower case, script in title case, region in
   upper case). `lang.name` is the language's name in itself (`Deutsch`), `web.locale` the BCP 47 locale
   the page shows times in (`de-DE`). A regional file such as `pt-BR.json` serves `pt-BR`; `pt-PT` and
   plain `pt` need a `pt.json`.
2. Keep each message's shape:
   - CLI and daemon messages are Go formats: the same `%` verbs (`%s %d %q %v %w %%`) as English for the
     same arguments, in English's order. To reorder, index them: `%[2]s … %[1]s`.
   - Page messages (keys under `web.`) keep English's `{0}` `{1}` … placeholders, each as many times as
     in English, and as many `**bold**` and `` `code` `` marks; word order may move them.
   - A plural message is an object of CLDR forms (`one`, `few`, `many`, `other` …): give every form your
     language uses, each with the number in it (`%d`, or `{0}` on the page).
   - A few TUI lines are pieces around a value in another style (`tui.moves_here_once` + account +
     `tui.runs_out`; `tui.run`, `tui.checked`, `tui.next_label`): keep their leading and trailing spaces;
     a piece may be empty (`""`) when your word order puts everything in the other one.
3. Keep the file's keys sorted with a 2-space indent: `jq -S --indent 2 . de.json` prints it that way.
4. Run `go test ./internal/i18n/ ./internal/web/` (with node installed it also runs the page's script on
   the texts). It fails on a missing or extra key, a value that is neither a text nor an object of texts, a
   changed verb, placeholder or mark, a missing plural form, a file name or `web.locale` that is not a tag, a
   `web.locale` of another language, a `lang.name` that is empty or another file's, or a file out of order.
5. Open a pull request. `ccswap lang`, the locale match and the page's switch pick the language up.

To propose a language without translating it yourself, open an issue. Since the tests fail until every
language has every key, a new language also needs someone willing to translate new texts as they come.

## The web page

The page in `internal/web` is plain HTML, CSS and JavaScript, embedded in the binary with `go:embed`:
no build step, no npm packages, no frameworks. The daemon serves it with a strict Content Security
Policy (`default-src 'none'`, scripts, styles, fonts and requests from the page's own origin only), so
no inline scripts or styles, no `eval`, and nothing from another host. The fonts are subsets made by
`scripts/fonts.sh` (a maintainer script; `TestFontsDrawEveryText` fails when a new text needs a glyph
they lack).

[DESIGN.md](DESIGN.md) is the page's design system: colours, type, spacing, components and their
states. Use its tokens and components rather than new values, check both the light and the dark theme,
and update DESIGN.md in the same pull request when the design itself changes.

## Commits and pull requests

- One logical change per commit, with a short English subject that names the area:
  `daemon: keep the vault copy until the switch settles`, `web: label the rota in both themes`,
  `docs: …`, `build: …`, `test: …`. Lower case after the colon, no trailing period. Say why in the body
  when the diff does not make it obvious.
- CI must pass on both Linux and macOS.
- Describe what you tested by hand, if anything (for example a real switch on your own accounts).
