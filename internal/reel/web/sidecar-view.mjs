// Sidecar view (LIBRARY.md "Sidecar view"): the links lane under the player,
// synced panels in full-width tabs beside the Parent tab (or, in Compare,
// shown with the parent), the "Now running" grid and a child's "↑ parent" link.
// Go owns every link fact: /api/recordings/ID/links gives each link's anchor
// pairs (links.Alignment), placed stop and child duration, and this module
// only applies those pairs (child = pair.child + (parent − pair.parent)); it
// never derives an anchor. Text is written with textContent only; a path link
// is never named (the API has no path).
import { bounded, openRendererDocument, paintCast, previewCastMax } from '/assets/snapshot-renderer.mjs';

// An instant screenshot stays on screen this long after its anchor.
export const screenshotLingerSeconds = 5;
// At most this many panels (tabs besides Parent); opening another closes the
// oldest.
const panelsMax = 2;
// Live recordings refetch their links on the library's refresh cadence.
const liveRefreshMs = 5000;
// The grid recomputes and repaints at most this often while scrubbing.
const gridThrottleMs = 400;
// Stills are cached per recording and half-second position.
const stillQuantum = 0.5, stillCacheMax = 64;
// Compare shows the parent and a child side by side on a stage at least this
// wide (else stacked); it is not offered on narrow screens.
const compareSideMin = 1300;
const narrowQuery = matchMedia('(max-width: 960px)');

const rail = document.querySelector('#marks');
const stage = document.querySelector('.stage');
const playerBox = document.querySelector('#player');
const recording = rail?.dataset.recording;
const segment = Number(rail?.dataset.segment);
const liveMode = rail?.dataset.mode === 'live';
const liveRecording = !!document.querySelector('.viewer-bar .badge-live');
const mainFrame = () => document.querySelector('#terminal-frame');

const pad = n => String(n).padStart(2, '0');
const clock = seconds => {
  const s = Math.max(0, Math.round(seconds));
  return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${Math.floor(s / 60)}:${pad(s % 60)}`;
};
// Query-safe seconds: at most millisecond precision, Go's shortest form.
const seconds = value => String(Math.max(0, Math.floor(value * 1000) / 1000));
const element = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};
const getJSON = async url => {
  const response = await fetch(url, { cache: 'no-store', credentials: 'same-origin' });
  if (!response.ok) throw new Error(url);
  return response.json();
};
const timings = globalThis.agentReelSidecarTimings = { stills: [], batches: [] };
const note = (list, entry) => { list.push(entry); if (list.length > 256) list.shift(); };

// ---- Mapping: applying the API's anchor pairs --------------------------------
const earlier = (aSeg, aAt, bSeg, bAt) => aSeg < bSeg || (aSeg === bSeg && aAt < bAt);
const placedStop = link => link.stop && link.stop.seconds !== null && link.stop.segment >= 1 ? link.stop : null;
const pairFor = (link, seg) => link.anchors.find(p => p.parentSegment === seg) ?? null;
// childEnd is where the child's own clock ends: a recording's footage (a
// panel's pinned cutoff when known), a screenshot's linger; a video's is unknown.
const childEnd = (link, override) => link.kind === 'screenshot' ? screenshotLingerSeconds
  : link.kind === 'recording' ? (override ?? link.childSeconds ?? null) : null;
// toChild mirrors links.Alignment.ParentToChild, then bounds by the child's end.
// open says the end (a panel's pinned cutoff) froze footage the child may
// still extend: past it the state is 'beyond' (loaded is that cutoff), never
// 'after'. raw is the unbounded child time; stopped marks a placed stop.
export function toChild(link, seg, t, endOverride, open = false) {
  const pairs = link.anchors;
  if (!pairs.length) return { state: 'unplaced' };
  const first = pairs[0];
  if (earlier(seg, t, first.parentSegment, first.parentSeconds)) {
    return { state: 'before', startsIn: seg === first.parentSegment ? first.parentSeconds - t : null, startSegment: first.parentSegment };
  }
  const pair = pairFor(link, seg);
  const raw = pair ? pair.childSeconds + (t - pair.parentSeconds) : null;
  const end = childEnd(link, endOverride);
  const stop = placedStop(link);
  if (stop && earlier(stop.segment, stop.seconds, seg, t)) return { state: 'after', c: end, raw, stopped: true };
  if (!pair) return { state: 'unanchored' };
  if (raw < 0) return { state: 'before', startsIn: -raw, startSegment: seg };
  if (end !== null && raw > end) return open ? { state: 'beyond', c: end, raw, loaded: end } : { state: 'after', c: end, raw };
  return { state: 'on', c: raw };
}
// toParent mirrors links.Alignment.ChildToParent: the latest pair at or
// before c maps it, and whatever Go refuses is refused here with a reason,
// never clamped. 'on' is a valid {segment, at}; otherwise the state says why:
// 'before-start' (no pair yet, or a parent time before 0), 'unanchored' (the
// start is not placed, or with segment: the parent paused after that segment
// and nothing re-anchors the next one), 'after-stop' (past a placed stop,
// named before a segment end: the more useful reason; Go checks the end
// first), 'in-pause-gap' (the child ran on while the parent was paused:
// pausedAt and resumedAt are where the parent paused and resumed) and
// 'beyond-segment' (past a known segment end, with no later pair).
export function toParent(link, c, segmentEnds, issues = link.issues) {
  const known = issues ?? [];
  let best = null;
  for (const p of link.anchors) if (p.childSeconds <= c && (!best || p.childSeconds >= best.childSeconds)) best = p;
  if (!best) {
    // A resume pair can exist without a placed start (align.go).
    const first = link.anchors[0];
    return !first || known.includes('anchor-unplaced') || first.phase === 'resume' ? { state: 'unanchored' } : { state: 'before-start' };
  }
  const seg = best.parentSegment, at = best.parentSeconds + (c - best.childSeconds);
  if (at < 0) return { state: 'before-start' };
  const stop = placedStop(link);
  if (stop && earlier(stop.segment, stop.seconds, seg, at)) return { state: 'after-stop' };
  const end = segmentEnds[seg];
  if (end !== undefined && at > end) {
    if (known.includes(`segment-unanchored:${seg + 1}`)) return { state: 'unanchored', segment: seg };
    let next = null;
    for (const p of link.anchors) if (p.parentSegment > seg && (!next || p.parentSegment < next.parentSegment)) next = p;
    return next
      ? { state: 'in-pause-gap', pausedAt: { segment: seg, at: end }, resumedAt: { segment: next.parentSegment, at: next.parentSeconds }, precision: best.precision }
      : { state: 'beyond-segment', segment: seg };
  }
  return { state: 'on', segment: seg, at, precision: best.precision };
}
// Time labels of an estimated mapping carry "≈".
const approx = p => p.precision === 'estimated' ? '≈' : '';
// Why a child position has no parent position (the Move parent reason and
// the child page's "↑ parent" text).
function unmappedReason(p) {
  switch (p.state) {
    case 'before-start': return 'Before the link starts';
    case 'unanchored': return p.segment === undefined ? 'The link’s start is not placed' : 'Not re-anchored: the parent paused while this ran';
    case 'after-stop': return 'After the link’s stop';
    case 'beyond-segment': return 'Beyond the parent’s segment end';
    case 'beyond-parent': return 'Beyond the loaded parent footage';
    case 'in-pause-gap': return `The parent was paused here (segment ${p.pausedAt.segment} ended at ${clock(p.pausedAt.at)}; resumed in segment ${p.resumedAt.segment})`;
    default: return 'No parent position';
  }
}
// parentHref opens a parent's history at (seg, at) with the link's panel.
const parentHref = (parentID, seg, at, linkID) =>
  `/recordings/${parentID}?${new URLSearchParams({ mode: 'history', segment: String(seg), at: seconds(at) })}#${new URLSearchParams({ sidecar: linkID, tab: linkID })}`;
// spans lays one link out on segment seg of duration d: its bar (or pin) and,
// where the parent paused while the child ran on un-anchored, a hatched gap.
export function spans(link, seg, d, segmentEnds) {
  const first = link.anchors[0];
  const stop = placedStop(link);
  if (!first || first.parentSegment > seg || (stop && stop.segment < seg) || !(d > 0)) return [];
  const stopHere = stop && stop.segment === seg ? stop.seconds : null;
  const end = childEnd(link);
  const pair = pairFor(link, seg);
  if (pair) {
    const from = Math.min(d, Math.max(0, pair.parentSeconds));
    if (link.kind === 'screenshot') return from === pair.parentSeconds ? [{ from, to: from, pin: true }] : [];
    let to = d;
    if (stopHere !== null) to = Math.min(to, stopHere);
    if (end !== null) to = Math.min(to, pair.parentSeconds + (end - pair.childSeconds));
    return to > from ? [{ from, to }] : [];
  }
  if (link.kind === 'screenshot' || !link.issues.includes(`segment-unanchored:${seg}`)) return [];
  // The server reports the segment un-anchored; skip it when the child is
  // known to have ended before the pause.
  let prev = null;
  for (const p of link.anchors) if (p.parentSegment < seg) prev = p;
  const prevEnd = prev ? segmentEnds[prev.parentSegment] : undefined;
  if (prev && end !== null && prevEnd !== undefined && prev.parentSeconds + (end - prev.childSeconds) <= prevEnd) return [];
  return [{ from: 0, to: stopHere ?? d, gap: true }];
}

// ---- Stills: one reused renderer document, a bounded queue, a small cache ----
const stillCache = new Map();
let renderer = null, pending = new Map(), working = false;
function cloneSnapshot(image) {
  const copy = image.cloneNode(true);
  const from = image.querySelectorAll('canvas'), to = copy.querySelectorAll('canvas');
  to.forEach((canvas, i) => canvas.getContext('2d')?.drawImage(from[i], 0, 0));
  return copy;
}
const stillURL = (id, seg, at) => `/api/recordings/${id}/preview-at?segment=${seg}&at=${seconds(Math.floor(at / stillQuantum) * stillQuantum)}`;
// requestStill paints the still of recording id at (seg, at) and calls done
// with a fresh snapshot element (or null). A newer request for the same slot
// replaces an older pending one, so scrubbing never queues stale work.
function requestStill(slot, id, seg, at, done) {
  const url = stillURL(id, seg, at);
  const hit = stillCache.get(url);
  if (hit) { stillCache.delete(url); stillCache.set(url, hit); note(timings.stills, { ms: 0, cached: true }); done(cloneSnapshot(hit)); return; }
  pending.delete(slot);
  pending.set(slot, { url, done });
  while (pending.size > 12) pending.delete(pending.keys().next().value);
  void pump();
}
async function pump() {
  if (working) return;
  working = true;
  const batchStart = performance.now();
  let painted = 0;
  try {
    while (pending.size) {
      const [slot, job] = [...pending.entries()].at(-1);
      pending.delete(slot);
      const waiting = [job.done];
      for (const [other, next] of [...pending]) if (next.url === job.url) { waiting.push(next.done); pending.delete(other); }
      const cached = stillCache.get(job.url);
      if (cached) { for (const done of waiting) done(cloneSnapshot(cached)); continue; }
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 12000);
      const started = performance.now();
      let image = null;
      try {
        const response = await bounded(fetch(job.url, { signal: controller.signal, cache: 'no-store', credentials: 'same-origin' }), controller.signal);
        if (!response.ok || response.headers.get('AgentReel-Preview-Source') !== 'visual-view-at') {
          void response.body?.cancel().catch(() => {});
          throw new Error('Still unavailable');
        }
        const reader = response.body.getReader(); let size = 0; const chunks = [];
        try {
          for (;;) {
            const part = await bounded(reader.read(), controller.signal);
            if (part.done) break;
            size += part.value.byteLength;
            if (size > previewCastMax) throw new Error('Still response limit');
            chunks.push(part.value);
          }
        } finally { void reader.cancel().catch(() => {}); }
        const raw = await new Blob(chunks).text();
        renderer ??= openRendererDocument();
        const owner = renderer;
        try {
          image = await paintCast(owner, raw, controller.signal);
        } catch (error) {
          // An unclean paint retires the document (and its vendor work).
          if (renderer === owner) { owner.frame.remove(); renderer = null; }
          throw error;
        }
        stillCache.set(job.url, image);
        while (stillCache.size > stillCacheMax) stillCache.delete(stillCache.keys().next().value);
        painted++;
        note(timings.stills, { ms: Math.round(performance.now() - started), cached: false });
      } catch { image = null; } finally { clearTimeout(timer); }
      for (const done of waiting) done(image ? cloneSnapshot(image) : null);
    }
  } finally {
    // The renderer lives for one batch; an idle one is retired.
    renderer?.frame.remove(); renderer = null;
    working = false;
    if (painted) note(timings.batches, { stills: painted, ms: Math.round(performance.now() - batchStart) });
  }
}

