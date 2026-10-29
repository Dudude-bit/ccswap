'use strict';

const RETRY_MS = 3000;
const STALE_MS = 40000; // the daemon sends at least a ping every 15 s
const TIMEOUT_MS = 60000;
const UPDATE_MS = 150000; // the daemon gives an install two minutes
const RESTART_MS = 90000; // how long the page waits for the daemon to come back on the new build
const UPDATED_ITEM = 'ccswap-updated'; // the version this tab installed, to say so once the page reloads
const KEY_ITEM = 'ccswap-key'; // the read key, typed in: the page only looks with it
const ADMIN_ITEM = 'ccswap-admin-key'; // the admin key a ccswap web code was traded for: this tab only
const HOUR = 3600000;
const TAGS = 8; // account colours, .c0 to .c7 in app.css

const $ = (id) => document.getElementById(id);
const phone = matchMedia('(max-width: 720px)');
const oneRow = matchMedia('(min-width: 1181px)'); // the top bar keeps one row (app.css)
const stacked = matchMedia('(max-width: 1180px)'); // the table as blocks, one per account, at any length, as the lead stacks (app.css)
let timeFmt, dayFmt, dateFmt, numFmt;

const state = {
  key: '', admin: false, autoBusy: false, strategyBusy: false, screen: '', loading: false, status: null, accounts: null,
  loadedAt: null, stream: null, retry: 0, watchdog: 0, hover: '', laneTab: '', fault: '', offline: false,
  seen: '', updated: '', upd: { to: '', lost: false },
};
const rows = new Map();
const lanes = new Map();

class ApiError extends Error {
  constructor(code, message) {
    super(message || code);
    this.code = code;
  }
}

// A remembered key survives restarts (localStorage); any other lasts as long as the tab (sessionStorage).
function storedKey() {
  try { return localStorage.getItem(KEY_ITEM) || sessionStorage.getItem(KEY_ITEM) || ''; } catch { return ''; }
}

function storeKey(k, remember) {
  try {
    localStorage.removeItem(KEY_ITEM);
    sessionStorage.removeItem(KEY_ITEM);
    if (k) (remember ? localStorage : sessionStorage).setItem(KEY_ITEM, k);
  } catch { /* storage off: the key lives in this page only */ }
}

function adminKey() {
  try { return sessionStorage.getItem(ADMIN_ITEM) || ''; } catch { return ''; }
}

// storeAdmin keeps the admin key for the tab. With sessionStorage off it is not kept: init holds the
// key it traded for in the page, and a reload needs a new ccswap web link.
function storeAdmin(k) {
  try {
    if (k) sessionStorage.setItem(ADMIN_ITEM, k);
    else sessionStorage.removeItem(ADMIN_ITEM);
  } catch { /* storage off */ }
}

// takeCode returns the one-time code of a ccswap web link and drops the fragment from the address
// bar and history at once.
function takeCode() {
  const m = location.hash.match(/[#&]code=([^&]+)/);
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  return m ? decodeURIComponent(m[1]) : '';
}

async function api(method, path, body, timeout = TIMEOUT_MS) {
  const headers = { Authorization: 'Bearer ' + state.key };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let r;
  try {
    r = await fetch(path, {
      method, headers, cache: 'no-store',
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeout),
    });
  } catch (e) {
    throw new ApiError(e.name === 'TimeoutError' ? 'timeout' : 'offline');
  }
  if (r.status === 401) throw new ApiError('unauthorized');
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    throw new ApiError(e.code || 'http', e.message || 'HTTP ' + r.status);
  }
  return r.status === 204 ? null : r.json();
}

const where = () => state.status?.host || t('host.any');

// ERR_CODES are the error codes with a text of their own, e.<code>: the API's and the page's own offline
// and timeout. Any other code reads e.other.
const ERR_CODES = ['offline', 'timeout', 'unknown_active', 'internal', 'entry_expired', 'entry_changed', 'cannot_identify',
  'invalid_thresholds', 'target_refreshing', 'not_found', 'needs_login', 'read_only', 'local_only', 'bad_code', 'migrating',
  'no_update', 'updating', 'update_failed'];

const errArgs = (e, name) => ({
  cannot_identify: [e.message], invalid_thresholds: [e.message], target_refreshing: [name], not_found: [name],
  needs_login: [name, where()], migrating: [name], update_failed: [e.message],
}[e.code] ?? [where()]);

function describe(e, name = '<name>') {
  return ERR_CODES.includes(e.code) ? t('e.' + e.code, ...errArgs(e, name)) : t('e.other', e.message);
}

// describeNodes is describe's text as nodes, its commands set as code.
function describeNodes(e, name = '<name>') {
  return ERR_CODES.includes(e.code) ? tNodes('e.' + e.code, ...errArgs(e, name)) : tNodes('e.other', e.message);
}

// h makes an element with a class and children; strings become text, empty ones are left out.
function h(tag, cls, ...kids) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  el.append(...kids.filter((k) => k || k === 0));
  return el;
}

// hold disables el while its request runs and hands focus back afterwards, as a disabled control drops it
// to the page; unless the user has moved on meanwhile. A control asleep while the daemon is away stays
// disabled when its request ends (asleep keeps a held one disabled in turn).
function hold(el, busy) {
  if (busy) el.dataset.had = String(document.activeElement === el);
  el.classList.toggle('held', busy);
  el.disabled = busy || el.classList.contains('asleep');
  const lost = !document.activeElement || document.activeElement === document.body;
  if (!el.disabled && el.dataset.had === 'true' && lost) el.focus();
}

function setBusy(btn, busy, label) {
  btn.classList.toggle('busy', busy);
  hold(btn, busy);
  if (label) btn.querySelector('.label').textContent = label;
}

function showMsg(el, text, kind = 'crit') {
  el.replaceChildren(text);
  el.classList.toggle('warn', kind === 'warn');
  el.hidden = false;
}

function hideMsg(el) {
  el.hidden = true;
  el.replaceChildren();
}

function duration(ms) {
  const min = Math.round(ms / 60000);
  if (min < 1) return t('duration.under');
  if (min < 60) return t('duration.min', min);
  const h = Math.floor(min / 60);
  if (h < 48) return min % 60 ? t('duration.hMin', h, min % 60) : t('duration.h', h);
  const d = Math.floor(h / 24);
  return h % 24 ? t('duration.dH', d, h % 24) : t('duration.d', d);
}

function ago(iso) {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  return s < 10 ? t('ago.now') : s < 60 ? t('ago.s', s) : s < 3600 ? t('ago.min', Math.round(s / 60)) : t('ago.at', clock(new Date(iso)));
}

// clock is at's time, with its weekday off today, or its date when the weekday could be another week's.
function clock(at) {
  const now = new Date();
  if (Math.abs(at - now) >= 6 * 24 * HOUR) return `${dateFmt.format(at)} ${timeFmt.format(at)}`;
  return at.toDateString() === now.toDateString() ? timeFmt.format(at) : `${dayFmt.format(at)} ${timeFmt.format(at)}`;
}

const num = (n) => numFmt.format(Math.round(n * 10) / 10);
const pct = (n) => num(n) + '%';
const upFirst = (s) => s.charAt(0).toUpperCase() + s.slice(1);

function hash(name) {
  let x = 2166136261;
  for (const ch of name) x = Math.imul(x ^ ch.codePointAt(0), 16777619);
  return x >>> 0;
}

let colours = new Map();

// assignColours gives each account its tag colour, the same on every machine for the same accounts in the
// same priority order. In name order, each takes the colour its name hashes to, or the next one used least,
// so the first eight all differ; past eight, a colour repeats, but not on a priority neighbour while another
// is free.
function assignColours() {
  const order = ordered().map((a) => a.name);
  const uses = new Array(TAGS).fill(0);
  colours = new Map();
  for (const name of [...order].sort()) {
    const i = order.indexOf(name);
    const near = [order[i - 1], order[i + 1]].map((n) => colours.get(n));
    let best = 0, bestKey = Infinity;
    for (let step = 0; step < TAGS; step++) {
      const c = (hash(name) + step) % TAGS;
      const key = (near.includes('c' + c) ? 1e6 : 0) + uses[c] * TAGS + step; // apart from neighbours, then least used, then nearest
      if (key < bestKey) [best, bestKey] = [c, key];
    }
    colours.set(name, 'c' + best);
    uses[best]++;
  }
}

const tagOf = (name) => colours.get(name) ?? 'c' + (hash(name) % TAGS);

const chip = (name) => {
  const el = h('span', 'chip ' + tagOf(name));
  el.setAttribute('aria-hidden', 'true');
  return el;
};

// REASONS are the reasons an account ran out (exhausted_reason) with a text of their own, r.<reason>.
const REASONS = ['five_hour', 'seven_day', 'limit_hit'];

function exhaustedText(r) {
  if (r?.startsWith('model:')) return t('r.model', upFirst(r.slice(6)));
  return REASONS.includes(r) ? t('r.' + r) : t('r.default');
}

