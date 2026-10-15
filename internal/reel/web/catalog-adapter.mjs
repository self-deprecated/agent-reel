// Go owns the catalog and current-screen endpoint. This adapter only paints
// visible observations through the unmodified official terminal renderer.
// One renderer document at a time owns initialization, painting and disposal.
// It is reused across the cards of one batch, so the official player's
// script, stylesheet and fonts load (and its WASM initializes) once per batch
// rather than once per card. Each card still gets its own player instance in
// a fresh host element, disposed before the next is created. A card that does
// not finish cleanly (abort, timeout, failed disposal) retires the whole
// document; destroying it also cancels unfinished vendor work before another
// may start. An idle renderer is retired when its batch ends.
import '/assets/search-adapter.mjs'; // text search box (library and rail)
import { abortError, bounded, crop, openRendererDocument, paintCast, previewCastMax } from '/assets/snapshot-renderer.mjs';
const visible = new Set();
const due = new WeakMap();
let active, stopped = false, batchAt = 0;
const refreshAge = 10000;
function current(job) {
  return !stopped && !document.hidden && visible.has(job.card)
    && job.card.isConnected && job.card.dataset.previewUrl === job.url;
}
function alive(job) { return current(job) && !job.controller.signal.aborted; }
// A visual/1 durable card names its own screen in every caption; recorder-owned
// current-screen captions are unchanged.
const durableURL = url => typeof url === 'string' && url.endsWith('/preview-durable');
const screenName = card => durableURL(card.dataset.previewUrl) ? 'Durable screen' : 'Current screen';
let renderer = null;
function retireRenderer() { renderer?.frame.remove(); renderer = null; }
function openRenderer() {
  renderer ??= openRendererDocument();
  return renderer;
}
async function render(job, raw) {
  const owner = openRenderer();
  job.frame = owner.frame;
  let clean = false;
  try {
    const image = await paintCast(owner, raw, job.controller.signal, () => alive(job));
    clean = !job.controller.signal.aborted;
    return image;
  } finally {
    job.frame = null;
    if (!clean && renderer === owner) retireRenderer();
  }
}
// Fetch one preview URL and check that it names its own source.
async function observe(url, signal) {
  const response = await bounded(fetch(url, { signal, cache: 'no-store', credentials: 'same-origin' }), signal);
  const durable = durableURL(url);
  if (!response.ok || response.headers.get('AgentReel-Preview-Source') !== (durable ? 'visual-durable-view' : 'recorder-initialization')) {
    void response.body?.cancel().catch(() => {});
    throw new Error('Current screen unavailable');
  }
  return response;
}
async function paint(job) {
  const signal = job.controller.signal;
  try {
    // A visual/1 card's durable still names its own source; each URL accepts
    // only its own source value. A live card whose current screen is
    // unavailable falls back to its durable still, captioned as such.
    let url = job.url, fallback = false, response;
    try {
      response = await observe(url, signal);
    } catch (error) {
      const alternative = job.card.dataset.previewFallbackUrl;
      if (signal.aborted || !durableURL(alternative)) throw error;
      url = alternative; fallback = true;
      response = await observe(url, signal);
    }
    const durable = durableURL(url);
    const lastChange = durable ? /^segment=(\d+);at=(\d+\.\d{3})$/.exec(response.headers.get('AgentReel-Preview-Last-Change') ?? '') : null;
    if (durable && !lastChange) throw new Error('Last change unavailable');
    const observed = new Date(response.headers.get('AgentReel-Preview-Observed-At') ?? '');
    if (!Number.isFinite(observed.getTime())) throw new Error('Observation time unavailable');
    // Bound the decoded response even when Content-Length is absent or false.
    const reader = response.body.getReader(); let size = 0; const chunks = [];
    try {
      for (;;) {
        const part = await bounded(reader.read(), signal);
        if (part.done) break;
        size += part.value.byteLength;
        if (size > previewCastMax) throw new Error('Preview response limit');
        chunks.push(part.value);
      }
    } finally { void reader.cancel().catch(() => {}); }
    if (!alive(job)) throw abortError();
    const raw = await new Blob(chunks).text();
    if (!alive(job)) throw abortError();
    const image = await render(job, raw);
    if (!alive(job)) return;
    job.card.querySelector('.preview-snapshot')?.remove();
    job.card.prepend(image);
    crop(image);
    const caption = job.card.querySelector('[data-preview-status]');
    caption.textContent = durable
      ? `${fallback ? 'Live unavailable · durable screen' : 'Durable screen'} · last change segment ${lastChange[1]} at ${lastChange[2]}s · observed ${observed.toISOString().slice(11, 19)} UTC`
      : `Current screen · observed ${observed.toISOString().slice(11, 19)} UTC`;
    caption.title = observed.toISOString();
    job.card.dataset.previewState = 'ready';
    if (fallback) job.card.dataset.previewFallback = 'durable'; else delete job.card.dataset.previewFallback;
  } catch {
    if (current(job)) {
      job.card.querySelector('.preview-snapshot')?.remove();
      job.card.querySelector('[data-preview-status]').textContent = `${screenName(job.card)} unavailable`;
      job.card.dataset.previewState = 'unavailable';
    }
  }
}
async function pump() {
  if (active || stopped || document.hidden || Date.now() < batchAt) return;
  batchAt = Date.now() + 2000;
  let more = false;
  try { more = await batch(); } finally { if (!active && !more) retireRenderer(); }
  // A batch cut short while visible cards have never been painted continues
  // at once, on the same renderer, when every card it attempted became
  // ready. Any failure keeps the two-second spacing, which also paces
  // refreshes.
  if (more) { batchAt = 0; setTimeout(() => { void pump(); }, 0); }
}
async function batch() {
  let count = 0, failed = false;
  for (const card of [...visible]) {
    if(stopped || document.hidden) return false;
    if (!visible.has(card) || !card.isConnected || (due.get(card) ?? 0) > Date.now()) continue;
    if (++count > 8) return !failed && [...visible].some(other => other.isConnected && !due.has(other));
    // Rotate attempted cards to the end: a busy/failing card cannot starve peers.
    visible.delete(card); visible.add(card);
    const job = { card, url: card.dataset.previewUrl, controller: new AbortController(), frame: null };
    active = job;
    card.dataset.previewState = 'loading';
    if (!card.querySelector('.preview-snapshot')) card.querySelector('[data-preview-status]').textContent = durableURL(job.url) ? 'Loading durable screen…' : 'Loading current screen…';
    const timeout = setTimeout(() => job.controller.abort(), 12000);
    await paint(job);
    failed ||= card.dataset.previewState !== 'ready';
    clearTimeout(timeout); job.controller.abort();
    due.set(card, Date.now() + refreshAge); active = null;
    if (stopped) return false;
  }
  return false;
}
const observer = new IntersectionObserver(entries => {
  for (const entry of entries) {
    if (entry.isIntersecting) visible.add(entry.target);
    else {
      visible.delete(entry.target);
      if (active?.card === entry.target) active.controller.abort();
      entry.target.querySelector('.preview-snapshot')?.remove();
      entry.target.querySelector('[data-preview-status]').textContent = `${screenName(entry.target)} not loaded`;
      delete entry.target.dataset.previewState; due.delete(entry.target);
    }
  }
  void pump();
});
for (const card of document.querySelectorAll('[data-preview-url]')) observer.observe(card);
const timer = setInterval(() => { void pump(); }, 1000);
document.addEventListener('visibilitychange', () => {
  if (document.hidden) { active?.controller.abort(); retireRenderer(); catalogRequest?.abort(); }
  else void pump();
});
window.addEventListener('pagehide', () => {
  stopped = true; clearInterval(timer); observer.disconnect();
  active?.controller.abort(); retireRenderer(); catalogRequest?.abort(); clearInterval(catalogTimer); visible.clear();
}, { once: true });
window.addEventListener('pageshow', event => { if (event.persisted) location.reload(); });