// ---- Parent state -------------------------------------------------------------
// speed is the main player's (its 'speed' notify); pending, when set, is a
// position the parent was asked to move to (Move parent), used until its
// deadline or until the parent reports a time within 1 s of it.
const parent = { t: Number(rail?.dataset.at) || 0, duration: null, playing: false, speed: 1, pending: null };
const parentAt = () => parent.pending && performance.now() < parent.pending.until ? parent.pending.at : parent.t;
// History parent playback is gone: viewer-adapter removed the frame and says so.
const parentDown = () => !liveMode && (!mainFrame() || document.querySelector('#player-status')?.dataset.state === 'unavailable');
let links = [], segmentEnds = {}, segmentStart = null;
const byID = id => links.find(link => link.id === id) ?? null;
const listeners = new Set();
const changed = () => { for (const listener of listeners) listener(); };

async function loadFacts() {
  const [page, detail] = await Promise.all([getJSON(`/api/recordings/${recording}/links`), getJSON(`/api/recordings/${recording}`).catch(() => null)]);
  links = page.links.filter(link => Array.isArray(link.anchors));
  segmentEnds = {};
  for (const seg of detail?.segments ?? []) {
    const start = Date.parse(seg.recordedStart ?? ''), end = Date.parse(seg.recordedEnd ?? '');
    if (Number.isFinite(start) && Number.isFinite(end) && end > start) segmentEnds[seg.number] = (end - start) / 1000;
    if (seg.number === segment && Number.isFinite(start)) segmentStart = start;
  }
  return page;
}

// ---- Lane --------------------------------------------------------------------
const kinds = [['recording', 'Sub-recordings'], ['video', 'Videos'], ['screenshot', 'Screenshots']];
let lane, tracks, playhead, tip, gridToggle;
function buildLane() {
  lane = element('section', 'sidecar-lane');
  lane.dataset.sidecarLane = '';
  lane.setAttribute('aria-label', 'Linked sidecars on this segment');
  const head = element('div', 'sidecar-lane-head');
  head.append(element('span', 'sidecar-lane-title', `Links · segment ${segment}`));
  gridToggle = element('button', 'btn btn-small sidecar-grid-toggle', 'Now running');
  gridToggle.type = 'button'; gridToggle.dataset.sidecarGridToggle = '';
  gridToggle.setAttribute('aria-pressed', 'false');
  gridToggle.title = 'Show every link active at the playhead';
  gridToggle.addEventListener('click', () => setGrid(!gridOn));
  head.append(gridToggle);
  const body = element('div', 'sidecar-lane-body');
  tracks = new Map();
  for (const [kind, label] of kinds) {
    const row = element('div', 'sidecar-row');
    row.dataset.sidecarKind = kind;
    const track = element('div', 'sidecar-track');
    row.append(element('span', 'sidecar-row-label', label), track);
    body.append(row);
    tracks.set(kind, { row, track });
  }
  playhead = element('span', 'sidecar-playhead');
  playhead.setAttribute('aria-hidden', 'true');
  tip = element('div', 'sidecar-tip');
  tip.hidden = true; tip.setAttribute('role', 'tooltip');
  body.append(playhead);
  lane.append(head, body, tip);
  document.querySelector('#player').after(lane);
  body.addEventListener('click', event => {
    const item = event.target.closest('[data-sidecar-link]');
    if (item) { openPanel(item.dataset.sidecarLink); return; }
    const track = event.target.closest('.sidecar-track');
    const frame = mainFrame();
    if (!track || liveMode || !parent.duration || !frame?.contentWindow) return;
    const r = track.getBoundingClientRect();
    const at = Math.max(0, Math.min(parent.duration, (event.clientX - r.left) / r.width * parent.duration));
    frame.contentWindow.postMessage({ agentReel: 'seek', at }, location.origin);
  });
  body.addEventListener('pointermove', hover);
  body.addEventListener('pointerleave', () => { tip.hidden = true; hoverKey = ''; });
  new ResizeObserver(align).observe(lane);
}
// align lines the tracks up under the main player's own scrubber.
function align() {
  let left = 150, right = 16;
  try {
    const frame = mainFrame(), bar = frame?.contentDocument?.querySelector('.ap-progressbar .ap-bar');
    if (bar) {
      const f = frame.getBoundingClientRect(), b = bar.getBoundingClientRect(), l = lane.getBoundingClientRect();
      const barLeft = f.left + b.left - l.left, barRight = l.right - (f.left + b.right);
      if (barLeft >= 60 && barRight >= 0 && barLeft + barRight < l.width - 80) { left = barLeft; right = barRight; }
    }
  } catch { /* The default gutter applies. */ }
  lane.style.setProperty('--sidecar-track-left', `${Math.round(left)}px`);
  lane.style.setProperty('--sidecar-track-right', `${Math.round(right)}px`);
}
const duration = () => parent.duration ?? (liveMode ? parent.t : null);
const percent = (value, d) => `${(Math.max(0, Math.min(d, value)) / d * 100).toFixed(4)}%`;
function renderLane() {
  const d = duration();
  const counts = { recording: 0, video: 0, screenshot: 0 };
  for (const { track } of tracks.values()) track.replaceChildren();
  lane.dataset.duration = d ?? '';
  for (const link of links) {
    const entry = tracks.get(link.kind);
    if (!entry || !d) continue;
    const laid = spans(link, segment, d, segmentEnds);
    if (!laid.length) continue;
    const row = link.kind === 'screenshot' ? 0 : counts[link.kind]++;
    for (const span of laid) {
      const item = element('button', span.pin ? 'sidecar-pin' : span.gap ? 'sidecar-bar sidecar-gap' : 'sidecar-bar');
      item.type = 'button';
      item.dataset.sidecarLink = link.id;
      item.dataset.from = String(Math.round(span.from * 1000) / 1000);
      item.dataset.to = String(Math.round(span.to * 1000) / 1000);
      if (span.gap) item.dataset.sidecarGap = '';
      if (span.pin) item.dataset.sidecarPin = '';
      item.style.left = percent(span.from, d);
      if (!span.pin) item.style.width = percent(span.to - span.from, d);
      item.style.setProperty('--sidecar-stack', String(row));
      item.setAttribute('aria-label', `${link.label} · ${link.kind}${span.gap ? ' · parent paused, not re-anchored' : ''}`);
      if (!span.pin) item.append(element('span', 'sidecar-bar-label', link.label));
      entry.track.append(item);
    }
  }
  for (const [kind, { row }] of tracks) {
    row.hidden = !row.querySelector('[data-sidecar-link]');
    row.style.setProperty('--sidecar-rows', String(Math.max(1, kind === 'screenshot' ? 1 : counts[kind])));
  }
  lane.dataset.empty = lane.querySelector('[data-sidecar-link]') ? 'false' : 'true';
  movePlayhead();
}
function movePlayhead() {
  const d = duration();
  playhead.hidden = !d;
  if (d) playhead.style.setProperty('--sidecar-at', (Math.max(0, Math.min(d, parent.t)) / d).toFixed(5));
}
// Hover: label, duration and a still of the link at the hovered time.
let hoverKey = '', hoverTimer;
function hover(event) {
  const item = event.target.closest('[data-sidecar-link]');
  const d = duration();
  if (!item || !d) { tip.hidden = true; hoverKey = ''; return; }
  const link = byID(item.dataset.sidecarLink);
  const track = item.closest('.sidecar-track').getBoundingClientRect();
  const at = Math.max(0, Math.min(d, (event.clientX - track.left) / track.width * d));
  const from = Number(item.dataset.from), to = Number(item.dataset.to);
  const box = lane.getBoundingClientRect();
  tip.style.left = `${Math.max(0, Math.min(box.width - 240, event.clientX - box.left - 120))}px`;
  tip.hidden = false;
  const key = `${link.id}@${Math.floor(at / stillQuantum)}`;
  if (key === hoverKey) return;
  hoverKey = key;
  const m = toChild(link, segment, at);
  const lines = [element('strong', '', link.label), element('span', 'muted', item.dataset.sidecarGap !== undefined
    ? `${link.kind} · parent paused; not re-anchored here`
    : link.kind === 'screenshot' ? `screenshot at ${clock(from)}` : `${link.kind} · ${clock(from)}–${clock(to)} (${clock(to - from)})`)];
  const thumb = element('div', 'sidecar-thumb');
  tip.replaceChildren(...lines, thumb);
  if (link.kind === 'screenshot' && link.target.type === 'asset') {
    const img = element('img'); img.alt = ''; img.src = link.target.url; thumb.append(img);
  } else if (link.kind === 'video') {
    thumb.append(element('span', 'sidecar-note', 'Local video file (not served)'));
  } else if (link.target.type === 'recording' && m.state === 'on') {
    thumb.append(element('span', 'sidecar-note', `child ${clock(m.c)}`));
    clearTimeout(hoverTimer);
    hoverTimer = setTimeout(() => requestStill('hover', link.target.recording, 1, m.c, image => {
      if (hoverKey !== key || !image) return;
      thumb.replaceChildren(image);
    }), 120);
  } else {
    thumb.hidden = true;
  }
}

