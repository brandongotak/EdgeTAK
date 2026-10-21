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
  clearInterval(mapTimer);
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
  // The map is open by default beside the tabs; on phones it opens on demand.
  setMapVisible(!isNarrow() && prefs.get('map.open', true), false);
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
  groups: loadGroups,
  signups: loadSignups,
  certs: loadCerts,
  packages: loadPackages,
  missions: loadMissions,
  video: loadVideo,
  send: loadSend,
  connect: loadConnect,
};

let currentTab = '';

function selectTab(name) {
  if (!loaders[name]) name = 'dashboard';
  currentTab = name;
  history.replaceState(null, '', '#' + name);
  for (const b of $('#tabs').children) b.classList.toggle('active', b.dataset.tab === name);
  for (const s of document.querySelectorAll('.tab')) s.hidden = s.id !== 'tab-' + name;
  clearInterval(pollTimer);
  guard(loaders[name])();
  if (name === 'dashboard') {
    pollTimer = setInterval(guard(loadDashboard), 3000);
  } else if (name === 'video') {
    pollTimer = setInterval(() => loadVideo().catch(() => {}), 5000); // live streams come and go
  } else if (name === 'send') {
    pollTimer = setInterval(guard(loadSendLive), 5000); // Inbox updates
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

async function loadUsers() {
  const [users, groups] = await Promise.all([api('GET', 'api/users'), api('GET', 'api/groups')]);
  allGroups = groups;
  usersByName = Object.fromEntries(users.map((u) => [u.username, u]));
  renderNewUserPicker();
  $('#users-table tbody').innerHTML = users.map((u) => `<tr>
    <td><strong>${esc(u.username)}</strong></td>
    <td>${userGroupPills(u)}</td>
    <td>${u.admin ? '<span class="pill ok">admin</span>' : 'user'}</td>
    <td>${u.online ? `<span class="pill ok">${u.online} online</span>` : '<span class="muted">offline</span>'}</td>
    <td>${esc(date(u.created))}</td>
    <td class="actions">
      <button class="small" data-act="groups" data-user="${esc(u.username)}">Groups</button>
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
    const status = f.hosted
      ? (f.live ? `<span class="pill ok">live</span> <span class="muted">${f.readers} watching${f.tracks?.length ? ' · ' + esc(f.tracks.join(', ')) : ''}</span>` : '<span class="pill">offline</span>')
      : '<span class="pill">external</span>';
    const pos = f.lat != null ? `<span class="mono">${f.lat.toFixed(5)}, ${f.lon.toFixed(5)}</span>` : '<span class="muted">—</span>';
    const src = f.hosted ? `server stream <span class="mono">${esc(f.path)}</span>${f.publisher ? ` · from ${esc(f.publisher)}` : ''}` : `<span class="mono">${esc(redactURL(f.url))}</span>`;
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
  syncFeedKind();
  renderChips('#feed-groups', f?.groups || []);
  $('#feed-dialog').showModal();
}

function syncFeedKind() {
  const url = $('#feed-form').kind.value === 'url';
  $('#feed-url-label').hidden = !url;
  $('#feed-path-label').hidden = url;
}
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
    ...(f.kind.value === 'url' ? { url: f.url.value } : { path: f.path.value }),
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

// The version endpoint needs no login, so the sign-in page can show it too.
fetch('/Marti/api/version').then((r) => (r.ok ? r.text() : '')).then((v) => {
  $('#login-version').textContent = v;
}).catch(() => {});

start();
