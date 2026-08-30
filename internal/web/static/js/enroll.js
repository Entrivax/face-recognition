/* recogn — enroll modal.
	 Photos chosen for enrollment are held client-side for review; the upload
	 only happens when the user confirms with the "Enroll" button. More photos
	 can be added at any time by drag & drop or the file input. */

import { el } from "./dom.js";
import { showToast, fmtSize } from "./util.js";
import * as api from "./api.js";
import { drawFaces } from "./overlay.js";
import { state } from "./state.js";
import { openCheckModal, updateIfShowing } from "./facecheck.js";

const NAME_HINT = "Shown as the identity when their face is recognized.";
let pending = []; // { key, file, url, li, canvas, badge, faces, checking }
let enrolling = false;
let lastFocus = null;

// Called after a successful enroll so main.js can reload people + health.
let onChange = () => {};
export function onEnrollChange(fn) { onChange = fn; }

export function isOpen() { return !el.enrollModal.hidden; }
export function isBusy() { return enrolling; }

el.enrollBtn.addEventListener("click", openEnroll);

function openEnroll() {
	clearPending();
	el.enrollName.value = "";
	setNameHint();
	updateEnrollMeta();
	lastFocus = document.activeElement;
	el.enrollModal.hidden = false;
	document.body.classList.add("modal-open");
	el.enrollName.focus();
}

export function closeEnroll() {
	if (enrolling) return; // locked while the request is in flight
	el.enrollModal.hidden = true;
	document.body.classList.remove("modal-open");
	clearPending();
	if (lastFocus && lastFocus.focus) lastFocus.focus();
}

function clearPending() {
	pending.forEach((p) => URL.revokeObjectURL(p.url));
	pending = [];
	el.enrollThumbs.innerHTML = "";
}

function setEnrollBusy(busy) {
	enrolling = busy;
	el.enrollModal.classList.toggle("locked", busy);
	el.enrollName.disabled = busy;
	el.enrollDrop.classList.toggle("disabled", busy);
}

// Add files to the review list (no upload). Dedupes by name+size+mtime.
// Each photo immediately gets a client-side face-check preview so the user
// can see the detected faces before committing to enrollment.
export function addPending(fileList) {
	let added = 0, notImage = 0, duplicate = 0;
	for (const file of Array.from(fileList)) {
		if (!file.type.startsWith("image/")) { notImage++; continue; }
		const key = `${file.name}\n${file.size}\n${file.lastModified}`;
		if (pending.some((p) => p.key === key)) { duplicate++; continue; }
		const url = URL.createObjectURL(file);
		const p = { key, file, url, faces: null, checking: true };
		buildThumb(p);
		pending.push(p);
		el.enrollThumbs.appendChild(p.li);
		queueFaceCheck(p);
		added++;
	}
	if (notImage) showToast(`${notImage} file(s) skipped — not images.`, "err");
	if (duplicate && !added) showToast(`${duplicate} file(s) already selected.`);
	if (added) updateEnrollMeta();
}

// Briefly highlight the dropzone (used when photos arrive via paste).
export function pulseDrop() {
	el.enrollDrop.classList.add("pulse");
	clearTimeout(pulseTimer);
	pulseTimer = setTimeout(() => el.enrollDrop.classList.remove("pulse"), 900);
}
let pulseTimer = null;

// Scroll the newest thumb into view (used after a paste).
export function scrollLastThumb() {
	if (el.enrollThumbs.lastElementChild) {
		el.enrollThumbs.lastElementChild.scrollIntoView({ block: "nearest" });
	}
}

// ---- pre-submit face check ----
// Each pending photo is sent to /api/recognize as soon as it joins the list
// (sequentially — inference is a single serialized stream) so the user sees
// the detected faces before deciding to enroll.
let checkChain = Promise.resolve();

function queueFaceCheck(p) {
	checkChain = checkChain.then(() => checkPendingFace(p)).catch(() => {});
}

