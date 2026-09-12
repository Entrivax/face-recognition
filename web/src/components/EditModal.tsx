// Stage photo editor.
// Opt-in refinement pass before recognition: rotate (free angle −180…180°),
// crop a region, and add a solid pad ("skirt") around the image, with
// multi-step undo/redo. Opened from the stage's Edit button; "Use this
// image" exports a JPEG and hands it back for a fresh recognition run.
//
// Sessions are iterative by design: the editor always works from the
// ORIGINAL file — never a previous export — and its committed history
// (ops + undo position + active tool) survives close/reopen, so clicking
// Edit again restores the previous parameters. The session resets only when
// a different file becomes the source (any fresh upload on the stage).
//
// The image is shown WITHOUT EXIF auto-rotation (createImageBitmap's
// imageOrientation: "none"), matching what the Go server sees: image.Decode
// ignores the EXIF orientation flag, so a phone photo taken sideways must be
// fixed here rather than trusted to the browser preview.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedPointerEvent } from "preact";
import { useToast } from "../toast";
import { Modal } from "./Modal";

// Working-resolution cap. Recognition letterboxes to 640² and embeds at
// 112², so 2048 px keeps every edit visually faithful while staying fast,
// clear of iOS canvas-area limits, and far under the 32 MiB upload cap.
const MAX_WORK_SIDE = 2048;
// A crop smaller than this (working px) is treated as a click, not a drag.
const MIN_CROP = 16;

type PadColor = "black" | "white";
type Tool = "rotate" | "crop" | "pad";

// One committed edit. Crop rects are pixel coordinates in the space of the
// image as it exists at that point in the op list (i.e. after the ops
// before it), which is exactly the space the live preview draws.
type EditOp =
	| { kind: "rotate"; deg: number; color: PadColor } // expands the canvas to the rotated AABB
	| { kind: "crop"; x: number; y: number; w: number; h: number }
	| { kind: "skirt"; px: number; color: PadColor }; // uniform solid margin

interface Hist {
	ops: EditOp[];
	idx: number; // ops[0..idx) are applied; the tail is redoable
}

interface Rect {
	x: number;
	y: number;
	w: number;
	h: number;
}

interface EditModalProps {
	/** the original file to edit (null = closed); identity change resets the session */
	source: File | null;
	onCloseRequest: () => void;
	/** "Use this image": hand the exported file back to the stage */
	onUse: (file: File) => void;
}

const TOOL_HINTS: Record<Tool, string> = {
	rotate: "Straighten the photo — crop away the exposed corners afterwards.",
	crop: "Drag on the photo to pick the area to keep.",
	pad: "Adds a solid border around the image; black matches the detector's own padding.",
};

