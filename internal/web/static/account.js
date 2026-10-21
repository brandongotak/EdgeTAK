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
  $('#quick').hidden = !info.quick_connect;
  render();
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

// The quick-connect code holds the password, so it is built from what the
// user types here and never stored.
$('#qr-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const pw = e.target.password.value;
  e.target.reset();
  const host = info.host || location.hostname;
  const uri = `tak://com.atakmap.app/enroll?host=${encodeURIComponent(host)}&username=${encodeURIComponent(me.username)}&token=${encodeURIComponent(pw)}`;
  $('#qr').innerHTML = '';
  if (window.QRCode) new QRCode($('#qr'), { text: uri, width: 200, height: 200, correctLevel: QRCode.CorrectLevel.M });
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
