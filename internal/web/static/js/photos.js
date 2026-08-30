/* recogn — photos manager modal.
	 Opens from a person's row or avatar. Grid view lists their enrolled
	 photos (remove per tile, add via drop/paste/browse); clicking a tile opens
	 the detail view, which runs detection on the stored photo and draws the
	 faces over it so the user can judge the photo's quality. */

import { el } from "./dom.js";
import { showToast } from "./util.js";
import * as api from "./api.js";
import { drawWhenReady } from "./overlay.js";

let photoPerson = null;   // name of the person being managed
let photoDetail = null;   // { path } currently shown in the detail view
let photoBusy = false;
let photoLastFocus = null;

// Called whenever photos are added/removed or the avatar changes, so main.js
// can reload the people list and health without this module importing them.
let onChange = () => {};
export function onPhotosChange(fn) { onChange = fn; }

export function isOpen() { return !el.thumbModal.hidden; }

export async function openPhotosModal(name) {
	photoLastFocus = document.activeElement;
	photoPerson = name;
	el.photoTitle.textContent = name;
	el.photoHint.textContent = "Loading photos…";
	showPhotoGrid();
	el.photoGrid.innerHTML = "";
	el.thumbModal.hidden = false;
	document.body.classList.add("modal-open");
	el.photoCard.querySelector(".modal-close").focus();
	await loadPhotoGrid(name);
}

export function closePhotosModal() {
	if (photoBusy) return; // locked while a request is in flight
	el.thumbModal.hidden = true;
	document.body.classList.remove("modal-open");
	el.photoGrid.innerHTML = "";
	photoPerson = null;
	photoDetail = null;
	if (photoLastFocus && photoLastFocus.focus) photoLastFocus.focus();
}

function showPhotoDetail(state) {
	el.photoDetailView.hidden = !state;
	el.photoGridView.hidden = Boolean(state);
}

function showPhotoGrid() {
	showPhotoDetail(false);
}

async function loadPhotoGrid(name) {
	el.photoHint.textContent = "Loading photos…";
	try {
		const j = await api.getPerson(name);
		renderPhotoTiles(j.photos || []);
	} catch (e) {
		el.photoHint.textContent = e.message || "Could not load photos.";
	}
}

function renderPhotoTiles(photos) {
	el.photoGrid.innerHTML = "";
	el.photoHint.textContent = photos.length
		? "Click a photo to see the detected faces. Drop, paste, or use the box below to add more."
		: "No photos enrolled for this person yet — add some below.";
	for (const ph of photos) {
		const li = document.createElement("li");
		li.className = "photo-tile";
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "photo-tile";
		btn.title = `${ph.path} — click to inspect faces`;
		const img = document.createElement("img");
		img.alt = ph.path;
		img.src = api.photoURL(photoPerson, ph.path);
		img.addEventListener("error", () => {
			// File gone from the people folder (e.g. legacy DB entry).
			li.classList.add("unavailable");
			btn.disabled = true;
			btn.textContent = "unavailable";
			img.remove();
		});
		btn.appendChild(img);
		btn.addEventListener("click", () => openPhotoDetail(ph.path));

		const del = document.createElement("button");
		del.type = "button";
		del.className = "photo-tile-del";
		del.textContent = "×";
		del.setAttribute("aria-label", `Remove ${ph.path}`);
		del.addEventListener("click", (e) => { e.stopPropagation(); deletePhoto(ph.path); });

		li.append(btn, del);
		el.photoGrid.appendChild(li);
	}
}

// Detail view: load the photo, detect its faces, draw the overlay and a
// plain-language verdict.
async function openPhotoDetail(photoPath) {
	if (!photoPerson) return;
	photoDetail = { path: photoPath };
	el.photoVerdict.classList.remove("warn");
	el.photoVerdict.textContent = "Detecting faces…";
	// Clear any boxes left over from a previously viewed photo.
	el.photoOverlay.getContext("2d").clearRect(0, 0, el.photoOverlay.width, el.photoOverlay.height);
	el.photoDetailImg.src = api.photoURL(photoPerson, photoPath);
	showPhotoDetail(true);
	el.photoBackBtn.focus();

	try {
		const j = await api.detectPhoto(photoPerson, photoPath);
		if (!photoDetail || photoDetail.path !== photoPath) return; // view moved on
		renderPhotoVerdict(j.faces || []);
	} catch (e) {
		if (photoDetail && photoDetail.path === photoPath) {
			el.photoVerdict.textContent = e.message || "Could not analyze this photo.";
			el.photoVerdict.classList.add("warn");
		}
	}
}

