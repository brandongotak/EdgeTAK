'use strict';

const $ = (sel) => document.querySelector(sel);
const invite = new URLSearchParams(location.search).get('invite') || '';
let info = null;
let creds = null; // kept in memory only, for the package downloads and QR

function show(id) {
  for (const el of ['#loading', '#closed', '#register-form', '#pending', '#done']) $(el).hidden = el !== id;
}

async function load() {
  const r = await fetch('/api/register/info' + (invite ? `?invite=${encodeURIComponent(invite)}` : ''));
  info = await r.json();
  showLogo(info.logo);
  if (info.server_name) {
    $('#server-name').textContent = info.server_name;
    document.title = `${info.server_name} sign-up`;
  }
  if (invite) {
    if (!info.invite?.valid) {
      $('#closed').querySelector('h2').textContent = 'Invite not valid';
      $('#closed-text').textContent = 'This invite link has expired, been used up or been revoked. Ask an administrator for a new one.';
      return show('#closed');
    }
    $('#intro').textContent = (info.invite.label ? `You were invited: ${info.invite.label}. Choose a username and password.` : 'You were invited. Choose a username and password.')
      + (info.invite.event_ends ? ` This account is for the event only: it is removed on ${new Date(info.invite.event_ends).toLocaleString()}.` : '');
  } else if (info.mode === 'approval') {
    $('#intro').textContent = 'Request an account. An administrator approves new accounts before you can sign in.';
    $('#note-field').hidden = false;
    $('#register-submit').textContent = 'Request account';
  } else if (info.mode === 'open') {
    $('#intro').textContent = 'Choose a username and password for this TAK server.';
  } else {
    return show('#closed');
  }
  show('#register-form');
}

$('#register-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const err = $('#register-error');
  err.textContent = '';
  if (f.get('password') !== f.get('confirm')) {
    err.textContent = 'The passwords do not match';
    return;
  }
  const btn = $('#register-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/register', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: f.get('username'), password: f.get('password'), note: f.get('note') || '', invite }),
    });
    let body = {};
    try { body = await r.json(); } catch { /* not JSON */ }
    if (!r.ok) throw new Error(body.error || r.statusText);
    if (body.status === 'pending') {
      $('#pending-user').textContent = body.username;
      return show('#pending');
    }
    creds = { username: body.username, password: f.get('password') };
    showDone(body.qr_link);
  } catch (ex) {
    err.textContent = ex.message;
  } finally {
    btn.disabled = false;
  }
});

function showDone(qrLink) {
  const host = info.host || location.hostname;
  $('#done-user').textContent = creds.username;
  $('#done-host').textContent = host;
  $('#done-port').textContent = info.streaming_port || '8089';
  // Typing the address only works when the server has a publicly trusted
  // certificate; otherwise the package carries the trust store.
  $('#quick').hidden = !info.quick_connect;
  $('#package-only').hidden = info.quick_connect;
  // The QR code holds a one-time code from the server, so it works with or
  // without a publicly trusted certificate.
  $('#qr-block').hidden = !(qrLink && window.QRCode);
  if (qrLink && window.QRCode) {
    new QRCode($('#qr'), { text: qrLink, width: 200, height: 200, correctLevel: QRCode.CorrectLevel.M });
  }
  show('#done');
}

for (const b of document.querySelectorAll('[data-pkg]')) {
  b.addEventListener('click', async () => {
    $('#pkg-error').textContent = '';
    try {
      const r = await fetch(`/api/register/package?mode=${b.dataset.pkg}`, {
        headers: { Authorization: 'Basic ' + btoa(unescape(encodeURIComponent(`${creds.username}:${creds.password}`))) },
      });
      if (!r.ok) throw new Error(`Download failed (${r.status})`);
      const blob = await r.blob();
      const m = (r.headers.get('content-disposition') || '').match(/filename="?([^";]+)"?/);
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = m ? m[1] : 'edgetak.zip';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 5000);
    } catch (ex) {
      $('#pkg-error').textContent = ex.message;
    }
  });
}

load().catch(() => {
  $('#closed-text').textContent = 'Could not reach the server.';
  show('#closed');
});
