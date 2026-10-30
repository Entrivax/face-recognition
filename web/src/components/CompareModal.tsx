// Compare modal (any signed-in user).
// Face-to-face comparison of two arbitrary photos: the server embeds the
// largest face of each and reports their cosine similarity. Nothing is
// matched against the enrolled-people database. Photos can be added by
// browse, drag & drop, or paste (routed here by the modal service while the
// modal is open); the result shows the similarity, a meter, and a "same
// person?" verdict against the recognizer's threshold.
// The tool is also opened seeded from the enroll and photos modals: both
// photos are picked there (pending thumbs / enrolled grid) and handed over
// as a { a, b } seed arg — the comparison auto-runs when the instance opens.

import { useEffect, useRef } from "preact/hooks";
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals";
import type { ReadonlySignal } from "@preact/signals";
import { Show } from "@preact/signals/utils";
import * as api from "../api";
import type { CompareResponse, Face } from "../types";
import { drawWhenReady } from "../overlay";
import { fmtSize } from "../util";
import { useToast } from "../toast";
import { usePasteFiles } from "../paste";
import { Modal } from "./Modal";

// Both photos of a comparison launched from another modal (enroll pending
// thumbs / photos-manager grid). Compare builds its own preview URLs from
// these and revokes them when the instance unmounts.
export interface CompareSeed {
	a: File;
	b: File;
}

interface CompareModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service: true while no other instance sits above */
	isTopmost: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
	/** photos picked in the launching modal; consumed once on mount */
	seed?: CompareSeed | null;
}

interface Slot {
	file: File;
	url: string;
}

type SlotId = "a" | "b";

