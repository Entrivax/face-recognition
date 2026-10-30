// Photos manager modal.
// Opens from the person details modal (writers, via the modal service — the
// details modal stays mounted underneath, paused): the grid lists their
// enrolled photos (remove per tile, add via drop/paste/browse); clicking a
// tile opens the detail view, which runs detection on the stored photo and
// draws the faces over it so the user can judge the photo's quality. The
// header carries an inline rename editor; saving renames everything
// server-side and patches both open modals' person (the photos manager
// reloads under the new name, the details modal follows).
//
// Any two enrolled photos of this person can be compared face-to-face: each
// grid tile has a compare button (the detail view has a "Compare faces…"
// button) — the first pick marks the photo (A), the second pick loads both
// photos and opens the compare modal seeded, auto-running the similarity
// check.
//
// All component-local state is signals. The props mirror (props are not
// signals) lets imperative closures see the person the modal currently
// manages, and every handler reads signal values at call time — so no
// handler can act on a stale render's detail view, rename edit or compare
// pick (the former detailRef/busyRef mirrors are gone: detailPath and busy
// are read directly, .peek() where a guard must not subscribe).

import { useEffect, useRef } from "preact/hooks";
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals";
import type { ReadonlySignal } from "@preact/signals";
import { For, Show } from "@preact/signals/utils";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { Face, PersonPhoto } from "../types";
import { drawFaces } from "../overlay";
import { useToast } from "../toast";
import { useApp } from "../appStore";
import { useModals } from "../modalRegistry";
import { usePasteFiles } from "../paste";
import { Modal } from "./Modal";

interface PhotosModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service: true while no other instance sits above */
	isTopmost: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
	/** the person being managed */
	person: string;
}

