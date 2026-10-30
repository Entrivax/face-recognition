// The main stage: drop, paste, or browse a photo, detect every face, show
// each face's identity with its ranked matches. Unknown faces can be named
// and enrolled right from their row (re-sending the same file with the
// row's 1-based face index).
//
// Clipboard images with no modal to take them land here (the modal
// service's fallback route). The photo editor is opened through the modal
// service and keeps its per-file session in this component's vault between
// the close (which unmounts the editor and hands the session back) and the
// next Edit on the same file.
//
// All component-local state is signals; the refs hold what signals must not
// (the current File, the object-URL lifecycle, the request sequence, the
// editor session vault).

import { useCallback, useRef } from "preact/hooks";
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals";
import type { Signal } from "@preact/signals";
import { For, Show } from "@preact/signals/utils";
import type { TargetedDragEvent, TargetedKeyboardEvent } from "preact";
import * as api from "../api";
import type { Face } from "../types";
import { drawWhenReady } from "../overlay";
import { useToast } from "../toast";
import { useApp } from "../appStore";
import { useModals } from "../modalRegistry";
import { usePasteFiles, PASTE_FALLBACK } from "../paste";
import type { EditSession } from "./EditModal";

/** The editor's session vault: the committed session of the file it belongs
 *  to (null source = no session kept). File identity is the key — any other
 *  file invalidates it. */
interface EditVault {
	source: File | null;
	session: EditSession | null;
}

