'use strict';

// The account page: any user's own password, connection packages, devices
// and certificates. It talks only to /account/api, which uses its own
// session cookie.

const $ = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const plural = (n, word) => `${n} ${word}${Number(n) === 1 ? '' : 's'}`;
let info = {};
let me = null;
let pollTimer = null;

async function api(method, path, body) {
  const r = await fetch('/account/api/' + path, {
    method, headers: { 'Content-Type': 'application/json', 'X-EdgeTAK': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await r.json(); } catch { /* empty */ }
  if (!r.ok) {
    const err = new Error(data?.error || r.statusText);
    err.status = r.status;
    err.data = data;
    throw err;
  }
  return data;
}

function toast(msg, bad = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('err', bad);
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, 3500);
}

function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} days ago`;
}

function daysLeft(d) {
  if (d < 0) return `expired ${plural(-d, 'day')} ago`;
  if (d === 0) return 'expires today';
  return `${plural(d, 'day')} left`;
}

// Signing in happens on the shared page at /, which brings people back here.
function showSignin() {
  clearInterval(pollTimer);
  location.replace('/?next=/account');
}

$('#signout').addEventListener('click', async () => {
  await fetch('/account/api/logout', { method: 'POST', headers: { 'X-EdgeTAK': '1' } });
  location.replace('/');
});

async function start() {
  try {
    me = await api('GET', 'me');
  } catch (ex) {
    if (ex.status === 401) return showSignin();
    throw ex;
  }
  $('#page').hidden = false;
  $('#me').textContent = me.username;
  $('#admin-link').hidden = !me.admin;
  $('#conn-host').textContent = info.host || location.hostname;
  $('#conn-port').textContent = info.streaming_port || '8089';
  $('#conn-user').textContent = me.username;
  render();
  loadTwoFactor().catch(() => {});
  clearInterval(pollTimer);
  pollTimer = setInterval(refresh, 10000);
}

async function refresh() {
  try {
    me = await api('GET', 'me');
    render();
  } catch (ex) {
    if (ex.status === 401) showSignin();
  }
}

function render() {
  const chips = (gs) => gs.length ? gs.map((g) => `<span class="pill">${esc(g)}</span>`).join(' ') : '<span class="muted">none</span>';
  $('#groups-in').innerHTML = chips(me.groups_in);
  $('#groups-out').innerHTML = chips(me.groups_out);
  // Directory users change their password in the directory.
  $('#password-card').hidden = !!me.directory;

  $('#devices').innerHTML = me.devices.length ? me.devices.map((d) => `<div class="account-item">
      <div><strong>${esc(d.callsign || '(no position report yet)')}</strong> <span class="pill ${d.transport === 'tls' ? 'ok' : 'warn'}">${esc(d.transport.toUpperCase())}</span>
        ${d.battery != null ? `<span class="pill ${d.battery < 20 ? 'bad' : d.battery < 50 ? 'warn' : 'ok'}">battery ${d.battery}%</span>` : ''}</div>
      <div class="muted">${esc([d.platform, d.device].filter(Boolean).join(' · ') || 'device')} · connected ${esc(ago(d.connected_at))} · last report ${esc(ago(d.last_seen))}</div>
      <div class="muted mono">${esc(d.uid)}</div>
    </div>`).join('') : '<p class="muted">No devices connected right now.</p>';
  $('#devices-updated').textContent = 'updated ' + new Date().toLocaleTimeString();

  const pill = { valid: 'ok', expiring: 'warn', expired: 'bad', revoked: 'bad' };
  $('#certs').innerHTML = me.certs.length ? me.certs.map((c) => `<div class="account-item${c.replaced && !c.attention ? ' dim' : ''}">
      <div class="account-item-head">
        <div><strong>${esc(c.client_uid || 'device')}</strong> <span class="pill ${pill[c.state] || ''}">${esc(c.state)}</span>
          ${c.replaced ? '<span class="pill" title="This device enrolled again since">replaced</span>' : ''}
          ${c.in_use?.length ? `<span class="pill ok">connected: ${esc(c.in_use.join(', '))}</span>` : ''}</div>
        ${c.revoked ? '' : `<button type="button" class="small danger" data-revoke="${esc(c.serial)}" data-name="${esc(c.client_uid || 'this device')}">Revoke</button>`}
      </div>
      <div class="muted">Issued ${esc(new Date(c.issued).toLocaleDateString())} · expires ${esc(new Date(c.expires).toLocaleDateString())}${c.revoked ? '' : ` (${esc(daysLeft(c.days_left))})`}</div>
      ${c.attention ? `<div class="issue warning"><span class="status-icon warning" aria-hidden="true">▲</span> <span>${c.state === 'expired' ? 'Expired: this device has to enroll again.' : `Expires within ${me.warn_days} days: enroll this device again to renew it.`}</span></div>` : ''}
    </div>`).join('') : '<p class="muted">No certificates yet. Your devices get one when they enroll.</p>';
}

$('#certs').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-revoke]');
  if (!b) return;
  if (!confirm(`Revoke the certificate for ${b.dataset.name}? The device is disconnected and can't connect again until it enrolls with your password.`)) return;
  try {
    await api('POST', `certs/${encodeURIComponent(b.dataset.revoke)}/revoke`);
    toast('Certificate revoked');
    refresh();
  } catch (ex) {
    toast(ex.message, true);
  }
});

