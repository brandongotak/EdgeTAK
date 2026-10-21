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

// Signing in happens on the shared page at /, which brings admins back here.
function showLogin() {
  clearInterval(pollTimer);
  clearInterval(mapTimer);
  location.replace('/?next=' + encodeURIComponent('/admin/' + location.hash));
}

async function start() {
  try {
    const me = await api('GET', 'api/me');
    $('#me').textContent = me.username;
  } catch {
    return;
  }
  $('#shell').hidden = false;
  await refreshStatus();
  selectTab(location.hash.slice(1) || 'dashboard');
  // The map is open by default beside the tabs; on phones it opens on demand.
  setMapVisible(!isNarrow() && prefs.get('map.open', true), false);
}

$('#logout').addEventListener('click', async () => {
  await fetch('api/logout', { method: 'POST', headers: { 'X-EdgeTAK': '1' } });
  location.replace('/');
});

// ---------- tabs ----------

const loaders = {
  dashboard: loadDashboard,
  health: loadHealth,
  users: loadUsers,
  groups: loadGroups,
  signups: loadSignups,
  certs: loadCerts,
  packages: loadPackages,
  missions: loadMissions,
  video: loadVideo,
  send: loadSend,
  connect: loadConnect,
  chat: loadChat,
  settings: () => loadRetention(),
};

let currentTab = '';

// The tab bar scrolls when the window is narrow; keep the selected tab in
// view, also when the bar itself narrows (the Install app button appears).
function showActiveTab() {
  const t = $('#tabs'), b = t.querySelector('button.active');
  if (!b) return;
  const bar = t.getBoundingClientRect(), r = b.getBoundingClientRect();
  if (r.left < bar.left) t.scrollLeft -= bar.left - r.left;
  else if (r.right > bar.right) t.scrollLeft += r.right - bar.right;
}
new ResizeObserver(showActiveTab).observe($('#tabs'));

function selectTab(name) {
  if (!loaders[name]) name = 'dashboard';
  currentTab = name;
  history.replaceState(null, '', '#' + name);
  for (const b of $('#tabs').children) b.classList.toggle('active', b.dataset.tab === name);
  showActiveTab();
  for (const s of document.querySelectorAll('.tab')) s.hidden = s.id !== 'tab-' + name;
  clearInterval(pollTimer);
  guard(loaders[name])();
  if (name === 'dashboard') {
    loadSelfUpdate().catch(() => {});
    pollTimer = setInterval(guard(loadDashboard), 3000);
  } else if (name === 'health') {
    pollTimer = setInterval(() => loadHealth().catch(() => {}), 10000);
  } else if (name === 'video') {
    pollTimer = setInterval(() => loadVideo().catch(() => {}), 5000); // live streams come and go
  } else if (name === 'send') {
    pollTimer = setInterval(guard(loadSendLive), 5000); // Inbox updates
  } else if (name === 'chat') {
    pollTimer = setInterval(() => loadChat().catch(() => {}), 5000); // new messages
  }
}
$('#tabs').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-tab]');
  if (!b) return;
  if (isNarrow() && mapVisible()) setMapVisible(false); // the map covers the tabs on phones
  selectTab(b.dataset.tab);
});

// ---------- dashboard ----------

// A client has a position once it reported SA with a GPS fix (ATAK sends 0,0 without one).
const hasPos = (c) => Number.isFinite(c.lat) && Number.isFinite(c.lon) && !(c.lat === 0 && c.lon === 0);

async function refreshStatus() {
  status = await api('GET', 'api/status');
  $('#server-name').textContent = status.server_name;
  $('#server-version').textContent = status.version;
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
  if (Date.now() - certCheckedAt > 60000) dashCertIssues().catch(() => {}); // the dashboard polls every 3 s; certificates change slowly

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
      <td><strong${hasPos(c) ? ` class="callsign-link" data-focus="${esc(c.uid || String(c.id))}" title="Show on the map"` : ''}>${esc(c.callsign || '(no SA yet)')}</strong><div class="muted mono">${esc(c.uid)}</div></td>
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

// ---------- health ----------

let healthMinutes = null; // read from prefs on first load (prefs is defined further down)

function fmtSize(n) {
  if (n == null || n < 0) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${u[i]}`;
}
function fmtBits(bytesPerSec) {
  if (bytesPerSec == null || bytesPerSec < 0) return '—';
  let v = bytesPerSec * 8;
  const u = ['bit/s', 'kbit/s', 'Mbit/s', 'Gbit/s'];
  let i = 0;
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}
// Whole numbers stay whole; small fractions keep one decimal.
const fmtNum = (v) => (Number.isInteger(v) ? String(v) : v < 10 ? v.toFixed(1) : String(Math.round(v)));
const fmtPct = (v) => (v == null || v < 0 ? '—' : `${fmtNum(v)}%`);
const fmtRate = (v) => (v == null ? '—' : `${fmtNum(v)}/s`);
const fmtTime = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const known = (v) => (v == null || v < 0 ? null : v);

function niceMax(v) {
  if (!(v > 0)) return 1;
  const e = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * e >= v) return m * e;
  return 10 * e;
}

const svgNS = 'http://www.w3.org/2000/svg';
function svgEl(tag, attrs, parent) {
  const el = document.createElementNS(svgNS, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  if (parent) parent.appendChild(el);
  return el;
}

// sparkline: a muted trend line with the latest value marked.
function sparkline(values, { w = 120, h = 28 } = {}) {
  const pts = values.map((v, i) => [i, v]).filter(([, v]) => v != null);
  const svg = svgEl('svg', { viewBox: `0 0 ${w} ${h}`, class: 'spark', 'aria-hidden': 'true', preserveAspectRatio: 'none' });
  if (pts.length < 2) return svg;
  const max = Math.max(...pts.map(([, v]) => v)), min = Math.min(...pts.map(([, v]) => v));
  const span = max - min || 1;
  const x = (i) => (i / (values.length - 1)) * (w - 6) + 3;
  const y = (v) => h - 4 - ((v - min) / span) * (h - 8);
  svgEl('polyline', { points: pts.map(([i, v]) => `${x(i)},${y(v)}`).join(' '), fill: 'none', stroke: 'var(--muted)', 'stroke-width': 1.5,
    'stroke-linejoin': 'round', 'stroke-linecap': 'round', 'vector-effect': 'non-scaling-stroke' }, svg);
  const [li, lv] = pts[pts.length - 1];
  svgEl('circle', { cx: x(li), cy: y(lv), r: 3, fill: 'var(--series-1)' }, svg);
  return svg;
}

// lineChart draws series over time with one y axis, a crosshair and tooltip.
function lineChart(box, { times, series, max, fmt }) {
  box.innerHTML = '';
  const have = series.some((s) => s.values.some((v) => v != null));
  if (!have || times.length < 2) {
    box.innerHTML = '<div class="empty">No data yet</div>';
    return;
  }
  const W = Math.max(280, box.clientWidth || 560), H = 170, L = 52, R = 12, T = 10, B = 22;
  const allMax = Math.max(...series.flatMap((s) => s.values.filter((v) => v != null)));
  const yMax = max ?? niceMax(allMax * 1.1);
  const t0 = times[0], t1 = times[times.length - 1];
  const x = (t) => L + ((t - t0) / (t1 - t0 || 1)) * (W - L - R);
  const y = (v) => T + (1 - Math.min(v, yMax) / yMax) * (H - T - B);
  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img' });
  const grid = svgEl('g', { class: 'grid' }, svg);
  const axis = svgEl('g', { class: 'axis' }, svg);
  for (const f of [0, 0.5, 1]) { // recessive: baseline, middle, top
    const gy = y(yMax * f);
    svgEl('line', { x1: L, x2: W - R, y1: gy, y2: gy }, grid);
    svgEl('text', { x: L - 6, y: gy + 4, 'text-anchor': 'end' }, axis).textContent = fmt(yMax * f);
  }
  for (const [f, anchor] of [[0, 'start'], [0.5, 'middle'], [1, 'end']]) {
    const t = t0 + (t1 - t0) * f;
    svgEl('text', { x: x(t), y: H - 6, 'text-anchor': anchor }, axis).textContent = fmtTime(t);
  }
  for (const s of series) {
    let d = '', open = false, area = '', last = null;
    s.values.forEach((v, i) => {
      if (v == null) { open = false; return; }
      d += `${open ? 'L' : 'M'}${x(times[i]).toFixed(1)},${y(v).toFixed(1)}`;
      open = true;
      last = i;
    });
    if (series.length === 1) { // a single series gets a light wash under its line
      const idx = s.values.map((v, i) => (v == null ? null : i)).filter((i) => i != null);
      area = `M${x(times[idx[0]])},${y(0)}` + idx.map((i) => `L${x(times[i])},${y(s.values[i])}`).join('') + `L${x(times[idx[idx.length - 1]])},${y(0)}Z`;
      svgEl('path', { d: area, fill: s.color, opacity: 0.1 }, svg);
    }
    svgEl('path', { d, fill: 'none', stroke: s.color, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }, svg);
    if (last != null) svgEl('circle', { cx: x(times[last]), cy: y(s.values[last]), r: 4, fill: s.color, stroke: 'var(--panel)', 'stroke-width': 2 }, svg);
  }
  // Hover / keyboard layer.
  const cross = svgEl('line', { class: 'crosshair', y1: T, y2: H - B, visibility: 'hidden' }, svg);
  const dots = series.map((s) => svgEl('circle', { r: 4, fill: s.color, stroke: 'var(--panel)', 'stroke-width': 2, visibility: 'hidden' }, svg));
  const hit = svgEl('rect', { x: L, y: T, width: W - L - R, height: H - T - B, fill: 'transparent', tabindex: 0, 'aria-label': 'Chart: use the arrow keys to read values' }, svg);
  box.appendChild(svg);
  const tip = document.createElement('div');
  tip.className = 'tip';
  tip.hidden = true;
  box.appendChild(tip);
  let cur = times.length - 1;
  const show = (i) => {
    cur = Math.max(0, Math.min(times.length - 1, i));
    const cx = x(times[cur]);
    cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.setAttribute('visibility', 'visible');
    tip.replaceChildren();
    const head = document.createElement('div');
    head.className = 't';
    head.textContent = new Date(times[cur]).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
    tip.appendChild(head);
    series.forEach((s, k) => {
      const v = s.values[cur];
      if (v == null) { dots[k].setAttribute('visibility', 'hidden'); } else {
        dots[k].setAttribute('cx', cx); dots[k].setAttribute('cy', y(v)); dots[k].setAttribute('visibility', 'visible');
      }
      const row = document.createElement('div');
      row.className = 'row';
      row.style.setProperty('--key', s.color);
      const strong = document.createElement('strong');
      strong.textContent = v == null ? '—' : fmt(v);
      row.append(strong, document.createTextNode(series.length > 1 ? ` ${s.name}` : ''));
      tip.appendChild(row);
    });
    tip.hidden = false;
    const px = (cx / W) * box.clientWidth;
    tip.style.left = `${Math.min(Math.max(px + 12, 0), box.clientWidth - tip.offsetWidth)}px`;
    tip.style.top = '4px';
    if (px + 12 + tip.offsetWidth > box.clientWidth) tip.style.left = `${Math.max(0, px - tip.offsetWidth - 12)}px`;
  };
  const hide = () => { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); dots.forEach((d) => d.setAttribute('visibility', 'hidden')); };
  hit.addEventListener('pointermove', (e) => {
    const r = svg.getBoundingClientRect();
    const t = t0 + (((e.clientX - r.left) / r.width) * W - L) / (W - L - R) * (t1 - t0);
    let best = 0;
    for (let i = 1; i < times.length; i++) if (Math.abs(times[i] - t) < Math.abs(times[best] - t)) best = i;
    show(best);
  });
  hit.addEventListener('pointerleave', hide);
  hit.addEventListener('focus', () => show(cur));
  hit.addEventListener('blur', hide);
  hit.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowLeft') { show(cur - 1); e.preventDefault(); }
    if (e.key === 'ArrowRight') { show(cur + 1); e.preventDefault(); }
  });
}

function chartCard(title, legend) {
  const card = document.createElement('div');
  card.className = 'card chart-card';
  const h = document.createElement('h2');
  h.textContent = title;
  card.appendChild(h);
  if (legend.length > 1) {
    const lg = document.createElement('div');
    lg.className = 'legend';
    for (const [name, color] of legend) {
      const s = document.createElement('span');
      s.style.setProperty('--key', color);
      s.textContent = name;
      lg.appendChild(s);
    }
    card.appendChild(lg);
  }
  const box = document.createElement('div');
  box.className = 'chart';
  card.appendChild(box);
  return [card, box];
}

const STATUS = {
  critical: ['✖', 'Critical'], warning: ['▲', 'Warning'], info: ['●', 'Note'], ok: ['✓', 'OK'],
};
const statusBadge = (level) => `<span class="status-icon ${level}" aria-hidden="true">${STATUS[level][0]}</span> ${STATUS[level][1]}`;

async function loadHealth() {
  if (healthMinutes == null) {
    healthMinutes = Number(prefs.get('health.minutes', 60)) || 60;
    for (const o of document.querySelectorAll('.segmented button[data-minutes]')) o.classList.toggle('active', Number(o.dataset.minutes) === healthMinutes);
  }
  const [h, devices] = await Promise.all([api('GET', `api/health?minutes=${healthMinutes}`), api('GET', 'api/health/devices')]);
  const S = h.samples;
  const last = h.latest || S[S.length - 1] || {};
  const times = S.map((s) => s.t);
  const col = (k) => S.map((s) => known(s[k]));
  $('#health-updated').textContent = `Updated ${new Date().toLocaleTimeString()} · samples every ${h.interval}s`;

  $('#health-issues').innerHTML = h.issues.map((i) => `<div class="issue ${i.level}">${statusBadge(i.level)} <span>${esc(i.text)}</span></div>`).join('');

  const memPct = last.mem_total ? (100 * last.mem_used) / last.mem_total : -1;
  const diskPct = last.disk_total ? (100 * last.disk_used) / last.disk_total : -1;
  const tiles = [
    ['CPU', fmtPct(last.cpu), `EdgeTAK ${fmtPct(last.proc_cpu)}${h.info.cpus ? ` · ${h.info.cpus} cores` : ''}`, col('cpu')],
    ['Memory', last.mem_total ? fmtPct(memPct) : fmtSize(last.proc_rss), last.mem_total ? `${fmtSize(last.mem_used)} of ${fmtSize(last.mem_total)} · EdgeTAK ${fmtSize(last.proc_rss)}` : 'EdgeTAK only', col('mem_used')],
    ['Disk', fmtPct(diskPct), last.disk_total ? `${fmtSize(last.disk_total - last.disk_used)} free` : '', null],
    ...(known(last.temp_c) != null ? [['Temperature', `${Math.round(last.temp_c)} °C`, '', col('temp_c')]] : []),
    ['Network in', fmtBits(last.net_rx), `Out ${fmtBits(last.net_tx)}`, col('net_rx')],
    ['Devices', String(last.clients ?? 0), `${fmtRate(last.cot_in)} messages in`, col('clients')],
  ];
  if (S.some((s) => s.video_in > 0 || s.video_out > 0)) tiles.push(['Video in', fmtBits(last.video_in), `Out ${fmtBits(last.video_out)}`, col('video_in')]);
  $('#health-tiles').replaceChildren(...tiles.map(([k, v, sub, spark]) => {
    const t = document.createElement('div');
    t.className = 'stat';
    t.innerHTML = `<div class="k">${esc(k)}</div><div class="v">${esc(v)}</div><div class="sub">${esc(sub)}</div>`;
    if (spark) t.appendChild(sparkline(spark));
    return t;
  }));

  const c1 = 'var(--series-1)', c2 = 'var(--series-2)';
  const charts = [
    ['CPU', [['System', c1, col('cpu')], ['EdgeTAK', c2, col('proc_cpu')]], 100, fmtPct],
    ['Memory used', [['System', c1, col('mem_used')], ['EdgeTAK', c2, col('proc_rss')]], last.mem_total || null, fmtSize],
    ['Network', [['In', c1, col('net_rx')], ['Out', c2, col('net_tx')]], null, fmtBits],
    ['Connected devices', [['Devices', c1, col('clients')]], null, fmtNum],
    ['CoT messages', [['In', c1, col('cot_in')], ['Out', c2, col('cot_out')]], null, fmtRate],
  ];
  if (S.some((s) => s.video_in > 0 || s.video_out > 0)) charts.push(['Video', [['In', c1, col('video_in')], ['Out', c2, col('video_out')]], null, fmtBits]);
  const grid = $('#health-charts');
  grid.replaceChildren();
  for (const [title, ser, max, fmt] of charts) {
    const shown = ser.filter(([, , v]) => v.some((x) => x != null));
    const [card, box] = chartCard(title, (shown.length ? shown : ser).map(([n, c]) => [n, c]));
    grid.appendChild(card);
    lineChart(box, { times, series: (shown.length ? shown : ser).map(([name, color, values]) => ({ name, color, values })), max, fmt });
  }

  // Table view of the same data (about 24 rows).
  const step = Math.max(1, Math.ceil(S.length / 24));
  const rows = S.filter((_, i) => i % step === 0 || i === S.length - 1).reverse();
  $('#health-table').innerHTML = '<thead><tr><th>Time</th><th>CPU</th><th>EdgeTAK CPU</th><th>Memory</th><th>Network in</th><th>Network out</th><th>Devices</th><th>CoT in</th><th>CoT out</th><th>Temp</th></tr></thead><tbody>' +
    rows.map((s) => `<tr><td>${esc(new Date(s.t).toLocaleTimeString())}</td><td>${fmtPct(s.cpu)}</td><td>${fmtPct(s.proc_cpu)}</td><td>${fmtSize(s.mem_used || s.proc_rss)}</td>` +
      `<td>${fmtBits(s.net_rx)}</td><td>${fmtBits(s.net_tx)}</td><td>${s.clients}</td><td>${fmtRate(s.cot_in)}</td><td>${fmtRate(s.cot_out)}</td>` +
      `<td>${known(s.temp_c) == null ? '—' : Math.round(s.temp_c) + ' °C'}</td></tr>`).join('') + '</tbody>';

  // Devices, worst first.
  const counts = { critical: 0, warning: 0, info: 0, ok: 0 };
  devices.forEach((d) => { counts[d.worst]++; });
  $('#device-summary').textContent = devices.length
    ? [['critical', 'critical'], ['warning', 'warning'], ['ok', 'OK']].filter(([k]) => counts[k] || k === 'ok').map(([k, l]) => `${counts[k] + (k === 'ok' ? counts.info : 0)} ${l}`).join(' · ')
    : '';
  $('#device-health-table tbody').innerHTML = devices.map((d) => {
    const batt = d.battery != null
      ? `<div class="batt"><strong>${d.battery}%</strong><span data-spark="${esc(d.uid)}"></span></div>${d.drain_per_hour > 0 ? `<div class="muted">−${d.drain_per_hour}%/h</div>` : ''}`
      : '<span class="muted">—</span>';
    const pos = d.lat != null && !(d.lat === 0 && d.lon === 0)
      ? `<span class="mono">${d.lat.toFixed(4)}, ${d.lon.toFixed(4)}</span>${d.geo_source ? `<div class="muted">${esc(d.geo_source)}</div>` : ''}` : '<span class="muted">none</span>';
    // Each cell's content sits in one block so it stacks in the phone card layout.
    const cell = (html, attrs = '') => `<td${attrs}><div>${html}</div></td>`;
    return `<tr>
      ${cell(statusBadge(d.worst))}
      ${cell(`<strong>${esc(d.callsign || '(no position yet)')}</strong><div class="muted">${esc(d.username || 'anonymous')}${d.team ? ' · ' + esc(d.team) : ''}</div>
        <div class="muted">${esc([d.platform, d.version, d.device, d.os && 'Android ' + d.os].filter(Boolean).join(' · '))}</div>`)}
      ${cell(batt)}
      ${cell(esc(d.last_seen_ago < 5 ? 'just now' : ago(d.last_seen)), ` title="${esc(date(d.last_seen))}"`)}
      ${cell(pos)}
      ${cell(`<span class="pill ${d.transport === 'tls' ? 'ok' : 'warn'}">${esc(d.transport.toUpperCase())}</span> ${esc(d.format)}<div class="muted mono">${esc(d.remote)}</div><div class="muted">connected ${esc(ago(d.connected_at))}</div>`)}
      ${cell(`<span class="mono">${d.in} / ${d.out}</span>${d.dropped ? `<div class="muted">${d.dropped} dropped</div>` : ''}`)}
      ${cell(d.issues.length ? `<ul>${d.issues.map((i) => `<li><span class="status-icon ${i.level}" aria-hidden="true">${STATUS[i.level][0]}</span> ${esc(i.text)}</li>`).join('')}</ul>` : '<span class="muted">None</span>')}
    </tr>`;
  }).join('') || emptyRow(8, 'No devices connected');
  for (const d of devices) {
    const slot = document.querySelector(`[data-spark="${CSS.escape(d.uid)}"]`);
    if (slot && d.battery_history?.length > 1) slot.appendChild(sparkline(d.battery_history.map((p) => p.pct), { w: 70, h: 20 }));
  }

  const i = h.info;
  $('#health-machine').textContent = `${i.model || i.hostname} · ${i.os}/${i.arch} · ${i.cpus} CPU cores` +
    (i.uptime ? ` · system up ${duration(i.uptime)}` : '') + ` · EdgeTAK ${h.version} up ${duration(h.edgetak_uptime)}`;
}

for (const b of document.querySelectorAll('.segmented button[data-minutes]')) {
  b.addEventListener('click', guard(async () => {
    healthMinutes = Number(b.dataset.minutes);
    prefs.set('health.minutes', healthMinutes);
    for (const o of document.querySelectorAll('.segmented button[data-minutes]')) o.classList.toggle('active', o === b);
    await loadHealth();
  }));
}

// ---------- data retention ----------

const RETENTION_RULES = [
  ['packages_days', 'packages', 'Data packages', 'by upload date; files a mission uses are kept', 'package'],
  ['missions_idle_days', 'missions', 'Missions', 'with no changes for this long', 'mission'],
  ['inbox_days', 'inbox', 'Inbox messages', 'sent to the server contact', 'message'],
  ['requests_days', 'requests', 'Pending sign-up requests', 'rejected when they have waited this long', 'request'],
  ['invites_days', 'invites', 'Old invites', 'used up, expired or revoked', 'invite'],
  ['certs_days', 'certs', 'Expired certificate records', 'after the certificate expired; valid ones are always kept', 'record'],
  ['chat_days', 'chat', 'Chat log', 'messages recorded on the Chat log tab', 'message'],
];
let retentionTimer = null;

function plural(n, word) { return `${n} ${word}${Number(n) === 1 ? '' : 's'}`; }

function renderRetentionEffects(pending) {
  for (const [key, kind, , , noun] of RETENTION_RULES) {
    const cell = $(`#retention-rows [data-effect="${key}"]`);
    const days = Number($(`#retention-form [name="${key}"]`).value) || 0;
    const n = pending?.removed?.[kind];
    cell.innerHTML = days === 0 ? '<span class="muted">kept forever</span>'
      : n ? `<span class="status-icon warning" aria-hidden="true">▲</span> ${esc(plural(n, noun))} would be removed now` : '<span class="muted">nothing to remove now</span>';
  }
}

