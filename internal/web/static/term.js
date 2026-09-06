// Terminal panel glue. The server keeps shells per thread and pane; this
// file renders each pane with xterm.js, sends keystrokes with POSTs
// (serialized per pane so ordering survives), and reads output from an
// SSE stream of base64 chunks. Shells outlive the page: reconnecting
// replays the scrollback, and the pane list comes back from the server.
(() => {
  const panel = document.getElementById('termpanel');
  if (!panel || typeof Terminal === 'undefined') return;
  const id = panel.dataset.thread;
  const mount = panel.querySelector('.term-mount');
  const panes = new Map(); // pane number -> { el, term, fit, es, queue }
  let active = null;
  let booted = false;

  const cssVar = (name) => getComputedStyle(document.body).getPropertyValue(name).trim();
  const theme = () => ({
    background: cssVar('--bg'),
    foreground: cssVar('--fg'),
    cursor: cssVar('--cursor') || cssVar('--accent'),
    cursorAccent: cssVar('--bg'),
    selectionBackground: cssVar('--sel'),
  });

  const decode = (b64) => {
    const bin = atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  };

  const url = (n, path, extra = '') => `/api/term/${id}/${path}?pane=${n}${extra}`;

  const setActive = (n) => {
    active = n;
    for (const [k, p] of panes) p.el.classList.toggle('active', k === n);
  };

  const connect = (n) => {
    const p = panes.get(n);
    if (!p) return;
    p.es = new EventSource(url(n, 'stream', `&cols=${p.term.cols}&rows=${p.term.rows}`));
    // Every (re)connect replays the scrollback, so start from a clean grid.
    p.es.onopen = () => p.term.reset();
    p.es.addEventListener('out', (e) => e.data && p.term.write(decode(e.data)));
    p.es.addEventListener('exit', () => {
      p.es.close();
      p.es = null;
      p.term.write('\r\n\x1b[2m[shell exited, use the restart button above to start a new one]\x1b[0m\r\n');
    });
  };

  const restart = (n) => {
    const p = panes.get(n);
    if (!p) return;
    if (p.es) p.es.close();
    p.es = null;
    p.queue = Promise.resolve();
    fetch(url(n, 'kill'), { method: 'POST' }).finally(() => {
      connect(n);
      p.term.focus();
    });
  };

  // addPane opens pane n: an xterm in its own column, connected once it
  // has a size. Panes keep the panel's height and share its width.
  const addPane = (n) => {
    if (panes.has(n)) return panes.get(n);
    const el = document.createElement('div');
    el.className = 'term-pane';
    el.dataset.pane = n;
    mount.append(el);
    const term = new Terminal({
      fontFamily: cssVar('--font-mono') || 'monospace',
      fontSize: 13,
      theme: theme(),
      cursorBlink: true,
      scrollback: 5000,
    });
    // Page shortcuts marked for the terminal (toggle it, the changes
    // panel) go to the layout's key handler instead of the shell.
    term.attachCustomKeyEventHandler((e) => !(e.type === 'keydown' && window.hotkeyInTerm && hotkeyInTerm(e)));
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(el);
    const p = { el, term, fit, es: null, queue: Promise.resolve() };
    panes.set(n, p);
    term.onData((data) => {
      p.queue = p.queue
        .then(() => fetch(url(n, 'input'), { method: 'POST', body: data }))
        .catch(() => {});
    });
    // A resize before the stream has opened (the first fit) or after the
    // shell is gone would only get a 409; the stream URL carries the size.
    term.onResize(({ cols, rows }) => {
      if (!p.es) return;
      fetch(url(n, 'resize', `&cols=${cols}&rows=${rows}`), { method: 'POST' }).catch(() => {});
    });
    term.textarea?.addEventListener('focus', () => setActive(n));
    el.addEventListener('pointerdown', () => setActive(n));
    // Connect once the pane has real dimensions; refit when they change.
    let connected = false;
    new ResizeObserver(() => {
      if (el.clientWidth <= 0) return;
      fit.fit();
      if (!connected) {
        connected = true;
        connect(n);
      }
    }).observe(el);
    setActive(n);
    return p;
  };

  const closePane = (n) => {
    const p = panes.get(n);
    if (!p) return;
    if (p.es) p.es.close();
    p.es = null;
    fetch(url(n, 'kill'), { method: 'POST' }).catch(() => {});
    p.term.dispose();
    p.el.remove();
    panes.delete(n);
    if (panes.size === 0) {
      // The last pane closing hides the panel; the next open starts fresh.
      booted = false;
      panel.querySelector('.term-hide')?.click();
      return;
    }
    const rest = [...panes.keys()];
    setActive(rest[rest.length - 1]);
    for (const q of panes.values()) q.fit.fit();
    panes.get(active).term.focus();
  };

  // The server allows 16 panes per thread; more is not a terminal anyone
  // can read anyway.
  const split = () => {
    let n = 1;
    while (panes.has(n)) n++;
    if (n > 16) return;
    addPane(n);
    for (const q of panes.values()) q.fit.fit();
    panes.get(n).term.focus();
  };

  // boot restores the panes the server still has shells for (a reload,
  // or the panel reopened after closing the page), or opens the first.
  const boot = async () => {
    booted = true;
    let list = [];
    try {
      const r = await fetch(`/api/term/${id}/panes`);
      if (r.ok) list = await r.json();
    } catch {}
    if (!list.length) list = [1];
    for (const n of list) addPane(n);
    setActive(list[list.length - 1]);
    panes.get(active).term.focus();
  };

  panel.querySelector('.term-split').addEventListener('click', split);
  panel.querySelector('.term-restart').addEventListener('click', () => active && restart(active));
  panel.querySelector('.term-close-pane').addEventListener('click', () => active && closePane(active));
  // Datastar mirrors the theme signal onto <body data-theme>.
  new MutationObserver(() => {
    for (const p of panes.values()) p.term.options.theme = theme();
  }).observe(document.body, { attributeFilter: ['data-theme'] });

  // The panel starts hidden; boot the terminals the first time it gets
  // real dimensions, refit whenever they change, and focus on each open.
  let width = 0;
  new ResizeObserver(() => {
    const w = mount.clientWidth;
    if (w > 0 && !booted) boot();
    else if (w > 0) for (const p of panes.values()) p.fit.fit();
    if (w > 0 && width === 0 && active) panes.get(active)?.term.focus();
    width = w;
  }).observe(mount);

  const drag = panel.querySelector('.term-drag');
  drag.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    drag.setPointerCapture(e.pointerId);
    const startY = e.clientY;
    const startH = panel.clientHeight;
    const move = (ev) => {
      const h = Math.min(Math.max(startH + (startY - ev.clientY), 120), innerHeight * 0.8);
      panel.style.height = h + 'px';
    };
    const up = () => {
      drag.removeEventListener('pointermove', move);
      drag.removeEventListener('pointerup', up);
      drag.removeEventListener('pointercancel', up);
    };
    drag.addEventListener('pointermove', move);
    drag.addEventListener('pointerup', up);
    drag.addEventListener('pointercancel', up);
  });
})();