export function Stage() {
	const toast = useToast();
	const app = useApp();
	const modals = useModals();

	const previewURL = useSignal<string | null>(null);
	const meta = useSignal("");
	const faces = useSignal<Face[] | null>(null);
	const enrolled = useSignal<Record<number, string>>({});
	const drag = useSignal(false);

	const fileInputRef = useRef<HTMLInputElement | null>(null);
	const imgRef = useRef<HTMLImageElement | null>(null);
	const canvasRef = useRef<HTMLCanvasElement | null>(null);
	// The File currently under inspection — kept so an unknown face row can
	// be enrolled via /api/people/{name}/enroll-face with the exact same
	// bytes (detection order is deterministic, so face_index matches the row).
	const currentFileRef = useRef<File | null>(null);
	// The pristine original behind the inspected file when that file is an
	// edit export (null = fresh upload). Lets every Edit round reopen the
	// editor over the original with the committed history restored, and
	// backs the "Cancel edits" button (re-inspect the original); any new
	// upload resets it, so the edit session resets with the photo.
	const editOriginal = useSignal<File | null>(null);
	// Object URL of the current stage preview; revoked when replaced or
	// cleared — otherwise every inspected photo would stay pinned in memory.
	const stageURLRef = useRef<string | null>(null);
	const reqSeq = useRef(0);
	// The editor's per-file session vault (see EditVault).
	const editVaultRef = useRef<EditVault>({ source: null, session: null });

	// The mount-stable callbacks read the store's current values at call
	// time (the model object is context-stable, so a repo switch never
	// invalidates them — an open editor's onUse arg keeps working).

	const openPicker = () => fileInputRef.current?.click();

	const resetStage = useCallback(() => {
		if (stageURLRef.current) URL.revokeObjectURL(stageURLRef.current);
		stageURLRef.current = null;
		batch(() => {
			previewURL.value = null;
			faces.value = null;
			meta.value = "";
			enrolled.value = {};
			editOriginal.value = null;
		})
		currentFileRef.current = null;
	}, []);

	// Inspect one image file: show the preview immediately, then run
	// recognition and render the face rows. `editedFrom` marks the file as an
	// edit export of `editedFrom` (the pristine original) so the editor can
	// reopen over the original; fresh uploads pass nothing and reset the
	// edit session.
	const handleFile = useCallback(async (file: File, editedFrom?: File) => {
		if (!file.type.startsWith("image/")) {
			toast.show("That file isn't an image.", "err");
			return;
		}
		currentFileRef.current = file;
		editOriginal.value = editedFrom ?? null;
		const seq = ++reqSeq.current;
		// Show the preview immediately (revoking the previous object URL).
		const url = URL.createObjectURL(file);
		resetStage()
		batch(() => {
			stageURLRef.current = url;
			previewURL.value = url;
			meta.value = "analyzing…";
		})
		try {
			const j = await api.recognize(file, app.repoId.value ?? "");
			if (seq !== reqSeq.current) return; // another file took over
			faces.value = j.faces || [];
			meta.value = `${file.name} · ${(file.size / 1024).toFixed(0)} KB`;
		} catch (e) {
			if (seq !== reqSeq.current) return;
			meta.value = "";
			toast.show((e as Error).message || "Recognition failed.", "err");
		}
	}, [toast, resetStage]);

	// Clipboard routing entry point: when no modal is interested (registered
	// under a modal key), the paste lands here (the reserved "*" fallback).
	// More than one image: the first is inspected, the rest reported.
	usePasteFiles(true, (files) => {
		void handleFile(files[0]);
		if (files.length > 1) toast.show("Clipboard had several images — inspecting the first.");
	}, PASTE_FALLBACK);

	// Draw the overlay once both the image and the faces are ready.
	useSignalEffect(() => {
		const list = faces.value;
		const url = previewURL.value;
		if (url && list && imgRef.current && canvasRef.current) {
			drawWhenReady(canvasRef.current, imgRef.current, list, { labels: true });
		}
	});

	// The editor hands its committed session back at close time; keep it for
	// this file only (a different file invalidates the vault entry).
	const saveEditorSession = useCallback((source: File, session: EditSession) => {
		const vault = editVaultRef.current;
		if (vault.source === source) vault.session = session;
		else editVaultRef.current = { source, session };
	}, []);

	// "Use this image": re-inspect the exported file like any fresh upload
	// (preview + recognition re-run, same reqSeq guard), while remembering
	// which pristine original it came from — the next Edit round reopens the
	// editor over that original with the committed history restored. The
	// editor instance closes (= unmounts) and saved its session into the
	// vault on the way out.
	const applyEdited = useCallback((exported: File) => {
		const original = editOriginal.peek() ?? currentFileRef.current;
		modals.close("edit");
		if (original) void handleFile(exported, original);
	}, [modals, handleFile]);

	// Open the editor over the pristine original (or the fresh upload) —
	// never the previously exported bytes. The session vault rides along:
	// when it holds a session for this file it is restored (initialSession),
	// and the editor saves back into it at close time (onSaveSession).
	const openEditor = useCallback(() => {
		const source = editOriginal.peek() ?? currentFileRef.current;
		if (!source) return;
		const vault = editVaultRef.current;
		modals.open("edit", {
			source,
			initialSession: vault.source === source ? vault.session : null,
			onUse: applyEdited,
			onSaveSession: saveEditorSession,
		});
	}, [modals, applyEdited, saveEditorSession]);

	const onDropzoneClick = () => {
		if (previewURL.value === null) openPicker();
	};
	const onDropzoneKeyDown = (e: TargetedKeyboardEvent<HTMLDivElement>) => {
		if ((e.key === "Enter" || e.key === " ") && previewURL.value === null) {
			e.preventDefault();
			openPicker();
		}
	};
	const onDrop = (e: TargetedDragEvent<HTMLDivElement>) => {
		e.preventDefault();
		drag.value = false;
		const f = e.dataTransfer?.files?.[0];
		if (f) void handleFile(f);
	};

	// Enroll the given 1-based face of the current photo under the typed
	// name; done(ok) lets the row re-enable itself on failure.
	const handleRowEnroll = useCallback((faceNo: number, person: string, done: (ok: boolean) => void) => {
		const file = currentFileRef.current;
		if (!file) { done(false); return; }
		api.enrollFace(person, app.repoId.value ?? "", file, faceNo)
			.then((j) => {
				toast.show(`Enrolled ${person} (${j.saved}).`, "ok");
				enrolled.value = { ...enrolled.value, [faceNo - 1]: person };
				app.refreshAll();
				done(true);
			})
			.catch((err: Error) => {
				toast.show(err.message || "Enrollment failed.", "err");
				done(false);
			});
	}, [toast]);

	// Derived state as computeds; they drive the attribute signals.
	const noPreview = useComputed(() => previewURL.value === null);
	const cursorStyle = useComputed(() => `cursor: ${previewURL.value ? "default" : "pointer"}`);
	const dropzoneClass = useComputed(() => "dropzone" + (drag.value ? " drag" : ""));

	return (
		<section class="panel stage" aria-labelledby="stageTitle">
			<div class="panel-head">
				<h1 id="stageTitle">Inspect a photo</h1>
				<p class="lede">Drop or paste an image to detect every face and match each against the enrolled people.</p>
			</div>

			<div
				id="dropzone"
				class={dropzoneClass}
				style={cursorStyle}
				tabindex={0}
				role="button"
				aria-label="Upload a photo to recognize faces"
				onClick={onDropzoneClick}
				onKeyDown={onDropzoneKeyDown}
				onDragEnter={(e) => { e.preventDefault(); drag.value = true; }}
				onDragOver={(e) => { e.preventDefault(); drag.value = true; }}
				onDragLeave={(e) => { e.preventDefault(); drag.value = false; }}
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
						if (f) void handleFile(f);
						e.currentTarget.value = "";
					}}
				/>
				<Show when={previewURL} fallback={
					<div class="dz-empty" id="dzEmpty">
						<svg viewBox="0 0 48 48" class="dz-icon" aria-hidden="true">
							<rect x="6" y="10" width="36" height="28" rx="3" fill="none" stroke="currentColor" stroke-width="2" />
							<circle cx="19" cy="21" r="4" fill="none" stroke="currentColor" stroke-width="2" />
							<path d="M6 34l10-9 8 7 8-6 10 8" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" />
						</svg>
						<p class="dz-primary">Drop a photo here, or <span class="dz-browse">browse</span></p>
						<p class="dz-hint">JPG, PNG, WebP or BMP — or paste from the clipboard (Ctrl+V)</p>
					</div>
				}>
					{(url) => (
						<div class="dz-preview" id="dzPreview">
							<div class="canvas-wrap">
								<img ref={imgRef} id="previewImg" alt="Uploaded photo preview" src={url} />
								<canvas ref={canvasRef} id="overlay" />
							</div>
						</div>
					)}
				</Show>
			</div>

			<div class="stage-actions">
				{app.perms.value.writer && (
					<>
						<button
							id="editBtn"
							class="btn"
							hidden={noPreview}
							onClick={(e) => {
								e.stopPropagation();
								openEditor();
							}}
						>
							Edit
						</button>
						<button
							id="cancelEditsBtn"
							class="btn btn-ghost"
							hidden={editOriginal.value === null}
							onClick={(e) => {
								e.stopPropagation();
								// Back to the pristine original: re-inspecting it without
								// the edit lineage resets the editor session and re-runs
								// recognition on the untouched bytes.
								const original = editOriginal.value;
								if (original) void handleFile(original);
							}}
						>
							Cancel edits
						</button>
					</>
				)}
				<button id="clearBtn" class="btn btn-ghost" hidden={noPreview} onClick={resetStage}>Clear</button>
				<button
					id="againBtn"
					class="btn btn-ghost"
					hidden={noPreview}
					onClick={(e) => { e.stopPropagation(); openPicker(); }}
				>
					Choose another
				</button>
				<span class="stage-meta" id="stageMeta">{meta}</span>
			</div>

			<Show when={faces} fallback={null}>
				{(list) => (
					<div id="results" class="results">
						<h2 class="results-title"><span id="resultCount">{list.length}</span> face(s) found</h2>
						<ul id="faceList" class="face-list">
							<For each={list}>
								{(f, i) => (
									<FaceRow
										face={f}
										index={i}
										enrolled={enrolled}
										onEnroll={handleRowEnroll}
									/>
								)}
							</For>
						</ul>
					</div>
				)}
			</Show>
		</section>
	);
}

