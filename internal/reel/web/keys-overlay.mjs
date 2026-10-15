// Keystroke overlay of the history player: a Keys button in the control bar
// opens a popover (show/hide, one toggle and colour per client); the overlay
// draws the strokes of the last few seconds over the terminal, following the
// player's own getCurrentTime(), so pausing, seeking and speed changes need
// no extra wiring. Events come from the keys JSON (Go supplies the URL);
// text is written with textContent only. Display only: nothing is replayed.
const WINDOW = 60;      // seconds per fetched range
const SHOW = 2.5;       // seconds a stroke stays on screen
const MAX_CHIPS = 6;
const TICK = 100;
const STORE = 'agent-reel.keys';
// Distinct on dark and light terminal backgrounds; clients take them in the
// order the JSON lists them (sorted, whole segment), so colours are stable.
const PALETTE = ['#4fc3f7', '#ffb74d', '#81c784', '#f06292', '#ba68c8', '#e6ee9c'];

const load = () => {
  try {
    const value = JSON.parse(localStorage.getItem(STORE) ?? 'null');
    if (value && typeof value.on === 'boolean' && Array.isArray(value.hidden)) return { on: value.on, hidden: value.hidden.filter(v => typeof v === 'string') };
  } catch { /* Defaults below. */ }
  return { on: true, hidden: [] };
};
const save = prefs => {
  try { localStorage.setItem(STORE, JSON.stringify(prefs)); } catch { /* Kept for this page only. */ }
};
const element = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};

// label renders one event as its chip text.
export const label = event => {
  if (event.masked) return '••• hidden input';
  if (event.paste) {
    const { bytes, lines } = event.paste;
    return lines > 1 ? `Paste · ${lines} lines` : `Paste · ${bytes} chars`;
  }
  let text = event.label ?? '';
  if (!event.mod && text.trim() === '') text = '␣'.repeat(Math.min(text.length, 3)) || '␣';
  if (event.mod) text = `${event.mod}+${text}`;
  if (event.n > 1) text += ` ×${event.n}`;
  return text;
};

export function attach({ host, extras, api, time }) {
  const prefs = load();
  let clients = [], coverage = null, stopped = false;
  const windows = new Map();
  const button = element('button', 'reel-keys', 'Keys');
  button.type = 'button';
  button.hidden = true;
  button.title = 'Keystroke overlay';
  button.setAttribute('aria-haspopup', 'dialog');
  button.setAttribute('aria-expanded', 'false');
  extras.append(button);
  const popover = element('div', 'keys-popover');
  popover.setAttribute('role', 'dialog');
  popover.setAttribute('aria-label', 'Keystroke overlay');
  popover.hidden = true;
  document.body.append(popover);
  const overlay = element('div', 'keys-overlay');
  overlay.setAttribute('aria-hidden', 'true');

  const colour = index => PALETTE[index % PALETTE.length];
  const shown = index => !prefs.hidden.includes(clients[index]);
  const renderPopover = () => {
    popover.replaceChildren();
    const toggle = element('label', 'keys-toggle');
    const box = element('input');
    box.type = 'checkbox'; box.checked = prefs.on; box.dataset.keysShow = '';
    box.addEventListener('change', () => { prefs.on = box.checked; save(prefs); draw(lastTime); });
    toggle.append(box, element('span', '', 'Show keystrokes'));
    popover.append(element('strong', 'keys-title', 'Keystrokes'), toggle);
    const list = element('ul', 'keys-clients');
    clients.forEach((client, index) => {
      const item = element('li');
      const row = element('label');
      const check = element('input');
      check.type = 'checkbox'; check.checked = shown(index); check.dataset.keysClient = client;
      check.addEventListener('change', () => {
        prefs.hidden = prefs.hidden.filter(c => c !== client);
        if (!check.checked) prefs.hidden.push(client);
        save(prefs); draw(lastTime);
      });
      const swatch = element('span', 'keys-swatch');
      swatch.style.background = colour(index);
      row.append(check, swatch, element('span', 'keys-client', client));
      item.append(row);
      list.append(item);
    });
    popover.append(list, element('p', 'keys-note', 'Coverage is partial: only clients attached with areel attach are captured. Password prompts show as hidden input.'));
  };
  const setOpen = open => {
    popover.hidden = !open;
    button.setAttribute('aria-expanded', String(open));
    if (open) renderPopover();
  };
  button.addEventListener('click', () => setOpen(popover.hidden));
  document.addEventListener('click', event => { if (!popover.hidden && !popover.contains(event.target) && event.target !== button) setOpen(false); });
  document.addEventListener('keydown', event => { if (event.key === 'Escape' && !popover.hidden) { setOpen(false); button.focus(); } });

  const fetchWindow = index => {
    if (windows.has(index)) return;
    windows.set(index, null);
    fetch(`${api}&from=${index * WINDOW}&to=${(index + 1) * WINDOW}`, { cache: 'no-store', credentials: 'same-origin' })
      .then(response => { if (!response.ok) throw new Error('keys'); return response.json(); })
      .then(page => {
        if (!Array.isArray(page.events) || !Array.isArray(page.clients)) throw new Error('keys');
        windows.set(index, page.events);
        clients = page.clients;
        if (coverage !== page.coverage) {
          coverage = page.coverage;
          button.hidden = coverage !== 'partial';
          if (coverage !== 'partial') { stopped = true; overlay.remove(); }
        }
        if (!popover.hidden) renderPopover();
        draw(lastTime);
      })
      // A failed range is retried after a pause, not on every tick.
      .catch(() => { windows.set(index, []); setTimeout(() => windows.delete(index), 5000); });
  };

  let lastTime = 0, drawn = '';
  const draw = t => {
    const anchor = host.querySelector('.ap-wrapper') ?? host;
    if (overlay.parentElement !== anchor) anchor.append(overlay);
    const events = [];
    if (prefs.on && coverage === 'partial') {
      for (let i = Math.max(0, Math.floor((t - SHOW) / WINDOW)); i <= Math.floor(t / WINDOW); i++) {
        for (const event of windows.get(i) ?? []) {
          if (event.at <= t + 0.05 && event.at > t - SHOW && shown(event.client)) events.push(event);
        }
      }
    }
    const recent = events.slice(-MAX_CHIPS);
    const key = recent.map(e => `${e.at}|${e.client}|${label(e)}`).join('\n');
    if (key === drawn) return;
    drawn = key;
    overlay.replaceChildren(...recent.map(event => {
      const chip = element('span', `keys-chip${event.masked ? ' keys-masked' : ''}${event.paste ? ' keys-paste' : ''}`, label(event));
      chip.style.borderColor = colour(event.client);
      chip.title = clients[event.client] ?? '';
      chip.dataset.at = String(event.at);
      return chip;
    }));
    overlay.hidden = recent.length === 0;
  };

  fetchWindow(0);
  let busy = false;
  const tick = async () => {
    if (stopped || busy || document.hidden) return;
    busy = true;
    try {
      const t = await time();
      if (Number.isFinite(t) && t >= 0) {
        lastTime = t;
        const index = Math.floor(t / WINDOW);
        fetchWindow(index);
        if (index > 0 && t - index * WINDOW < SHOW) fetchWindow(index - 1);
        if ((index + 1) * WINDOW - t < 10) fetchWindow(index + 1);
        draw(t);
      }
    } catch { /* The next tick retries. */ }
    busy = false;
  };
  const timer = setInterval(() => { if (stopped) clearInterval(timer); else void tick(); }, TICK);
}