// Go renders search, grouping, order and lifecycle labels. Reconcile only those
// server-authored fragments, retaining controls, card links and inert previews.
let catalogRequest;
async function refreshCatalog(force = false) {
  if (force) catalogRequest?.abort();
  else if (stopped || document.hidden || catalogRequest) return;
  if (stopped) return;
  const controller = new AbortController();
  catalogRequest = controller;
  const timeout = setTimeout(() => controller.abort(), 6000);
  try {
    const response = await fetch(location.href, {signal: controller.signal, cache:'no-store', credentials:'same-origin'});
    if (!response.ok) return;
    const observed = response.headers.get('AgentReel-Catalog-Observed-At');
    const label = document.querySelector('[data-catalog-observation]');
    if (!observed || (!force && observed === label?.dataset.catalogObservation)) { await response.body.cancel(); return; }
    const reader = response.body.getReader(), chunks = []; let bytes = 0;
    try {for (;;) {const part = await reader.read(); if(part.done) break; bytes += part.value.byteLength; if(bytes > 1<<20) throw Error('Catalog response limit'); chunks.push(part.value);}}
    finally {void reader.cancel().catch(()=>{});}
    const html = await new Blob(chunks).text();
    if (stopped || controller.signal.aborted || (document.hidden && !force)) return;
    const parsed = new DOMParser().parseFromString(html,'text/html');
    const old = document.querySelector('.cards'), fresh = parsed.querySelector('.cards');
    const nextLabel = parsed.querySelector('[data-catalog-observation]');
    if (!old || !fresh || !nextLabel || nextLabel.dataset.catalogObservation !== observed) return;
    const filters = document.querySelector('[data-catalog-filters]'), freshFilters = parsed.querySelector('[data-catalog-filters]');
    if (filters && freshFilters && filters.outerHTML !== freshFilters.outerHTML) filters.replaceWith(freshFilters);
    if(old.dataset.catalogRevision === fresh.dataset.catalogRevision){label.replaceWith(nextLabel);return;}
    const focus = document.activeElement, x = scrollX, y = scrollY;
    const retained = new Map([...old.querySelectorAll('[data-recording-card]')].map(card=>[card.dataset.recordingCard,card]));
    for (const card of fresh.querySelectorAll('[data-recording-card]')) {
      const previous = retained.get(card.dataset.recordingCard);
      if (previous) {
        retained.delete(card.dataset.recordingCard);
        const beforePreview=previous.querySelector('.current-preview'), afterPreview=card.querySelector('.current-preview');
        if (beforePreview.dataset.previewUrl!==afterPreview.dataset.previewUrl) {
          observer.unobserve(beforePreview);visible.delete(beforePreview);due.delete(beforePreview);
          if(active?.card===beforePreview){active.controller.abort();if(active.frame)retireRenderer();}
          beforePreview.replaceWith(afterPreview);
          if(afterPreview.dataset.previewUrl) observer.observe(afterPreview);
        }
        for (const selector of ['.card-badges','.card-body']) {
          const before=previous.querySelector(selector), after=card.querySelector(selector);
          if (before && after && before.outerHTML!==after.outerHTML) before.replaceWith(after);
        }
        if (previous.className!==card.className) previous.className=card.className;
        card.replaceWith(previous);
      } else {const preview=card.querySelector('[data-preview-url]'); if(preview) observer.observe(preview);}
    }
    for (const card of retained.values()) {
      const preview = card.querySelector('[data-preview-url]');
      if(preview) observer.unobserve(preview); visible.delete(preview);
      if (active?.card===preview) {active.controller.abort(); if(active.frame)retireRenderer();}
    }
    old.replaceWith(fresh);
    label.replaceWith(nextLabel);
    const next = document.querySelector('[data-catalog-next]'), nextFresh = parsed.querySelector('[data-catalog-next]');
    if (next && nextFresh) {if(next.href!==nextFresh.href) next.href=nextFresh.href;}
    else if(next) next.remove(); else if(nextFresh) fresh.after(nextFresh);
    if(focus?.isConnected && document.activeElement!==focus) focus.focus({preventScroll:true});
    scrollTo(x,y);
  } catch { /* Keep the last explicitly timestamped observation on failure. */ }
  finally {clearTimeout(timeout); if (catalogRequest === controller) catalogRequest = undefined;}
}
const catalogTimer = setInterval(refreshCatalog, 5000);