export function CompareModal(props: CompareModalProps) {
	const toast = useToast();

	const slotA = useSignal<Slot | null>(null);
	const slotB = useSignal<Slot | null>(null);
	const result = useSignal<CompareResponse | null>(null);
	const busy = useSignal(false);
	const dropHint = useSignal(false); // drag framing on the slots
	const pulse = useSignal<SlotId | null>(null); // brief highlight on paste

	const pulseTimer = useRef<number | undefined>(undefined);

	const revoke = (s: Slot | null) => {
		if (s) URL.revokeObjectURL(s.url);
	};

	// A seeded open (both photos picked in the launching modal) fills both
	// slots immediately and auto-runs the comparison. Consumed once: the seed
	// lives in the instance's args, and a later open() replaces them — a new
	// instance (after a close) gets a fresh seed anyway.
	useEffect(() => {
		const seed = props.seed;
		if (!seed || !seed.a || !seed.b) return;
		batch(() => {
			slotA.value = { file: seed.a, url: URL.createObjectURL(seed.a) };
			slotB.value = { file: seed.b, url: URL.createObjectURL(seed.b) };
			result.value = null;
		})
		compare(); // the slot signals were set synchronously above
	}, []);

	// The instance unmounts on close: revoke the previews and drop the pulse
	// timer (the previews die with the instance).
	useEffect(() => () => {
		clearTimeout(pulseTimer.current);
		revoke(slotA.value);
		revoke(slotB.value);
	}, []);

	// Replace one slot's contents (null removes). Any change invalidates the
	// previous result.
	const setSlot = (id: SlotId, file: File | null) => {
		const sig = id === "a" ? slotA : slotB;
		revoke(sig.value);
		batch(() => {
			sig.value = file ? { file, url: URL.createObjectURL(file) } : null;
			result.value = null;
		})
	};

	const clearAll = () => {
		batch(() => {
			setSlot("a", null);
			setSlot("b", null);
		})
	};

	// ---- paste routing (via the injected isTopmost flag) ----
	// While this modal is topmost, pasted images fill the empty slots.
	usePasteFiles(props.isTopmost, (files) => {
		if (busy.value) return; // busy swallows the paste silently
		addFiles(files);
	});

	const compare = async () => {
		const a = slotA.value;
		const b = slotB.value;
		if (!a || !b || busy.value) return;
		batch(() => {
			busy.value = true;
			result.value = null;
		})
		try {
			const j = await api.compareFaces(a.file, b.file);
			// Ignore a result for photos that were swapped/cleared meanwhile
			// (the modal can be force-hidden by a session expiry mid-request).
			if (slotA.value?.file !== a.file || slotB.value?.file !== b.file) return;
			result.value = j;
		} catch (err) {
			toast.show((err as Error).message || "Comparison failed.", "err");
		} finally {
			busy.value = false;
		}
	};

	// Place image files into the first empty slot(s); extra images and
	// non-images are reported, never silently dropped.
	function addFiles(files: FileList | File[]) {
		if (busy.value) return;
		const all = Array.from(files);
		const images = onlyImages(all);
		const notImage = all.length - images.length;
		const landed: SlotId[] = [];
		for (const file of images) {
			const id: SlotId | null = !slotA.value ? "a" : !slotB.value ? "b" : null;
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
			pulse.value = landed[landed.length - 1];
			clearTimeout(pulseTimer.current);
			pulseTimer.current = window.setTimeout(() => { pulse.value = null; }, 900);
			if (slotA.value && slotB.value) {
				compare()
			}
		}
		if (notImage > 0) toast.show(`${notImage} file(s) skipped — not images.`, "err");
	}

	const close = () => {
		if (busy.value) return; // locked while the request is in flight
		props.onCloseRequest();
	};

	// The compared face of each photo, as an overlay Face for the brackets.
	const usedA = useComputed(() => usedFaceOf(result.value, "image1"));
	const usedB = useComputed(() => usedFaceOf(result.value, "image2"));

	const gridClass = useComputed(() => "compare-grid" + (dropHint.value ? " drag" : ""));
	// Footer states as signal attributes (busy flips re-render this component
	// anyway for the label above).
	const clearDisabled = useComputed(() => busy.value || (!slotA.value && !slotB.value));
	const runDisabled = useComputed(() => !slotA.value || !slotB.value || busy.value);

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
				onDragEnter: (e) => { e.preventDefault(); if (!busy.value) dropHint.value = true; },
				onDragOver: (e) => { e.preventDefault(); if (!busy.value) dropHint.value = true; },
				onDragLeave: (e) => {
					if (!(e as DragEvent).relatedTarget) dropHint.value = false;
				},
				onDrop: (e) => {
					e.preventDefault();
					dropHint.value = false;
					if (!busy.value && e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
				},
			}}
			body={
				<div class="modal-body">
					<p class="compare-intro" id="compareHint">
						Drop two photos — the largest face in each is compared directly, without the
						enrolled-people database.
					</p>

					<div class={gridClass}>
						<CompareSlot
							id="compareSlotA"
							label="Photo A"
							slot={slotA}
							usedFace={usedA}
							pulse={pulse.value === "a"}
							busy={busy.value}
							onPick={(f) => setSlot("a", f)}
							onRemove={() => setSlot("a", null)}
						/>
						<CompareSlot
							id="compareSlotB"
							label="Photo B"
							slot={slotB}
							usedFace={usedB}
							pulse={pulse.value === "b"}
							busy={busy.value}
							onPick={(f) => setSlot("b", f)}
							onRemove={() => setSlot("b", null)}
						/>
					</div>

					<Show when={result}>
						{(r) => <CompareResult result={r} />}
					</Show>

					<footer class="modal-foot">
						<button
							type="button"
							class="btn btn-ghost"
							onClick={clearAll}
							disabled={clearDisabled}
						>
							Clear
						</button>
						<button type="button" class="btn btn-ghost" data-close>Cancel</button>
						<button
							type="button"
							id="compareSubmit"
							class="btn btn-accent"
							disabled={runDisabled}
							onClick={compare}
						>
							{busy.value ? "Comparing…" : "Compare faces"}
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
// face's brackets and a remove button when filled. (slot/usedFace arrive as
// signals; the filled/empty split and the bracket redraw are the slot's own
// concern, so the parent does not re-render for them.)
function CompareSlot(props: {
	id: string;
	label: string;
	slot: ReadonlySignal<Slot | null>;
	usedFace: ReadonlySignal<Face | null>;
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
	useSignalEffect(() => {
		const s = slot.value;
		const f = usedFace.value;
		if (!s || !f || !imgRef.current || !canvasRef.current) return;
		drawWhenReady(canvasRef.current, imgRef.current, [f], { labels: false });
	});

	return (
		<div
			id={props.id}
			class={
				"compare-slot" +
				(slot.value ? " filled" : "") +
				(props.pulse ? " pulse" : "") +
				(props.busy ? " disabled" : "")
			}
		>
			<span class="compare-slot-label">{props.label}</span>
			<Show when={slot} fallback={
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
			}>
				{(s) => (
					<>
						<div class="thumb-wrap">
							<img ref={imgRef} src={s.url} alt={props.label} />
							<canvas ref={canvasRef} />
						</div>
						<div class="compare-slot-meta">
							<span class="compare-slot-name" title={s.file.name}>{s.file.name}</span>
							<span class="compare-slot-size">{fmtSize(s.file.size)}</span>
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
				)}
			</Show>
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
				<span style={`width:${pct}%`} />
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
