// Own one renderer document, the marks rail and small page affordances. No
// catalog, archive or timing model: Go supplies every URL and footage target.
import * as themes from '/assets/themes.mjs';
import '/assets/search-adapter.mjs'; // text search box (library and rail)
const frame = document.querySelector('#terminal-frame');
const status = document.querySelector('#player-status');
const setStatus = (text, problem = false, state = '') => {
  status.textContent = text;
  status.toggleAttribute('data-problem', problem);
  if (state) status.dataset.state = state; else delete status.dataset.state;
};
let onTime = () => {};
if (frame) {
  const live = frame.dataset.mode === 'live';
  let deadline;
  const clearDeadline = () => { clearTimeout(deadline); deadline = undefined; };
  const unavailable = () => {
    clearDeadline();
    frame.remove(); // Hard disposal includes the vendor's pending retry timers.
    if (live) setStatus('Live unavailable or ended. Retry, or play the recording from the start.', true, 'unavailable');
    else setStatus('Playback unavailable. The selected history may not be complete or supported.', true, 'unavailable');
  };
  const armDeadline = () => {
    if (deadline === undefined) deadline = setTimeout(unavailable, live ? 14900 : 60000);
  };
  window.addEventListener('message', event => {
    if (event.origin !== location.origin || event.source !== frame.contentWindow || event.data?.agentReel !== 'player') return;
    switch (event.data.state) {
      case 'playing':
        clearDeadline();
        setStatus(live ? 'Live' : 'Playing', false, 'playing');
        break;
      case 'pause': setStatus('Paused', false, 'paused'); break;
      case 'loading': armDeadline(); setStatus(live ? 'Connecting to Live…' : 'Loading…', false, 'loading'); break;
      case 'ended':
        if (live) unavailable();
        else { clearDeadline(); setStatus('Ended', false, 'ended'); }
        break;
      case 'offline': case 'error': unavailable(); break;
      case 'time': if (Number.isFinite(event.data.time)) onTime(event.data.time); break;
    }
  });
  armDeadline();
  frame.src = frame.dataset.src;
  window.addEventListener('pagehide', () => { clearDeadline(); frame.remove(); }, { once: true });
  window.addEventListener('pageshow', event => { if (event.persisted) location.reload(); });
}

// The ⋯ menu closes on outside clicks and Escape; "Diagnostics" opens the
// disclosure below the player.
const menu = document.querySelector('[data-menu]');
if (menu) {
  document.addEventListener('click', event => { if (menu.open && !menu.contains(event.target)) menu.open = false; });
  document.addEventListener('keydown', event => { if (event.key === 'Escape' && menu.open) { menu.open = false; menu.querySelector('summary').focus(); } });
}
for (const link of document.querySelectorAll('[data-open-diagnostics]')) {
  link.addEventListener('click', event => {
    const diagnostics = document.querySelector('#diagnostics');
    if (!diagnostics) return;
    event.preventDefault();
    diagnostics.open = true;
    if (menu) menu.open = false;
    diagnostics.scrollIntoView({ block: 'nearest' });
    diagnostics.querySelector('summary').focus({ preventScroll: true });
  });
}

