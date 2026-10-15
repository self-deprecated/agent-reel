// Text search: the library page searches every sealed recording, the viewer's
// rail searches this one. Go owns the index, matching and every URL shape;
// this module only asks GET /api/search, renders the hits with textContent,
// and opens or seeks the player to a hit. Go renders the controls hidden, so
// without script nothing changes.
const form = document.querySelector('[data-text-search]');
const results = document.querySelector('[data-search-results]');
if (form && results) {
  const input = form.querySelector('[data-text-search-input]');
  const fuzzy = form.querySelector('[data-text-search-fuzzy]');
  const recording = form.dataset.recording ?? '';
  // The viewer: marks give way to hits while a query is active.
  const rail = document.querySelector('#marks');
  const marksBody = rail?.querySelector('[data-rail-body]');
  const segment = Number(rail?.dataset.segment ?? 0);
  const cutoff = rail?.dataset.cutoff;
  const history = rail?.dataset.mode === 'history';
  const pad = n => String(n).padStart(2, '0');
  const clock = seconds => {
    const s = Math.max(0, Math.floor(seconds));
    return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${pad(Math.floor(s / 60))}:${pad(s % 60)}`;
  };
  // Exact seconds ("12.5") to the viewer's canonical at= form, rounded up to
  // the millisecond so the hit's own view is on screen.
  const atParam = at => String(Math.ceil(Number(at) * 1000 - 1e-6) / 1000);
  const element = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };
  // The hit text with the first case-insensitive match marked.
  const snippet = (text, query) => {
    const box = element('span', 'search-text');
    const i = text.toLowerCase().indexOf(query.toLowerCase());
    if (i < 0) { box.textContent = text; return box; }
    box.append(text.slice(0, i), element('mark', '', text.slice(i, i + query.length)), text.slice(i + query.length));
    return box;
  };
  const target = hit => `/recordings/${hit.recording}?mode=history&segment=${hit.segment}&at=${atParam(hit.at)}${cutoff && hit.recording === recording && hit.segment === segment ? `&cutoff=${cutoff}` : ''}`;
  const hitRow = (hit, query, multi) => {
    const row = element('a', `search-hit search-${hit.kind}`);
    row.href = target(hit);
    row.dataset.recording = hit.recording;
    row.dataset.segment = hit.segment;
    row.dataset.at = hit.at;
    const seconds = Number(hit.at);
    row.append(element('span', 'mark-at', multi && hit.segment > 1 ? `${hit.segment} · ${clock(seconds)}` : clock(seconds)), snippet(hit.text, query));
    const meta = element('span', 'search-meta', hit.kind === 'activity' ? 'mark' : hit.count > 1 ? `×${hit.count}` : '');
    if (hit.kind !== 'activity') row.title = `First shown at ${clock(seconds)} on row ${hit.y + 1}${hit.count > 1 ? `; shown ${hit.count} times` : ''}`;
    row.append(meta);
    return row;
  };
  const heading = rec => {
    const parts = [rec.repository, rec.workspace].filter(Boolean).join(' / ');
    const when = rec.recordedAt ? new Date(rec.recordedAt).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';
    const title = element('h3', 'search-recording');
    title.append(element('span', 'search-recording-name', [parts, rec.session].filter(Boolean).join(' · ') || rec.recording.slice(0, 8)));
    if (when) title.append(element('span', 'search-recording-when', when));
    return title;
  };
  const show = active => {
    results.hidden = !active;
    if (marksBody) marksBody.hidden = active;
  };
  let controller, timer, retries = 0;
  const render = (report, query) => {
    results.replaceChildren();
    const byRecording = new Map(report.recordings.map(rec => [rec.recording, rec]));
    const groups = new Map();
    for (const hit of report.hits) {
      if (!groups.has(hit.recording)) groups.set(hit.recording, []);
      groups.get(hit.recording).push(hit);
    }
    const summary = element('p', 'search-summary');
    const n = report.hits.length;
    summary.textContent = n ? `${n}${report.more ? '+' : ''} ${n === 1 ? 'match' : 'matches'}${recording ? '' : ` in ${groups.size} ${groups.size === 1 ? 'recording' : 'recordings'}`}` : 'No matches';
    results.append(summary);
    for (const [id, hits] of groups) {
      const section = element('section', 'search-group');
      if (!recording) section.append(heading(byRecording.get(id) ?? { recording: id }));
      const list = element('ol', 'rail-list search-list');
      for (const hit of hits) {
        const item = element('li');
        item.append(hitRow(hit, query, true));
        list.append(item);
      }
      section.append(list);
      results.append(section);
    }
    const notes = [];
    if (report.pending > 0) notes.push(`Indexing ${report.pending} ${report.pending === 1 ? 'recording' : 'recordings'}… results will update.`);
    const live = report.recordings.filter(rec => rec.status === 'live').length;
    if (live && recording) notes.push('Search works once this recording is stopped and sealed.');
    else if (live) notes.push(`${live} live ${live === 1 ? 'recording is' : 'recordings are'} not searched until sealed.`);
    const failed = report.recordings.filter(rec => rec.status === 'unavailable').length;
    if (failed) notes.push(`${failed} ${failed === 1 ? 'recording' : 'recordings'} could not be searched.`);
    for (const note of notes) results.append(element('p', 'search-note', note));
    clearTimeout(timer);
    if (report.pending > 0 && retries < 40) {
      retries++;
      timer = setTimeout(() => run(false), 1500);
    }
  };
  const run = async fresh => {
    const query = input.value.trim();
    controller?.abort();
    clearTimeout(timer);
    if (fresh) retries = 0;
    if (!query) { results.replaceChildren(); show(false); return; }
    controller = new AbortController();
    const params = new URLSearchParams({ q: query });
    if (recording) params.set('recording', recording);
    if (fuzzy?.checked) params.set('fuzzy', '1');
    show(true);
    results.setAttribute('aria-busy', 'true');
    try {
      const response = await fetch(`/api/search?${params}`, { cache: 'no-store', credentials: 'same-origin', signal: controller.signal });
      if (!response.ok) throw new Error('search');
      render(await response.json(), query);
    } catch (error) {
      if (error.name === 'AbortError') return;
      results.replaceChildren(element('p', 'search-note', 'Search is unavailable right now.'));
    } finally {
      results.removeAttribute('aria-busy');
    }
  };
  let debounce;
  input.addEventListener('input', () => { clearTimeout(debounce); debounce = setTimeout(() => run(true), 250); });
  fuzzy?.addEventListener('change', () => run(true));
  form.addEventListener('submit', event => { event.preventDefault(); clearTimeout(debounce); void run(true); });
  input.addEventListener('keydown', event => {
    if (event.key === 'Escape' && input.value) { event.preventDefault(); input.value = ''; void run(true); }
  });
  // A hit in the segment the viewer is playing seeks in place, keeping the
  // history cutoff; any other hit opens its viewer.
  results.addEventListener('click', event => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const row = event.target.closest('a.search-hit');
    const frame = document.querySelector('#terminal-frame');
    if (!row || !history || !frame?.contentWindow || row.dataset.recording !== recording || Number(row.dataset.segment) !== segment) return;
    event.preventDefault();
    for (const current of results.querySelectorAll('.search-hit[aria-current]')) { current.removeAttribute('aria-current'); current.classList.remove('mark-current'); }
    row.setAttribute('aria-current', 'true');
    row.classList.add('mark-current');
    frame.contentWindow.postMessage({ agentReel: 'seek', at: Number(atParam(row.dataset.at)) }, location.origin);
  });
  form.hidden = false;
}
