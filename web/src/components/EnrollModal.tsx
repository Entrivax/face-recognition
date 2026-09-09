// Enroll modal.
// Photos chosen for enrollment are held client-side for review; the upload
// only happens when the user confirms with the "Enroll" button. More photos
// can be added at any time by drag & drop, paste, or the file input. Each
// pending photo immediately gets a server-side face check (sequential —
// inference is a single serialized stream) so the user sees the detected
// faces before committing to enrollment.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { CheckView, Face } from "../types";
import { drawFaces } from "../overlay";
import { fmtSize, plural } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

const NAME_HINT = "Shown as the identity when their face is recognized.";

interface PendingPhoto {
	key: string;
	file: File;
	url: string;
	faces: Face[] | null;
	status: "checking" | "checked" | "error";
	failed?: boolean;
	note?: string;
}

// Imperative surface App uses for clipboard routing while the modal is open.
export interface EnrollPasteTarget {
	addPending: (files: FileList | File[]) => void;
	pulseDrop: () => void;
	scrollLastThumb: () => void;
	isBusy: () => boolean;
}

interface EnrollModalProps {
	open: boolean;
	peopleNames: string[];
	onCloseRequest: () => void;
	/** refresh people + health after a successful enroll */
	onChange: () => void;
	/** open the enlarged face-check viewer for a pending photo */
	onCheck: (view: CheckView) => void;
	/** live-update the viewer when a check finishes for the photo it shows */
	onCheckFaces: (src: string, faces: Face[]) => void;
	registerPasteTarget: (t: EnrollPasteTarget | null) => void;
}

