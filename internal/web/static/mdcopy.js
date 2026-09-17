// Copying a selection out of a rendered reply puts markdown on the
// clipboard, not the flattened text the browser would make of it. Bold,
// links, lists, fences and tables survive a paste into a ticket or
// another chat. The rendered HTML rides along in the text/html flavor for
// editors that paste rich text. Ported from T3 Code's markdown-clipboard
// and cut down to what goldmark emits (see views/md.go).
(() => {
	const SKIP = new Set(['BUTTON', 'INPUT', 'SCRIPT', 'STYLE', 'TEMPLATE', 'svg']);
	const skipped = (el) => SKIP.has(el.tagName) || el.getAttribute('aria-hidden') === 'true';

	// Whitespace around an inline marker moves outside it: "` bold `"
	// becomes " **bold** ", which markdown parses; "** bold **" does not.
	const wrap = (s, m) => {
		const [, lead, core, tail] = /^(\s*)([\s\S]*?)(\s*)$/.exec(s);
		return core ? lead + m + core + m + tail : s;
	};
	const longestRun = (s, re) => Math.max(0, ...(s.match(re) || []).map((r) => r.length));
	const inlineCode = (code) => {
		const run = longestRun(code, /`+/g);
		const fence = '`'.repeat(run ? run + 1 : 1);
		const pad = code.startsWith('`') || code.endsWith('`') ? ' ' : '';
		return fence + pad + code + pad + fence;
	};
	const codeBlock = (pre) => {
		const code = pre.textContent.replace(/\n$/, '');
		const fence = '`'.repeat(Math.max(3, longestRun(code, /`{3,}/g) + 1));
		const m = /(?:^|\s)language-(\S+)/.exec(pre.querySelector('code')?.className || '');
		return fence + (m ? m[1] : '') + '\n' + code + '\n' + fence + '\n\n';
	};

	const anchor = (a) => {
		const href = a.getAttribute('href') || '';
		if (!href) return children(a);
		const label = children(a).trim();
		if (!label) return '';
		// Autolinks render their address as the label; give the address back.
		if (label === href || label === href.replace(/^(https?:\/\/|mailto:)/, '')) return label;
		const title = a.getAttribute('title');
		return '[' + label + '](' + href + (title ? ' "' + title.replace(/"/g, '\\"') + '"' : '') + ')';
	};

	const listItem = (li, ordered, n) => {
		const box = li.querySelector(':scope > input[type="checkbox"], :scope > p:first-child > input[type="checkbox"]');
		const task = box ? '[' + (box.checked ? 'x' : ' ') + '] ' : '';
		const marker = (ordered ? n + '. ' : '- ') + task;
		let body = children(li).replace(/\n{3,}/g, '\n\n').trim();
		// A tight item (no paragraph of its own) keeps a nested list on the
		// next line; a blank line there would loosen the whole list.
		if (!li.querySelector(':scope > p')) body = body.replace(/\n{2,}/g, '\n');
		const indent = ' '.repeat(marker.length);
		const [first = '', ...rest] = body.split('\n');
		return [marker + first, ...rest.map((l) => (l ? indent + l : l))].join('\n');
	};
	const list = (el, ordered) => {
		const start = parseInt(el.getAttribute('start') || '1', 10) || 1;
		const items = [...el.children].filter((c) => c.tagName === 'LI');
		if (!items.length) return '';
		const loose = items.some((li) => li.querySelector(':scope > p'));
		return items.map((li, i) => listItem(li, ordered, start + i)).join(loose ? '\n\n' : '\n') + '\n\n';
	};

	const quote = (bq) => {
		const body = children(bq).replace(/\n{3,}/g, '\n\n').trim();
		return body ? body.split('\n').map((l) => (l ? '> ' + l : '>')).join('\n') + '\n\n' : '';
	};

	const cell = (td) => children(td).replace(/\n+/g, ' ').trim().replace(/\|/g, '\\|');
	const align = (td) => {
		const a = td.style.textAlign || td.getAttribute('align') || '';
		return a === 'center' ? ':---:' : a === 'right' ? '---:' : a === 'left' ? ':---' : '---';
	};
	const table = (t) => {
		const lines = [];
		for (const row of t.querySelectorAll(':scope > thead > tr, :scope > tbody > tr, :scope > tr')) {
			const cells = [...row.children].filter((c) => c.tagName === 'TH' || c.tagName === 'TD');
			if (!cells.length) continue;
			lines.push('| ' + cells.map(cell).join(' | ') + ' |');
			if (lines.length === 1) lines.push('| ' + cells.map(align).join(' | ') + ' |');
		}
		return lines.length ? lines.join('\n') + '\n\n' : '';
	};

	// Text nodes pass through, except for two kinds of whitespace: the
	// newline goldmark leaves between blocks becomes one newline, and the
	// newline after a <br> is dropped since the <br> already made one.
	const children = (node) => {
		let out = '';
		let br = false;
		for (const c of node.childNodes) {
			if (c.nodeType === Node.TEXT_NODE) {
				let t = c.textContent;
				if (br) t = t.replace(/^\s+/, '');
				else if (t.includes('\n') && !t.trim()) t = '\n';
				out += t;
				br = false;
				continue;
			}
			br = c.nodeType === Node.ELEMENT_NODE && c.tagName === 'BR';
			out += serialize(c);
		}
		return out;
	};

	const serialize = (el) => {
		if (el.nodeType === Node.TEXT_NODE) return el.textContent;
		if (el.nodeType !== Node.ELEMENT_NODE || skipped(el)) return '';
		// A prompt in a selection that spans messages is markdown already.
		if (el.classList.contains('bubble')) return el.textContent.trim() + '\n\n';
		const h = /^H([1-6])$/.exec(el.tagName);
		if (h) return '#'.repeat(+h[1]) + ' ' + children(el).trim() + '\n\n';
		switch (el.tagName) {
			case 'BR': return '\n';
			case 'HR': return '---\n\n';
			case 'P': return children(el).trim() + '\n\n';
			case 'PRE': return codeBlock(el);
			case 'CODE': { const t = el.textContent; return t.includes('\n') ? t : inlineCode(t); }
			case 'STRONG': case 'B': return wrap(children(el), '**');
			case 'EM': case 'I': return wrap(children(el), '*');
			case 'DEL': case 'S': return wrap(children(el), '~~');
			case 'A': return anchor(el);
			case 'IMG': {
				const alt = el.getAttribute('alt') || '';
				const src = el.getAttribute('src') || '';
				const title = el.getAttribute('title');
				return src ? '![' + alt + '](' + src + (title ? ' "' + title.replace(/"/g, '\\"') + '"' : '') + ')' : '';
			}
			case 'UL': return list(el, false);
			case 'OL': return list(el, true);
			case 'BLOCKQUOTE': return quote(el);
			case 'TABLE': return table(el);
			case 'DIV': case 'SECTION': case 'ARTICLE': {
				const s = children(el);
				return s && !s.endsWith('\n') ? s + '\n' : s;
			}
			default: return children(el);
		}
	};

	// A drag over a code block usually ends on its last newline and pulls
	// the closing pre into the range, so the fragment is a whole block
	// even though only code was highlighted. A fragment whose only visible
	// content is one code block copies as bare code, the same as a
	// selection that stayed inside the pre.
	const soleCodeBlock = (root) => {
		let pre = null;
		let other = false;
		const scan = (node) => {
			for (const c of node.childNodes) {
				if (other) return;
				if (c.nodeType === Node.TEXT_NODE) { if (c.textContent.trim()) other = true; continue; }
				if (c.nodeType !== Node.ELEMENT_NODE || skipped(c)) continue;
				if (c.tagName === 'PRE') { if (pre) other = true; else pre = c; continue; }
				if (c.tagName === 'IMG' || c.tagName === 'HR') { other = true; continue; }
				if (c.tagName === 'LI') {
					// An item gets a marker even when it renders no text, so an
					// item that does not hold the block is content of its own.
					const before = pre;
					scan(c);
					if (!other && pre === before) other = true;
					continue;
				}
				scan(c);
			}
		};
		scan(root);
		return other ? null : pre;
	};

	// Trailing spaces and runs of blank lines are serializer artifacts;
	// fenced code keeps its own.
	const tidy = (md) => md.split(/(```[\s\S]*?(?:```|$))/)
		.map((p, i) => (i % 2 ? p : p.replace(/[ \t]+(?=\n)/g, '').replace(/\n{3,}/g, '\n\n')))
		.join('').trim();

	// payload turns the selection into both clipboard flavors, or returns
	// null when any range lies outside rendered markdown so the browser's
	// own copy runs. Exposed for the browser checks in /tmp/pw.
	window.mdClipboard = (sel) => {
		const texts = [];
		const htmls = [];
		for (let i = 0; i < sel.rangeCount; i++) {
			const r = sel.getRangeAt(i);
			if (r.collapsed) continue;
			const a = r.commonAncestorContainer;
			const el = a.nodeType === Node.ELEMENT_NODE ? a : a.parentElement;
			const box = document.createElement('div');
			box.append(r.cloneContents());
			if (!el || (!el.closest('.md') && !box.querySelector('.md'))) return null;
			let text;
			if (el.closest('pre')) {
				text = r.toString();
			} else {
				const pre = soleCodeBlock(box);
				text = pre ? pre.textContent.replace(/\n$/, '') : tidy(children(box));
			}
			if (!text) continue;
			texts.push(text);
			for (const n of box.querySelectorAll('button, input, script, style, svg, [aria-hidden="true"]')) n.remove();
			htmls.push(box.innerHTML);
		}
		if (!texts.length) return null;
		return { text: texts.join('\n\n'), html: '<meta charset="utf-8">' + htmls.join('') };
	};

	document.addEventListener('copy', (e) => {
		// A form field, the terminal and the file editor copy their own text.
		if (e.target instanceof Element && e.target.closest('input, textarea, [contenteditable], .xterm')) return;
		const sel = window.getSelection();
		if (!sel || sel.isCollapsed || !e.clipboardData) return;
		const p = window.mdClipboard(sel);
		if (!p) return;
		e.preventDefault();
		e.clipboardData.setData('text/plain', p.text);
		e.clipboardData.setData('text/html', p.html);
	});
})();