// KINDS are the states the rota, the table and the legend draw alike, with their names.
const KINDS = { duty: 'st.duty', next: 'st.next', resting: 'st.resting', login: 'st.login', off: 'st.off', ready: 'st.ready' };

// kindOf is a's state on the rota. While Claude Code holds a login ccswap does not know, nobody is in use.
function kindOf(a) {
  const st = state.status;
  if (a.active && !st.unknown_active) return 'duty';
  if (a.needs_login) return 'login';
  if (!a.enabled) return 'off';
  if (a.exhausted) return 'resting';
  if (st.auto && st.next === a.name) return 'next';
  return 'ready';
}

const onDuty = () => state.accounts.find((a) => kindOf(a) === 'duty');
const ordered = () => [...state.accounts].sort((a, b) => b.priority - a.priority || a.name.localeCompare(b.name));

function windows(a) {
  const th = state.status.thresholds;
  return [
    { label: t('w.five'), short: t('w.fiveShort'), w: a.five_hour, limit: th.five_hour },
    { label: t('w.week'), short: t('w.weekShort'), w: a.seven_day, limit: th.seven_day },
    ...a.models.map((m) => ({ label: t('w.model', upFirst(m.name)), short: upFirst(m.name), w: m, limit: th.seven_day })),
  ];
}

// freeAt is when a resting account is back, as the daemon counts it, with the window at its threshold that resets
// then (none when a limit hit lasts longer); null when that time is unknown or has passed.
function freeAt(a) {
  const at = a.free_at && new Date(a.free_at);
  if (!at || at <= Date.now()) return null;
  return { at, x: windows(a).find((x) => x.w?.resets_at && +new Date(x.w.resets_at) === +at && x.w.utilization >= x.limit) };
}

// waiting is whether a resting account's time to be back has passed already: the next check frees it.
const waiting = (a) => Boolean(a.free_at) && !freeAt(a);

// closest is the window nearest its threshold: the one auto-switch leaves a for, as things stand.
const closest = (a) => windows(a).filter((x) => x.w).sort((p, q) => (p.limit - p.w.utilization) - (q.limit - q.w.utilization))[0];

// shiftStart is when the account in use took over: its open shift, else the last switch.
function shiftStart() {
  const st = state.status;
  const at = st.shifts?.findLast((s) => !s.to && s.account === st.active)?.from || st.last_switch_at;
  return at ? new Date(at) : null;
}

// leavesAt is when the account in use is expected to reach a threshold, while that is still ahead.
function leavesAt() {
  const iso = onDuty()?.leaves_at;
  return iso && new Date(iso) > Date.now() ? new Date(iso) : null;
}

// checkEvery is how often a's limits are asked, in seconds: a notice shows once two checks were missed.
function checkEvery(a) {
  const t = state.status.thresholds;
  return a.active ? t.check_active_seconds : t.check_others_seconds;
}

// checkNote is what to say about a's last check, if anything: [text, bad].
function checkNote(a) {
  if (a.error) return [t('n.failed', a.error), true];
  if (!a.checked_at) return a.needs_login ? null : [t('n.notChecked'), false];
  if (Date.now() - new Date(a.checked_at) > 2000 * checkEvery(a)) return [t('n.checked', ago(a.checked_at)), false];
  return null;
}

// nextChange is the next thing that happens to a, in a few words.
function nextChange(a, kind = kindOf(a)) {
  const st = state.status, leaves = leavesAt();
  switch (kind) {
    case 'duty':
      if (!st.auto) return t('nc.stays');
      if (a.exhausted) return st.next ? t('nc.leaving') : t('nc.stuck');
      return leaves ? t('nc.leaves', clock(leaves)) : t('nc.none');
    case 'next': return leaves ? t('nc.takesOver', clock(leaves)) : t('nc.nextUp');
    case 'resting': {
      const f = freeAt(a);
      return f ? t('nc.back', clock(f.at)) : waiting(a) ? t('nc.backCheck') : t('nc.backUnknown');
    }
    case 'login': return t('nc.login');
    case 'off': return t('nc.off');
    default: return t('nc.ready');
  }
}

// render draws the page from state. A fault in drawing is the page's own, not the daemon's: it is logged and
// said once, the stream stays up, and the next update draws again.
function render() {
  try {
    draw();
    if (state.fault === 'draw') fault('');
  } catch (e) {
    console.error('ccswap: drawing an update failed', e);
    fault('draw');
  }
}

const ISSUES = 'https://github.com/Dudude-bit/ccswap/issues';

// fault says the page could not draw ('draw') or read ('read') an update, in the current language, with a
// Reload button and a link to report it; '' clears it. One that could not be read clears only when an update
// is read: a redraw of the old state proves nothing.
function fault(kind) {
  state.fault = kind;
  const el = $('draw-err');
  if (!kind) return hideMsg(el);
  if (!el.hidden && el.dataset.lang === lang) return;
  el.dataset.lang = lang;
  const reload = h('button', 'btn', t('x.reload'));
  reload.type = 'button';
  reload.addEventListener('click', () => location.reload());
  const report = h('a', '', t('x.report'));
  Object.assign(report, { href: ISSUES, target: '_blank', rel: 'noopener noreferrer' });
  el.replaceChildren(h('span', 'grow', t('x.draw')), reload, report);
  el.hidden = false;
}

function draw() {
  assignColours();
  document.body.classList.toggle('ro', !state.admin);
  $('ro-note').hidden = state.admin;
  const st = state.status;
  const any = state.accounts.length > 0;
  for (const id of ['lead', 'rota-sec', 'acct-sec']) $(id).hidden = !any;
  $('empty').hidden = any;
  renderTop(st);
  renderAlerts();
  renderUpdate();
  if (any) {
    renderDuty();
    renderNext();
    renderRota();
  } else {
    $('empty-p').replaceChildren(...tNodes('empty.add', where()));
  }
  renderAccounts();
  renderHow();
  renderRules();
  if (!state.admin) readOnly();
  else asleep(state.offline);
}

// readOnly disables every control of a page without the admin key: it only looks, and the controls are
// hidden. The daemon refuses such a change anyway; ccswap web on this machine opens a page that can make it.
function readOnly() {
  for (const el of document.querySelectorAll('#auto, #strategy, #refresh, #rules button, #accounts button, #accounts .enabled')) {
    el.disabled = true;
  }
  for (const el of document.querySelectorAll('#rules input, #accounts input[type="number"]')) el.readOnly = true;
  for (const el of document.querySelectorAll('#rules input')) el.style.width = `${el.value.length + 1}ch`; // a value, not a box
}

function renderTop(st) {
  $('where').textContent = [st.host, st.version].filter(Boolean).join(' · ');
  // Each clause with the order it gives way in when the bar is short of room; a problem never does.
  // Nothing to check without accounts; offline, Claude Code's activity is not known.
  const segs = state.accounts.length ? [[st.last_poll_at ? t('top.checked', ago(st.last_poll_at)) : t('top.notChecked'), '']] : [];
  if (st.last_activity_at && !state.offline) segs.push([t('top.activity', ago(st.last_activity_at)), 'm', 1]);
  let bad = false;
  if (st.remote) {
    const host = new URL(st.remote.url).host;
    bad = !st.remote.ok;
    segs.push(st.remote.ok ? [t('top.via', host), 'm', 2] : [t('top.unreachable', host), 'bad']);
  }
  $('live-text').replaceChildren(...segs.map(([text, cls, drop]) => {
    const s = h('span', 'lseg ' + cls, text);
    if (drop) s.dataset.drop = drop;
    return s;
  }));
  $('live').title = segs.map(([text]) => text).join(' · ');
  $('live').hidden = !segs.length;
  $('refresh').hidden = !state.accounts.length;
  $('live').classList.toggle('bad', bad || !$('banner').hidden);
  renderHost(st.host);

  if (!state.autoBusy) $('auto').checked = st.auto;
  if (!state.strategyBusy) $('strategy').value = st.strategy;
  $('auto-ro').textContent = t('top.ro', t(st.auto ? 'top.on' : 'top.off'), t(st.strategy === 'fill-first' ? 'strategy.fill' : 'strategy.cache'));
  fitTop();
}

// fitTop folds Check limits to its icon on a phone, and elsewhere when the status beside it would otherwise
// be cut short or run onto a second line. Where the bar keeps one row and the status is still too long, its
// quiet clauses give way whole, never half a time: Claude Code's activity first, then where limits come
// from; the check time and a problem stay, the first alone ending in an ellipsis if it must. The status's
// title has it all. Where the bar wraps anyway, nothing gives way.
function fitTop() {
  const top = $('dash').querySelector('.top'), text = $('live-text');
  const short = () => text.scrollWidth > text.clientWidth + 1 || $('live').offsetHeight > 30;
  top.classList.remove('compact');
  for (const s of text.children) s.hidden = false;
  if (phone.matches || short()) top.classList.add('compact');
  if (!oneRow.matches) return;
  for (const s of [...text.children].filter((c) => c.dataset.drop).sort((p, q) => p.dataset.drop - q.dataset.drop)) {
    if (!short()) break;
    s.hidden = true;
  }
}