export function EditModal(props: EditModalProps) {
	const toast = useToast();
	const { source, onCloseRequest, onUse } = props;
	const open = source !== null;

	// ---- persistent session (survives close/reopen; reset per file) ----
	const [hist, setHist] = useState<Hist>({ ops: [], idx: 0 });
	const [tool, setTool] = useState<Tool>("rotate");
	const [padColor, setPadColor] = useState<PadColor>("black");

	// ---- transient state (dropped on close) ----
	const [pendingRot, setPendingRot] = useState(0); // slider angle, live preview
	const [pendingPadPct, setPendingPadPct] = useState(0); // pad slider, live preview
	const [crop, setCrop] = useState<Rect | null>(null);
	const [busy, setBusy] = useState(false);
	const [src, setSrc] = useState<HTMLCanvasElement | null>(null); // decoded original

	// Refs mirroring state for stable reads inside effects/async callbacks.
	const srcRef = useRef<HTMLCanvasElement | null>(null);
	srcRef.current = src;
	const histRef = useRef(hist);
	histRef.current = hist;
	const closeRef = useRef(onCloseRequest);
	closeRef.current = onCloseRequest;

	const decodedForRef = useRef<File | null>(null);
	const lastSourceRef = useRef<File | null>(null);
	const viewRef = useRef<HTMLCanvasElement | null>(null);
	const overlayRef = useRef<HTMLCanvasElement | null>(null);
	const dragRef = useRef<{ x: number; y: number } | null>(null);
	const scratchRef = useRef<[HTMLCanvasElement, HTMLCanvasElement] | null>(null);

	const getScratch = (): [HTMLCanvasElement, HTMLCanvasElement] => {
		if (!scratchRef.current) {
			scratchRef.current = [
				document.createElement("canvas"),
				document.createElement("canvas"),
			];
		}
		return scratchRef.current;
	};

	// A different file resets the whole session (parameters are per-file);
	// closing keeps everything so the next Edit restores it.
	useEffect(() => {
		if (!source || source === lastSourceRef.current) return;
		lastSourceRef.current = source;
		setHist({ ops: [], idx: 0 });
		setTool("rotate");
		setPadColor("black");
		setPendingRot(0);
		setPendingPadPct(0);
		setCrop(null);
		decodedForRef.current = null;
		setSrc(null);
	}, [source]);

	// Closing drops uncommitted tool state and the decoded bitmap (history
	// is kept); the bitmap is re-decoded on reopen and refolded identically.
	useEffect(() => {
		if (open) return;
		setPendingRot(0);
		setPendingPadPct(0);
		setCrop(null);
		decodedForRef.current = null;
		setSrc(null);
	}, [open]);

	// Decode the source (once per open session) into the working canvas.
	useEffect(() => {
		if (!open || !source) return;
		if (decodedForRef.current === source && srcRef.current) return;
		let cancelled = false;
		decodeToCanvas(source)
			.then((canvas) => {
				if (cancelled) return;
				decodedForRef.current = source;
				setSrc(canvas);
			})
			.catch(() => {
				if (cancelled) return;
				toast.show("Could not decode the image.", "err");
				closeRef.current();
			});
		return () => {
			cancelled = true;
		};
	}, [open, source, toast]);

	// Ops currently shown: everything applied plus a live-preview tail for
	// the tool being dragged (slider angle / pad size).
	const previewOps = (): EditOp[] | null => {
		const s = srcRef.current;
		if (!s) return null;
		const ops = histRef.current.ops.slice(0, histRef.current.idx);
		if (pendingRot !== 0) ops.push({ kind: "rotate", deg: pendingRot, color: padColor });
		if (pendingPadPct > 0) {
			const [w, h] = foldedSize(s.width, s.height, ops);
			const px = Math.round((pendingPadPct / 100) * Math.max(w, h));
			if (px > 0) ops.push({ kind: "skirt", px, color: padColor });
		}
		return ops;
	};

	// Refold the display canvas whenever anything visual changes.
	useEffect(() => {
		if (!open || !src) return;
		const view = viewRef.current;
		if (!view) return;
		const out = foldOps(src, previewOps() ?? [], getScratch());
		if (out === src) {
			view.width = src.width;
			view.height = src.height;
		} else {
			view.width = out.width;
			view.height = out.height;
		}
		const ctx = view.getContext("2d");
		if (ctx) ctx.drawImage(out, 0, 0);
	}, [open, src, hist, pendingRot, pendingPadPct, padColor]);

	// Crop overlay: matched to the display size, redrawn on crop changes.
	useEffect(() => {
		const view = viewRef.current;
		const ov = overlayRef.current;
		if (!view || !ov) return;
		if (ov.width !== view.width || ov.height !== view.height) {
			ov.width = view.width;
			ov.height = view.height;
		}
		drawCrop(ov, crop);
	}, [crop, open, src, hist, pendingRot, pendingPadPct, padColor]);

	// ---- history ----
	const commit = (op: EditOp) => {
		setHist((h) => ({ ops: [...h.ops.slice(0, h.idx), op], idx: h.idx + 1 }));
	};
	const undo = () => {
		setCrop(null);
		setHist((h) => (h.idx > 0 ? { ...h, idx: h.idx - 1 } : h));
	};
	const redo = () => {
		setCrop(null);
		setHist((h) => (h.idx < h.ops.length ? { ...h, idx: h.idx + 1 } : h));
	};
	const resetAll = () => {
		setCrop(null);
		setHist({ ops: [], idx: 0 });
	};

	// Ctrl/Cmd+Z, Ctrl/Cmd+Shift+Z, Ctrl+Y (not while a field has focus).
	useEffect(() => {
		if (!open) return;
		const onKey = (e: KeyboardEvent) => {
			const t = e.target as HTMLElement | null;
			if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
			if (!(e.ctrlKey || e.metaKey) || e.altKey) return;
			const k = e.key.toLowerCase();
			if (k === "z" && !e.shiftKey) {
				e.preventDefault();
				undo();
			} else if ((k === "z" && e.shiftKey) || k === "y") {
				e.preventDefault();
				redo();
			}
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	});

	// ---- tool commits ----
	const commitRotate = () => {
		if (pendingRot !== 0) commit({ kind: "rotate", deg: pendingRot, color: padColor });
		setPendingRot(0);
	};
	const commitQuick = (deg: number) => {
		setPendingRot(0);
		commit({ kind: "rotate", deg, color: padColor });
	};
	const commitPad = () => {
		const s = srcRef.current;
		if (s && pendingPadPct > 0) {
			const [w, h] = foldedSize(s.width, s.height, histRef.current.ops.slice(0, histRef.current.idx));
			const px = Math.round((pendingPadPct / 100) * Math.max(w, h));
			if (px > 0) commit({ kind: "skirt", px, color: padColor });
		}
		setPendingPadPct(0);
	};
	const applyCrop = () => {
		const view = viewRef.current;
		if (!view || !crop || crop.w < MIN_CROP || crop.h < MIN_CROP) return;
		const x = clamp(crop.x, 0, view.width - MIN_CROP);
		const y = clamp(crop.y, 0, view.height - MIN_CROP);
		const w = clamp(crop.w, MIN_CROP, view.width - x);
		const h = clamp(crop.h, MIN_CROP, view.height - y);
		commit({ kind: "crop", x, y, w, h });
		setCrop(null);
	};

	// ---- crop dragging (overlay canvas; pointer coords → image pixels) ----
	const onCropDown = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (tool !== "crop" || busy || !src) return;
		e.preventDefault();
		dragRef.current = toImgCoords(e);
		setCrop({ x: dragRef.current.x, y: dragRef.current.y, w: 0, h: 0 });
		e.currentTarget.setPointerCapture(e.pointerId);
	};
	const onCropMove = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (!dragRef.current) return;
		e.preventDefault();
		setCrop(normRect(dragRef.current, toImgCoords(e)));
	};
	const onCropUp = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (!dragRef.current) return;
		// pointercancel may already have released (and deactivated) the pointer
		if (e.currentTarget.hasPointerCapture?.(e.pointerId)) {
			e.currentTarget.releasePointerCapture(e.pointerId);
		}
		dragRef.current = null;
		setCrop((c) => (c && c.w >= MIN_CROP && c.h >= MIN_CROP ? c : null));
	};

	// ---- export ----
	const handleUse = () => {
		const s = srcRef.current;
		if (!source || !s) {
			closeRef.current();
			return;
		}
		if (hist.idx === 0) {
			closeRef.current(); // nothing effective applied — no pointless re-request
			return;
		}
		setBusy(true);
		try {
			const out = foldOps(s, hist.ops.slice(0, hist.idx), getScratch());
			out.toBlob(
				(blob) => {
					setBusy(false);
					if (!blob) {
						toast.show("Could not export the edited image.", "err");
						return;
					}
					onUse(new File([blob], exportName(source), { type: "image/jpeg" }));
				},
				"image/jpeg",
				0.92,
			);
		} catch {
			setBusy(false);
			toast.show("Could not export the edited image.", "err");
		}
	};

	const dims = src ? foldedSize(src.width, src.height, previewOps() ?? []) : null;

	return (
		<Modal
			open={open}
			id="editModal"
			cardId="editCard"
			cardClass="wide"
			locked={busy}
			eyebrow="edit image"
			titleId="editTitle"
			title="Adjust the photo"
			onCloseRequest={onCloseRequest}
			body={
				<div class="modal-body">
					<div class="edit-tools">
						<button type="button" class="edit-tool" aria-pressed={tool === "rotate"}
							onClick={() => { setCrop(null); setTool("rotate"); }}>Rotate</button>
						<button type="button" class="edit-tool" aria-pressed={tool === "crop"}
							onClick={() => { setCrop(null); setTool("crop"); }}>Crop</button>
						<button type="button" class="edit-tool" aria-pressed={tool === "pad"}
							onClick={() => { setCrop(null); setTool("pad"); }}>Pad</button>
						<span class="edit-dims">{dims ? `${dims[0]} × ${dims[1]} px` : ""}</span>
					</div>

					<div class="edit-stage">
						{!src ? (
							<p class="edit-loading">Loading image…</p>
						) : (
							<div class="edit-canvas-wrap" data-cropping={tool === "crop" ? "true" : undefined}>
								<canvas ref={viewRef} class="edit-canvas" aria-label="Photo being edited" />
								<canvas
									ref={overlayRef}
									class="edit-overlay"
									aria-hidden="true"
									onPointerDown={onCropDown}
									onPointerMove={onCropMove}
									onPointerUp={onCropUp}
									onPointerCancel={onCropUp}
								/>
							</div>
						)}
					</div>

					{tool === "rotate" && (
						<div class="edit-slider-row">
							<span class="edit-slider-label">Angle</span>
							<input
								type="range"
								min={-180}
								max={180}
								step={1}
								value={pendingRot}
								disabled={busy || !src}
								aria-label="Rotation angle"
								onInput={(e) => setPendingRot(Number(e.currentTarget.value))}
								onChange={commitRotate}
							/>
							<span class="edit-slider-value">{pendingRot}°</span>
							<button type="button" class="btn btn-sm" disabled={busy || !src} onClick={() => commitQuick(-90)}>−90°</button>
							<button type="button" class="btn btn-sm" disabled={busy || !src} onClick={() => commitQuick(90)}>+90°</button>
						</div>
					)}

					{tool === "crop" && (
						<div class="edit-crop-row">
							<span>{crop ? `${crop.w} × ${crop.h} px` : "No area selected yet"}</span>
							<button type="button" class="btn btn-ghost btn-sm" hidden={!crop} disabled={busy} onClick={() => setCrop(null)}>Clear</button>
							<button
								type="button"
								class="btn btn-accent btn-sm"
								disabled={!crop || crop.w < MIN_CROP || crop.h < MIN_CROP || busy}
								onClick={applyCrop}
							>
								Apply crop
							</button>
						</div>
					)}

					{tool === "pad" && (
						<div class="edit-slider-row">
							<span class="edit-slider-label">Pad</span>
							<input
								type="range"
								min={0}
								max={25}
								step={1}
								value={pendingPadPct}
								disabled={busy || !src}
								aria-label="Padding around the image"
								onInput={(e) => setPendingPadPct(Number(e.currentTarget.value))}
								onChange={commitPad}
							/>
							<span class="edit-slider-value">{pendingPadPct}%</span>
							<div class="edit-swatches" role="group" aria-label="Pad color">
								<button type="button" class="edit-swatch black" aria-pressed={padColor === "black"}
									aria-label="Black pad" disabled={busy} onClick={() => setPadColor("black")} />
								<button type="button" class="edit-swatch white" aria-pressed={padColor === "white"}
									aria-label="White pad" disabled={busy} onClick={() => setPadColor("white")} />
							</div>
						</div>
					)}

					<p class="edit-hint">{TOOL_HINTS[tool]}</p>

					<footer class="edit-foot">
						<div class="edit-history">
							<button type="button" class="btn btn-ghost btn-sm" disabled={busy || hist.idx === 0} onClick={undo}>Undo</button>
							<button type="button" class="btn btn-ghost btn-sm" disabled={busy || hist.idx >= hist.ops.length} onClick={redo}>Redo</button>
							<button
								type="button"
								class="btn btn-ghost btn-sm"
								disabled={busy || (hist.idx === 0 && hist.ops.length === 0)}
								onClick={resetAll}
							>
								Reset
							</button>
						</div>
						<div class="edit-actions">
							<button type="button" class="btn btn-ghost" data-close disabled={busy}>Cancel</button>
							<button type="button" class="btn btn-accent" disabled={busy || hist.idx === 0} onClick={handleUse}>
								{busy ? "Preparing…" : "Use this image"}
							</button>
						</div>
					</footer>
				</div>
			}
		/>
	);
}

