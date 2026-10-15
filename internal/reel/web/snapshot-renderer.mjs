// The reused-iframe snapshot renderer shared by the catalog's card previews
// (catalog-adapter.mjs) and the viewer's sidecar thumbnails (sidecar-view.mjs).
// One hidden renderer document loads the official player's script, stylesheet
// and fonts once; each one-frame cast gets its own player instance in a fresh
// host element, is snapshotted into inert DOM and disposed before the next is
// created. The caller owns the document's lifetime: a paint that did not
// finish cleanly (abort, timeout, failed disposal) must retire the document,
// which also cancels unfinished vendor work.
import * as themes from '/assets/themes.mjs';

// Connected box drawing first (unicode-range U+2500-257F), then the text font.
export const terminalFonts = 'AgentReelBox, AgentReelMono';
// Match Go libraryPreviewCastMax: a 256KiB seed can expand sixfold when JSON
// escapes control characters, plus bounded header/event framing overhead.
export const previewCastMax = 6 * 256 * 1024 + 2048;
export const abortError = () => new DOMException('Preview canceled', 'AbortError');
export function bounded(promise, signal, milliseconds = 6000) {
  return new Promise((resolve, reject) => {
    const abort = () => finish(reject, abortError());
    const timer = setTimeout(() => finish(reject, new Error('Preview timeout')), milliseconds);
    function finish(settle, value) {
      clearTimeout(timer); signal.removeEventListener('abort', abort); settle(value);
    }
    signal.addEventListener('abort', abort, { once: true });
    if (signal.aborted) abort();
    Promise.resolve(promise).then(value => finish(resolve, value), error => finish(reject, error));
  });
}
let sequence = 0;
export function snapshot(doc, host) {
  const term = host.querySelector('.ap-term');
  if (!term) throw new Error('Terminal did not paint');
  const cols = Number(term.style.getPropertyValue('--term-cols'));
  const rows = Number(term.style.getPropertyValue('--term-rows'));
  if (!Number.isInteger(cols) || !Number.isInteger(rows) || cols < 1 || rows < 1 || cols > 512 || rows > 512 || cols * rows > 32768) throw new Error('Preview geometry');
  const walker = doc.createTreeWalker(term, 0xffffffff);
  let nodes = 0;
  while (walker.nextNode()) if (++nodes > 8192) throw new Error('Preview detail limit');
  // This subtree is trusted renderer-built DOM, never parsed recording HTML.
  // cloneNode copies no listeners; keep snapshots inert and namespace SVG IDs.
  const copy = term.cloneNode(true), prefix = `preview-${++sequence}-`;
  for (const node of copy.querySelectorAll('*')) {
    for (const attr of [...node.attributes]) {
      if (/^on/i.test(attr.name) || ['tabindex', 'contenteditable', 'src'].includes(attr.name)) node.removeAttribute(attr.name);
    }
    if (node.id) node.id = prefix + node.id;
    for (const name of ['href', 'xlink:href']) {
      if (!node.hasAttribute(name)) continue;
      const value = node.getAttribute(name);
      if (value.startsWith('#')) node.setAttribute(name, '#' + prefix + value.slice(1));
      else node.removeAttribute(name);
    }
  }
  for (const layer of copy.querySelectorAll('.ap-term-text,.ap-term-symbols')) layer.classList.add('ap-blink');
  const canvases = [...term.querySelectorAll('canvas')];
  for (const [i, canvas] of [...copy.querySelectorAll('canvas')].entries()) {
    canvas.width = cols * 2; canvas.height = rows * 4;
    const context = canvas.getContext('2d');
    if (!context) throw new Error('Canvas unavailable');
    context.drawImage(canvases[i], 0, 0, canvas.width, canvas.height);
  }
  const wrap = document.createElement('div');
  wrap.className = 'preview-snapshot ' + [...term.parentElement.classList].filter(name => /^(ap-player|ap-default-term-ff|asciinema-player-theme-[a-z0-9-]+)$/.test(name)).join(' ');
  wrap.setAttribute('aria-hidden', 'true'); wrap.inert = true;
  for (const name of ['--term-color-foreground', '--term-color-background', ...Array.from({ length: 16 }, (_, i) => `--term-color-${i}`)]) {
    wrap.style.setProperty(name, doc.defaultView.getComputedStyle(term.parentElement).getPropertyValue(name));
  }
  wrap.style.setProperty('--term-font-family', terminalFonts);
  // The player writes font-size:100% inline; release it so the catalog's
  // fixed, legible snapshot size applies. The card crops to the active area.
  copy.style.removeProperty('font-size');
  copy.style.width = `${cols}ch`; copy.style.height = `${rows * 1.3333333333}em`;
  wrap.append(document.adoptNode(copy));
  return wrap;
}
// Show the active area of a legible snapshot: its top-left, moved up just
// enough that the last non-blank row is in view (a TUI's prompt and status
// sit at the bottom of the screen).
export function crop(wrap) {
  const term = wrap.firstElementChild;
  const rows = Number(term?.style.getPropertyValue('--term-rows'));
  if (!term || !Number.isInteger(rows) || rows < 1) return;
  let last = -1;
  for (const line of term.querySelectorAll('.ap-line')) {
    const row = Number(line.style.getPropertyValue('--row'));
    if (Number.isInteger(row) && row > last && line.textContent.trim()) last = row;
  }
  if (last < 0) return;
  const border = (term.offsetHeight - term.clientHeight) / 2;
  const line = term.clientHeight / rows;
  // Move by whole rows so the top row is never cut, keeping the border inset.
  const fits = Math.floor((wrap.clientHeight - 2 * border) / line);
  const skip = Math.max(0, last + 1 - fits);
  if (skip > 0) {
    const shift = Math.round(skip * line);
    term.style.transform = `translateY(-${shift}px)`;
    // Rows moved above the top border stay hidden, never half-visible.
    term.style.clipPath = `inset(${Math.round(border) + shift}px 0 0 0)`;
  }
}
// openRendererDocument appends one hidden renderer iframe: {frame, loaded}.
export function openRendererDocument() {
  const frame = document.createElement('iframe');
  frame.className = 'current-preview-renderer'; frame.tabIndex = -1; frame.inert = true;
  frame.setAttribute('aria-hidden', 'true'); frame.title = 'Temporary preview renderer';
  const loaded = new Promise((resolve, reject) => { frame.onload = resolve; frame.onerror = reject; });
  loaded.catch(() => {});
  frame.srcdoc = '<!doctype html><link rel="stylesheet" href="/assets/player.css"><link rel="stylesheet" href="/assets/app.css"><script src="/assets/player.js"></script>';
  document.body.append(frame);
  return { frame, loaded };
}
// paintCast paints the last frame of one cast in owner's document and returns
// its inert snapshot. alive() is checked between steps; a false answer aborts.
// On any throw the caller must retire owner's document.
export async function paintCast(owner, raw, signal, alive = () => true) {
  await bounded(owner.loaded, signal);
  if (!alive()) throw abortError();
  const doc = owner.frame.contentDocument, win = owner.frame.contentWindow;
  if (!doc || !win?.AsciinemaPlayer) throw new Error('Renderer unavailable');
  // Load both terminal faces before the first instance measures its cell.
  owner.fonts ??= Promise.all([doc.fonts.load('16px AgentReelMono'), doc.fonts.load('16px AgentReelBox', '│')]);
  await bounded(owner.fonts, signal);
  const host = doc.createElement('div');
  // Snapshots use the colours this browser's viewer shows (its stored
  // choice, else the library default), so a card matches what opens.
  const theme = themes.current(document.querySelector('#app')?.dataset.themeDefault);
  themes.apply(host, theme);
  doc.body.append(host);
  const player = win.AsciinemaPlayer.create({ data: raw }, host, {
    autoPlay: false, preload: true, controls: false, fit: false,
    cursorMode: 'hidden', terminalFontFamily: terminalFonts, idleTimeLimit: Infinity,
    theme: themes.playerTheme(theme),
  });
  await bounded(player.pause(), signal);
  await bounded(player.seek('100%'), signal);
  await bounded(doc.fonts.ready, signal);
  await bounded(new Promise(resolve => win.requestAnimationFrame(() => win.requestAnimationFrame(resolve))), signal);
  if (!alive()) throw abortError();
  const image = snapshot(doc, host);
  // Do not publish or start another instance until disposal settles. A failed
  // or pending disposal loses the entire document (the caller retires it).
  await bounded(player.dispose(), signal);
  host.remove();
  return image;
}