// ---- Panels ------------------------------------------------------------------
const panels = [];
let panelBox;
function hashState() {
  const params = new URLSearchParams(location.hash.slice(1));
  return { ids: (params.get('sidecar') ?? '').split(',').filter(Boolean), grid: params.get('grid') === '1', tab: params.get('tab'), compare: params.get('compare') === '1' };
}
function writeHash() {
  const params = new URLSearchParams(location.hash.slice(1));
  const ids = panels.map(panel => panel.link.id);
  if (ids.length) params.set('sidecar', ids.join(',')); else params.delete('sidecar');
  // The selected panel's tab; Parent is the default and is never written.
  if (selected) params.set('tab', selected.link.id); else params.delete('tab');
  if (gridOn) params.set('grid', '1'); else params.delete('grid');
  if (comparing) params.set('compare', '1'); else params.delete('compare');
  const hash = params.toString();
  history.replaceState(history.state, '', location.pathname + location.search + (hash ? '#' + hash : ''));
}

// ---- Tabs: Parent and one per open panel, one view at a time ------------------
// The selected view fills the stage's view row; every other view stays laid
// out (align() measures the parent's scrubber) but is invisible and inert, so
// it never takes a click, a tap or focus. Never hidden or display:none.
// The tab bar holds the tablist (which owns tabs only) and, after it, Compare.
let tabbar, tablist, tabsAnnounce, parentTab, tabTools, compareButton, closeHint;
let selected = null; // the selected panel; null is the Parent tab
// Compare (explicit only, never entered by itself) shows the parent beside
// the selected child tab; beforeCompare is the tab selected before it.
let comparing = false, beforeCompare = null;
const tabs = () => [parentTab, ...panels.map(panel => panel.tab)];
const conceal = (node, on) => { node.classList.toggle('sidecar-hidden', on); node.inert = on; };
// say speaks on the tablist's announcer. Cleared first, so the same words
// twice in a row ("Closed worker A" again) are a change that is heard again.
function say(text) {
  tabsAnnounce.textContent = '';
  queueMicrotask(() => { tabsAnnounce.textContent = text; });
}
function tabButton(key, id, controls, text) {
  const tab = element('button', 'sidecar-tab', text);
  tab.type = 'button'; tab.id = id; tab.dataset.sidecarTab = key;
  tab.setAttribute('role', 'tab');
  tab.setAttribute('aria-controls', controls);
  tab.setAttribute('aria-selected', 'false');
  tab.tabIndex = -1;
  return tab;
}
// tabItem wraps a tab (and a link tab's ×) for layout only: it is presentation,
// so the tablist owns nothing but tabs.
function tabItem() {
  const item = element('div', 'sidecar-tab-item');
  item.setAttribute('role', 'presentation');
  return item;
}
function buildTabs() {
  tabbar = element('div', 'sidecar-tabbar');
  tabbar.dataset.sidecarTabbar = '';
  tablist = element('div', 'sidecar-tabs');
  tablist.dataset.sidecarTabs = '';
  tablist.setAttribute('role', 'tablist');
  tablist.setAttribute('aria-label', 'Recording and linked sidecars');
  // A link ID may be "parent": the Parent tab's element ID cannot collide.
  parentTab = tabButton('parent', 'sidecar-parent-tab', playerBox.id, 'Parent');
  // In Compare the parent already shows (its tab is aria-disabled).
  parentTab.addEventListener('click', () => { if (!comparing) selectTab(null, { focus: true }); });
  const item = tabItem();
  item.append(parentTab);
  tablist.append(item);
  // Describes each link tab (aria-describedby reads a hidden element).
  closeHint = element('span', '', 'Delete closes this tab');
  closeHint.id = 'sidecar-tab-close-hint'; closeHint.hidden = true;
  // After the tablist, outside it: Compare (never on a narrow screen).
  tabTools = element('div', 'sidecar-tab-tools');
  tabbar.append(tablist, closeHint, tabTools);
  tablist.addEventListener('keydown', tabKey);
  compareButton = element('button', 'btn btn-small sidecar-compare', 'Compare');
  compareButton.type = 'button'; compareButton.dataset.sidecarCompare = '';
  compareButton.setAttribute('aria-pressed', 'false');
  compareButton.title = 'Show the parent and the selected tab together';
  compareButton.addEventListener('click', () => setCompare(!comparing));
  narrowQuery.addEventListener('change', () => {
    // A narrow screen ends Compare; widening again never restarts it. Focus
    // in a view that ending Compare conceals moves to the selected tab.
    if (narrowQuery.matches && comparing) {
      const focused = document.activeElement;
      setCompare(false);
      if (focused && focused !== document.body && focused.closest('[inert]')) tablist.querySelector('[role="tab"][aria-selected="true"]')?.focus();
    }
    placeCompare();
  });
  // Closing a tab (to make room, or by Delete) and Compare's layout are said
  // here (after the tab bar, outside the tablist).
  tabsAnnounce = element('span', 'sr-only');
  tabsAnnounce.dataset.sidecarAnnounce = ''; tabsAnnounce.setAttribute('role', 'status');
}
// Arrow keys (wrapping), Home and End move to a tab and select it; Enter and
// Space select the focused tab (its click). An aria-disabled tab (Parent, in
// Compare) is skipped. Delete (or Backspace) closes a focused link tab, as its
// × does, and says so; the Parent tab cannot be closed.
function tabKey(event) {
  const all = tabs(), i = all.indexOf(event.target);
  if (i < 0 || event.altKey || event.ctrlKey || event.metaKey) return;
  if (event.key === 'Delete' || event.key === 'Backspace') {
    const panel = panels.find(p => p.tab === event.target);
    if (!panel) return;
    event.preventDefault();
    // One press closes one tab: a held key's repeats do not close its neighbours.
    if (event.repeat) return;
    closePanel(panel, { spoken: true, focus: true });
    return;
  }
  const list = all.filter(tab => tab.getAttribute('aria-disabled') !== 'true');
  const step = by => {
    for (let k = 1; k <= all.length; k++) {
      const tab = all[((i + by * k) % all.length + all.length) % all.length];
      if (list.includes(tab)) return tab;
    }
    return null;
  };
  const next = { ArrowRight: step(1), ArrowLeft: step(-1), Home: list[0], End: list.at(-1) }[event.key];
  if (!next) return;
  event.preventDefault();
  selectTab(panels.find(panel => panel.tab === next) ?? null, { focus: true });
}
// The Parent tab names the main player's state (viewer-adapter's
// #player-status); a Live parent that plays is "Live".
const parentStates = { playing: 'Playing', paused: 'Paused', loading: 'Loading', ended: 'Ended', unavailable: 'Unavailable' };
function renderParentTab() {
  if (!parentTab) return;
  const state = document.querySelector('#player-status')?.dataset.state;
  const word = liveMode && state === 'playing' ? 'Live' : parentStates[state];
  setText(parentTab, word ? `Parent · ${word}` : 'Parent');
}
// selectTab shows one view (focusing its tab only for the tablist's own
// keys and clicks, never for an open from the lane, grid or hash). In Compare
// the parent shows too, and the selected tab is always a child's.
function selectTab(panel, { focus = false } = {}) {
  selected = panel && panels.includes(panel) ? panel : null;
  if (comparing && !selected) selected = panels.at(-1) ?? null;
  const showing = selected?.tab ?? parentTab;
  for (const tab of tabs()) {
    tab.setAttribute('aria-selected', String(tab === showing));
    tab.tabIndex = tab === showing ? 0 : -1;
  }
  setAttr(parentTab, 'aria-disabled', comparing ? 'true' : null);
  for (const p of panels) conceal(p.el, p !== selected);
  conceal(playerBox, selected !== null && !comparing);
  // The panels follow the player in tree order and would sit on top of it.
  conceal(panelBox, selected === null && panels.length > 0);
  writeHash();
  requestAnimationFrame(align);
  if (focus) showing.focus();
}
// layout switches the stage to tabs while a panel is open; with none it is
// the plain player again.
function layout() {
  const on = panels.length > 0;
  stage.classList.toggle('sidecar-tabs', on);
  if (on && !tabbar.isConnected) {
    playerBox.before(tabbar);
    // A close said as the last tab went (Delete) is not said again.
    tabsAnnounce.textContent = '';
    tabbar.after(tabsAnnounce);
    playerBox.setAttribute('role', 'tabpanel');
    playerBox.setAttribute('aria-labelledby', parentTab.id);
    renderParentTab();
    placeCompare();
  } else if (!on && tabbar.isConnected) {
    tabbar.remove(); tabsAnnounce.remove();
    playerBox.removeAttribute('role'); playerBox.removeAttribute('aria-labelledby');
  }
  requestAnimationFrame(align);
}

