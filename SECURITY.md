# Security policy

ccswap holds the OAuth logins of several Claude accounts and runs a local HTTP listener. This page says
how to report a vulnerability and what ccswap keeps where.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: the repository's **Security** tab, **Report a
vulnerability** (<https://github.com/Dudude-bit/ccswap/security/advisories/new>). Please do not open a
public issue, discussion or pull request for a vulnerability.

Include the ccswap version (`ccswap --version`), the OS, and the steps or a proof of concept. Leave out
real tokens, emails and keys: describe them instead. ccswap is maintained in spare time; expect a first
reply within 14 days. Fixes go into the next release, and the advisory is published with it.

Only the latest release gets fixes.

## Scope

In scope: how ccswap stores logins and keys, the local API (the Unix socket and the TCP listener), the
web page, the `remote` client, `migrate` bundles, and what ccswap writes into Claude Code's files.

Out of scope: Claude Code and Anthropic's services (report those to Anthropic), and attacks that need
code already running as your user: such code can read ccswap's items the same way it can read Claude
Code's own login.

## What ccswap stores, and where

ccswap's folder is `~/Library/Application Support/ccswap` on macOS and `~/.config/ccswap` on Linux
(`CCSWAP_HOME` overrides it). It holds `config.toml`, `state.json`, the Unix socket and `daemon.log`;
none of them hold tokens.

On macOS the secrets live in the keychain under the service `ccswap`:

- one item per inactive account: its login (the access and refresh tokens) while Claude Code is not
  using it;
- `_t3-management-key`, the read key of the TCP listener;
- `_web-admin-key`, the admin key of the TCP listener;
- `_remote-key`, the read key of another ccswap set with `ccswap remote set`;
- `_fingerprint`, a SHA-256 of the current login's refresh token with the account's name and email,
  without any token.

On Linux there is no keychain: the same items are 0600 files in `vault/` (a 0700 folder) inside
ccswap's folder. Do not keep ccswap's folder in git or a dotfiles repository: on Linux it holds your
refresh tokens, and a copy used on another machine spoils the original's login.

During `ccswap login` or a login from the page the new login waits in a temporary `login-*` folder
(0700) inside ccswap's folder until the daemon takes it into the vault: on Linux in
`login-*/.credentials.json` (0600), on macOS in a keychain item `Claude Code-credentials-<hash>`, which
Claude Code names after that folder. ccswap removes both when the login ends. Those a killed `ccswap
login` or daemon left are removed, once older than an hour, by the next `ccswap login` and at the
daemon's start.

The daemon's service file is 0600, because it copies the proxy variables (`HTTPS_PROXY` and the like,
which may hold credentials) from your environment: the LaunchAgent
`~/Library/LaunchAgents/com.github.dudude-bit.ccswap.plist` on macOS, the systemd unit
`~/.config/systemd/user/ccswap.service` on Linux.

To switch accounts ccswap writes into Claude Code's own storage (default paths shown): the
`claudeAiOauth` key of the `Claude Code-credentials` keychain item on macOS, or of
`~/.claude/.credentials.json` (0600) on Linux, leaving everything else in it alone; `oauthAccount` in
`~/.claude.json`; and one `StopFailure` hook in `~/.claude/settings.json`.

The web page keeps the read key in the tab's `sessionStorage`, or in the browser's `localStorage` when
you tick "Remember the key on this device"; the admin key only ever in `sessionStorage`.

On macOS, a value too long for one line of `security -i` input is passed on its command line in hex, so
it is visible in the process list for a moment. Claude Code writes its own item the same way.

## Network

ccswap talks to Anthropic only: `api.anthropic.com` (`/api/oauth/usage` and `/api/oauth/profile`) and
`platform.claude.com` (`/v1/oauth/token`, to refresh logins). It never sends prompts, proxies nothing and
has no telemetry. With `ccswap remote set` it also asks the other ccswap you name for its limits.

## The local API

- **Unix socket** (`ccswap.sock` in ccswap's folder, mode 0600): the full API, for the CLI, the TUI and
  the hook. Only your user can reach it.
- **TCP listener** (`[t3] listen`, `127.0.0.1:8318` by default; `[t3] enabled = false` turns it off):
  part of the API, for T3 Code, the web page and another ccswap.
  - The **read key** (the one `ccswap t3` prints) changes no setting. It reads the accounts' names,
    emails, limits and status, and lets T3 Code ask for an account's usage; it never reveals a token.
  - Either key, on any listener and `Host`, renews the login of an account that is already here: the
    daemon runs `claude auth login`, hands out its sign-in link and takes the code back. That login is
    kept only if it is the account's own email, and the sign-in's PKCE verifier never leaves the
    daemon's claude, so a key holder can start such a login and cancel it, but only the account's owner
    can finish it, and an intercepted code works nowhere else. Adding an account stays on the socket.
    The page's texts (`GET /v1/i18n/<tag>`) need no key: they hold nothing secret.
  - The **admin key** changes settings: priorities, enabling and disabling accounts for auto-switch,
    auto-switch itself, the strategy, thresholds, a manual switch and checking limits now. It is never
    printed and never put in a link. `ccswap web` asks the daemon over the socket for a one-time code
    (single use, valid for a minute) and opens the page with it; the page trades the code for the admin
    key and keeps it in the tab's `sessionStorage`.
  - A change is taken only when the listener is on a loopback address and the request's `Host` names
    one (`127.0.0.1`, `[::1]` or `localhost`), so a site that rebinds its name to 127.0.0.1 gets
    nothing. A listener on any other address only reads, even with the admin key.
  - Everything else is served on the socket only: a forced switch, adding and removing accounts, the
    hook signal, the language, the web codes, `remote` and `migrate`, among others.
  - The listener speaks plain HTTP. Outside a network you trust, keep it on loopback and use an SSH
    tunnel.
- **Replacing a key.** Both keys last until you replace them. If one leaks, delete it from the vault
  and restart the daemon, which makes a new one:
  - on macOS, `security delete-generic-password -s ccswap -a _t3-management-key` (or
    `-a _web-admin-key`), or delete that item of service `ccswap` in Keychain Access;
  - on Linux, `rm ~/.config/ccswap/vault/ccswap/_t3-management-key` (or `_web-admin-key`; the folder
    is ccswap's, `CCSWAP_HOME` moves it);
  - then `ccswap daemon install`.

  After a new read key, T3 Code, `ccswap remote set` on other machines and pages that remember the key
  need the new one from `ccswap t3`. After a new admin key, open the page again with `ccswap web`.
- **Remote** (`ccswap remote set`): the other ccswap's read key goes with every request, only to the URL
  you set. The URL must be https, or plain http to `localhost` or a loopback, private (RFC 1918, IPv6
  ULA) or CGNAT address (100.64.0.0/10, which Tailscale uses), and carry no user or password. Over plain
  http the key travels in clear on that network.

## Moving accounts (`ccswap migrate`)

A bundle is encrypted with AES-256-GCM under a key derived with HKDF-SHA256 from a random 160-bit
one-time code. ccswap shows the code once and never stores it. A bundle ccswap had to keep (an export
it could not deliver, or an import it could not finish) is a 0600 file in `migrate/` (0700) inside
ccswap's folder. Accounts move, they are not copied: a refresh token is single-use.

## Known advisories

`govulncheck` reports GO-2026-5932 (`golang.org/x/crypto/openpgp` is unmaintained) at module level:
`golang.org/x/crypto` is in the module graph, but ccswap never imports `openpgp`, so the code cannot
reach it, and no fixed version exists. CI runs `govulncheck` on every change and fails on any
vulnerability the code reaches.