$('#password-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target;
  $('#password-error').textContent = '';
  if (f.new.value !== f.confirm.value) {
    $('#password-error').textContent = 'The new passwords do not match';
    return;
  }
  try {
    await api('POST', 'password', { current: f.current.value, new: f.new.value });
    f.reset();
    $('#qr').innerHTML = '';
    toast('Password changed. Other sign-ins were signed out.');
  } catch (ex) {
    $('#password-error').textContent = ex.message;
  }
});

// ---- two-factor login ----

async function loadTwoFactor() {
  const st = await api('GET', '2fa');
  $('#twofa-state').innerHTML = st.enabled ? '<span class="pill ok">on</span>' : '<span class="pill">off</span>';
  $('#twofa-text').innerHTML = st.enabled
    ? `Signing in here asks for a code from your authenticator app. Recovery codes left: <strong>${st.recovery_left}</strong>.` +
      (st.recovery_left < 3 ? ' <span class="error">Make new recovery codes soon.</span>' : '') +
      ' Your TAK apps keep signing in with only your password: they have no way to ask for a code.'
    : 'Optional: after your password, also ask for a 6-digit code from an authenticator app on your phone when you sign in here. Your TAK apps are not affected.';
  $('#twofa-setup').hidden = st.enabled;
  $('#twofa-recovery').hidden = !st.enabled;
  $('#twofa-disable').hidden = !st.enabled;
}

function showRecoveryCodes(codes) {
  $('#twofa-step1').hidden = true;
  $('#twofa-step2').hidden = false;
  $('#twofa-codes').textContent = codes.join('\n');
  if (!$('#twofa-dialog').open) $('#twofa-dialog').showModal();
}

$('#twofa-setup').addEventListener('click', async () => {
  try {
    const { secret, uri } = await api('POST', '2fa/setup');
    $('#twofa-step1').hidden = false;
    $('#twofa-step2').hidden = true;
    $('#twofa-error').textContent = '';
    $('#twofa-form').code.value = '';
    $('#twofa-secret').textContent = secret.match(/.{1,4}/g).join(' ');
    const box = $('#twofa-qr');
    box.innerHTML = '';
    if (window.QRCode) new QRCode(box, { text: uri, width: 200, height: 200, correctLevel: QRCode.CorrectLevel.M });
    $('#twofa-dialog').showModal();
    $('#twofa-form').code.focus();
  } catch (ex) {
    toast(ex.message, true);
  }
});

$('#twofa-form').addEventListener('submit', async (e) => {
  if (e.submitter?.value !== 'enable') return; // cancel / done close the dialog
  e.preventDefault();
  try {
    const res = await api('POST', '2fa/enable', { code: e.target.code.value });
    showRecoveryCodes(res.recovery_codes);
    loadTwoFactor();
  } catch (ex) {
    $('#twofa-error').textContent = ex.message;
  }
});

$('#twofa-copy').addEventListener('click', async () => {
  await navigator.clipboard.writeText($('#twofa-codes').textContent);
  toast('Recovery codes copied');
});

$('#twofa-download').addEventListener('click', () => {
  const name = info.server_name || 'EdgeTAK';
  const text = `${name} recovery codes for ${me.username}\nEach code signs you in once.\n\n${$('#twofa-codes').textContent}\n`;
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
  a.download = `${name}-recovery-codes.txt`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
});

// reauth asks for the password and a current code before a sensitive change.
function reauth(title, text) {
  return new Promise((resolve) => {
    const d = $('#reauth-dialog');
    const f = $('#reauth-form');
    f.reset();
    $('#reauth-title').textContent = title;
    $('#reauth-text').textContent = text;
    d.onclose = () => resolve(d.returnValue === 'ok' ? { password: f.password.value, code: f.code.value } : null);
    d.showModal();
  });
}

$('#twofa-disable').addEventListener('click', async () => {
  const creds = await reauth('Turn off two-factor login', 'Your password and a current code (or a recovery code) are needed.');
  if (!creds) return;
  try {
    await api('POST', '2fa/disable', creds);
    toast('Two-factor login turned off');
    loadTwoFactor();
  } catch (ex) {
    toast(ex.message, true);
  }
});

$('#twofa-recovery').addEventListener('click', async () => {
  const creds = await reauth('New recovery codes', 'The old recovery codes stop working. Your password and a current code are needed.');
  if (!creds) return;
  try {
    const res = await api('POST', '2fa/recovery', creds);
    showRecoveryCodes(res.recovery_codes);
    loadTwoFactor();
  } catch (ex) {
    toast(ex.message, true);
  }
});

// The QR code holds a one-time code from the server (not the password), so
// it works on servers with or without Let's Encrypt.
$('#qr-make').addEventListener('click', async () => {
  try {
    const r = await api('POST', 'qr');
    $('#qr').innerHTML = '';
    if (window.QRCode) new QRCode($('#qr'), { text: r.link, width: 200, height: 200, correctLevel: QRCode.CorrectLevel.M });
    $('#qr-info').textContent = `Works until ${new Date(r.expires).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}, for one device.`;
  } catch (ex) {
    toast(ex.message, true);
  }
});

for (const b of document.querySelectorAll('[data-pkg]')) {
  b.addEventListener('click', async () => {
    $('#pkg-error').textContent = '';
    try {
      const r = await fetch(`/account/api/package?mode=${b.dataset.pkg}`);
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

(async () => {
  try {
    info = await (await fetch('/account/api/info')).json();
    for (const el of document.querySelectorAll('.server-name')) el.textContent = info.server_name || 'EdgeTAK';
    document.title = `${info.server_name || 'EdgeTAK'} account`;
  } catch { /* keep defaults */ }
  await start();
})();
