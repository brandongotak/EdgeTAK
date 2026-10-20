'use strict';

const $ = (sel, el = document) => el.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// ATAK team colors.
const TEAM_COLORS = {
  White: '#ffffff', Yellow: '#ffff00', Orange: '#ff7700', Magenta: '#ff00ff', Red: '#ff0000', Maroon: '#7f0000',
  Purple: '#7f007f', 'Dark Blue': '#00007f', Blue: '#0000ff', Cyan: '#00ffff', Teal: '#007f7f', Green: '#00ff00',
  'Dark Green': '#007f00', Brown: '#a0714f',
};

let status = null;
let pollTimer = null;
let map = null;
const markers = new Map();
let fittedOnce = false;

// ---------- API ----------

async function api(method, path, body) {
  const opts = { method, headers: { 'X-EdgeTAK': '1' } };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) {
    showLogin();
    throw new Error('Please sign in');
  }
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch { /* not JSON */ }
    throw new Error(msg);
  }
  if (r.status === 204) return null;
  const ct = r.headers.get('content-type') || '';
  if (ct.includes('json')) return r.json();
  return r;
}

async function download(method, path, body, fallbackName) {
  const r = await api(method, path, body);
  const blob = await r.blob();
  const cd = r.headers.get('content-disposition') || '';
  const m = cd.match(/filename="?([^";]+)"?/);
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = m ? m[1] : fallbackName;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}

function toast(msg, isError = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'toast' + (isError ? ' err' : '');
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
}

const guard = (fn) => async (...args) => {
  try { await fn(...args); } catch (e) { toast(e.message, true); }
};

// ---------- formatting ----------

function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
const date = (iso) => new Date(iso).toLocaleString();
function bytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}
function duration(sec) {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}
const groupPills = (gs) => (gs && gs.length ? gs : ['__ANON__']).map((g) => `<span class="pill">${esc(g)}</span>`).join('');
const emptyRow = (cols, text) => `<tr class="empty"><td colspan="${cols}">${esc(text)}</td></tr>`;

// ---------- auth ----------

function showLogin() {
  clearInterval(pollTimer);
  $('#shell').hidden = true;
  $('#login').hidden = false;
  $('#login-form [name=username]').focus();
}

async function start() {
  try {
    const me = await api('GET', 'api/me');
    $('#me').textContent = me.username;
  } catch {
    return;
  }
  $('#login').hidden = true;
  $('#shell').hidden = false;
  await refreshStatus();
  selectTab(location.hash.slice(1) || 'dashboard');
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  $('#login-error').textContent = '';
  const r = await fetch('api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: f.get('username'), password: f.get('password') }),
  });
  if (!r.ok) {
    $('#login-error').textContent = (await r.json().catch(() => ({}))).error || 'Sign in failed';
    return;
  }
  e.target.reset();
  start();
});

$('#logout').addEventListener('click', async () => {
  await fetch('api/logout', { method: 'POST', headers: { 'X-EdgeTAK': '1' } });
  showLogin();
});

// ---------- tabs ----------

const loaders = {
  dashboard: loadDashboard,
  users: loadUsers,
  certs: loadCerts,
  packages: loadPackages,
  missions: loadMissions,
  connect: loadConnect,
};

function selectTab(name) {
  if (!loaders[name]) name = 'dashboard';
  history.replaceState(null, '', '#' + name);
  for (const b of $('#tabs').children) b.classList.toggle('active', b.dataset.tab === name);
  for (const s of document.querySelectorAll('.tab')) s.hidden = s.id !== 'tab-' + name;
  clearInterval(pollTimer);
  guard(loaders[name])();
  if (name === 'dashboard') {
    pollTimer = setInterval(guard(loadDashboard), 3000);
    if (map) setTimeout(() => map.invalidateSize(), 0);
  }
}
$('#tabs').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-tab]');
  if (b) selectTab(b.dataset.tab);
});

// ---------- dashboard ----------

// A client has a position once it reported SA with a GPS fix (ATAK sends 0,0 without one).
const hasPos = (c) => Number.isFinite(c.lat) && Number.isFinite(c.lon) && !(c.lat === 0 && c.lon === 0);

async function refreshStatus() {
  status = await api('GET', 'api/status');
  $('#server-name').textContent = status.server_name;
  document.title = `${status.server_name} Console`;
}

function publicCertText(pc) {
  if (!pc || pc.mode === 'off') return 'off (enrollment uses the EdgeTAK CA)';
  const where = `${(pc.domains || []).join(', ')} on ${(pc.listeners || []).join(', ')}`;
  if (pc.status === 'pending') return `${pc.mode}: pending for ${where}`;
  return `${pc.mode}${pc.staging ? ' (staging)' : ''}: ${pc.issuer}, expires ${new Date(pc.not_after).toLocaleDateString()} (${where})`;
}