// Terminal colours (⋯ menu): a preset, a background override and Reset. The
// choice is remembered in this browser and applied live to the player frames;
// Export and .cast exports default to what is shown. Go renders the controls
// hidden, so without script only the library default applies.
const themeControl = document.querySelector('[data-theme-control]');
if (themeControl) {
  const libraryDefault = document.querySelector('#app')?.dataset.themeDefault;
  const presetSelect = themeControl.querySelector('[data-theme-preset]');
  const bgInput = themeControl.querySelector('[data-theme-bg]');
  for (const preset of themes.presets) presetSelect.append(new Option(preset.label, preset.name));
  let spec = themes.current(libraryDefault);
  const show = () => {
    presetSelect.value = spec.preset;
    bgInput.value = themes.resolve(spec).bg;
    const text = themes.format(spec);
    for (const field of document.querySelectorAll('[data-theme-field]')) field.value = text;
    for (const link of document.querySelectorAll('[data-export-link]')) {
      const url = new URL(link.href, location.href);
      url.searchParams.set('theme', text);
      link.href = url.pathname + url.search;
    }
    document.querySelector('#app').dataset.theme = text;
  };
  // Frames get the latest choice after a short pause (a colour picker fires
  // continuously while dragged).
  let post;
  const choose = (next, remember = true) => {
    spec = next;
    if (remember) themes.store(spec);
    show();
    clearTimeout(post);
    post = setTimeout(() => {
      for (const target of document.querySelectorAll('iframe.terminal-frame')) {
        target.contentWindow?.postMessage({ agentReel: 'theme', theme: themes.format(spec) }, location.origin);
      }
    }, 120);
  };
  presetSelect.addEventListener('change', () => {
    // A new preset keeps a chosen background override.
    choose({ ...spec, preset: presetSelect.value });
  });
  bgInput.addEventListener('input', () => {
    const value = bgInput.value.toLowerCase();
    if (/^#[0-9a-f]{6}$/.test(value)) choose({ ...spec, bg: value });
  });
  themeControl.querySelector('[data-theme-reset]').addEventListener('click', () => {
    themes.store(null);
    choose(themes.current(libraryDefault), false);
  });
  show();
  themeControl.hidden = false;
}

// Live bar: fullscreen of the player and an elapsed clock that keeps counting
// from the server's figure.
const player = document.querySelector('#player');
document.querySelector('[data-fullscreen]')?.addEventListener('click', () => {
  if (document.fullscreenElement) void document.exitFullscreen();
  else void player?.requestFullscreen?.();
});
const liveClock = document.querySelector('[data-live-clock]');
if (liveClock && document.querySelector('#terminal-frame, #still-frame')) {
  const parts = liveClock.dataset.liveClock.split(':').map(Number);
  if (parts.length >= 2 && parts.every(Number.isFinite)) {
    const base = parts.reduce((total, part) => total * 60 + part, 0), started = Date.now();
    const pad = n => String(n).padStart(2, '0');
    setInterval(() => {
      const s = base + Math.floor((Date.now() - started) / 1000);
      liveClock.textContent = s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${Math.floor(s / 60)}:${pad(s % 60)}`;
    }, 1000);
  }
}

// Marks rail. Rows come from the marks JSON (who, label, segment, seconds); only
// marker rows are shown, consecutive tool results merge into one row. Text is
// written with textContent only.
const rail = document.querySelector('#marks');
if (rail) {
  const body = rail.querySelector('[data-rail-body]');
  const count = rail.querySelector('[data-rail-count]');
  const segment = Number(rail.dataset.segment);
  const segments = Number(rail.dataset.segments);
  const history = rail.dataset.mode === 'history';
  const recording = rail.dataset.recording;
  const pad = n => String(n).padStart(2, '0');
  const clock = seconds => {
    const s = Math.max(0, Math.floor(seconds));
    return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${pad(Math.floor(s / 60))}:${pad(s % 60)}`;
  };
  const labels = { you: 'You', agent: 'Agent replied', tool: 'Tool result', other: 'Mark' };
  // A mark that declares its own label (areel mark, /reel mark, an agent's mark) is named by it.
  const name = mark => typeof mark.label === 'string' && mark.label.trim() ? mark.label.trim() : labels[mark.who] ?? 'Mark';
  let rows = [];
  const element = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const empty = () => {
    body.replaceChildren();
    const box = element('div', 'rail-empty');
    box.append(element('strong', '', 'No marks yet'),
      element('p', '', 'Marks appear when an agent or areel mark sends events to this recording. If you expect Pi marks and none show up, run this in the Pi session:'),
      element('code', '', '/reel bind'));
    body.append(box);
  };
  const seekable = mark => mark.status === 'stored' && mark.seconds !== null && mark.coverage === 'within-durable';
  // Same-segment links keep the viewer's history cutoff; other segments pin
  // their own when opened.
  const cutoff = rail.dataset.cutoff;
  const target = mark => `/recordings/${recording}?mode=history&segment=${mark.segment}&at=${mark.seconds}${cutoff && mark.segment === segment ? `&cutoff=${cutoff}` : ''}`;
  const render = page => {
    // A retroactive mark (areel mark --ago) points its offset earlier.
    const marks = page.items.filter(mark => mark.marker && mark.status === 'stored' && !(mark.kind === 'areel:keys' && mark.source === 'areel:attach'))
      .map(mark => Number.isSafeInteger(mark.offsetNs) && mark.offsetNs > 0 && typeof mark.seconds === 'number' ? { ...mark, seconds: Math.max(0, Math.round((mark.seconds - mark.offsetNs / 1e9) * 1000) / 1000) } : mark);
    // Merge consecutive tool results (same segment) into one row.
    const merged = [];
    for (const mark of marks) {
      const previous = merged.at(-1);
      if (previous && mark.who === 'tool' && previous.who === 'tool' && previous.segment === mark.segment) { previous.count++; continue; }
      merged.push({ ...mark, count: 1 });
    }
    body.replaceChildren();
    if (!merged.length && !page.moreBefore && !page.moreAfter) { empty(); return; }
    const list = element('ol', 'rail-list');
    rows = [];
    if (page.moreBefore) {
      const earlier = element('button', 'rail-more', 'Earlier marks');
      earlier.type = 'button';
      earlier.addEventListener('click', () => load(`before=${page.prevBefore}`));
      body.append(earlier);
    }
    for (const mark of merged) {
      const item = element('li');
      const open = seekable(mark);
      const row = element(open ? 'a' : 'span', `mark-row mark-${mark.who}${open ? '' : ' mark-unplaced'}`);
      if (open) row.href = target(mark);
      row.dataset.segment = mark.segment; row.dataset.seconds = mark.seconds ?? '';
      const time = mark.seconds === null ? '—' : clock(mark.seconds);
      row.append(element('span', 'mark-at', segments > 1 && mark.segment ? `${mark.segment} · ${time}` : time), element('span', 'mark-dot'),
        element('span', 'mark-label', mark.who === 'tool' && mark.count > 1 ? `${mark.count} tool results` : name(mark)));
      if (typeof mark.label === 'string' && mark.label.trim()) row.title = mark.label.trim();
      if (!open) row.title = 'Not placed on the saved footage';
      item.append(row);
      list.append(item);
      if (open) rows.push({ row, segment: mark.segment, seconds: mark.seconds });
    }
    body.append(list);
    if (page.moreAfter) {
      const later = element('button', 'rail-more', 'Later marks');
      later.type = 'button';
      later.addEventListener('click', () => load(`after=${page.nextAfter}`));
      body.append(later);
    }
    const at = Number(rail.dataset.at);
    if (history && at > 0) highlight(at, true);
    else if (!history) list.lastElementChild?.scrollIntoView({ block: 'nearest' });
  };
  let current;
  const highlight = (time, reveal) => {
    let found;
    for (const entry of rows) if (entry.segment === segment && entry.seconds <= time + 0.25) found = entry;
    if (found === current) return;
    current?.row.classList.remove('mark-current');
    current?.row.removeAttribute('aria-current');
    current = found;
    if (!found) return;
    found.row.classList.add('mark-current');
    found.row.setAttribute('aria-current', 'true');
    if (reveal || !rail.matches(':hover')) {
      // Scroll the rail only, never the page.
      const top = found.row.offsetTop - body.offsetTop, bottom = top + found.row.offsetHeight;
      if (top < body.scrollTop) body.scrollTop = top - 8;
      else if (bottom > body.scrollTop + body.clientHeight) body.scrollTop = bottom - body.clientHeight + 8;
    }
  };
  onTime = time => highlight(time, false);
  body.addEventListener('click', event => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const row = event.target.closest('a.mark-row');
    const live = document.querySelector('#terminal-frame');
    if (!row || !history || !live?.contentWindow || Number(row.dataset.segment) !== segment) return;
    event.preventDefault();
    const seconds = Number(row.dataset.seconds);
    live.contentWindow.postMessage({ agentReel: 'seek', at: seconds }, location.origin);
    highlight(seconds, false);
  });
  const load = async query => {
    try {
      const response = await fetch(`${rail.dataset.marksApi}?${query}&limit=256`, { cache: 'no-store', credentials: 'same-origin' });
      if (!response.ok) throw new Error('marks');
      render(await response.json());
    } catch {
      body.replaceChildren(element('p', 'rail-note', 'Marks are unavailable right now. Playback is unaffected.'));
    }
  };
  // Mark button and m key (live recordings only): one manual mark at now,
  // with an optional label, through POST marks. There is no body: the label
  // travels as unpadded base64url of its UTF-8 bytes. The rail then reloads
  // its newest page; nothing else changes.
  const markButton = rail.querySelector('[data-mark-button]');
  if (markButton) {
    const encode = text => {
      let binary = '';
      for (const byte of new TextEncoder().encode(text)) binary += String.fromCharCode(byte);
      return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    };
    const title = markButton.title;
    let busy = false, restore;
    const show = (text, reason) => {
      clearTimeout(restore);
      markButton.textContent = text; markButton.title = reason ?? title;
      restore = setTimeout(() => { markButton.textContent = 'Mark'; markButton.title = title; }, 2500);
    };
    const place = async () => {
      if (busy) return;
      const text = window.prompt('Mark this moment. Label (optional):', '');
      if (text === null) return;
      // At most 256 characters; the server keeps at most 256 bytes.
      const label = Array.from(text.trim()).slice(0, 256).join('');
      busy = true; markButton.disabled = true;
      try {
        const response = await fetch(`${rail.dataset.marksApi}${label ? `?label=${encode(label)}` : ''}`, { method: 'POST', headers: { 'AgentReel-Mark': '1' }, cache: 'no-store', credentials: 'same-origin' });
        const reply = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(reply?.error === 'not-live' ? 'This recording is no longer live; the mark was not added.' : 'The mark could not be added right now.');
        show('Marked');
        void load(`before=${rail.dataset.newest}`);
      } catch (error) {
        show('Not added', error instanceof Error ? error.message : undefined);
      } finally {
        busy = false; markButton.disabled = false;
      }
    };
    markButton.addEventListener('click', () => { void place(); });
    document.addEventListener('keydown', event => {
      if (event.key !== 'm' || event.repeat || event.metaKey || event.ctrlKey || event.altKey || event.defaultPrevented) return;
      const target = event.target;
      if (target instanceof HTMLElement && (target.isContentEditable || target.closest('input, textarea, select'))) return;
      event.preventDefault();
      void place();
    });
  }
  const at = Number(rail.dataset.at);
  void load(history && at > 0 && segment > 0 ? `at=${at}&segment=${segment}` : `before=${rail.dataset.newest}`);
  fetch(rail.dataset.summaryApi, { cache: 'no-store', credentials: 'same-origin' })
    .then(response => response.ok ? response.json() : null)
    .then(summary => { if (summary?.marks && Number.isInteger(summary.marks.count) && summary.marks.count > 0) count.textContent = String(summary.marks.count); })
    .catch(() => {});
}