// renderHost names the computer whose Claude Code this page switches; opened from elsewhere, the page
// would otherwise look like the local one.
function renderHost(host) {
  document.title = pageTitle(host);
  const note = $('host-note');
  note.hidden = !host || ['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname);
  if (note.hidden || note.dataset.shown === lang + host) return;
  note.dataset.shown = lang + host;
  note.replaceChildren(...tNodes('host.note', host));
}

const pageTitle = (host) => (host ? t('title', host) : 'ccswap');

// alertBox is one line of the ink strip: an account's tag, the message, a quieter tail, and a button that
// copies the command when the browser lets the page write the clipboard.
function alertBox(name, nodes, tail, cmd) {
  const body = h('p', 'alert-body', ...nodes);
  if (tail) body.append(' ', h('span', 'q', tail));
  const box = h('div', 'alert', name ? chip(name) : null, body);
  box.setAttribute('role', 'alert');
  if (cmd && navigator.clipboard) {
    const b = h('button', 'btn', t('alert.copy'));
    b.type = 'button';
    b.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(cmd);
        b.textContent = t('alert.copied');
        setTimeout(() => { b.textContent = t('alert.copy'); }, 2000);
      } catch { /* the browser said no: the command is on screen */ }
    });
    box.append(b);
  }
  return box;
}

let alertsShown = '';

// renderAlerts raises the ink strip only when something needs the user, the gravest first: every account at
// its threshold, a login ccswap does not know, accounts that need a new login. (The daemon being down comes
// before them all, in the banner.) It redraws only when that changes, so the alerts are announced once and a
// focused button keeps its focus.
function renderAlerts() {
  const st = state.status, specs = [];
  const usable = state.accounts.filter((a) => a.enabled && !a.needs_login);
  if (usable.length && usable.every((a) => a.exhausted)) {
    const first = usable.find((a) => a.name === st.first_free && a.free_at), f = first && freeAt(first); // the daemon's pick
    specs.push(!first ? [null, 'alert.allUnknown', [], null, null]
      : f ? [first.name, 'alert.all', [first.name, clock(f.at)], null, null] : [first.name, 'alert.allCheck', [first.name], null, null]);
  }
  if (st.unknown_active) specs.push([null, 'alert.unknown', [where()], 'alert.unknownTail', null]);
  const login = ordered().filter((a) => a.needs_login);
  if (login.length === 1) {
    const n = login[0].name;
    specs.push([n, 'alert.login', [n, where()], 'alert.skips', `ccswap login ${n}`]);
  } else if (login.length > 1) {
    specs.push([null, 'alert.logins', [login.length, login.map((a) => a.name).join(', '), where()], 'alert.skipsAll', null]);
  }
  const shown = JSON.stringify([lang, specs.map(([n, ...rest]) => [n && tagOf(n), n, ...rest])]);
  if (shown === alertsShown) return;
  alertsShown = shown;
  $('alerts').replaceChildren(...specs.map(([n, key, args, tail, cmd]) => alertBox(n, tNodes(key, ...args), tail && t(tail), cmd)));
}

let updateShown = '';

// renderUpdate says when a newer release is out: with Update on a page that may change settings, with the
// command on one that only looks. While this tab's update restarts the daemon it says so, and once the daemon
// is back on the new build the page reloads to get its own new version; another tab offers the reload. It
// redraws only when that changes, so a focused button keeps its focus.
function renderUpdate() {
  const st = state.status, u = state.upd;
  const spec = u.to ? [u.lost ? 'lost' : 'restarting', u.to]
    : state.seen && st.version !== state.seen ? ['reload', st.version]
    : state.updated && st.version === state.updated ? ['done', st.version]
    : st.update ? [state.admin ? 'out' : 'outRo', st.update.version, st.update.url, st.version]
    : null;
  const shown = JSON.stringify([lang, spec]);
  if (shown === updateShown) return;
  updateShown = shown;
  const el = $('update');
  el.classList.remove('busy');
  if (!spec) return hideMsg(el);
  const [kind, version, url, running] = spec;
  const button = (label, onClick) => {
    const b = h('button', 'btn', h('span', 'spin'), h('span', 'label', label));
    b.type = 'button';
    b.addEventListener('click', onClick);
    return b;
  };
  let kids;
  if (kind === 'out' || kind === 'outRo') {
    const notes = h('a', '', t('update.notes'));
    Object.assign(notes, { href: url, target: '_blank', rel: 'noopener noreferrer' });
    const text = h('span', 'grow', ...tNodes('update.out', version, where(), running));
    if (kind === 'outRo') text.append(' ', ...tNodes('update.how'));
    kids = [text, notes];
    if (kind === 'out') {
      const go = button(t('update.install'), installUpdate);
      go.id = 'upd-go';
      kids.splice(1, 0, go);
    }
  } else if (kind === 'reload') {
    kids = [h('span', 'grow', ...tNodes('update.reload', version)), button(t('x.reload'), () => location.reload())];
  } else if (kind === 'restarting') {
    el.classList.add('busy');
    kids = [h('span', 'spin'), h('span', 'grow', ...tNodes('update.restarting', version))];
  } else {
    kids = [h('span', 'grow', ...tNodes(kind === 'lost' ? 'update.lost' : 'update.done', version, where()))];
  }
  el.replaceChildren(...kids);
  el.hidden = false;
}

// installUpdate has the daemon install the newest release. It answers once the new binary is in place and
// then restarts on it: the page keeps quiet about the daemon being away for RESTART_MS, and onEvent reloads
// it when the daemon is back.
async function installUpdate() {
  const b = $('upd-go');
  setBusy(b, true, t('update.installing'));
  hideMsg($('hero-err'));
  try {
    const r = await api('POST', '/v1/update', undefined, UPDATE_MS);
    state.upd.to = r.version;
    setTimeout(() => {
      state.upd.lost = true;
      if (state.status) render();
    }, RESTART_MS);
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    showMsg($('hero-err'), describe(e));
  } finally {
    setBusy(b, false, t('update.install'));
  }
  render();
}

function renderDuty() {
  const st = state.status, a = onDuty();
  $('duty').classList.toggle('idle', !a);
  $('d-on').hidden = !a;
  $('d-off').hidden = Boolean(a);
  if (!a) {
    $('d-off-h').textContent = t(st.unknown_active ? 'd.unknown' : 'd.none');
    $('d-off-p').replaceChildren(...tNodes(st.unknown_active ? 'd.unknownHint' : 'd.noneHint', where()));
    return;
  }
  const tag = $('d-tag');
  tag.className = 'tag ' + tagOf(a.name);
  tag.textContent = a.name;
  const since = shiftStart();
  $('d-since').replaceChildren(...(since ? tNodes('d.since', clock(since)) : tNodes('d.inUse')));
  $('d-mail').textContent = t('d.mail', a.email, a.priority);
  const ws = windows(a);
  $('d-models').replaceChildren(...ws.slice(2).map((x) => h('div', x.w.utilization >= x.limit ? 'over' : '', h('dt', '', x.label), h('dd', '', pct(x.w.utilization)))));
  gauge($('g-five'), ws[0]);
  gauge($('g-week'), ws[1]);
  const note = $('d-note');
  const check = checkNote(a);
  const over = !st.auto ? 'd.overOff' : st.next ? 'd.over' : 'd.overStuck';
  const text = a.exhausted ? t(over, exhaustedText(a.exhausted_reason)) : check?.[0];
  note.textContent = text || '';
  note.classList.toggle('bad', a.exhausted || Boolean(check?.[1]));
  note.hidden = !text;
}

function gauge(el, { label, w, limit }) {
  el.querySelector('.g-label').textContent = label;
  const bar = el.querySelector('.bar');
  el.style.setProperty('--t', `${Math.min(100, limit)}%`);
  el.querySelector('.g-tick span').textContent = t('g.leaves', pct(limit));
  el.classList.toggle('none', !w);
  el.classList.toggle('over', Boolean(w) && w.utilization >= limit);
  if (!w) {
    el.querySelector('.n').textContent = '—';
    el.style.setProperty('--u', '0%');
    bar.removeAttribute('aria-valuenow');
    bar.setAttribute('aria-label', t('m.noDataLabel', label));
    el.querySelector('.g-reset').textContent = t('m.noData');
    return;
  }
  const u = Math.max(0, Math.min(100, w.utilization));
  el.querySelector('.n').textContent = num(w.utilization);
  el.style.setProperty('--u', `${u}%`);
  bar.setAttribute('aria-valuenow', String(u));
  bar.setAttribute('aria-label', t('m.label', label, pct(w.utilization), pct(limit)));
  el.querySelector('.g-reset').textContent = resetText(w, true);
}

// resetText is when w resets: the time and, when long is set, how long until then.
function resetText(w, long) {
  const at = w.resets_at && new Date(w.resets_at);
  if (!at) return long ? t('m.unknown') : '—';
  if (at <= new Date()) return long ? t('m.waiting') : t('m.reset');
  return long ? t('g.resets', clock(at), duration(at - Date.now())) : clock(at);
}