// ---- Compare: the parent and the selected child together ------------------------
// Only ever entered by its toggle (or a hash that says so). The stage's width
// picks the layout (data-compare): 'side' by side, else 'stack'ed with the
// page scrolling and each player at least half the viewport tall.
// placeCompare offers the toggle only where Compare fits (not on a narrow
// screen, where it is not in the document at all).
function placeCompare() {
  if (!narrowQuery.matches) { if (!compareButton.isConnected) tabTools.append(compareButton); return; }
  if (compareButton.contains(document.activeElement)) (selected?.tab ?? parentTab).focus();
  compareButton.remove();
}
function setCompare(on) {
  on = on && panels.length > 0 && !narrowQuery.matches;
  if (on === comparing) return;
  comparing = on;
  compareButton.setAttribute('aria-pressed', String(on));
  if (on) {
    beforeCompare = selected;
    fitCompare();
    selectTab(selected);
    return;
  }
  delete stage.dataset.compare;
  // Back to the tab shown before Compare (if it is still open).
  const back = beforeCompare;
  beforeCompare = null;
  selectTab(back === null || panels.includes(back) ? back : selected);
}
// fitCompare picks the layout for the stage's current width (its
// ResizeObserver re-evaluates it while Compare is on) and says it.
function fitCompare() {
  if (!comparing) return;
  const mode = stage.clientWidth >= compareSideMin ? 'side' : 'stack';
  if (stage.dataset.compare === mode) return;
  stage.dataset.compare = mode;
  say(mode === 'side' ? 'Comparing side by side' : 'Comparing stacked');
  requestAnimationFrame(align);
}
// A recording panel has three modes (data-mode): 'follow' mirrors the parent
// (the child frame hides its controls and swallows input), 'explore' leaves
// the child to the user (no seek, play, pause or speed from here), 'live'
// plays the child's own Live stream in the panel (never navigating the page).
// Other panels always follow.
const actionButton = (text, data) => {
  const node = element('button', 'sidecar-action', text);
  node.type = 'button'; node.dataset[data] = '';
  return node;
};
function panelControls(panel) {
  const mode = actionButton('Explore child', 'sidecarModeToggle');
  mode.addEventListener('click', () => { if (panel.mode === 'follow') explore(panel); else if (panel.mode === 'explore') follow(panel); });
  const live = actionButton('Live', 'sidecarLive');
  live.setAttribute('aria-pressed', 'false');
  live.addEventListener('click', () => { if (panel.mode === 'live') void repin(panel); else goLive(panel); });
  const still = actionButton('Show latest saved screen', 'sidecarStill');
  still.addEventListener('click', () => showStill(panel));
  const retry = actionButton('Retry Live', 'sidecarLiveRetry');
  retry.addEventListener('click', () => goLive(panel));
  const back = actionButton('Back to history', 'sidecarHistory');
  back.addEventListener('click', () => void repin(panel));
  const newer = actionButton('Load newer footage', 'sidecarLoadNewer');
  newer.addEventListener('click', () => void loadNewer(panel));
  // Move parent (explore only): its controls are made by updateMove.
  const reason = element('span', 'sidecar-move-reason');
  reason.dataset.sidecarMoveReason = ''; reason.id = `sidecar-move-reason-${++moveReasons}`;
  const move = { main: null, paused: null, resumed: null, reason };
  return { mode, live, still, retry, back, newer, move };
}
let moveReasons = 0;
// place puts exactly the want nodes, in order, into the panel's actions,
// moving none that stays (a focused control keeps its focus).
function place(panel, want) {
  for (const node of [...panel.actions.children]) if (!want.includes(node)) node.remove();
  let previous = null;
  for (const node of want) {
    if (node.parentNode !== panel.actions || node.previousElementSibling !== previous) {
      if (previous) previous.after(node); else panel.actions.prepend(node);
    }
    previous = node;
  }
}
// renderControls shows the header controls the panel's mode and state allow.
function renderControls(panel) {
  if (!panel.controls) return;
  const { mode, live, still, retry, back, newer } = panel.controls;
  const isLive = panel.mode === 'live';
  mode.textContent = panel.mode === 'explore' ? 'Follow parent' : 'Explore child';
  mode.title = panel.mode === 'explore' ? 'Follow the parent’s playhead again' : 'Play, pause and seek this recording on its own';
  mode.disabled = isLive || panel.failed || !panel.frame;
  live.setAttribute('aria-pressed', String(isLive));
  live.textContent = isLive ? 'Live · not synced to parent' : 'Live';
  live.title = isLive ? 'Back to this recording’s history, following the parent' : 'Play this recording’s Live stream in the panel';
  newer.textContent = panel.newerFailed ? 'Retry' : 'Load newer footage';
  newer.title = panel.newerFailed ? 'Retry loading newer footage' : 'Load what this recording has saved since its panel pinned it';
  newer.disabled = panel.newerBusy;
  const want = [mode, ...updateMove(panel)];
  if (panel.childLive || isLive) want.push(live);
  if (isLive && panel.el.dataset.state === 'live-unavailable') want.push(still, retry, back);
  // Only a following panel past its pinned, still-open footage offers it.
  if (panel.mode === 'follow' && panel.el.dataset.state === 'beyond') want.push(newer);
  place(panel, want);
}

// ---- Move parent: where the exploring child is, honestly -----------------------
// childNow extrapolates the child's last reported position (sync's expected).
const childNow = (panel, now = performance.now()) => panel.childTime === null ? null
  : panel.childTime + (panel.childPlaying ? (now - panel.childAt) / 1000 * (panel.speed ?? 1) : 0);
