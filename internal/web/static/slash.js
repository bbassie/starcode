// The composer's slash menu. A prompt that is "/" or "/" and letters,
// and nothing else, opens the form's .slash-menu box with the thread's
// commands and skills (GET /api/threads/<id>/commands, plain HTML); what
// follows the slash filters the rows by their data-k. Arrows walk the
// rows, Enter picks the marked one, Escape closes, as does a click
// elsewhere or text that no longer starts with "/". A pick puts the
// command in the prompt with a space after it, so a command without an
// argument only needs send. The home page has no thread and no menu.
(() => {
	const opener = /^\/[\w-]*$/;
	let ta = null;   // the prompt whose menu is open
	let menu = null;
	let fetches = 0; // ignore a reply for a menu that closed meanwhile

	const threadID = () => (location.pathname.match(/^\/threads\/([^/]+)/) || [])[1];
	const rows = () => menu ? [...menu.querySelectorAll('.opt')].filter((r) => !r.hidden) : [];
	const close = () => {
		if (menu) menu.classList.remove('open');
		menu = ta = null;
		fetches++;
	};
	const mark = (row) => {
		for (const r of rows()) r.classList.toggle('sel', r === row);
		row?.scrollIntoView({ block: 'nearest' });
	};
	const filter = () => {
		if (!menu || !ta) return;
		const q = ta.value.slice(1).toLowerCase();
		let n = 0;
		for (const r of menu.querySelectorAll('.opt')) {
			const hit = !q || (r.dataset.k || '').includes(q);
			r.hidden = !hit;
			if (hit) n++;
		}
		const empty = menu.querySelector('.slash-empty');
		if (empty) empty.hidden = n > 0;
		const vis = rows();
		mark(vis.find((r) => r.classList.contains('sel')) || vis[0]);
	};
	const open = async (t) => {
		const id = threadID();
		const box = t.form && t.form.querySelector('.slash-menu');
		if (!id || !box) return;
		ta = t;
		menu = box;
		const n = ++fetches;
		let html = '';
		try {
			const res = await fetch('/api/threads/' + id + '/commands');
			if (res.ok) html = await res.text();
		} catch (e) {}
		if (n !== fetches || !html) return;
		box.innerHTML = html;
		box.classList.add('open');
		filter();
	};

	document.addEventListener('input', (e) => {
		const t = e.target;
		if (!(t instanceof HTMLTextAreaElement) || !t.classList.contains('prompt')) return;
		if (!opener.test(t.value)) { if (ta === t) close(); return; }
		if (ta === t && menu && menu.isConnected) filter();
		else { close(); open(t); }
	});

	// Capture phase: the prompt's own keydown (Enter sends) and the page's
	// hotkeys (Escape closes panels) must not see a key the menu took.
	document.addEventListener('keydown', (e) => {
		if (!menu || e.target !== ta || e.isComposing) return;
		const vis = rows();
		const take = () => { e.preventDefault(); e.stopPropagation(); };
		if (e.key === 'Escape') { take(); close(); return; }
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			if (!vis.length) return;
			take();
			const i = vis.findIndex((r) => r.classList.contains('sel'));
			mark(vis[(i + (e.key === 'ArrowDown' ? 1 : vis.length - 1)) % vis.length]);
			return;
		}
		if (e.key === 'Enter' && !e.shiftKey) {
			const sel = vis.find((r) => r.classList.contains('sel')) || vis[0];
			// Nothing matches: close and let Enter send the text as typed.
			if (!sel) { close(); return; }
			take();
			slashPick(sel);
		}
	}, true);

	document.addEventListener('click', (e) => {
		if (!menu) return;
		const t = e.target instanceof Element ? e.target : null;
		if (t && (t === ta || menu.contains(t))) return;
		close();
	});

	// slashPick puts the row's command in its form's prompt. The input
	// event lifts the text into the $prompt signal; the trailing space
	// keeps the menu from reopening on it.
	window.slashPick = (el) => {
		const form = el.closest('form');
		const t = form && form.querySelector('textarea.prompt');
		close();
		if (!t) return;
		const text = el.dataset.cmd + ' ';
		t.value = text;
		t.dispatchEvent(new Event('input', { bubbles: true }));
		t.focus();
		t.setSelectionRange(text.length, text.length);
	};
})();
