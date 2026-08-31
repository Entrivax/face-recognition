/* recogn — entry point: health, rescan, modal wiring, keyboard, boot. */

import { el } from "./dom.js";
import { showToast } from "./util.js";
import * as api from "./api.js";
import * as facecheck from "./facecheck.js";
import * as photos from "./photos.js";
import * as enroll from "./enroll.js";
import { loadPeople, onPeopleChange } from "./people.js";
import { onPhotosChange } from "./photos.js";
import { onEnrollChange } from "./enroll.js";
import { onFaceEnrolled } from "./recognize.js"; // stage wires its own listeners on import
import "./paste.js";     // clipboard routing wires itself on import

// ---------- health ----------
async function checkHealth() {
	try {
		const j = await api.getHealth();
		el.statusEl.className = "status ok";
		el.statusText.textContent = `${j.people} people · threshold ${j.threshold.toFixed(2)}`;
	} catch (e) {
		el.statusEl.className = "status err";
		el.statusText.textContent = "server unreachable";
	}
}

// Cross-module refresh: any mutation (person removed, photos added/deleted,
// avatar changed, enroll succeeded) reloads the people list and health.
function refreshAll() {
	loadPeople();
	checkHealth();
}
onPeopleChange(refreshAll);
onPhotosChange(refreshAll);
onEnrollChange(refreshAll);
onFaceEnrolled(refreshAll); // a face enrolled from the results list adds a person

// facecheck needs to know whether another modal is still open underneath it
// (to keep the body's modal-open class) without importing the other modals.
facecheck.onAnyModalOpen(() => photos.isOpen() || enroll.isOpen());

// ---------- rescan ----------
el.rescanBtn.addEventListener("click", async () => {
	el.rescanBtn.disabled = true;
	el.rescanBtn.textContent = "Rescanning…";
	try {
		const j = await api.rescan();
		showToast(`Rescan done: ${j.added} new, ${j.kept} kept, ${j.failed} failed.`, "ok");
		refreshAll();
	} catch (e) {
		showToast(e.message, "err");
	} finally {
		el.rescanBtn.disabled = false;
		el.rescanBtn.textContent = "Rescan people folder";
	}
});

// ---------- threshold ----------
// The slider mirrors the server's persisted threshold; changing it POSTs to
// /api/config, which also saves it for future restarts.
async function loadThreshold() {
	try {
		const j = await api.getConfig();
		el.thresholdSlider.value = String(j.threshold);
		el.thresholdValue.textContent = Number(j.threshold).toFixed(2);
	} catch (e) {
		// Leave the slider at its default; the status pill shows the live value.
	}
}

el.thresholdSlider.addEventListener("input", () => {
	el.thresholdValue.textContent = Number(el.thresholdSlider.value).toFixed(2);
});
el.thresholdSlider.addEventListener("change", async () => {
	const v = Number(el.thresholdSlider.value);
	try {
		const j = await api.setThreshold(v);
		el.thresholdSlider.value = String(j.threshold);
		el.thresholdValue.textContent = Number(j.threshold).toFixed(2);
		showToast(`Threshold set to ${Number(j.threshold).toFixed(2)} (saved).`, "ok");
		checkHealth();
	} catch (e) {
		showToast(e.message || "Could not set the threshold.", "err");
	}
});

// ---------- keyboard ----------

// keep Tab focus inside whichever dialog is on top
function trapTab(card) {
	card.addEventListener("keydown", (e) => {
		if (e.key !== "Tab") return;
		const focusables = [...card.querySelectorAll(
			"button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])"
		)].filter((el) => !el.disabled && el.offsetParent !== null);
		if (!focusables.length) return;
		const first = focusables[0];
		const last = focusables[focusables.length - 1];
		if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
		else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
	});
}
trapTab(el.photoCard);
trapTab(el.checkCard);

// keep Tab focus inside the enroll dialog while it is open (narrower
// selector: only enabled, visible controls and explicit tabstops)
el.enrollCard.addEventListener("keydown", (e) => {
	if (e.key !== "Tab") return;
	const focusables = [...el.enrollCard.querySelectorAll(
		'button:not([hidden]):not(:disabled), input:not([hidden]):not(:disabled), [tabindex="0"]'
	)].filter((el) => el.offsetParent !== null);
	if (!focusables.length) return;
	const first = focusables[0];
	const last = focusables[focusables.length - 1];
	if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
	else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
});

// Escape closes the topmost open dialog only; while the photos modal's
// inline rename editor is open, Escape cancels the edit instead.
document.addEventListener("keydown", (e) => {
	if (e.key !== "Escape") return;
	if (facecheck.isOpen()) { facecheck.closeCheckModal(); return; }
	if (photos.isOpen()) {
		if (photos.isRenaming()) photos.cancelRename();
		else photos.closePhotosModal();
	}
	else if (enroll.isOpen()) enroll.closeEnroll();
});

// ---------- boot ----------
checkHealth();
loadPeople();
loadThreshold();
setInterval(checkHealth, 30000);