// seekable says whether this page can seek its own parent player to
// (seg, at): a history parent showing seg with its footage loaded that far.
const seekable = (seg, at) => seg === segment && !liveMode && parent.duration !== null && at <= parent.duration && !!mainFrame() && !parentDown();
// moveTarget is how to put the parent at (seg, at): a seek in this page, or a
// link to the parent's history there.
const moveTarget = (seg, at) => ({ kind: seekable(seg, at) ? 'seek' : 'link', segment: seg, at });
// moveOutcome is what Move parent can honestly do for the exploring child's
// position: 'seek' or 'link' to its parent position, 'invalid' with the
// mapping's reason, or 'gap' (the parent was paused there) with a target
// where it paused and one where it resumed.
function moveOutcome(panel) {
  const c = childNow(panel);
  if (c === null) return { kind: 'invalid', state: 'waiting', reason: 'Waiting for the child’s position' };
  const p = toParent(panel.link, c, segmentEnds);
  if (p.state === 'on') {
    // Past the footage this history page loaded of the viewed segment.
    if (p.segment === segment && !liveMode && parent.duration !== null && p.at > parent.duration) return { kind: 'invalid', state: 'beyond-parent', reason: unmappedReason({ state: 'beyond-parent' }) };
    return { ...moveTarget(p.segment, p.at), precision: p.precision };
  }
  if (p.state !== 'in-pause-gap') return { kind: 'invalid', state: p.state, reason: unmappedReason(p) };
  // Where it paused is bounded by the loaded footage of the viewed segment
  // (the segment end is its wall clock, which can outlast its footage).
  const pausedHere = p.pausedAt.segment === segment && !liveMode && parent.duration !== null;
  const pausedAt = pausedHere ? Math.min(p.pausedAt.at, parent.duration) : p.pausedAt.at;
  const resumedHere = p.resumedAt.segment === segment && !liveMode && parent.duration !== null;
  const resumedAt = resumedHere ? Math.min(p.resumedAt.at, parent.duration) : p.resumedAt.at;
  return {
    kind: 'gap', state: p.state, reason: unmappedReason(p),
    paused: { ...moveTarget(p.pausedAt.segment, pausedAt), clamped: pausedAt < p.pausedAt.at },
    resumed: { ...moveTarget(p.resumedAt.segment, resumedAt), clamped: resumedAt < p.resumedAt.at },
  };
}
// moveNode keeps one Move parent control up to date in place: a button
// (seek, or aria-disabled with its reason) or a link. The element is replaced
// only when that kind changes, and then keeps the focus it had.
function moveNode(panel, node, which, tag, data) {
  if (node?.localName === tag) return node;
  const next = element(tag, 'sidecar-action sidecar-move');
  next.dataset[data] = '';
  next.dataset.sidecarMovePart = which;
  if (tag === 'button') {
    next.type = 'button';
    next.addEventListener('click', () => moveParent(panel, which, next));
  } else if (which === 'main') {
    next.dataset.sidecarMoveLink = '';
  }
  if (node?.isConnected) {
    const focused = node.contains(document.activeElement);
    node.replaceWith(next);
    // After dress: a link is focusable only once it has its href.
    if (focused) queueMicrotask(() => next.focus());
  }
  return next;
}
const setText = (node, text) => { if (node.textContent !== text) node.textContent = text; };
const setAttr = (node, name, value) => {
  if (value === null) node.removeAttribute(name);
  else if (node.getAttribute(name) !== value) node.setAttribute(name, value);
};
// dress writes a target onto its control (a seek button or a link), or, with
// no target, marks it aria-disabled and described by the reason.
function dress(panel, node, target, text, title, reason = null) {
  setText(node, text);
  setAttr(node, 'title', title);
  setAttr(node, 'aria-disabled', target ? null : 'true');
  setAttr(node, 'aria-describedby', target ? null : panel.controls.move.reason.id);
  setAttr(node, 'data-reason', reason);
  setAttr(node, 'data-segment', target ? String(target.segment) : null);
  setAttr(node, 'data-at', target ? seconds(target.at) : null);
  if (node.localName === 'a') setAttr(node, 'href', parentHref(recording, target.segment, target.at, panel.link.id));
}
// updateMove brings the Move parent controls up to date and returns the ones
// the header shows: none unless the panel is exploring.
function updateMove(panel) {
  const m = panel.controls?.move;
  if (!m) return [];
  if (panel.mode !== 'explore' || panel.failed || !panel.frame) { m.reason.remove(); return []; }
  const o = moveOutcome(panel);
  // A gap control leaving with its gap hands its focus to the main control.
  if (o.kind !== 'gap' && [m.paused, m.resumed].some(node => node?.isConnected && node.contains(document.activeElement))) {
    queueMicrotask(() => m.main?.focus());
  }
  m.main = moveNode(panel, m.main, 'main', o.kind === 'link' ? 'a' : 'button', 'sidecarMove');
  if (o.kind === 'seek') dress(panel, m.main, o, 'Move parent here', `Move the parent to ${approx(o)}${clock(o.at)} and follow it`);
  else if (o.kind === 'link') dress(panel, m.main, o, `Open parent at ${o.segment} · ${approx(o)}${clock(o.at)}`, 'Open the parent’s history at this moment');
  // Focusable, and says why nothing happens.
  else dress(panel, m.main, null, 'Move parent here', null, o.state);
  const nodes = [m.main];
  if (o.kind === 'gap') {
    for (const [which, data, label] of [['paused', 'sidecarMovePaused', 'Parent where it paused'], ['resumed', 'sidecarMoveResumed', 'Parent where it resumed']]) {
      const target = o[which];
      m[which] = moveNode(panel, m[which], which, target.kind === 'link' ? 'a' : 'button', data);
      dress(panel, m[which], target, `${label}${target.clamped ? ' (end of saved footage)' : ''}`,
        target.kind === 'link' ? `Open the parent’s history at ${target.segment} · ${clock(target.at)}` : `Move the parent to ${clock(target.at)} and follow it`);
      nodes.push(m[which]);
    }
  }
  if (o.kind === 'invalid' || o.kind === 'gap') {
    setText(m.reason, o.reason);
    // Read right after the controls it describes (CSS order shows it last).
    if (m.reason.previousElementSibling !== panel.actions) panel.actions.after(m.reason);
  } else {
    m.reason.remove();
  }
  return nodes;
}
// renderMove recomputes the Move parent controls (a child report or parent
// tick) without touching the panel's other controls.
function renderMove(panel) {
  if (!panel.controls || panel.mode !== 'explore') return;
  const moves = updateMove(panel);
  const others = [...panel.actions.children].filter(node => node.dataset.sidecarMovePart === undefined);
  place(panel, [others[0], ...moves, ...others.slice(1)].filter(Boolean));
}
// moveParent acts on a Move parent button: a seek moves this page's parent
// and follows it again (the child is where it is, so nothing jumps back); an
// aria-disabled control does nothing. Links navigate by themselves. A target
// that changed since it was shown is re-shown, not acted on.
function moveParent(panel, which, node) {
  if (panel.mode !== 'explore') return;
  const o = moveOutcome(panel);
  const target = which === 'main' ? o : o.kind === 'gap' ? o[which] : null;
  if (node.getAttribute('aria-disabled') === 'true' || target?.kind !== 'seek') { renderMove(panel); return; }
  mainFrame().contentWindow.postMessage({ agentReel: 'seek', at: target.at }, location.origin);
  // Until the parent reports it, sync maps from where it was asked to be.
  parent.pending = { at: target.at, until: performance.now() + 1500 };
  const focused = node.contains(document.activeElement);
  follow(panel);
  // The move controls leave with exploring: the focus stays in the header.
  if (focused) panel.controls.mode.focus();
  changed();
}
// announce speaks mode transitions only (never a countdown).
const announce = (panel, text) => { if (panel.announce.textContent !== text) panel.announce.textContent = text; };
function setMode(panel, mode) {
  panel.mode = mode;
  panel.el.dataset.mode = mode;
  delete panel.el.dataset.after;
  if (panel.frame && mode !== 'live') panel.frame.dataset.present = mode;
  if (panel.status && mode !== 'explore') panel.status.textContent = '';
  renderControls(panel);
}
// headNote writes feedback in a following panel's head (sync's explore
// branch owns that text while exploring); a timed note clears itself.
function headNote(panel, text, ms = 0) {
  clearTimeout(panel.headTimer);
  if (!panel.status || panel.mode !== 'follow') return;
  panel.status.textContent = text;
  if (ms) panel.headTimer = setTimeout(() => { if (panel.mode === 'follow' && panel.status.textContent === text) panel.status.textContent = ''; }, ms);
}
// Entering or leaving 'beyond' resets its "Load newer footage" offer and that
// offer's feedback.
function beyondChanged(panel) {
  panel.newerFailed = false;
  headNote(panel, '');
  renderControls(panel);
}
function openPanel(id) {
  const link = byID(id);
  if (!link) return;
  const existing = panels.find(panel => panel.link.id === id);
  if (existing) { selectTab(existing); existing.el.scrollIntoView({ block: 'nearest' }); return; }
  // Making room is announced: the oldest tab closes.
  while (panels.length >= panelsMax) closePanel(panels[0], { spoken: true });
  const el = element('section', 'sidecar-panel');
  el.dataset.sidecarPanel = id; el.dataset.kind = link.kind; el.dataset.mode = 'follow';
  el.id = `sidecar-panel-${id}`;
  el.setAttribute('role', 'tabpanel');
  el.setAttribute('aria-labelledby', `sidecar-tab-${id}`);
  const head = element('header', 'sidecar-panel-head');
  // Its tab, closed by Delete while focused or by a pointer on the × beside it
  // (never inside the tab; hidden from assistive technology and never
  // focusable, so the tablist owns tabs only). The panel's own header has a
  // real close button.
  const tab = tabButton(id, `sidecar-tab-${id}`, el.id, `${link.label} · ${link.kind}`);
  tab.setAttribute('aria-keyshortcuts', 'Delete Backspace');
  tab.setAttribute('aria-describedby', closeHint.id);
  const tabClose = element('span', 'sidecar-tab-close', '×');
  tabClose.dataset.sidecarTabClose = ''; tabClose.title = 'Close';
  tabClose.setAttribute('aria-hidden', 'true');
  const item = tabItem();
  item.append(tab, tabClose);
  const close = element('button', 'sidecar-close', '×');
  close.type = 'button'; close.dataset.sidecarClose = ''; close.title = 'Close'; close.setAttribute('aria-label', `Close ${link.label}`);
  const title = element('span', 'sidecar-panel-title', link.label);
  head.append(title, element('span', 'sidecar-panel-kind', link.kind));
  const body = element('div', 'sidecar-panel-body');
  const overlay = element('p', 'sidecar-overlay');
  overlay.dataset.sidecarOverlay = '';
  // Transitions are announced here; the overlay's countdown is not.
  const spoken = element('span', 'sr-only');
  spoken.dataset.sidecarAnnounce = ''; spoken.setAttribute('role', 'status');
  const panel = {
    link, el, tab, tabItem: item, body, overlay, announce: spoken, status: null, actions: null, controls: null,
    frame: null, frameURL: null, img: null, video: null, duration: null, loaded: false,
    childTime: null, childAt: 0, childPlaying: false, wantPlay: null, ended: false, speed: null, speedPosted: null,
    mode: 'follow', exploreFrom: 0, child: null, childLive: false, liveFrame: null, failed: false, pinnedOpen: false, repinning: false,
    newerBusy: false, newerFailed: false, headTimer: null,
  };
  if (link.kind === 'recording') {
    panel.status = element('span', 'sidecar-panel-status');
    panel.status.dataset.sidecarStatus = '';
    panel.actions = element('span', 'sidecar-panel-actions');
    panel.controls = panelControls(panel);
    head.append(panel.status, panel.actions);
    renderControls(panel);
  }
  if (link.target.type === 'recording') {
    const open = element('a', 'sidecar-panel-open', 'Open');
    open.href = `/recordings/${link.target.recording}`;
    head.append(open);
  }
  head.append(close);
  el.append(head, body, spoken);
  close.addEventListener('click', () => closePanel(panel));
  // The × takes no focus: its click moves the focus as the header's close does.
  tabClose.addEventListener('click', () => closePanel(panel, { focus: true }));
  tab.addEventListener('click', () => selectTab(panel, { focus: true }));
  if (link.kind === 'recording') {
    body.append(overlay);
    void loadFrame(panel);
  } else if (link.kind === 'screenshot' && link.target.type === 'asset') {
    panel.img = element('img', 'sidecar-shot');
    panel.img.alt = link.label; panel.img.src = link.target.url; panel.img.dataset.sidecarShot = '';
    body.append(panel.img, overlay);
  } else if (link.target.type === 'asset' && /^video\//.test(link.target.contentType ?? '')) {
    panel.video = element('video', 'sidecar-video');
    panel.video.src = link.target.url; panel.video.muted = true; panel.video.preload = 'auto';
    body.append(panel.video, overlay);
  } else {
    const note = element('div', 'sidecar-placeholder');
    note.dataset.sidecarPlaceholder = '';
    note.append(element('strong', '', link.label), element('span', '', `${link.kind} · local file, referenced in place and never served; open it with the CLI (areel link ls)`));
    body.append(note, overlay);
  }
  panels.push(panel);
  panelBox.append(el);
  tablist.append(item);
  layout();
  selectTab(panel);
  sync(panel);
}
// closePanel removes a panel and its tab; closing the selected one selects
// its neighbour (the next tab, else the previous, else Parent). spoken says
// "Closed <label>" (making room, or Delete on its tab); focus moves the focus
// as closing from inside its tab or panel does (the × never holds it).
function closePanel(panel, { spoken = false, focus = false } = {}) {
  const i = panels.indexOf(panel);
  if (i < 0) return;
  const focused = focus || panel.el.contains(document.activeElement) || panel.tabItem.contains(document.activeElement);
  panels.splice(i, 1);
  panel.frame?.remove();
  panel.el.remove();
  panel.tabItem.remove();
  if (spoken) say(`Closed ${panel.link.label}`);
  // With no child left there is nothing to compare.
  if (!panels.length) setCompare(false);
  layout();
  selectTab(selected === panel ? panels[i] ?? panels[i - 1] ?? null : selected, { focus: focused && panels.length > 0 });
  // With no tab left the focus returns to the link's lane bar.
  if (focused && !panels.length) lane.querySelector(`[data-sidecar-link="${CSS.escape(panel.link.id)}"]`)?.focus();
}
// setFrameSource replaces the panel's frame with a new element. Inserted
// first, then given its one src (as the viewer does its own frame): that
// first navigation replaces the initial about:blank, so switching sources
// never adds a session history entry (a second src write would). Messages
// from a replaced frame no longer match the panel.
function setFrameSource(panel, url, present) {
  const frame = element('iframe', 'terminal-frame sidecar-frame');
  frame.title = `${panel.link.label}: linked recording`;
  frame.setAttribute('allow', 'fullscreen');
  if (present) frame.dataset.present = present;
  panel.frame?.remove();
  panel.body.insertBefore(frame, panel.overlay);
  frame.src = url;
  Object.assign(panel, { frame, frameURL: url, loaded: false, childTime: null, childPlaying: false, wantPlay: null, duration: null, ended: false, speed: null, speedPosted: null });
}
// The child's detail says whether it is live (its persisted state).
async function readChild(panel) {
  const child = await getJSON(`/api/recordings/${panel.link.target.recording}`).catch(() => null);
  if (!child || !panels.includes(panel)) return;
  panel.child = child;
  const live = child.state === 'live';
  if (live !== panel.childLive) { panel.childLive = live; renderControls(panel); }
}
async function loadFrame(panel) {
  panel.overlay.textContent = 'Loading…';
  await readChild(panel);
  if (!panels.includes(panel)) return;
  // On a Live parent a live child plays its own Live stream.
  if (liveMode && panel.childLive) { goLive(panel); return; }
  await pinHistory(panel);
}
// segmentOneOpen says whether a pin of the child's segment 1 (the child clock)
// froze footage it may still extend. Only an open segment 1 grows, and the
// links API's childSeconds is null exactly while segment 1 has no end (the
// recorder ends it only once its capture finished; an ended segment never
// reopens). Liveness is not the signal: a live child may be recording a later
// segment while its segment 1 is closed for good.
const segmentOneOpen = link => link.childSeconds === null;
// pinHistory plays the child's pinned history (segment 1, the child clock)
// where the parent's playhead maps, following the parent. From Live it is the
// explicit re-pin: a fresh cutoff of what the child has saved by now.
async function pinHistory(panel) {
  const recording = panel.link.target.recording;
  try {
    const frame = await getJSON(`/api/recordings/${recording}/sidecar-frame`);
    if (typeof frame.player !== 'string' || !frame.player.startsWith(`/player/${recording}?`)) throw new Error('frame');
    if (!panels.includes(panel)) return;
    const duration = Number.isFinite(frame.duration) ? frame.duration : null;
    const m = toChild(panel.link, segment, parentAt(), duration);
    const at = m.state === 'on' ? m.c : m.state === 'after' && duration !== null ? duration : 0;
    panel.failed = false; panel.liveFrame = null;
    setFrameSource(panel, `${frame.player}&at=${seconds(Math.min(at, duration ?? at))}`, 'follow');
    panel.duration = duration;
    // Past a cutoff of footage the child may still extend sync says 'beyond'.
    panel.pinnedOpen = segmentOneOpen(panel.link);
    setMode(panel, 'follow');
    announce(panel, 'Following parent');
    sync(panel);
  } catch {
    if (!panels.includes(panel)) return;
    panel.failed = true;
    panel.overlay.textContent = 'This recording cannot be played here.';
    panel.overlay.hidden = false;
    panel.el.dataset.state = 'unavailable';
    renderControls(panel);
  }
}
async function repin(panel) {
  if (panel.mode !== 'live' || panel.repinning) return;
  panel.repinning = true;
  try { await pinHistory(panel); } finally { panel.repinning = false; }
}
// loadNewer is the explicit "Load newer footage" of a panel beyond its pinned
// footage (never called automatically): it refreshes the links, the child's
// detail and the shared sidecar-frame cutoff, and swaps the frame only when
// the saved footage grew. A fresh cutoff token alone is not newer footage (a
// state.json change mints one too).
async function loadNewer(panel) {
  if (panel.mode !== 'follow' || panel.newerBusy || panel.failed) return;
  const child = panel.link.target.recording;
  panel.newerBusy = true;
  headNote(panel, '');
  renderControls(panel);
  let frame = null;
  try {
    await loadFacts();
    renderLane();
    // As the Live refresh: an exploring panel keeps its link until Follow parent.
    for (const p of panels) {
      if (p.mode === 'explore') continue;
      p.link = byID(p.link.id) ?? p.link;
      if (p !== panel) sync(p);
    }
    await readChild(panel);
    frame = await getJSON(`/api/recordings/${child}/sidecar-frame`);
    if (typeof frame.player !== 'string' || !frame.player.startsWith(`/player/${child}?`)) frame = null;
  } catch { frame = null; }
  panel.newerBusy = false;
  if (!panels.includes(panel)) return;
  const duration = Number.isFinite(frame?.duration) ? frame.duration : null;
  panel.newerFailed = !frame;
  if (!frame) {
    // It stays (beside Retry) while the panel is beyond its footage.
    headNote(panel, 'Could not load newer footage', panel.el.dataset.state === 'beyond' ? 0 : 2500);
  } else if (panel.mode !== 'follow' || panel.failed) {
    // The user left following meanwhile: the child is theirs, nothing swaps.
  } else if (!(duration > panel.duration + 1e-6)) {
    // The refreshed link may say segment 1 ended since the pin: then the
    // loaded footage is all there is, and sync leaves 'beyond' for 'after'
    // (beyondChanged clears the head). Only a panel still beyond gets the note.
    panel.pinnedOpen = segmentOneOpen(panel.link);
    sync(panel);
    if (panel.el.dataset.state === 'beyond') headNote(panel, 'No newer footage saved yet', 2500);
  } else {
    const m = toChild(panel.link, segment, parentAt(), duration);
    const at = m.state === 'on' ? m.c : m.state === 'after' ? duration : 0;
    setFrameSource(panel, `${frame.player}&at=${seconds(Math.min(at, duration))}`, 'follow');
    panel.duration = duration;
    panel.pinnedOpen = segmentOneOpen(panel.link);
    setMode(panel, 'follow');
    announce(panel, 'Loaded newer footage');
    sync(panel);
    return;
  }
  renderControls(panel);
}
function explore(panel) {
  if (panel.mode !== 'follow' || !panel.frame || panel.failed) return;
  panel.exploreFrom = parentAt();
  panel.el.dataset.state = 'explore';
  panel.overlay.textContent = ''; panel.overlay.hidden = true;
  setMode(panel, 'explore');
  post(panel, { agentReel: 'present', mode: 'explore' });
  announce(panel, 'Exploring child');
  sync(panel);
}
// follow hands the child back to the parent's playhead; the parent never moves.
function follow(panel) {
  if (panel.mode !== 'explore') return;
  panel.link = byID(panel.link.id) ?? panel.link;
  setMode(panel, 'follow');
  panel.wantPlay = null; panel.childTime = null; panel.speedPosted = null;
  post(panel, { agentReel: 'present', mode: 'follow' });
  announce(panel, 'Following parent');
  sync(panel);
}
function goLive(panel) {
  if (!panel.controls) return;
  panel.failed = false; panel.liveFrame = 'live';
  setFrameSource(panel, `/player/${panel.link.target.recording}?mode=live`, null);
  panel.el.dataset.state = 'live';
  panel.overlay.textContent = ''; panel.overlay.hidden = true;
  setMode(panel, 'live');
  announce(panel, 'Live');
}
function liveUnavailable(panel) {
  panel.el.dataset.state = 'live-unavailable';
  panel.overlay.textContent = 'Live unavailable'; panel.overlay.hidden = false;
  renderControls(panel);
  announce(panel, 'Live unavailable');
}
// The latest saved screen of the child's newest segment (still=1 is only a
// Live fallback, so it rides on mode=live).
function showStill(panel) {
  if (panel.mode !== 'live') return;
  const total = panel.child?.segmentsTotal;
  panel.liveFrame = 'still';
  setFrameSource(panel, `/player/${panel.link.target.recording}?mode=live&segment=${Number.isInteger(total) && total >= 1 ? total : 1}&still=1`, null);
  panel.el.dataset.state = 'live-unavailable';
  panel.overlay.textContent = 'Latest saved screen'; panel.overlay.hidden = false;
  renderControls(panel);
}
const post = (panel, message) => panel.frame?.contentWindow?.postMessage(message, location.origin);
// sync puts one following panel where the parent's playhead says it should
// be. An exploring panel only names where the parent is; Live is its own.
function sync(panel) {
  const link = panel.link;
  if (panel.mode === 'live' || panel.failed) return;
  if (panel.mode === 'explore') {
    const at = parentAt();
    panel.status.textContent = Math.abs(at - panel.exploreFrom) > 0.5 ? `Parent moved to ${clock(at)}` : `Parent at ${clock(at)}`;
    renderMove(panel);
    return;
  }
  const wasBeyond = panel.el.dataset.state === 'beyond';
  if (parentDown()) {
    // Nothing to mirror: hold still, never seek.
    panel.el.dataset.state = 'parent-unavailable';
    delete panel.el.dataset.after;
    if (wasBeyond) beyondChanged(panel);
    panel.overlay.textContent = 'Parent playback unavailable'; panel.overlay.hidden = false;
    panel.video?.pause();
    if (panel.frame && panel.loaded && (panel.childPlaying || panel.wantPlay)) { panel.wantPlay = false; post(panel, { agentReel: 'pause' }); }
    return;
  }
  // A pin of still-open footage is 'beyond' past its cutoff, never 'ended':
  // footage (panel.duration) and the link's wall clock (childSeconds) differ.
  const m = toChild(link, segment, parentAt(), panel.duration, panel.pinnedOpen);
  panel.el.dataset.state = m.state;
  if (wasBeyond !== (m.state === 'beyond')) beyondChanged(panel);
  let text = '', after = null;
  if (m.state === 'before') text = m.startsIn !== null ? `starts in ${clock(Math.ceil(m.startsIn))}` : `starts in segment ${m.startSegment}`;
  else if (m.state === 'beyond') text = 'Beyond loaded footage';
  else if (m.state === 'after') {
    // The child clock is its segment 1 only: say why it stops here.
    const child = panel.child;
    if (link.kind === 'screenshot') text = 'shown earlier';
    else if (m.stopped && child?.state === 'live') text = 'Link stopped here';
    else if (!m.stopped && (child?.segmentsTotal ?? 1) >= 2) { text = 'Continued in segment 2 · not linked'; after = 'continued'; }
    else if (!m.stopped && child?.state === 'paused') { text = 'Child paused here'; after = 'paused'; }
    else text = 'ended';
  }
  else if (m.state === 'unanchored') text = 'Not re-anchored: the parent paused while this ran';
  else if (m.state === 'unplaced') text = 'This link has no placed anchor';
  if (after) panel.el.dataset.after = after; else delete panel.el.dataset.after;
  panel.overlay.textContent = text;
  panel.overlay.hidden = text === '';
  if (panel.img) panel.img.hidden = m.state !== 'on';
  if (panel.video && m.state === 'on' && Math.abs(panel.video.currentTime - m.c) > 0.5) panel.video.currentTime = m.c;
  if (panel.video) { if (m.state === 'on' && parent.playing) void panel.video.play().catch(() => {}); else panel.video.pause(); }
  if (!panel.frame || !panel.loaded) return;
  // Speed: ask once for the parent's, then wait for the child's notify.
  if (panel.speed !== null && panel.speed !== parent.speed && panel.speedPosted !== parent.speed) {
    panel.speedPosted = parent.speed;
    post(panel, { agentReel: 'speed', speed: parent.speed });
  }
  // Beyond its pinned footage a child is held exactly like after its end.
  const held = m.state === 'after' || m.state === 'beyond';
  const target = m.state === 'on' ? m.c : held ? m.c ?? panel.duration : m.state === 'before' ? 0 : null;
  const end = panel.duration ?? Infinity;
  const moving = parent.playing && m.state === 'on';
  const now = performance.now();
  const expected = childNow(panel, now);
  // The parent reports its time every 500ms, so a faster parent lags more.
  const slack = Math.max(1, parent.speed);
  // End guard. A child playing into its own end with the parent is left to
  // end by itself (it reports ended once); an ended child is never seeked to
  // its end again nor asked to play (which would restart it from 0), and a
  // child is never started within 0.3s of its end.
  const runningOut = parent.playing && panel.childPlaying && !panel.ended && target !== null && expected !== null && target >= end - slack && expected >= end - slack;
  if (!runningOut && target !== null && !(panel.ended && held)) {
    // After its end a child rests exactly there (so it reports ended).
    const tolerance = held ? 0.01 : moving ? slack : 0.15;
    if (expected === null || Math.abs(expected - target) > tolerance) {
      post(panel, { agentReel: 'seek', at: Math.max(0, target) });
      panel.childTime = target; panel.childAt = now; panel.ended = false;
    }
  }
  if (runningOut) return;
  const wantPlay = moving && !panel.ended && target < end - 0.3;
  if (wantPlay !== panel.wantPlay || wantPlay !== panel.childPlaying) {
    panel.wantPlay = wantPlay;
    post(panel, { agentReel: wantPlay ? 'play' : 'pause' });
  }
}
function syncAll() { for (const panel of panels) sync(panel); }

