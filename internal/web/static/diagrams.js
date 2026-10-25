// Mermaid diagrams in rendered replies. A ```mermaid block of a finished
// message (not .streaming) is drawn as a diagram; the source stays in the
// page, hidden, and a click on the diagram shows it again. Mermaid is big
// (static/mermaid.js, gzipped), so it loads on the first diagram only.
// Datastar's morph can put the original <pre> back when it patches the
// message; the next scan draws it again from the cache, keyed by theme
// and source. A block mermaid cannot parse stays as code.
(() => {
	const src = document.currentScript.dataset.mermaid;
	let loading = null;
	let theme = '';
	const cache = new Map();
	let seq = 0;
	// busy holds the blocks being drawn. Not an attribute: the page's
	// first morph drops attributes the server did not send.
	const busy = new WeakSet();

	const load = () => loading ||= new Promise((ok, fail) => {
		const s = document.createElement('script');
		s.src = src;
		s.onload = ok;
		s.onerror = fail;
		document.head.append(s);
	});
	const dark = () => getComputedStyle(document.body).colorScheme !== 'light';

	const draw = async (code) => {
		const pre = code.parentElement;
		const text = code.textContent;
		const want = dark() ? 'dark' : 'default';
		const key = want + '\n' + text;
		busy.add(pre);
		let svg = cache.get(key);
		if (svg === undefined) {
			try {
				await load();
				if (theme !== want) {
					window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: want });
					theme = want;
				}
				svg = (await window.mermaid.render('mmd-' + ++seq, text)).svg;
			} catch (e) {
				svg = '';
				// A failed render leaves its error box in the body.
				document.getElementById('dmmd-' + seq)?.remove();
			}
			cache.set(key, svg);
		}
		busy.delete(pre);
		if (!pre.isConnected || code.textContent !== text || drawn(pre)) return;
		pre.dataset.mmd = svg ? 'drawn' : 'failed';
		if (!svg) return;
		const box = document.createElement('div');
		box.className = 'mermaid-diagram';
		box.title = 'Click to show the source';
		box.innerHTML = svg;
		box.addEventListener('click', () => { pre.hidden = !pre.hidden; });
		pre.hidden = true;
		pre.after(box);
	};

	const drawn = (pre) => pre.nextElementSibling?.classList.contains('mermaid-diagram');
	const scan = () => {
		for (const code of document.querySelectorAll('.md:not(.streaming) pre > code.language-mermaid')) {
			const pre = code.parentElement;
			if (drawn(pre) || busy.has(pre) || pre.dataset.mmd === 'failed') continue;
			draw(code);
		}
	};
	window.onDOM?.(scan);
	scan();
	// A change of theme redraws every diagram in the new colours.
	new MutationObserver(() => {
		for (const d of document.querySelectorAll('.mermaid-diagram')) {
			const pre = d.previousElementSibling;
			pre?.removeAttribute('data-mmd');
			if (pre) pre.hidden = false;
			d.remove();
		}
		scan();
	}).observe(document.body, { attributeFilter: ['data-theme'] });
})();
