// Compare modal (admin tool).
// Face-to-face comparison of two arbitrary photos: the server embeds the
// largest face of each and reports their cosine similarity. Nothing is
// matched against the enrolled-people database. Photos can be added by
// browse, drag & drop, or paste; the result shows the similarity, a meter,
// and a "same person?" verdict against the recognizer's threshold.

import { useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { CompareResponse, Face } from "../types";
import { drawWhenReady } from "../overlay";
import { fmtSize } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

// Imperative surface App uses for clipboard routing while the modal is open.
export interface ComparePasteTarget {
	addFiles: (files: FileList | File[]) => void;
	isBusy: () => boolean;
}

interface CompareModalProps {
	open: boolean;
	onCloseRequest: () => void;
	registerPasteTarget: (t: ComparePasteTarget | null) => void;
}

interface Slot {
	file: File;
	url: string;
}

type SlotId = "a" | "b";

export function CompareModal(props: CompareModalProps) {
	const toast = useToast();

	const [slots, setSlots] = useState<{ a: Slot | null; b: Slot | null }>({ a: null, b: null });
	const [result, setResult] = useState<CompareResponse | null>(null);
	const [busy, setBusy] = useState(false);
	const [dropHint, setDropHint] = useState(false); // drag framing on the slots
	const [pulse, setPulse] = useState<SlotId | null>(null); // brief highlight on paste

	// Refs mirror the state so the (mount-stable) paste target closure and the
	// busy guard always see current values.
	const slotsRef = useRef(slots);
	const busyRef = useRef(false);
	const pulseTimer = useRef<number | undefined>(undefined);

	const setSlotsBoth = (next: { a: Slot | null; b: Slot | null }) => {
		slotsRef.current = next;
		setSlots(next);
	};

	const revoke = (s: Slot | null) => {
		if (s) URL.revokeObjectURL(s.url);
	};

	const setBusyBoth = (b: boolean) => {
		busyRef.current = b;
		setBusy(b);
	};

	// Fresh tool each time the modal opens; revoke the previews when it closes.
	useEffect(() => {
		if (!props.open) {
			revoke(slotsRef.current.a);
			revoke(slotsRef.current.b);
			setSlotsBoth({ a: null, b: null });
			setResult(null);
			setBusyBoth(false);
		}
	}, [props.open]);

	// Replace one slot's contents (null removes). Any change invalidates the
	// previous result.
	const setSlot = (id: SlotId, file: File | null) => {
		const prev = slotsRef.current;
		revoke(prev[id]);
		setSlotsBoth({ ...prev, [id]: file ? { file, url: URL.createObjectURL(file) } : null });
		setResult(null);
	};

	const clearAll = () => {
		setSlot("a", null);
		setSlot("b", null);
	};

	// ---- paste/drop routing surface (registered with App) ----
	useEffect(() => {
		props.registerPasteTarget({
			addFiles,
			isBusy: () => busyRef.current,
		});
		return () => props.registerPasteTarget(null);
	}, []);

	const compare = async () => {
		const a = slotsRef.current.a;
		const b = slotsRef.current.b;
		if (!a || !b || busyRef.current) return;
		setBusyBoth(true);
		setResult(null);
		try {
			const j = await api.compareFaces(a.file, b.file);
			// Ignore a result for photos that were swapped/cleared meanwhile
			// (the modal can be force-hidden by a session expiry mid-request).
			if (slotsRef.current.a?.file !== a.file || slotsRef.current.b?.file !== b.file) return;
			setResult(j);
		} catch (err) {
			toast.show((err as Error).message || "Comparison failed.", "err");
		} finally {
			setBusyBoth(false);
		}
	};

	// Place image files into the first empty slot(s); extra images and
	// non-images are reported, never silently dropped.
	function addFiles(files: FileList | File[]) {
		if (busyRef.current) return;
		const all = Array.from(files);
		const images = onlyImages(all);
		const notImage = all.length - images.length;
		const landed: SlotId[] = [];
		for (const file of images) {
			const id: SlotId | null = !slotsRef.current.a ? "a" : !slotsRef.current.b ? "b" : null;
			if (id === null) {
				toast.show("Both photo slots are filled — remove one first.", "err");
				break;
			}
			setSlot(id, file);
			landed.push(id);
		}
		if (landed.length) {
			const where = landed.length === 1 ? `Photo ${landed[0].toUpperCase()}` : "both photos";
			toast.show(`Added to ${where}.`, "ok");
			setPulse(landed[landed.length - 1]);
			clearTimeout(pulseTimer.current);
			pulseTimer.current = window.setTimeout(() => setPulse(null), 900);
			if (slotsRef.current.a && slotsRef.current.b) {
				compare()
			}
		}
		if (notImage > 0) toast.show(`${notImage} file(s) skipped — not images.`, "err");
	}

	const close = () => {
		if (busyRef.current) return; // locked while the request is in flight
		props.onCloseRequest();
	};

	// The compared face of each photo, as an overlay Face for the brackets.
	const usedA = usedFaceOf(result, "image1");
	const usedB = usedFaceOf(result, "image2");

	const canDrop = !busy;

	return (
		<Modal
			open={props.open}
			id="compareModal"
			cardId="compareCard"
			cardClass="wide"
			locked={busy}
			eyebrow="admin tools"
			titleId="compareTitle"
			title="Compare two photos"
			backdropClickDisabled={true}
			onCloseRequest={close}
			rootProps={{
				onDragEnter: (e) => { e.preventDefault(); if (canDrop) setDropHint(true); },
				onDragOver: (e) => { e.preventDefault(); if (canDrop) setDropHint(true); },
				onDragLeave: (e) => {
					if (!(e as DragEvent).relatedTarget) setDropHint(false);
				},
				onDrop: (e) => {
					e.preventDefault();
					setDropHint(false);
					if (canDrop && e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
				},
			}}
			body={
				<div class="modal-body">
					<p class="compare-intro" id="compareHint">
						Drop two photos — the largest face in each is compared directly, without the
						enrolled-people database.
					</p>

					<div class={"compare-grid" + (dropHint ? " drag" : "")}>
						<CompareSlot
							id="compareSlotA"
							label="Photo A"
							slot={slots.a}
							usedFace={usedA}
							pulse={pulse === "a"}
							busy={busy}
							onPick={(f) => setSlot("a", f)}
							onRemove={() => setSlot("a", null)}
						/>
						<CompareSlot
							id="compareSlotB"
							label="Photo B"
							slot={slots.b}
							usedFace={usedB}
							pulse={pulse === "b"}
							busy={busy}
							onPick={(f) => setSlot("b", f)}
							onRemove={() => setSlot("b", null)}
						/>
					</div>

					{result !== null && <CompareResult result={result} />}

					<footer class="modal-foot">
						<button
							type="button"
							class="btn btn-ghost"
							onClick={clearAll}
							disabled={busy || (!slots.a && !slots.b)}
						>
							Clear
						</button>
						<button type="button" class="btn btn-ghost" data-close>Cancel</button>
						<button
							type="button"
							id="compareSubmit"
							class="btn btn-accent"
							disabled={!slots.a || !slots.b || busy}
							onClick={compare}
						>
							{busy ? "Comparing…" : "Compare faces"}
						</button>
					</footer>
				</div>
			}
		/>
	);
}

// Keep only image files, preserving their order.
function onlyImages(files: File[]): File[] {
	return files.filter((f) => f.type.startsWith("image/"));
}

// The compared face of one photo, mapped onto the overlay renderer's Face
// shape (no identity fields — brackets only).
function usedFaceOf(result: CompareResponse | null, key: "image1" | "image2"): Face | null {
	if (!result) return null;
	const f = result[key].faces.find((x) => x.used);
	if (!f) return null;
	return { bbox: f.bbox, score: f.score, landmarks: [], name: "", person_id: "", confidence: 0 };
}

// One photo slot: dashed drop target when empty, preview with the compared
// face's brackets and a remove button when filled.
function CompareSlot(props: {
	id: string;
	label: string;
	slot: Slot | null;
	usedFace: Face | null;
	pulse: boolean;
	busy: boolean;
	onPick: (file: File) => void;
	onRemove: () => void;
}) {
	const inputRef = useRef<HTMLInputElement | null>(null);
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);
	const { slot, usedFace } = props;

	// Draw the compared face's brackets once the result arrives (after the
	// image element exists).
	useEffect(() => {
		if (!slot || !usedFace || !imgRef.current || !canvasRef.current) return;
		drawWhenReady(canvasRef.current, imgRef.current, [usedFace], { labels: false });
	}, [slot, usedFace]);

	return (
		<div
			id={props.id}
			class={
				"compare-slot" +
				(slot ? " filled" : "") +
				(props.pulse ? " pulse" : "") +
				(props.busy ? " disabled" : "")
			}
		>
			<span class="compare-slot-label">{props.label}</span>
			{slot ? (
				<>
					<div class="thumb-wrap">
						<img ref={imgRef} src={slot.url} alt={props.label} />
						<canvas ref={canvasRef} />
					</div>
					<div class="compare-slot-meta">
						<span class="compare-slot-name" title={slot.file.name}>{slot.file.name}</span>
						<span class="compare-slot-size">{fmtSize(slot.file.size)}</span>
					</div>
					<button
						type="button"
						class="enroll-thumb-del"
						aria-label={`Remove ${props.label}`}
						onClick={props.onRemove}
					>
						×
					</button>
				</>
			) : (
				<div
					class="compare-empty"
					tabindex={0}
					role="button"
					aria-label={`${props.label} — drop, paste, or browse`}
					onClick={() => { if (!props.busy) inputRef.current?.click(); }}
					onKeyDown={(e) => {
						if (e.key === "Enter" || e.key === " ") {
							e.preventDefault();
							inputRef.current?.click();
						}
					}}
				>
					<svg viewBox="0 0 48 48" class="edz-icon" aria-hidden="true">
						<path
							d="M15 8h-5a2 2 0 0 0-2 2v5M33 8h5a2 2 0 0 1 2 2v5M15 40h-5a2 2 0 0 1-2-2v-5M33 40h5a2 2 0 0 0 2-2v-5"
							fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"
						/>
						<circle cx="24" cy="21" r="5" fill="none" stroke="currentColor" stroke-width="2" />
						<path
							d="M15 36c2-4.5 5.4-6 9-6s7 1.6 9 6"
							fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"
						/>
					</svg>
					<p class="edz-primary">Drop, paste, or <span class="edz-browse">browse</span></p>
					<p class="edz-hint">a photo with one face</p>
				</div>
			)}
			<input
				ref={inputRef}
				id={props.id + "Input"}
				type="file"
				accept="image/*"
				hidden
				onChange={(e) => {
					const f = e.currentTarget.files?.[0];
					if (f) props.onPick(f);
					e.currentTarget.value = ""; // allow re-picking the same file later
				}}
			/>
		</div>
	);
}

// The comparison outcome: similarity value, meter, and verdict.
function CompareResult(props: { result: CompareResponse }) {
	const r = props.result;
	const same = r.similarity >= r.threshold;
	const pct = Math.round(Math.max(0, Math.min(1, r.similarity)) * 1000) / 10;
	return (
		<div class="compare-result" aria-live="polite">
			<div class="compare-score-row">
				<span class={"compare-score " + (same ? "ok" : "warn")}>
					{(Math.max(0, r.similarity) * 100).toFixed(1)}%
				</span>
				<span class={"compare-verdict " + (same ? "ok" : "warn")}>
					{same ? "Likely the same person" : "Likely different people"}
				</span>
			</div>
			<div class={"conf-bar compare-meter " + (same ? "ok" : "warn")}>
				<span style={{ width: `${pct}%` }} />
			</div>
			<p class="compare-notes">
				cosine {r.similarity.toFixed(4)} · threshold {r.threshold.toFixed(2)} ·{" "}
				{describePhoto("A", r.image1)} · {describePhoto("B", r.image2)}
			</p>
		</div>
	);
}

// "Photo A: 1 face" / "Photo A: 3 faces — compared #2 (largest)".
function describePhoto(label: string, img: CompareResponse["image1"]): string {
	const used = img.faces.find((f) => f.used);
	if (img.count <= 1) return `Photo ${label}: 1 face`;
	return `Photo ${label}: ${img.count} faces — compared #${used?.index ?? "?"} (largest)`;
}