async function loadDashboard() {
  const [st, clients] = await Promise.all([api('GET', 'api/status'), api('GET', 'api/clients')]);
  status = st;
  const tiles = [
    ['Connected', clients.length],
    ['With position', clients.filter(hasPos).length],
    ['Users', st.users],
    ['Data packages', st.packages],
    ['Missions', st.missions],
    ['Uptime', duration(st.uptime_seconds)],
    ['Memory', `${st.memory_mb} MB`],
  ];
  $('#stats').innerHTML = tiles.map(([k, v]) => `<div class="stat"><div class="v">${esc(v)}</div><div class="k">${esc(k)}</div></div>`).join('');

  const l = st.listeners;
  const info = [
    ['Version', st.version],
    ['Public host', st.public_host],
    ['CoT TCP', l.tcp || 'disabled'],
    ['CoT TLS', l.tls || 'disabled'],
    ['HTTPS API', l.https || 'disabled'],
    ['Enrollment', l.enroll || 'disabled'],
    ['HTTP API', l.http || 'disabled'],
    ['CA', st.ca_subject],
    ['CA expires', new Date(st.ca_expires).toLocaleDateString()],
    ['Public certificate', publicCertText(st.public_cert)],
  ];
  $('#server-info').innerHTML = info.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');

  const rows = clients.map((c) => {
    const color = TEAM_COLORS[c.team] || '#888';
    const team = c.team ? `<span class="team-dot" style="background:${color}"></span>${esc(c.team)}${c.role ? ' · ' + esc(c.role) : ''}` : '—';
    const pos = hasPos(c) ? `<span class="mono">${c.lat.toFixed(5)}, ${c.lon.toFixed(5)}</span>` : '—';
    const batt = c.battery != null ? `<span class="pill ${c.battery < 20 ? 'bad' : c.battery < 50 ? 'warn' : 'ok'}">${c.battery}%</span>` : '—';
    const transport = `<span class="pill ${c.transport === 'tls' ? 'ok' : 'warn'}">${esc(c.transport.toUpperCase())}</span>`;
    return `<tr>
      <td><strong>${esc(c.callsign || '(no SA yet)')}</strong><div class="muted mono">${esc(c.uid)}</div></td>
      <td>${esc(c.username || 'anonymous')}</td>
      <td>${team}</td>
      <td>${transport}</td>
      <td><span class="pill ${c.format === 'protobuf' ? 'ok' : ''}" title="${c.format === 'protobuf' ? 'TAK protocol v1 (negotiated)' : 'CoT XML (no protocol upgrade)'}">${esc(c.format)}</span></td>
      <td class="mono">${esc(c.remote)}</td>
      <td>${pos}</td>
      <td>${batt}</td>
      <td>${esc([c.platform, c.device].filter(Boolean).join(' · ') || '—')}</td>
      <td class="mono">${c.in} / ${c.out}${c.dropped ? ` <span class="pill bad" title="dropped">${c.dropped}</span>` : ''}</td>
      <td title="${esc(date(c.connected_at))}">${ago(c.connected_at)}</td>
      <td class="actions"><button class="small danger" data-kick="${c.id}">Disconnect</button></td>
    </tr>`;
  });
  $('#clients-table tbody').innerHTML = rows.join('') || emptyRow(12, 'No clients connected');
  $('#clients-updated').textContent = 'updated ' + new Date().toLocaleTimeString();
  updateMap(clients);
}

$('#clients-table').addEventListener('click', guard(async (e) => {
  const id = e.target.dataset.kick;
  if (!id) return;
  await api('POST', `api/clients/${id}/disconnect`);
  toast('Client disconnected');
  loadDashboard();
}));

function updateMap(clients) {
  if (!window.L) return;
  const withPos = clients.filter(hasPos);
  if (!map) {
    if (!withPos.length) return;
    const el = $('#map');
    el.innerHTML = '';
    map = L.map(el, { worldCopyJump: true }).setView([withPos[0].lat, withPos[0].lon], 13);
    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: 19, attribution: '&copy; OpenStreetMap contributors',
    }).addTo(map);
  }
  const seen = new Set();
  for (const c of withPos) {
    const key = c.uid || String(c.id);
    seen.add(key);
    const color = TEAM_COLORS[c.team] || '#4cc38a';
    const label = `<strong>${esc(c.callsign)}</strong><br>${esc(c.team || '')} ${esc(c.role || '')}<br>` +
      `${c.lat.toFixed(5)}, ${c.lon.toFixed(5)}${c.speed ? `<br>${(c.speed * 3.6).toFixed(1)} km/h` : ''}`;
    let m = markers.get(key);
    if (!m) {
      m = L.circleMarker([c.lat, c.lon], { radius: 8, weight: 2, color: '#000', fillOpacity: 0.9 }).addTo(map);
      m.bindTooltip(esc(c.callsign), { permanent: true, direction: 'right', offset: [8, 0] });
      markers.set(key, m);
    }
    m.setLatLng([c.lat, c.lon]).setStyle({ fillColor: color });
    m.bindPopup(label);
  }
  for (const [key, m] of markers) {
    if (!seen.has(key)) { m.remove(); markers.delete(key); }
  }
  if (!fittedOnce && withPos.length > 1) {
    map.fitBounds(L.latLngBounds(withPos.map((c) => [c.lat, c.lon])).pad(0.3));
    fittedOnce = true;
  }
}

