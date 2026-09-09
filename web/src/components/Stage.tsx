// The main stage: drop, paste, or browse a photo, detect every face, show
// each face's identity with its ranked matches. Unknown faces can be named
// and enrolled right from their row (re-sending the same file with the
// row's 1-based face index).

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import type { TargetedDragEvent, TargetedKeyboardEvent } from "preact";
import * as api from "../api";
import type { Face } from "../types";
import { drawWhenReady } from "../overlay";
import { useToast } from "../toast";

interface StageProps {
	/** refresh people + health after a face was enrolled from a row */
	onEnrolled: () => void;
	/** register the paste-routing entry point (inspect a file on the stage) */
	registerInspect: (fn: ((file: File) => void) | null) => void;
}

export function Stage({ onEnrolled, registerInspect }: StageProps) {
	const toast = useToast();

	const [previewURL, setPreviewURL] = useState<string | null>(null);
	const [meta, setMeta] = useState("");
	const [faces, setFaces] = useState<Face[] | null>(null);
	const [enrolled, setEnrolled] = useState<Record<number, string>>({});
	const [drag, setDrag] = useState(false);

	const fileInputRef = useRef<HTMLInputElement | null>(null);
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);
	// The File currently under inspection — kept so an unknown face row can
	// be enrolled via /api/people/{name}/enroll-face with the exact same
	// bytes (detection order is deterministic, so face_index matches the row).
	const currentFileRef = useRef<File | null>(null);
	// Object URL of the current stage preview; revoked when replaced or
	// cleared — otherwise every inspected photo would stay pinned in memory.
	const stageURLRef = useRef<string | null>(null);
	const reqSeq = useRef(0);

	const openPicker = () => fileInputRef.current?.click();

	const resetStage = useCallback(() => {
		if (stageURLRef.current) URL.revokeObjectURL(stageURLRef.current);
		stageURLRef.current = null;
		setPreviewURL(null);
		setFaces(null);
		setMeta("");
		setEnrolled({});
		currentFileRef.current = null;
	}, []);

	// Inspect one image file: show the preview immediately, then run
	// recognition and render the face rows.
	const handleFile = useCallback(async (file: File) => {
		if (!file.type.startsWith("image/")) {
			toast.show("That file isn't an image.", "err");
			return;
		}
		currentFileRef.current = file;
		const seq = ++reqSeq.current;
		// Show the preview immediately (revoking the previous object URL).
		const url = URL.createObjectURL(file);
		if (stageURLRef.current) URL.revokeObjectURL(stageURLRef.current);
		stageURLRef.current = url;
		setPreviewURL(url);
		setFaces(null);
		setEnrolled({});
		setMeta("analyzing…");
		try {
			const j = await api.recognize(file);
			if (seq !== reqSeq.current) return; // another file took over
			setFaces(j.faces || []);
			setMeta(`${file.name} · ${(file.size / 1024).toFixed(0)} KB`);
		} catch (e) {
			if (seq !== reqSeq.current) return;
			setMeta("");
			toast.show((e as Error).message || "Recognition failed.", "err");
		}
	}, [toast]);

	// Paste routing entry point.
	useEffect(() => {
		registerInspect(handleFile);
		return () => registerInspect(null);
	}, [handleFile, registerInspect]);

	// Draw the overlay once both the image and the faces are ready.
	useEffect(() => {
		if (previewURL && faces && imgRef.current && canvasRef.current) {
			drawWhenReady(canvasRef.current, imgRef.current, faces, { labels: true });
		}
	}, [previewURL, faces]);

	const onDropzoneClick = () => {
		if (previewURL === null) openPicker();
	};
	const onDropzoneKeyDown = (e: TargetedKeyboardEvent<HTMLDivElement>) => {
		if ((e.key === "Enter" || e.key === " ") && previewURL === null) {
			e.preventDefault();
			openPicker();
		}
	};
	const onDrop = (e: TargetedDragEvent<HTMLDivElement>) => {
		e.preventDefault();
		setDrag(false);
		const f = e.dataTransfer?.files?.[0];
		if (f) handleFile(f);
	};

	// Enroll the given 1-based face of the current photo under the typed
	// name; done(ok) lets the row re-enable itself on failure.
	const handleRowEnroll = useCallback((faceNo: number, person: string, done: (ok: boolean) => void) => {
		const file = currentFileRef.current;
		if (!file) { done(false); return; }
		api.enrollFace(person, file, faceNo)
			.then((j) => {
				toast.show(`Enrolled ${person} (${j.saved}).`, "ok");
				setEnrolled((m) => ({ ...m, [faceNo - 1]: person }));
				onEnrolled();
				done(true);
			})
			.catch((err: Error) => {
				toast.show(err.message || "Enrollment failed.", "err");
				done(false);
			});
	}, [onEnrolled, toast]);

	const cursor = previewURL ? "default" : "pointer";

	return (
		<section class="panel stage" aria-labelledby="stageTitle">
			<div class="panel-head">
				<h1 id="stageTitle">Inspect a photo</h1>
				<p class="lede">Drop or paste an image to detect every face and match each against the enrolled people.</p>
			</div>

			<div
				id="dropzone"
				class={"dropzone" + (drag ? " drag" : "")}
				style={`cursor: ${cursor}`}
				tabindex={0}
				role="button"
				aria-label="Upload a photo to recognize faces"
				onClick={onDropzoneClick}
				onKeyDown={onDropzoneKeyDown}
				onDragEnter={(e) => { e.preventDefault(); setDrag(true); }}
				onDragOver={(e) => { e.preventDefault(); setDrag(true); }}
				onDragLeave={(e) => { e.preventDefault(); setDrag(false); }}
				onDrop={onDrop}
			>
				<input
					ref={fileInputRef}
					id="fileInput"
					type="file"
					accept="image/*"
					hidden
					onChange={(e) => {
						const f = e.currentTarget.files?.[0];
						if (f) handleFile(f);
						e.currentTarget.value = "";
					}}
				/>
				{previewURL === null ? (
					<div class="dz-empty" id="dzEmpty">
						<svg viewBox="0 0 48 48" class="dz-icon" aria-hidden="true">
							<rect x="6" y="10" width="36" height="28" rx="3" fill="none" stroke="currentColor" stroke-width="2" />
							<circle cx="19" cy="21" r="4" fill="none" stroke="currentColor" stroke-width="2" />
							<path d="M6 34l10-9 8 7 8-6 10 8" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" />
						</svg>
						<p class="dz-primary">Drop a photo here, or <span class="dz-browse">browse</span></p>
						<p class="dz-hint">JPG, PNG, WebP or BMP — or paste from the clipboard (Ctrl+V)</p>
					</div>
				) : (
					<div class="dz-preview" id="dzPreview">
						<div class="canvas-wrap">
							<img ref={imgRef} id="previewImg" alt="Uploaded photo preview" src={previewURL} />
							<canvas ref={canvasRef} id="overlay" />
						</div>
					</div>
				)}
			</div>

			<div class="stage-actions">
				<button id="clearBtn" class="btn btn-ghost" hidden={previewURL === null} onClick={resetStage}>Clear</button>
				<button
					id="againBtn"
					class="btn btn-ghost"
					hidden={previewURL === null}
					onClick={(e) => { e.stopPropagation(); openPicker(); }}
				>
					Choose another
				</button>
				<span class="stage-meta" id="stageMeta">{meta}</span>
			</div>

			{faces !== null && (
				<div id="results" class="results">
					<h2 class="results-title"><span id="resultCount">{faces.length}</span> face(s) found</h2>
					<ul id="faceList" class="face-list">
						{faces.map((f, i) => (
							<FaceRow
								key={i}
								face={f}
								index={i}
								enrolledName={enrolled[i]}
								canEnroll={currentFileRef.current !== null}
								onEnroll={handleRowEnroll}
							/>
						))}
					</ul>
				</div>
			)}
		</section>
	);
}

