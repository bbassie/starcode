// Image viewer. A click on an attached image or an image in a reply opens
// it over the page instead of in a new tab: the wheel zooms at the
// pointer, dragging pans, the buttons zoom on a phone (no pinch: pinch
// zoom stays off everywhere in starcode), and Escape, the close button or
// a click beside the image close it. The original is one click away.
(() => {
	const box = document.createElement('div');
	box.className = 'lightbox';
	box.hidden = true;
	box.innerHTML =
		'<div class="lightbox-bar">' +
		'<span class="lightbox-name ellipsis"></span><span class="spacer"></span>' +
		'<button type="button" class="btn icon ghost" data-z="out" title="Zoom out" aria-label="Zoom out"><svg class="i ui-icon" width="16" height="16" viewBox="0 0 24 24"><use href="#zoom-out"></use></svg></button>' +
		'<button type="button" class="btn ghost lightbox-pct" data-z="fit" title="Fit to the window">100%</button>' +
		'<button type="button" class="btn icon ghost" data-z="in" title="Zoom in" aria-label="Zoom in"><svg class="i ui-icon" width="16" height="16" viewBox="0 0 24 24"><use href="#zoom-in"></use></svg></button>' +
		'<a class="btn icon ghost lightbox-open" target="_blank" rel="noopener" title="Open the original" aria-label="Open the original"><svg class="i ui-icon" width="16" height="16" viewBox="0 0 24 24"><use href="#external-link"></use></svg></a>' +
		'<button type="button" class="btn icon ghost" data-z="close" title="Close (Esc)" aria-label="Close"><svg class="i ui-icon" width="16" height="16" viewBox="0 0 24 24"><use href="#x"></use></svg></button>' +
		'</div><div class="lightbox-stage"><img alt=""></div>';
	const stage = box.querySelector('.lightbox-stage');
	const img = box.querySelector('img');
	const pct = box.querySelector('.lightbox-pct');
	let scale = 1, fit = 1, x = 0, y = 0;

	const apply = () => {
		img.style.transform = `translate(${x}px, ${y}px) scale(${scale})`;
		pct.textContent = Math.round(scale * 100) + '%';
	};
	// Fit scales the image down to the stage, never up.
	const reset = () => {
		const sw = stage.clientWidth, sh = stage.clientHeight;
		fit = Math.min(1, sw / img.naturalWidth, sh / img.naturalHeight) || 1;
		scale = fit;
		x = (sw - img.naturalWidth * scale) / 2;
		y = (sh - img.naturalHeight * scale) / 2;
		apply();
	};
	// zoom to s, keeping the stage point (px, py) where it is.
	const zoom = (s, px, py) => {
		s = Math.max(fit / 4, Math.min(8, s));
		x = px - (px - x) * (s / scale);
		y = py - (py - y) * (s / scale);
		scale = s;
		apply();
	};
	const open = (src, name) => {
		box.querySelector('.lightbox-name').textContent = name || '';
		box.querySelector('.lightbox-open').href = src;
		box.hidden = false;
		img.onload = reset;
		img.src = src;
		if (img.complete && img.naturalWidth) reset();
	};
	const close = () => {
		box.hidden = true;
		img.removeAttribute('src');
	};

	document.addEventListener('DOMContentLoaded', () => document.body.append(box));
	document.addEventListener('click', (e) => {
		const t = e.target instanceof Element ? e.target : null;
		const pic = t && t.closest('img.attach-thumb, .item.assistant.md img, .pr-detail-body img');
		if (!pic || e.ctrlKey || e.metaKey || e.shiftKey) return;
		e.preventDefault();
		const link = pic.closest('a');
		open(link?.href || pic.currentSrc || pic.src, pic.alt || pic.title);
	});
	box.addEventListener('click', (e) => {
		const b = e.target instanceof Element && e.target.closest('[data-z]');
		const mid = () => [stage.clientWidth / 2, stage.clientHeight / 2];
		if (b) {
			if (b.dataset.z === 'close') close();
			else if (b.dataset.z === 'fit') reset();
			else zoom(scale * (b.dataset.z === 'in' ? 1.5 : 1 / 1.5), ...mid());
			return;
		}
		if (e.target === stage) close();
	});
	document.addEventListener('keydown', (e) => {
		if (box.hidden) return;
		if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); }
		else if (e.key === '+' || e.key === '=') zoom(scale * 1.25, stage.clientWidth / 2, stage.clientHeight / 2);
		else if (e.key === '-') zoom(scale / 1.25, stage.clientWidth / 2, stage.clientHeight / 2);
		else if (e.key === '0') reset();
	}, true);
	stage.addEventListener('wheel', (e) => {
		e.preventDefault();
		const r = stage.getBoundingClientRect();
		zoom(scale * Math.exp(-e.deltaY * 0.0015), e.clientX - r.left, e.clientY - r.top);
	}, { passive: false });
	// Drag to pan, with a mouse or one finger.
	let drag = null;
	img.addEventListener('pointerdown', (e) => {
		e.preventDefault();
		drag = { px: e.clientX, py: e.clientY, x, y };
		img.setPointerCapture(e.pointerId);
		img.classList.add('dragging');
	});
	img.addEventListener('pointermove', (e) => {
		if (!drag) return;
		x = drag.x + e.clientX - drag.px;
		y = drag.y + e.clientY - drag.py;
		apply();
	});
	const end = () => { drag = null; img.classList.remove('dragging'); };
	img.addEventListener('pointerup', end);
	img.addEventListener('pointercancel', end);
	img.addEventListener('dblclick', (e) => {
		const r = stage.getBoundingClientRect();
		scale > fit * 1.01 ? reset() : zoom(Math.max(1, fit * 2), e.clientX - r.left, e.clientY - r.top);
	});
	addEventListener('resize', () => { if (!box.hidden) reset(); });
})();
