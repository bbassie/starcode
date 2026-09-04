// Terminal panel glue. The server keeps one shell per thread; this file
// renders it with xterm.js, sends keystrokes with POSTs (serialized so
// ordering survives), and reads output from an SSE stream of base64 chunks.
// The shell outlives the page: reconnecting replays the scrollback.
(() => {
  const panel = document.getElementById('termpanel');
  if (!panel || typeof Terminal === 'undefined') return;
  const id = panel.dataset.thread;
  const mount = panel.querySelector('.term-mount');
  let term, fit, es;
  let queue = Promise.resolve();

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

  const connect = () => {
    es = new EventSource(`/api/term/${id}/stream?cols=${term.cols}&rows=${term.rows}`);
    // Every (re)connect replays the scrollback, so start from a clean grid.
    es.onopen = () => term.reset();
    es.addEventListener('out', (e) => e.data && term.write(decode(e.data)));
    es.addEventListener('exit', () => {
      es.close();
      es = null;
      term.write('\r\n\x1b[2m[shell exited, use the restart button above to start a new one]\x1b[0m\r\n');
    });
  };

  const restart = () => {
    if (es) es.close();
    es = null;
    queue = Promise.resolve();
    fetch(`/api/term/${id}/kill`, { method: 'POST' }).finally(() => {
      connect();
      term.focus();
    });
  };

  const init = () => {
    term = new Terminal({
      fontFamily: cssVar('--font-mono') || 'monospace',
      fontSize: 13,
      theme: theme(),
      cursorBlink: true,
      scrollback: 5000,
    });
    fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(mount);
    fit.fit();
    term.onData((data) => {
      queue = queue
        .then(() => fetch(`/api/term/${id}/input`, { method: 'POST', body: data }))
        .catch(() => {});
    });
    term.onResize(({ cols, rows }) => {
      fetch(`/api/term/${id}/resize?cols=${cols}&rows=${rows}`, { method: 'POST' }).catch(() => {});
    });
    panel.querySelector('.term-restart').addEventListener('click', restart);
    // Datastar mirrors the theme signal onto <body data-theme>.
    new MutationObserver(() => {
      term.options.theme = theme();
    }).observe(document.body, { attributeFilter: ['data-theme'] });
    connect();
  };

  // The panel starts hidden; boot the terminal the first time it gets real
  // dimensions, refit whenever they change, and focus it on each open.
  let width = 0;
  new ResizeObserver(() => {
    const w = mount.clientWidth;
    if (w > 0 && !term) init();
    else if (w > 0) fit.fit();
    if (w > 0 && width === 0) term.focus();
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
