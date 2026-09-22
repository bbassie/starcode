// Quote a reply. Selecting text in an agent's reply (or its thinking, or
// a tool's output) raises a small "Quote" button over the selection; it
// puts the selection into the composer as a markdown blockquote at the
// caret, with a blank line after it for the reader's comment. Rendered
// markdown is quoted as markdown (window.mdClipboard, from mdcopy.js),
// so code and lists survive; anything else as plain text.
(() => {
	const btn = document.createElement('button');
	btn.type = 'button';
	btn.className = 'btn tiny quote-btn';
	btn.hidden = true;
	btn.title = 'Quote the selection in the composer';
	btn.innerHTML = '<svg class="i ui-icon" width="16" height="16" viewBox="0 0 24 24"><use href="#text-quote"></use></svg> <span>Quote</span>';
	document.addEventListener('DOMContentLoaded', () => document.body.append(btn));

	const source = (node) => {
		const el = node && (node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement);
		return el && el.closest('.item.assistant, .item.thinking, .item.tool, .item.result, .pr-detail-body');
	};
	let quote = '';
	const place = () => {
		const sel = document.getSelection();
		if (!sel || sel.isCollapsed || !sel.rangeCount || !document.querySelector('form.composer .prompt')) {
			btn.hidden = true;
			return;
		}
		const r = sel.getRangeAt(0);
		if (!source(r.commonAncestorContainer)) {
			btn.hidden = true;
			return;
		}
		const md = window.mdClipboard ? window.mdClipboard(sel) : null;
		quote = (md ? md.text : sel.toString()).trim();
		if (!quote) {
			btn.hidden = true;
			return;
		}
		const rect = r.getBoundingClientRect();
		btn.hidden = false;
		const w = btn.offsetWidth, h = btn.offsetHeight;
		btn.style.left = Math.round(Math.min(innerWidth - w - 8, Math.max(8, rect.left + rect.width / 2 - w / 2))) + 'px';
		// Above the selection, or below it when that is off the top.
		const top = rect.top - h - 8;
		btn.style.top = Math.round(top > 8 ? top : rect.bottom + 8) + 'px';
	};
	let frame = 0;
	document.addEventListener('selectionchange', () => {
		cancelAnimationFrame(frame);
		frame = requestAnimationFrame(place);
	});
	document.addEventListener('scroll', () => { btn.hidden = true; }, true);
	// Pressing the button must not clear the selection before the click.
	btn.addEventListener('mousedown', (e) => e.preventDefault());
	btn.addEventListener('click', () => {
		const lines = quote.split('\n').map((l) => (l ? '> ' + l : '>'));
		window.promptInsert(lines.join('\n') + '\n\n');
		document.getSelection()?.removeAllRanges();
		btn.hidden = true;
	});
})();
