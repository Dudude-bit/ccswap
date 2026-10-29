# How ccswap works

This page describes what ccswap does as you can observe it: which files and keychain items it reads and
writes, which locks it takes, when it calls Anthropic, and what its daemon serves. The code is the source
of truth; package names are given where they help you find it.

## Where Claude Code keeps its login

ccswap works with Claude Code's own storage. With the default config folder (`~/.claude`):

| What | macOS | Linux |
|---|---|---|
| The login (OAuth tokens) | keychain item, service `Claude Code-credentials` | `~/.claude/.credentials.json`, mode 0600 |
| The account's details | `oauthAccount` in `~/.claude.json` | the same |
| Hooks | `~/.claude/settings.json` | the same |
| Session transcripts | `~/.claude/projects/**/*.jsonl` | the same |

- The login is a JSON object. Its `claudeAiOauth` key holds the access token, the refresh token, the
  access token's expiry and the scopes. Other keys, such as `mcpOAuth` with MCP server logins, belong to
  Claude Code.
- On macOS the item's account is your login name (`$USER`), or `claude-code-user` when that name has
  characters other than letters, digits, `.`, `_` and `-`.
- With another config folder (`CLAUDE_CONFIG_DIR`, which ccswap takes from `[claude] config_dir`), the
  macOS service is `Claude Code-credentials-<hash>`, where `<hash>` is the first 8 hex digits of the
  SHA-256 of the folder's path (NFC-normalised); on Linux the file is `<folder>/.credentials.json`, and
  the details are in `<folder>/.claude.json`. A `.config.json` in the config folder, from older Claude
  Code versions, takes the place of `.claude.json` when it exists.
- `CLAUDE_SECURESTORAGE_CONFIG_DIR` moves the login elsewhere; ccswap does not support it, and
  `ccswap doctor` says so.

What ccswap writes there (package `claudecode`, `hook`):

- **The login item:** only the value of `claudeAiOauth`. Every other key keeps its exact bytes.
- **`~/.claude.json`:** only `oauthAccount`, set to the target account's saved copy, so Claude Code shows
  the right email and organisation. The other keys keep their values; the file is written out again as a
  whole (sorted keys, two-space indent), atomically: a temporary file, fsync, rename. If the file changes
  while the new copy is prepared, ccswap starts over from a fresh read, up to five times.
- **`settings.json`:** one hook under `hooks.StopFailure`:
  `{"type": "command", "command": "<path to ccswap> signal", "timeout": 5}`. Every other value keeps its
  bytes and key order, a symlinked file is followed, and an entry left by a ccswap at an older path is
  replaced. `ccswap daemon uninstall` removes it.
- **Transcripts:** never written. The `cache-aware` strategy reads the newest modification time under
  `projects/`, never the contents; `fill-first` does not look.

## The vault

The logins of accounts Claude Code is not using live in ccswap's vault (package `store`, `keychain`):

- **macOS:** the keychain, service `ccswap`, one item per account (the item's account is the ccswap
  account name). ccswap talks to the keychain through `/usr/bin/security` and reads every write back to
  check it.
- **Linux:** files in `vault/ccswap/` inside ccswap's folder (`~/.config/ccswap`), mode 0600 in 0700
  folders, each written to a temporary file, fsynced and renamed, so a crash cannot bring back a spent
  refresh token.

An item holds `{"claudeAiOauth": …}` and nothing else. Items whose names start with `_`, which no account
name can, hold ccswap's own data: `_fingerprint` (see below), `_t3-management-key` and `_web-admin-key`
(the TCP listener's read and admin keys, 32 random bytes each, made when the listener first starts), and
`_remote-key` (the read key of another ccswap, see `ccswap remote set`).

`state.json` in ccswap's folder holds no tokens: the accounts (name, email, priority, enabled, needs a
login, the saved `oauthAccount`), the account in use, auto-switch, the strategy, the time of the last
switch and a 48-hour log of who held Claude Code's login, which the web page draws as the rota.

The account in use normally has no vault item: its login is the one in Claude Code's item, which Claude
Code refreshes itself.

## Identifying the login

Every tick (`poll_interval`, 60 s) the daemon reads Claude Code's item and works out which account it
holds (package `switcher`):

1. **Fingerprint.** ccswap keeps the SHA-256 of the refresh token it last put in the item or identified
   there, with the account's name and email, as `_fingerprint`. If the item's refresh token has that hash
   and the account still has that email, it is that account. No network call.
2. **Profile.** Otherwise, if the access token is still valid, ccswap asks
   `GET https://api.anthropic.com/api/oauth/profile` for its email and looks for the account with that
   email (case does not matter). A match updates the fingerprint.
3. **No answer.** If the access token has expired, because Claude Code has not run for a while, the
   login cannot be identified until Claude Code refreshes it. Auto-switch waits, and `ccswap use` refuses
   unless `--force`.

