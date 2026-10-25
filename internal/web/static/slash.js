// The composer's two menus. A prompt that is "/" or "/" and letters,
// and nothing else, opens the form's .slash-menu box with the thread's
// commands and skills (GET /api/threads/<id>/commands, plain HTML); what
// follows the slash filters the rows by their data-k. A "#" at the start
// of a word, anywhere in the prompt, opens the same box with the
// repository's open pull requests (GET /api/threads/<id>/prpick), and the
// digits or words typed after it filter them. Arrows walk the rows, Enter
// picks the marked one, Escape closes, as does a click elsewhere or text
// that no longer fits. A command pick puts the command in the prompt with
// a space after it, so a command without an argument only needs send; a
// pull request pick puts a link to it where the "#" was. An "@" at the
// start of a word opens the project's files and the other threads (GET
// /api/threads/<id>/atpick?q=); the server filters those, so every few
// keystrokes refetch them. A pick puts @path or a link to the thread
// where the "@" was. The home page has no thread and no menus.
(() => {
	const opener = /^\/[\w-]*$/;
	const hash = /(?:^|\s)#([\w-]*)$/;
	const at = /(?:^|\s)@([^\s@]*)$/;
	const trigger = { pr: hash, at: at };
	let ta = null;   // the prompt whose menu is open
	let menu = null;
	let mode = '';   // "slash", "pr" or "at"
	let refetch = 0; // the "@" menu's pending refetch timer
	let fetches = 0; // ignore a reply for a menu that closed meanwhile

	const threadID = () => (location.pathname.match(/^\/threads\/([^/]+)/) || [])[1];
	const rows = () => menu ? [...menu.querySelectorAll('.opt')].filter((r) => !r.hidden) : [];
	const close = () => {
		if (menu) menu.classList.remove('open');
		menu = ta = null;
		mode = '';
		fetches++;
		clearTimeout(refetch);
	};
	const mark = (row) => {
		for (const r of rows()) r.classList.toggle('sel', r === row);
		row?.scrollIntoView({ block: 'nearest' });
	};
	// query is what the reader typed after the "/", the "#" or the "@".
	const query = (t) => {
		if (mode === 'slash') return t.value.slice(1).toLowerCase();
		const m = (trigger[mode] || hash).exec(t.value.slice(0, t.selectionStart));
		return m ? m[1].toLowerCase() : '';
	};
	const filter = () => {
		if (!menu || !ta) return;
		if (mode === 'at') {
			// The server filtered the rows; mark the first.
			const vis = rows();
			mark(vis.find((r) => r.classList.contains('sel')) || vis[0]);
			return;
		}
		const q = query(ta);
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
	const open = async (t, want) => {
		const id = threadID();
		const box = t.form && t.form.querySelector('.slash-menu');
		if (!id || !box) return;
		ta = t;
		menu = box;
		mode = want;
		const n = ++fetches;
		let html = '';
		try {
			const path = want === 'pr' ? '/prpick' : want === 'at' ? '/atpick?q=' + encodeURIComponent(query(t)) : '/commands';
			const res = await fetch('/api/threads/' + id + path);
			if (res.ok) html = await res.text();
		} catch (e) {}
		if (n !== fetches || !html) return;
		box.innerHTML = html;
		// An agent without commands (Codex takes none over its app
		// server) gets no menu rather than an empty one.
		if (!box.querySelector('.opt') && want !== 'at') { close(); return; }
		box.classList.add('open');
		filter();
	};
	// wanted is the menu the prompt's text calls for, if any.
	const wanted = (t) => {
		if (opener.test(t.value)) return 'slash';
		if (t.selectionStart !== t.selectionEnd) return '';
		const before = t.value.slice(0, t.selectionStart);
		if (hash.test(before)) return 'pr';
		if (at.test(before)) return 'at';
		return '';
	};

	document.addEventListener('input', (e) => {
		const t = e.target;
		if (!(t instanceof HTMLTextAreaElement) || !t.classList.contains('prompt')) return;
		const want = wanted(t);
		if (!want) { if (ta === t) close(); return; }
		if (ta === t && mode === want && menu && menu.isConnected) {
			if (want !== 'at') { filter(); return; }
			clearTimeout(refetch);
			refetch = setTimeout(() => { if (ta === t && mode === 'at') open(t, 'at'); }, 120);
		} else { close(); open(t, want); }
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
			// Nothing matches, or a bare "#" (a heading, say): close and
			// let Enter do what it does.
			if (!sel || (mode !== 'slash' && !query(ta))) { close(); return; }
			take();
			if (mode === 'pr') prPick(sel);
			else if (mode === 'at') atPick(sel);
			else slashPick(sel);
		}
	}, true);

	document.addEventListener('click', (e) => {
		if (!menu) return;
		const t = e.target instanceof Element ? e.target : null;
		if (t && (t === ta || menu.contains(t))) return;
		close();
	});

	const put = (t, text, caret) => {
		t.value = text;
		t.dispatchEvent(new Event('input', { bubbles: true }));
		t.focus();
		t.setSelectionRange(caret, caret);
	};

	// slashPick puts the row's command in its form's prompt. The input
	// event lifts the text into the $prompt signal; the trailing space
	// keeps the menu from reopening on it.
	window.slashPick = (el) => {
		const form = el.closest('form');
		const t = form && form.querySelector('textarea.prompt');
		close();
		if (!t) return;
		const text = el.dataset.cmd + ' ';
		put(t, text, text.length);
	};

	// prPick swaps the "#" and what follows it for a markdown link to the
	// pull request, which reads as #123 and gives the agent the address.
	window.prPick = (el) => swap(el, hash, el.dataset.link);

	// atPick swaps the "@" and what follows it for the row's @path or
	// thread link.
	window.atPick = (el) => swap(el, at, el.dataset.ins);

	// swap replaces the trigger (re) before the caret with text.
	const swap = (el, re, text) => {
		const form = el.closest('form');
		const t = form && form.querySelector('textarea.prompt');
		close();
		if (!t) return;
		const before = t.value.slice(0, t.selectionStart);
		const m = re.exec(before);
		if (!m) return;
		const start = before.length - m[1].length - 1;
		const ins = text + ' ';
		put(t, t.value.slice(0, start) + ins + t.value.slice(t.selectionStart), start + ins.length);
	};
})();