function renderPhotoVerdict(faces) {
	drawWhenReady(el.photoOverlay, el.photoDetailImg, faces, { labels: true });

	el.photoVerdict.classList.remove("warn");
	if (!faces.length) {
		el.photoVerdict.textContent = "No face detected — this photo contributes nothing to recognition.";
		el.photoVerdict.classList.add("warn");
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
	el.photoVerdict.textContent = `${lead} · ${parts.join(", ")}`;
}

async function deletePhoto(photoPath) {
	if (photoBusy || !photoPerson) return;
	if (!confirm(`Remove ${photoPath} from ${el.photoTitle.textContent}? The file is deleted from the people folder too.`)) return;
	photoBusy = true;
	el.thumbModal.classList.add("locked");
	try {
		await api.deletePhoto(photoPerson, photoPath);
		showToast(`Removed ${photoPath}.`, "ok");
		if (photoDetail && photoDetail.path === photoPath) {
			photoDetail = null;
			showPhotoGrid(); // the photo under inspection is gone — back to the grid
		}
		loadPhotoGrid(photoPerson);
		onChange();
	} catch (e) {
		showToast(e.message || "Could not remove the photo.", "err");
	} finally {
		photoBusy = false;
		el.thumbModal.classList.remove("locked");
	}
}

el.photoBackBtn.addEventListener("click", () => {
	photoDetail = null;
	showPhotoGrid();
});
el.photoDelBtn.addEventListener("click", () => {
	if (photoDetail) deletePhoto(photoDetail.path);
});
el.photoThumbBtn.addEventListener("click", async () => {
	if (photoBusy || !photoPerson || !photoDetail) return;
	photoBusy = true;
	el.thumbModal.classList.add("locked");
	try {
		await api.setThumbnail(photoPerson, photoDetail.path);
		showToast("Avatar updated.", "ok");
		onChange();
	} catch (e) {
		showToast(e.message || "Could not update the avatar.", "err");
	} finally {
		photoBusy = false;
		el.thumbModal.classList.remove("locked");
	}
});

// Add photos: browse, drag & drop anywhere on the modal, or paste.
el.photoAdd.addEventListener("click", () => { if (!photoBusy) el.photoFiles.click(); });
el.photoAdd.addEventListener("keydown", (e) => {
	if ((e.key === "Enter" || e.key === " ") && !photoBusy) {
		e.preventDefault();
		el.photoFiles.click();
	}
});
el.photoFiles.addEventListener("change", () => {
	if (el.photoFiles.files.length) addPhotos(el.photoFiles.files);
	el.photoFiles.value = "";
});
["dragenter", "dragover"].forEach((ev) =>
	el.thumbModal.addEventListener(ev, (e) => {
		e.preventDefault();
		if (!photoBusy && el.photoGridView && !el.photoGridView.hidden) el.photoAdd.classList.add("drag");
	})
);
el.thumbModal.addEventListener("dragleave", (e) => {
	if (!e.relatedTarget) el.photoAdd.classList.remove("drag");
});
el.thumbModal.addEventListener("drop", (e) => {
	e.preventDefault();
	el.photoAdd.classList.remove("drag");
	if (!photoBusy && el.photoGridView && !el.photoGridView.hidden &&
			e.dataTransfer && e.dataTransfer.files.length) {
		addPhotos(e.dataTransfer.files);
	}
});

// Upload the chosen files into the person's profile via the enroll
// endpoint; only successfully enrolled photos land in the grid.
export async function addPhotos(fileList) {
	const files = Array.from(fileList).filter((f) => f.type.startsWith("image/"));
	if (!files.length) { showToast("No image files to add.", "err"); return; }
	photoBusy = true;
	el.thumbModal.classList.add("locked");
	el.photoHint.textContent = `Uploading ${files.length} photo${files.length === 1 ? "" : "s"}…`;
	try {
		const j = await api.enrollPhotos(photoPerson, files);
		if (j.added > 0) {
			showToast(`Added ${j.added} photo${j.added === 1 ? "" : "s"}.`, "ok");
		}
		if (j.failures && j.failures.length) {
			showToast(`${j.failures.length} photo(s) rejected: ${j.failures[0]}`, "err");
		}
		onChange();
	} catch (e) {
		showToast(e.message || "Upload failed.", "err");
	} finally {
		photoBusy = false;
		el.thumbModal.classList.remove("locked");
		if (!el.thumbModal.hidden) loadPhotoGrid(photoPerson);
	}
}

el.thumbModal.addEventListener("click", (e) => {
	if (e.target === el.thumbModal || e.target.closest("[data-close]")) closePhotosModal();
});