Claude Code rotates its refresh token when it refreshes its login, so step 2 runs about once after each
of its refreshes. When the item holds a login whose email no account has, ccswap reports it once
(`ccswap adopt <name>` adds it), shows no account in use, and never switches away from it: it has no
copy of that login, and a switch would lose it.

When the item holds a known account other than the one `state.json` names (you ran `/login` in Claude
Code), the state follows the item. The account that was there before is flagged "log in again" if
ccswap has no copy of its login. A fresh `/login` to an account flagged that way clears the flag.

## Switching

A switch, by hand or automatic, goes like this:

1. **Checks, outside any lock.** The target exists, does not need a login and is not being moved by
   `ccswap migrate`. The login in the item is identified (this may call the profile endpoint). Unless the
   switch is forced, Claude Code's access token must not have expired or be within 6 minutes of expiring:
   a switch while Claude Code refreshes could leave ccswap with a copy of the old login that the refresh
   has just spent. The target's vault copy must have an access token.
2. **Claude Code's locks.** ccswap takes the lock directories Claude Code creates around writes to its
   login, in this order, waiting 10 seconds at most in all:
   - `<config folder>/.oauth_refresh.lock` (counted stale after 60 s)
   - `<config folder with symlinks resolved>.lock`, for example `~/.claude.lock` (60 s)
   - `<config folder>/.storage-write.lock` (15 s)

   They are empty directories made with `mkdir`; a writer that finds one waits. A lock older than its
   stale time is removed; a fresh one is never touched. When the wait runs out the switch
   gives up (`entry_changed`), and auto-switch tries again on its next tick.
3. **Under the locks, no network.** ccswap saves the current login to the vault under the account it
   was identified as. It reads the item once more and, if Claude Code rewrote it meanwhile, saves the
   newest copy; if the refresh token changed, it lets go of the locks and starts over (once at most).
   Then it writes the target's `claudeAiOauth` into the item, keeping every other key, and lets go of the
   locks at once.
4. **`oauthAccount`.** ccswap writes the target's saved details into `~/.claude.json` under
   `~/.claude.json.lock` (stale after 10 s, a 10 s wait). A failure here is a warning; the switch has
   happened.
5. **State.** The account in use and the time of the switch go into `state.json`.

Running Claude Code sessions read the item again on their own and move to the new account, in our
experience within about half a minute. Nothing restarts. Each session's prompt cache starts over on the
new account.

### Parked copies

After a switch the target's vault copy is not deleted at once: a running session may still write the
login it had read before the switch back over it. If that happens, ccswap identifies the old login on
its next tick, the state follows, and the target's login is still in the vault. The copy is "parked":
nothing refreshes it. The daemon deletes it once the target has held Claude Code's item for 2 minutes
since the last switch and the item holds exactly that refresh token. A copy with any other refresh token
is kept.

## Refreshing tokens

A refresh token works once: refreshing a login returns a new pair and spends the old refresh token. So
ccswap refreshes only the vault logins of accounts Claude Code is not using (package `daemon`):

- **When:** an access token less than 10 minutes from expiry, or none; and at once for a login that
  `ccswap migrate import` brought in, whose bundle token must be spent before anyone else can use it.
- **Never:** the account Claude Code holds or `state.json` names as in use; a vault copy with the same
  refresh token as Claude Code's item (a parked copy); an account a `migrate export` has frozen. If
  ccswap spent the refresh token Claude Code holds, Claude Code's own next refresh would fail and log it
  out.
- **How:** `POST https://platform.claude.com/v1/oauth/token` with a JSON body: `grant_type` is
  `refresh_token`, `refresh_token`, `client_id` is Claude Code's public OAuth client ID (the client
  `claude auth login` issued the login to), and `scope` is the login's scopes.
- Reading the vault, refreshing and saving the new pair happen under one lock, so no switch lands in
  between. The request is never cancelled halfway, as a lost answer would be a lost login. The new pair
  is saved before its access token is used; if the save fails, the pair is kept in memory and saved on
  the next tick or at shutdown.
- An `invalid_grant` answer means the login is gone: the account is flagged "log in again" and you get
  a notification.
- **On demand:** the page's button, `t` in the TUI and `POST /v1/accounts/{name}/refresh` refresh one
  login now, under the same rules: the login Claude Code uses gets `409 claude_owns`, and a login ccswap
  refreshed under a minute ago is not asked again (the call returns the account as it is). A failed
  refresh is `502 refresh_failed`, a spent login `409 needs_login`.
- **The login's end:** a refresh answer's `refresh_token_expires_in` is kept in the vault as
  `refreshTokenExpiresAt`, and the API shows it as `login_expires_at` beside the access token's
  `token_expires_at`. No refresh moves it; only a new login does. 72 hours ahead the daemon sends one
  notification per login.