// ---- helpers (module-level: pure, no state) ----

function clamp(v: number, lo: number, hi: number): number {
	return Math.min(hi, Math.max(lo, v));
}

function normRect(a: { x: number; y: number }, b: { x: number; y: number }): Rect {
	return {
		x: Math.round(Math.min(a.x, b.x)),
		y: Math.round(Math.min(a.y, b.y)),
		w: Math.round(Math.abs(a.x - b.x)),
		h: Math.round(Math.abs(a.y - b.y)),
	};
}

function exportName(f: File): string {
	const stem = (f.name || "photo").replace(/\.[^.]+$/, "") || "photo";
	return `${stem}-edited.jpg`;
}

// Pointer position on the crop overlay mapped into image-pixel space: the
// overlay's backing store matches the working-image dimensions while its CSS
// box is the displayed size, so the two ratios scale the coords.
function toImgCoords(e: TargetedPointerEvent<HTMLCanvasElement>): { x: number; y: number } {
	const cv = e.currentTarget;
	const r = cv.getBoundingClientRect();
	const sx = r.width > 0 ? cv.width / r.width : 1;
	const sy = r.height > 0 ? cv.height / r.height : 1;
	return {
		x: clamp((e.clientX - r.left) * sx, 0, cv.width),
		y: clamp((e.clientY - r.top) * sy, 0, cv.height),
	};
}

