// A shared live map (/share/<token>): read-only positions of the devices the
// link covers, refreshed every few seconds, until the link ends.
(() => {
  const tr = (s) => (window.t ? window.t(s) : s);
  const $ = (s) => document.querySelector(s);
  const dataURL = location.pathname.replace(/\/+$/, '') + '/data';
  const TEAM_COLORS = {
    White: '#ffffff', Yellow: '#ffff00', Orange: '#ff7700', Magenta: '#ff00ff', Red: '#ff0000', Maroon: '#7f0000',
    Purple: '#7f007f', 'Dark Blue': '#00007f', Blue: '#0000ff', Cyan: '#00ffff', Teal: '#007f7f', Green: '#00ff00',
    'Dark Green': '#007f00', Brown: '#a0714f',
  };
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
  const esri = (n) => `https://server.arcgisonline.com/ArcGIS/rest/services/${n}/MapServer/tile/{z}/{y}/{x}`;
  const ESRI = 'Tiles &copy; Esri';

  const map = L.map('map', { zoomControl: true, worldCopyJump: true }).setView([20, 0], 2);
  const bases = {
    [tr('Streets')]: L.tileLayer(esri('World_Street_Map'), { maxNativeZoom: 19, maxZoom: 21, attribution: ESRI }),
    [tr('Satellite')]: L.tileLayer(esri('World_Imagery'), { maxNativeZoom: 19, maxZoom: 21, attribution: 'Imagery &copy; Esri, Maxar, Earthstar Geographics' }),
    [tr('Topo')]: L.tileLayer('https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png', { maxNativeZoom: 17, maxZoom: 21, attribution: 'Map data &copy; OpenStreetMap contributors, SRTM | style &copy; OpenTopoMap (CC-BY-SA)' }),
  };
  bases[tr('Streets')].addTo(map);
  L.control.layers(bases, null, { position: 'topright' }).addTo(map);
  L.control.scale().addTo(map);
  const layer = L.layerGroup().addTo(map);

  let fitted = false;
  let lastOK = 0;
  let timer = null;

  function ago(ms) {
    const s = Math.max(0, Math.round(ms / 1000));
    if (s < 60) return tr('{0} s ago').replace('{0}', s);
    const m = Math.round(s / 60);
    if (m < 60) return tr('{0} min ago').replace('{0}', m);
    return tr('{0} h ago').replace('{0}', Math.round(m / 60));
  }

  function draw(d) {
    document.title = d.name;
    $('#name').textContent = d.name;
    $('#expires').textContent = `${tr('Link ends')} ${new Date(d.expires).toLocaleString()}`;
    $('#count').textContent = d.devices.length === 1 ? tr('1 unit') : tr('{0} units').replace('{0}', d.devices.length);
    $('#empty').hidden = d.devices.length > 0;
    layer.clearLayers();
    const pts = [];
    for (const u of d.devices) {
      const colour = TEAM_COLORS[u.team] || '#2f80ed';
      const m = L.circleMarker([u.lat, u.lon], { radius: 8, weight: 2, color: '#222', fillColor: colour, fillOpacity: 0.95 });
      if (u.callsign) m.bindTooltip(esc(u.callsign), { permanent: true, direction: 'right', offset: [8, 0], className: 'unit-label' });
      const lines = [u.callsign ? `<strong>${esc(u.callsign)}</strong>` : ''];
      if (u.team) lines.push(esc(u.team));
      if (u.speed > 0.5) lines.push(`${Math.round(u.speed * 3.6)} km/h`);
      lines.push(`${tr('Last report')}: ${ago(d.now - u.seen)}`);
      m.bindPopup(lines.filter(Boolean).join('<br>'));
      m.addTo(layer);
      pts.push([u.lat, u.lon]);
    }
    if (!fitted && pts.length) {
      fitted = true;
      if (pts.length === 1) map.setView(pts[0], 15);
      else map.fitBounds(pts, { padding: [40, 40], maxZoom: 16 });
    }
  }

  async function poll() {
    try {
      const r = await fetch(dataURL, { cache: 'no-store' });
      if (r.status === 410 || r.status === 404) {
        clearInterval(timer);
        $('#ended').hidden = false;
        return;
      }
      if (!r.ok) throw new Error(r.status);
      draw(await r.json());
      lastOK = Date.now();
    } catch {
      // Keep the last positions; the status line shows they're getting old.
    }
    const live = $('#live');
    const stale = !lastOK || Date.now() - lastOK > 20000;
    live.classList.toggle('stale', stale);
    live.textContent = lastOK ? `${stale ? tr('Reconnecting…') : tr('Live')} · ${tr('updated')} ${new Date(lastOK).toLocaleTimeString()}` : tr('Connecting…');
  }

  poll();
  timer = setInterval(poll, 5000);
})();