async function loadRetention() {
  const r = await api('GET', 'api/retention');
  const rows = $('#retention-rows');
  rows.innerHTML = '<div class="head">Data</div><div class="head">Keep (days)</div><div class="head">Effect</div>' +
    RETENTION_RULES.map(([key, , label, hint]) => `<div class="label"><strong>${esc(label)}</strong><span>${esc(hint)}</span></div>
      <input type="number" min="0" max="3650" step="1" name="${key}" value="${Number(r.settings[key]) || 0}" aria-label="${esc(label)}: days to keep">
      <div class="effect" data-effect="${key}"></div>`).join('') +
    `<div class="label"><strong>Position history</strong><span>set with the map's History button</span></div>
     <div>${esc(r.history.enabled ? plural(fmtNum(r.history.retention_hours / 24), 'day') : 'off')}</div>
     <div class="effect muted">${r.history.enabled ? `kept ${esc(r.history.retention_hours)} hours` : 'not recorded'}</div>` +
    (r.video ? `<div class="label"><strong>Video recordings</strong><span>set on the Video tab</span></div>
     <div>${esc(r.recording.mode === 'off' ? 'off' : r.recording.keep_days ? plural(r.recording.keep_days, 'day') : 'forever')}</div>
     <div class="effect muted">${r.recording.mode === 'off' ? 'not recorded' : 'deleted by the media server'}</div>` : '');
  renderRetentionEffects(r.pending);
  const last = r.last;
  $('#retention-last').textContent = last
    ? `Last run ${new Date(last.at).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}: ` +
      (Object.values(last.removed).some((n) => n) ? Object.entries(last.removed).filter(([, n]) => n).map(([k, n]) => `${n} ${k}`).join(', ') + ' removed' : 'nothing removed')
    : '';
}

function retentionForm() {
  return Object.fromEntries(RETENTION_RULES.map(([key]) => [key, Number($(`#retention-form [name="${key}"]`).value) || 0]));
}

// Preview as the admin types: what these numbers would remove now.
$('#retention-form').addEventListener('input', () => {
  clearTimeout(retentionTimer);
  retentionTimer = setTimeout(guard(async () => {
    renderRetentionEffects(await api('POST', 'api/retention/preview', retentionForm()));
  }), 400);
});

$('#retention-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const set = retentionForm();
  const preview = await api('POST', 'api/retention/preview', set);
  const total = Object.values(preview.removed).reduce((a, b) => a + b, 0);
  if (total && !confirm(`Save these rules? ${total} item(s) will be removed at the next hourly run (or press Run now). This cannot be undone.`)) return;
  await api('PUT', 'api/retention', set);
  toast('Retention rules saved');
  loadRetention();
}));

$('#retention-run').addEventListener('click', guard(async () => {
  const r = await api('GET', 'api/retention');
  const total = Object.values(r.pending.removed).reduce((a, b) => a + b, 0);
  if (!total) { toast('Nothing to remove with the saved rules'); return; }
  if (!confirm(`Remove ${total} item(s) now with the saved rules? This cannot be undone.`)) return;
  const res = await api('POST', 'api/retention/run');
  toast(res.errors?.length ? `Done with errors: ${res.errors.join('; ')}` : `Removed ${Object.values(res.removed).reduce((a, b) => a + b, 0)} item(s)`, !!res.errors?.length);
  loadRetention();
}));

// ---------- EdgeTAK updates ----------

let selfUpd = null;
let selfUpdChecked = false;
let selfUpdWatch = null;

async function loadSelfUpdate() {
  selfUpd = await api('GET', 'api/server/update');
  if (!selfUpdChecked && selfUpd.available && !selfUpd.latest) { // look once per visit
    selfUpdChecked = true;
    api('POST', 'api/server/update/check').then(() => loadSelfUpdate()).catch(() => {});
  }
  renderSelfUpdate();
}

function renderSelfUpdate() {
  const u = selfUpd;
  if (!u) return;
  // A dot on the Dashboard tab says an update is waiting.
  $('#tabs button[data-tab="dashboard"]').classList.toggle('has-update', !!(u.available && u.update_available));
  const st = u.status;
  const running = st?.state === 'running' || !!selfUpdWatch;
  $('#update-state').innerHTML = running ? '<span class="pill warn">updating</span>'
    : u.update_available ? '<span class="pill warn">update available</span>'
    : u.latest ? '<span class="pill ok">up to date</span>' : '';
  let text;
  if (!u.available) {
    text = esc(u.reason);
  } else {
    text = `Running ${esc(u.version)}.` + (u.latest ? ` Latest release: ${esc(u.latest.version)} (${esc(new Date(u.latest.published).toLocaleDateString())}).` : '') +
      (u.checked_at ? ` <span class="muted">Checked ${esc(ago(u.checked_at))}.</span>` : '');
    if (running) text += ` <strong>${esc(st?.message || 'Starting the update')}…</strong> EdgeTAK restarts during the update; this page reloads when it is back.`;
    else if (st?.state === 'error') text += ` <span class="error">Last update failed: ${esc(st.message)}</span>`;
    else if (st?.state === 'done' && st.at && Date.now() - new Date(st.at) < 86400000) text += ` <span class="pill ok">done</span> ${esc(st.message)}.`;
  }
  $('#update-text').innerHTML = text;
  $('#update-check').hidden = !u.available;
  $('#update-check').disabled = running;
  const b = $('#update-install');
  b.hidden = !u.available || !u.update_available || running;
  if (u.latest) b.textContent = `Update to ${u.latest.version}`;
  const notes = $('#update-notes');
  notes.hidden = !u.latest?.notes;
  if (u.latest) {
    notes.querySelector('pre').textContent = u.latest.notes;
    notes.querySelector('summary').innerHTML = `Release notes for ${esc(u.latest.version)} · <a href="${esc(u.latest.url)}" target="_blank" rel="noopener">GitHub</a>`;
  }
}

$('#update-check').addEventListener('click', guard(async () => {
  await api('POST', 'api/server/update/check');
  await loadSelfUpdate();
  toast(selfUpd.update_available ? `EdgeTAK ${selfUpd.latest.version} is available` : `EdgeTAK ${selfUpd.version} is the latest version`);
}));

$('#update-install').addEventListener('click', guard(async () => {
  const v = selfUpd.latest.version;
  if (!confirm(`Update EdgeTAK ${selfUpd.version} to ${v}?\n\nEdgeTAK restarts: devices reconnect by themselves within a minute, and live video drops briefly. The update downloads ${v} from GitHub, checks it against the release checksums, and reinstalls with this server's settings.`)) return;
  await api('POST', 'api/server/update', { version: v });
  watchSelfUpdate(selfUpd.version, v);
}));

// watchSelfUpdate follows the update through EdgeTAK's restart and reloads
// the console once the new version answers.
function watchSelfUpdate(from, to) {
  const started = Date.now();
  clearInterval(selfUpdWatch);
  selfUpdWatch = setInterval(async () => {
    let v = '';
    try { v = (await (await fetch('/Marti/api/version', { cache: 'no-store' })).text()).trim(); } catch { /* restarting */ }
    try { selfUpd = await api('GET', 'api/server/update'); } catch { /* restarting */ }
    renderSelfUpdate();
    if (v && v.includes(to)) {
      clearInterval(selfUpdWatch);
      selfUpdWatch = null;
      toast(`Updated to ${to}; reloading`);
      setTimeout(() => location.reload(), 1500);
    } else if (selfUpd?.status?.state === 'error' && new Date(selfUpd.status.at) > started - 5000) {
      clearInterval(selfUpdWatch);
      selfUpdWatch = null;
      renderSelfUpdate();
      toast('The update failed; see the EdgeTAK updates card', true);
    } else if (Date.now() - started > 15 * 60000) {
      clearInterval(selfUpdWatch);
      selfUpdWatch = null;
      toast(`Still on ${from} after 15 minutes; check: journalctl -u edgetak-update`, true);
    }
  }, 3000);
  renderSelfUpdate();
}

$('#clients-table').addEventListener('click', guard(async (e) => {
  if (e.target.dataset.focus) {
    focusClient(e.target.dataset.focus);
    return;
  }
  const id = e.target.dataset.kick;
  if (!id) return;
  await api('POST', `api/clients/${id}/disconnect`);
  toast('Client disconnected');
  loadDashboard();
}));

// ---------- map ----------
//
// One map beside every tab (full screen on narrow screens), with base maps
// and overlays like ATAK's map sources. Admins can add their own sources,
// including ATAK map source XML files.