// ---- "Now running" grid --------------------------------------------------------
let grid, gridOn = false, gridTimer = null, gridLast = 0;
const tiles = new Map();
function setGrid(on) {
  gridOn = on;
  gridToggle.setAttribute('aria-pressed', String(on));
  grid.hidden = !on;
  if (!on) { for (const tile of tiles.values()) tile.el.remove(); tiles.clear(); }
  else renderGrid();
  writeHash();
  requestAnimationFrame(align);
}
function scheduleGrid() {
  if (!gridOn || gridTimer !== null) return;
  const wait = Math.max(0, gridLast + gridThrottleMs - performance.now());
  gridTimer = setTimeout(() => { gridTimer = null; renderGrid(); }, wait);
}
function renderGrid() {
  gridLast = performance.now();
  const active = new Map();
  for (const link of links) {
    const m = toChild(link, segment, parent.t);
    if (m.state === 'on') active.set(link.id, { link, c: m.c });
  }
  for (const [id, tile] of tiles) if (!active.has(id)) { tile.el.remove(); tiles.delete(id); }
  for (const [id, { link, c }] of active) {
    let tile = tiles.get(id);
    if (!tile) {
      const el = element('button', 'sidecar-tile');
      el.type = 'button'; el.dataset.sidecarTile = id; el.title = `Open ${link.label} in a tab`;
      const view = element('div', 'sidecar-tile-view');
      el.append(view, element('span', 'sidecar-tile-label', `${link.label} · ${link.kind}`));
      el.addEventListener('click', () => openPanel(id));
      tile = { el, view, key: '' };
      tiles.set(id, tile);
      grid.append(el);
      if (link.kind === 'screenshot' && link.target.type === 'asset') {
        const img = element('img'); img.alt = ''; img.src = link.target.url; view.append(img);
      } else if (link.kind !== 'recording') {
        view.append(element('span', 'sidecar-note', 'Local video file (not served)'));
      }
    }
    if (link.kind === 'recording' && link.target.type === 'recording') {
      const key = stillURL(link.target.recording, 1, c);
      if (key !== tile.key) {
        tile.key = key;
        tile.el.dataset.stillState = 'loading';
        requestStill(`tile:${id}`, link.target.recording, 1, c, image => {
          if (tile.key !== key || !tile.el.isConnected) return;
          tile.el.dataset.stillState = image ? 'ready' : 'unavailable';
          tile.view.replaceChildren(image ?? element('span', 'sidecar-note', 'Still unavailable'));
        });
      }
    }
  }
  grid.dataset.count = String(tiles.size);
  grid.classList.toggle('sidecar-grid-empty', tiles.size === 0);
}