async function checkPendingFace(p) {
	if (!pending.includes(p)) return; // removed while queued
	setBadge(p, "checking…", "wait");
	try {
		const j = await api.recognize(p.file);
		if (!pending.includes(p)) return;
		p.faces = j.faces || [];
		p.checking = false;
		drawThumbOverlay(p);
		if (!p.faces.length) {
			setBadge(p, "no face", "warn");
		} else if (p.faces.length === 1) {
			setBadge(p, "1 face", "ok");
		} else {
			setBadge(p, `${p.faces.length} faces`, "multi");
		}
		// Live-update the enlarged viewer when it is showing this photo.
		updateIfShowing(p.url, p.faces);
	} catch (e) {
		p.checking = false;
		setBadge(p, "check failed", "warn");
	}
}

function setBadge(p, text, kind) {
	if (!p.badge) return;
	p.badge.textContent = text;
	p.badge.className = "face-badge" + (kind ? " " + kind : "");
}

function drawThumbOverlay(p) {
	if (!p.canvas) return;
	const img = p.li.querySelector("img");
	const draw = () => drawFaces(p.canvas, img, p.faces || [], { labels: false });
	if (img.complete && img.naturalWidth) draw();
	else img.onload = draw;
}

function buildThumb(p) {
	const li = document.createElement("li");
	li.className = "enroll-thumb";
	p.li = li;
	p.canvas = document.createElement("canvas");
	p.badge = document.createElement("span");
	p.badge.className = "face-badge wait";
	p.badge.textContent = "checking…";

	const img = document.createElement("img");
	img.src = p.url;
	img.alt = "";

	// Face boxes are drawn over the thumbnail as soon as detection returns.
	const wrap = document.createElement("div");
	wrap.className = "thumb-wrap";
	wrap.append(img, p.canvas);

	// Click anywhere on the tile to inspect the faces full-size.
	li.tabIndex = 0;
	li.title = "Click to inspect faces";
	const openViewer = () => openCheckModal({ src: p.url, title: p.file.name, faces: p.faces });
	li.addEventListener("click", (e) => {
		if (e.target.closest(".enroll-thumb-del")) return;
		openViewer();
	});
	li.addEventListener("keydown", (e) => {
		if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openViewer(); }
	});

	const del = document.createElement("button");
	del.type = "button";
	del.className = "enroll-thumb-del";
	del.textContent = "×";
	del.setAttribute("aria-label", `Remove ${p.file.name}`);
	del.addEventListener("click", () => removePending(p.file));

	const meta = document.createElement("div");
	meta.className = "enroll-thumb-meta";
	const name = document.createElement("div");
	name.className = "enroll-thumb-name";
	name.textContent = p.file.name;
	name.title = p.file.name;
	const size = document.createElement("div");
	size.className = "enroll-thumb-size";
	size.textContent = fmtSize(p.file.size);
	meta.append(name, size);

	const note = document.createElement("p");
	note.className = "enroll-thumb-note";

	li.append(wrap, p.badge, del, meta, note);
	return li;
}

function removePending(file) {
	const i = pending.findIndex((p) => p.file === file);
	if (i < 0) return;
	URL.revokeObjectURL(pending[i].url);
	pending[i].li.remove();
	pending.splice(i, 1);
	updateEnrollMeta();
}

function updateEnrollMeta() {
	el.enrollThumbs.hidden = pending.length === 0;
	const n = pending.length;
	if (!n) {
		el.enrollMeta.textContent = "No photos selected yet.";
	} else {
		const bytes = pending.reduce((s, p) => s + p.file.size, 0);
		el.enrollMeta.textContent = `${n} photo${n === 1 ? "" : "s"} ready · ${fmtSize(bytes)} · review, then enroll`;
	}
	if (!enrolling) {
		el.enrollSubmit.disabled = !(n && el.enrollName.value.trim());
		el.enrollSubmit.textContent = n ? `Enroll ${n} photo${n === 1 ? "" : "s"}` : "Enroll photos";
	}
}

