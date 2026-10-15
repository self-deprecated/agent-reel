// Render-time terminal colours. Mirrors Go internal/termtheme: the preset
// table below is checked against it byte for byte (TestLibraryAssetThemesMatchGo).
// Only the default colours and the 16 palette slots are re-mappable; colours an
// application set explicitly (256-colour / true colour) are painted as recorded.
// A theme is a spec "preset[,fg=#rrggbb][,bg=#rrggbb][,cN=#rrggbb]…", parsed
// strictly; values only ever reach CSS through style.setProperty.
export const presets = /*PRESETS*/[{"name":"asciinema","label":"Default (asciinema)","light":false,"theme":{"fg":"#cccccc","bg":"#121314","palette":["#000000","#dd3c69","#4ebf22","#ddaf3c","#26b0d7","#b954e1","#54e1b9","#d9d9d9","#4d4d4d","#dd3c69","#4ebf22","#ddaf3c","#26b0d7","#b954e1","#54e1b9","#ffffff"]}},{"name":"dracula","label":"Dracula","light":false,"theme":{"fg":"#f8f8f2","bg":"#282a36","palette":["#21222c","#ff5555","#50fa7b","#f1fa8c","#bd93f9","#ff79c6","#8be9fd","#f8f8f2","#6272a4","#ff6e6e","#69ff94","#ffffa5","#d6acff","#ff92df","#a4ffff","#ffffff"]}},{"name":"solarized-dark","label":"Solarized Dark","light":false,"theme":{"fg":"#839496","bg":"#002b36","palette":["#073642","#dc322f","#859900","#b58900","#268bd2","#d33682","#2aa198","#eee8d5","#002b36","#cb4b16","#586e75","#657b83","#839496","#6c71c4","#93a1a1","#fdf6e3"]}},{"name":"solarized-light","label":"Solarized Light","light":true,"theme":{"fg":"#657b83","bg":"#fdf6e3","palette":["#073642","#dc322f","#859900","#b58900","#268bd2","#d33682","#2aa198","#eee8d5","#002b36","#cb4b16","#586e75","#657b83","#839496","#6c71c4","#93a1a1","#fdf6e3"]}},{"name":"nord","label":"Nord","light":false,"theme":{"fg":"#d8dee9","bg":"#2e3440","palette":["#3b4252","#bf616a","#a3be8c","#ebcb8b","#81a1c1","#b48ead","#88c0d0","#e5e9f0","#4c566a","#bf616a","#a3be8c","#ebcb8b","#81a1c1","#b48ead","#8fbcbb","#eceff4"]}},{"name":"gruvbox-dark","label":"Gruvbox Dark","light":false,"theme":{"fg":"#ebdbb2","bg":"#282828","palette":["#282828","#cc241d","#98971a","#d79921","#458588","#b16286","#689d6a","#a89984","#928374","#fb4934","#b8bb26","#fabd2f","#83a598","#d3869b","#8ec07c","#ebdbb2"]}}]/*END*/;

export const defaultPreset = 'asciinema';
// The player theme option for a spec. app.css maps the "agentreel" class to the
// --reel-term-* values. The unmodified default keeps the player's own
// behaviour ("auto/…": a colour set recorded in the source still wins), so
// nothing changes unless colours are chosen; any other choice overrides it.
export function playerTheme(spec) {
  return spec && format(spec) !== defaultPreset ? 'agentreel' : 'auto/agentreel';
}
const storageKey = 'agentReel.theme';
const hex = /^#[0-9a-fA-F]{6}$/;
const byName = new Map(presets.map(p => [p.name, p]));

// parse returns {preset, fg, bg, slots[16]} or null for anything invalid.
export function parse(text) {
  if (typeof text !== 'string' || text === '' || text.length > 256) return null;
  const [name, ...parts] = text.split(',');
  if (!byName.has(name)) return null;
  const spec = { preset: name, fg: '', bg: '', slots: Array(16).fill('') };
  const seen = new Set();
  for (const part of parts) {
    const i = part.indexOf('=');
    if (i < 0) return null;
    const key = part.slice(0, i), value = part.slice(i + 1);
    if (seen.has(key) || !hex.test(value)) return null;
    seen.add(key);
    const color = value.toLowerCase();
    if (key === 'fg') spec.fg = color;
    else if (key === 'bg') spec.bg = color;
    else {
      const m = /^c(0|[1-9]|1[0-5])$/.exec(key);
      if (!m) return null;
      spec.slots[Number(m[1])] = color;
    }
  }
  return spec;
}

// format is the canonical spec text (fg, bg, then slots), as Go's Spec.String.
export function format(spec) {
  let text = spec.preset;
  if (spec.fg) text += `,fg=${spec.fg}`;
  if (spec.bg) text += `,bg=${spec.bg}`;
  spec.slots.forEach((c, i) => { if (c) text += `,c${i}=${c}`; });
  return text;
}

export function resolve(spec) {
  const base = (byName.get(spec?.preset) ?? byName.get(defaultPreset)).theme;
  return {
    fg: spec?.fg || base.fg,
    bg: spec?.bg || base.bg,
    palette: base.palette.map((c, i) => spec?.slots?.[i] || c),
  };
}

// The effective spec: this browser's stored choice, else the library default
// (a Go-validated spec from the page), else the built-in default.
export function current(libraryDefault) {
  let stored = null;
  try { stored = localStorage.getItem(storageKey); } catch { /* Storage blocked. */ }
  return parse(stored) ?? parse(libraryDefault) ?? parse(defaultPreset);
}

export function store(spec) {
  try {
    if (spec) localStorage.setItem(storageKey, format(spec));
    else localStorage.removeItem(storageKey);
  } catch { /* Not remembered; still applied. */ }
}

// apply sets the --reel-term-* properties the agentreel player theme reads.
export function apply(element, spec) {
  const theme = resolve(spec);
  element.style.setProperty('--reel-term-fg', theme.fg);
  element.style.setProperty('--reel-term-bg', theme.bg);
  theme.palette.forEach((c, i) => element.style.setProperty(`--reel-term-${i}`, c));
}