export function PhotosModal(props: PhotosModalProps) {
	const toast = useToast();
	const app = useApp();
	const modals = useModals();
	const { open, isTopmost, onCloseRequest } = props;

	// Component-local state as signals.
	const photos = useSignal<PersonPhoto[] | null>(null);
	const hint = useSignal("Loading photos…");
	// detailPath doubles as the former detailRef mirror: async continuations
	// read detailPath.peek() to detect that the detail view moved on.
	const detailPath = useSignal<string | null>(null);
	const verdict = useSignal("");
	const verdictWarn = useSignal(false);
	const busy = useSignal(false);
	const renaming = useSignal(false);
	const renameVal = useSignal("");
	const dropHint = useSignal(false);
	// Enrolled photo currently marked as photo A of a comparison (path).
	const cmpPath = useSignal<string | null>(null);

	// The person being managed, mirrored from props (props are not signals):
	// imperative closures (paste routing, uploads, compare launches) read it
	// at call time.
	const personRef = useRef(props.person);
	personRef.current = props.person;

	const detailImgRef = useRef<HTMLImageElement | null>(null);
	const detailCanvasRef = useRef<HTMLCanvasElement | null>(null);
	const addInputRef = useRef<HTMLInputElement | null>(null);
	const renameInputRef = useRef<HTMLInputElement | null>(null);

	// Open: drop any detail view and load the grid.
	useEffect(() => {
		batch(() => {
			detailPath.value = null;
			renaming.value = false;
			cmpPath.value = null;
			photos.value = null;
			hint.value = "Loading photos…";
		})
		loadGrid(props.person);
	}, [props.person]);

	// ---- paste routing (via the injected isTopmost flag) ----
	// While this modal is topmost, pasted images upload straight into that
	// person's profile — but not while the detail view is showing (the add
	// grid is gone).
	usePasteFiles(isTopmost, (files) => {
		if (busy.value || !personRef.current || detailPath.peek() !== null) return;
		addPhotos(files);
	});

	const close = () => {
		if (busy.value) return; // locked while a request is in flight
		onCloseRequest();
	};

	// Escape cancels the inline rename edit, then a pending compare pick,
	// then closes. All reads are call-time signal reads — no handler can act
	// on a stale render's state.
	const onEscape = () => {
		if (renaming.value) cancelRename();
		else if (cmpPath.value) cmpPath.value = null;
		else close();
	};

	// ---- grid ----
	async function loadGrid(name: string) {
		hint.value = "Loading photos…";
		try {
			const j = await api.getPersonPhotos(name, app.repoId.value ?? "");
			const list = j.photos || [];
			batch(() => {
				photos.value = list;
				hint.value = list.length
					? "Click a photo to see the detected faces. Drop, paste, or use the box below to add more."
					: "No photos enrolled for this person yet — add some below.";
			})
		} catch (e) {
			hint.value = (e as Error).message || "Could not load photos.";
		}
	}

	// ---- rename ----
	// The header's Rename button swaps the person's name for an inline editor.
	// Saving renames everything server-side: the people/ folder, the person's
	// ID, the thumbnail sidecar and the DB record.
	function openRename() {
		if (busy.value || renaming.value || !personRef.current) return;
		batch(() => {
			renameVal.value = personRef.current;
			renaming.value = true;
		})
	}

	useSignalEffect(() => {
		if (renaming.value) {
			renameInputRef.current?.focus();
			renameInputRef.current?.select();
		}
	});

	function cancelRename() {
		renaming.value = false;
	}

	const submitRename = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy.value || !renaming.value || !personRef.current) return;
		const oldName = personRef.current;
		const newName = renameVal.value.trim();
		if (!newName) {
			toast.show("Enter the person's new name.", "err");
			renameInputRef.current?.focus();
			return;
		}
		if (newName === oldName) { cancelRename(); return; }

		busy.value = true;
		try {
			const j = await api.renamePerson(oldName, app.repoId.value ?? "", newName);
			const canonical = j.name || newName; // DB-canonical name
			// Photo URLs embed the person's name: drop any open detail view and
			// reload the grid under the new name (the person-args effect does
			// it), and let the details modal underneath follow the person too.
			detailPath.value = null;
			cancelRename();
			modals.patch("photos", { person: canonical });
			modals.patch("person", { name: canonical }, (cur) => cur?.name === oldName);
			toast.show(`Renamed to ${canonical}.`, "ok");
			app.refreshAll(); // refresh people list + health
		} catch (err) {
			toast.show((err as Error).message || "Could not rename.", "err");
		} finally {
			busy.value = false;
		}
	};

	// ---- detail view ----
	// Load the photo, detect its faces, draw the overlay and a plain-language
	// verdict. Responses for a photo the user already left behind are dropped.
	async function openDetail(photoPath: string) {
		const person = personRef.current;
		if (!person) return;
		batch(() => {
			detailPath.value = photoPath;
			verdictWarn.value = false;
			verdict.value = "Detecting faces…";
		})
		// Clear any boxes left over from a previously viewed photo.
		const c = detailCanvasRef.current;
		c?.getContext("2d")?.clearRect(0, 0, c.width, c.height);
		try {
			const j = await api.detectPhoto(person, app.repoId.value ?? "", photoPath);
			if (detailPath.peek() !== photoPath) return; // view moved on
			renderVerdict(j.faces || [], photoPath);
		} catch (e) {
			if (detailPath.peek() === photoPath) {
				batch(() => {
					verdict.value = (e as Error).message || "Could not analyze this photo.";
					verdictWarn.value = true;
				})
			}
		}
	}

	function renderVerdict(faces: Face[], photoPath: string) {
		// Overlay: draw once the image has dimensions.
		const img = detailImgRef.current;
		const canvas = detailCanvasRef.current;
		if (img && canvas) {
			const draw = () => {
				if (detailPath.peek() !== photoPath) return; // view moved on
				drawFaces(canvas, img, faces, { labels: true });
			};
			if (img.complete && img.naturalWidth) draw();
			else img.onload = draw;
		}

		verdictWarn.value = false;
		if (!faces.length) {
			batch(() => {
				verdict.value = "No face detected — this photo contributes nothing to recognition.";
				verdictWarn.value = true;
			})
			return;
		}
		const parts = faces.map((f) => {
			const conf = Math.round((f.confidence || 0) * 100);
			return f.name && f.name !== "unknown"
				? `${f.name} ${conf}%`
				: `unknown (best match under threshold)`;
		});
		const n = faces.length;
		const lead = n === 1 ? "1 face detected" : `${n} faces detected — enrollment uses the largest`;
		verdict.value = `${lead} · ${parts.join(", ")}`;
	}

	function backToGrid() {
		detailPath.value = null;
	}

	// ---- compare two enrolled photos ----
	// First pick marks the photo as photo A; picking the marked photo again
	// cancels; picking a different photo loads both photos' bytes and hands
	// them to the compare modal, which fills its slots and auto-runs the
	// similarity check. Works from the grid's per-tile buttons and from the
	// detail view's "Compare faces…" button alike.
	function pickComparePath(path: string) {
		if (busy.value) return;
		if (!cmpPath.value) {
			cmpPath.value = path;
			toast.show(`${path} set as photo A — pick another photo to compare.`, "ok");
			return;
		}
		if (cmpPath.value === path) {
			cmpPath.value = null;
			return;
		}
		launchCompare(cmpPath.value, path);
	}

	async function launchCompare(aPath: string, bPath: string) {
		const person = personRef.current;
		if (busy.value || !person) return;
		busy.value = true;
		try {
			const [a, b] = await Promise.all([
				api.fetchPhotoFile(person, app.repoId.value ?? "", aPath),
				api.fetchPhotoFile(person, app.repoId.value ?? "", bPath),
			]);
			cmpPath.value = null;
			// Stacked on top of this modal (which stays open underneath).
			modals.open("compare", { seed: { a, b } });
		} catch (e) {
			cmpPath.value = null;
			toast.show((e as Error).message || "Could not load the photos for comparison.", "err");
		} finally {
			busy.value = false;
		}
	}

	// Detail-view trigger: a fresh pick marks this photo as A and shows the
	// grid so another photo can be picked; with A already marked it follows
	// the usual rule (click the marked photo again = cancel, otherwise launch).
	function compareFromDetail() {
		const path = detailPath.value;
		if (busy.value || !path) return;
		if (cmpPath.value && cmpPath.value !== path) {
			launchCompare(cmpPath.value, path);
			return;
		}
		if (!cmpPath.value) {
			cmpPath.value = path;
			backToGrid();
			toast.show(`${path} set as photo A — pick another photo to compare.`, "ok");
			return;
		}
		cmpPath.value = null; // the detail photo was the marked one — cancel
	}

	async function deletePhoto(photoPath: string) {
		const person = personRef.current;
		if (busy.value || !person) return;
		if (!confirm(`Remove ${photoPath} from ${person}? The file is deleted from the people folder too.`)) return;
		busy.value = true;
		try {
			await api.deletePhoto(person, app.repoId.value ?? "", photoPath);
			toast.show(`Removed ${photoPath}.`, "ok");
			if (detailPath.peek() === photoPath) {
				detailPath.value = null; // the photo under inspection is gone — back to the grid
			}
			if (cmpPath.value === photoPath) cmpPath.value = null; // was marked for comparison
			loadGrid(person);
			app.refreshAll();
		} catch (e) {
			toast.show((e as Error).message || "Could not remove the photo.", "err");
		} finally {
			busy.value = false;
		}
	}

	async function setAsAvatar() {
		const person = personRef.current;
		if (busy.value || !person || !detailPath.value) return;
		busy.value = true;
		try {
			await api.setThumbnail(person, app.repoId.value ?? "", detailPath.value);
			toast.show("Avatar updated.", "ok");
			app.refreshAll();
		} catch (e) {
			toast.show((e as Error).message || "Could not update the avatar.", "err");
		} finally {
			busy.value = false;
		}
	}

	// ---- add photos ----
	// Upload the chosen files into the person's profile via the enroll
	// endpoint; only successfully enrolled photos land in the grid.
	async function addPhotos(fileList: FileList | File[]) {
		const person = personRef.current;
		const files = Array.from(fileList).filter((f) => f.type.startsWith("image/"));
		if (!files.length) { toast.show("No image files to add.", "err"); return; }
		if (busy.value || !person) return;
		batch(() => {
			busy.value = true;
			hint.value = `Uploading ${files.length} photo${files.length === 1 ? "" : "s"}…`;
		})
		try {
			const j = await api.enrollPhotos(person, app.repoId.value ?? "", files);
			if (j.added > 0) toast.show(`Added ${j.added} photo${j.added === 1 ? "" : "s"}.`, "ok");
			if (j.failures && j.failures.length) {
				toast.show(`${j.failures.length} photo(s) rejected: ${j.failures[0]}`, "err");
			}
			app.refreshAll();
		} catch (e) {
			toast.show((e as Error).message || "Upload failed.", "err");
		} finally {
			busy.value = false;
			if (personRef.current) loadGrid(personRef.current);
		}
	}

	// Derived state as computeds; they drive the attribute signals.
	const canDrop = useComputed(() => !busy.value && detailPath.value === null);
	const addBoxClass = useComputed(() => "photo-add" + (dropHint.value ? " drag" : ""));
	const hintClass = useComputed(() => "field-hint" + (cmpPath.value ? " cmp-hint" : ""));
	// While a photo is marked for comparison the hint line guides the pick.
	const hintShown = useComputed(() => cmpPath.value
		? `Pick another photo to compare with "${cmpPath.value}" — or click its compare button again to cancel.`
		: hint.value);
	const verdictClass = useComputed(() => "photo-verdict" + (verdictWarn.value ? " warn" : ""));
	const renameFormHidden = useComputed(() => !renaming.value);
	const gridPhotos = useComputed(() => photos.value ?? []);

	return (
		<Modal
			open={open}
			id="photoModal"
			cardId="photoCard"
			cardClass="wide"
			locked={busy}
			eyebrow="photos"
			titleId="photoTitle"
			title={props.person}
			headerExtras={
				<form
					id="photoRenameForm"
					class="rename-row"
					hidden={renameFormHidden}
					onSubmit={submitRename}
				>
					<input
						ref={renameInputRef}
						id="photoRenameInput"
						type="text"
						maxLength={120}
						autocomplete="off"
						spellcheck={false}
						aria-label="New name"
						disabled={busy}
						value={renameVal}
						onInput={(e) => { renameVal.value = e.currentTarget.value; }}
					/>
					<button type="submit" class="btn btn-accent btn-sm" id="photoRenameSave" disabled={busy}>Save</button>
					<button type="button" class="btn btn-ghost btn-sm" id="photoRenameCancel" onClick={cancelRename}>Cancel</button>
				</form>
			}
			headerActions={
				<button
					type="button"
					class="btn btn-ghost btn-sm"
					id="photoRenameBtn"
					hidden={renaming}
					onClick={openRename}
				>
					Rename
				</button>
			}
			onEscape={onEscape}
			onCloseRequest={close}
			rootProps={{
				onDragEnter: (e) => { e.preventDefault(); if (canDrop.value) dropHint.value = true; },
				onDragOver: (e) => { e.preventDefault(); if (canDrop.value) dropHint.value = true; },
				onDragLeave: (e) => {
					if (!(e as DragEvent).relatedTarget) dropHint.value = false;
				},
				onDrop: (e) => {
					e.preventDefault();
					dropHint.value = false;
					if (canDrop.value && e.dataTransfer?.files?.length) addPhotos(e.dataTransfer.files);
				},
			}}
			body={
				<div class="modal-body">
					<p class={hintClass} id="photoHint">{hintShown}</p>

					{/* grid view (fallback) ↔ detail view (when a photo is open) */}
					<Show
						when={detailPath}
						fallback={
							<div id="photoGridView">
								<ul id="photoGrid" class="photo-grid">
									<For each={gridPhotos} getKey={(ph) => ph.path}>
										{(ph) => (
											<PhotoTile
												person={props.person}
												repoId={app.repoId.value ?? ""}
												ph={ph}
												cmpPath={cmpPath}
												onOpen={() => openDetail(ph.path)}
												onDelete={() => deletePhoto(ph.path)}
												onCompare={() => pickComparePath(ph.path)}
											/>
										)}
									</For>
								</ul>
								<div
									id="photoAdd"
									class={addBoxClass}
									tabindex={0}
									role="button"
									aria-label="Add photos — drag and drop, paste, or press Enter to browse"
									onClick={() => { if (!busy.value) addInputRef.current?.click(); }}
									onKeyDown={(e) => {
										if ((e.key === "Enter" || e.key === " ") && !busy.value) {
											e.preventDefault();
											addInputRef.current?.click();
										}
									}}
								>
									<p class="pa-primary">Drag &amp; drop, paste, or <span class="pa-browse">browse</span> to add photos</p>
									<p class="pa-hint">Photos upload immediately and become part of this person's dataset</p>
									<input
										ref={addInputRef}
										id="photoFiles"
										type="file"
										accept="image/*"
										multiple
										hidden
										onChange={(e) => {
											if (e.currentTarget.files?.length) addPhotos(e.currentTarget.files);
											e.currentTarget.value = "";
										}}
									/>
								</div>
							</div>
						}
					>
						{(path) => (
							<div id="photoDetailView">
								<button type="button" class="photo-back" id="photoBackBtn" onClick={backToGrid}>← All photos</button>
								<div class="canvas-wrap photo-canvas">
									<img
										ref={detailImgRef}
										id="photoDetailImg"
										alt="Enrolled photo"
										src={api.withRepoQuery(api.photoURL(props.person, path), app.repoId.value ?? "")}
									/>
									<canvas ref={detailCanvasRef} id="photoOverlay" />
								</div>
								<p class={verdictClass} id="photoVerdict" aria-live="polite">{verdict}</p>
								<div class="photo-actions">
									<button type="button" class="btn btn-ghost" id="photoThumbBtn" onClick={setAsAvatar}>Set as avatar</button>
									<button
										type="button"
										class="btn btn-ghost"
										id="photoCompareBtn"
										title={cmpPath.value === path
											? "Photo A selected — click again to cancel"
											: "Compare this photo with another enrolled photo"}
										onClick={compareFromDetail}
									>
										Compare faces…
									</button>
									<button type="button" class="btn btn-ghost photo-remove" id="photoDelBtn" onClick={() => deletePhoto(path)}>Remove photo</button>
								</div>
							</div>
						)}
					</Show>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

// One grid tile; a photo missing from the people folder (legacy DB entry)
// degrades to a disabled "unavailable" tile. The compare button marks the
// photo as photo A of a face-to-face comparison (second pick launches).
function PhotoTile(props: {
	person: string;
	repoId: string;
	ph: PersonPhoto;
	/** the photo currently marked as comparison photo A (read here, so a
	 *  cmpPath change re-renders only the affected tiles) */
	cmpPath: ReadonlySignal<string | null>;
	onOpen: () => void;
	onDelete: () => void;
	onCompare: () => void;
}) {
	const broken = useSignal(false);
	const ph = props.ph;
	const cmpMark = props.cmpPath.value === ph.path;
	return (
		<li class={"photo-tile" + (cmpMark ? " cmp-mark" : "")}>
			<button
				type="button"
				class="photo-tile"
				title={`${ph.path} — click to inspect faces`}
				disabled={broken}
				onClick={props.onOpen}
			>
				{broken.value
					? "unavailable"
					// The photo route is repo-scoped (?repo= is required), so the
					// URL must carry the repository the person lives in.
					: <img alt={ph.path} src={api.withRepoQuery(api.photoURL(props.person, ph.path), props.repoId)} onError={() => broken.value = true} />}
			</button>
			<button
				type="button"
				class="photo-tile-cmp"
				aria-label={`Compare ${ph.path} with another photo`}
				aria-pressed={cmpMark}
				title={cmpMark
					? "Photo A selected — click again to cancel, or pick another photo"
					: "Compare with another photo"}
				onClick={(e) => { e.stopPropagation(); props.onCompare(); }}
			>
				<svg viewBox="0 0 48 48" aria-hidden="true">
					<path
						d="M15 8h-5a2 2 0 0 0-2 2v5M33 8h5a2 2 0 0 1 2 2v5M15 40h-5a2 2 0 0 1-2-2v-5M33 40h5a2 2 0 0 0 2-2v-5"
						fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round"
					/>
					<circle cx="24" cy="21" r="5" fill="none" stroke="currentColor" stroke-width="2.6" />
					<path
						d="M15 36c2-4.5 5.4-6 9-6s7 1.6 9 6"
						fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"
					/>
				</svg>
			</button>
			{cmpMark && <span class="cmp-badge">A</span>}
			<button
				type="button"
				class="photo-tile-del"
				aria-label={`Remove ${ph.path}`}
				onClick={(e) => { e.stopPropagation(); props.onDelete(); }}
			>
				×
			</button>
		</li>
	);
}