function setNameHint() {
	const name = el.enrollName.value.trim();
	const known = name && state.peopleNames.some((n) => n.toLowerCase() === name.toLowerCase());
	el.enrollNameHint.textContent = known
		? `${name} is already enrolled — photos will be added to their profile.`
		: NAME_HINT;
	el.enrollNameHint.classList.toggle("warn", Boolean(known));
}

el.enrollName.addEventListener("input", () => { setNameHint(); updateEnrollMeta(); });

// dropzone: click / keyboard opens the picker; drag & drop appends files.
el.enrollDrop.addEventListener("click", () => { if (!enrolling) el.enrollPhotos.click(); });
el.enrollDrop.addEventListener("keydown", (e) => {
	if ((e.key === "Enter" || e.key === " ") && !enrolling) {
		e.preventDefault();
		el.enrollPhotos.click();
	}
});
el.enrollPhotos.addEventListener("change", () => {
	if (el.enrollPhotos.files.length) addPending(el.enrollPhotos.files);
	el.enrollPhotos.value = ""; // allow re-picking the same file later
});

// Drop anywhere on the modal adds photos; brackets highlight the dropzone.
["dragenter", "dragover"].forEach((ev) =>
	el.enrollModal.addEventListener(ev, (e) => {
		e.preventDefault();
		if (!enrolling) el.enrollDrop.classList.add("drag");
	})
);
el.enrollModal.addEventListener("dragleave", (e) => {
	if (!e.relatedTarget) el.enrollDrop.classList.remove("drag");
});
el.enrollModal.addEventListener("drop", (e) => {
	e.preventDefault();
	el.enrollDrop.classList.remove("drag");
	if (!enrolling && e.dataTransfer && e.dataTransfer.files.length) {
		addPending(e.dataTransfer.files);
	}
});

// close: backdrop, ×/Cancel (Escape handled by the unified keydown in main.js)
el.enrollModal.addEventListener("click", (e) => {
	if (e.target === el.enrollModal || e.target.closest("[data-close]")) closeEnroll();
});

// Submit uploads the reviewed photos — the only point where anything is sent.
el.enrollForm.addEventListener("submit", async (e) => {
	e.preventDefault();
	if (enrolling) return;
	const name = el.enrollName.value.trim();
	if (!name) { showToast("Enter the person's name first.", "err"); el.enrollName.focus(); return; }
	if (!pending.length) { showToast("Add at least one photo.", "err"); return; }

	setEnrollBusy(true);
	el.enrollSubmit.textContent = "Enrolling…";
	el.enrollMeta.textContent = `Enrolling ${pending.length} photo${pending.length === 1 ? "" : "s"} for ${name}…`;

	try {
		const j = await api.enrollPhotos(name, pending.map((p) => p.file));

		if (j.added > 0) showToast(`Enrolled ${j.added} photo${j.added === 1 ? "" : "s"} for ${name}.`, "ok");
		onChange();

		// Keep the modal open when some photos failed, so they can be swapped.
		const failedNames = new Set((j.failures || [])
			.map((f) => (f.includes(":") ? f.slice(0, f.indexOf(":")) : f).trim())
			.filter(Boolean));
		pending.filter((p) => !failedNames.has(p.file.name))
			.forEach((p) => { URL.revokeObjectURL(p.url); p.li.remove(); });
		pending = pending.filter((p) => failedNames.has(p.file.name));
		pending.forEach((p) => {
			const raw = (j.failures || []).find((f) => f.startsWith(p.file.name + ":")) || "";
			const reason = raw.slice(p.file.name.length + 1).trim() || "could not be enrolled";
			p.li.classList.add("failed");
			const note = p.li.querySelector(".enroll-thumb-note");
			note.textContent = reason.includes("no face detected") ? "no face detected" : reason;
		});
		setEnrollBusy(false);
		if (pending.length) {
			showToast(`${pending.length} photo(s) had no detectable face — remove or replace them.`, "err");
			updateEnrollMeta();
		} else {
			closeEnroll();
		}
	} catch (err) {
		setEnrollBusy(false);
		showToast(err.message || "Enroll failed.", "err");
		updateEnrollMeta();
	}
});
