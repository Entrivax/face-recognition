/* recogn — face-check viewer.
	 Enlarged preview used by the enroll modal: shows a pending photo with its
	 detected faces drawn over it before anything is uploaded. Read-only. */

import { el } from "./dom.js";
import { drawWhenReady } from "./overlay.js";

let checkFaces = [];   // faces for the photo being viewed
let checkSrc = "";     // image URL (object or server) currently shown
let checkLastFocus = null;

// Returns true when another modal is still open underneath (body keeps the
// modal-open class). Wired by main.js to avoid importing the other modals.
let anyModalOpen = () => false;
export function onAnyModalOpen(fn) { anyModalOpen = fn; }

// openCheckModal({ src, title, faces }) — faces may arrive after the image.
export function openCheckModal({ src, title, faces }) {
	checkLastFocus = document.activeElement;
	el.checkTitle.textContent = title || "Face check";
	checkSrc = src;
	el.checkVerdict.textContent = faces ? "Loading…" : "Detecting faces…";
	el.checkVerdict.classList.remove("warn");
	el.checkImg.src = src;
	checkFaces = faces || null;
	if (faces) drawCheckOverlay(faces);
	el.checkModal.hidden = false;
	document.body.classList.add("modal-open");
	el.checkCard.querySelector(".modal-close").focus();
	if (faces) updateCheckVerdict(faces);
}

export function closeCheckModal() {
	el.checkModal.hidden = true;
	el.checkImg.src = "";
	checkSrc = "";
	checkFaces = [];
	if (!anyModalOpen()) document.body.classList.remove("modal-open");
	// Return focus to the trigger: the enroll thumb when coming from the
	// enroll modal, otherwise back into the stacked photos modal.
	if (checkLastFocus && checkLastFocus.focus) checkLastFocus.focus();
}

export function isOpen() { return !el.checkModal.hidden; }
export function showing(url) { return isOpen() && checkSrc === url; }

// Live-update the viewer when faces arrive for the photo it is showing.
export function updateIfShowing(url, faces) {
	if (!showing(url)) return;
	drawCheckOverlay(faces);
	updateCheckVerdict(faces);
}

function drawCheckOverlay(faces) {
	drawWhenReady(el.checkOverlay, el.checkImg, faces, { labels: true });
}

function updateCheckVerdict(faces) {
	el.checkVerdict.classList.remove("warn");
	if (!faces.length) {
		el.checkVerdict.textContent = "No face detected — this photo will be rejected at enrollment.";
		el.checkVerdict.classList.add("warn");
		return;
	}
	const parts = faces.map((f) => {
		const conf = Math.round((f.confidence || 0) * 100);
		return f.name && f.name !== "unknown"
			? `${f.name} ${conf}%`
			: `unknown (under threshold)`;
	});
	const n = faces.length;
	const lead = n === 1 ? "1 face detected" : `${n} faces detected — the largest face is used`;
	el.checkVerdict.textContent = `${lead} · ${parts.join(", ")}`;
}

el.checkModal.addEventListener("click", (e) => {
	if (e.target === el.checkModal || e.target.closest("[data-close]")) closeCheckModal();
});
