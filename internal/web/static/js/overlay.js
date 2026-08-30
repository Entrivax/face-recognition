/* recogn — shared face overlay renderer.
	 One renderer for every surface that shows detected faces: the main stage,
	 the photos-manager detail view, and the enroll face-check viewer. Draws
	 scaled corner brackets per face; labels (identity + confidence) are
	 optional for small thumbnails. */

export function drawFaces(canvas, img, faces, opts = {}) {
	const w = img.naturalWidth;
	const h = img.naturalHeight;
	if (!w || !h) return;
	canvas.width = w;
	canvas.height = h;
	const ctx = canvas.getContext("2d");
	ctx.clearRect(0, 0, w, h);
	const scale = Math.max(w, h) / 900; // line width scales with image size

	faces.forEach((f) => {
		const [x, y, bw, bh] = f.bbox;
		const known = f.name && f.name !== "unknown";
		const col = known ? "#38e0c8" : "#f5b53f";
		const L = Math.max(14 * scale, Math.min(bw, bh) * 0.22); // bracket arm
		const lw = Math.max(2, 2.5 * scale);
		ctx.strokeStyle = col;
		ctx.lineWidth = lw;
		ctx.shadowColor = col;
		ctx.shadowBlur = 6 * scale;

		// corner brackets
		const corners = [
			[x, y, 1, 1], [x + bw, y, -1, 1],
			[x, y + bh, 1, -1], [x + bw, y + bh, -1, -1],
		];
		corners.forEach(([cx, cy, sx, sy]) => {
			ctx.beginPath();
			ctx.moveTo(cx + L * sx, cy);
			ctx.lineTo(cx, cy);
			ctx.lineTo(cx, cy + L * sy);
			ctx.stroke();
		});

		if (!opts || !opts.labels) return;

		// label
		ctx.shadowBlur = 0;
		const conf = Math.round((f.confidence || f.score || 0) * 100);
		const label = known ? `${f.name} ${conf}%` : `unknown ${conf}%`;
		const fs = Math.max(12, 15 * scale);
		ctx.font = `600 ${fs}px "Space Grotesk", sans-serif`;
		const tw = ctx.measureText(label).width;
		const pad = 6 * scale;
		const bx = x;
		const by = y - fs - pad * 2 < 0 ? y + bh : y - fs - pad * 2; // above, else below
		ctx.fillStyle = "rgba(11,14,18,0.85)";
		ctx.fillRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
		ctx.strokeStyle = col;
		ctx.lineWidth = 1;
		ctx.strokeRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
		ctx.fillStyle = col;
		ctx.fillText(label, bx + pad, by + fs + pad - 2 * scale);
	});
}

// Draw once the image has dimensions (immediately when already loaded).
export function drawWhenReady(canvas, img, faces, opts) {
	const draw = () => drawFaces(canvas, img, faces, opts);
	if (img.complete && img.naturalWidth) draw();
	else img.onload = draw;
}