function renderNext() {
  const st = state.status, th = st.thresholds, a = onDuty();
  const nx = st.auto && st.next ? state.accounts.find((x) => x.name === st.next) : null;
  const leaves = leavesAt();
  $('n-proj').hidden = !(a && leaves && nx);
  const hand = [];
  let pace, rule = null;
  const arrow = () => {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'ico arrow');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('aria-hidden', 'true');
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', 'M4 12h15M13 6l6 6-6 6');
    svg.append(p);
    return svg;
  };
  if (!a) {
    hand.push(h('span', 'none', t('next.nobody')));
    pace = t(st.unknown_active ? 'next.unknown' : 'next.none');
  } else {
    const pair = (n, ...more) => h('span', 'pair', chip(n), h('span', 'nm', n), ...more);
    if (!st.auto) {
      hand.push(pair(a.name), h('span', 'none', t('next.stays')));
      pace = t('next.off', a.name);
    } else {
      hand.push(pair(a.name, arrow()), nx ? pair(nx.name) : h('span', 'none', t('next.noTarget')));
      if (leaves) hand.push(h('span', 'at', t('next.at', clock(leaves))));
      pace = a.exhausted ? t(nx ? 'next.leaving' : 'next.stuck', a.name)
        : leaves ? t('next.pace', a.name, clock(leaves)) : t('next.noPace', a.name);
      rule = nx ? tNodes('next.rule', a.name, pct(th.five_hour), pct(th.seven_day), nx.name)
        : tNodes('next.ruleNone', a.name, pct(th.five_hour), pct(th.seven_day));
    }
  }
  $('n-hand').replaceChildren(...hand);
  $('n-pace').textContent = pace;
  $('n-rule').replaceChildren(...(rule || []));
  $('n-rule').hidden = !rule;
  $('th-five').textContent = pct(th.five_hour);
  $('th-week').textContent = pct(th.seven_day);
  $('th-hyst').textContent = t('th.points', num(th.hysteresis));
  $('th-dwell').textContent = duration(th.min_dwell_seconds * 1000);
}

// span is the rota's window on whole local hours: a day ending a few hours ahead, a shorter one on a phone.
function span() {
  const [hours, ahead] = phone.matches ? [9, 4] : [24, 5];
  const end = new Date(Date.now() + ahead * HOUR);
  end.setMinutes(60, 0, 0);
  return { from: end - hours * HOUR, to: +end, hours };
}

let puckReady = false;

// renderRota draws the day: who held Claude Code, the shift in use up to now, the projection to the
// expected handover, the next account's outline from there, resting accounts hatched until their reset,
// the ones that need a login dashed, and each account's reset times as ticks on the table's clock.
function renderRota() {
  // Offline, the strip stops at the data's time and says so.
  const st = state.status, now = state.offline ? +state.loadedAt : Date.now();
  const { from, to, hours } = span();
  const x = (at) => Math.max(0, Math.min(100, ((+at) - from) / (to - from) * 100));
  $('rota').style.setProperty('--now', x(now));
  const leaves = leavesAt(), duty = onDuty();
  const nx = st.auto && st.next && state.accounts.find((a) => a.name === st.next);

  const at = (el, when) => { el.style.setProperty('--x', x(when)); return el; };
  const flags = [at(h('span', 'flag now', t(state.offline ? 'rota.data' : 'rota.now', timeFmt.format(new Date(now)))), now)];
  const marks = [at(h('i', 'vl now'), now)];
  const ho = duty && leaves && leaves < to;
  if (ho) {
    flags.push(at(h('span', 'flag ho', t(nx ? 'rota.handover' : 'rota.reaches', clock(leaves))), leaves));
    marks.push(at(h('i', 'vl ho'), leaves));
  }
  $('rota-flags').replaceChildren(...flags);
  if (ho) fitFlag(flags[0], flags[1], clock(leaves));

  const hourEls = [], grid = [h('i', 'ahead')];
  const step = $('rota-hours').clientWidth / hours < 28 ? 2 : 1; // narrow: every other hour is labelled
  for (let i = 0; i < hours; i++) {
    const when = new Date(from + i * HOUR), hr = when.getHours();
    if (hr % step === 0) hourEls.push(at(h('span', hr === 0 ? 'day' : '', hr === 0 ? dayFmt.format(when) : String(hr).padStart(2, '0')), when));
    if (i) grid.push(at(h('i', hr % 6 === 0 ? 'hr maj' : 'hr'), when));
  }
  $('rota-hours').replaceChildren(...hourEls);
  $('rota-grid').replaceChildren(...grid);
  $('rota-marks').replaceChildren(...marks);

  // Lanes are kept by account, as table rows are, so a focused lane stays put and is not announced again.
  const box = $('rota-lanes'), accts = ordered();
  for (const [name, l] of lanes) {
    if (!accts.some((a) => a.name === name)) { l.el.remove(); lanes.delete(name); }
  }
  const win = { x, from, to, now, leaves };
  for (const a of accts) {
    if (!lanes.has(a.name)) lanes.set(a.name, makeLane(a.name));
    fillLane(lanes.get(a.name), a, win);
  }
  // A new lane joins at once; the order is fixed only while focus is elsewhere, as moving drops focus.
  for (const a of accts) if (!lanes.get(a.name).el.isConnected) box.append(lanes.get(a.name).el);
  if (!box.contains(document.activeElement)) {
    accts.forEach((a, i) => {
      const el = lanes.get(a.name).el;
      if (box.children[i] !== el) box.insertBefore(el, box.children[i] || null);
    });
  }
  // One lane is a tab stop, the arrow keys move between them.
  if (!lanes.has(state.laneTab)) state.laneTab = accts[0]?.name || '';
  for (const [name, l] of lanes) l.el.tabIndex = name === state.laneTab ? 0 : -1;
  fitNotes();

  const puck = $('puck');
  puck.hidden = !duty;
  if (duty) {
    puck.className = 'puck ' + tagOf(duty.name) + (puckReady ? ' moves' : '');
    puck.style.setProperty('--i', [...box.children].indexOf(lanes.get(duty.name).el));
    // From the next frame on, a switch slides the tag to its new lane.
    if (!puckReady) requestAnimationFrame(() => requestAnimationFrame(() => { puckReady = true; puck.classList.add('moves'); }));
  }
}

// fitFlag keeps the handover flag on the scale and off the now flag: after its line, then after it as the
// bare time, then before it in full and bare; gone when none fits.
function fitFlag(now, flag, time) {
  const edge = $('rota-flags').getBoundingClientRect().right, full = flag.textContent;
  const fits = () => {
    const r = flag.getBoundingClientRect();
    return r.right <= edge && r.left >= now.getBoundingClientRect().right + 4;
  };
  for (const [flip, text] of [[false, full], [false, t('next.at', time)], [true, full], [true, t('next.at', time)]]) {
    flag.classList.toggle('flip', flip);
    flag.textContent = text;
    if (fits()) return;
  }
  flag.hidden = true;
}

function makeLane(name) {
  const el = h('div', 'lane');
  el.dataset.name = name;
  el.setAttribute('role', 'listitem');
  const nm = h('b', 'nm', name);
  const chipEl = h('span', 'chip');
  chipEl.setAttribute('aria-hidden', 'true');
  const track = h('div', 'r-track');
  el.append(h('div', 'r-label', chipEl, nm), track);
  return { el, nm, chip: chipEl, track };
}