const prefs = {
  get(k, d) { try { const v = localStorage.getItem('edgetak.' + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem('edgetak.' + k, JSON.stringify(v)); } catch { /* storage unavailable */ } },
};

const MAX_ZOOM = 21;
const esri = (svc) => `https://server.arcgisonline.com/ArcGIS/rest/services/${svc}/MapServer/tile/{z}/{y}/{x}`;
const usgs = (svc) => `https://basemap.nationalmap.gov/arcgis/rest/services/${svc}/MapServer/tile/{z}/{y}/{x}`;
const OSM_ATTR = '&copy; OpenStreetMap contributors';
// [name, layers: [url, options]...]
const BASEMAPS = [
  ['Streets', [['https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', { maxNativeZoom: 19, attribution: OSM_ATTR }]]],
  ['Satellite', [[esri('World_Imagery'), { maxNativeZoom: 19, attribution: 'Imagery &copy; Esri, Maxar, Earthstar Geographics' }]]],
  ['Hybrid', [[esri('World_Imagery'), { maxNativeZoom: 19, attribution: 'Imagery &copy; Esri, Maxar, Earthstar Geographics' }],
    [esri('Reference/World_Boundaries_and_Places'), { maxNativeZoom: 19 }]]],
  ['Google Hybrid', [['https://mt{s}.google.com/vt/lyrs=y&x={x}&y={y}&z={z}', { maxNativeZoom: 20, subdomains: '0123', attribution: 'Imagery &copy; Google' }]]],
  ['Topo', [['https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png', { maxNativeZoom: 17, attribution: 'Map data ' + OSM_ATTR + ', SRTM | style &copy; OpenTopoMap (CC-BY-SA)' }]]],
  ['USGS Topo', [[usgs('USGSTopo'), { maxNativeZoom: 16, attribution: 'USGS The National Map' }]]],
  ['USGS Imagery Topo', [[usgs('USGSImageryTopo'), { maxNativeZoom: 16, attribution: 'USGS The National Map' }]]],
  ['Dark', [['https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', { maxNativeZoom: 20, subdomains: 'abcd', attribution: OSM_ATTR + ' &copy; CARTO' }]]],
];

let baseLayers = {};   // name -> layer
let overlayLayers = {}; // name -> layer
let layersControl = null;
let rebuildingLayers = false;
let devicesLayer = null;
let contactLayer = null;
let labelsLayer = null;
let trailsLayer = null;
let trailsTimer = null;
let videoLayer = null;
let mapSources = [];
let mapTimer = null;
let latestClients = [];

const isNarrow = () => window.matchMedia('(max-width: 1000px)').matches;
const mapVisible = () => !$('#map-panel').hidden;

function setMapVisible(on, remember = true) {
  $('#map-panel').hidden = !on;
  $('#workspace').classList.toggle('with-map', on);
  $('#map-toggle').setAttribute('aria-pressed', String(on));
  if (remember && !isNarrow()) prefs.set('map.open', on);
  clearInterval(mapTimer);
  if (!on) return;
  initMap();
  if (map) setTimeout(() => map.invalidateSize(), 0);
  refreshMap();
  // The dashboard refreshes clients itself; elsewhere the map polls.
  mapTimer = setInterval(() => { if (currentTab !== 'dashboard') refreshMap(); }, 3000);
}

$('#map-toggle').addEventListener('click', () => setMapVisible(!mapVisible()));
$('#map-close').addEventListener('click', () => setMapVisible(false));
$('#map-fit').addEventListener('click', () => fitDevices(true));

function tileLayer(url, opts) {
  return L.tileLayer(url, { maxZoom: MAX_ZOOM, ...opts });
}

function buildBase(def) {
  const layers = def.map(([url, opts]) => tileLayer(url, opts));
  return layers.length === 1 ? layers[0] : L.layerGroup(layers);
}

function initMap() {
  if (map || !window.L) return;
  const el = $('#map');
  el.innerHTML = '';
  const view = prefs.get('map.view', null);
  map = L.map(el, { worldCopyJump: true, maxZoom: MAX_ZOOM, zoomControl: true });
  if (view) map.setView([view.lat, view.lon], view.zoom);
  else map.setView([20, 0], 2);
  L.control.scale({ imperial: true }).addTo(map);
  devicesLayer = L.layerGroup().addTo(map);
  contactLayer = L.layerGroup().addTo(map);
  // Toggling callsign labels hides the tooltips rather than removing markers.
  labelsLayer = L.layerGroup();
  labelsLayer.on('add', () => el.classList.remove('no-labels'));
  labelsLayer.on('remove', () => el.classList.add('no-labels'));
  videoLayer = L.layerGroup();
  videoLayer.on('add', () => loadVideoFeeds().catch(() => {}));
  trailsLayer = L.layerGroup();
  trailsLayer.on('add', () => {
    $('#trails-show').checked = true;
    guard(loadTrails)();
    clearInterval(trailsTimer);
    trailsTimer = setInterval(() => { if (mapVisible()) loadTrails().catch(() => {}); }, 10000);
  });
  trailsLayer.on('remove', () => {
    $('#trails-show').checked = false;
    clearInterval(trailsTimer);
  });
  map.on('moveend', () => {
    const c = map.getCenter();
    prefs.set('map.view', { lat: c.lat, lon: c.lng, zoom: map.getZoom() });
  });
  map.on('baselayerchange', (e) => {
    if (rebuildingLayers) return;
    prefs.set('map.base', e.name);
    $('#basemap').value = e.name;
  });
  map.on('overlayadd overlayremove', () => {
    if (rebuildingLayers) return;
    prefs.set('map.overlays', Object.entries(overlayLayers).filter(([, l]) => map.hasLayer(l)).map(([n]) => n));
  });
  buildLayers();
  guard(loadMapSources)();
  if (view) fittedOnce = true;
  // Bring back the packages this viewer had on the map.
  for (const hash of prefs.get('map.packages', [])) showPackage(hash, { quiet: true }).catch(() => forgetPackage(hash));
}

// buildLayers (re)creates the layer switcher from the built-in base maps and
// the admin-defined sources, restoring the viewer's last choices.
function buildLayers() {
  if (!map) return;
  const activeBase = prefs.get('map.base', 'Streets');
  const activeOverlays = new Set(prefs.get('map.overlays', ['Devices', 'Callsigns', 'Server contact', 'Video']));
  // Detach the switcher first, or removing the old layers would be saved as
  // the viewer turning them off.
  rebuildingLayers = true;
  if (layersControl) layersControl.remove();
  for (const l of [...Object.values(baseLayers), ...Object.values(overlayLayers)]) map.removeLayer(l);

  baseLayers = {};
  for (const [name, def] of BASEMAPS) baseLayers[name] = buildBase(def);
  overlayLayers = { Devices: devicesLayer, Callsigns: labelsLayer, 'Server contact': contactLayer, Trails: trailsLayer, Video: videoLayer };
  for (const p of packageLayers.values()) overlayLayers[p.name] = p.layer;
  for (const s of mapSources) {
    const layer = tileLayer(s.url, {
      minZoom: 0, maxNativeZoom: s.max_zoom || 19, attribution: esc(s.attribution || ''),
      subdomains: s.subdomains ? (s.subdomains.includes(',') ? s.subdomains.split(',') : s.subdomains) : 'abc',
    });
    (s.overlay ? overlayLayers : baseLayers)[s.name] = layer;
  }

  (baseLayers[activeBase] || baseLayers.Streets).addTo(map);
  for (const [name, l] of Object.entries(overlayLayers)) {
    if (activeOverlays.has(name)) l.addTo(map); else map.removeLayer(l);
  }
  if (!map.hasLayer(labelsLayer)) $('#map').classList.add('no-labels');
  layersControl = L.control.layers(baseLayers, overlayLayers, { collapsed: true, position: 'topright' }).addTo(map);
  rebuildingLayers = false;

  $('#basemap').innerHTML = Object.keys(baseLayers).map((n) => `<option>${esc(n)}</option>`).join('');
  $('#basemap').value = baseLayers[activeBase] ? activeBase : 'Streets';
}

$('#basemap').addEventListener('change', (e) => {
  for (const l of Object.values(baseLayers)) map.removeLayer(l);
  baseLayers[e.target.value].addTo(map);
  map.fire('baselayerchange', { name: e.target.value, layer: baseLayers[e.target.value] });
});

async function refreshMap() {
  if (!map || !mapVisible()) return;
  try {
    updateMap(await api('GET', 'api/clients'));
  } catch { /* shown elsewhere */ }
}

function updateMap(clients) {
  latestClients = clients;
  if (!map) return;
  const withPos = clients.filter(hasPos);
  const seen = new Set();
  for (const c of withPos) {
    const key = c.uid || String(c.id);
    seen.add(key);
    const color = TEAM_COLORS[c.team] || '#4cc38a';
    const label = `<strong>${esc(c.callsign)}</strong><br>${esc(c.team || '')} ${esc(c.role || '')}<br>` +
      `<span class="mono">${c.lat.toFixed(5)}, ${c.lon.toFixed(5)}</span>` +
      `${c.speed ? `<br>${(c.speed * 3.6).toFixed(1)} km/h` : ''}${c.battery != null ? `<br>Battery ${c.battery}%` : ''}` +
      `<br><span class="muted">${esc(c.username || 'anonymous')} · ${esc([c.platform, c.device].filter(Boolean).join(' '))}</span>`;
    let m = markers.get(key);
    if (!m) {
      m = L.circleMarker([c.lat, c.lon], { radius: 8, weight: 2, color: '#000', fillOpacity: 0.9, className: 'live-device' }).addTo(devicesLayer);
      m.bindTooltip(esc(c.callsign), { permanent: true, direction: 'right', offset: [8, 0], className: 'live-device' });
      m.bindPopup('');
      markers.set(key, m);
    }
    m.setLatLng([c.lat, c.lon]).setStyle({ fillColor: color });
    m.setTooltipContent(esc(c.callsign));
    m.setPopupContent(label);
  }
  for (const [key, m] of markers) {
    if (!seen.has(key)) { m.remove(); markers.delete(key); }
  }
  if (!fittedOnce && withPos.length) fitDevices(false);
}

function fitDevices(explicit) {
  const pts = latestClients.filter(hasPos).map((c) => [c.lat, c.lon]);
  if (!map) return;
  if (!pts.length) {
    if (explicit) toast('No devices with a position yet');
    return;
  }
  if (pts.length === 1) map.setView(pts[0], Math.max(map.getZoom(), 14));
  else map.fitBounds(L.latLngBounds(pts).pad(0.3), { maxZoom: 16 });
  fittedOnce = true;
}

// focusClient shows a device on the map (from the clients table).
function focusClient(uid) {
  const m = markers.get(uid);
  if (!m) return;
  setMapVisible(true, false);
  map.invalidateSize();
  fittedOnce = true;
  map.setView(m.getLatLng(), Math.max(map.getZoom(), 15));
  m.openPopup();
}

async function refreshContactMarker() {
  if (!map) return;
  let c;
  try { c = await api('GET', 'api/send/contact'); } catch { return; }
  contactLayer.clearLayers();
  if (!c?.enabled || (!c.lat && !c.lon)) return;
  L.marker([c.lat, c.lon], { title: c.callsign })
    .bindTooltip(esc(c.callsign || 'Server'), { permanent: true, direction: 'right', offset: [10, -12] })
    .bindPopup(`<strong>${esc(c.callsign || 'Server')}</strong><br>Server contact (direct messages)`)
    .addTo(contactLayer);
}

// ---- custom map sources ----

async function loadMapSources() {
  mapSources = await api('GET', 'api/map/sources');
  buildLayers();
  renderMapSources();
  refreshContactMarker();
}

function renderMapSources() {
  $('#map-sources-list').innerHTML = mapSources.map((s, i) => `<div class="source-row">
      <div class="grow"><strong>${esc(s.name)}</strong> ${s.overlay ? '<span class="pill">overlay</span>' : '<span class="pill">base map</span>'}
        <div class="muted mono grow" title="${esc(s.url)}">${esc(s.url)}</div></div>
      <button type="button" class="small danger" data-remove-source="${i}">Remove</button>
    </div>`).join('') || '<p class="muted">No custom sources yet. The built-in ones are Streets, Satellite, Hybrid, Google Hybrid, Topo, USGS Topo, USGS Imagery Topo and Dark.</p>';
}

async function saveMapSources(list) {
  mapSources = await api('PUT', 'api/map/sources', list);
  buildLayers();
  renderMapSources();
}

$('#map-sources-open').addEventListener('click', () => {
  renderMapSources();
  $('#map-sources-dialog').showModal();
});

$('#map-sources-list').addEventListener('click', guard(async (e) => {
  const i = e.target.dataset.removeSource;
  if (i === undefined) return;
  await saveMapSources(mapSources.filter((_, j) => j !== Number(i)));
  toast('Map source removed');
}));

$('#ms-add').addEventListener('click', guard(async () => {
  const src = { name: $('#ms-name').value, url: $('#ms-url').value, max_zoom: Number($('#ms-max').value) || 19, overlay: $('#ms-overlay').checked };
  await saveMapSources([...mapSources, src]);
  $('#ms-name').value = '';
  $('#ms-url').value = '';
  toast(`Added ${src.name}`);
}));

$('#map-source-file').addEventListener('change', guard(async (e) => {
  const files = [...e.target.files];
  e.target.value = '';
  for (const f of files) {
    const r = await fetch('api/map/sources/import', { method: 'POST', headers: { 'X-EdgeTAK': '1', 'Content-Type': 'application/xml' }, body: await f.text() });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(`${f.name}: ${body.error || r.statusText}`);
    mapSources = body;
  }
  buildLayers();
  renderMapSources();
  toast(`Imported ${files.length} map source${files.length === 1 ? '' : 's'}`);
}));

// ---- position history (trails) ----
//
// Recording is a server setting; showing trails is this viewer's choice
// (the Trails overlay), over a time window they pick.

const trailWindow = () => Number(prefs.get('trails.window', 60));

async function loadTrails() {
  if (!map || !map.hasLayer(trailsLayer)) return;
  const tracks = await api('GET', `api/history?minutes=${trailWindow()}`);
  trailsLayer.clearLayers();
  for (const t of tracks) {
    const pts = t.points.map((p) => [p.lat, p.lon]);
    if (!pts.length) continue;
    const color = TEAM_COLORS[t.team] || '#4cc38a';
    const first = new Date(t.points[0].t), last = new Date(t.points.at(-1).t);
    let metres = 0;
    for (let i = 1; i < pts.length; i++) metres += map.distance(pts[i - 1], pts[i]);
    const info = `<strong>${esc(t.callsign || t.uid)}</strong><br>${esc(first.toLocaleTimeString())} – ${esc(last.toLocaleTimeString())}` +
      `<br>${metres >= 1000 ? (metres / 1000).toFixed(2) + ' km' : Math.round(metres) + ' m'} · ${t.points.length} points`;
    // A dark casing keeps light team colours (White, Yellow) visible on any base map.
    L.polyline(pts, { color: '#000', weight: 6, opacity: 0.45, interactive: false, className: 'trail-el' }).addTo(trailsLayer);
    L.polyline(pts, { color, weight: 3, opacity: 0.95, className: 'trail-el' }).bindPopup(info)
      .bindTooltip(esc(t.callsign || t.uid), { sticky: true, className: 'trail-el' }).addTo(trailsLayer);
    for (const p of t.points.filter((_, i) => i % Math.max(1, Math.ceil(t.points.length / 60)) === 0)) {
      L.circleMarker([p.lat, p.lon], { radius: 2.5, weight: 1, color: '#000', fillColor: color, fillOpacity: 1, className: 'trail-el' })
        .bindTooltip(esc(new Date(p.t).toLocaleString()), { direction: 'top', className: 'trail-el' }).addTo(trailsLayer);
    }
  }
}

async function loadHistorySettings() {
  const st = await api('GET', 'api/history/settings');
  $('#history-enabled').checked = st.enabled;
  $('#history-retention').value = String(st.retention_hours);
  if (!$('#history-retention').value) $('#history-retention').value = '24';
  $('#history-stats').textContent = st.enabled
    ? `Recording. Stored: ${st.tracks || 0} device(s), ${st.points || 0} points, ${bytes(st.bytes || 0)}.`
    : `Recording is off.${st.points ? ` Stored from before: ${st.tracks} device(s), ${st.points} points.` : ''}`;
}

$('#history-open').addEventListener('click', guard(async () => {
  initMap();
  $('#trails-show').checked = !!(map && map.hasLayer(trailsLayer));
  $('#trails-window').value = String(trailWindow());
  await loadHistorySettings();
  $('#history-dialog').showModal();
}));

$('#history-save').addEventListener('click', guard(async () => {
  await api('PUT', 'api/history/settings', { enabled: $('#history-enabled').checked, retention_hours: Number($('#history-retention').value) });
  await loadHistorySettings();
  toast($('#history-enabled').checked ? 'Position history is being recorded' : 'Position history recording is off');
}));

$('#history-clear').addEventListener('click', guard(async () => {
  if (!confirm('Delete all recorded position history? This cannot be undone.')) return;
  await api('DELETE', 'api/history');
  await loadHistorySettings();
  trailsLayer?.clearLayers();
  toast('History deleted');
}));

$('#trails-show').addEventListener('change', (e) => {
  if (!map) return;
  if (e.target.checked) trailsLayer.addTo(map); else map.removeLayer(trailsLayer);
});

$('#trails-window').addEventListener('change', (e) => {
  prefs.set('trails.window', Number(e.target.value));
  guard(loadTrails)();
});

// ---- replay ----
//
// Scrubs through recorded history: each device moves along its trail
// (interpolated between points) and draws its path up to the replay time.

const REPLAY_TICK = 100; // ms between frames while playing
const REPLAY_GAP = 5 * 60 * 1000; // don't interpolate across gaps longer than this
const REPLAY_STALE = 2 * 60 * 1000; // fade devices whose last report is older

let replay = null;

$('#replay-open').addEventListener('click', guard(async () => {
  $('#history-dialog').close();
  await openReplay();
}));

async function openReplay() {
  initMap();
  if (!map) throw new Error('The map library could not be loaded');
  closeReplay();
  const tracks = (await api('GET', `api/history?minutes=${trailWindow()}`)).filter((t) => t.points.length);
  if (!tracks.length) throw new Error('No recorded history in this time window. Turn on recording in History first.');
  setMapVisible(true, false);
  map.invalidateSize();
  fittedOnce = true;

  const layer = L.layerGroup().addTo(map);
  const items = tracks.map((t) => {
    const color = TEAM_COLORS[t.team] || '#4cc38a';
    const casing = L.polyline([], { color: '#000', weight: 6, opacity: 0.45, interactive: false }).addTo(layer);
    const line = L.polyline([], { color, weight: 3, opacity: 0.95, interactive: false }).addTo(layer);
    const marker = L.circleMarker([t.points[0].lat, t.points[0].lon], { radius: 8, weight: 2, color: '#000', fillColor: color, fillOpacity: 0.9 })
      .bindTooltip(esc(t.callsign || t.uid), { permanent: true, direction: 'right', offset: [8, 0] });
    return { t, casing, line, marker, layer };
  });
  const start = Math.min(...tracks.map((t) => t.points[0].t));
  const end = Math.max(...tracks.map((t) => t.points.at(-1).t));
  replay = { items, layer, start, end, time: start, timer: null };

  const slider = $('#replay-slider');
  slider.min = start;
  slider.max = end;
  slider.value = start;
  $('#map').classList.add('replaying');
  $('#replay-bar').hidden = false;
  map.invalidateSize();
  map.fitBounds(L.latLngBounds(tracks.flatMap((t) => t.points.map((p) => [p.lat, p.lon]))).pad(0.15), { maxZoom: 17 });
  setReplayTime(start);
  playReplay();
}

// replayPosition finds where a track is at time t: the index of the last
// point at or before t and the (interpolated) position, or null before the
// track starts.
function replayPosition(points, t) {
  let lo = 0, hi = points.length - 1, i = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (points[mid].t <= t) { i = mid; lo = mid + 1; } else hi = mid - 1;
  }
  if (i < 0) return null;
  const p = points[i], n = points[i + 1];
  if (!n || n.t - p.t > REPLAY_GAP) return { i, ll: [p.lat, p.lon], stale: t - p.t > REPLAY_STALE };
  const f = (t - p.t) / (n.t - p.t);
  return { i, ll: [p.lat + (n.lat - p.lat) * f, p.lon + (n.lon - p.lon) * f], stale: false };
}

