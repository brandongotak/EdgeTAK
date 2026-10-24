// Applies the theme chosen with the console's theme button (Auto follows
// the device's light or dark setting). Loaded in <head>, before the page is
// drawn, by every EdgeTAK page.
(() => {
  try {
    const t = localStorage.getItem('edgetak.theme');
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  } catch { /* storage blocked: Auto */ }
})();