// fillLane draws one account's lane in the window: x places a time on it, in percent. Notes in the table's
// words sit at their times; the state needs no word beside the name, the drawing says it.
function fillLane(l, a, { x, from, to, now, leaves }) {
  const st = state.status, kind = kindOf(a), c = tagOf(a.name);
  l.el.className = `lane ${c} k-${kind}${state.hover === a.name ? ' hl' : ''}`;
  l.el.title = `${a.name} · ${t(KINDS[kind])}`;
  l.chip.className = 'chip ' + c;
  l.nm.textContent = a.name;
  const out = [];
  const seg = (cls, s, e) => {
    const [p, q] = [x(s), x(e)];
    if (q <= p && !cls.includes('stub')) return null;
    const i = h('i', 'seg ' + cls);
    i.style.setProperty('--a', p);
    i.style.setProperty('--b', q);
    out.push(i);
    return i;
  };
  // note is a label at a time. rank orders the claim on room: 0 the account's own next change, 1 a reset
  // time, 2 a word about now. A label may also stand before `before`, the start of the segment it ends; side
  // keeps it after or before its point only; it may shrink to `short`.
  const note = (nodes, when, { cls = '', rank = 0, before, side = '', short } = {}) => {
    const n = h('span', 'lt ' + cls, ...nodes);
    n.dataset.at = x(when);
    n.dataset.before = x(before ?? when);
    n.dataset.rank = rank;
    n.dataset.side = side;
    if (short) n.dataset.short = short;
    n.style.setProperty('--a', n.dataset.at);
    out.push(n);
  };
  const tick = (when) => {
    const i = h('i', 'tk');
    i.style.setProperty('--a', x(when));
    out.push(i);
    note([clock(when)], when, { cls: 'tl', rank: 1 });
  };
  const aboutNow = { rank: 2 };

  const held = []; // the shifts drawn, in words for the lane's label
  for (const s of st.shifts || []) {
    if (s.account !== a.name || (!s.to && (kind !== 'duty' || st.unknown_active))) continue;
    const start = new Date(s.from), end = s.to ? new Date(s.to) : now;
    if (seg(s.to ? 'on' : 'on open', start, end)) held.push(s.to ? t('rota.span', clock(start), clock(end)) : t('row.since', clock(start)));
  }
  l.el.setAttribute('aria-label', `${a.name}: ${t(KINDS[kind])}, ${nextChange(a, kind)}` + (held.length ? `; ${t('rota.held', held.join(', '))}` : ''));
  const f = kind === 'resting' ? freeAt(a) : null;
  switch (kind) {
    case 'duty':
      if (leaves) seg('proj', now, Math.min(+leaves, to));
      else if (a.exhausted || !st.auto) note([nextChange(a, kind)], now, aboutNow);
      else {
        const near = closest(a);
        if (near) note([t('rota.leavesAt', pct(near.limit), near.short)], now, aboutNow);
      }
      break;
    case 'next':
      if (leaves && leaves < to) {
        seg('nx', leaves, to);
        note([nextChange(a, kind)], leaves, { side: 'after', short: t('rota.takesOver') }); // inside the outline; the flag has the time
      } else {
        seg('nx stub', now, now);
        note([nextChange(a, kind)], now, { cls: 'nudge', rank: 2 });
      }
      break;
    case 'resting': {
      if (!f && waiting(a)) {
        note([nextChange(a, kind)], now, aboutNow);
        break;
      }
      const r = seg('rest', now, f ? Math.min(+f.at, to) : to);
      if (f && +f.at <= to) {
        const words = [nextChange(a, kind)];
        if (f.x) words.push(h('span', 'm', ` · ${f.x.short} ${pct(f.x.w.utilization)}`));
        note(words, f.at, { before: now, short: clock(f.at) });
      } else {
        r?.classList.add('cut');
        note([nextChange(a, kind)], to, { side: 'before', before: now, short: f && clock(f.at) });
      }
      break;
    }
    case 'login': // since when is not known: the whole lane, under its past shifts, with its words inside
      seg('login', from, to);
      out.unshift(out.pop());
      note(tNodes('rota.login', a.name), from, { cls: 'in', side: 'after', short: t('legend.login') });
      break;
    case 'off':
      note([nextChange(a, kind)], now, aboutNow);
      break;
  }
  // The table's reset times, on this lane at their place; a resting lane's own return is labelled already.
  for (const w of [a.five_hour, a.seven_day]) {
    const when = w?.resets_at && new Date(w.resets_at);
    if (when && when > now && when <= to && !(f && +f.at === +when)) tick(when);
  }
  l.track.replaceChildren(...out);
}

// fitNotes lays out each lane's labels by rank, then in time order. None may cover another label, the now
// or handover line, or the bars, hatches and dashes the lane draws; the next account's outline and a
// needs-login box hold their own words. A label stands after its point, else before the segment it ends; in
// full, then without its detail, then as its short form; at first clear of ticks, and a next change then over
// one. A label that fits nowhere hides, leaving its tick or segment and the table to say it.
function fitNotes() {
  for (const l of lanes.values()) {
    const w = l.track.clientWidth;
    if (!w) continue;
    const px = (el, v) => el.style.getPropertyValue(v) / 100 * w;
    const segs = [...l.track.querySelectorAll('.seg:not(.login):not(.nx), .seg.nx.stub')].map((s) => [px(s, '--a'), Math.max(px(s, '--b'), px(s, '--a') + s.offsetWidth)]);
    const ticks = [...l.track.querySelectorAll('.tk')].map((i) => px(i, '--a'));
    const lines = [...$('rota-marks').children].map((v) => px(v, '--x'));
    const taken = [];
    const free = (p, q, own, strict) => p >= 0 && q <= w - 4
      && !taken.some(([u, v]) => p < v + 6 && q > u - 6)
      && !segs.some(([u, v]) => p < v + 3 && q > u - 3)
      && !lines.some((v) => v > p - 4 && v < q + 4)
      && !(strict && ticks.some((tk) => Math.abs(tk - own) > 1 && tk > p - 3 && tk < q + 3));
    const labels = [...l.track.querySelectorAll('.lt')].sort((p, q) => p.dataset.rank - q.dataset.rank || p.dataset.at - q.dataset.at);
    for (const n of labels) {
      const a = n.dataset.at / 100 * w, b = n.dataset.before / 100 * w;
      const gap = n.classList.contains('nudge') ? 26 : n.classList.contains('in') ? 6 : 8;
      const detail = n.querySelector('.m'), full = [...n.childNodes];
      const forms = [() => n.replaceChildren(...full), detail && (() => n.replaceChildren(...full.filter((c) => c !== detail))),
        n.dataset.short && (() => { n.textContent = n.dataset.short; })].filter(Boolean);
      const place = (p, q) => {
        n.style.setProperty('--dx', `${p - a}px`);
        taken.push([p, q]);
      };
      const fit = (strict) => {
        for (const form of forms) {
          form();
          const nw = n.offsetWidth;
          if (n.dataset.side !== 'before' && free(a + gap, a + gap + nw, a, strict)) return place(a + gap, a + gap + nw), true;
          if (n.dataset.side !== 'after' && free(b - gap - nw, b - gap, a, strict)) return place(b - gap - nw, b - gap), true;
        }
        return false;
      };
      if (!fit(true) && !(n.dataset.rank === '0' && fit(false))) n.classList.add('gone');
    }
  }
}

function setHover(name) {
  if (state.hover === name) return;
  state.hover = name;
  for (const el of document.querySelectorAll('.lane, .acct')) el.classList.toggle('hl', Boolean(name) && el.dataset.name === name);
}

function makeRow(name) {
  const el = $('acct-tpl').content.firstElementChild.cloneNode(true);
  translate(el);
  el.dataset.name = name;
  const r = {
    el, name, account: null,
    prio: el.querySelector('.stepper input'),
    enabled: el.querySelector('.enabled'),
    use: el.querySelector('.use'),
    msg: el.querySelector('.msg'),
    note: el.querySelector('.row-note'),
    pending: { prio: false, enabled: false, use: false },
    prioTimer: 0,
  };
  for (const b of el.querySelectorAll('.step')) {
    b.addEventListener('click', () => {
      const cur = parseInt(r.prio.value, 10);
      r.prio.value = String((Number.isNaN(cur) ? r.account.priority : cur) + Number(b.dataset.step));
      clearTimeout(r.prioTimer);
      r.prioTimer = setTimeout(() => savePriority(r), 400);
    });
  }
  r.prio.addEventListener('change', () => { clearTimeout(r.prioTimer); savePriority(r); });
  r.prio.addEventListener('keydown', (e) => { if (e.key === 'Enter') r.prio.blur(); });
  r.enabled.addEventListener('change', () => saveEnabled(r));
  r.use.addEventListener('click', () => switchTo(r));
  return r;
}

// usage fills a table cell with a window's use, its threshold tick and its reset.
function usage(td, x) {
  const line = h('span', 'use-line');
  const bar = h('span', 'mbar', h('i', 'fill'), h('i', 'tick'));
  bar.style.setProperty('--t', `${Math.min(100, x.limit)}%`);
  if (x.w) {
    bar.style.setProperty('--u', `${Math.max(0, Math.min(100, x.w.utilization))}%`);
    line.classList.toggle('over', x.w.utilization >= x.limit);
    line.append(h('span', 'pc', pct(x.w.utilization)), bar, h('span', 'rs', resetText(x.w, false)));
  } else {
    line.classList.add('none');
    line.append(h('span', 'pc', '—'), bar, h('span', 'rs', t('m.noData')));
  }
  td.replaceChildren(line);
}