function setReplayTime(t) {
  if (!replay) return;
  replay.time = Math.min(Math.max(t, replay.start), replay.end);
  $('#replay-slider').value = replay.time;
  $('#replay-time').textContent = new Date(replay.time).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
  for (const it of replay.items) {
    const pos = replayPosition(it.t.points, replay.time);
    if (!pos) {
      it.marker.remove();
      it.line.setLatLngs([]);
      it.casing.setLatLngs([]);
      continue;
    }
    const path = it.t.points.slice(0, pos.i + 1).map((p) => [p.lat, p.lon]);
    path.push(pos.ll);
    it.line.setLatLngs(path);
    it.casing.setLatLngs(path);
    it.marker.setLatLng(pos.ll).setStyle({ fillOpacity: pos.stale ? 0.35 : 0.9, opacity: pos.stale ? 0.4 : 1 });
    if (!it.layer.hasLayer(it.marker)) it.marker.addTo(it.layer);
  }
}

function playReplay() {
  if (!replay) return;
  if (replay.time >= replay.end) setReplayTime(replay.start); // play again from the start
  clearInterval(replay.timer);
  replay.timer = setInterval(() => {
    const next = replay.time + Number($('#replay-speed').value) * REPLAY_TICK;
    setReplayTime(next);
    if (next >= replay.end) pauseReplay();
  }, REPLAY_TICK);
  $('#replay-play').textContent = '❚❚';
}

function pauseReplay() {
  if (!replay) return;
  clearInterval(replay.timer);
  replay.timer = null;
  $('#replay-play').textContent = '▶';
}

function closeReplay() {
  if (!replay) return;
  pauseReplay();
  replay.layer.remove();
  replay = null;
  $('#map').classList.remove('replaying');
  $('#replay-bar').hidden = true;
  map?.invalidateSize();
}

$('#replay-play').addEventListener('click', () => (replay?.timer ? pauseReplay() : playReplay()));
$('#replay-slider').addEventListener('input', (e) => setReplayTime(Number(e.target.value)));
$('#replay-close').addEventListener('click', closeReplay);

// ---- data packages on the map ----
//
// A package is fetched like a download and drawn in the browser: CoT items
// (markers, routes, polylines, polygons, rectangles, circles) and any KML,
// KMZ or GeoJSON inside it. Each shown package is an overlay.

const packageLayers = new Map(); // hash -> { name, layer }
let packagesByHash = {};

function rememberPackages() {
  prefs.set('map.packages', [...packageLayers.keys()]);
}

function forgetPackage(hash) {
  const p = packageLayers.get(hash);
  if (p) {
    map?.removeLayer(p.layer);
    layersControl?.removeLayer(p.layer);
    delete overlayLayers[p.name];
    packageLayers.delete(hash);
  }
  rememberPackages();
}

async function showPackage(hash, { quiet = false } = {}) {
  initMap();
  if (!map) throw new Error('The map library could not be loaded');
  if (packageLayers.has(hash)) {
    const p = packageLayers.get(hash);
    p.layer.addTo(map);
    if (!quiet) fitLayer(p.layer);
    return;
  }
  const r = await fetch(`/Marti/sync/content?hash=${encodeURIComponent(hash)}`);
  if (!r.ok) throw new Error(`Could not fetch the package (${r.status})`);
  const meta = packagesByHash[hash];
  const fileName = meta?.name || r.headers.get('content-disposition')?.match(/filename="?([^";]+)"?/)?.[1] || hash.slice(0, 12);
  const layer = L.featureGroup();
  await loadMilsymbol().catch(() => {}); // without it, units are drawn as coloured dots
  const count = await drawFile(layer, fileName, new Uint8Array(await r.arrayBuffer()));
  if (!count) {
    if (!quiet) toast(`${fileName} has nothing to show on a map`, true);
    forgetPackage(hash);
    return;
  }
  let name = `Package: ${fileName}`;
  if (Object.prototype.hasOwnProperty.call(overlayLayers, name)) name += ` (${hash.slice(0, 6)})`;
  packageLayers.set(hash, { name, layer });
  overlayLayers[name] = layer;
  layersControl.addOverlay(layer, name);
  layer.addTo(map);
  rememberPackages();
  if (!quiet) {
    fitLayer(layer);
    toast(`${fileName}: ${count} item${count === 1 ? '' : 's'} on the map`);
  }
}

function fitLayer(layer) {
  setMapVisible(true, false);
  // The panel may have just been unhidden (phones); measure it before fitting,
  // and stop the first device update from moving the view away.
  map.invalidateSize();
  fittedOnce = true;
  const b = layer.getBounds();
  if (b.isValid()) map.fitBounds(b.pad(0.2), { maxZoom: 17 });
}

const isZip = (b) => b[0] === 0x50 && b[1] === 0x4b;
const text = (b) => new TextDecoder().decode(b);

// drawFile adds whatever it understands in one file (recursing into zips)
// and returns how many items it drew.
async function drawFile(layer, name, bytes) {
  const lower = name.toLowerCase();
  if (isZip(bytes)) {
    if (!window.JSZip) throw new Error('The zip library could not be loaded');
    const zip = await JSZip.loadAsync(bytes);
    let n = 0;
    for (const f of Object.values(zip.files)) {
      if (f.dir || /(^|\/)manifest\.xml$/i.test(f.name) || !/\.(cot|xml|kml|kmz|geojson|json|zip)$/i.test(f.name)) continue;
      n += await drawFile(layer, f.name, await f.async('uint8array'));
    }
    return n;
  }
  if (/\.(geo)?json$/.test(lower)) {
    try { return drawGeoJSON(layer, JSON.parse(text(bytes))); } catch { return 0; }
  }
  const doc = new DOMParser().parseFromString(text(bytes), 'application/xml');
  if (doc.querySelector('parsererror')) return 0;
  const root = doc.documentElement;
  if (root.localName === 'kml' && window.toGeoJSON) return drawGeoJSON(layer, toGeoJSON.kml(doc));
  if (root.localName === 'event') return drawCoT(layer, root);
  if (root.localName === 'events' || root.querySelector('event')) {
    let n = 0;
    for (const ev of doc.querySelectorAll('event')) n += drawCoT(layer, ev);
    return n;
  }
  return 0;
}

// ATAK stores colours as signed 32-bit ARGB integers.
function argb(v, fallback) {
  const n = Number(v);
  if (v == null || v === '' || !Number.isFinite(n)) return fallback;
  const u = n >>> 0;
  return { color: '#' + (u & 0xffffff).toString(16).padStart(6, '0'), opacity: ((u >>> 24) & 0xff) / 255 };
}

function cotPopup(title, type, remarks) {
  return `<strong>${esc(title)}</strong><br><span class="muted mono">${esc(type)}</span>${remarks ? `<br>${esc(remarks)}` : ''}`;
}

function drawCoT(layer, ev) {
  const type = ev.getAttribute('type') || '';
  if (type.startsWith('t-x-') || type === 'b-t-f' || type.startsWith('b-f-t')) return 0; // control, chat, file transfers
  const detail = ev.querySelector('detail');
  const pt = ev.querySelector(':scope > point');
  const lat = Number(pt?.getAttribute('lat')), lon = Number(pt?.getAttribute('lon'));
  const title = detail?.querySelector('contact')?.getAttribute('callsign') || ev.getAttribute('uid') || type;
  const remarks = detail?.querySelector('remarks')?.textContent?.trim() || '';
  const stroke = argb(detail?.querySelector('strokeColor')?.getAttribute('value'), { color: '#ffff00', opacity: 1 });
  const fill = argb(detail?.querySelector('fillColor')?.getAttribute('value'), null);
  const weight = Number(detail?.querySelector('strokeWeight')?.getAttribute('value')) || 3;
  const style = { color: stroke.color, opacity: stroke.opacity || 1, weight, fillColor: fill?.color || stroke.color, fillOpacity: fill ? fill.opacity : 0 };
  const popup = cotPopup(title, type, remarks);

  // Shapes and routes list their vertices as <link point="lat,lon,hae">.
  const verts = [...(detail?.querySelectorAll('link[point]') || [])]
    .map((l) => l.getAttribute('point').split(',').map(Number)).filter(([a, b]) => Number.isFinite(a) && Number.isFinite(b)).map(([a, b]) => [a, b]);
  const ellipse = detail?.querySelector('shape ellipse');
  let item = null;
  if (ellipse && Number.isFinite(lat)) {
    item = L.circle([lat, lon], { radius: Number(ellipse.getAttribute('major')) || 1, ...style });
  } else if (verts.length >= 2) {
    const closed = type === 'u-d-r' || detail?.querySelector('closed')?.getAttribute('value') === 'true' ||
      (verts.length > 2 && verts[0][0] === verts.at(-1)[0] && verts[0][1] === verts.at(-1)[1]) || /^u-d-f/.test(type) && fill;
    item = closed ? L.polygon(verts, style) : L.polyline(verts, type.startsWith('b-m-r') ? { ...style, dashArray: '8 6' } : style);
  } else if (Number.isFinite(lat) && Number.isFinite(lon) && !(lat === 0 && lon === 0)) {
    item = cotPointMarker([lat, lon], type, detail);
  }
  if (!item) return 0;
  item.bindPopup(popup);
  if (item instanceof L.Marker || (item instanceof L.CircleMarker && !(item instanceof L.Circle))) {
    item.bindTooltip(esc(title), { permanent: true, direction: 'right', offset: item instanceof L.Marker ? [0, 0] : [8, 0] });
  } else {
    item.bindTooltip(esc(title), { sticky: true });
  }
  item.addTo(layer);
  return 1;
}

// cotPointMarker draws a point the way ATAK does: MIL-STD-2525 symbols for
// a-* types, coloured dots for spot map markers, and a pin in the marker's
// colour for everything else (waypoints, custom icon sets we don't have).
function cotPointMarker(ll, type, detail) {
  const iconset = detail?.querySelector('usericon')?.getAttribute('iconsetpath') || '';
  const color = argb(detail?.querySelector('color')?.getAttribute('argb'), null)?.color;
  const spot = type.startsWith('b-m-p-s-m') || iconset.startsWith('COT_MAPPING_SPOTMAP');
  const sidc = !spot && (iconset === '' || iconset.startsWith('COT_MAPPING_2525')) ? cotToSIDC(type) : null;
  if (sidc && window.ms) {
    try {
      const sym = new ms.Symbol(sidc, { size: 22 });
      if (sym.isValid()) {
        const { width, height } = sym.getSize();
        const a = sym.getAnchor();
        return L.marker(ll, { icon: L.icon({
          iconUrl: sym.asCanvas().toDataURL(), iconSize: [width, height], iconAnchor: [a.x, a.y],
          tooltipAnchor: [width - a.x + 2, height / 2 - a.y], popupAnchor: [0, -a.y],
        }) });
      }
    } catch { /* fall through to a plain marker */ }
  }
  if (spot) return L.circleMarker(ll, { radius: 7, weight: 2, color: '#000', fillColor: color || '#ffffff', fillOpacity: 0.95 });
  if (sidc) return L.circleMarker(ll, { radius: 7, weight: 2, color: '#000', fillColor: cotAffiliationColor(type), fillOpacity: 0.9 });
  return L.marker(ll, { icon: pinIcon(color && color !== '#ffffff' ? color : '#ffd24c') });
}

// cotToSIDC maps a CoT atom type (a-f-G-U-C-I) to a 2525B symbol code
// (SFGPUCI--------).
function cotToSIDC(type) {
  const p = type.split('-');
  if (p[0] !== 'a' || p.length < 3) return null;
  const aff = { f: 'F', h: 'H', u: 'U', n: 'N', a: 'A', s: 'S', j: 'J', k: 'K', p: 'P', o: 'U', x: 'U' }[p[1]] || 'U';
  const dim = 'PAGSUF'.includes(p[2]) ? p[2] : 'G';
  const fn = p.slice(3).join('').toUpperCase().replace(/[^A-Z0-9]/g, '');
  return 'S' + aff + dim + 'P' + (fn + '------').slice(0, 6) + '-----';
}

