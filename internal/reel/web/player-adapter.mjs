// Renderer glue only. Routes, mode selection, history and marks belong to Go.
import * as themes from '/assets/themes.mjs';
const host = document.querySelector('#terminal');
const live = host.dataset.mode === 'live';
// A still frame plays the one-frame durable still cast while Live is unavailable.
const still = host.dataset.mode === 'still';
const history = !live && !still;
const notify = (state, extra) => parent.postMessage({ agentReel: 'player', state, ...extra }, location.origin);
const speeds = [1, 1.5, 2, 4, 0.5];
// Format seconds like the player's own timer (mm:ss or h:mm:ss).
const clock = seconds => {
  const s = Math.max(0, Math.floor(seconds));
  const pad = n => String(n).padStart(2, '0');
  return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${pad(Math.floor(s / 60))}:${pad(s % 60)}`;
};
// The official player labels a timeline marker "mm:ss - Label". Go writes
// activity marks as You / Agent / Tool (a labelled manual mark by its own
// label, tagged other); tag each tick so CSS can colour and shape it. Only a
// data attribute is added; the player's DOM is not replaced.
const whoOf = text => {
  const label = /\s-\s(.*)$/.exec(text ?? '')?.[1] ?? '';
  return { You: 'you', Agent: 'agent', Tool: 'tool' }[label] ?? 'other';
};
// A standalone player page has no owning viewer. Route it through the Go page
// before constructing a renderer (including “Open frame in new tab”).
if (parent === window && location.pathname.startsWith('/player/')) {
  location.replace(location.href.replace('/player/', '/recordings/'));
} else if (host.dataset.source) {
  // A sidecar panel's frame (sidecar-view.mjs) is either a mirror of the
  // parent (follow: no reachable controls, input swallowed with a hint) or
  // the child explored on its own. The main viewer is never a sidecar.
  let sidecar = false, present = null;
  try {
    const fe = window.frameElement;
    sidecar = !!fe?.classList.contains('sidecar-frame');
    if (sidecar && history) present = fe.dataset.present === 'explore' ? 'explore' : 'follow';
  } catch { /* Not ours to read: an ordinary frame. */ }
  const applyPresent = () => {
    document.documentElement.classList.toggle('reel-follow', present === 'follow');
    host.inert = present === 'follow';
  };
  applyPresent();
  // The follow strip lives in the body, outside #terminal (which a player
  // swap empties), and shows only in follow (app.css).
  let strip = null, stripText = 'Following parent', hintUntil = 0, hintTimer;
  if (sidecar) {
    strip = document.createElement('div');
    strip.className = 'reel-follow-strip'; strip.dataset.reelStrip = '';
    strip.textContent = stripText;
    document.body.append(strip);
  }
  const hint = () => {
    if (!strip) return;
    hintUntil = performance.now() + 2000;
    strip.textContent = 'Following parent · use Explore child to control';
    clearTimeout(hintTimer);
    hintTimer = setTimeout(() => { strip.textContent = stripText; }, 2000);
  };
  if (sidecar && history) {
    // Capture on the document runs before the player's own delegated
    // (bubble-phase) handlers; host.inert also blocks real focus and hits.
    const shortcuts = new Set([' ', ',', '.', 'f', 'm', '[', ']', '?', 'k', 'ArrowLeft', 'ArrowRight', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9']);
    const swallow = event => { event.stopImmediatePropagation(); event.preventDefault(); hint(); };
    document.addEventListener('keydown', event => {
      if (present === 'follow' && !event.altKey && !event.metaKey && !event.ctrlKey && shortcuts.has(event.key)) swallow(event);
    }, true);
    // Wheel and touch pass through: the player has no handlers for them, and
    // cancelling them would stop the page scrolling past a following panel.
    for (const type of ['mousedown', 'pointerdown', 'click']) {
      document.addEventListener(type, event => { if (present === 'follow') swallow(event); }, true);
    }
    // The owning viewer switches follow / explore: only our own parent
    // document, same origin, a known mode. Listening from the start keeps a
    // switch made while the player loads.
    window.addEventListener('message', event => {
      if (event.source !== parent || event.origin !== location.origin || event.data?.agentReel !== 'present') return;
      const mode = event.data.mode;
      if (mode !== 'follow' && mode !== 'explore') return;
      present = mode;
      applyPresent();
    });
  }
  let deadline;
  const clearDeadline = () => { clearTimeout(deadline); deadline = undefined; };
  const terminate = () => {
    clearDeadline();
    notify('error');
    // Also self-dispose when embedded without our viewer's message listener.
    // Navigation destroys this realm, including the vendor's retry timers.
    location.replace('about:blank');
  };
  const armDeadline = () => {
    if (live && deadline === undefined) deadline = setTimeout(terminate, 14900);
  };
  armDeadline();
  // The player measures its cell once, at creation: load the bundled faces
  // first (bounded, so a slow font never holds the player back for long).
  await Promise.race([
    Promise.all([document.fonts.load('16px AgentReelMono'), document.fonts.load('16px AgentReelBox', '\u2502')]),
    new Promise(resolve => setTimeout(resolve, 3000)),
  ]).catch(() => {});
  let player, speed = 1, playing = false, duration = null;
  // A replacement (history's swap, below) reads 0 until its first playing or
  // seeked places it where it was created for (target), and whether it plays
  // there (targetPlaying) is known only from how it was created. A failed
  // player is no longer placed either. last is the last position reported
  // (by lastFrom).
  let placed = true, target = 0, targetPlaying = false, last = -1, lastFrom = null;
  // A player recreated paused is held until it next plays (swap, below). Its
  // positioning load may hang (a dead connection): after the viewer's own
  // history deadline it is reported failed, as the player reports a failed
  // load, and its switch gives up (giveUp).
  const heldLimitMs = 60000;
  let heldDeadline, giveUp = null;
  const clearHeld = () => { clearTimeout(heldDeadline); heldDeadline = undefined; };
  // Terminal colours: a theme carried by a reload (#theme=…, see below), else
  // this browser's stored choice, else the library default.
  const carried = new URLSearchParams(location.hash.slice(1)).get('theme');
  let theme = themes.parse(carried) ?? themes.current(host.dataset.themeDefault);
  themes.apply(host, theme); themes.apply(document.documentElement, theme);
  // History only: our extras sit inside the official control bar (speed,
  // Live, then the player's own fullscreen button, ordered by CSS).
  const extras = document.createElement('span');
  extras.className = 'reel-extras';
  const decorate = () => {
    const bar = host.querySelector('.ap-control-bar');
    if (bar && extras.parentElement !== bar) bar.append(extras);
    const elapsed = host.querySelector('.ap-time-elapsed');
    if (elapsed && duration !== null && elapsed.dataset.duration !== clock(duration)) elapsed.dataset.duration = clock(duration);
    for (const marker of host.querySelectorAll('.ap-marker-container')) {
      const who = whoOf(marker.querySelector('.ap-tooltip')?.textContent);
      if (marker.dataset.who !== who) marker.dataset.who = who;
    }
  };
  const readDuration = async () => {
    try {
      const value = await player.getDuration();
      if (Number.isFinite(value) && value > 0) { duration = value; notify('duration', { duration }); decorate(); }
    } catch { /* The timer keeps its elapsed-only form. */ }
  };
  try {
    const url = new URL(host.dataset.source, location.href);
    if (live) url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    // The pinned stock driver retries indefinitely. Five-second retry spacing,
    // together with the owning document's 15s reconnect deadline, bounds each
    // connection episode to three attempts. Destroying the iframe cancels even
    // scheduled vendor timers; player.dispose alone does not establish that.
    const source = live ? { driver: 'websocket', url: url.href, reconnectDelay: () => 5000 } : still ? { url: url.href } : { url: url.href, format: 'segmented' };
    const create = (startAt, autoPlay) => {
      player = AsciinemaPlayer.create(source, host, {
        autoPlay, fit: 'both', controls: history, ...(still ? {cursorMode: 'hidden'} : {}),
        startAt, ...(live ? {idleTimeLimit: Infinity} : {}), ...(history ? {speed} : {}),
        // Connected box drawing (U+2500-257F) ahead of the bundled text font.
        terminalFontFamily: 'AgentReelBox, AgentReelMono',
        // app.css maps this theme to the --reel-term-* colours on the host.
        theme: themes.playerTheme(theme),
      });
      const own = player;
      for (const state of ['loading', 'playing', 'pause', 'ended', 'offline', 'error']) {
        own.addEventListener(state, () => {
          // A replaced player is no longer reported (a hung load may still
          // finish or fail after its switch gave up).
          if (own !== player) return;
          if (state === 'loading' && host.classList.contains('reel-held')) {
            // A held player (see swap) loads only to position itself, and
            // only play ends the player's loading state, which its play
            // button shows as Pause although it plays: say Play (app.css
            // draws the icon). Vendor coupling, asciinema-player 3.17.0
            // (re-verify on any bump): vendor/player.js:6834 writes this
            // label only when props.isPlaying changes, :7771 derives that
            // from isPlaying() || overlay() == "loader" (loader set :7341,
            // cleared on play :7324), and togglePlay (:7682) tests the raw
            // isPlaying(), so the button plays.
            host.classList.add('reel-stalled');
            host.querySelector('.ap-playback-button')?.setAttribute('aria-label', 'Play');
            // Not the owner's loading (it would arm the viewer's unavailable
            // deadline for a player standing still); the held deadline
            // stands in for it should the load hang.
            heldDeadline ??= setTimeout(() => {
              heldDeadline = undefined;
              notify('error');
              giveUp?.(new Error('held load timed out'));
            }, heldLimitMs);
            return;
          }
          if (state === 'playing') { clearDeadline(); playing = true; placed = true; }
          if (state === 'pause' || state === 'ended') playing = false;
          if (history && state === 'error' && placed) {
            // Failed for good (the player refuses every later command, and
            // reads 0 or where it stopped): no more positions from it, which
            // a sidecar would take as recovery. A later switch recreates it,
            // paused, where it last stood.
            const at = own.getCurrentTime();
            target = at > 0 ? at : lastFrom === own ? last : target;
            targetPlaying = false;
            placed = false;
          }
          if (state === 'loading') armDeadline();
          if (live && ['ended', 'offline', 'error'].includes(state)) { terminate(); return; }
          notify(state);
          if (history && state === 'playing' && duration === null) void readDuration();
        });
      }
      own.addEventListener('seeked', () => { if (own === player) placed = true; });
      // Any play request ends the held state (and the player's own overlays).
      own.addEventListener('play', () => {
        if (own !== player) return;
        clearHeld();
        host.classList.remove('reel-held', 'reel-stalled');
      });
      // The owning viewer mirrors speed between the parent and its panels.
      if (history) notify('speed', { speed });
    };
    create(Number(host.dataset.at), true);
    // The owning viewer may change the colours: only our own parent document,
    // same origin, with a valid spec. History recreates the player at the
    // same position; Live and the still frame reload this document with the
    // theme carried in its fragment (the stored choice may be unavailable).
    // Theme changes coalesce: one queued replacement applies the latest.
    let replace = null, wanted = null;
    window.addEventListener('message', event => {
      if (event.source !== parent || event.origin !== location.origin || event.data?.agentReel !== 'theme') return;
      const spec = themes.parse(event.data.theme);
      if (!spec) return;
      if (history && replace) {
        const queued = wanted !== null;
        wanted = spec;
        if (!queued) void replace(() => { theme = wanted; themes.apply(host, theme); themes.apply(document.documentElement, theme); wanted = null; });
        return;
      }
      // Replace, not push: repeated colour changes must not fill the viewer's
      // session history with fragment entries.
      window.history.replaceState(null, '', '#' + new URLSearchParams({ theme: themes.format(spec) }));
      location.reload();
    });
    if (history) {
      // Speed recreates the player at the same position: the pinned player
      // fixes its speed at creation.
      const speedButton = document.createElement('button');
      speedButton.type = 'button'; speedButton.className = 'reel-speed';
      speedButton.title = 'Playback speed'; speedButton.textContent = '1×';
      speedButton.setAttribute('aria-label', 'Playback speed 1×');
      // One switch at a time, in order: switches queue behind the one in
      // progress, so no replaced player escapes disposal. The speed button is
      // disabled (synchronously) while any switch is pending, so a click during
      // a switch is ignored. The player fixes its speed and colours at
      // creation, so both recreate it in place.
      let chain = Promise.resolve(), pending = 0;
      const swap = async change => {
        const old = player;
        // Not yet placed (still loading, or its switch gave up on a hung
        // load), it reads 0 and not playing: use where it was going and
        // whether it was to play there.
        const at = placed ? await old.getCurrentTime() : target;
        const resume = placed ? playing : targetPlaying;
        change();
        clearHeld();
        try { await old.dispose(); } catch { /* Replaced below either way. */ }
        host.replaceChildren();
        playing = false;
        placed = false; target = at; targetPlaying = resume;
        // Held: recreated paused, until it next plays.
        host.classList.remove('reel-stalled');
        host.classList.toggle('reel-held', !resume);
        create(at, resume);
        // Created paused, the player loads nothing and shows a blank terminal
        // under its start overlay. Seeking it loads the recording and renders
        // this frame, still paused (it reports seeked, never play or
        // playing); app.css hides the start overlay. The switch stays pending
        // until then, so the next one reads this position, not 0, unless the
        // held deadline gives up on a hung load first.
        if (!resume) {
          const fresh = player;
          try {
            await new Promise((resolve, reject) => { giveUp = reject; fresh.seek(at).then(resolve, reject); });
          } finally { giveUp = null; clearHeld(); }
        }
      };
      replace = change => {
        pending++;
        speedButton.disabled = true;
        chain = chain.then(() => swap(change)).catch(() => {}).finally(() => {
          if (--pending === 0) speedButton.disabled = false;
        });
        return chain;
      };
      speedButton.addEventListener('click', () => replace(() => {
        speed = speeds[(speeds.indexOf(speed) + 1) % speeds.length];
        speedButton.textContent = `${speed}×`;
        speedButton.setAttribute('aria-label', `Playback speed ${speed}×`);
      }));
      extras.append(speedButton);
      // A following sidecar mirrors the parent's speed: only our own parent
      // document, same origin, a speed this player offers.
      if (sidecar) {
        window.addEventListener('message', event => {
          if (event.source !== parent || event.origin !== location.origin || event.data?.agentReel !== 'speed') return;
          const value = event.data.speed;
          if (!speeds.includes(value) || value === speed) return;
          void replace(() => {
            speed = value;
            speedButton.textContent = `${speed}×`;
            speedButton.setAttribute('aria-label', `Playback speed ${speed}×`);
          });
        });
      }
      // A sidecar panel has no in-frame Live link: its Live is the panel's own
      // toggle and never navigates the page.
      const liveURL = sidecar ? '' : host.dataset.liveUrl;
      if (liveURL) {
        const toLive = document.createElement('a');
        toLive.className = 'reel-live'; toLive.href = liveURL; toLive.target = '_top';
        toLive.setAttribute('aria-pressed', 'false'); toLive.title = 'Return to Live';
        const dot = document.createElement('span'); dot.className = 'dot';
        toLive.append(dot, 'Live');
        extras.append(toLive);
      }
      // Keystroke overlay (keys-overlay.mjs): only when Go names a keys source.
      if (host.dataset.keysApi) {
        import('/assets/keys-overlay.mjs')
          .then(keys => keys.attach({ host, extras, api: host.dataset.keysApi, time: async () => player ? await player.getCurrentTime() : NaN }))
          .catch(() => {});
      }
      new MutationObserver(decorate).observe(host, { childList: true, subtree: true });
      decorate();
      // Report the position so the viewer can highlight the current mark
      // (not before a replacement is placed: it reads 0 until then). Each
      // player reports once when placed, even where the last one stood: a
      // sidecar takes that as the child playing again after a failure.
      setInterval(async () => {
        if (document.hidden || !player || !placed) return;
        try {
          const from = player, time = await from.getCurrentTime();
          // The await yields: a failure (or a swap) in that gap has un-placed or
          // replaced the player read, so its reading is not reported.
          if (!placed || from !== player) return;
          if (Number.isFinite(time) && (Math.abs(time - last) >= 0.2 || from !== lastFrom)) { last = time; lastFrom = from; notify('time', { time }); }
          if (strip && Number.isFinite(time)) {
            stripText = `Following parent · ${clock(time)}`;
            if (performance.now() >= hintUntil && strip.textContent !== stripText) strip.textContent = stripText;
          }
        } catch { /* Reported on the next tick. */ }
      }, 500);
      // The owning viewer may ask to seek (a click on a mark): only our own
      // parent document, same origin, with a finite position.
      window.addEventListener('message', event => {
        if (event.source !== parent || event.origin !== location.origin || event.data?.agentReel !== 'seek') return;
        const at = Number(event.data.at);
        if (Number.isFinite(at) && at >= 0) void player.seek(at);
      });
      // A sidecar panel (sidecar-view.mjs) follows its parent's play state.
      window.addEventListener('message', event => {
        if (event.source !== parent || event.origin !== location.origin) return;
        if (event.data?.agentReel === 'play' && !playing) void player.play();
        else if (event.data?.agentReel === 'pause' && playing) void player.pause();
      });
    }
  } catch {
    clearDeadline();
    document.querySelector('#player-error').textContent = 'Player unavailable.';
    notify('error');
  }
} else {
  notify('error');
}
