// Applies the theme chosen with the console's theme button (Auto follows
// the device's light or dark setting). Loaded in <head>, before the page is
// drawn, by every EdgeTAK page.
(() => {
  try {
    const t = localStorage.getItem('edgetak.theme');
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  } catch { /* storage blocked: Auto */ }
})();

// showLogo puts the server's logo (its version from the page's info, ""
// for none) in place of the ◆ mark.
function showLogo(version) {
  for (const el of document.querySelectorAll('.brand .logo')) {
    if (version) {
      const img = Object.assign(document.createElement('img'), { className: 'logo-img', alt: '', src: `/admin/logo.png?v=${encodeURIComponent(version)}` });
      el.replaceChildren(img);
    } else {
      el.textContent = '◆';
    }
  }
  // The tab icon is made from the logo; a new version gets past the cache.
  for (const link of document.querySelectorAll('link[rel="icon"], link[rel="apple-touch-icon"]')) {
    const url = new URL(link.href, location.href);
    url.searchParams.set('v', version || 'default');
    link.href = url.pathname + url.search;
  }
}