function renderRow(r, a) {
  r.account = a;
  const { el } = r;
  const kind = kindOf(a);
  el.className = `acct ${tagOf(a.name)} k-${kind}${state.hover === a.name ? ' hl' : ''}`;
  el.querySelector('.c-acct .chip').className = 'chip ' + tagOf(a.name);
  const nm = el.querySelector('.nm');
  nm.textContent = a.name;
  nm.title = `${a.name} · ${a.email}`;
  el.querySelector('.mail').textContent = a.email;
  el.querySelector('.st b').textContent = t(KINDS[kind]);
  const since = kind === 'duty' && shiftStart();
  el.querySelector('.st2').textContent = since ? t('row.since', clock(since))
    : kind === 'resting' || (kind === 'duty' && a.exhausted) ? exhaustedText(a.exhausted_reason) : '';
  const ws = windows(a);
  const [five, week, models, next] = el.querySelectorAll('.c-use, .c-models, .c-next');
  usage(five, ws[0]);
  usage(week, ws[1]);
  const ms = ws.slice(2).filter((m) => m.w);
  models.replaceChildren(...(ms.length ? ms.map((m) => h('span', 'use-model' + (m.w.utilization >= m.limit ? ' over' : ''), `${m.short} ${pct(m.w.utilization)}`))
    : [h('span', 'use-model', '—')]));
  next.textContent = nextChange(a, kind);
  next.classList.toggle('m', kind === 'ready' || kind === 'off');
  for (const td of el.querySelectorAll('[data-col]')) td.dataset.label = t(td.dataset.col);

  el.querySelector('[data-step="-1"]').setAttribute('aria-label', t('row.lower', a.name));
  el.querySelector('[data-step="1"]').setAttribute('aria-label', t('row.raise', a.name));
  r.prio.setAttribute('aria-label', t('row.prio', a.name));
  r.enabled.setAttribute('aria-label', t('row.auto', a.name));
  r.use.setAttribute('aria-label', t('row.useLabel', a.name));
  if (!r.pending.use) r.use.querySelector('.label').textContent = t('row.use');
  if (!r.pending.prio && document.activeElement !== r.prio) r.prio.value = String(a.priority);
  if (!r.pending.enabled) r.enabled.checked = a.enabled;
  r.use.hidden = a.active || a.needs_login;
  el.querySelector('.cur').hidden = kind !== 'duty';

  const check = checkNote(a);
  r.note.textContent = check?.[0] || '';
  r.note.classList.toggle('bad', Boolean(check?.[1]));
  r.note.hidden = !check;
  syncRowMsg(r);
}

// syncRowMsg shows the row under an account only while it has something to say.
function syncRowMsg(r) {
  r.el.querySelector('.row-msg').hidden = r.note.hidden && r.msg.hidden;
}

function renderAccounts() {
  const table = $('accounts');
  const accts = ordered();
  for (const [name, r] of rows) {
    if (!accts.some((a) => a.name === name)) { r.el.remove(); rows.delete(name); }
  }
  $('acct-count').textContent = t('accts.count', accts.length);
  table.classList.toggle('no-models', !accts.some((a) => a.models.length));
  for (const a of accts) {
    let r = rows.get(a.name);
    if (!r) { r = makeRow(a.name); rows.set(a.name, r); }
    renderRow(r, a);
  }
  for (const a of accts) if (!rows.get(a.name).el.isConnected) table.append(rows.get(a.name).el);
  const bodies = [...table.tBodies];
  if (!bodies.some((b) => b.contains(document.activeElement))) {
    accts.forEach((a, i) => {
      const el = rows.get(a.name).el;
      if (table.tBodies[i] !== el) table.insertBefore(el, table.tBodies[i] || null);
    });
  }
  fitTable();
}

const ruler = document.createElement('canvas').getContext('2d');

// textWidth is how wide el draws s, the start of its text, with an ellipsis when s is short of it.
function textWidth(el, s) {
  const cs = getComputedStyle(el); // its font shorthand reads empty: tabular-nums has no place in it
  ruler.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
  return ruler.measureText(s.length < el.textContent.length ? s + '…' : s).width;
}

// fitTable lays the table out as one block per account where its columns do not fit the page, always so on
// narrow screens. As a table, it shows the emails beside the names only if every row has the room for its
// name whole and for its email's local part and about a dozen characters; a stub says nothing, and a
// column with gaps reads as random. The names' titles keep the emails. A name with no room for about a
// dozen characters even then means the columns do not fit.
function fitTable() {
  const table = $('accounts'), all = [...rows.values()];
  table.classList.remove('blocks', 'no-mail');
  if (stacked.matches || table.offsetWidth > table.parentElement.clientWidth + 1) {
    table.classList.add('blocks');
    return;
  }
  const cut = all.some(({ el }) => {
    const who = el.querySelector('.acc-who'), nm = el.querySelector('.nm'), mail = el.querySelector('.mail');
    const end = who.getBoundingClientRect().right, email = mail.textContent;
    const need = Math.max(textWidth(mail, email.split('@')[0] + '@'), textWidth(mail, email.slice(0, 12)));
    return nm.getBoundingClientRect().right > end + 1 || end - mail.getBoundingClientRect().left + 1 < need;
  });
  table.classList.toggle('no-mail', cut);
  if (cut && all.some(({ el }) => {
    const nm = el.querySelector('.nm'), who = el.querySelector('.acc-who').getBoundingClientRect();
    return nm.getBoundingClientRect().right > who.right + 1 && who.width + 1 < textWidth(nm, nm.textContent.slice(0, 12));
  })) table.classList.replace('no-mail', 'blocks');
}

function renderHow() {
  const st = state.status, th = st.thresholds;
  $('how').classList.toggle('off', !st.auto);
  $('how-off').hidden = st.auto;
  $('how-leave').replaceChildren(...tNodes('how.leave', pct(th.five_hour), pct(th.seven_day)));
  $('how-back').replaceChildren(...tNodes('how.back', num(th.hysteresis)));
  $('how-stay').replaceChildren(...tNodes('how.stay', duration(th.min_dwell_seconds * 1000)));
  const cache = st.strategy === 'cache-aware';
  $('how-pick').replaceChildren(...tNodes(cache ? 'how.cache' : 'how.fill'));
  const idle = st.last_activity_at ? Date.now() - new Date(st.last_activity_at) : Infinity;
  $('how-cache').textContent = !cache ? '' : idle < HOUR ? t('how.warm', duration(HOUR - idle)) : t('how.cold');
}

async function setStrategy() {
  const sel = $('strategy');
  const want = sel.value;
  const prev = state.status.strategy;
  state.strategyBusy = true;
  hold(sel, true);
  hideMsg($('hero-err'));
  try {
    await api('PUT', '/v1/strategy', { strategy: want });
    state.status.strategy = want;
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    sel.value = prev;
    showMsg($('hero-err'), `${t('x.strategy')} ${describe(e)}`);
  } finally {
    state.strategyBusy = false;
    hold(sel, false);
  }
  render();
}

let bootTimer = 0;

// reveal shows one screen. The loader waits a moment first, so a page that loads fast never flashes it.
function reveal(id) {
  clearTimeout(bootTimer);
  state.screen = id;
  for (const s of ['boot', 'auth', 'dash']) $(s).hidden = true;
  if (id === 'boot') bootTimer = setTimeout(() => { $('boot').hidden = false; }, 150);
  else $(id).hidden = false;
}

// loadDash shows the dashboard with placeholders until the first snapshot arrives.
function loadDash() {
  state.loading = true;
  state.status = state.accounts = state.loadedAt = null;
  $('dash').classList.add('loading');
  $('main').setAttribute('aria-busy', 'true');
  $('where').textContent = location.host;
  $('live-text').replaceChildren(h('span', 'lseg m', t('boot.connecting', location.host)));
  $('banner').hidden = true;
  for (const id of ['alerts', 'hero-err']) $(id).replaceChildren();
  alertsShown = updateShown = '';
  hideMsg($('update'));
  $('hero-err').hidden = true;
  fault('');
  reveal('dash');
}

// setOffline shows that the daemon cannot be reached, or no longer. With data on screen, the page then shows
// it as of its time and puts the controls that ask the daemon to sleep (draw, asleep).
function setOffline(e) {
  const b = $('banner'), was = state.offline;
  b.hidden = !e || Boolean(state.upd.to && !state.upd.lost); // the update strip says the daemon is restarting
  state.offline = Boolean(e && state.status);
  if (e) {
    const nodes = [...describeNodes(e), ' ', state.loadedAt ? t('offline.from', clock(state.loadedAt)) : t('offline.retry')];
    const text = nodes.map((n) => (typeof n === 'string' ? n : n.textContent)).join('');
    if (b.textContent !== text) b.replaceChildren(h('p', 'alert-body', ...nodes)); // each retry would announce it again
  }
  $('live').classList.toggle('bad', Boolean(e) || Boolean(state.status?.remote && !state.status.remote.ok));
  if (state.offline && !was) render();
}

// asleep disables the controls that ask the daemon while it cannot be reached, saying why in their title, and
// wakes them once it is back.
function asleep(off) {
  for (const el of document.querySelectorAll('#auto, #strategy, #refresh, #upd-go, #accounts .use, #accounts .step, #accounts .enabled, #accounts .stepper input')) {
    if (off) {
      el.classList.add('asleep');
      el.disabled = true;
      el.title = t('off.title');
    } else if (el.classList.contains('asleep')) {
      el.classList.remove('asleep');
      el.disabled = el.classList.contains('held'); // its request still runs
      el.title = el.dataset.tTitle ? t(el.dataset.tTitle) : '';
    }
  }
  syncRuleButtons();
}

function authError(text) {
  const err = $('auth-err');
  err.textContent = text;
  err.hidden = !text;
  $('auth-key').setAttribute('aria-invalid', text ? 'true' : 'false');
}

// showAuth logs out; rejected means the daemon turned the saved key down, so it is dropped too.
function showAuth(rejected, why) {
  stopStream();
  if (rejected) {
    storeKey('');
    storeAdmin('');
    state.key = '';
    state.admin = false;
  }
  reveal('auth');
  $('auth-key').type = 'password';
  $('auth-eye').setAttribute('aria-pressed', 'false');
  $('auth-eye').setAttribute('aria-label', t('auth.show'));
  authError(why || (rejected ? t('auth.rejected') : ''));
  $('auth-key').focus();
}

