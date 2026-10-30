// Stage photo editor.
// Opt-in refinement pass before recognition: rotate (free angle −180…180°),
// crop a region, and add a solid pad ("skirt") around the image, with
// multi-step undo/redo. Opened from the stage through the modal service;
// "Use this image" exports a JPEG and hands it back for a fresh recognition
// run.
//
// Sessions are iterative by design: the editor always works from the
// ORIGINAL file — never a previous export — and its committed history
// (ops + undo position + active tool) survives close/reopen. Closing
// UNMOUNTS the instance (the modal service removes it), so the session is
// handed to the opener at close time (onSaveSession — the stage keeps it in
// a vault keyed by the source file) and restored at open (initialSession).
// Uncommitted tool state and the decoded bitmap are dropped on unmount; the
// bitmap is re-decoded on reopen and refolded identically. The session
// resets only when a different file becomes the source (any fresh upload on
// the stage).
//
// The image is shown WITHOUT EXIF auto-rotation (createImageBitmap's
// imageOrientation: "none"), matching what the Go server sees: image.Decode
// ignores the EXIF orientation flag, so a phone photo taken sideways must be
// fixed here rather than trusted to the browser preview.

import { useCallback, useEffect, useRef } from "preact/hooks";
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals";
import { Show } from "@preact/signals/utils";
import type { TargetedPointerEvent } from "preact";
import { useToast } from "../toast";
import { usePasteFiles } from "../paste";
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

/** The editor's per-file committed session — what survives close/reopen
 *  (kept by the opener between the two, see onSaveSession/initialSession). */
export interface EditSession {
	hist: Hist;
	tool: Tool;
	padColor: PadColor;
}

interface EditModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service: true while no other instance sits above */
	isTopmost: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
	/** the original file to edit */
	source: File;
	/** "Use this image": hand the exported file back to the stage */
	onUse: (file: File) => void;
	/** committed session of the SAME file to restore (null = fresh session) */
	initialSession?: EditSession | null;
	/** called at close time (unmount) with the committed session to keep */
	onSaveSession?: (source: File, session: EditSession) => void;
}

const TOOL_HINTS: Record<Tool, string> = {
	rotate: "Straighten the photo — crop away the exposed corners afterwards.",
	crop: "Drag on the photo to pick the area to keep.",
	pad: "Adds a solid border around the image; black matches the detector's own padding.",
};