export function EnrollModal(props: EnrollModalProps) {
	const toast = useToast();

	const [name, setName] = useState("");
	const [pending, setPending] = useState<PendingPhoto[]>([]);
	const [enrolling, setEnrolling] = useState(false);
	const [dropHint, setDropHint] = useState(false); // .drag on the dropzone
	const [pulse, setPulse] = useState(false); // brief highlight on paste
	const [submitMeta, setSubmitMeta] = useState<string | null>(null);

	const pendingRef = useRef<PendingPhoto[]>([]);
	const enrollingRef = useRef(false);
	const nameInputRef = useRef<HTMLInputElement | null>(null);
	const dropInputRef = useRef<HTMLInputElement | null>(null);
	const thumbsRef = useRef<HTMLUListElement | null>(null);
	const pulseTimer = useRef<number | undefined>(undefined);
	const chainRef = useRef<Promise<void>>(Promise.resolve());

	// State + ref are updated together: async checks test membership via the
	// ref (photos can be removed while their check is queued/in flight).
	const setPendingBoth = (updater: (prev: PendingPhoto[]) => PendingPhoto[]) => {
		setPending((prev) => {
			const next = updater(prev);
			pendingRef.current = next;
			return next;
		});
	};

	const clearPending = () => {
		for (const p of pendingRef.current) URL.revokeObjectURL(p.url);
		pendingRef.current = [];
		setPending([]);
	};

	const setBusy = (b: boolean) => {
		enrollingRef.current = b;
		setEnrolling(b);
	};

	// Fresh session each time the modal opens; empty it when it closes.
	useEffect(() => {
		clearPending();
		setSubmitMeta(null);
		if (props.open) setName("");
	}, [props.open]);

	// ---- paste/drop routing surface (registered with App) ----
	useEffect(() => {
		props.registerPasteTarget({
			addPending,
			pulseDrop: () => {
				setPulse(true);
				clearTimeout(pulseTimer.current);
				pulseTimer.current = window.setTimeout(() => setPulse(false), 900);
			},
			scrollLastThumb: () => {
				thumbsRef.current?.lastElementChild?.scrollIntoView({ block: "nearest" });
			},
			isBusy: () => enrollingRef.current,
		});
		return () => props.registerPasteTarget(null);
	}, []);

	// ---- pre-submit face check ----
	// Each pending photo is sent to /api/recognize as soon as it joins the
	// list (sequentially — inference is a single serialized stream) so the
	// user sees the detected faces before deciding to enroll.
	const updatePending = (target: PendingPhoto, patch: Partial<PendingPhoto>) => {
		setPendingBoth((prev) => prev.map((p) => (p === target ? { ...p, ...patch } : p)));
	};

	const checkPendingFace = async (p: PendingPhoto) => {
		if (!pendingRef.current.includes(p)) return; // removed while queued
		try {
			const j = await api.recognize(p.file);
			if (!pendingRef.current.includes(p)) return;
			const faces = j.faces || [];
			updatePending(p, { faces, status: "checked" });
			// Live-update the enlarged viewer when it is showing this photo.
			props.onCheckFaces(p.url, faces);
		} catch {
			if (!pendingRef.current.includes(p)) return;
			updatePending(p, { status: "error" });
		}
	};

	// Add files to the review list (no upload). Dedupes by name+size+mtime.
	function addPending(fileList: FileList | File[]) {
		let added = 0, notImage = 0, duplicate = 0;
		const additions: PendingPhoto[] = [];
		for (const file of Array.from(fileList)) {
			if (!file.type.startsWith("image/")) { notImage++; continue; }
			const key = `${file.name}\n${file.size}\n${file.lastModified}`;
			if (pendingRef.current.some((p) => p.key === key)) { duplicate++; continue; }
			additions.push({ key, file, url: URL.createObjectURL(file), faces: null, status: "checking" });
			added++;
		}
		if (additions.length) {
			setPendingBoth((prev) => [...prev, ...additions]);
			for (const p of additions) {
				chainRef.current = chainRef.current
					.then(() => checkPendingFace(p))
					.catch(() => {});
			}
		}
		if (notImage) toast.show(`${notImage} file(s) skipped — not images.`, "err");
		if (duplicate && !added) toast.show(`${duplicate} file(s) already selected.`);
	}

	function removePending(file: File) {
		const i = pendingRef.current.findIndex((p) => p.file === file);
		if (i < 0) return;
		URL.revokeObjectURL(pendingRef.current[i].url);
		setPendingBoth((prev) => prev.filter((p) => p.file !== file));
	}

	const close = () => {
		if (enrollingRef.current) return; // locked while the request is in flight
		props.onCloseRequest();
	};

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (enrollingRef.current) return;
		const person = name.trim();
		if (!person) {
			toast.show("Enter the person's name first.", "err");
			nameInputRef.current?.focus();
			return;
		}
		if (!pendingRef.current.length) {
			toast.show("Add at least one photo.", "err");
			return;
		}

		setBusy(true);
		setSubmitMeta(`Enrolling ${pendingRef.current.length} photo${pendingRef.current.length === 1 ? "" : "s"} for ${person}…`);

		try {
			const j = await api.enrollPhotos(person, pendingRef.current.map((p) => p.file));

			if (j.added > 0) toast.show(`Enrolled ${j.added} photo${j.added === 1 ? "" : "s"} for ${person}.`, "ok");
			props.onChange();

			// Keep the modal open when some photos failed, so they can be swapped.
			const failures = j.failures || [];
			const failedNames = new Set(failures
				.map((f) => (f.includes(":") ? f.slice(0, f.indexOf(":")) : f).trim())
				.filter(Boolean));
			const kept: PendingPhoto[] = [];
			for (const p of pendingRef.current) {
				if (!failedNames.has(p.file.name)) {
					URL.revokeObjectURL(p.url);
					continue;
				}
				const raw = failures.find((f) => f.startsWith(p.file.name + ":")) || "";
				const reason = raw.slice(p.file.name.length + 1).trim() || "could not be enrolled";
				kept.push({ ...p, failed: true, note: reason.includes("no face detected") ? "no face detected" : reason });
			}
			pendingRef.current = kept;
			setPending(kept);
			setBusy(false);
			if (kept.length) {
				setSubmitMeta(null);
				toast.show(`${kept.length} photo(s) had no detectable face — remove or replace them.`, "err");
			} else {
				close();
			}
		} catch (err) {
			setBusy(false);
			setSubmitMeta(null);
			toast.show((err as Error).message || "Enroll failed.", "err");
		}
	};

	// ---- render ----
	const n = pending.length;
	const bytes = pending.reduce((s, p) => s + p.file.size, 0);
	const meta = submitMeta ?? (n
		? `${plural(n)} ready · ${fmtSize(bytes)} · review, then enroll`
		: "No photos selected yet.");

	const known = Boolean(name.trim()) && props.peopleNames.some((x) => x.toLowerCase() === name.trim().toLowerCase());
	const nameHint = known
		? `${name.trim()} is already enrolled — photos will be added to their profile.`
		: NAME_HINT;

	const canDrop = !enrolling;

	return (
		<Modal
			open={props.open}
			id="enrollModal"
			cardId="enrollCard"
			locked={enrolling}
			eyebrow="enrollment"
			titleId="enrollTitle"
			title="Enroll a new person"
			initialFocus={() => nameInputRef.current}
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
					if (canDrop && e.dataTransfer?.files?.length) addPending(e.dataTransfer.files);
				},
			}}
			body={
				<form id="enrollForm" class="modal-body" novalidate onSubmit={submit}>
					<label class="field-label" for="enrollName">Name</label>
					<input
						ref={nameInputRef}
						id="enrollName"
						type="text"
						placeholder="Person's name"
						autocomplete="off"
						spellcheck={false}
						disabled={enrolling}
						value={name}
						onInput={(e) => setName(e.currentTarget.value)}
					/>
					<p class={"field-hint" + (known ? " warn" : "")} id="enrollNameHint">{nameHint}</p>

					<div
						id="enrollDrop"
						class={"enroll-drop" + (enrolling ? " disabled" : "") + (dropHint ? " drag" : "") + (pulse ? " pulse" : "")}
						tabindex={0}
						role="button"
						aria-label="Add photos — drag and drop, paste, or press Enter to browse"
						onClick={() => { if (!enrolling) dropInputRef.current?.click(); }}
						onKeyDown={(e) => {
							if ((e.key === "Enter" || e.key === " ") && !enrolling) {
								e.preventDefault();
								dropInputRef.current?.click();
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
						<p class="edz-primary">Drag &amp; drop, paste, or <span class="edz-browse">browse</span></p>
						<p class="edz-hint">Nothing uploads until you review and confirm</p>
						<input
							ref={dropInputRef}
							id="enrollPhotos"
							type="file"
							accept="image/*"
							multiple
							hidden
							onChange={(e) => {
								if (e.currentTarget.files?.length) addPending(e.currentTarget.files);
								e.currentTarget.value = ""; // allow re-picking the same file later
							}}
						/>
					</div>

					<ul ref={thumbsRef} id="enrollThumbs" class="enroll-thumbs" hidden={n === 0}>
						{pending.map((p) => (
							<EnrollThumb
								key={p.key}
								p={p}
								onRemove={() => removePending(p.file)}
								onInspect={() => props.onCheck({ src: p.url, title: p.file.name, faces: p.faces })}
							/>
						))}
					</ul>
					<p class="enroll-meta" id="enrollMeta">{meta}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Cancel</button>
						<button type="submit" id="enrollSubmit" class="btn btn-accent" disabled={!n || !name.trim()}>
							{enrolling ? "Enrolling…" : n ? `Enroll ${n} photo${n === 1 ? "" : "s"}` : "Enroll photos"}
						</button>
					</footer>
				</form>
			}
		/>
	);
}

// One pending-photo tile: preview with a face-check overlay canvas, status
// badge, and remove button. Clicking the tile inspects the faces full-size.
function EnrollThumb(props: {
	p: PendingPhoto;
	onRemove: () => void;
	onInspect: () => void;
}) {
	const { p } = props;
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);

	// Face boxes are drawn over the thumbnail as soon as detection returns.
	useEffect(() => {
		if (p.status !== "checked" || !p.faces) return;
		const img = imgRef.current;
		const canvas = canvasRef.current;
		if (!img || !canvas) return;
		const draw = () => drawFaces(canvas, img, p.faces || [], { labels: false });
		if (img.complete && img.naturalWidth) draw();
		else img.onload = draw;
	}, [p.status, p.faces]);

	let badgeText = "checking…";
	let badgeCls = "face-badge wait";
	if (p.status === "error") {
		badgeText = "check failed";
		badgeCls = "face-badge warn";
	} else if (p.status === "checked") {
		const n = p.faces?.length ?? 0;
		if (n === 0) { badgeText = "no face"; badgeCls = "face-badge warn"; }
		else if (n === 1) { badgeText = "1 face"; badgeCls = "face-badge ok"; }
		else { badgeText = `${n} faces`; badgeCls = "face-badge multi"; }
	}

	return (
		<li
			class={"enroll-thumb" + (p.failed ? " failed" : "")}
			tabIndex={0}
			title="Click to inspect faces"
			onClick={(e) => {
				if ((e.target as Element).closest?.(".enroll-thumb-del")) return;
				props.onInspect();
			}}
			onKeyDown={(e) => {
				if (e.key === "Enter" || e.key === " ") { e.preventDefault(); props.onInspect(); }
			}}
		>
			<div class="thumb-wrap">
				<img ref={imgRef} src={p.url} alt="" />
				<canvas ref={canvasRef} />
			</div>
			<span class={badgeCls}>{badgeText}</span>
			<button
				type="button"
				class="enroll-thumb-del"
				aria-label={`Remove ${p.file.name}`}
				onClick={props.onRemove}
			>
				×
			</button>
			<div class="enroll-thumb-meta">
				<div class="enroll-thumb-name" title={p.file.name}>{p.file.name}</div>
				<div class="enroll-thumb-size">{fmtSize(p.file.size)}</div>
			</div>
			{p.note !== undefined && <p class="enroll-thumb-note">{p.note}</p>}
		</li>
	);
}