// ---- Child page: "↑ parent at m:ss" -------------------------------------------
// A link only while the child's position maps (toParent, strictly); otherwise
// a plain "↑ parent: <reason>" (data-reason names the state).
async function childLink(page) {
  const parentID = page.parent.recording;
  const [parentPage, parentDetail] = await Promise.all([getJSON(`/api/recordings/${parentID}/links`), getJSON(`/api/recordings/${parentID}`).catch(() => null)]);
  const link = parentPage.links.find(l => l.id === page.parent.link && Array.isArray(l.anchors));
  if (!link) return;
  const ends = {};
  for (const seg of parentDetail?.segments ?? []) {
    const start = Date.parse(seg.recordedStart ?? ''), end = Date.parse(seg.recordedEnd ?? '');
    if (Number.isFinite(start) && Number.isFinite(end) && end > start) ends[seg.number] = (end - start) / 1000;
  }
  // The child clock is its segment 1 only (docs/visual-recording-links.md), so
  // a later child segment has no parent time to point at.
  if (segment !== 1) return;
  let node = null;
  const update = childTime => {
    const p = toParent(link, childTime, ends);
    const tag = p.state === 'on' ? 'a' : 'span';
    if (node?.localName !== tag) {
      // Swapped only when the kind changes (the focus goes with it).
      const next = element(tag, 'sidecar-parent');
      next.dataset.sidecarParent = '';
      if (tag === 'a') next.title = 'Open the parent recording at this moment';
      if (node) {
        const focused = node.contains(document.activeElement);
        node.replaceWith(next);
        if (focused) next.focus();
      }
      node = next;
    }
    if (p.state === 'on') {
      setAttr(node, 'href', parentHref(parentID, p.segment, p.at, link.id));
      setText(node, `↑ parent at ${p.segment > 1 ? `${p.segment} · ` : ''}${approx(p)}${clock(p.at)}`);
      node.dataset.segment = String(p.segment); node.dataset.at = seconds(p.at);
    } else {
      node.dataset.reason = p.state;
      setText(node, `↑ parent: ${unmappedReason(p)}`);
    }
  };
  update(parent.t);
  document.querySelector('.below-player')?.prepend(node);
  listeners.add(() => update(parent.t));
}