export function EditModal(props: EditModalProps) {
	const toast = useToast();
	const { open, isTopmost, source, onUse, onCloseRequest, initialSession, onSaveSession } = props;

	// ---- per-file session (restored from the opener's vault when it holds
	// one for this file; fresh otherwise). Committed history, active tool and
	// pad color survive close/reopen through that vault; uncommitted tool
	// state and the decoded bitmap die with the instance, as before.
	const hist = useSignal<Hist>(initialSession?.hist ?? { ops: [], idx: 0 });
	const tool = useSignal<Tool>(initialSession?.tool ?? "rotate");
	const padColor = useSignal<PadColor>(initialSession?.padColor ?? "black");

	// ---- transient state (dropped at unmount) ----
	const pendingRot = useSignal(0); // slider angle, live preview
	const pendingPadPct = useSignal(0); // pad slider, live preview
	const crop = useSignal<Rect | null>(null);
	const busy = useSignal(false);
	const src = useSignal<HTMLCanvasElement | null>(null); // decoded original
	const view = useSignal<HTMLCanvasElement | null>(null);
	const overlay = useSignal<HTMLCanvasElement | null>(null);

	// Props mirrors for stable reads in the unmount cleanup and the async
	// decode flow (props are not signals).
	const closeRef = useRef(onCloseRequest);
	closeRef.current = onCloseRequest;
	const sourceRef = useRef(source);
	sourceRef.current = source;
	const saveRef = useRef(onSaveSession);
	saveRef.current = onSaveSession;

	const decodedForRef = useRef<File | null>(null);
	const dragRef = useRef<{ x: number; y: number } | null>(null);
	const scratchRef = useRef<[HTMLCanvasElement, HTMLCanvasElement] | null>(null);
	const setView = useCallback((canvas: HTMLCanvasElement | null) => { view.value = canvas; }, []);
	const setOverlay = useCallback((canvas: HTMLCanvasElement | null) => { overlay.value = canvas; }, []);

	const getScratch = (): [HTMLCanvasElement, HTMLCanvasElement] => {
		if (!scratchRef.current) {
			scratchRef.current = [
				document.createElement("canvas"),
				document.createElement("canvas"),
			];
		}
		return scratchRef.current;
	};

	// The session is per file: a source swap on a still-mounted instance (an
	// args replacement by the service) resets it and adopts the session the
	// opener passed for the new file. The service normally opens a fresh
	// instance per file — where this re-applies the values the init above
	// already used (skipped when the source is unchanged).
	const lastSourceRef = useRef<File | null>(null);
	useEffect(() => {
		if (source === lastSourceRef.current) return;
		lastSourceRef.current = source;
		batch(() => {
			hist.value = initialSession?.hist ?? { ops: [], idx: 0 };
			tool.value = initialSession?.tool ?? "rotate";
			padColor.value = initialSession?.padColor ?? "black";
			pendingRot.value = 0;
			pendingPadPct.value = 0;
			crop.value = null;
			src.value = null;
		})
		decodedForRef.current = null;
	}, [source]);

	// Close-time session save: the service unmounts the instance on close,
	// so the unmount cleanup hands the committed session to the opener's
	// vault. Uncommitted tool state (pending sliders, an in-progress crop)
	// is deliberately not saved — it dies with the instance. (Signals stay
	// readable after unmount, so the cleanup reads them directly.)
	useEffect(() => () => {
		saveRef.current?.(sourceRef.current, { hist: hist.value, tool: tool.value, padColor: padColor.value });
	}, []);

	// ---- clipboard routing (via the injected isTopmost flag) ----
	// While the editor is open it sits over the stage: a pasted image must
	// not swap the stage photo (and reset the edit session) under it.
	usePasteFiles(isTopmost, () => {
		toast.show("Close the editor to inspect a new photo.");
	});

	// Decode the source (once per open session) into the working canvas.
	useEffect(() => {
		if (decodedForRef.current === source && src.peek()) return;
		let cancelled = false;
		decodeToCanvas(source)
			.then((canvas) => {
				if (cancelled) return;
				decodedForRef.current = source;
				src.value = canvas;
			})
			.catch(() => {
				if (cancelled) return;
				toast.show("Could not decode the image.", "err");
				closeRef.current();
			});
		return () => {
			cancelled = true;
		};
	}, [source, toast]);

	// Ops currently shown: everything applied plus a live-preview tail for
	// the tool being dragged (slider angle / pad size).
	const previewOps = useComputed<EditOp[] | null>(() => {
		const s = src.value;
		if (!s) return null;
		const ops = hist.value.ops.slice(0, hist.value.idx);
		if (pendingRot.value !== 0) ops.push({ kind: "rotate", deg: pendingRot.value, color: padColor.value });
		if (pendingPadPct.value > 0) {
			const [w, h] = foldedSize(s.width, s.height, ops);
			const px = Math.round((pendingPadPct.value / 100) * Math.max(w, h));
			if (px > 0) ops.push({ kind: "skirt", px, color: padColor.value });
		}
		return ops;
	});

	// Refold the display canvas whenever anything visual changes.
	useSignalEffect(() => {
		const s = src.value;
		const ops = previewOps.value;
		const canvas = view.value;
		if (!s || !canvas) return;
		const out = foldOps(s, ops ?? [], getScratch());
		if (out === s) {
			canvas.width = s.width;
			canvas.height = s.height;
		} else {
			canvas.width = out.width;
			canvas.height = out.height;
		}
		const ctx = canvas.getContext("2d");
		if (ctx) ctx.drawImage(out, 0, 0);
	});

	// Crop overlay: matched to the display size, redrawn on crop changes.
	// (The signal reads come first so the effect subscribes to every refold
	// input even while the canvases are not mounted yet.)
	useSignalEffect(() => {
		previewOps.value;
		const c = crop.value;
		const canvas = view.value;
		const ov = overlay.value;
		if (!canvas || !ov) return;
		if (ov.width !== canvas.width || ov.height !== canvas.height) {
			ov.width = canvas.width;
			ov.height = canvas.height;
		}
		drawCrop(ov, c);
	});

	// ---- history ----
	const commit = (op: EditOp) => {
		const h = hist.value;
		hist.value = { ops: [...h.ops.slice(0, h.idx), op], idx: h.idx + 1 };
	};
	// Stable for the keyboard binding below (undo/redo read the signals at
	// call time, so no re-binding per render is needed).
	const undo = useCallback(() => {
		batch(() => {
			crop.value = null;
			const h = hist.value;
			if (h.idx > 0) hist.value = { ...h, idx: h.idx - 1 };
		})
	}, []);
	const redo = useCallback(() => {
		batch(() => {
			crop.value = null;
			const h = hist.value;
			if (h.idx < h.ops.length) hist.value = { ...h, idx: h.idx + 1 };
		})
	}, []);
	const resetAll = () => {
		batch(() => {
			crop.value = null;
			hist.value = { ops: [], idx: 0 };
		})
	};

	// Ctrl/Cmd+Z, Ctrl/Cmd+Shift+Z, Ctrl+Y (not while a field has focus).
	// Bound once: undo/redo are stable and touch only signals.
	useEffect(() => {
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
	}, [undo, redo]);

	// ---- tool commits ----
	const commitRotate = () => {
		batch(() => {
			if (pendingRot.value !== 0) commit({ kind: "rotate", deg: pendingRot.value, color: padColor.value });
			pendingRot.value = 0;
		})
	};
	const commitQuick = (deg: number) => {
		batch(() => {
			pendingRot.value = 0;
			commit({ kind: "rotate", deg, color: padColor.value });
		})
	};
	const commitPad = () => {
		const s = src.value;
		batch(() => {
			if (s && pendingPadPct.value > 0) {
				const [w, h] = foldedSize(s.width, s.height, hist.value.ops.slice(0, hist.value.idx));
				const px = Math.round((pendingPadPct.value / 100) * Math.max(w, h));
				if (px > 0) commit({ kind: "skirt", px, color: padColor.value });
			}
			pendingPadPct.value = 0;
		})
	};
	const applyCrop = () => {
		const canvas = view.peek();
		const c = crop.value;
		if (!canvas || !c || c.w < MIN_CROP || c.h < MIN_CROP) return;
		const x = clamp(c.x, 0, canvas.width - MIN_CROP);
		const y = clamp(c.y, 0, canvas.height - MIN_CROP);
		const w = clamp(c.w, MIN_CROP, canvas.width - x);
		const h = clamp(c.h, MIN_CROP, canvas.height - y);
		batch(() => {
			commit({ kind: "crop", x, y, w, h });
			crop.value = null;
		})
	};

	// ---- crop dragging (overlay canvas; pointer coords → image pixels) ----
	const onCropDown = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (tool.value !== "crop" || busy.value || !src.value) return;
		e.preventDefault();
		dragRef.current = toImgCoords(e);
		crop.value = { x: dragRef.current.x, y: dragRef.current.y, w: 0, h: 0 };
		e.currentTarget.setPointerCapture(e.pointerId);
	};
	const onCropMove = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (!dragRef.current) return;
		e.preventDefault();
		crop.value = normRect(dragRef.current, toImgCoords(e));
	};
	const onCropUp = (e: TargetedPointerEvent<HTMLCanvasElement>) => {
		if (!dragRef.current) return;
		// pointercancel may already have released (and deactivated) the pointer
		if (e.currentTarget.hasPointerCapture?.(e.pointerId)) {
			e.currentTarget.releasePointerCapture(e.pointerId);
		}
		dragRef.current = null;
		const c = crop.value;
		crop.value = c && c.w >= MIN_CROP && c.h >= MIN_CROP ? c : null;
	};

	// ---- export ----
	const handleUse = () => {
		const s = src.value;
		if (!source || !s) {
			closeRef.current();
			return;
		}
		const h = hist.value;
		if (h.idx === 0) {
			closeRef.current(); // nothing effective applied — no pointless re-request
			return;
		}
		busy.value = true;
		try {
			const out = foldOps(s, h.ops.slice(0, h.idx), getScratch());
			out.toBlob(
				(blob) => {
					busy.value = false;
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
			busy.value = false;
			toast.show("Could not export the edited image.", "err");
		}
	};

	const dims = useComputed(() => src.value ? foldedSize(src.value.width, src.value.height, previewOps.value ?? []) : null);

	// Footer states that depend on busy + history, as signal attributes.
	const undoDisabled = useComputed(() => busy.value || hist.value.idx === 0);
	const redoDisabled = useComputed(() => busy.value || hist.value.idx >= hist.value.ops.length);
	const resetDisabled = useComputed(() => busy.value || (hist.value.idx === 0 && hist.value.ops.length === 0));
	const editingDisabled = useComputed(() => busy.value || !src.value);

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
						<button type="button" class="edit-tool" aria-pressed={tool.value === "rotate"}
							onClick={() => { batch(() => { crop.value = null; tool.value = "rotate"; }) }}>Rotate</button>
						<button type="button" class="edit-tool" aria-pressed={tool.value === "crop"}
							onClick={() => { batch(() => { crop.value = null; tool.value = "crop"; }) }}>Crop</button>
						<button type="button" class="edit-tool" aria-pressed={tool.value === "pad"}
							onClick={() => { batch(() => { crop.value = null; tool.value = "pad"; }) }}>Pad</button>
						<span class="edit-dims">{dims.value ? `${dims.value[0]} × ${dims.value[1]} px` : ""}</span>
					</div>

					<div class="edit-stage">
						<Show when={src} fallback={<p class="edit-loading">Loading image…</p>}>
							{() => (
								<div class="edit-canvas-wrap" data-cropping={tool.value === "crop" ? "true" : undefined}>
									<canvas ref={setView} class="edit-canvas" aria-label="Photo being edited" />
									<canvas
										ref={setOverlay}
										class="edit-overlay"
										aria-hidden="true"
										onPointerDown={onCropDown}
										onPointerMove={onCropMove}
										onPointerUp={onCropUp}
										onPointerCancel={onCropUp}
									/>
								</div>
							)}
						</Show>
					</div>

					<Show when={() => tool.value === "rotate"}>
						{() => (
							<div class="edit-slider-row">
								<span class="edit-slider-label">Angle</span>
								<input
									type="range"
									min={-180}
									max={180}
									step={1}
									value={pendingRot}
									disabled={editingDisabled}
									aria-label="Rotation angle"
									onInput={(e) => { pendingRot.value = Number(e.currentTarget.value); }}
									onChange={commitRotate}
								/>
								<span class="edit-slider-value">{pendingRot}°</span>
								<button type="button" class="btn btn-sm" disabled={editingDisabled} onClick={() => commitQuick(-90)}>−90°</button>
								<button type="button" class="btn btn-sm" disabled={editingDisabled} onClick={() => commitQuick(90)}>+90°</button>
							</div>
						)}
					</Show>

					<Show when={() => tool.value === "crop"}>
						{() => {
							const c = crop.value;
							return (
								<div class="edit-crop-row">
									<span>{c ? `${c.w} × ${c.h} px` : "No area selected yet"}</span>
									<button type="button" class="btn btn-ghost btn-sm" hidden={!c} disabled={busy} onClick={() => { crop.value = null; }}>Clear</button>
									<button
										type="button"
										class="btn btn-accent btn-sm"
										disabled={!c || c.w < MIN_CROP || c.h < MIN_CROP || busy.value}
										onClick={applyCrop}
									>
										Apply crop
									</button>
								</div>
							);
						}}
					</Show>

					<Show when={() => tool.value === "pad"}>
						{() => (
							<div class="edit-slider-row">
								<span class="edit-slider-label">Pad</span>
								<input
									type="range"
									min={0}
									max={25}
									step={1}
									value={pendingPadPct}
									disabled={editingDisabled}
									aria-label="Padding around the image"
									onInput={(e) => { pendingPadPct.value = Number(e.currentTarget.value); }}
									onChange={commitPad}
								/>
								<span class="edit-slider-value">{pendingPadPct.value}%</span>
								<div class="edit-swatches" role="group" aria-label="Pad color">
									<button type="button" class="edit-swatch black" aria-pressed={padColor.value === "black"}
										aria-label="Black pad" disabled={editingDisabled} onClick={() => { padColor.value = "black"; }} />
									<button type="button" class="edit-swatch white" aria-pressed={padColor.value === "white"}
										aria-label="White pad" disabled={editingDisabled} onClick={() => { padColor.value = "white"; }} />
								</div>
							</div>
						)}
					</Show>

					<p class="edit-hint">{TOOL_HINTS[tool.value]}</p>

					<footer class="edit-foot">
						<div class="edit-history">
							<button type="button" class="btn btn-ghost btn-sm" disabled={undoDisabled} onClick={undo}>Undo</button>
							<button type="button" class="btn btn-ghost btn-sm" disabled={redoDisabled} onClick={redo}>Redo</button>
							<button
								type="button"
								class="btn btn-ghost btn-sm"
								disabled={resetDisabled}
								onClick={resetAll}
							>
								Reset
							</button>
						</div>
						<div class="edit-actions">
							<button type="button" class="btn btn-ghost" data-close disabled={busy}>Cancel</button>
							<button type="button" class="btn btn-accent" disabled={undoDisabled} onClick={handleUse}>
								Use this image
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