// One face row: index badge, identity, ranked matches (with a duplicate hint
// when the top two scores are nearly tied), and — for unknown faces when the
// viewer may write to this repo — the inline "name this person" enroll form.
// The row subscribes to the store (canEnroll) and the enrolled map itself;
// a re-render of the Stage's own signals does not reach past the row list.
function FaceRow(props: {
	face: Face;
	index: number;
	enrolled: Signal<Record<number, string>>;
	onEnroll: (faceNo: number, person: string, done: (ok: boolean) => void) => void;
}) {
	const app = useApp();
	const f = props.face;
	const enrolledName = props.enrolled.value[props.index];
	const name = enrolledName ?? f.name;
	const known = name !== "unknown";
	const conf = Math.round((f.confidence || 0) * 100);

	const matches = f.matches || [];
	const showMatches = matches.length >= 2;
	const near = showMatches && matches[0].score - matches[1].score <= 0.05;

	const person = useSignal("");
	const busy = useSignal(false);
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
				{!known && app.perms.value.writer && (
					<form
						class="face-enroll"
						onSubmit={(e) => {
							e.preventDefault();
							const p = person.value.trim();
							if (!p) { inputRef.current?.focus(); return; }
							busy.value = true;
							props.onEnroll(props.index + 1, p, (ok) => {
								if (!ok) {
									busy.value = false;
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
							onInput={(e) => { person.value = e.currentTarget.value; }}
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