async function logIn(e) {
  e.preventDefault();
  const k = $('auth-key').value.trim();
  if (!k) return authError(t('auth.empty'));
  const submit = $('auth-submit');
  setBusy(submit, true, t('auth.connecting'));
  authError('');
  state.key = k;
  try {
    await api('GET', '/v1/status');
  } catch (err) {
    state.key = '';
    authError(err.code === 'unauthorized' ? t('auth.wrong')
      : err.code === 'offline' ? t('auth.offline', location.origin)
      : describe(err));
    return;
  } finally {
    setBusy(submit, false, t('auth.submit'));
  }
  storeKey(k, $('auth-remember').checked);
  $('auth-key').value = '';
  loadDash();
  connect();
}

function onEvent(block) {
  let type = 'message';
  let data = '';
  for (const line of block.split('\n')) {
    if (line.startsWith('event:')) type = line.slice(6).trim();
    else if (line.startsWith('data:')) data += line.slice(5).trimStart();
  }
  if (type !== 'state' || !data) return;
  let snap;
  try {
    snap = JSON.parse(data);
  } catch (e) { // the stream still works: a later snapshot can be read
    console.error('ccswap: an update could not be read', e);
    fault('read');
    return;
  }
  if (state.fault === 'read') fault('');
  if (state.upd.to && snap.status.version !== state.seen) { // back on the new build: its page may differ too
    try { sessionStorage.setItem(UPDATED_ITEM, snap.status.version); } catch { /* storage off: no word after the reload */ }
    location.reload();
    return;
  }
  state.seen ||= snap.status.version;
  state.status = snap.status;
  state.accounts = snap.accounts;
  state.loadedAt = new Date();
  if (state.screen !== 'dash') reveal('dash');
  if (state.loading) {
    state.loading = false;
    $('dash').classList.remove('loading');
    $('main').removeAttribute('aria-busy');
  }
  setOffline(null);
  render();
}

function stopStream() {
  const s = state.stream;
  state.stream = null;
  clearTimeout(state.retry);
  clearTimeout(state.watchdog);
  s?.abort();
}

// connect reads /v1/events with fetch: EventSource cannot send the key in a header.
async function connect() {
  stopStream();
  const ctl = new AbortController();
  state.stream = ctl;
  let failure = new ApiError('offline');
  try {
    const r = await fetch('/v1/events', { headers: { Authorization: 'Bearer ' + state.key }, cache: 'no-store', signal: ctl.signal });
    if (r.status === 401) { showAuth(Boolean(state.key)); return; }
    if (!r.ok) throw new ApiError('http', 'HTTP ' + r.status);
    const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
    let buf = '';
    for (;;) {
      clearTimeout(state.watchdog);
      state.watchdog = setTimeout(() => ctl.abort(), STALE_MS);
      const { value, done } = await reader.read();
      if (done) break;
      buf += value;
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        onEvent(buf.slice(0, i));
        buf = buf.slice(i + 2);
      }
    }
  } catch (e) {
    if (e instanceof ApiError) failure = e;
  }
  if (state.stream !== ctl) return;
  clearTimeout(state.watchdog);
  setOffline(failure);
  state.retry = setTimeout(connect, RETRY_MS);
}

function patchLocal(name, a) {
  const i = state.accounts.findIndex((x) => x.name === name);
  if (i >= 0) state.accounts[i] = a;
}

function rowMsg(r, text, kind) {
  showMsg(r.msg, text, kind);
  syncRowMsg(r);
}

function hideRowMsg(r) {
  hideMsg(r.msg);
  syncRowMsg(r);
}

async function savePriority(r) {
  if (r.pending.prio) return;
  const v = parseInt(r.prio.value, 10);
  const prev = r.account.priority;
  if (Number.isNaN(v)) { r.prio.value = String(prev); return; }
  if (v === prev) return;
  r.pending.prio = true;
  hideRowMsg(r);
  try {
    r.account = await api('PATCH', `/v1/accounts/${encodeURIComponent(r.name)}`, { priority: v });
    patchLocal(r.name, r.account);
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    r.prio.value = String(prev);
    rowMsg(r, `${t('x.prio')} ${describe(e, r.name)}`);
    return;
  } finally {
    r.pending.prio = false;
  }
  if (parseInt(r.prio.value, 10) !== r.account.priority) return savePriority(r); // stepped again while saving
  render();
}

async function saveEnabled(r) {
  const want = r.enabled.checked;
  r.pending.enabled = true;
  hold(r.enabled, true);
  hideRowMsg(r);
  try {
    patchLocal(r.name, await api('PATCH', `/v1/accounts/${encodeURIComponent(r.name)}`, { enabled: want }));
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    r.enabled.checked = !want;
    rowMsg(r, `${t('x.enabled')} ${describe(e, r.name)}`);
  } finally {
    r.pending.enabled = false;
    hold(r.enabled, false);
  }
  render();
}

// switchTo never forces a switch: the daemon takes force on its unix socket only (ccswap use --force).
async function switchTo(r) {
  r.pending.use = true;
  setBusy(r.use, true, t('row.switching'));
  hideRowMsg(r);
  try {
    const st = await api('POST', `/v1/accounts/${encodeURIComponent(r.name)}/activate`);
    if (st.warning) rowMsg(r, t('x.warning', r.name, st.warning), 'warn');
    rows.forEach((x) => { if (x !== r) hideRowMsg(x); });
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    rowMsg(r, e.code === 'near_refresh' ? t('x.near', r.name) : describe(e, r.name), e.code === 'near_refresh' ? 'warn' : 'crit');
  } finally {
    r.pending.use = false;
    setBusy(r.use, false, t('row.use'));
  }
}

async function setAuto() {
  const auto = $('auto');
  const want = auto.checked;
  state.autoBusy = true;
  hold(auto, true);
  hideMsg($('hero-err'));
  try {
    await api('PUT', '/v1/auto', { enabled: want });
    state.status.auto = want;
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    auto.checked = !want;
    showMsg($('hero-err'), `${t('x.auto')} ${describe(e)}`);
  } finally {
    state.autoBusy = false;
    hold(auto, false);
  }
  render();
}

async function refresh() {
  const b = $('refresh');
  setBusy(b, true, t('refresh.busy'));
  hideMsg($('hero-err'));
  try {
    await api('POST', '/v1/refresh');
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    showMsg($('hero-err'), `${t('x.refresh')} ${describe(e)}`);
  } finally {
    setBusy(b, false, t('refresh'));
  }
}

const ruleFields = {
  five_hour: { input: 'r-five', err: 'r-five-err' },
  seven_day: { input: 'r-week', err: 'r-week-err' },
  hysteresis: { input: 'r-hyst', err: 'r-hyst-err' },
  min_dwell: { input: 'r-dwell', err: 'r-dwell-err' },
  check_active: { input: 'r-act', err: 'r-act-err' },
  check_others: { input: 'r-oth', err: 'r-oth-err' },
};
const rules = { dirty: false, saving: false, touched: new Set(), savedTimer: 0 };

function serverRules() {
  const t = state.status.thresholds;
  return {
    five_hour: t.five_hour, seven_day: t.seven_day, hysteresis: t.hysteresis, min_dwell: t.min_dwell_seconds / 60,
    check_active: t.check_active_seconds / 60, check_others: t.check_others_seconds / 60,
  };
}

function formRules() {
  const out = {};
  for (const [k, f] of Object.entries(ruleFields)) {
    const raw = $(f.input).value.trim();
    out[k] = raw === '' ? NaN : Number(raw);
  }
  return out;
}

function ruleErrors(v) {
  const e = {};
  const pctOk = (n) => n > 0 && n <= 100;
  if (!pctOk(v.five_hour)) e.five_hour = t('rules.e.pct');
  if (!pctOk(v.seven_day)) e.seven_day = t('rules.e.pct');
  const cap = Math.min(v.five_hour, v.seven_day);
  if (!(v.hysteresis >= 0)) e.hysteresis = t('rules.e.zero');
  else if (Number.isFinite(cap) && !(v.hysteresis < cap)) e.hysteresis = t('rules.e.hystCap', num(cap));
  if (!(v.min_dwell >= 0)) e.min_dwell = t('rules.e.dwell');
  const every = (n) => n >= 1 && n <= 1440;
  if (!every(v.check_active)) e.check_active = t('rules.e.every');
  if (!every(v.check_others)) e.check_others = t('rules.e.every');
  return e;
}

function showRuleErrors(all) {
  const errs = ruleErrors(formRules());
  for (const [k, f] of Object.entries(ruleFields)) {
    const show = errs[k] && (all || rules.touched.has(k));
    const el = $(f.err);
    el.textContent = show ? errs[k] : '';
    el.hidden = !show;
    $(f.input).setAttribute('aria-invalid', show ? 'true' : 'false');
  }
  return errs;
}

function sameRules(a, b) {
  return Object.keys(ruleFields).every((k) => Math.abs(a[k] - b[k]) < 1e-9);
}