## Usage polling and backoff

The daemon asks `GET https://api.anthropic.com/api/oauth/usage` with the account's access token and the
header `anthropic-beta: oauth-2025-04-20` (package `anthropic`). It reads `five_hour`, `seven_day`,
`seven_day_opus`, `seven_day_sonnet`, and the `limits` entries of kind `weekly_scoped` that name a model.
Requests time out after 15 seconds, and at most four accounts are asked at once. Every request carries
the user agent `ccswap/<version>`.

- **Which token:** for the account Claude Code holds, the access token in Claude Code's item, read only;
  for the others, their vault copy. An account without a live access token is not asked; for the
  account in use this shows as "Claude Code has not run for a while".
- **How often:** each account has an interval, `check_active` (5 minutes) for the account in use and
  `check_others` (30 minutes) for the others. A tick asks the accounts whose interval has passed, with a
  10% allowance for a tick that comes early. "Check limits" asks every account now, except one asked
  less than a minute ago, with no allowance. Nothing asks for the usage of disabled accounts or accounts
  waiting for a login: not a tick, "Check limits", T3 Code or a limit signal. One account is never asked
  twice at once: a T3 request that finds an ask of its account in flight waits for that ask, as long as a
  request may take (15 seconds), and gets its answer.
- **After a `429`:** nothing asks that account before its `Retry-After` (an hour at most) has passed.
  Its interval doubles, up to 10 minutes; a longer configured interval stays as it is. "Check limits"
  keeps to both. After 30 answers in a row at the slower pace it speeds up by a quarter, until it is
  back at its configured interval.
- **Freshness:** numbers count for one interval past the next ask, about two intervals; while a `429`
  forbids asking, longer, by an hour at most. Policy uses only fresh numbers: an account whose numbers
  are older is never switched to, and with stale numbers for the account in use auto-switch waits,
  unless a limit signal came in.
- **Failures** (no connection, a 5xx, an unreadable answer) keep the last numbers and show the error; the
  next tick asks again.
- **Another ccswap:** with `ccswap remote set`, each tick first takes every account's numbers from
  `GET <url>/v1/accounts` of that ccswap, matched by email, and asks Anthropic only for what it lacks or
  has stale (see the README).
- **T3 Code:** a usage request from T3 Code is answered from numbers up to 60 seconds old; otherwise
  from a live ask if the account's interval allows one, or from the ask in flight; otherwise from the
  last answer, however old. A disabled account or one waiting for a login gets only the last answer.