function pinIcon(color) {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="22" height="30" viewBox="0 0 22 30"><path d="M11 29S2 17.5 2 11a9 9 0 0 1 18 0c0 6.5-9 18-9 18z" fill="${color}" stroke="#000" stroke-width="1.5"/><circle cx="11" cy="11" r="3.5" fill="#000" fill-opacity=".5"/></svg>`;
  return L.divIcon({ className: 'pin-icon', html: svg, iconSize: [22, 30], iconAnchor: [11, 29], tooltipAnchor: [10, -18], popupAnchor: [0, -26] });
}

// The 2525 symbol library is large, so it loads the first time a package is shown.
let milsymbolLoading = null;
function loadMilsymbol() {
  if (window.ms) return Promise.resolve();
  milsymbolLoading ||= new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = 'vendor/milsymbol.js';
    s.onload = resolve;
    s.onerror = () => { milsymbolLoading = null; reject(new Error('symbol library unavailable')); };
    document.head.appendChild(s);
  });
  return milsymbolLoading;
}

// MIL-STD-2525 affiliation colours for points without their own colour.
function cotAffiliationColor(type) {
  const aff = type.startsWith('a-') ? type[2] : '';
  return { f: '#80e0ff', a: '#80e0ff', h: '#ff8080', j: '#ff8080', k: '#ff8080', n: '#aaffaa', u: '#ffff80', p: '#ffff80' }[aff] || '#ffd24c';
}

function drawGeoJSON(layer, gj) {
  let n = 0;
  L.geoJSON(gj, {
    style: (f) => {
      const p = f.properties || {};
      return { color: p.stroke || '#ffd24c', weight: p['stroke-width'] || 3, opacity: p['stroke-opacity'] ?? 1, fillColor: p.fill || p.stroke || '#ffd24c', fillOpacity: p['fill-opacity'] ?? 0.2 };
    },
    pointToLayer: (f, ll) => L.circleMarker(ll, { radius: 7, weight: 2, color: '#000', fillColor: f.properties?.['marker-color'] || '#ffd24c', fillOpacity: 0.9 }),
    onEachFeature: (f, l) => {
      n++;
      const p = f.properties || {};
      if (p.name) l.bindTooltip(esc(p.name), f.geometry?.type === 'Point' ? { permanent: true, direction: 'right', offset: [8, 0] } : { sticky: true });
      const desc = typeof p.description === 'string' ? p.description.replace(/<[^>]*>/g, ' ').trim() : '';
      l.bindPopup(`<strong>${esc(p.name || 'Feature')}</strong>${desc ? `<br>${esc(desc)}` : ''}`);
    },
  }).eachLayer((l) => l.addTo(layer));
  return n;
}

// Drag the panel's left edge to resize it.
(() => {
  const handle = $('#map-resize');
  const saved = prefs.get('map.width', null);
  if (saved) document.documentElement.style.setProperty('--map-w', saved + 'px');
  handle.addEventListener('pointerdown', (e) => {
    handle.setPointerCapture(e.pointerId);
    handle.classList.add('dragging');
  });
  handle.addEventListener('pointermove', (e) => {
    if (!handle.classList.contains('dragging')) return;
    const w = Math.min(Math.max(window.innerWidth - e.clientX, 320), window.innerWidth - 420);
    document.documentElement.style.setProperty('--map-w', w + 'px');
    map?.invalidateSize();
  });
  handle.addEventListener('pointerup', () => {
    handle.classList.remove('dragging');
    prefs.set('map.width', parseInt(getComputedStyle(document.documentElement).getPropertyValue('--map-w'), 10) || null);
  });
})();

// The sticky map panel sits below the top bar, whose height varies.
new ResizeObserver(([e]) => {
  document.documentElement.style.setProperty('--topbar-h', Math.ceil(e.target.getBoundingClientRect().height) + 'px');
}).observe($('.topbar'));
window.addEventListener('resize', () => map?.invalidateSize());

// ---------- users ----------

let allGroups = [];
let usersByName = {};

function renderNewUserPicker() {
  const checked = new Set([...document.querySelectorAll('#new-user-groups input:checked')].map((i) => i.value));
  $('#new-user-groups').innerHTML = allGroups.map((g) => `<label class="chip" title="${esc(g.description)}">
      <input type="checkbox" name="g" value="${esc(g.name)}"${checked.has(g.name) ? ' checked' : ''}>${esc(g.name)}</label>`).join('')
    + '<button type="button" class="small" id="new-user-new-group">+ New group</button>';
}

// promptNewGroup creates a group from the Users tab and refreshes the list.
async function promptNewGroup() {
  const name = prompt('Name of the new group');
  if (!name) return null;
  await api('POST', 'api/groups', { name: name.trim() });
  allGroups = await api('GET', 'api/groups');
  toast(`Group ${name.trim()} created`);
  return name.trim();
}

// ---- two-factor login (own account) ----

async function loadTwoFactor() {
  const st = await api('GET', 'api/me/2fa');
  $('#twofa-state').innerHTML = st.enabled ? '<span class="pill ok">on</span>' : '<span class="pill">off</span>';
  $('#twofa-text').innerHTML = st.enabled
    ? `Signing in to this console asks for a code from your authenticator app. Recovery codes left: <strong>${st.recovery_left}</strong>.` +
      (st.recovery_left < 3 ? ' <span class="error">Make new recovery codes soon.</span>' : '') +
      ' Devices (ATAK, WinTAK, iTAK, TAKX) keep signing in with the password.'
    : 'Optional: after the password, also ask for a 6-digit code from an authenticator app on your phone. Devices (ATAK, WinTAK, iTAK, TAKX) are not affected.';
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

$('#twofa-setup').addEventListener('click', guard(async () => {
  const { secret, uri } = await api('POST', 'api/me/2fa/setup');
  $('#twofa-step1').hidden = false;
  $('#twofa-step2').hidden = true;
  $('#twofa-form').code.value = '';
  $('#twofa-secret').textContent = secret.match(/.{1,4}/g).join(' ');
  const box = $('#twofa-qr');
  box.innerHTML = '';
  if (window.QRCode) new QRCode(box, { text: uri, width: 200, height: 200, correctLevel: QRCode.CorrectLevel.M });
  $('#twofa-dialog').showModal();
  $('#twofa-form').code.focus();
}));

$('#twofa-form').addEventListener('submit', guard(async (e) => {
  if (e.submitter?.value !== 'enable') return; // cancel / done close the dialog
  e.preventDefault();
  const res = await api('POST', 'api/me/2fa/enable', { code: e.target.code.value });
  showRecoveryCodes(res.recovery_codes);
  loadTwoFactor();
}));

$('#twofa-copy').addEventListener('click', guard(async () => {
  await navigator.clipboard.writeText($('#twofa-codes').textContent);
  toast('Recovery codes copied');
}));

$('#twofa-download').addEventListener('click', () => {
  const name = status?.server_name || 'EdgeTAK';
  const text = `${name} recovery codes for ${$('#me').textContent}\nEach code signs you in once.\n\n${$('#twofa-codes').textContent}\n`;
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

$('#twofa-disable').addEventListener('click', guard(async () => {
  const creds = await reauth('Turn off two-factor login', 'Your password and a current code (or a recovery code) are needed.');
  if (!creds) return;
  await api('POST', 'api/me/2fa/disable', creds);
  toast('Two-factor login turned off');
  loadTwoFactor();
}));

$('#twofa-recovery').addEventListener('click', guard(async () => {
  const creds = await reauth('New recovery codes', 'The old recovery codes stop working. Your password and a current code are needed.');
  if (!creds) return;
  const res = await api('POST', 'api/me/2fa/recovery', creds);
  showRecoveryCodes(res.recovery_codes);
  loadTwoFactor();
}));

async function loadUsers() {
  loadTwoFactor().catch(() => {});
  const [users, groups] = await Promise.all([api('GET', 'api/users'), api('GET', 'api/groups')]);
  allGroups = groups;
  usersByName = Object.fromEntries(users.map((u) => [u.username, u]));
  renderNewUserPicker();
  $('#users-table tbody').innerHTML = users.map((u) => `<tr>
    <td><strong>${esc(u.username)}</strong></td>
    <td>${userGroupPills(u)}</td>
    <td>${u.admin ? '<span class="pill ok">admin</span>' : 'user'}${u.two_factor ? ' <span class="pill ok" title="Two-factor login is on">2FA</span>' : ''}</td>
    <td>${u.online ? `<span class="pill ok">${u.online} online</span>` : '<span class="muted">offline</span>'}</td>
    <td>${esc(date(u.created))}</td>
    <td class="actions">
      <button class="small" data-act="groups" data-user="${esc(u.username)}">Groups</button>
      <button class="small" data-act="password" data-user="${esc(u.username)}">Password</button>
      <button class="small" data-act="admin" data-user="${esc(u.username)}" data-admin="${u.admin}">${u.admin ? 'Revoke admin' : 'Make admin'}</button>
      ${u.two_factor && u.username !== $('#me').textContent ? `<button class="small" data-act="reset2fa" data-user="${esc(u.username)}" title="For a lost phone">Reset 2FA</button>` : ''}
      <button class="small" data-act="package" data-user="${esc(u.username)}" title="Connection data package for this user">Package</button>
      <button class="small" data-act="p12" data-user="${esc(u.username)}">Issue .p12</button>
      <button class="small danger" data-act="delete" data-user="${esc(u.username)}">Delete</button>
    </td></tr>`).join('') || emptyRow(6, 'No users');
}

$('#user-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  await api('POST', 'api/users', {
    username: f.get('username'), password: f.get('password'), groups: f.getAll('g'), admin: f.get('admin') === 'on',
  });
  e.target.reset();
  toast('User created');
  loadUsers();
}));

$('#new-user-groups').addEventListener('click', guard(async (e) => {
  if (e.target.id !== 'new-user-new-group') return;
  const name = await promptNewGroup();
  renderNewUserPicker();
  if (name) {
    const box = [...document.querySelectorAll('#new-user-groups input')].find((i) => i.value === name);
    if (box) box.checked = true;
  }
}));

// ---- per-user groups dialog ----

let dialogUser = null;

function userDirection(u, group) {
  if ((u.groups || []).includes(group)) return 'both';
  if ((u.groups_in || []).includes(group)) return 'in';
  if ((u.groups_out || []).includes(group)) return 'out';
  return '';
}

function renderGroupsDialog(selected) {
  $('#groups-dialog-list').innerHTML = allGroups.map((g) => {
    const cur = selected[g.name] || '';
    const opt = (v, label) => `<option value="${v}"${cur === v ? ' selected' : ''}>${label}</option>`;
    return `<div class="group-row">
      <div><strong>${esc(g.name)}</strong>${g.description ? `<div class="muted">${esc(g.description)}</div>` : ''}</div>
      <select data-group="${esc(g.name)}">${opt('', 'not a member')}${opt('both', 'in + out')}${opt('in', 'in (receive only)')}${opt('out', 'out (send only)')}</select>
    </div>`;
  }).join('') || '<p class="muted">No groups yet. Create one with "+ New group".</p>';
}

function dialogSelections() {
  return Object.fromEntries([...document.querySelectorAll('#groups-dialog-list select')].map((s) => [s.dataset.group, s.value]));
}

function openGroupsDialog(username) {
  const u = usersByName[username];
  dialogUser = username;
  $('#groups-dialog-title').textContent = `Groups for ${username}`;
  renderGroupsDialog(Object.fromEntries(allGroups.map((g) => [g.name, userDirection(u, g.name)])));
  $('#groups-dialog').showModal();
}

$('#groups-dialog-new').addEventListener('click', guard(async () => {
  const selected = dialogSelections();
  const name = await promptNewGroup();
  if (name) selected[name] = 'both';
  renderGroupsDialog(selected);
}));

$('#groups-dialog-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const dialog = $('#groups-dialog');
  if (e.submitter?.value !== 'save') {
    dialog.close();
    return;
  }
  const sel = Object.entries(dialogSelections());
  const pick = (dir) => sel.filter(([, d]) => d === dir).map(([g]) => g);
  await api('PATCH', `api/users/${encodeURIComponent(dialogUser)}`, { groups: pick('both'), groups_in: pick('in'), groups_out: pick('out') });
  dialog.close();
  toast(`Groups updated for ${dialogUser}`);
  loadUsers();
}));

$('#users-table').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  const user = b.dataset.user;
  const path = `api/users/${encodeURIComponent(user)}`;
  switch (b.dataset.act) {
    case 'groups':
      openGroupsDialog(user);
      return;
    case 'package':
      connectPick = user; // preselected when the Connect tab loads
      selectTab('connect');
      return;
    case 'password': {
      const pw = prompt(`New password for ${user} (8+ characters)`);
      if (!pw) return;
      await api('PATCH', path, { password: pw });
      toast('Password changed');
      break;
    }
    case 'reset2fa':
      if (!confirm(`Turn off two-factor login for ${user}? They sign in with only their password until they set it up again.`)) return;
      await api('DELETE', `${path}/2fa`);
      toast(`Two-factor login reset for ${user}`);
      break;
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

// ---------- sign-ups ----------

function renderChips(sel, selected) {
  const on = new Set(selected);
  $(sel).innerHTML = allGroups.map((g) => `<label class="chip" title="${esc(g.description)}">
      <input type="checkbox" name="g" value="${esc(g.name)}"${on.has(g.name) ? ' checked' : ''}>${esc(g.name)}</label>`).join('')
    || '<span class="muted">No groups yet; create them in the Groups tab.</span>';
}

const checkedIn = (sel) => [...document.querySelectorAll(`${sel} input:checked`)].map((i) => i.value);

let invitesByCode = {};

async function loadSignups() {
  const [reg, groups] = await Promise.all([api('GET', 'api/registration'), api('GET', 'api/groups')]);
  allGroups = groups;
  const inviteSel = checkedIn('#invite-groups');
  renderChips('#invite-groups', inviteSel);
  renderChips('#registration-groups', reg.settings.default_groups || []);
  for (const r of document.querySelectorAll('#registration-form input[name="mode"]')) r.checked = r.value === reg.settings.mode;
  $('#register-url').textContent = reg.register_url;
  $('#register-url').href = reg.register_url;

  $('#requests-table tbody').innerHTML = reg.requests.map((q) => `<tr>
    <td><strong>${esc(q.username)}</strong></td>
    <td class="wrap">${esc(q.note) || '<span class="muted">-</span>'}</td>
    <td class="mono">${esc(q.remote)}</td>
    <td>${esc(ago(q.created))}</td>
    <td class="actions">
      <button class="small primary" data-act="approve" data-id="${esc(q.id)}" title="Creates the account in the groups for new accounts">Approve</button>
      <button class="small danger" data-act="reject" data-id="${esc(q.id)}">Reject</button>
    </td></tr>`).join('') || '<tr class="empty"><td colspan="5">No pending requests</td></tr>';

  invitesByCode = Object.fromEntries(reg.invites.map((i) => [i.code, i]));
  $('#invites-table tbody').innerHTML = reg.invites.map((i) => {
    const st = i.revoked ? '<span class="pill bad">revoked</span>' : i.usable ? '<span class="pill ok">active</span>' : '<span class="pill warn">used up / expired</span>';
    const groupsHTML = (i.groups || []).map((g) => `<span class="pill">${esc(g)}</span>`).join('') || '<span class="pill">__ANON__</span>';
    return `<tr>
    <td>${esc(i.label) || '<span class="muted">(no label)</span>'}</td>
    <td>${groupsHTML}</td>
    <td>${i.uses} / ${i.max_uses || '∞'}</td>
    <td>${i.expires && !i.expires.startsWith('0001') ? esc(date(i.expires)) : 'never'}</td>
    <td>${st}</td>
    <td>${esc(ago(i.created))} by ${esc(i.created_by)}</td>
    <td class="actions">${i.usable ? `<button class="small" data-act="show" data-code="${esc(i.code)}">Link / QR</button>
      <button class="small danger" data-act="revoke" data-code="${esc(i.code)}">Revoke</button>` : ''}</td></tr>`;
  }).join('') || '<tr class="empty"><td colspan="7">No invites yet</td></tr>';
}

function showInvite(inv) {
  $('#invite-dialog-title').textContent = inv.label ? `Invite: ${inv.label}` : 'Invite link';
  $('#invite-dialog-url').textContent = inv.url;
  const box = $('#invite-qr');
  box.innerHTML = '';
  if (window.QRCode) new QRCode(box, { text: inv.url, width: 220, height: 220, correctLevel: QRCode.CorrectLevel.M });
  $('#invite-dialog').showModal();
}

$('#invite-copy').addEventListener('click', guard(async () => {
  await navigator.clipboard.writeText($('#invite-dialog-url').textContent);
  toast('Link copied');
}));

$('#invite-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const inv = await api('POST', 'api/registration/invites', {
    label: f.get('label'), groups: checkedIn('#invite-groups'),
    max_uses: Number(f.get('max_uses')) || 0, expires_days: Number(f.get('expires_days')) || 0,
  });
  e.target.querySelector('input[name="label"]').value = '';
  await loadSignups();
  showInvite(inv);
}));

$('#registration-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const mode = new FormData(e.target).get('mode') || 'off';
  await api('PUT', 'api/registration', { mode, default_groups: checkedIn('#registration-groups') });
  toast('Sign-up settings saved');
  loadSignups();
}));

$('#tab-signups').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  switch (b.dataset.act) {
    case 'show':
      showInvite(invitesByCode[b.dataset.code]);
      return;
    case 'revoke':
      if (!confirm('Revoke this invite? The link stops working; accounts already created stay.')) return;
      await api('DELETE', `api/registration/invites/${encodeURIComponent(b.dataset.code)}`);
      toast('Invite revoked');
      break;
    case 'approve': {
      const u = await api('POST', `api/registration/requests/${encodeURIComponent(b.dataset.id)}/approve`, {});
      toast(`Account ${u.username} created`);
      break;
    }
    case 'reject':
      if (!confirm('Reject this request?')) return;
      await api('DELETE', `api/registration/requests/${encodeURIComponent(b.dataset.id)}`);
      toast('Request rejected');
      break;
  }
  loadSignups();
}));

// ---------- groups ----------

const DIR_LABEL = { both: '', in: 'in', out: 'out' };

function userGroupPills(u) {
  const parts = [
    ...(u.groups || []).map((g) => [g, 'both']),
    ...(u.groups_in || []).map((g) => [g, 'in']),
    ...(u.groups_out || []).map((g) => [g, 'out']),
  ];
  if (!parts.length) return '<span class="pill">__ANON__</span>';
  const off = new Set(u.inactive || []);
  return parts.map(([g, d]) => {
    const dirs = d === 'both' ? ['IN', 'OUT'] : [d.toUpperCase()];
    const allOff = dirs.every((x) => off.has(`${g}:${x}`));
    const someOff = dirs.some((x) => off.has(`${g}:${x}`));
    const note = allOff ? ' <span class="muted" title="switched off by the user in their client (Channels)">off</span>' : someOff ? ' <span class="muted">partly off</span>' : '';
    return `<span class="pill"${allOff ? ' style="opacity:.55"' : ''}>${esc(g)}${d !== 'both' ? ` <span class="muted">${d}</span>` : ''}${note}</span>`;
  }).join('');
}

async function loadGroups() {
  const [groups, users] = await Promise.all([api('GET', 'api/groups'), api('GET', 'api/users')]);
  const userOptions = users.map((u) => `<option value="${esc(u.username)}">${esc(u.username)}</option>`).join('');
  $('#groups-table tbody').innerHTML = groups.map((g) => {
    const members = g.members.map((m) => `<span class="member pill">${esc(m.username)}${DIR_LABEL[m.direction] ? ` <span class="dir">${DIR_LABEL[m.direction]}</span>` : ''}<button title="Remove from group" data-remove="${esc(m.username)}" data-group="${esc(g.name)}">×</button></span>`).join('');
    const implicit = g.builtin ? '<div class="muted">Also: users without groups and plaintext TCP clients</div>' : '';
    return `<tr>
      <td><strong>${esc(g.name)}</strong>${g.description ? `<div class="muted">${esc(g.description)}</div>` : ''}</td>
      <td>${members || '<span class="muted">no members</span>'}${implicit}</td>
      <td><form class="addmember" data-group="${esc(g.name)}">
        <select name="user" required><option value="">user…</option>${userOptions}</select>
        <select name="direction"><option value="both">in + out</option><option value="in">in (receive)</option><option value="out">out (send)</option></select>
        <button class="small" type="submit">Add</button>
      </form></td>
      <td>${g.online}</td>
      <td>${esc(g.created)}</td>
      <td class="actions">
        <button class="small" data-act="describe" data-group="${esc(g.name)}" data-desc="${esc(g.description)}">Description</button>
        ${g.builtin ? '' : `<button class="small" data-act="rename" data-group="${esc(g.name)}">Rename</button>
        <button class="small danger" data-act="delete" data-group="${esc(g.name)}">Delete</button>`}
      </td></tr>`;
  }).join('') || emptyRow(6, 'No groups');
}

$('#group-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  await api('POST', 'api/groups', { name: f.get('name'), description: f.get('description') });
  e.target.reset();
  toast('Group created');
  loadGroups();
}));

$('#groups-table').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const form = e.target.closest('form.addmember');
  const f = new FormData(form);
  if (!f.get('user')) return;
  await api('PUT', `api/groups/${encodeURIComponent(form.dataset.group)}/members/${encodeURIComponent(f.get('user'))}`, { direction: f.get('direction') });
  toast(`${f.get('user')} added to ${form.dataset.group}`);
  loadGroups();
}));

$('#groups-table').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button');
  if (!b || b.type === 'submit') return;
  const group = b.dataset.group;
  const path = `api/groups/${encodeURIComponent(group)}`;
  if (b.dataset.remove) {
    await api('DELETE', `${path}/members/${encodeURIComponent(b.dataset.remove)}`);
    toast(`${b.dataset.remove} removed from ${group}`);
  } else if (b.dataset.act === 'describe') {
    const d = prompt(`Description for ${group}`, b.dataset.desc);
    if (d === null) return;
    await api('PATCH', path, { description: d });
  } else if (b.dataset.act === 'rename') {
    const n = prompt(`New name for ${group}. Members, data packages and missions follow the rename.`, group);
    if (!n || n === group) return;
    await api('PATCH', path, { name: n });
    toast('Group renamed');
  } else if (b.dataset.act === 'delete') {
    if (!confirm(`Delete group ${group}? Its members lose access to what it shares.`)) return;
    await api('DELETE', path);
    toast('Group deleted');
  } else {
    return;
  }
  loadGroups();
}));

// ---------- certificates ----------

let certStatus = null;
let certCheckedAt = 0;

// Certificate warnings at the top of the Dashboard, with a way to the details.
async function dashCertIssues() {
  certCheckedAt = Date.now();
  const st = await api('GET', 'api/certs/status');
  certBadge(st);
  const issues = st.issues.filter((i) => i.level !== 'info');
  $('#dash-issues').innerHTML = issues.length ? issueList(issues) + '<p><button type="button" class="small" data-goto="certs">Open Certificates</button></p>' : '';
}

$('#dash-issues').addEventListener('click', (e) => {
  if (e.target.dataset.goto) selectTab(e.target.dataset.goto);
});

const CERT_PILL = { valid: 'ok', expiring: 'warn', expired: 'bad', revoked: 'bad', pending: '' };

function daysLeftText(d) {
  if (d < 0) return `expired ${plural(-d, 'day')} ago`;
  if (d === 0) return 'expires today';
  return `${plural(d, 'day')} left`;
}

// The server certificate can cover a dozen addresses; show a few, the rest on hover.
function hostList(hosts) {
  if (!hosts?.length) return '';
  const more = hosts.length > 4 ? ` <span title="${esc(hosts.slice(4).join('\n'))}">+${hosts.length - 4} more</span>` : '';
  return `<div class="muted mono">${esc(hosts.slice(0, 4).join(', '))}${more}</div>`;
}

const issueList = (issues) => issues.map((i) => `<div class="issue ${i.level}">${statusBadge(i.level)} <span>${esc(i.text)}</span></div>`).join('');

// The Certificates tab shows how many certificates need attention.
function certBadge(st) {
  const n = st.report.summary.attention || 0;
  const serious = st.issues.some((i) => i.level !== 'info');
  const tab = $('#tabs [data-tab="certs"]');
  let b = tab.querySelector('.tab-badge');
  if (!serious) { b?.remove(); return; }
  if (!b) { b = document.createElement('span'); b.className = 'tab-badge'; tab.append(b); }
  const server = st.report.server.filter((c) => c.state === 'expiring' || c.state === 'expired').length;
  b.textContent = n + server;
  b.title = 'Certificates that need attention';
}

async function loadCerts() {
  certStatus = await api('GET', 'api/certs/status');
  const rep = certStatus.report;
  certBadge(certStatus);
  $('#cert-issues').innerHTML = issueList(certStatus.issues);
  const form = $('#cert-warn-form');
  if (document.activeElement !== form.warn_days) form.warn_days.value = rep.warn_days;

  // Cells with several parts get one wrapper so phone layouts (one flex row per cell) keep them together.
  $('#server-certs-table tbody').innerHTML = rep.server.map((c) => `<tr>
      <td><div><strong>${esc(c.name)}</strong>${c.subject ? `<div class="muted">${esc(c.subject)}</div>` : ''}${hostList(c.hosts)}</div></td>
      <td>${esc(c.kind === 'ca' ? c.ports : c.ports ? 'ports ' + c.ports : '—')}</td>
      <td>${c.expires ? esc(new Date(c.expires).toLocaleDateString()) : '—'}</td>
      <td><div><span class="pill ${CERT_PILL[c.state]}">${esc(c.state)}</span>${c.expires ? `<div class="muted">${esc(daysLeftText(c.days_left))}</div>` : '<div class="muted">waiting for Let\'s Encrypt</div>'}</div></td>
      <td class="renewal"><div>${c.auto_renew ? '<span class="pill ok">automatic</span> ' : '<span class="pill">manual</span> '}<span class="muted">${esc(c.renewal)}</span></div></td>
    </tr>`).join('');

  const s = rep.summary;
  const tiles = [['Needs attention', s.attention || 0, 'attention'], ['Valid', s.valid || 0, 'valid'], [`Expiring (${rep.warn_days} days)`, s.expiring || 0, 'expiring'],
    ['Expired', s.expired || 0, 'expired'], ['Revoked', s.revoked || 0, 'revoked'], ['Replaced', s.replaced || 0, '']];
  $('#cert-tiles').innerHTML = tiles.map(([k, v, f]) => `<div class="stat${f === 'attention' && v ? ' alert' : ''}"><div class="v">${v}</div><div class="k">${esc(k)}</div></div>`).join('');
  renderCertTable();
}

function renderCertTable() {
  if (!certStatus) return;
  const filter = prefs.get('certs.filter', 'all');
  const hideReplaced = prefs.get('certs.hideReplaced', false);
  for (const b of $('#cert-filter').children) b.classList.toggle('active', b.dataset.filter === filter);
  $('#cert-hide-replaced').checked = hideReplaced;
  const certs = certStatus.report.certs.filter((c) => (filter === 'all' || (filter === 'attention' ? c.attention : c.state === filter))
    && !(hideReplaced && c.replaced && !c.in_use?.length));
  $('#certs-table tbody').innerHTML = certs.map((c) => {
    const notes = [c.replaced ? '<span class="pill" title="The same user and device enrolled again since">replaced</span>' : '',
      c.attention ? `<span class="status-icon ${c.state === 'expired' ? 'critical' : 'warning'}" title="Needs attention">${STATUS[c.state === 'expired' ? 'critical' : 'warning'][0]}</span>` : ''].join(' ');
    return `<tr class="${c.replaced && !c.attention ? 'dim' : ''}">
      <td><strong>${esc(c.common_name)}</strong></td>
      <td class="mono">${esc(c.client_uid || '—')}</td>
      <td class="mono" title="${esc(c.serial)}">${esc(c.serial.slice(0, 12))}…</td>
      <td>${esc(date(c.issued))}</td>
      <td><div>${esc(new Date(c.expires).toLocaleDateString())}${c.revoked ? '' : `<div class="muted">${esc(daysLeftText(c.days_left))}</div>`}</div></td>
      <td><div><span class="pill ${CERT_PILL[c.state]}">${esc(c.state)}</span> ${notes}</div></td>
      <td>${c.in_use?.length ? `<div><span class="pill ok">connected</span> ${esc(c.in_use.join(', '))}</div>` : '<span class="muted">—</span>'}</td>
      <td class="actions">${c.revoked ? '' : `<button class="small danger" data-revoke="${esc(c.serial)}">Revoke</button>`}</td>
    </tr>`;
  }).join('') || emptyRow(8, certStatus.report.certs.length ? 'No certificates match' : 'No certificates issued yet');
}

$('#cert-filter').addEventListener('click', (e) => {
  const f = e.target.closest('button')?.dataset.filter;
  if (!f) return;
  prefs.set('certs.filter', f);
  renderCertTable();
});
$('#cert-hide-replaced').addEventListener('change', (e) => { prefs.set('certs.hideReplaced', e.target.checked); renderCertTable(); });

$('#cert-warn-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  await api('PUT', 'api/certs/settings', { warn_days: Number(e.target.warn_days.value) });
  toast('Warning period saved');
  loadCerts();
}));

$('#certs-table').addEventListener('click', guard(async (e) => {
  const serial = e.target.dataset.revoke;
  if (!serial || !confirm('Revoke this certificate? Devices using it are disconnected and must re-enroll.')) return;
  await api('POST', `api/certs/${serial}/revoke`);
  toast('Certificate revoked');
  loadCerts();
}));

// ---------- chat log ----------

const CHAT_KIND = { all: 'All', team: 'Team', group: 'Group', direct: 'Direct' };
const roomLabel = (kind, room) => (kind === 'all' ? room : `${CHAT_KIND[kind] || kind}: ${room}`);
let chatLimit = 200;
let chatFilterTimer = null;
let chatStatus = null;
let chatRendered = ''; // what the list shows, to skip redrawing it unchanged

// Query string for the current filters (shared by the list and exports).
function chatParams() {
  const f = $('#chat-filter');
  const p = new URLSearchParams();
  for (const k of ['q', 'room', 'from']) if (f[k].value.trim()) p.set(k, f[k].value.trim());
  const range = f.range.value;
  if (range === 'custom') {
    if (f.since.value) p.set('since', new Date(f.since.value).toISOString());
    if (f.until.value) p.set('until', new Date(f.until.value).toISOString());
  } else if (Number(range) > 0) {
    p.set('since', new Date(Date.now() - Number(range) * 3600e3).toISOString());
  }
  return p;
}

function highlight(text, q) {
  if (!q) return esc(text);
  const re = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'gi');
  let out = '', last = 0;
  for (const m of text.matchAll(re)) {
    out += esc(text.slice(last, m.index)) + '<mark>' + esc(m[0]) + '</mark>';
    last = m.index + m[0].length;
  }
  return out + esc(text.slice(last));
}

function fillSelect(sel, items, first) {
  if (document.activeElement === sel) return; // refreshing would close an open list
  const cur = sel.value;
  sel.innerHTML = `<option value="">${esc(first)}</option>` + items.join('');
  sel.value = [...sel.options].some((o) => o.value === cur) ? cur : '';
}

async function loadChat() {
  const [st, rooms] = await Promise.all([api('GET', 'api/chat/settings'), api('GET', 'api/chat/rooms')]);
  chatStatus = st;
  $('#chat-enabled').checked = st.enabled;
  $('#chat-off').hidden = st.enabled;
  const f = $('#chat-filter');
  fillSelect(f.room, rooms.rooms.map((r) => `<option value="${esc(r.room_id)}">${esc(roomLabel(r.kind, r.room))} (${r.messages})</option>`), 'All conversations');
  fillSelect(f.from, rooms.senders.map((s) => `<option value="${esc(s.uid)}">${esc(s.callsign || s.uid)} (${s.messages})</option>`), 'Anyone');
  await loadChatMessages();
}

async function loadChatMessages() {
  const p = chatParams();
  const q = p.get('q') || '';
  p.set('limit', chatLimit);
  const res = await api('GET', 'api/chat?' + p);
  const key = p.get('q') + p.get('room') + p.get('from') + p.get('limit') + chatStatus?.bytes + JSON.stringify(res); // not "since": it moves with the clock
  if (key === chatRendered) return; // unchanged: keep selections and scroll as they are
  chatRendered = key;
  const st = chatStatus || {};
  const total = res.total;
  $('#chat-count').textContent = total ? `${plural(total, 'message')}${res.messages.length < total ? `, newest ${res.messages.length} shown` : ''}`
    + (st.bytes != null ? ` · ${fmtSize(st.bytes)} on disk` : '') : '';
  $('#chat-more').hidden = res.messages.length >= total;
  let day = '';
  const rows = [];
  for (const m of res.messages) {
    const d = new Date(m.time);
    const dayText = d.toLocaleDateString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric' });
    if (dayText !== day) { day = dayText; rows.push(`<div class="chat-day">${esc(dayText)}</div>`); }
    rows.push(`<div class="chat-msg">
      <time datetime="${esc(m.time)}" title="${esc(d.toLocaleString())}">${esc(d.toLocaleTimeString())}</time>
      <div class="chat-body">
        <div class="chat-meta">
          <button type="button" class="linkish" data-chat-from="${esc(m.from_uid)}" title="Only messages from ${esc(m.from_callsign || m.from_uid)}"><strong>${highlight(m.from_callsign || m.from_uid, q)}</strong></button>
          ${m.username ? `<span class="muted">${highlight(m.username, q)}${m.console ? ' (console)' : ''}</span>` : ''}
          <span class="muted">→</span>
          <button type="button" class="pill chat-room ${esc(m.kind)}" data-chat-room="${esc(m.room_id)}" title="Only this conversation">${m.kind === 'all' ? '' : esc(CHAT_KIND[m.kind] || m.kind) + ': '}${highlight(m.room, q)}</button>
        </div>
        <div class="chat-text">${highlight(m.text, q) || '<span class="muted">(empty)</span>'}</div>
      </div>
    </div>`);
  }
  $('#chat-list').innerHTML = rows.join('') || `<p class="muted chat-empty">${total === 0 && !p.get('q') && !p.get('room') && !p.get('from') ? 'No messages recorded yet.' : 'No messages match.'}</p>`;
  for (const fmt of ['csv', 'json']) {
    const exp = chatParams();
    exp.set('format', fmt);
    $(`#chat-export-${fmt}`).href = 'api/chat/export?' + exp;
  }
}