// Decode + downscale to the working resolution. imageOrientation "none"
// serves the raw pixels — what the Go server sees (image.Decode ignores the
// EXIF orientation flag). Browsers without the option fall back to the
// default decode (EXIF applied), noted here rather than worked around.
async function decodeToCanvas(file: File): Promise<HTMLCanvasElement> {
	let src: CanvasImageSource;
	let w: number;
	let h: number;
	let bmp: ImageBitmap | null = null;
	if (typeof createImageBitmap === "function") {
		try {
			bmp = await createImageBitmap(file, { imageOrientation: "none" });
		} catch {
			try {
				bmp = await createImageBitmap(file);
			} catch {
				bmp = null;
			}
		}
	}
	if (bmp) {
		src = bmp;
		w = bmp.width;
		h = bmp.height;
	} else {
		const img = await loadViaImg(file); // last-resort fallback (old Safari)
		src = img;
		w = img.naturalWidth;
		h = img.naturalHeight;
	}
	const scale = Math.min(1, MAX_WORK_SIDE / Math.max(w, h));
	const canvas = document.createElement("canvas");
	canvas.width = Math.max(1, Math.round(w * scale));
	canvas.height = Math.max(1, Math.round(h * scale));
	const ctx = canvas.getContext("2d");
	if (ctx) {
		ctx.imageSmoothingEnabled = true;
		ctx.imageSmoothingQuality = "high";
		ctx.drawImage(src, 0, 0, canvas.width, canvas.height);
	}
	bmp?.close();
	return canvas;
}

