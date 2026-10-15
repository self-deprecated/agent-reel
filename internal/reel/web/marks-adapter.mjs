// DOM-only focus/reveal. Go supplies every slot, page URL and footage target.
const rail = document.querySelector('.rail-list');
const page = document.querySelector('[data-marks-page]')?.dataset.marksPage;
const prefix = 'agent-reel-marks:';
const validID = value => typeof value === 'string' && /^mark-\d{1,16}$/.test(value);
const remember = (page, id) => { window.name = prefix + JSON.stringify({ page, id }); };
const reveal = mark => {
  mark.focus({ preventScroll: true });
  const left = mark.getBoundingClientRect().left - rail.getBoundingClientRect().left + rail.scrollLeft;
  const right = left + mark.offsetWidth;
  if (left < rail.scrollLeft || mark.offsetWidth >= rail.clientWidth) rail.scrollLeft = left;
  else if (right > rail.scrollLeft + rail.clientWidth) rail.scrollLeft = right - rail.clientWidth;
};
if (rail && page) {
  let restoredFocus = false;
  // window.name belongs only to this iframe and replaces, rather than grows, the
  // saved hint. No application cache, storage key accumulation or parent access.
  if (window.name.startsWith(prefix) && window.name.length <= 512) {
    try {
      const saved = JSON.parse(window.name.slice(prefix.length));
      if (saved.page === page && validID(saved.id)) {
        const mark = document.getElementById(saved.id);
        if (mark && rail.contains(mark)) { reveal(mark); restoredFocus = true; }
      }
    } catch { /* An invalid hint never changes navigation or marks. */ }
  }
  // Go identifies the explicit newest-page observation. Reveal its end without
  // stealing focus from controls; a same-page focused mark takes precedence.
  if (!restoredFocus && document.querySelector('[data-marks-reveal-tail]')) {
    rail.scrollLeft = rail.scrollWidth - rail.clientWidth;
  }
  document.addEventListener('focusin', event => {
    const mark = event.target.closest('.mark');
    if (mark && rail.contains(mark) && validID(mark.id)) remember(page, mark.id);
    else if (window.name.startsWith(prefix)) window.name = ''; // Never steal focus from page controls.
  });
  document.addEventListener('click', () => {
    if (!document.activeElement?.closest('.mark') && window.name.startsWith(prefix)) window.name = '';
  }, true);
  rail.addEventListener('click', event => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const link = event.target.closest('a[href]');
    if (!link) return;
    const target = new URL(link.href);
    if (target.origin !== location.origin || target.pathname !== location.pathname || !validID(target.hash.slice(1))) return;
    const id = target.hash.slice(1);
    const samePage = target.search === location.search;
    target.hash = '';
    event.preventDefault();
    // Strip a Go-authored fragment before cross-page navigation so native
    // scrollIntoView cannot scroll the outer viewer. Restore it as focus only.
    if (!samePage) { remember(target.pathname + target.search, id); location.assign(target.href); return; }
    const mark = document.getElementById(id);
    if (mark && rail.contains(mark)) { remember(page, id); reveal(mark); }
  });
}