$('#chat-filter').addEventListener('input', (e) => {
  const f = e.currentTarget;
  $('.chat-custom').hidden = f.range.value !== 'custom';
  chatLimit = 200;
  clearTimeout(chatFilterTimer);
  chatFilterTimer = setTimeout(guard(() => loadChatMessages()), e.target.name === 'q' ? 300 : 0);
});
$('#chat-filter').addEventListener('submit', (e) => e.preventDefault());

$('#chat-list').addEventListener('click', (e) => {
  const b = e.target.closest('[data-chat-room], [data-chat-from]');
  if (!b) return;
  const f = $('#chat-filter');
  if (b.dataset.chatRoom != null) f.room.value = b.dataset.chatRoom;
  if (b.dataset.chatFrom != null) f.from.value = b.dataset.chatFrom;
  chatLimit = 200;
  guard(() => loadChatMessages())();
});

$('#chat-more').addEventListener('click', guard(async () => {
  chatLimit = Math.min(chatLimit * 2, 5000);
  await loadChatMessages();
}));

$('#chat-enabled').addEventListener('change', guard(async (e) => {
  const on = e.target.checked;
  if (on && !confirm('Record chat messages? Admins can then read every GeoChat message sent through this server, including one-to-one messages. Let your users know.')) {
    e.target.checked = false;
    return;
  }
  await api('PUT', 'api/chat/settings', { enabled: on });
  toast(on ? 'Chat recording on' : 'Chat recording off');
  loadChat();
}));

$('#chat-clear').addEventListener('click', guard(async () => {
  if (!confirm('Delete every recorded chat message? This cannot be undone. Export first if you need a copy.')) return;
  await api('DELETE', 'api/chat');
  toast('Chat log cleared');
  loadChat();
}));

// ---------- packages ----------

