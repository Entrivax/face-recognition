// ==UserScript==
// @name         recogn — hover capture
// @namespace    recogn.hover-capture
// @version      1.3.1
// @description  Press Alt+R for full-screen capture mode: click any image or video to send its current frame to your local recogn server; recognised faces are drawn over the element until you clear them.
// @author       recogn
// @license      MIT
// @match        *://*/*
// @match        file:///*
// @grant        GM_xmlhttpRequest
// @grant        GM.xmlHttpRequest
// @grant        GM_registerMenuCommand
// @grant        GM.registerMenuCommand
// @grant        GM.getValue
// @grant        GM.setValue
// @grant        GM_getValue
// @grant        GM_setValue
// @connect      127.0.0.1
// @connect      localhost
// @run-at       document-idle
// ==/UserScript==

/* recogn — capture-mode userscript (one-shot; not part of the served UI).
 *
 * Install: Tampermonkey / Violentmonkey → new userscript → paste this file.
 *
 * Use: press Alt+R to toggle full-screen capture mode. The shield blocks all
 * clicks to the page; clicking it scans whatever image/video is under the
 * click: the current frame is drawn to a canvas and POSTed to your local
 * recogn server (POST /api/recognize, JPEG body), and every recognised face
 * is drawn over that element with the web UI's corner-bracket style. The
 * shield stays up for scanning several media in a row; clicking an
 * already-annotated one re-scans it in place (e.g. a fresh video frame).
 * While a scan is in flight a pixel shimmer (ported from the repo's
 * shimmer-animation.html demo) blooms over the media being scanned and
 * shrinks away when the pill resolves; prefers-reduced-motion skips it.
 *
 * Results stay on the page after you leave capture mode until you clear them:
 *   • hover a status pill to expand the detected-people list beneath it
 *   • ✕ on a status pill removes that overlay
 *   • Escape removes all overlays and exits capture mode
 *   • userscript menu: "recogn: set server URL…", "recogn: clear all
 *     overlays", "recogn: toggle capture mode (Alt+R)"
 *
 * The recogn API sets no CORS headers, so pages on other origins need the GM
 * transport (GM_xmlhttpRequest); same-origin pages (e.g. the recogn UI itself
 * at http://127.0.0.1:8080) fall back to plain fetch. Pointing CONFIG.server
 * at a non-local host requires adding an @connect line for it above.
 *
 * While capture mode is up the page receives no clicks (native video
 * controls, links, … are blocked) — press Alt+R again to interact with the
 * page. Keyboard stays live so the toggle always works. Frames are enabled
 * (no @noframes) so media inside iframes works when the frame has focus.
 *
 * Manual test pass:
 *   1. `make serve`, then Alt+R on http://127.0.0.1:8080 (fetch path) and on
 *      any external site with photos/videos (GM path).
 *   2. Shield blocks page clicks; clicking an image/video draws brackets +
 *      a status pill above the shield; several media keep their own results.
 *   3. Clicking a playing video's overlay re-scans: boxes snap to the new
 *      frame; playing and paused frames both capture.
 *   4. Hover a pill: the detected-people list slides open below the header
 *      and collapses on leave; it refreshes after a re-scan.
 *   5. object-fit: cover images: boxes track the visible crop, honouring
 *      object-position (e.g. a top-anchored 2/3 card).
 *   6. Scroll/resize while results are up: they follow their element.
 *   7. Alt+R exits the mode but keeps results; Escape/✕ clear them.
 *   8. Server stopped: a red pill/toast names the configured URL.
 *   9. While a scan runs, a pixel shimmer blooms over the media and shrinks
 *      away when the pill resolves (result or error); re-clicking restarts
 *      it; squares fade with proximity to the centre; prefers-reduced-motion
 *      skips it.
 */