function syncRuleButtons() {
  rules.dirty = !sameRules(formRules(), serverRules());
  $('r-save').disabled = rules.saving || !rules.dirty || state.offline;
  $('r-save').title = state.offline ? t('off.title') : '';
  $('r-reset').hidden = !rules.dirty || rules.saving;
}

function renderRules() {
  const form = $('rules');
  if (!rules.dirty && !rules.saving && !form.contains(document.activeElement)) {
    const v = serverRules();
    for (const [k, f] of Object.entries(ruleFields)) $(f.input).value = String(Math.round(v[k] * 100) / 100);
  }
  syncRuleButtons();
}

async function saveRules(ev) {
  ev.preventDefault();
  if (rules.saving || !rules.dirty) return;
  const errs = showRuleErrors(true);
  const first = Object.keys(ruleFields).find((k) => errs[k]);
  if (first) { $(ruleFields[first].input).focus(); return; }
  const v = formRules();
  rules.saving = true;
  setBusy($('r-save'), true, t('rules.saving'));
  syncRuleButtons();
  hideMsg($('r-err'));
  try {
    state.status.thresholds = await api('PUT', '/v1/thresholds', {
      five_hour: v.five_hour, seven_day: v.seven_day, hysteresis: v.hysteresis, min_dwell_seconds: Math.round(v.min_dwell * 60),
      check_active_seconds: Math.round(v.check_active * 60), check_others_seconds: Math.round(v.check_others * 60),
    });
    rules.dirty = false;
    rules.touched.clear();
    $('r-status').textContent = t('rules.saved');
    clearTimeout(rules.savedTimer);
    rules.savedTimer = setTimeout(() => { $('r-status').textContent = ''; }, 4000);
  } catch (e) {
    if (e.code === 'unauthorized') return showAuth(true);
    showMsg($('r-err'), `${t('x.rules')} ${describe(e)}`);
  } finally {
    rules.saving = false;
    setBusy($('r-save'), false, t('rules.save'));
  }
  document.activeElement?.blur?.();
  render();
}

function discardRules() {
  rules.dirty = false;
  rules.touched.clear();
  showRuleErrors(false);
  hideMsg($('r-err'));
  document.activeElement?.blur?.();
  renderRules();
}

// switchLang loads another language's texts. If they do not come, the page stays in its language and says
// so next to the switch; of two quick picks, the last wins.
async function switchLang(code) {
  const res = await changeLang(code);
  if (res === 'stale') return;
  applyLang();
  const name = languages.find((l) => l.code === code)?.name ?? code;
  for (const err of document.querySelectorAll('.lang-switch .field-err')) {
    err.textContent = res === 'failed' ? t('lang.failed', name) : '';
    err.hidden = res !== 'failed';
  }
}

function applyLang() {
  document.documentElement.lang = lang;
  const locale = t('locale');
  timeFmt = new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' });
  dayFmt = new Intl.DateTimeFormat(locale, { weekday: 'short' });
  dateFmt = new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short' });
  numFmt = new Intl.NumberFormat(locale, { maximumFractionDigits: 1 });
  translate(document);
  for (const tpl of document.querySelectorAll('template')) translate(tpl.content);
  for (const box of document.querySelectorAll('.lang-switch')) {
    box.hidden = languages.length < 2;
    const sel = box.querySelector('select');
    sel.replaceChildren(...languages.map(({ code, name }) => Object.assign(new Option(name, code), { lang: code })));
    sel.value = lang;
  }
  $('auth-hint').replaceChildren(...tNodes('auth.hint'));
  $('auth-addr').replaceChildren(...tNodes('auth.addr', location.origin));
  const eye = $('auth-eye');
  eye.setAttribute('aria-label', t(eye.getAttribute('aria-pressed') === 'true' ? 'auth.hide' : 'auth.show'));
  document.title = pageTitle(state.status?.host);
  if (state.screen === 'boot') $('boot-msg').textContent = t('boot.connecting', location.host);
  if (state.status) {
    render();
    showRuleErrors(false);
  }
  if (state.fault) fault(state.fault); // in the new language
  if (!state.status && state.loading) {
    $('live-text').replaceChildren(h('span', 'lseg m', t('boot.connecting', location.host)));
  }
}

// laneKeys moves between the rota's lanes with the arrow keys, Home and End, as in a list; the focused lane
// is the one tab stop.
function laneKeys(e) {
  const cur = e.target.closest('.lane'), box = $('rota-lanes');
  const to = { ArrowDown: cur?.nextElementSibling, ArrowUp: cur?.previousElementSibling, Home: box.firstElementChild, End: box.lastElementChild }[e.key];
  if (!to) return;
  e.preventDefault();
  to.focus();
}

function laneFocus(e) {
  const el = e.target.closest('.lane');
  if (!el) return;
  state.laneTab = el.dataset.name;
  for (const l of lanes.values()) l.el.tabIndex = l.el === el ? 0 : -1;
}

async function init() {
  reveal('boot');
  await loadTexts(wanted(), () => { $('boot-msg').textContent = 'Cannot reach ccswap, retrying…'; });
  applyLang();
  const code = takeCode();
  let why = '', admin = '';
  if (code) {
    try {
      admin = (await api('POST', '/v1/web/session', { code })).key;
      storeAdmin(admin);
    } catch (e) {
      why = describe(e);
    }
  }
  admin ||= adminKey();
  state.admin = Boolean(admin);
  try {
    state.updated = sessionStorage.getItem(UPDATED_ITEM) || '';
    sessionStorage.removeItem(UPDATED_ITEM);
  } catch { /* storage off */ }
  if (state.updated) {
    setTimeout(() => {
      state.updated = '';
      if (state.status) render();
    }, 15000);
  }
  state.key = admin || storedKey();

  $('auth-form').addEventListener('submit', logIn);
  $('auth-key').addEventListener('input', () => authError(''));
  $('auth-eye').addEventListener('click', () => {
    const eye = $('auth-eye');
    const show = eye.getAttribute('aria-pressed') !== 'true';
    eye.setAttribute('aria-pressed', String(show));
    eye.setAttribute('aria-label', t(show ? 'auth.hide' : 'auth.show'));
    $('auth-key').type = show ? 'text' : 'password';
  });
  $('forget').addEventListener('click', () => {
    storeKey('');
    storeAdmin('');
    state.key = '';
    state.admin = false;
    showAuth(false);
  });
  $('refresh').addEventListener('click', refresh);
  $('auto').addEventListener('change', setAuto);
  $('strategy').addEventListener('change', setStrategy);
  for (const sel of document.querySelectorAll('.lang-switch select')) sel.addEventListener('change', () => switchLang(sel.value));

  // Pointing at or focusing an account on the rota lights up its table row, and the other way round.
  for (const root of [$('rota-lanes'), $('accounts')]) {
    const named = (e) => e.target.closest?.('[data-name]')?.dataset.name || '';
    root.addEventListener('pointerover', (e) => setHover(named(e)));
    root.addEventListener('pointerleave', () => setHover(document.activeElement?.closest?.('[data-name]')?.dataset.name || ''));
    root.addEventListener('focusin', (e) => setHover(named(e)));
    root.addEventListener('focusout', (e) => { if (!root.contains(e.relatedTarget)) setHover(''); });
  }
  $('rota-lanes').addEventListener('keydown', laneKeys);
  $('rota-lanes').addEventListener('focusin', laneFocus);
  phone.addEventListener('change', () => {
    if (!state.status) return;
    fitTop();
    if (state.accounts.length) renderRota();
  });
  stacked.addEventListener('change', () => { if (state.status) fitTable(); });
  // Labels, flags and the folds are measured in pixels: a resize or the web fonts arriving measure again.
  let refitting = 0;
  const refit = () => {
    cancelAnimationFrame(refitting);
    refitting = requestAnimationFrame(() => {
      if (state.screen !== 'dash' || !state.status) return;
      fitTop();
      fitTable();
      if (state.accounts.length) renderRota();
    });
  };
  window.addEventListener('resize', refit);
  document.fonts?.addEventListener('loadingdone', refit);

  const form = $('rules');
  form.addEventListener('submit', saveRules);
  form.addEventListener('input', () => { syncRuleButtons(); showRuleErrors(false); });
  form.addEventListener('focusout', (e) => {
    const k = e.target.name;
    if (k in ruleFields) { rules.touched.add(k); showRuleErrors(false); }
  });
  $('r-reset').addEventListener('click', discardRules);
  window.addEventListener('beforeunload', (e) => { if (rules.dirty) e.preventDefault(); });

  // A hidden tab drops its stream: a browser opens at most 6 HTTP/1.1 connections to one host, and
  // a few idle tabs holding streams would leave none for the visible one.
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) stopStream();
    else if (state.key && state.screen !== 'auth') connect();
  });
  setInterval(() => { if (state.screen === 'dash' && state.status) render(); }, 15000);

  if (state.key) {
    loadDash();
    if (why) showMsg($('hero-err'), why);
    connect();
  } else {
    showAuth(false, why);
  }
}

init();