async function loadPackages() {
  const pkgs = await api('GET', 'api/packages');
  packagesByHash = Object.fromEntries(pkgs.map((p) => [p.hash, p]));
  const onMap = (h) => packageLayers.has(h) && map?.hasLayer(packageLayers.get(h).layer);
  $('#packages-table tbody').innerHTML = pkgs.map((p) => `<tr>
    <td><strong>${esc(p.name)}</strong><div class="muted mono" title="${esc(p.hash)}">${esc(p.hash.slice(0, 16))}…</div></td>
    <td>${bytes(p.size)}</td>
    <td>${esc(p.submitter)}${p.creator_uid ? `<div class="muted mono">${esc(p.creator_uid)}</div>` : ''}</td>
    <td>${groupPills(p.groups)}</td>
    <td>${p.tool === 'public' ? '<span class="pill ok">public</span>' : `<span class="pill">${esc(p.tool || 'private')}</span>`}</td>
    <td>${esc(date(p.submitted))}</td>
    <td class="actions">
      <button class="small" data-map="${esc(p.hash)}">${onMap(p.hash) ? 'Hide from map' : 'Show on map'}</button>
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
  const show = e.target.dataset.map;
  if (show) {
    const p = packageLayers.get(show);
    if (p && map?.hasLayer(p.layer)) forgetPackage(show);
    else await showPackage(show);
    loadPackages();
    return;
  }
  const hash = e.target.dataset.del;
  if (!hash || !confirm(`Delete ${e.target.dataset.name}?`)) return;
  forgetPackage(hash);
  await api('DELETE', `api/packages/${hash}`);
  toast('Package deleted');
  loadPackages();
}));

// ---------- video ----------

let videoStatus = null;
let editingFeed = null; // uid being edited, or null for a new feed

async function loadVideoFeeds() {
  videoStatus = await api('GET', 'api/video');
  renderVideoMarkers();
  return videoStatus;
}

async function loadVideo() {
  const [st, groups] = await Promise.all([loadVideoFeeds(), api('GET', 'api/groups'), loadMediaMTX().catch(() => {})]);
  allGroups = groups;
  renderAnon(st);
  // The tab refreshes every 5 s; the recordings folder is scanned less often.
  if (st.enabled && Date.now() - recCheckedAt > 15000) await loadRecordings().catch(() => {});
  const host = st.host;
  $('#video-server').innerHTML = !st.enabled
    ? `<h2>Media server</h2><p class="muted">The MediaMTX media server is not set up, so this server only lists external feeds (cameras or media servers with their own URL).
       To host streams here, reinstall with <code>--video</code>, which installs MediaMTX alongside EdgeTAK.</p>`
    : `<div class="card-head"><h2>Media server</h2>${st.mediamtx_error
      ? `<span class="pill bad" title="${esc(st.mediamtx_error)}">MediaMTX not reachable</span>` : '<span class="pill ok">MediaMTX connected</span>'}</div>
      <p class="muted">Publish to this server with an EdgeTAK username and password; pick any stream name. Live streams appear below automatically, visible to the publisher's groups.</p>
      <div class="publish-urls">
        <span class="muted">RTSP</span><code>rtsp://USER:PASSWORD@${esc(host)}:${st.rtsp_port}/NAME</code>
        <span class="muted">RTMP</span><code>rtmp://${esc(host)}:${st.rtmp_port}/NAME?user=USER&amp;pass=PASSWORD</code>
        <span class="muted">SRT</span><code>srt://${esc(host)}:${st.srt_port}?streamid=publish:NAME:USER:PASSWORD</code>
      </div>
      <p class="muted">ATAK UAS tool / DJI: use the RTMP or RTSP address. Viewers never need the password: their devices get personal links.</p>`;

  $('#video-table tbody').innerHTML = st.feeds.map((f) => {
    const rec = recStatus && !recStatus.paused && (recStatus.settings.mode === 'all' || (recStatus.settings.mode === 'feeds' && f.record));
    const status = f.hosted
      ? (f.live ? `<span class="pill ok">live</span>${rec ? ' <span class="pill bad" title="Being recorded">● rec</span>' : ''} <span class="muted">${f.readers} watching${f.tracks?.length ? ' · ' + esc(f.tracks.join(', ')) : ''}</span>`
        : `<span class="pill">offline</span>${f.record && recStatus?.settings.mode === 'feeds' ? ' <span class="muted">records when live</span>' : ''}`)
      : '<span class="pill">external</span>';
    const pos = f.lat != null ? `<span class="mono">${f.lat.toFixed(5)}, ${f.lon.toFixed(5)}</span>` : '<span class="muted">—</span>';
    const anonOn = st.settings && st.settings.anon_publish !== 'off';
    const openNote = f.open_publish
      ? `<div class="muted">${anonOn ? '' : '<span class="pill warn" title="Turn on publishing without a login above">off</span> '}No-login publish: <span class="mono">rtsp://${esc(host)}:${st.rtsp_port}/${esc(f.path)}</span></div>` : '';
    const src = (f.hosted ? `server stream <span class="mono">${esc(f.path)}</span>${f.publisher ? ` · from ${esc(f.publisher)}` : (f.live ? ' · no login' : '')}` : `<span class="mono">${esc(redactURL(f.url))}</span>`) + openNote;
    return `<tr>
      <td><strong>${esc(f.alias)}</strong><div class="muted">${src}</div></td>
      <td>${status}</td>
      <td>${groupPills(f.groups)}</td>
      <td>${pos}</td>
      <td class="actions">
        ${f.hosted && f.live ? `<button class="small primary" data-act="watch" data-uid="${esc(f.uid)}">Watch</button>` : ''}
        ${f.saved ? `<button class="small" data-act="edit" data-uid="${esc(f.uid)}">Edit</button>
          <button class="small danger" data-act="delete" data-uid="${esc(f.uid)}">Delete</button>`
        : `<button class="small" data-act="save" data-uid="${esc(f.uid)}" title="Name it, choose who can watch and give it a position">Save as feed</button>`}
      </td></tr>`;
  }).join('') || emptyRow(5, st.enabled ? 'No feeds yet. Publish a stream or add an external camera.' : 'No feeds yet');
}

// ---- MediaMTX process (when EdgeTAK runs it) ----

let mtx = null;
let mtxChecked = false;

async function loadMediaMTX() {
  mtx = await api('GET', 'api/video/mediamtx');
  const card = $('#mediamtx-card');
  card.hidden = !mtx.managed;
  if (!mtx.managed) return;
  if (!mtxChecked && !mtx.latest) { // look for updates once per visit
    mtxChecked = true;
    api('POST', 'api/video/mediamtx/check').then(() => loadMediaMTX()).catch(() => {});
  }
  const u = mtx.update;
  $('#mediamtx-state').innerHTML = u.running ? '<span class="pill warn">updating</span>'
    : mtx.running && mtx.version ? '<span class="pill ok">running</span>' : mtx.running ? '<span class="pill warn">starting</span>' : '<span class="pill bad">stopped</span>';
  const info = [
    ['Version', mtx.version || 'unknown'],
    ['Latest release', mtx.latest ? `${mtx.latest.version} (${new Date(mtx.latest.published).toLocaleDateString()})` : 'not checked yet'],
    ['Running since', mtx.started ? new Date(mtx.started).toLocaleString() : '—'],
    ['Restarts', String(mtx.restarts)],
  ];
  if (mtx.last_exit) info.push(['Last exit', mtx.last_exit]);
  $('#mediamtx-info').innerHTML = info.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');

  $('#mediamtx-update-status').innerHTML = u.running ? `Updating: ${esc(u.step)}…`
    : u.error ? `<span class="error">${esc(u.error)}</span>`
    : u.result ? `<span class="pill ok">done</span> ${esc(u.result)}` : '';
  const up = $('#mediamtx-update');
  up.hidden = !mtx.update_available || u.running;
  if (mtx.latest) up.textContent = `Update to ${mtx.latest.version}`;
  const rb = $('#mediamtx-rollback');
  rb.hidden = !mtx.previous || u.running;
  rb.textContent = `Roll back to ${mtx.previous}`;
  for (const b of document.querySelectorAll('#mediamtx-actions button')) b.disabled = u.running;
  const notes = $('#mediamtx-notes');
  notes.hidden = !mtx.latest?.notes;
  if (mtx.latest) {
    notes.querySelector('pre').textContent = mtx.latest.notes;
    notes.querySelector('summary').innerHTML = `Release notes for ${esc(mtx.latest.version)} · <a href="${esc(mtx.latest.url)}" target="_blank" rel="noopener">GitHub</a>`;
  }
  const log = $('#mediamtx-log');
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 4;
  log.textContent = mtx.log.join('\n') || '(no output yet)';
  if (atBottom) log.scrollTop = log.scrollHeight;
}

$('#mediamtx-check').addEventListener('click', guard(async () => {
  await api('POST', 'api/video/mediamtx/check');
  await loadMediaMTX();
  toast(mtx.update_available ? `MediaMTX ${mtx.latest.version} is available` : `MediaMTX ${mtx.version} is the latest version`);
}));

$('#mediamtx-update').addEventListener('click', guard(async () => {
  const v = mtx.latest.version;
  if (!confirm(`Update MediaMTX ${mtx.version} to ${v}?\n\nLive streams drop for a few seconds while it restarts. If ${v} does not start, EdgeTAK goes back to ${mtx.version} automatically.`)) return;
  await api('POST', 'api/video/mediamtx/update', { version: v });
  toast(`Updating MediaMTX to ${v}`);
  await loadMediaMTX();
}));

$('#mediamtx-rollback').addEventListener('click', guard(async () => {
  if (!confirm(`Go back to MediaMTX ${mtx.previous}? Live streams drop for a few seconds.`)) return;
  await api('POST', 'api/video/mediamtx/rollback');
  toast(`Rolled back to ${mtx.previous}`);
  await loadMediaMTX();
}));

$('#mediamtx-restart').addEventListener('click', guard(async () => {
  if (!confirm('Restart MediaMTX? Live streams drop for a few seconds.')) return;
  await api('POST', 'api/video/mediamtx/restart');
  toast('MediaMTX restarting');
  setTimeout(() => loadMediaMTX().catch(() => {}), 2000);
}));

const redactURL = (u) => String(u || '').replace(/\/\/([^:/@]+):[^@]*@/, '//$1:•••@');
const feedByUID = (uid) => videoStatus?.feeds.find((f) => f.uid === uid);

function openFeedDialog(f) {
  editingFeed = f?.saved ? f.uid : null;
  const form = $('#feed-form');
  form.reset();
  $('#feed-dialog-title').textContent = f?.saved ? `Edit ${f.alias}` : 'Add feed';
  form.alias.value = f?.alias || '';
  const kind = f && !f.hosted ? 'url' : 'path';
  form.kind.value = kind;
  form.path.value = f?.hosted ? f.path : '';
  form.url.value = f && !f.hosted ? f.url : '';
  form.lat.value = f?.lat ?? '';
  form.lon.value = f?.lon ?? '';
  form.open_publish.checked = !!f?.open_publish;
  form.record.checked = !!f?.record;
  syncFeedKind();
  renderChips('#feed-groups', f?.groups || []);
  $('#feed-dialog').showModal();
}

function syncFeedKind() {
  const url = $('#feed-form').kind.value === 'url';
  $('#feed-url-label').hidden = !url;
  $('#feed-path-label').hidden = url;
  $('#feed-open-label').hidden = url;
  $('#feed-record-label').hidden = url;
}

// ---- publishing without a login ----

let anonEditing = false; // don't overwrite the form while the admin is changing it

function renderAnon(st) {
  $('#anon-card').hidden = !st.enabled;
  if (!st.enabled || anonEditing) return;
  const s = st.settings || { anon_publish: 'off', anon_groups: [] };
  for (const r of document.querySelectorAll('#anon-form input[name="anon"]')) r.checked = r.value === s.anon_publish;
  renderChips('#anon-groups', s.anon_groups || []);
  $('#anon-groups-wrap').hidden = s.anon_publish !== 'any';
  $('#anon-url').textContent = `rtsp://${st.host}:${st.rtsp_port}/NAME`;
}

$('#anon-form').addEventListener('change', (e) => {
  anonEditing = true;
  if (e.target.name === 'anon') $('#anon-groups-wrap').hidden = e.target.value !== 'any';
});

$('#anon-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const mode = new FormData(e.target).get('anon') || 'off';
  if (mode === 'any' && !confirm('Let anyone who can reach this server publish video under any stream name?\n\nThe safer choice is "Only to feeds marked …", which limits it to stream names you saved.')) return;
  await api('PUT', 'api/video/settings', { anon_publish: mode, anon_groups: checkedIn('#anon-groups') });
  anonEditing = false;
  toast('Saved');
  loadVideo();
}));
for (const r of document.querySelectorAll('#feed-form input[name="kind"]')) r.addEventListener('change', syncFeedKind);

$('#video-add').addEventListener('click', () => openFeedDialog(null));

$('#feed-map-center').addEventListener('click', () => {
  initMap();
  if (!map) return;
  const c = map.getCenter();
  $('#feed-form').lat.value = c.lat.toFixed(6);
  $('#feed-form').lon.value = c.lng.toFixed(6);
});

$('#feed-form').addEventListener('submit', guard(async (e) => {
  if (e.submitter?.value !== 'save') return;
  e.preventDefault();
  const f = e.target;
  const num = (v) => (v === '' ? null : Number(v));
  const body = {
    alias: f.alias.value, groups: checkedIn('#feed-groups'), lat: num(f.lat.value), lon: num(f.lon.value),
    ...(f.kind.value === 'url' ? { url: f.url.value } : { path: f.path.value, open_publish: f.open_publish.checked, record: f.record.checked }),
  };
  if ((body.lat == null) !== (body.lon == null)) throw new Error('Enter both latitude and longitude, or neither');
  await api(editingFeed ? 'PUT' : 'POST', editingFeed ? `api/video/feeds/${encodeURIComponent(editingFeed)}` : 'api/video/feeds', body);
  $('#feed-dialog').close();
  toast('Feed saved');
  loadVideo();
}));

$('#video-table').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  const f = feedByUID(b.dataset.uid);
  switch (b.dataset.act) {
    case 'watch': return playFeed(f);
    case 'edit': return openFeedDialog(f);
    case 'save': return openFeedDialog({ ...f, saved: false, alias: f.alias });
    case 'delete':
      if (!confirm(`Delete the feed ${f.alias}? Devices lose it from their list.`)) return;
      await api('DELETE', `api/video/feeds/${encodeURIComponent(f.uid)}`);
      toast('Feed deleted');
      return loadVideo();
  }
}));

// ---- recording ----

let recStatus = null;
let recCheckedAt = 0;
let recEditing = false; // don't overwrite the form while the admin is changing it

function fmtDuration(sec) {
  sec = Math.round(sec || 0);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return h ? `${h} h ${String(m).padStart(2, '0')} min` : m ? `${m} min ${String(s).padStart(2, '0')} s` : `${s} s`;
}

const segURL = (stream, file, download) => `video/recordings/file?${new URLSearchParams({ path: stream, file, ...(download ? { download: '1' } : {}) })}`;

async function loadRecordings() {
  recCheckedAt = Date.now();
  recStatus = await api('GET', 'api/video/recordings');
  const r = recStatus;
  $('#record-card').hidden = !r.enabled;
  $('#recordings-card').hidden = !r.enabled || (!r.streams.length && r.settings.mode === 'off');
  $('#record-seg-min').textContent = r.segment_minutes;
  $('#record-state').innerHTML = r.paused ? '<span class="pill bad">paused: disk nearly full</span>'
    : r.settings.mode === 'off' ? '<span class="pill">off</span>'
    : `<span class="pill ok">${r.settings.mode === 'all' ? 'every stream' : 'saved feeds'}</span>`;
  if (!recEditing) {
    const f = $('#record-form');
    for (const el of f.querySelectorAll('input[name="mode"]')) el.checked = el.value === r.settings.mode;
    f.keep_days.value = r.settings.keep_days;
  }
  const disk = r.disk_total ? ` · disk ${Math.round((100 * r.disk_used) / r.disk_total)}% full (${fmtSize(r.disk_total - r.disk_used)} free)` : '';
  $('#record-usage').textContent = `Recordings use ${fmtSize(r.bytes)}${disk}.`;
  $('#recordings-total').textContent = r.streams.length ? `${plural(r.streams.reduce((n, s) => n + s.spans.length, 0), 'recording')} · ${fmtSize(r.bytes)}` : '';

  $('#recordings').innerHTML = r.streams.map((st) => `<div class="rec-stream">
      <div class="card-head"><h3>${esc(st.alias || st.path)}${st.alias ? ` <span class="muted mono">${esc(st.path)}</span>` : ''}</h3>
        <span><span class="muted">${fmtSize(st.bytes)}</span> <button type="button" class="small danger" data-rec-act="delete-all" data-path="${esc(st.path)}">Delete all</button></span></div>
      <div class="table-wrap"><table>
        <thead><tr><th>Started</th><th>Length</th><th>Size</th><th></th></tr></thead>
        <tbody>${st.spans.map((sp, i) => `<tr>
          <td>${esc(new Date(sp.start).toLocaleString())}${sp.recording ? ' <span class="pill bad">● recording</span>' : ''}</td>
          <td>${esc(fmtDuration(sp.duration))}${sp.segments.length > 1 ? ` <span class="muted">(${sp.segments.length} parts)</span>` : ''}</td>
          <td>${fmtSize(sp.bytes)}</td>
          <td class="actions">
            <button type="button" class="small primary" data-rec-act="play" data-path="${esc(st.path)}" data-i="${i}">Play</button>
            <a class="button small" href="video/recordings/export?${new URLSearchParams({ path: st.path, start: sp.start, duration: sp.duration })}" title="One MP4 file, made by the media server">Download</a>
            ${sp.recording ? '' : `<button type="button" class="small danger" data-rec-act="delete" data-path="${esc(st.path)}" data-i="${i}">Delete</button>`}
          </td></tr>`).join('')}</tbody>
      </table></div>
    </div>`).join('') || `<p class="muted">${r.settings.mode === 'off' ? 'Recording is off.' : 'No recordings yet. Streams are recorded while they are live.'}</p>`;
}