// One face row: index badge, identity, ranked matches (with a duplicate hint
// when the top two scores are nearly tied), and — for unknown faces — the
// inline "name this person" enroll form.
function FaceRow(props: {
	face: Face;
	index: number;
	enrolledName?: string;
	canEnroll: boolean;
	onEnroll: (faceNo: number, person: string, done: (ok: boolean) => void) => void;
}) {
	const f = props.face;
	const name = props.enrolledName ?? f.name;
	const known = name !== "unknown";
	const conf = Math.round((f.confidence || 0) * 100);

	const matches = f.matches || [];
	const showMatches = matches.length >= 2;
	const near = showMatches && matches[0].score - matches[1].score <= 0.05;

	const [person, setPerson] = useState("");
	const [busy, setBusy] = useState(false);
	const inputRef = useRef<HTMLInputElement | null>(null);

	const matchesBlock = showMatches && (
		<>
			<ul class={"face-matches" + (near ? " ambiguous" : "")}>
				{matches.map((m, idx) => (
					<li key={`${m.person_id}-${idx}`} class={idx === 0 ? "top" : ""}>
						<span class="fm-name">{m.name}</span>
						<span class="fm-score">{Math.round((m.score || 0) * 100)}%</span>
					</li>
				))}
			</ul>
			{near && <p class="face-dup-hint">scores nearly tied — possible duplicate people?</p>}
		</>
	);

	return (
		<li class={"face-row" + (known ? "" : " unknown")}>
			<span class="face-index">{String(props.index + 1).padStart(2, "0")}</span>
			<div>
				<div class="face-name">{name}</div>
				{matchesBlock}
				{!known && props.canEnroll && (
					<form
						class="face-enroll"
						onSubmit={(e) => {
							e.preventDefault();
							const p = person.trim();
							if (!p) { inputRef.current?.focus(); return; }
							setBusy(true);
							props.onEnroll(props.index + 1, p, (ok) => {
								if (!ok) {
									setBusy(false);
									inputRef.current?.focus();
								}
							});
						}}
					>
						<input
							ref={inputRef}
							type="text"
							class="face-enroll-input"
							placeholder="Name this person…"
							maxLength={120}
							autocomplete="off"
							spellcheck={false}
							aria-label="Enroll this face as"
							disabled={busy}
							value={person}
							onInput={(e) => setPerson(e.currentTarget.value)}
						/>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Enroll</button>
					</form>
				)}
			</div>
			<div class="face-right">
				<span class="face-conf">{conf}%</span>
				<div class="conf-bar" style={`color:${known ? "var(--match)" : "var(--unknown)"}`}>
					<span style={`width:${conf}%`} />
				</div>
			</div>
		</li>
	);
}
