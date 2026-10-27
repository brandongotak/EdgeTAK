// EdgeTAK translations. /admin/i18n.js sends the chosen language's
// dictionary (English text -> translation, {0}, {1}... standing for values,
// {1|Gerät|Geräte} choosing a word by whether the English value is empty)
// followed by this file, which swaps English text in the page for it: text
// in the HTML, text the pages add later, a few attributes, and confirm,
// alert and prompt messages. Text without a translation stays English.
(() => {
  const lang = window.EDGETAK_LANG || 'en';
  const dict = window.EDGETAK_I18N || {};
  document.documentElement.lang = lang;
  const norm = (s) => s.replace(/\s+/g, ' ').trim();

  const exact = new Map();
  const patterns = [];
  const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  for (const [k, v] of Object.entries(dict)) {
    if (!/\{\d+\}/.test(k)) {
      exact.set(k, v);
      continue;
    }
    const order = [];
    const lits = [];
    const re = k.split(/(\{\d+\})/).map((part) => {
      const m = /^\{(\d+)\}$/.exec(part);
      if (m) {
        order.push(Number(m[1]));
        return '(.*?)';
      }
      if (part) lits.push(part);
      return escapeRe(part);
    }).join('');
    patterns.push({ re: new RegExp('^' + re + '$', 's'), v, order, lits });
  }
  // Longer patterns first: they say more about the text.
  for (const p of patterns) p.litLen = p.lits.join('').length;
  patterns.sort((a, b) => b.litLen - a.litLen);

  // value translates a pattern's captured value when it is itself
  // translatable text (a word, a status), keeping its spacing.
  function value(v) {
    const k = v.trim();
    const out = k && lookup(k);
    return out == null ? v : v.replace(k, out);
  }

  const cache = new Map();
  function lookup(key) {
    if (cache.has(key)) return cache.get(key);
    let out = exact.has(key) ? exact.get(key) : null;
    if (out === null) {
      for (const p of patterns) {
        if (!p.lits.every((l) => key.includes(l))) continue;
        const m = p.re.exec(key);
        // A short pattern ("{0} on {1}") must not swallow a whole
        // sentence that merely contains its words, nor reach across a
        // " · " that joins separate pieces of text.
        if (!m || key.length - p.litLen > 3 * p.litLen + 20) continue;
        // Nor take a run of words as a value ("{0} received" in "Post to a
        // channel when a webhook request is received"): values are numbers,
        // names, times.
        if (p.litLen < 12 && m.slice(1).some((c) => (c.match(/ /g) || []).length >= 4)) continue;
        if (!p.lits.some((l) => l.includes('·')) && m.slice(1).some((c) => c.includes(' · '))) continue;
        // {n|one|many}: where the English adds a plural ending ("device{1}"
        // with "" or "s"), the first form when its value is empty.
        out = p.v.replace(/\{(\d+)(?:\|([^|}]*)\|([^}]*))?\}/g, (_, n, one, many) => {
          const i = p.order.indexOf(Number(n));
          if (i < 0) return '';
          if (one !== undefined) return m[i + 1].trim() === '' ? one : many;
          return value(m[i + 1]);
        });
        break;
      }
    }
    // Pieces joined with " · " (status lines) are translated one by one.
    if (out === null && key.includes(' · ')) {
      const parts = key.split(' · ').map((s) => lookup(s) ?? s);
      const joined = parts.join(' · ');
      if (joined !== key) out = joined;
    }
    if (cache.size > 20000) cache.clear();
    cache.set(key, out);
    return out;
  }

  // t translates a string (keeping blank-line paragraphs, as in confirm
  // messages); untranslated text comes back unchanged.
  function t(s) {
    if (lang === 'en' || s == null) return s;
    s = String(s);
    if (s.includes('\n\n')) return s.split('\n\n').map(t).join('\n\n');
    const key = norm(s);
    return (key && lookup(key)) ?? s;
  }
  window.t = t;

  // Language pickers (<select data-lang-pick>): the choice is a cookie the
  // server reads.
  document.addEventListener('DOMContentLoaded', () => {
    for (const sel of document.querySelectorAll('select[data-lang-pick]')) {
      sel.value = lang;
      sel.addEventListener('change', () => {
        document.cookie = `edgetak_lang=${sel.value}; path=/; max-age=31536000; SameSite=Lax`;
        location.reload();
      });
    }
  });
  if (lang === 'en') return;

  const SKIP = new Set(['SCRIPT', 'STYLE', 'CODE', 'PRE', 'TEXTAREA', 'KBD', 'SAMP']);
  const ATTRS = ['title', 'placeholder', 'aria-label', 'alt'];
  const skipped = (el) => !el || el.closest('[data-no-i18n], .mono, code, pre, textarea');

  function textNode(n) {
    const p = n.parentElement;
    if (!p || SKIP.has(p.tagName) || skipped(p)) return;
    const key = norm(n.data);
    if (!key || !/[A-Za-z]/.test(key)) return;
    const out = lookup(key);
    if (out != null && out !== key) {
      const lead = /^\s*/.exec(n.data)[0], trail = /\s*$/.exec(n.data)[0];
      n.data = lead + out + trail;
    }
  }
  function element(el) {
    if (skipped(el)) return;
    for (const a of ATTRS) {
      const v = el.getAttribute(a);
      if (v) {
        const out = lookup(norm(v));
        if (out != null) el.setAttribute(a, out);
      }
    }
    if (el.tagName === 'INPUT' && (el.type === 'submit' || el.type === 'button') && el.value) {
      const out = lookup(norm(el.value));
      if (out != null) el.value = out;
    }
  }
  function walk(root) {
    if (root.nodeType === Node.TEXT_NODE) return textNode(root);
    if (root.nodeType !== Node.ELEMENT_NODE || SKIP.has(root.tagName)) return;
    element(root);
    const w = (root.ownerDocument || document).createTreeWalker(root, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      if (n.nodeType === Node.TEXT_NODE) textNode(n);
      else element(n);
    }
  }

  // translateDOM translates another document's content (printed sheets).
  window.translateDOM = (root) => {
    walk(root);
    if (root.ownerDocument) root.ownerDocument.title = t(root.ownerDocument.title);
  };

  // Setting a translation fires the observer again; the translation isn't
  // a dictionary key, so nothing changes the second time.
  const observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === 'childList') r.addedNodes.forEach(walk);
      else if (r.type === 'characterData') textNode(r.target);
      else if (r.type === 'attributes') element(r.target);
    }
  });
  document.addEventListener('DOMContentLoaded', () => {
    walk(document.body);
    observer.observe(document.body, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ATTRS });
    // The pages set their titles once they know the server's name.
    const title = () => {
      const v = t(document.title);
      if (v !== document.title) document.title = v;
    };
    title();
    const el = document.querySelector('title');
    if (el) new MutationObserver(title).observe(el, { childList: true, characterData: true, subtree: true });
  });

  // Dialogs are text the page never shows.
  const { confirm, alert, prompt } = window;
  window.confirm = (m) => confirm.call(window, t(m));
  window.alert = (m) => alert.call(window, t(m));
  window.prompt = (m, d) => prompt.call(window, t(m), d);
})();