// ---- Wiring --------------------------------------------------------------------
window.addEventListener('message', event => {
  if (event.origin !== location.origin || event.data?.agentReel !== 'player') return;
  const frame = mainFrame();
  if (frame && event.source === frame.contentWindow) {
    const { state } = event.data;
    if (state === 'time' && Number.isFinite(event.data.time)) {
      parent.t = event.data.time;
      // The parent got where Move parent asked it to be.
      if (parent.pending && Math.abs(parent.t - parent.pending.at) <= 1) parent.pending = null;
    } else if (state === 'duration' && Number.isFinite(event.data.duration)) { parent.duration = event.data.duration; requestAnimationFrame(align); }
    else if (state === 'speed' && Number.isFinite(event.data.speed) && event.data.speed > 0) parent.speed = event.data.speed;
    else if (state === 'playing') parent.playing = true;
    else if (state === 'pause' || state === 'ended') parent.playing = false;
    else return;
    changed();
    return;
  }
  const panel = panels.find(p => p.frame && event.source === p.frame.contentWindow);
  if (!panel) return;
  const { state } = event.data;
  panel.loaded = true;
  if (panel.mode === 'live') {
    // The Live stream failed (at once when the recorder is not validated,
    // or at the frame's own reconnect deadline).
    if (panel.liveFrame === 'live' && ['error', 'offline', 'ended'].includes(state)) liveUnavailable(panel);
    else if (panel.liveFrame === 'still' && (state === 'error' || state === 'offline')) { panel.overlay.textContent = 'Saved screen unavailable'; panel.overlay.hidden = false; }
    return;
  }
  // A child that failed (error/offline, below) and then plays or reports
  // its position again recovered (a later speed switch loaded): lift that
  // overlay, and no other. A following panel's sync redraws its own.
  if ((state === 'playing' || state === 'pause' || state === 'time') && panel.overlay.textContent === 'Playback unavailable') {
    panel.overlay.textContent = ''; panel.overlay.hidden = true;
  }
  if (state === 'time' && Number.isFinite(event.data.time)) {
    panel.childTime = event.data.time; panel.childAt = performance.now();
    // An exploring child's position moves its Move parent target.
    if (panel.mode === 'explore') renderMove(panel);
    return;
  }
  if (state === 'speed') {
    if (!Number.isFinite(event.data.speed) || !(event.data.speed > 0)) return;
    panel.speed = event.data.speed;
    // A speed switch recreates the child's player where it stood: a
    // following child is put back where the parent says.
    if (panel.mode === 'follow') { panel.childTime = null; panel.wantPlay = null; sync(panel); }
    return;
  }
  if (state === 'playing') panel.childPlaying = true;
  else if (state === 'pause') panel.childPlaying = false;
  else if (state === 'ended') {
    panel.childPlaying = false; panel.ended = true;
    if (panel.duration !== null) { panel.childTime = panel.duration; panel.childAt = performance.now(); }
  } else if (state === 'error' || state === 'offline') { panel.overlay.textContent = 'Playback unavailable'; panel.overlay.hidden = false; return; }
  // An exploring child is the user's: its state is noted, never answered.
  if (panel.mode === 'explore') renderMove(panel);
  if (panel.mode !== 'follow') return;
  panel.wantPlay = null;
  sync(panel);
});

async function start() {
  if (!rail || !stage || !playerBox || !recording || !(segment >= 1)) return;
  if (!document.querySelector('link[href="/assets/sidecar.css"]')) {
    const sheet = document.createElement('link');
    sheet.rel = 'stylesheet'; sheet.href = '/assets/sidecar.css';
    document.head.append(sheet);
  }
  let page;
  try { page = await loadFacts(); } catch { return; }
  if (page.parent) void childLink(page).catch(() => {});
  if (!links.length) return;
  buildLane();
  buildTabs();
  // The panels share the player's view row (the lane still follows them).
  panelBox = element('div', 'sidecar-panels');
  panelBox.dataset.sidecarPanels = '';
  playerBox.after(panelBox);
  grid = element('div', 'sidecar-grid');
  grid.dataset.sidecarGrid = ''; grid.hidden = true;
  grid.setAttribute('aria-label', 'Links running at the playhead');
  lane.after(grid);
  if (liveMode) {
    // Live has no scrubber: the playhead is the live edge of this segment.
    const tick = () => { if (segmentStart !== null) { parent.t = Math.max(0, (Date.now() - segmentStart) / 1000); parent.playing = true; changed(); } };
    tick();
    setInterval(tick, 1000);
  }
  listeners.add(() => { movePlayhead(); syncAll(); scheduleGrid(); });
  // viewer-adapter marks a lost history player (and every playback state the
  // Parent tab names) on #player-status.
  const status = document.querySelector('#player-status');
  if (status) new MutationObserver(changed).observe(status, { attributes: true });
  listeners.add(renderParentTab);
  let lastDuration = null;
  listeners.add(() => { if (duration() !== lastDuration && !liveMode) { lastDuration = duration(); renderLane(); } });
  renderLane();
  align();
  new ResizeObserver(fitCompare).observe(stage);
  const restored = hashState();
  for (const id of restored.ids.slice(-panelsMax)) openPanel(id);
  // tab=ID selects that open panel's tab; otherwise Parent shows.
  if (panels.length) selectTab(panels.find(panel => panel.link.id === restored.tab) ?? null);
  // compare=1 comes back only where Compare is offered.
  if (restored.compare) setCompare(true);
  if (restored.grid) setGrid(true);
  if (liveRecording) {
    setInterval(async () => {
      if (document.hidden) return;
      try {
        await loadFacts();
        renderLane();
        await Promise.all(panels.filter(panel => panel.controls).map(readChild));
        // An exploring panel keeps its link and position until Follow parent.
        for (const panel of panels) {
          if (panel.mode === 'explore') continue;
          panel.link = byID(panel.link.id) ?? panel.link;
          sync(panel);
        }
        scheduleGrid();
      } catch { /* Keep the last lane. */ }
    }, liveRefreshMs);
  }
}
void start();