function loadViaImg(file: File): Promise<HTMLImageElement> {
	return new Promise((resolve, reject) => {
		const url = URL.createObjectURL(file);
		const img = new Image();
		img.onload = () => {
			URL.revokeObjectURL(url);
			resolve(img);
		};
		img.onerror = () => {
			URL.revokeObjectURL(url);
			reject(new Error("decode failed"));
		};
		img.src = url;
	});
}

function fillOf(c: PadColor): string {
	return c === "white" ? "#ffffff" : "#000000";
}

// Axis-aligned bounding box of a w×h rect rotated by deg.
function rotatedSize(w: number, h: number, deg: number): [number, number] {
	const a = (deg * Math.PI) / 180;
	const c = Math.abs(Math.cos(a));
	const s = Math.abs(Math.sin(a));
	return [Math.round(w * c + h * s), Math.round(w * s + h * c)];
}

// Dimensions after folding ops over a w×h image (no drawing).
function foldedSize(w: number, h: number, ops: EditOp[]): [number, number] {
	for (const op of ops) {
		if (op.kind === "rotate") {
			if (op.deg % 360 !== 0) [w, h] = rotatedSize(w, h, op.deg);
		} else if (op.kind === "crop") {
			w = op.w;
			h = op.h;
		} else {
			w += op.px * 2;
			h += op.px * 2;
		}
	}
	return [w, h];
}

