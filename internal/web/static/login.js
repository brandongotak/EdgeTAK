'use strict';

// The one sign-in page. Administrators go on to the admin console, everyone
// else to their account page; ?next= returns people to where they started.

const $ = (sel) => document.querySelector(sel);
const form = $('#signin-form');

// Only the console and the account page are valid destinations.
const next = (() => {
  const n = new URLSearchParams(location.search).get('next') || '';
  return /^\/(admin|account)(\/|$|#|\?)/.test(n) ? n : '';
})();

function destination(admin) {
  if (next.startsWith('/admin')) return admin ? next : '/account';
  return next || (admin ? '/admin/' : '/account');
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  $('#signin-error').textContent = '';
  const r = await fetch('/account/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-EdgeTAK': '1' },
    body: JSON.stringify({ username: form.username.value, password: form.password.value, code: form.code.value }),
  });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) {
    if (body.need_code) { // two-factor: ask for the code, keep what was typed
      const label = $('#signin-code');
      const first = label.hidden;
      label.hidden = false;
      form.code.value = '';
      form.code.focus();
      $('#signin-error').textContent = first ? '' : body.error;
      return;
    }
    $('#signin-error').textContent = body.error || 'Sign in failed';
    return;
  }
  location.replace(destination(body.admin));
});

(async () => {
  try {
    const info = await (await fetch('/account/api/info')).json();
    if (info.server_name) {
      $('#server-name').textContent = info.server_name;
      document.title = `${info.server_name} sign in`;
    }
    $('#signup-link').hidden = !info.signup;
  } catch { /* keep defaults */ }
  fetch('/Marti/api/version').then((r) => (r.ok ? r.text() : '')).then((v) => { $('#signin-version').textContent = v; }).catch(() => {});
  // Already signed in: go straight on. An admin sent here by the console has
  // no console session (it expired), so they sign in again instead.
  try {
    const r = await fetch('/account/api/me');
    if (r.ok) {
      const me = await r.json();
      if (!(next.startsWith('/admin') && me.admin)) {
        location.replace(destination(me.admin));
        return;
      }
    }
  } catch { /* show the form */ }
  form.hidden = false;
  form.username.focus();
})();
