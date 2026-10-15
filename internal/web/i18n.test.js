'use strict';
// The page's i18n.js on the real texts: node --test internal/web/i18n.test.js (go test ./internal/web/ runs it
// when node is installed; scripts/linux-test.sh runs it in a node container).
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const dir = path.join(__dirname, '..', 'i18n', 'locales');
const files = Object.fromEntries(fs.readdirSync(dir).filter((f) => f.endsWith('.json'))
  .map((f) => [f.slice(0, -5), JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'))]));
const codes = Object.keys(files).sort();
const plain = (x) => JSON.parse(JSON.stringify(x));

// catalog is what GET /v1/i18n/<code> answers: code's web. messages over English's, without the prefix.
function catalog(code) {
  const messages = {};
  for (const src of ['en', code]) {
    for (const [k, v] of Object.entries(files[src])) if (k.startsWith('web.')) messages[k.slice(4)] = v;
  }
  return { lang: code, name: files[code]['lang.name'], languages: codes.map((c) => ({ code: c, name: files[c]['lang.name'] })), messages };
}

// serve answers like the daemon for a file's exact tag, else 404.
function serve(url) {
  const code = decodeURIComponent(url.split('/').pop());
  return code in files ? { ok: true, status: 200, json: async () => catalog(code) } : { ok: false, status: 404 };
}

// page loads i18n.js into a context of its own, with fetch, storage, navigator and timers stubbed.
function page({ fetch = serve, stored = {}, langs = ['en-GB'], storage = true, lang } = {}) {
  const requests = [], delays = [], store = { ...stored };
  const off = () => { throw new Error('storage off'); };
  const ctx = vm.createContext({
    document: { createElement: (tag) => ({ tag, textContent: '' }) },
    localStorage: storage ? { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = v; } } : { getItem: off, setItem: off },
    navigator: { languages: langs },
    AbortSignal,
    setTimeout: (fn, ms) => {
      if (delays.push(ms) > 50) throw new Error('retries without end');
      setImmediate(fn);
    },
    fetch: async (url) => { requests.push(url); return fetch(url); },
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8'), ctx);
  if (lang) ctx.useTexts(catalog(lang));
  return { ctx, lang: () => vm.runInContext('lang', ctx), requests, delays, store };
}

const nodes = (list) => [...list].map((n) => (typeof n === 'string' ? { text: n } : { tag: n.tag, text: n.textContent }));
const count = (s, part) => s.split(part).length - 1;

// moved is a translation whose word order moves marks and placeholders, as a German one may.
const moved = {
  ...files.en,
  'web.host.note': '**{0}**: Einstellungen gelten nur hier. Andere Rechner: `ccswap web` dort.',
  'web.how.leave': '**{1}** einer Woche oder **{0}** von 5 Stunden: geht sofort',
};

// Every page message of every language, each plural form alone, rendered with the arguments
// <script>0</script>, <script>1</script> …: a <b> or <code> per mark pair, each argument as text as often as
// its placeholder, and no mark left over.
for (const [code, file] of Object.entries({ ...files, 'qaa (moved marks)': moved })) {
  test(`${code}: marks become elements, arguments stay text`, () => {
    const { ctx } = page();
    for (const [k, v] of Object.entries(file)) {
      if (!k.startsWith('web.')) continue;
      for (const [form, s] of Object.entries(typeof v === 'string' ? { other: v } : v)) {
        const where = `${code}: ${k} (${form}) ${JSON.stringify(s)}`;
        ctx.useTexts({ lang: 'en', languages: [], messages: { m: s } });
        const args = Array.from({ length: Math.max(0, ...[...s.matchAll(/\{(\d+)\}/g)].map((m) => +m[1] + 1)) }, (_, i) => `<script>${i}</script>`);
        const out = nodes(ctx.tNodes('m', ...args));
        assert.equal(out.filter((n) => n.tag === 'b').length, count(s, '**') / 2, where);
        assert.equal(out.filter((n) => n.tag === 'code').length, count(s, '`') / 2, where);
        const flat = ctx.t('m', ...args);
        for (const text of [...out.map((n) => n.text), flat]) assert.doesNotMatch(text, /\*\*|`/, where);
        args.forEach((a, i) => {
          assert.equal(count(out.map((n) => n.text).join(''), a), count(s, `{${i}}`), `${where}: argument ${i}`);
          assert.equal(count(flat, a), count(s, `{${i}}`), `${where}: argument ${i} in t()`);
        });
      }
    }
  });
}

test('arguments are text, never marks or placeholders, wherever word order puts them', () => {
  const { ctx } = page();
  ctx.useTexts({ lang: 'qaa', languages: [], messages: { leave: moved['web.how.leave'] } });
  const bold = nodes(ctx.tNodes('leave', '**a**', '`b` <i>')).filter((n) => n.tag === 'b').map((n) => n.text);
  assert.deepEqual(bold, ['`b` <i>', '**a**']);
  assert.ok(ctx.t('leave', '{1}', '**{0}** `x`').startsWith('**{0}** `x` einer Woche oder {1} von'));
});

for (const code of codes) {
  test(`${code}: a missing argument is left out, a missing key reads as itself`, () => {
    const { ctx } = page({ lang: code });
    const s = ctx.t('e.needs_login');
    assert.doesNotMatch(s, /undefined|\{\d\}/);
    assert.equal(ctx.t('no.such'), 'no.such');
  });

  test(`${code}: web.locale formats times`, () => {
    const loc = page({ lang: code }).ctx.t('locale');
    assert.deepEqual(Intl.DateTimeFormat.supportedLocalesOf([loc]), [loc]);
    assert.equal(Intl.PluralRules.supportedLocalesOf([code]).length, 1);
    const at = new Date(2026, 0, 2, 14, 5);
    assert.match(new Intl.DateTimeFormat(loc, { hour: '2-digit', minute: '2-digit' }).format(at), /\d.*05/);
    assert.ok(new Intl.DateTimeFormat(loc, { weekday: 'short' }).format(at));
  });
}

test('plural forms follow the language', () => {
  const want = { en: ['1 account', '2 accounts', '5 accounts', '21 accounts'], ru: ['1 аккаунт', '2 аккаунта', '5 аккаунтов', '21 аккаунт'] };
  for (const [code, forms] of Object.entries(want)) {
    const { ctx } = page({ lang: code });
    assert.deepEqual([1, 2, 5, 21].map((n) => ctx.t('accts.count', n)), forms);
  }
  const { ctx } = page();
  ctx.useTexts({ lang: 'ru', languages: [], messages: { x: { other: 'n={0}' } } });
  assert.equal(ctx.t('x', 1), 'n=1');
});

test('wanted: the kept choice, each browser language and its base, then English', () => {
  const langs = ['pt-BR', 'de', 'en-US', 'pt-PT'];
  assert.deepEqual(plain(page({ stored: { 'ccswap-lang': 'ru' }, langs }).ctx.wanted()), ['ru', 'pt-BR', 'pt', 'de', 'en-US', 'en', 'pt-PT']);
  assert.deepEqual(plain(page({ storage: false, langs: ['en-GB', 'en'] }).ctx.wanted()), ['en-GB', 'en']);
});

test('loadTexts takes the first language the daemon has', async () => {
  const p = page();
  let slow = 0;
  await p.ctx.loadTexts(['qaa-QA', 'qaa', 'ru', 'en'], () => slow++);
  assert.equal(p.lang(), 'ru');
  assert.deepEqual(p.requests, ['/v1/i18n/qaa-QA', '/v1/i18n/qaa', '/v1/i18n/ru']);
  assert.deepEqual([slow, p.delays.length], [0, 0]);
});

test('loadTexts retries with backoff and says so after the third failure', async () => {
  let n = 0, slowAt = -1;
  const p = page({ fetch: (url) => (++n <= 8 ? Promise.reject(new TypeError('offline')) : serve(url)) });
  await p.ctx.loadTexts(['en'], () => { slowAt = p.delays.length; });
  assert.equal(p.lang(), 'en');
  assert.equal(slowAt, 2);
  assert.deepEqual(p.delays, [1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]);
});

test('changeLang switches and keeps the choice', async () => {
  const p = page({ lang: 'en' });
  assert.equal(await p.ctx.changeLang('ru'), 'ok');
  assert.equal(p.lang(), 'ru');
  assert.equal(p.ctx.t('auth.submit'), files.ru['web.auth.submit']);
  assert.equal(p.store['ccswap-lang'], 'ru');
});

test('changeLang that fails keeps the language', async () => {
  for (const fetch of [() => Promise.reject(new TypeError('offline')), () => ({ ok: false, status: 500 }), () => ({ ok: false, status: 404 })]) {
    const p = page({ lang: 'en', fetch });
    assert.equal(await p.ctx.changeLang('ru'), 'failed');
    assert.equal(p.lang(), 'en');
    assert.equal(p.store['ccswap-lang'], undefined);
  }
});

test('changeLang: the last language asked for wins', async () => {
  let release;
  const held = new Promise((done) => { release = done; });
  const p = page({ lang: 'en', fetch: async (url) => { if (url.endsWith('/ru')) await held; return serve(url); } });
  const first = p.ctx.changeLang('ru');
  assert.equal(await p.ctx.changeLang('en'), 'ok');
  release();
  assert.equal(await first, 'stale');
  assert.equal(p.lang(), 'en');
  assert.equal(p.store['ccswap-lang'], 'en');
});