// ---------- users ----------

const splitGroups = (s) => String(s || '').split(',').map((g) => g.trim()).filter(Boolean);

async function loadUsers() {
  const users = await api('GET', 'api/users');
  $('#users-table tbody').innerHTML = users.map((u) => `<tr>
    <td><strong>${esc(u.username)}</strong></td>
    <td>${groupPills(u.groups)}</td>
    <td>${u.admin ? '<span class="pill ok">admin</span>' : 'user'}</td>
    <td>${u.online ? `<span class="pill ok">${u.online} online</span>` : '<span class="muted">offline</span>'}</td>
    <td>${esc(date(u.created))}</td>
    <td class="actions">
      <button class="small" data-act="groups" data-user="${esc(u.username)}" data-groups="${esc((u.groups || []).join(', '))}">Groups</button>
      <button class="small" data-act="password" data-user="${esc(u.username)}">Password</button>
      <button class="small" data-act="admin" data-user="${esc(u.username)}" data-admin="${u.admin}">${u.admin ? 'Revoke admin' : 'Make admin'}</button>
      <button class="small" data-act="p12" data-user="${esc(u.username)}">Issue .p12</button>
      <button class="small danger" data-act="delete" data-user="${esc(u.username)}">Delete</button>
    </td></tr>`).join('') || emptyRow(6, 'No users');
}

$('#user-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  await api('POST', 'api/users', {
    username: f.get('username'), password: f.get('password'), groups: splitGroups(f.get('groups')), admin: f.get('admin') === 'on',
  });
  e.target.reset();
  toast('User created');
  loadUsers();
}));

$('#users-table').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  const user = b.dataset.user;
  const path = `api/users/${encodeURIComponent(user)}`;
  switch (b.dataset.act) {
    case 'groups': {
      const g = prompt(`Groups for ${user} (comma separated, blank = __ANON__). Users must reconnect for changes to apply.`, b.dataset.groups);
      if (g === null) return;
      await api('PATCH', path, { groups: splitGroups(g) });
      toast('Groups updated');
      break;
    }
    case 'password': {
      const pw = prompt(`New password for ${user} (8+ characters)`);
      if (!pw) return;
      await api('PATCH', path, { password: pw });
      toast('Password changed');
      break;
    }
    case 'admin':
      await api('PATCH', path, { admin: b.dataset.admin !== 'true' });
      break;
    case 'p12': {
      const pw = prompt(`Password to protect ${user}.p12 (blank = server trust store password)`, '');
      if (pw === null) return;
      await download('POST', `${path}/certificate`, { password: pw }, `${user}.p12`);
      toast('Certificate issued');
      return;
    }
    case 'delete':
      if (!confirm(`Delete ${user}? Their certificates will be revoked and live connections dropped.`)) return;
      await api('DELETE', path);
      toast('User deleted');
      break;
  }
  loadUsers();
}));

// ---------- certificates ----------

async function loadCerts() {
  const certs = await api('GET', 'api/certs');
  const now = Date.now();
  $('#certs-table tbody').innerHTML = certs.map((c) => {
    const expired = new Date(c.expires).getTime() < now;
    const state = c.revoked ? '<span class="pill bad">revoked</span>' : expired ? '<span class="pill warn">expired</span>' : '<span class="pill ok">valid</span>';
    return `<tr>
      <td><strong>${esc(c.common_name)}</strong></td>
      <td class="mono">${esc(c.client_uid || '—')}</td>
      <td class="mono" title="${esc(c.serial)}">${esc(c.serial.slice(0, 12))}…</td>
      <td>${esc(date(c.issued))}</td>
      <td>${esc(date(c.expires))}</td>
      <td>${state}</td>
      <td class="actions">${c.revoked ? '' : `<button class="small danger" data-revoke="${esc(c.serial)}">Revoke</button>`}</td>
    </tr>`;
  }).join('') || emptyRow(7, 'No certificates issued yet');
}

$('#certs-table').addEventListener('click', guard(async (e) => {
  const serial = e.target.dataset.revoke;
  if (!serial || !confirm('Revoke this certificate? Devices using it are disconnected and must re-enroll.')) return;
  await api('POST', `api/certs/${serial}/revoke`);
  toast('Certificate revoked');
  loadCerts();
}));