// Search, sort and the All / Live / Interrupted filters apply as you type:
// the page URL is updated in place and the Go-rendered catalog reconciled.
// Without script, the form and filter links still navigate.
const form = document.querySelector('[data-catalog-form]');
if (form) {
  let typing;
  const show = () => new URL(location.href).searchParams.get('show') ?? 'all';
  const apply = value => {
    const params = new URLSearchParams();
    const q = form.elements.q.value.trim();
    if (q) params.set('q', q);
    if (form.elements.sort.value !== 'newest') params.set('sort', form.elements.sort.value);
    if (value !== 'all') params.set('show', value);
    const hidden = form.querySelector('input[name="show"]');
    if (hidden && value === 'all') hidden.remove();
    else if (value !== 'all') {
      const input = hidden ?? Object.assign(document.createElement('input'), { type: 'hidden', name: 'show' });
      input.value = value;
      if (!hidden) form.append(input);
    }
    history.replaceState(null, '', '/' + (params.size ? '?' + params : ''));
    void refreshCatalog(true);
  };
  form.elements.q.addEventListener('input', () => { clearTimeout(typing); typing = setTimeout(() => apply(show()), 250); });
  form.elements.sort.addEventListener('change', () => apply(show()));
  form.addEventListener('submit', event => { event.preventDefault(); clearTimeout(typing); apply(show()); });
  document.addEventListener('click', event => {
    const link = event.target.closest('[data-catalog-filters] a[data-show]');
    if (!link || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    apply(link.dataset.show);
  });
}