(function () {
	"use strict";

	// Editable defaults. The server URL can also be changed at runtime via the
	// userscript menu command (persisted with GM.setValue per manager install).
	const CONFIG = {
		server: "http://127.0.0.1:8080",
		hotkey: { key: "r", alt: true, ctrl: false, shift: false, meta: false },
		maxEdge: 1600, // cap the capture's long edge before upload (the engine letterboxes to 640 anyway)
		jpegQuality: 0.92,
		requestTimeoutMs: 30000,
	};

	// Palette and type mirror the web UI (internal/web/static/style.css) so the
	// HUD reads as recogn projected onto any page.
	const COL = {
		known: "#38e0c8",
		unknown: "#f5b53f",
		error: "#f2607a",
		labelBg: "rgba(11,14,18,0.85)",
	};

	const CSS = `
		.rcg-shield {
			position: fixed; inset: 0; z-index: 2147483646;
			background: rgba(11,14,18,0.18);
			cursor: crosshair; user-select: none; pointer-events: auto;
		}
		.rcg-shield-hint {
			position: fixed; top: 14px; left: 50%; transform: translateX(-50%);
			max-width: min(680px, 90vw); padding: 5px 11px; border-radius: 8px;
			background: rgba(11,14,18,0.88); border: 1px solid #232a35; color: #5b8cff;
			font: 500 11.5px/1.45 "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
			white-space: nowrap; overflow: hidden; text-overflow: ellipsis; pointer-events: none;
		}
		.rcg-hud { position: fixed; z-index: 2147483647; pointer-events: none; }
		.rcg-boxes { position: absolute; pointer-events: none; }
		.rcg-shimmer { position: absolute; pointer-events: none; }
		.rcg-pill {
			position: absolute; bottom: calc(100% + 6px); left: 6px; display: inline-flex; flex-direction: column;
			max-width: calc(100% - 12px); padding: 3px 8px; border-radius: 6px;
			background: rgba(11,14,18,0.88); border: 1px solid #232a35; color: #e8edf4;
			font: 500 11px/1.4 "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
			pointer-events: auto; /* hover target; body clicks forward to a rescan */
		}
		.rcg-pill-row { display: inline-flex; align-items: center; gap: 7px; min-width: 0; }
		.rcg-pill .rcg-txt { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
		/* person list — grid 0fr/1fr + overflow hidden for a smooth reveal */
		.rcg-people {
			display: grid; grid-template-rows: 0fr;
			transition: grid-template-rows 0.18s ease 0.12s; /* base delay = graceful close */
		}
		.rcg-pill:hover .rcg-people { grid-template-rows: 1fr; transition-delay: 0s; }
		.rcg-people-inner { overflow: hidden; min-height: 0; } /* no padding here — it would break the 0fr collapse */
		.rcg-person {
			display: flex; align-items: baseline; gap: 8px; margin-top: 3px;
			font-size: 10.5px; line-height: 1.5; white-space: nowrap;
		}
		.rcg-person .rcg-pname { min-width: 0; overflow: hidden; text-overflow: ellipsis; }
		.rcg-person .rcg-pconf { flex: none; margin-left: auto; font-size: 10px; opacity: 0.75; }
		.rcg-person.known { color: #38e0c8; }
		.rcg-person.unknown { color: #f5b53f; }
		.rcg-person.none { color: #96a1b0; }
		.rcg-pill.ok     { border-color: rgba(56,224,200,0.55); color: #38e0c8; }
		.rcg-pill.warn   { border-color: rgba(245,181,63,0.55); color: #f5b53f; }
		.rcg-pill.err    { border-color: rgba(242,96,122,0.65); color: #f2607a; }
		.rcg-pill.pending{ border-color: rgba(91,140,255,0.55); color: #5b8cff; }
		.rcg-x {
			all: unset; pointer-events: auto; cursor: pointer; margin-left: 2px; padding: 0 2px;
			font: inherit; line-height: 1; color: #96a1b0;
		}
		.rcg-x:hover { color: #f2607a; }
		.rcg-x:focus-visible { outline: 1px solid #5b8cff; outline-offset: 1px; }
		.rcg-spin {
			flex: none; width: 9px; height: 9px; border-radius: 50%;
			border: 1.5px solid rgba(91,140,255,0.3); border-top-color: #5b8cff;
			animation: rcg-rot 0.7s linear infinite;
		}
		@keyframes rcg-rot { to { transform: rotate(360deg); } }
		.rcg-toasts {
			position: fixed; left: 50%; bottom: 18px; transform: translateX(-50%); z-index: 2147483647;
			display: flex; flex-direction: column; gap: 6px; align-items: center; pointer-events: none;
		}
		.rcg-toast {
			max-width: min(560px, 82vw); padding: 7px 12px; border-radius: 8px;
			background: rgba(11,14,18,0.92); border: 1px solid #232a35; border-left: 3px solid #5b8cff;
			color: #e8edf4; font: 500 11.5px/1.45 "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
			box-shadow: 0 6px 24px rgba(0,0,0,0.35); animation: rcg-in 0.18s ease-out;
		}
		.rcg-toast.err { border-left-color: #f2607a; }
		.rcg-toast.out { opacity: 0; transition: opacity 0.25s; }
		@keyframes rcg-in { from { opacity: 0; transform: translateY(6px); } }
		@media (prefers-reduced-motion: reduce) {
			.rcg-spin { animation: none; }
			.rcg-toast { animation: none; }
			.rcg-people { transition: none; }
		}
	`;

	let server = CONFIG.server;
	let shield = null; // full-screen capture-mode layer, when active
	const overlays = new Map(); // captured element → HUD state

	// ---- GM plumbing (each helper degrades gracefully without the API) ----

	function gmHttp() {
		if (typeof GM !== "undefined" && GM && typeof GM.xmlHttpRequest === "function") return GM.xmlHttpRequest.bind(GM);
		if (typeof GM_xmlhttpRequest === "function") return GM_xmlhttpRequest;
		return null;
	}

	function gmSend(opts) {
		const gm = gmHttp();
		if (!gm) return Promise.reject(new Error("no GM transport"));
		return new Promise((resolve, reject) => {
			try {
				gm(Object.assign({}, opts, {
					onload: resolve,
					onerror: (res) => reject(new Error((res && (res.error || res.statusText)) || "network error")),
					ontimeout: () => reject(new Error("timed out after " + CONFIG.requestTimeoutMs + "ms")),
				}));
			} catch (err) { reject(err); }
		});
	}

	function gmGet(key) {
		try {
			if (typeof GM !== "undefined" && GM && typeof GM.getValue === "function") return Promise.resolve(GM.getValue(key));
		} catch (_) { /* fall through */ }
		try {
			if (typeof GM_getValue === "function") return Promise.resolve(GM_getValue(key));
		} catch (_) { /* fall through */ }
		return Promise.resolve(undefined);
	}

	function gmSet(key, value) {
		try {
			if (typeof GM !== "undefined" && GM && typeof GM.setValue === "function") return Promise.resolve(GM.setValue(key, value));
		} catch (_) { /* fall through */ }
		try {
			if (typeof GM_setValue === "function") return GM_setValue(key, value);
		} catch (_) { /* fall through */ }
		return Promise.resolve();
	}

	// ---- toasts ----

	function toast(msg, kind) {
		let wrap = document.querySelector(".rcg-toasts");
		if (!wrap) {
			wrap = document.createElement("div");
			wrap.className = "rcg-toasts";
			document.documentElement.appendChild(wrap);
		}
		while (wrap.children.length >= 4) wrap.firstChild.remove();
		const t = document.createElement("div");
		t.className = "rcg-toast" + (kind === "error" ? " err" : "");
		t.textContent = msg;
		wrap.appendChild(t);
		setTimeout(() => {
			t.classList.add("out");
			setTimeout(() => t.remove(), 300);
		}, 3800);
	}

	// ---- capture-mode shield ----

	function showShield() {
		if (shield) return;
		shield = document.createElement("div");
		shield.className = "rcg-shield";
		const hint = document.createElement("div");
		hint.className = "rcg-shield-hint";
		hint.textContent = "recogn capture mode — click an image or video to scan · Alt+R to exit";
		shield.appendChild(hint);
		shield.addEventListener("mousedown", (e) => {
			if (e.button !== 0) return; // no scan on right/middle click
			captureAt(e.clientX, e.clientY);
			e.preventDefault();
			e.stopPropagation();
		}, { capture: true });
		document.documentElement.appendChild(shield);
	}

	function hideShield() {
		if (!shield) return;
		shield.remove();
		shield = null;
	}

	function toggleShield() {
		if (shield) hideShield();
		else showShield();
	}

	function hotkeyMatches(e) {
		const hk = CONFIG.hotkey;
		const key = String(e.key || "").toLowerCase();
		const keyHit = key === hk.key.toLowerCase() || String(e.code || "") === "Key" + hk.key.toUpperCase();
		return keyHit && e.altKey === !!hk.alt && e.ctrlKey === !!hk.ctrl
			&& e.shiftKey === !!hk.shift && e.metaKey === !!hk.meta;
	}

	function isEditable(t) {
		return !!(t && t.closest && t.closest("input, textarea, select, [contenteditable]"));
	}

	function onKeyDown(e) {
		if (e.key === "Escape") {
			// Full reset: leave capture mode and clear every result overlay.
			if ((overlays.size || shield) && !isEditable(e.target)) {
				e.preventDefault();
				e.stopPropagation();
				clearAllOverlays();
				hideShield();
			}
			return;
		}
		if (e.repeat || !hotkeyMatches(e) || isEditable(e.target)) return;
		e.preventDefault();
		e.stopPropagation();
		toggleShield();
	}

	// ---- target lookup ----

	// elementsFromPoint lists everything under the point, topmost first; walk
	// past the shield, decorations and our own HUD to the first real IMG/VIDEO.
	function findTarget(x, y) {
		if (x < 0 || y < 0) return null;
		const stack = document.elementsFromPoint(x, y) || [];
		for (const el of stack) {
			if (!el || el.closest(".rcg-hud, .rcg-shield, .rcg-toasts")) continue;
			if (el.tagName === "IMG") {
				if (el.complete && el.naturalWidth > 0 && el.clientWidth > 0) return el;
			} else if (el.tagName === "VIDEO") {
				if (el.readyState >= 2 && el.videoWidth > 0 && el.clientWidth > 0) return el;
			}
		}
		return null;
	}

	// ---- capture ----

	function canvasToBlob(canvas) {
		return new Promise((resolve, reject) => {
			try {
				canvas.toBlob((b) => (b ? resolve(b) : reject(new Error("the frame could not be encoded"))),
					"image/jpeg", CONFIG.jpegQuality);
			} catch (err) { reject(err); } // SecurityError: tainted canvas
		});
	}

	// Tainted-canvas fallback for images: refetch the source through the GM
	// transport (bypasses CORS) and redraw. Video frames have no fallback.
	async function recaptureImage(img, cw, ch) {
		const src = img.currentSrc || img.src || "";
		if (!gmHttp()) throw new Error("the image is cross-origin protected; a manager with GM_xmlhttpRequest is needed to fetch it");
		if (!/^https?:/i.test(src)) throw new Error("the image is cross-origin protected and its URL cannot be re-fetched");
		const res = await gmSend({ method: "GET", url: src, responseType: "arraybuffer", timeout: CONFIG.requestTimeoutMs });
		if (res.status !== 200 || !res.response) throw new Error("re-fetching the image returned HTTP " + res.status);
		const bmp = await createImageBitmap(new Blob([res.response]));
		const c = document.createElement("canvas");
		c.width = cw;
		c.height = ch;
		c.getContext("2d").drawImage(bmp, 0, 0, cw, ch);
		if (bmp.close) bmp.close();
		return canvasToBlob(c);
	}

	async function captureMedia(target) {
		const isVideo = target.tagName === "VIDEO";
		const sw = isVideo ? target.videoWidth : target.naturalWidth;
		const sh = isVideo ? target.videoHeight : target.naturalHeight;
		if (!sw || !sh) throw new Error(isVideo ? "the video has no drawable frame yet" : "the image has no pixels yet");
		const scale = Math.min(1, CONFIG.maxEdge / Math.max(sw, sh));
		const cw = Math.max(1, Math.round(sw * scale));
		const ch = Math.max(1, Math.round(sh * scale));
		const draw = (source) => {
			const c = document.createElement("canvas");
			c.width = cw;
			c.height = ch;
			c.getContext("2d").drawImage(source, 0, 0, cw, ch);
			return c;
		};
		let blob;
		try {
			blob = await canvasToBlob(draw(target));
		} catch (err) {
			if (!isVideo) blob = await recaptureImage(target, cw, ch);
			else throw new Error("this video's frame is cross-origin protected and cannot be captured");
		}
		return { blob, cw, ch, sw, sh }; // full frame; the HUD maps boxes onto the element's visible region itself
	}

	// ---- recogn API ----

	function readRecognResponse(status, text) {
		let j = null;
		try { j = JSON.parse(text); } catch (_) { /* non-JSON error body */ }
		if (status !== 200) throw new Error((j && j.error) || "recogn returned HTTP " + status);
		if (!j || !Array.isArray(j.faces)) throw new Error("recogn returned an unexpected response");
		return j;
	}

	// POST the JPEG bytes directly (the server treats any non-multipart body
	// as the raw image) — more robust across managers than FormData over GM.
	async function recognize(blob) {
		const url = String(server).replace(/\/+$/, "") + "/api/recognize";
		if (gmHttp()) {
			try {
				const res = await gmSend({
					method: "POST",
					url,
					headers: { "Content-Type": "image/jpeg" },
					data: blob,
					timeout: CONFIG.requestTimeoutMs,
				});
				return readRecognResponse(res.status, res.responseText);
			} catch (err) { /* network-level failure: fall through to fetch */ }
		}
		try {
			const r = await fetch(url, { method: "POST", headers: { "Content-Type": "image/jpeg" }, body: blob });
			return readRecognResponse(r.status, await r.text());
		} catch (err) {
			throw new Error("cannot reach recogn at " + url + " — is the server running (make serve)?"
				+ " On cross-origin pages the manager must grant GM_xmlhttpRequest (see @connect).");
		}
	}

	// ---- geometry: where does the media actually paint? ----

	// object-position keywords are axis-bound: left/right position X, top/bottom
	// position Y, center fits either. Computed styles hand us percentages
	// ("50% 0%"); keyword parsing stays for raw/older values.
	const POS_KEYWORDS = {
		left: { axis: "x", pct: 0 },
		right: { axis: "x", pct: 1 },
		top: { axis: "y", pct: 0 },
		bottom: { axis: "y", pct: 1 },
		center: { pct: 0.5 },
	};

	function posToken(v) {
		const kw = POS_KEYWORDS[String(v || "").toLowerCase()];
		if (kw) return { axis: kw.axis, pct: kw.pct };
		if (v.endsWith("%")) return { pct: Math.min(1, Math.max(0, parseFloat(v) / 100)) };
		if (v.endsWith("px")) return { px: parseFloat(v) || 0 };
		return { pct: 0.5 };
	}

	function isYToken(token) { return token.axis === "y"; }

	function parseObjectPosition(value) {
		const parts = String(value || "").trim().split(/\s+/).filter(Boolean);
		if (parts.length > 2) parts.length = 2; // four-value syntax degrades to its first pair
		const a = posToken(parts[0]);
		const b = parts.length === 2 ? posToken(parts[1]) : { pct: 0.5 };
		// A lone vertical keyword pins Y ("top" = "center top"); with two tokens
		// a leading vertical keyword takes the Y slot regardless of order
		// ("top 20%" = x 20%, y top).
		if (isYToken(a)) return { x: isYToken(b) ? { pct: 0.5 } : b, y: a };
		return { x: a, y: b };
	}

	// Portion of the leftover space a token claims. Leftover is negative when
	// the content overflows the box (object-fit: cover), so 50% must resolve to
	// a negative offset (centered overflow) — no clamping here, matching
	// background-position semantics.
	function posOffset(token, leftover) {
		if (token.px !== undefined) return token.px;
		return token.pct * leftover;
	}

	// The server's boxes are in the full frame's pixel space, but object-fit
	// means the element box is often not the painted area — and object-position
	// may push the painted rect past the box (negative offsets under cover).
	// Compute both the painted content rect and the visible (clipped) part, in
	// viewport coords, from the computed style. Axis-aligned elements only —
	// transformed (rotated/skewed) boxes are a known limitation.
	function contentRect(el, sw, sh) {
		const box = el.getBoundingClientRect();
		const cs = getComputedStyle(el);
		const fit = cs.objectFit || "fill";
		const ar = sw / sh;
		const boxAr = box.width / Math.max(1, box.height);
		let w; let h;
		switch (fit) {
			case "contain":
				if (ar >= boxAr) { w = box.width; h = w / ar; } else { h = box.height; w = h * ar; }
				break;
			case "cover":
				if (ar >= boxAr) { h = box.height; w = h * ar; } else { w = box.width; h = w / ar; }
				break;
			case "none":
				w = sw; h = sh;
				break;
			case "scale-down": {
				const cw2 = ar >= boxAr ? box.width : box.height * ar;
				if (sw <= cw2) { w = sw; h = sh; } else { w = cw2; h = cw2 / ar; }
				break;
			}
			default: // fill
				w = box.width; h = box.height;
		}
		const pos = parseObjectPosition(cs.objectPosition);
		const left = box.left + posOffset(pos.x, box.width - w);
		const top = box.top + posOffset(pos.y, box.height - h);
		// The element clips the content to its box: intersect the painted rect
		// with the box to get the on-screen region (cover overflows it, contain
		// letterboxes inside it). Nothing paints when the intersection is empty.
		const visLeft = Math.max(left, box.left);
		const visTop = Math.max(top, box.top);
		const vw = Math.min(left + w, box.right) - visLeft;
		const vh = Math.min(top + h, box.bottom) - visTop;
		if (!(w > 0) || !(h > 0) || !(vw > 0) || !(vh > 0)) return null;
		return {
			left, top, width: w, height: h, // painted rect (may exceed the box)
			vis: { left: visLeft, top: visTop, width: vw, height: vh }, // on-screen
		};
	}

	// ---- pixel shimmer (port of shimmer-animation.html's <pixel-canvas>) ----
	// A field of tiny squares over the media a scan is running on: pixels bloom
	// outward from the centre (delay = distance to centre), pulse at full size
	// and shrink away when the request settles. Opacity falls off linearly
	// toward the centre (SHIMMER.centerDim), so the bloom reads as a soft glow
	// with a quiet middle. Skipped entirely under
	// prefers-reduced-motion. The canvas covers the visible content rect — the
	// same one the face-box canvas gets — so object-fit crops shimmer exactly
	// where the image paints. A standalone factory instead of the demo's custom
	// element (no shadow DOM / hover events), plus an adaptive gap that keeps
	// the grid ≤ ~3000 pixels on large media.

	const SHIMMER = {
		colors: ["#5b8cff", "#49c0f7", "#8bd1ff"], // pending blue + sky tones
		speed: 200, // demo default; a pixel's pulse step is this × 0.001 × rand(0.1, 0.9)
		maxPixels: 6000, // density cap; the gap adapts to honour it
		centerDim: 0.95, // how much opacity drops at the very centre (0 = off, 1 = invisible)
	};

	function createShimmer() {
		const el = document.createElement("canvas");
		el.className = "rcg-shimmer";
		const ctx = el.getContext("2d");
		const reduced = !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
		const speed = reduced ? 0 : SHIMMER.speed * 0.001;
		let pixels = [];
		let cssW = 0;
		let cssH = 0;
		let raf = 0;
		let prev = 0;

		const rand = (min, max) => Math.random() * (max - min) + min;

		// Rebuild the grid for the current size (the demo re-inits via its
		// ResizeObserver on resize, restarting the bloom — mirrored here).
		function makePixels() {
			pixels = [];
			if (!cssW || !cssH) return;
			const gap = Math.min(12, Math.max(6, Math.ceil(Math.sqrt((cssW * cssH) / SHIMMER.maxPixels))));
			const size = Math.min(6, Math.max(3, gap / 2))
			const maxDist = Math.hypot(cssW, cssH) / 2; // centre → farthest corner
			for (let x = gap / 2; x < cssW; x += gap) {
				for (let y = gap / 2; y < cssH; y += gap) {
					const dx = x - cssW / 2;
					const dy = y - cssH / 2;
					const dist = Math.sqrt(dx * dx + dy * dy) * 0.5;
					pixels.push({
						x, y,
						color: SHIMMER.colors[Math.floor(Math.random() * SHIMMER.colors.length)],
						speed: rand(0.1, 0.9) * speed,
						size: 0,
						sizeStep: Math.random() * 2.5,
						minSize: size,
						maxSize: rand(size, size + 1),
						delay: reduced ? 0 : dist,
						counter: 0,
						counterStep: Math.random() * 4 + (cssW + cssH) * 0.01,
						// radial opacity falloff: squares fade gradually toward the centre
						alpha: 1 - SHIMMER.centerDim * (1 - Math.min(1, Math.pow(2 * dist / maxDist, 2))),
						isReverse: false,
						isShimmer: false,
						isIdle: false,
					});
				}
			}
		}

		function draw(p) {
			const off = 1 - p.size * 0.5; // centre the square on (x, y)
			ctx.globalAlpha = p.alpha; // multiplies any alpha baked into the hex colour
			ctx.fillStyle = p.color;
			ctx.fillRect(p.x + off, p.y + off, p.size, p.size);
		}

		// The demo's Pixel.appear/disappear/shimmer inlined: appear=true grows
		// and then pulses, false shrinks to idle. Per-pixel state survives mode
		// switches, so interrupting a bloom shrinks what has appeared so far.
		function step(p, appear) {
			if (appear) {
				p.isIdle = false;
				if (p.counter <= p.delay) {
					p.counter += p.counterStep;
					return;
				}
				if (p.size >= p.maxSize) p.isShimmer = true;
				if (p.isShimmer) {
					if (p.size >= p.maxSize) p.isReverse = true;
					else if (p.size <= p.minSize) p.isReverse = false;
					p.size += p.isReverse ? -p.speed : p.speed;
				} else {
					p.size += p.sizeStep;
				}
			} else {
				p.isShimmer = false;
				p.counter = 0;
				if (p.size <= 0) {
					p.isIdle = true;
					return;
				}
				p.size -= 0.5;
			}
			if (p.size > 0) draw(p);
		}

		function loop(appear) {
			raf = requestAnimationFrame(() => loop(appear));
			const now = performance.now();
			if (now - prev < 1000 / 60) return; // throttle on high-refresh displays
			prev = now;
			ctx.clearRect(0, 0, cssW, cssH);
			let allIdle = true;
			for (const p of pixels) {
				step(p, appear);
				if (!p.isIdle) allIdle = false;
			}
			if (allIdle) {
				cancelAnimationFrame(raf);
				raf = 0;
				ctx.clearRect(0, 0, cssW, cssH);
			}
		}

		function start(appear) {
			if (reduced) return;
			if (raf) cancelAnimationFrame(raf);
			prev = 0; // let the first frame run immediately
			loop(appear);
		}

		// Ticker feed: keep the canvas on the content rect and the grid in step
		// with the displayed size.
		function sync(w, h) {
			w = Math.max(0, Math.round(w));
			h = Math.max(0, Math.round(h));
			if (w === cssW && h === cssH) return;
			cssW = w;
			cssH = h;
			const dpr = Math.min(2, window.devicePixelRatio || 1);
			el.width = Math.max(1, Math.round(w * dpr));
			el.height = Math.max(1, Math.round(h * dpr));
			ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
			makePixels();
		}

		return {
			el,
			sync,
			appear: () => start(true),
			disappear: () => start(false),
			destroy: () => {
				if (raf) cancelAnimationFrame(raf);
				raf = 0;
				pixels = [];
			},
		};
	}

	// ---- overlay HUD ----

	function setPill(s, text, kind, spinning) {
		s.txt.textContent = text;
		s.pill.className = "rcg-pill " + kind;
		let spin = s.row.querySelector(".rcg-spin");
		if (spinning && !spin) {
			spin = document.createElement("span");
			spin.className = "rcg-spin";
			s.row.prepend(spin);
		} else if (!spinning && spin) {
			spin.remove();
		}
	}

	// Hover-expand person list: one row per detected face, in detection order
	// (same numbering/data as the canvas labels). Pending clears the list,
	// 0 faces gets a dim note, otherwise name + confidence per person.
	function renderPeople(s) {
		s.list.textContent = "";
		const faces = s.faces;
		if (!faces) return;
		if (!faces.length) {
			const none = document.createElement("div");
			none.className = "rcg-person none";
			none.textContent = "no faces detected";
			s.list.appendChild(none);
			return;
		}
		for (let i = 0; i < faces.length; i++) {
			const f = faces[i] || {};
			const known = f.name && f.name !== "unknown";
			const conf = Math.round((f.confidence || f.score || 0) * 100);
			const row = document.createElement("div");
			row.className = "rcg-person " + (known ? "known" : "unknown");
			const name = document.createElement("span");
			name.className = "rcg-pname";
			name.textContent = String(i + 1).padStart(2, "0") + " · " + (known ? f.name : "unknown");
			const pc = document.createElement("span");
			pc.className = "rcg-pconf";
			pc.textContent = conf + "%";
			row.append(name, pc);
			s.list.appendChild(row);
		}
	}

	function removeHud(s) {
		if (!s || s.removed) return;
		s.removed = true;
		s.shimmer.destroy();
		s.el.remove();
		overlays.delete(s.target);
	}

	function clearAllOverlays() {
		for (const s of Array.from(overlays.values())) removeHud(s);
	}

	// Corner brackets + labels — port of internal/web/static/js/overlay.js.
	// Box coords are capture-canvas pixels, i.e. the full painted rect that the
	// element may crop. The paint-rect scale (kx/ky) plus the visible-origin
	// offset (ox/oy) map them onto the overlay canvas, which covers only the
	// on-screen rect (× devicePixelRatio so text and line widths stay
	// readable); the canvas clips whatever the element crops away.
	function renderFaces(s) {
		const c = s.canvas;
		const cr = s.cr;
		if (!cr) return;
		const vis = cr.vis;
		const dpr = Math.min(2, window.devicePixelRatio || 1);
		const pw = Math.max(1, Math.round(vis.width * dpr));
		const ph = Math.max(1, Math.round(vis.height * dpr));
		if (c.width !== pw) c.width = pw;
		if (c.height !== ph) c.height = ph;
		const ctx = c.getContext("2d");
		ctx.clearRect(0, 0, c.width, c.height);
		const faces = s.faces;
		if (!faces || !faces.length) return;
		// capture px → device px by the painted rect, then shift by the visible
		// origin (negative under cover) so boxes land on the painted pixels
		// that are actually on screen.
		const kx = cr.width / s.capture.cw * dpr;
		const ky = cr.height / s.capture.ch * dpr;
		const ox = (cr.left - vis.left) * dpr;
		const oy = (cr.top - vis.top) * dpr;
		const scale = Math.max(c.width, c.height) / 900;

		for (let i = 0; i < faces.length; i++) {
			const f = faces[i] || {};
			const b = f.bbox || [0, 0, 0, 0];
			const x = ox + b[0] * kx;
			const y = oy + b[1] * ky;
			const bw = Math.max(2, b[2] * kx);
			const bh = Math.max(2, b[3] * ky);
			const known = f.name && f.name !== "unknown";
			const col = known ? COL.known : COL.unknown;
			const L = Math.max(14 * scale, Math.min(bw, bh) * 0.22);
			const lw = Math.max(2, 2.5 * scale);
			ctx.strokeStyle = col;
			ctx.lineWidth = lw;
			ctx.shadowColor = col;
			ctx.shadowBlur = 6 * scale;

			const corners = [
				[x, y, 1, 1], [x + bw, y, -1, 1],
				[x, y + bh, 1, -1], [x + bw, y + bh, -1, -1],
			];
			for (const [cx, cy, sx, sy] of corners) {
				ctx.beginPath();
				ctx.moveTo(cx + L * sx, cy);
				ctx.lineTo(cx, cy);
				ctx.lineTo(cx, cy + L * sy);
				ctx.stroke();
			}

			// label — numbered by detection order, above the box else below
			ctx.shadowBlur = 0;
			const conf = Math.round((f.confidence || f.score || 0) * 100);
			const who = known ? f.name + " " + conf + "%" : "unknown " + conf + "%";
			const label = String(i + 1).padStart(2, "0") + " · " + who;
			const fs = Math.max(12, 15 * scale);
			ctx.font = "600 " + fs + 'px "Space Grotesk", system-ui, sans-serif';
			const tw = ctx.measureText(label).width;
			const pad = 6 * scale;
			const bx = x;
			const by = y - fs - pad * 2 < 0 ? y + bh : y - fs - pad * 2;
			ctx.fillStyle = COL.labelBg;
			ctx.fillRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
			ctx.strokeStyle = col;
			ctx.lineWidth = 1;
			ctx.strokeRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
			ctx.fillStyle = col;
			ctx.fillText(label, bx + pad, by + fs + pad - 2 * scale);
		}
	}

	// Reposition every frame so the HUD tracks scroll, resize, layout shifts
	// and element resizing until dismissed. Cheap: one rect + one style read
	// per HUD, and the canvas only redraws its few brackets.
	function positionHud(s) {
		if (!s.target.isConnected) { removeHud(s); return; }
		const box = s.target.getBoundingClientRect();
		if (box.width < 2 && box.height < 2) { removeHud(s); return; }
		const cr = contentRect(s.target, s.capture.sw, s.capture.sh);
		s.cr = cr;
		const st = s.el.style;
		st.left = box.left + "px";
		st.top = box.top + "px";
		st.width = box.width + "px";
		st.height = box.height + "px";
		const cs = s.canvas.style;
		const ss = s.shimmer.el.style;
		if (!cr) { // nothing paints inside the box right now — hide the canvases
			cs.width = ss.width = "0px";
			cs.height = ss.height = "0px";
			s.shimmer.sync(0, 0);
			return;
		}
		const vis = cr.vis; // the element clips the painted area to this
		cs.left = (vis.left - box.left) + "px";
		cs.top = (vis.top - box.top) + "px";
		cs.width = vis.width + "px";
		cs.height = vis.height + "px";
		ss.left = cs.left; // shimmer covers the same visible rect
		ss.top = cs.top;
		ss.width = cs.width;
		ss.height = cs.height;
		s.shimmer.sync(vis.width, vis.height);
		renderFaces(s);
	}

	let ticking = false;
	function startTicker() {
		if (ticking) return;
		ticking = true;
		requestAnimationFrame(tick);
	}
	function tick() {
		if (!overlays.size) { ticking = false; return; }
		for (const s of Array.from(overlays.values())) {
			if (!s.removed) positionHud(s);
		}
		if (overlays.size) requestAnimationFrame(tick);
		else ticking = false;
	}

	function createHud(target, capture) {
		const el = document.createElement("div");
		el.className = "rcg-hud";
		const canvas = document.createElement("canvas");
		canvas.className = "rcg-boxes";
		const shimmer = createShimmer();
		const pill = document.createElement("div");
		pill.className = "rcg-pill pending";
		const row = document.createElement("div");
		row.className = "rcg-pill-row";
		const txt = document.createElement("span");
		txt.className = "rcg-txt";
		txt.textContent = "recognizing…";
		const spin = document.createElement("span");
		spin.className = "rcg-spin";
		const x = document.createElement("button");
		x.className = "rcg-x";
		x.type = "button";
		x.title = "Dismiss this overlay (Esc clears all)";
		x.textContent = "✕";
		row.append(spin, txt, x);
		const people = document.createElement("div");
		people.className = "rcg-people";
		const list = document.createElement("div");
		list.className = "rcg-people-inner";
		people.appendChild(list);
		pill.append(row, people);
		el.append(canvas, shimmer.el, pill);
		document.documentElement.appendChild(el);

		const s = { target, capture, el, canvas, shimmer, pill, row, txt, list, faces: null, cr: null, seq: 0, removed: false };
		x.addEventListener("click", (e) => {
			e.stopPropagation(); // dismissal must not read as a rescan
			removeHud(s);
		});
		// The pill is clickable for hover's sake — forward body clicks to the
		// same rescan a shield click would do, so it is not a dead zone.
		pill.addEventListener("click", (e) => {
			if (x.contains(e.target)) return;
			captureAt(e.clientX, e.clientY);
		});
		// A run's results only land if its seq is still the HUD's latest —
		// rapid re-clicks must not let a slow first request win.
		s.showResult = (data, seq) => {
			if (s.removed || seq !== s.seq) return;
			s.shimmer.disappear();
			s.faces = data.faces;
			const known = s.faces.filter((f) => f && f.name && f.name !== "unknown").length;
			if (!s.faces.length) setPill(s, "0 faces", "warn", false);
			else {
				setPill(s, s.faces.length + (s.faces.length === 1 ? " face" : " faces") + " · " + known + " known",
					known ? "ok" : "warn", false);
			}
			renderPeople(s);
			renderFaces(s);
		};
		s.showError = (message, seq) => {
			if (s.removed || seq !== s.seq) return;
			s.shimmer.disappear();
			setPill(s, String(message).slice(0, 140), "err", false);
		};
		overlays.set(target, s);
		positionHud(s);
		startTicker();
		return s;
	}

	// ---- orchestration ----

	async function captureAt(x, y) {
		const target = findTarget(x, y);
		if (!target) {
			toast("No image or video under the cursor", "info");
			return;
		}
		const box = target.getBoundingClientRect();
		if (box.width < 2 && box.height < 2) {
			toast("That element is not visible on screen", "error");
			return;
		}
		// Reuse an existing HUD for this element (click-to-refresh): it re-runs
		// in place, so a failed run never makes the overlay vanish.
		let hud = overlays.get(target);
		if (hud) {
			hud.faces = null;
			renderFaces(hud);
			renderPeople(hud);
			setPill(hud, "recognizing…", "pending", true);
			hud.shimmer.appear();
		}
		let capture;
		try {
			capture = await captureMedia(target);
		} catch (err) {
			if (hud) hud.showError(err.message || "capture failed", hud.seq);
			else toast(err.message || "capture failed", "error");
			return;
		}
		if (!hud) hud = createHud(target, capture);
		const seq = ++hud.seq;
		hud.shimmer.appear();
		try {
			const data = await recognize(capture.blob);
			hud.showResult(data, seq);
		} catch (err) {
			hud.showError(err.message || "recognition failed", seq);
		}
	}

	// ---- menu commands ----

	function setServerURL() {
		const next = window.prompt("recogn server URL (e.g. " + CONFIG.server + ")", server);
		if (next == null) return;
		const v = next.trim().replace(/\/+$/, "");
		if (!v) {
			server = CONFIG.server;
			gmSet("server", "");
			toast("Using the built-in default " + server, "info");
			return;
		}
		try {
			const u = new URL(v);
			if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error("scheme");
		} catch (_) {
			toast("Not a valid http(s) URL", "error");
			return;
		}
		server = v;
		gmSet("server", v);
		toast("recogn server set to " + v, "info");
	}

	function registerMenu() {
		const fn = (typeof GM_registerMenuCommand === "function" && GM_registerMenuCommand)
			|| (typeof GM !== "undefined" && GM && typeof GM.registerMenuCommand === "function" && GM.registerMenuCommand.bind(GM));
		if (!fn) return;
		fn("recogn: set server URL…", setServerURL);
		fn("recogn: clear all overlays", clearAllOverlays);
		fn("recogn: toggle capture mode (Alt+R)", toggleShield);
	}

	// ---- init ----

	function injectStyles() {
		if (document.querySelector("style.rcg-style")) return;
		const st = document.createElement("style");
		st.className = "rcg-style";
		st.textContent = CSS;
		(document.head || document.documentElement).appendChild(st);
	}

	(async function init() {
		// Guard across sandbox re-injections (SPAs, double installs).
		const host = (typeof unsafeWindow !== "undefined") ? unsafeWindow : window;
		if (host.__recognHoverCaptureLoaded) return;
		host.__recognHoverCaptureLoaded = true;

		try {
			const saved = await gmGet("server");
			if (saved) server = String(saved);
		} catch (_) { /* keep default */ }

		injectStyles();
		window.addEventListener("keydown", onKeyDown, true);
		registerMenu();
	})();
})();