$('#record-form').addEventListener('input', () => { recEditing = true; });
$('#record-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = e.target;
  const mode = new FormData(f).get('mode') || 'off';
  await api('PUT', 'api/video/recording', { mode, keep_days: Number(f.keep_days.value) });
  recEditing = false;
  toast(mode === 'off' ? 'Recording is off' : 'Recording settings saved');
  recCheckedAt = 0;
  loadVideo();
}));

$('#recordings').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('[data-rec-act]');
  if (!b) return;
  const st = recStatus.streams.find((s) => s.path === b.dataset.path);
  const sp = st?.spans[Number(b.dataset.i)];
  const name = st?.alias || b.dataset.path;
  switch (b.dataset.recAct) {
    case 'play':
      return playRecording(st, sp);
    case 'delete':
      if (!confirm(`Delete the recording of ${name} from ${new Date(sp.start).toLocaleString()}? This cannot be undone.`)) return;
      await api('DELETE', `api/video/recordings?${new URLSearchParams({ path: st.path, start: sp.start })}`);
      break;
    case 'delete-all':
      if (!confirm(`Delete every recording of ${name}? This cannot be undone.`)) return;
      await api('DELETE', `api/video/recordings?${new URLSearchParams({ path: b.dataset.path })}`);
      break;
  }
  toast('Recording deleted');
  await loadRecordings();
}));

// A recording is played part by part (each part is one file, so seeking
// works within it); the next part starts when one ends.
let recQueue = null;

function playRecording(st, sp) {
  const video = $('#player');
  stopPlayer();
  const parts = sp.segments.slice().sort((a, b) => new Date(a.start) - new Date(b.start));
  recQueue = { st, parts, i: 0 };
  $('#player-title').textContent = `${st.alias || st.path} · ${new Date(sp.start).toLocaleString()}`;
  const list = $('#player-parts');
  list.hidden = parts.length < 2;
  list.innerHTML = parts.map((p, i) => `<button type="button" class="small" data-part="${i}">${esc(new Date(p.start).toLocaleTimeString())}</button>`).join('')
    + `<span class="spacer"></span><a class="button small" id="player-part-download" href="#">Download this part</a>`;
  if (parts.length < 2) list.hidden = true;
  $('#player-dialog').showModal();
  playPart(0);
  video.onended = () => { if (recQueue && recQueue.i < recQueue.parts.length - 1) playPart(recQueue.i + 1); };
}

function playPart(i) {
  const video = $('#player');
  const p = recQueue.parts[i];
  recQueue.i = i;
  video.src = segURL(recQueue.st.path, p.file);
  video.play().catch(() => {});
  const n = recQueue.parts.length;
  $('#player-status').textContent = `Recorded ${new Date(p.start).toLocaleString()} · ${n > 1 ? `part ${i + 1} of ${n} · ` : ''}${fmtDuration(p.duration)}${p.recording ? ' · still recording' : ''}`;
  for (const b of $('#player-parts').querySelectorAll('[data-part]')) b.classList.toggle('primary', Number(b.dataset.part) === i);
  const dl = $('#player-part-download');
  if (dl) dl.href = segURL(recQueue.st.path, p.file, true);
}

$('#player-parts').addEventListener('click', (e) => {
  const b = e.target.closest('[data-part]');
  if (b && recQueue) playPart(Number(b.dataset.part));
});

// ---- player (HLS through EdgeTAK's relay) ----

let hlsPlayer = null;
let hlsLoading = null;
function loadHls() {
  if (window.Hls) return Promise.resolve();
  hlsLoading ||= new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = 'vendor/hls.min.js';
    s.onload = resolve;
    s.onerror = () => { hlsLoading = null; reject(new Error('The video player could not be loaded')); };
    document.head.appendChild(s);
  });
  return hlsLoading;
}

async function playFeed(f) {
  if (!f?.hosted) throw new Error('Only streams on this server can be played in the console');
  const video = $('#player');
  stopPlayer();
  $('#player-title').textContent = f.alias;
  $('#player-status').textContent = 'Connecting…';
  $('#player-dialog').showModal();
  const src = `video/hls/${f.path.split('/').map(encodeURIComponent).join('/')}/index.m3u8`;
  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = src; // Safari plays HLS natively
  } else {
    await loadHls();
    if (!Hls.isSupported()) throw new Error('This browser cannot play HLS video');
    hlsPlayer = new Hls({ liveSyncDurationCount: 2, manifestLoadingMaxRetry: 6 });
    hlsPlayer.on(Hls.Events.ERROR, (_, d) => {
      if (d.fatal) $('#player-status').textContent = `Playback error: ${d.details}. The stream may have stopped, or use a codec browsers can't play (H.265).`;
    });
    hlsPlayer.loadSource(src);
    hlsPlayer.attachMedia(video);
  }
  video.onplaying = () => { $('#player-status').textContent = 'Live. Delay is a few seconds behind real time.'; };
  video.play().catch(() => {});
}

function stopPlayer() {
  const video = $('#player');
  hlsPlayer?.destroy();
  hlsPlayer = null;
  recQueue = null;
  video.onended = null;
  $('#player-parts').hidden = true;
  video.removeAttribute('src');
  video.load();
}
$('#player-dialog').addEventListener('close', stopPlayer);

// ---- camera markers on the map ----

function cameraIcon(live) {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="30" height="30" viewBox="0 0 30 30"><circle cx="15" cy="15" r="13" fill="${live ? '#1f8f5c' : '#5d6c72'}" stroke="#000" stroke-width="1.5"/>` +
    '<rect x="7" y="10" width="12" height="10" rx="2" fill="#fff"/><path d="M19 13.5l5-3v9l-5-3z" fill="#fff"/></svg>';
  return L.divIcon({ className: 'camera-icon', html: svg, iconSize: [30, 30], iconAnchor: [15, 15], tooltipAnchor: [14, 0], popupAnchor: [0, -14] });
}

function renderVideoMarkers() {
  if (!map || !videoLayer) return;
  videoLayer.clearLayers();
  for (const f of videoStatus?.feeds || []) {
    if (f.lat == null || f.lon == null) continue;
    const m = L.marker([f.lat, f.lon], { icon: cameraIcon(!f.hosted || f.live) })
      .bindTooltip(esc(f.alias), { permanent: true, direction: 'right' })
      .bindPopup(`<strong>${esc(f.alias)}</strong><br>${f.hosted ? (f.live ? 'Live' : 'Offline') : 'External camera'}` +
        (f.hosted && f.live ? `<br><button class="small primary" data-watch="${esc(f.uid)}">Watch</button>` : ''));
    m.addTo(videoLayer);
  }
}

document.addEventListener('click', guard(async (e) => {
  const uid = e.target.dataset?.watch;
  if (uid) await playFeed(feedByUID(uid));
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

// ---------- send to devices ----------

function sendScope() {
  return document.querySelector('input[name="send-scope"]:checked').value;
}

function syncSendScope() {
  const scope = sendScope();
  $('#send-groups').hidden = scope !== 'groups';
  $('#send-clients').hidden = scope !== 'clients';
}

let contactLoaded = false;

async function loadSendLive() {
  const [history, inbox] = await Promise.all([api('GET', 'api/send/history'), api('GET', 'api/send/inbox')]);
  $('#send-history tbody').innerHTML = history.map((h) => `<tr>
      <td title="${esc(date(h.time))}">${ago(h.time)}</td>
      <td><span class="pill">${esc(h.kind)}</span> ${esc(h.summary)}</td>
      <td>${esc(h.to)}</td><td>${h.delivered}</td><td>${esc(h.admin)}</td></tr>`).join('')
    || emptyRow(5, 'Nothing sent yet');
  $('#send-inbox tbody').innerHTML = inbox.map((m) => `<tr>
      <td title="${esc(date(m.time))}">${ago(m.time)}</td>
      <td><strong>${esc(m.from_callsign || m.from_uid)}</strong><div class="muted">${esc(m.username || 'anonymous')}</div></td>
      <td style="white-space: pre-wrap">${esc(m.text)}</td>
      <td class="actions"><button class="small" data-reply="${esc(m.from_uid)}" data-name="${esc(m.from_callsign)}">Reply</button></td></tr>`).join('')
    || emptyRow(4, 'No messages yet');
}

async function loadSend() {
  const [groups, clients, packages, contact] = await Promise.all([
    api('GET', 'api/groups'), api('GET', 'api/clients'), api('GET', 'api/packages'), api('GET', 'api/send/contact'),
  ]);
  if (!contactLoaded) {
    const f = $('#contact-form');
    f.enabled.checked = contact.enabled;
    f.callsign.value = contact.callsign;
    f.lat.value = contact.lat || '';
    f.lon.value = contact.lon || '';
    contactLoaded = true;
  }
  await loadSendLive();
  const keep = (sel) => new Set([...document.querySelectorAll(`${sel} input:checked`)].map((i) => i.value));
  const gKeep = keep('#send-groups');
  const cKeep = keep('#send-clients');
  $('#send-groups').innerHTML = groups.map((g) => `<label class="chip" title="${g.online} connected">
      <input type="checkbox" value="${esc(g.name)}"${gKeep.has(g.name) ? ' checked' : ''}>${esc(g.name)} <span class="muted">${g.online}</span></label>`).join('')
    || '<span class="muted">No groups</span>';
  $('#send-clients').innerHTML = clients.map((c) => `<label class="chip">
      <input type="checkbox" value="${c.id}"${cKeep.has(String(c.id)) ? ' checked' : ''}>${esc(c.callsign || '(no callsign)')}
      <span class="muted">${esc(c.username || 'anonymous')}</span></label>`).join('')
    || '<span class="muted">No devices connected</span>';
  const pkgSel = $('#send-package');
  const current = pkgSel.value;
  pkgSel.innerHTML = packages.map((p) => `<option value="${esc(p.hash)}"${p.hash === current ? ' selected' : ''}>${esc(p.name)} (${bytes(p.size)})</option>`).join('')
    || '<option value="">No data packages; upload one on the Data Packages tab</option>';
  if (!$('#send-from').value && status) $('#send-from').value = status.server_name;
  syncSendScope();
}

$('#send-direct').addEventListener('change', (e) => {
  // Direct messages always come from the server contact's callsign.
  $('#send-from').disabled = e.target.checked;
});

$('#contact-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = e.target;
  await api('PUT', 'api/send/contact', {
    enabled: f.enabled.checked, callsign: f.callsign.value, lat: Number(f.lat.value) || 0, lon: Number(f.lon.value) || 0,
  });
  toast(f.enabled.checked ? 'Server contact is on devices' : 'Server contact removed from devices');
  refreshContactMarker();
}));

$('#contact-here').addEventListener('click', () => {
  if (!navigator.geolocation) {
    toast('This browser cannot share its location', true);
    return;
  }
  navigator.geolocation.getCurrentPosition((p) => {
    $('#contact-form').lat.value = p.coords.latitude.toFixed(6);
    $('#contact-form').lon.value = p.coords.longitude.toFixed(6);
  }, (err) => toast(`Location unavailable: ${err.message}`, true));
});

$('#send-inbox').addEventListener('click', guard(async (e) => {
  const b = e.target.closest('button[data-reply]');
  if (!b) return;
  const text = prompt(`Reply to ${b.dataset.name}`);
  if (!text) return;
  await api('POST', 'api/send/reply', { uid: b.dataset.reply, text });
  toast(`Reply sent to ${b.dataset.name}`);
  loadSendLive();
}));

function sendTarget() {
  const scope = sendScope();
  const picked = (sel) => [...document.querySelectorAll(`${sel} input:checked`)].map((i) => i.value);
  if (scope === 'all') return { all: true };
  if (scope === 'groups') {
    const groups = picked('#send-groups');
    if (!groups.length) throw new Error('Pick at least one group');
    return { groups };
  }
  const clients = picked('#send-clients').map(Number);
  if (!clients.length) throw new Error('Pick at least one device');
  return { clients };
}

for (const r of document.querySelectorAll('input[name="send-scope"]')) r.addEventListener('change', syncSendScope);

$('#send-message-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const direct = $('#send-direct').checked;
  const res = await api('POST', 'api/send/message', { from: $('#send-from').value, text: f.get('text'), direct, to: sendTarget() });
  e.target.querySelector('textarea').value = '';
  toast(`${direct ? 'Direct message' : 'Message'} delivered to ${res.delivered} device(s)` + (res.skipped ? `; ${res.skipped} skipped (no position report yet)` : ''));
  loadSend();
}));

$('#send-package-form').addEventListener('submit', guard(async (e) => {
  e.preventDefault();
  const hash = $('#send-package').value;
  if (!hash) throw new Error('Upload a data package first');
  const res = await api('POST', 'api/send/package', { from: $('#send-from').value, hash, to: sendTarget() });
  toast(`Package offered to ${res.delivered} device(s)` + (res.skipped ? `; ${res.skipped} skipped (no download port for their connection)` : ''));
  loadSend();
}));

// ---------- connect ----------

// The user dropdown suits the transport: iTAK packages carry the user's
// certificate, so one is required; TCP is anonymous, so none applies.
function connectModeChanged() {
  const mode = $('#connect-mode').value, sel = $('#connect-user');
  sel.disabled = mode === 'tcp';
  sel.required = mode === 'itak';
  sel.options[0].textContent = mode === 'itak' ? 'Choose a user' : mode === 'tcp' ? 'Not used for TCP' : 'No username: they type it in the app';
  $('#connect-user-hint').textContent = mode === 'itak' ? 'The package includes a new certificate for this user, for one device.'
    : mode === 'tcp' ? 'TCP connections are anonymous.' : 'The package fills in the username; they still enter their password when the app enrolls.';
}
$('#connect-mode').addEventListener('change', connectModeChanged);

let connectPick = null;

async function loadConnect() {
  if (!$('#connect-host').value && status) $('#connect-host').value = status.public_host || location.hostname;
  const users = await api('GET', 'api/users');
  const sel = $('#connect-user');
  const keep = connectPick ?? sel.value;
  connectPick = null;
  sel.length = 1;
  for (const u of users.sort((a, b) => a.username.localeCompare(b.username))) {
    sel.add(new Option(u.username + (u.admin ? ' (admin)' : ''), u.username));
  }
  sel.value = users.some((u) => u.username === keep) ? keep : '';
  connectModeChanged();
  const url = status?.signin_url || location.origin + '/';
  $('#account-url').textContent = url;
  $('#account-url').href = url;
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

// On phones tables are shown as cards; label every cell with its column
// header so the CSS can show it.
function labelCells(table) {
  const heads = [...table.querySelectorAll('thead th')].map((th) => th.textContent.trim());
  for (const tr of table.querySelectorAll('tbody tr')) {
    [...tr.children].forEach((td, i) => {
      if (heads[i] && !td.hasAttribute('colspan') && !td.classList.contains('actions')) td.dataset.label = heads[i];
    });
  }
}
for (const table of document.querySelectorAll('table')) {
  const body = table.querySelector('tbody');
  if (body) new MutationObserver(() => labelCells(table)).observe(body, { childList: true });
}

// ---------- install as an app ----------
//
// Browsers only offer installation over a trusted certificate (port 8446
// with Let's Encrypt). Chrome and Edge raise beforeinstallprompt; iPhones
// and iPads install from Safari's Share menu.

let installPrompt = null;
const standalone = () => window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('sw.js').catch(() => { /* e.g. an untrusted certificate */ });
}
window.addEventListener('beforeinstallprompt', (e) => {
  e.preventDefault();
  installPrompt = e;
  $('#install-app').hidden = false;
});
window.addEventListener('appinstalled', () => {
  installPrompt = null;
  $('#install-app').hidden = true;
  toast('EdgeTAK is installed as an app');
});
if (isIOS && !standalone() && window.isSecureContext) $('#install-app').hidden = false;

$('#install-app').addEventListener('click', async () => {
  if (installPrompt) {
    installPrompt.prompt();
    const { outcome } = await installPrompt.userChoice;
    if (outcome === 'accepted') $('#install-app').hidden = true;
    installPrompt = null;
  } else if (isIOS) {
    toast('In Safari, tap Share, then "Add to Home Screen"');
  }
});

start();
