'use strict';

const LANG_ITEM = 'ccswap-lang';

// The texts come from the daemon (GET /v1/i18n/<lang>): messages with {0} {1} … placeholders and **bold**
// and `code` marks, and plural messages as their CLDR forms, picked by argument 0.
let lang = 'en';
let languages = [];
let messages = {};
let plural = new Intl.PluralRules('en');

function useTexts(c) {
  ({ lang, languages, messages } = c);
  plural = new Intl.PluralRules(lang);
}

// wanted are the languages to ask for, best first: the one picked on this device, then each browser
// language as it is and as its base language (pt-BR, then pt), then English.
function wanted() {
  const out = [];
  try {
    const saved = localStorage.getItem(LANG_ITEM);
    if (saved) out.push(saved);
  } catch { /* storage off */ }
  for (const l of navigator.languages || [navigator.language]) {
    if (l) out.push(l, l.split('-')[0].toLowerCase());
  }
  return [...new Set([...out, 'en'])];
}

// fetchTexts is the catalog of the first of codes the daemon has; it throws when it has none of them or
// cannot be asked.
async function fetchTexts(codes) {
  for (const code of codes) {
    const r = await fetch('/v1/i18n/' + encodeURIComponent(code), { signal: AbortSignal.timeout(20000) });
    if (r.status === 404) continue;
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.json();
  }
  throw new Error('no such language');
}

// loadTexts takes the texts of the first of codes the daemon has. Until it gets them it tries again, waiting
// twice as long each time up to 30 s, and calls slow after the third failure.
async function loadTexts(codes, slow) {
  for (let fails = 1, wait = 1000; ; fails++, wait = Math.min(2 * wait, 30000)) {
    try {
      return useTexts(await fetchTexts(codes));
    } catch { /* try again */ }
    if (fails === 3) slow();
    await new Promise((done) => setTimeout(done, wait));
  }
}

let asked = 0;

// changeLang switches to code's texts and keeps the choice on this device: 'ok', 'failed' (the language
// stays), or 'stale' when another language was asked for meanwhile, as the last one asked wins.
async function changeLang(code) {
  const mine = ++asked;
  let c;
  try {
    c = await fetchTexts([code]);
  } catch {
    return mine === asked ? 'failed' : 'stale';
  }
  if (mine !== asked) return 'stale';
  useTexts(c);
  keepLang(lang);
  return 'ok';
}

function keepLang(l) {
  try { localStorage.setItem(LANG_ITEM, l); } catch { /* storage off: this page only */ }
}

// message is key's text, a plural message's form for args[0]; the key itself when the daemon has none.
function message(key, args) {
  const m = messages[key] ?? key;
  return typeof m === 'string' ? m : m[plural.select(Number(args[0]))] ?? m.other;
}

// fill puts the arguments in for {0} {1} …; a missing one is left out.
const fill = (s, args) => s.replace(/\{(\d+)\}/g, (_, i) => String(args[i] ?? ''));

// t is key's text with its arguments, marks left out: for textContent and attributes.
function t(key, ...args) {
  return fill(message(key, args).replace(/\*\*|`/g, ''), args);
}

// tNodes is key's text as nodes: **bold** becomes <b>, `code` <code>. Arguments are text, never markup.
function tNodes(key, ...args) {
  return message(key, args).split(/(\*\*[^*]*\*\*|`[^`]*`)/).map((part, i) => {
    if (i % 2 === 0) return fill(part, args);
    const bold = part.startsWith('**');
    const el = document.createElement(bold ? 'b' : 'code');
    el.textContent = fill(bold ? part.slice(2, -2) : part.slice(1, -1), args);
    return el;
  }).filter((n) => n !== '');
}

function translate(root) {
  for (const el of root.querySelectorAll('[data-t]')) el.textContent = t(el.dataset.t);
  for (const el of root.querySelectorAll('[data-t-label]')) el.setAttribute('aria-label', t(el.dataset.tLabel));
  for (const el of root.querySelectorAll('[data-t-ph]')) el.placeholder = t(el.dataset.tPh);
  for (const el of root.querySelectorAll('[data-t-title]')) el.title = t(el.dataset.tTitle);
}
