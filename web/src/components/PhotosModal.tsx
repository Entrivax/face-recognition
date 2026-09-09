// Photos manager modal.
// Opens from a person's row or avatar. Grid view lists their enrolled photos
// (remove per tile, add via drop/paste/browse); clicking a tile opens the
// detail view, which runs detection on the stored photo and draws the faces
// over it so the user can judge the photo's quality. The header carries an
// inline rename editor; saving renames everything server-side.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { Face, PersonPhoto } from "../types";
import { drawFaces } from "../overlay";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface PhotosModalProps {
	/** name of the person being managed; null = closed */
	person: string | null;
	onCloseRequest: () => void;
	/** the person was renamed server-side; App updates the open person */
	onRenamed: (name: string) => void;
	/** photos added/removed or avatar changed → refresh people + health */
	onChange: () => void;
	registerAddFiles: (fn: ((files: FileList | File[]) => void) | null) => void;
}

export function PhotosModal(props: PhotosModalProps) {
	const toast = useToast();

	const [photos, setPhotos] = useState<PersonPhoto[] | null>(null);
	const [hint, setHint] = useState("Loading photos…");
	const [detailPath, setDetailPath] = useState<string | null>(null);
	const [verdict, setVerdict] = useState("");
	const [verdictWarn, setVerdictWarn] = useState(false);
	const [busy, setBusyState] = useState(false);
	const [renaming, setRenaming] = useState(false);
	const [renameVal, setRenameVal] = useState("");
	const [dropHint, setDropHint] = useState(false);

	const personRef = useRef<string | null>(props.person);
	const detailRef = useRef<string | null>(null);
	const busyRef = useRef(false);
	const detailImgRef = useRef<HTMLImageElement | null>(null);
	const detailCanvasRef = useRef<HTMLCanvasElement | null>(null);
	const addInputRef = useRef<HTMLInputElement | null>(null);
	const renameInputRef = useRef<HTMLInputElement | null>(null);

	// Keep the ref in sync for imperative closures (click handlers, async
	// continuations that must observe "which person/detail is current now").
	useEffect(() => {
		personRef.current = props.person;
	}, [props.person]);

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	// Open / person switch: drop any detail view and load the grid.
	useEffect(() => {
		if (!props.person) return;
		detailRef.current = null;
		setDetailPath(null);
		setRenaming(false);
		setPhotos(null);
		setHint("Loading photos…");
		loadGrid(props.person);
	}, [props.person]);

	// Paste routing surface (registered with App).
	useEffect(() => {
		props.registerAddFiles((files) => {
			if (busyRef.current || !personRef.current || detailRef.current !== null) return;
			addPhotos(files);
		});
		return () => props.registerAddFiles(null);
	}, []);

	const open = props.person !== null;

	const close = () => {
		if (busyRef.current) return; // locked while a request is in flight
		props.onCloseRequest();
	};

	// Escape cancels the inline rename edit first, then closes.
	const onEscape = () => {
		if (renaming) cancelRename();
		else close();
	};

	// ---- grid ----
	async function loadGrid(name: string) {
		setHint("Loading photos…");
		try {
			const j = await api.getPerson(name);
			const list = j.photos || [];
			setPhotos(list);
			setHint(list.length
				? "Click a photo to see the detected faces. Drop, paste, or use the box below to add more."
				: "No photos enrolled for this person yet — add some below.");
		} catch (e) {
			setHint((e as Error).message || "Could not load photos.");
		}
	}

	// ---- rename ----
	// The header's Rename button swaps the person's name for an inline editor.
	// Saving renames everything server-side: the people/ folder, the person's
	// ID, the thumbnail sidecar and the DB record.
	function openRename() {
		if (busyRef.current || renaming || !personRef.current) return;
		setRenameVal(personRef.current);
		setRenaming(true);
	}

	useEffect(() => {
		if (renaming) {
			renameInputRef.current?.focus();
			renameInputRef.current?.select();
		}
	}, [renaming]);

	function cancelRename() {
		setRenaming(false);
	}

	const submitRename = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current || !renaming || !personRef.current) return;
		const oldName = personRef.current;
		const newName = renameVal.trim();
		if (!newName) {
			toast.show("Enter the person's new name.", "err");
			renameInputRef.current?.focus();
			return;
		}
		if (newName === oldName) { cancelRename(); return; }

		setBusy(true);
		try {
			const j = await api.renamePerson(oldName, newName);
			const canonical = j.name || newName; // DB-canonical name
			// Photo URLs embed the person's name: drop any open detail view and
			// reload the grid under the new name (the person-prop effect does it).
			detailRef.current = null;
			setDetailPath(null);
			cancelRename();
			props.onRenamed(canonical);
			toast.show(`Renamed to ${canonical}.`, "ok");
			props.onChange(); // refresh people list + health
		} catch (err) {
			toast.show((err as Error).message || "Could not rename.", "err");
		} finally {
			setBusy(false);
		}
	};

	// ---- detail view ----
	// Load the photo, detect its faces, draw the overlay and a plain-language
	// verdict. Responses for a photo the user already left behind are dropped.
	async function openDetail(photoPath: string) {
		const person = personRef.current;
		if (!person) return;
		detailRef.current = photoPath;
		setDetailPath(photoPath);
		setVerdictWarn(false);
		setVerdict("Detecting faces…");
		// Clear any boxes left over from a previously viewed photo.
		const c = detailCanvasRef.current;
		c?.getContext("2d")?.clearRect(0, 0, c.width, c.height);
		try {
			const j = await api.detectPhoto(person, photoPath);
			if (detailRef.current !== photoPath) return; // view moved on
			renderVerdict(j.faces || [], photoPath);
		} catch (e) {
			if (detailRef.current === photoPath) {
				setVerdict((e as Error).message || "Could not analyze this photo.");
				setVerdictWarn(true);
			}
		}
	}

	function renderVerdict(faces: Face[], photoPath: string) {
		// Overlay: draw once the image has dimensions.
		const img = detailImgRef.current;
		const canvas = detailCanvasRef.current;
		if (img && canvas) {
			const draw = () => {
				if (detailRef.current !== photoPath) return; // view moved on
				drawFaces(canvas, img, faces, { labels: true });
			};
			if (img.complete && img.naturalWidth) draw();
			else img.onload = draw;
		}

		setVerdictWarn(false);
		if (!faces.length) {
			setVerdict("No face detected — this photo contributes nothing to recognition.");
			setVerdictWarn(true);
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
		setVerdict(`${lead} · ${parts.join(", ")}`);
	}

	function backToGrid() {
		detailRef.current = null;
		setDetailPath(null);
	}

	async function deletePhoto(photoPath: string) {
		const person = personRef.current;
		if (busyRef.current || !person) return;
		if (!confirm(`Remove ${photoPath} from ${person}? The file is deleted from the people folder too.`)) return;
		setBusy(true);
		try {
			await api.deletePhoto(person, photoPath);
			toast.show(`Removed ${photoPath}.`, "ok");
			if (detailRef.current === photoPath) {
				detailRef.current = null;
				setDetailPath(null); // the photo under inspection is gone — back to the grid
			}
			loadGrid(person);
			props.onChange();
		} catch (e) {
			toast.show((e as Error).message || "Could not remove the photo.", "err");
		} finally {
			setBusy(false);
		}
	}

	async function setAsAvatar() {
		const person = personRef.current;
		if (busyRef.current || !person || !detailRef.current) return;
		setBusy(true);
		try {
			await api.setThumbnail(person, detailRef.current);
			toast.show("Avatar updated.", "ok");
			props.onChange();
		} catch (e) {
			toast.show((e as Error).message || "Could not update the avatar.", "err");
		} finally {
			setBusy(false);
		}
	}

	// ---- add photos ----
	// Upload the chosen files into the person's profile via the enroll
	// endpoint; only successfully enrolled photos land in the grid.
	async function addPhotos(fileList: FileList | File[]) {
		const person = personRef.current;
		const files = Array.from(fileList).filter((f) => f.type.startsWith("image/"));
		if (!files.length) { toast.show("No image files to add.", "err"); return; }
		if (busyRef.current || !person) return;
		setBusy(true);
		setHint(`Uploading ${files.length} photo${files.length === 1 ? "" : "s"}…`);
		try {
			const j = await api.enrollPhotos(person, files);
			if (j.added > 0) toast.show(`Added ${j.added} photo${j.added === 1 ? "" : "s"}.`, "ok");
			if (j.failures && j.failures.length) {
				toast.show(`${j.failures.length} photo(s) rejected: ${j.failures[0]}`, "err");
			}
			props.onChange();
		} catch (e) {
			toast.show((e as Error).message || "Upload failed.", "err");
		} finally {
			setBusy(false);
			if (personRef.current) loadGrid(personRef.current);
		}
	}

	const canDrop = !busy && detailPath === null;

	return (
		<Modal
			open={open}
			id="photoModal"
			cardId="photoCard"
			cardClass="wide"
			locked={busy}
			eyebrow="photos"
			titleId="photoTitle"
			title={props.person ?? ""}
			headerExtras={
				<form
					id="photoRenameForm"
					class="rename-row"
					hidden={!renaming}
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
						onInput={(e) => setRenameVal(e.currentTarget.value)}
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
				onDragEnter: (e) => { e.preventDefault(); if (canDrop) setDropHint(true); },
				onDragOver: (e) => { e.preventDefault(); if (canDrop) setDropHint(true); },
				onDragLeave: (e) => {
					if (!(e as DragEvent).relatedTarget) setDropHint(false);
				},
				onDrop: (e) => {
					e.preventDefault();
					setDropHint(false);
					if (canDrop && e.dataTransfer?.files?.length) addPhotos(e.dataTransfer.files);
				},
			}}
			body={
				<div class="modal-body">
					<p class="field-hint" id="photoHint">{hint}</p>

					{/* grid view */}
					{detailPath === null && (
						<div id="photoGridView">
							<ul id="photoGrid" class="photo-grid">
								{(photos ?? []).map((ph) => (
									<PhotoTile
										key={ph.path}
										person={props.person ?? ""}
										ph={ph}
										onOpen={() => openDetail(ph.path)}
										onDelete={() => deletePhoto(ph.path)}
									/>
								))}
							</ul>
							<div
								id="photoAdd"
								class={"photo-add" + (dropHint ? " drag" : "")}
								tabindex={0}
								role="button"
								aria-label="Add photos — drag and drop, paste, or press Enter to browse"
								onClick={() => { if (!busy) addInputRef.current?.click(); }}
								onKeyDown={(e) => {
									if ((e.key === "Enter" || e.key === " ") && !busy) {
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
					)}

					{/* detail view: photo with detected faces drawn over it */}
					{detailPath !== null && (
						<div id="photoDetailView">
							<button type="button" class="photo-back" id="photoBackBtn" onClick={backToGrid}>← All photos</button>
							<div class="canvas-wrap photo-canvas">
								<img ref={detailImgRef} id="photoDetailImg" alt="Enrolled photo" src={api.photoURL(props.person ?? "", detailPath)} />
								<canvas ref={detailCanvasRef} id="photoOverlay" />
							</div>
							<p class={"photo-verdict" + (verdictWarn ? " warn" : "")} id="photoVerdict" aria-live="polite">{verdict}</p>
							<div class="photo-actions">
								<button type="button" class="btn btn-ghost" id="photoThumbBtn" onClick={setAsAvatar}>Set as avatar</button>
								<button type="button" class="btn btn-ghost photo-remove" id="photoDelBtn" onClick={() => detailPath && deletePhoto(detailPath)}>Remove photo</button>
							</div>
						</div>
					)}

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

// One grid tile; a photo missing from the people folder (legacy DB entry)
// degrades to a disabled "unavailable" tile.
function PhotoTile(props: {
	person: string;
	ph: PersonPhoto;
	onOpen: () => void;
	onDelete: () => void;
}) {
	const [broken, setBroken] = useState(false);
	const ph = props.ph;
	return (
		<li class="photo-tile">
			<button
				type="button"
				class="photo-tile"
				title={`${ph.path} — click to inspect faces`}
				disabled={broken}
				onClick={props.onOpen}
			>
				{broken
					? "unavailable"
					: <img alt={ph.path} src={api.photoURL(props.person, ph.path)} onError={() => setBroken(true)} />}
			</button>
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