// Fold ops over src into a canvas, reusing two scratch canvases ping-pong
// style so live previews never allocate a canvas per frame. Returns src
// itself when nothing applies — callers must not draw into it then.
function foldOps(src: HTMLCanvasElement, ops: EditOp[], scratch: HTMLCanvasElement[]): HTMLCanvasElement {
	let cur = src;
	let si = 0;
	for (const op of ops) {
		if (op.kind === "rotate" && op.deg % 360 === 0) continue;
		const [w, h] =
			op.kind === "rotate"
				? rotatedSize(cur.width, cur.height, op.deg)
				: op.kind === "crop"
					? [op.w, op.h]
					: [cur.width + op.px * 2, cur.height + op.px * 2];
		const next = scratch[si++ % 2];
		next.width = w; // resize also clears
		next.height = h;
		const ctx = next.getContext("2d");
		if (!ctx) continue;
		ctx.imageSmoothingEnabled = true;
		ctx.imageSmoothingQuality = "high";
		if (op.kind === "rotate") {
			ctx.fillStyle = fillOf(op.color);
			ctx.fillRect(0, 0, w, h);
			ctx.translate(w / 2, h / 2);
			ctx.rotate((op.deg * Math.PI) / 180);
			ctx.drawImage(cur, -cur.width / 2, -cur.height / 2);
		} else if (op.kind === "crop") {
			ctx.drawImage(cur, op.x, op.y, op.w, op.h, 0, 0, op.w, op.h);
		} else {
			ctx.fillStyle = fillOf(op.color);
			ctx.fillRect(0, 0, w, h);
			ctx.drawImage(cur, op.px, op.px);
		}
		cur = next;
	}
	return cur;
}

// Dim outside the crop rect and frame it with the same corner brackets the
// detector draws around faces — the editor borrows the recognition
// overlay's visual language on purpose.
function drawCrop(canvas: HTMLCanvasElement, rect: Rect | null): void {
	const ctx = canvas.getContext("2d");
	if (!ctx) return;
	ctx.clearRect(0, 0, canvas.width, canvas.height);
	if (!rect) return;
	const scale = Math.max(canvas.width, canvas.height) / 900;
	const { x, y, w, h } = rect;

	ctx.fillStyle = "rgba(11, 14, 18, 0.62)";
	ctx.fillRect(0, 0, canvas.width, y); // top
	ctx.fillRect(0, y + h, canvas.width, canvas.height - y - h); // bottom
	ctx.fillRect(0, y, x, h); // left
	ctx.fillRect(x + w, y, canvas.width - x - w, h); // right

	const col = "#38e0c8";
	const L = Math.max(14 * scale, Math.min(w, h) * 0.22); // bracket arm
	const lw = Math.max(2, 2.5 * scale);
	ctx.strokeStyle = col;
	ctx.lineWidth = lw;
	ctx.shadowColor = col;
	ctx.shadowBlur = 6 * scale;
	const corners: [number, number, number, number][] = [
		[x, y, 1, 1], [x + w, y, -1, 1],
		[x, y + h, 1, -1], [x + w, y + h, -1, -1],
	];
	for (const [cx, cy, sx, sy] of corners) {
		ctx.beginPath();
		ctx.moveTo(cx + L * sx, cy);
		ctx.lineTo(cx, cy);
		ctx.lineTo(cx, cy + L * sy);
		ctx.stroke();
	}
	ctx.shadowBlur = 0;
	ctx.lineWidth = 1;
	ctx.strokeStyle = "rgba(56, 224, 200, 0.35)";
	ctx.strokeRect(x, y, w, h);
}