From the numbers of the last hour the daemon also projects when the account in use will reach a
threshold, at its current pace (the rota's dashed handover). It needs at least 15 minutes of samples
and never projects past the window's reset.

## The hook signal and its guard

`ccswap daemon install` adds a `StopFailure` hook to Claude Code's settings. When a session stops on an
API error, Claude Code runs `ccswap signal` and passes the event as JSON on stdin.

`ccswap signal` must never get in a session's way: it prints nothing, always exits 0 and gives up after
2.5 seconds. It drops the event when the session does not use the login ccswap switches: when
`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_BASE_URL` is set,
or `CLAUDE_CONFIG_DIR` points to another login than `[claude] config_dir`. Otherwise it passes the event
to the daemon over the Unix socket.

The daemon acts only on `"error": "rate_limit"` (and then always checks at once). It marks the account
in use as at its limit for an hour when `last_assistant_message` reads like a usage limit (it mentions a
limit and a reset), with three guards:

- A signal within a minute after a switch belongs to the previous account and is ignored.
- When the message names its window (`You've hit your session limit`, `weekly`, or a model's name) and
  the numbers for that window are older than `check_active` (5 minutes at most) plus a tick, the daemon
  asks for fresh numbers first, unless the account is disabled or waits for a login, Anthropic made it
  wait, a T3 request's ask of it is in flight, or there is no live token.
- When fresh numbers show the named window under 80%, the signal is ignored: the session ran on another
  login, such as a session with its own token, whose environment the hook may not see. So it is when the
  window reset after those numbers were taken: the message is from before the reset (a late hook, a clock
  ahead), as no window runs out within minutes of its start.

When the limit's window resets less than an hour away, the mark ends at the reset instead: an hour would
outlast it, and with every account at its limit ccswap would move to one that resets later. That window is
the one the message names, or for wording that names none (`You've hit your limit · resets 6:30am`) the
first window at 80% or more to reset. A repeated signal does not extend the mark.

## The daemon and its API

One daemon runs per user: it holds an exclusive lock on `daemon.lock` in ccswap's default folder, even
when `CCSWAP_HOME` points elsewhere, and refuses to start while another daemon answers on the socket
(exit status 3). It stops with exit status 78 (`EX_CONFIG`, which systemd shows as `78/CONFIG`) when no
restart can help: `CCSWAP_HOME` is relative, ccswap's folder or your home folder cannot be found,
ccswap's folder cannot be created or made 0700, the socket path in it is too long, or `config.toml` or
`state.json` does not load. The systemd unit restarts on neither status, and under launchd, which
restarts on any non-zero status, both exit 0 instead. It runs as a LaunchAgent
(`~/Library/LaunchAgents/com.github.dudude-bit.ccswap.plist`) or a systemd user unit
(`~/.config/systemd/user/ccswap.service`), both 0600, with your shell's `CCSWAP_HOME`, proxy and locale
variables copied in. It logs to `daemon.log`, which it copies to `daemon.log.1` and empties past 10 MiB.

The API is described in [api/openapi.yaml](../api/openapi.yaml). It has two surfaces:

**The Unix socket** `ccswap.sock` in ccswap's folder, mode 0600, serves all of it: the CLI, the TUI and
the hook use it. Only your user can open it.

**The TCP listener** (`[t3] listen`, `127.0.0.1:8318`; `[t3] enabled = false` turns it off) serves the
web page, part of the API and the T3 Code hub:

| What | Needs |
|---|---|
| The page and its files (`/management.html`, scripts, styles, fonts), with a Content Security Policy that allows nothing from other origins | nothing: they hold no data |
| `GET /v1/i18n/<tag>`, the page's texts | nothing: they hold no secret |
| `GET /v1/status`, `GET /v1/accounts`, `GET /v1/events` | the read key or the admin key |
| `PATCH /v1/accounts/{name}`, `POST /v1/accounts/{name}/activate` (never forced), `PUT /v1/auto`, `PUT /v1/strategy`, `PUT /v1/thresholds`, `POST /v1/refresh`, `POST /v1/accounts/{name}/refresh` | the admin key, a listener on a loopback address, and a `Host` that names a loopback address or `localhost` |
| `POST /v1/web/session`: trades a one-time code from `ccswap web` for the admin key | the code, a loopback listener and a loopback `Host` |
| `POST /v1/accounts/{name}/login`, `POST /v1/accounts/{name}/login/code`, `DELETE /v1/accounts/{name}/login`: an account's new login from the page | the read key or the admin key, on any listener and `Host` |
| `GET /v0/management/auth-files`, `POST /v0/management/api-call`, `POST /v0/management/reset-quota`: the part of CLIProxyAPI's management API T3 Code reads | the read key or the admin key |

Everything else, such as adding and removing accounts, a forced switch, the hook signal, `remote`,
`migrate`, the language and the web codes, is served on the socket only. The `Host` check stops a site
whose name was pointed at 127.0.0.1 (DNS rebinding) from changing anything.

**A login from the page** renews an account that is already here. `POST /v1/accounts/{name}/login`
makes a `login-*` folder, runs `claude auth login` in it as `ccswap login` does (`CCSWAP_CLAUDE_BIN`,
else `claude` from `PATH`) and answers with the sign-in link it prints, within 30 seconds. The code the
sign-in shows goes to `POST /v1/accounts/{name}/login/code`, which writes it to claude and imports the
login it gets as `ccswap login` would: an account whose email differs is refused (`409
wrong_account`). A link takes one code: a refused one (`400 code_refused`) spends it, as it spends
claude's sign-in. A link waits 10 minutes, one per account: a new one, `DELETE` or a daemon restart stops
its claude and drops its folder, and a code for it then gets `409 login_gone`. Claude did not start or
gave no link: `502 login_failed`. The account Claude Code uses is refused (`409 active_account`), as
its login is Claude Code's to renew. These routes take the read key anywhere: the PKCE verifier of the
sign-in stays inside the daemon's claude, so a code is no use elsewhere, and the import's email check
means a key holder can at most start a login that only the account's owner can finish.

The hub routes answer as T3 Code expects: `auth-files` lists the accounts (name, email, whether they are
off), `api-call` accepts only `GET https://api.anthropic.com/api/oauth/usage` for one account and
returns its usage answer as described above, and `reset-quota` does nothing.

`GET /v1/events` is a stream of server-sent events: a `state` event with the status and the accounts at
once and after each change, and a `: ping` comment every 15 seconds.

## What ccswap never does

- Send prompts or model requests, or proxy anything: Claude Code is the only client for your work.
- Refresh the login Claude Code holds, or overwrite a login it has no copy of.
- Change anything in Claude Code's files beyond the `claudeAiOauth` value, `oauthAccount` and its one
  hook, or modify the Claude Code binary.
- Ask for your password: logins go through `claude auth login`.
- Talk to any server other than Anthropic's and a ccswap you name with `ccswap remote set`, or send
  telemetry.
