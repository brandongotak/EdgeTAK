'use strict';

// An eye button in every password box, to show the password as typed and
// hide it again. Resetting the form hides it.
(() => {
  const EYE = '<svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z" fill="none" stroke="currentColor" stroke-width="2"/><circle cx="12" cy="12" r="3" fill="none" stroke="currentColor" stroke-width="2"/><path class="pw-slash" d="M4 4l16 16" stroke="currentColor" stroke-width="2"/></svg>';
  for (const input of document.querySelectorAll('input[type="password"]')) {
    const wrap = document.createElement('span');
    wrap.className = 'pw-field';
    input.replaceWith(wrap);
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'pw-toggle';
    b.innerHTML = EYE;
    wrap.append(input, b);
    const set = (show) => {
      input.type = show ? 'text' : 'password';
      b.setAttribute('aria-pressed', String(show));
      b.title = show ? 'Hide password' : 'Show password';
      b.setAttribute('aria-label', b.title);
    };
    set(false);
    b.addEventListener('click', () => {
      set(input.type === 'password');
      input.focus();
    });
    input.form?.addEventListener('reset', () => set(false));
  }
})();