// ---------- packages ----------

async function loadPackages() {
  const pkgs = await api('GET', 'api/packages');
  $('#packages-table tbody').innerHTML = pkgs.map((p) => `<tr>
    <td><strong>${esc(p.name)}</strong><div class="muted mono" title="${esc(p.hash)}">${esc(p.hash.slice(0, 16))}…</div></td>
    <td>${bytes(p.size)}</td>
    <td>${esc(p.submitter)}${p.creator_uid ? `<div class="muted mono">${esc(p.creator_uid)}</div>` : ''}</td>
    <td>${groupPills(p.groups)}</td>
    <td>${p.tool === 'public' ? '<span class="pill ok">public</span>' : `<span class="pill">${esc(p.tool || 'private')}</span>`}</td>
    <td>${esc(date(p.submitted))}</td>
    <td class="actions">
      <a class="button small" href="/Marti/sync/content?hash=${esc(p.hash)}">Download</a>
      <button class="small danger" data-del="${esc(p.hash)}" data-name="${esc(p.name)}">Delete</button>
    </td></tr>`).join('') || emptyRow(7, 'No data packages');
}

$('#package-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const file = e.target.assetfile.files[0];
  if (!file) return;
  const fd = new FormData();
  fd.append('assetfile', file, file.name);
  await api('POST', `/Marti/sync/missionupload?filename=${encodeURIComponent(file.name)}&creatorUid=admin-console`, fd);
  e.target.reset();
  toast('Package uploaded');
  loadPackages();
}));

$('#packages-table').addEventListener('click', guard(async (e) => {
  const hash = e.target.dataset.del;
  if (!hash || !confirm(`Delete ${e.target.dataset.name}?`)) return;
  await api('DELETE', `api/packages/${hash}`);
  toast('Package deleted');
  loadPackages();
}));

// ---------- missions ----------

const ROLE_LABELS = { MISSION_SUBSCRIBER: 'subscribers can edit', MISSION_READONLY_SUBSCRIBER: 'read-only', MISSION_OWNER: 'everyone owner' };

async function loadMissions() {
  const missions = await api('GET', 'api/missions');
  $('#missions-table tbody').innerHTML = missions.map((m) => {
    const access = [
      m.password_protected ? '<span class="pill warn">password</span>' : '',
      m.invite_only ? '<span class="pill warn">invite only</span>' : '',
      `<span class="pill">${esc(ROLE_LABELS[m.default_role] || m.default_role)}</span>`,
    ].join('');
    return `<tr>
      <td><strong>${esc(m.name)}</strong>${m.description ? `<div class="muted">${esc(m.description)}</div>` : ''}</td>
      <td>${esc(m.creator || '—')}<div class="muted mono">${esc(m.creator_uid)}</div></td>
      <td>${groupPills(m.groups)}</td>
      <td>${access}</td>
      <td>${m.items}</td>
      <td>${m.files}</td>
      <td title="${esc(m.subscribers.join(', '))}">${m.subscribers.length}</td>
      <td>${m.logs}</td>
      <td title="${esc(date(m.last_edited))}">${ago(m.last_edited)}</td>
      <td class="actions">
        <a class="button small" href="/Marti/api/missions/${encodeURIComponent(m.name)}/archive">Export</a>
        <button class="small danger" data-del-mission="${esc(m.name)}">Delete</button>
      </td></tr>`;
  }).join('') || emptyRow(10, 'No missions yet. Create one from Data Sync in ATAK.');
}

$('#missions-table').addEventListener('click', guard(async (e) => {
  const name = e.target.dataset.delMission;
  if (!name || !confirm(`Delete mission "${name}"? Subscribers are notified and its change history is lost. Files stay on the Data Packages tab.`)) return;
  await api('DELETE', `api/missions/${encodeURIComponent(name)}`);
  toast('Mission deleted');
  loadMissions();
}));

// ---------- connect ----------

async function loadConnect() {
  if (!$('#connect-host').value && status) $('#connect-host').value = status.public_host || location.hostname;
}

$('#connect-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const q = new URLSearchParams(new FormData(e.target));
  await download('GET', `api/connection-package?${q}`, undefined, 'edgetak.zip');
}));

$('#qr-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const host = $('#connect-host').value || status?.public_host || location.hostname;
  const uri = `tak://com.atakmap.app/enroll?host=${encodeURIComponent(host)}&username=${encodeURIComponent(f.get('username'))}&token=${encodeURIComponent(f.get('password'))}`;
  const box = $('#qr');
  box.innerHTML = '';
  if (window.QRCode) {
    new QRCode(box, { text: uri, width: 220, height: 220, correctLevel: QRCode.CorrectLevel.M });
  } else {
    toast('QR library unavailable offline; use the link below', true);
  }
  $('#qr-uri').textContent = uri;
});

start();
