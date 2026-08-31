/* recogn — the main stage: drop a photo, detect every face, show matches. */

import { el } from "./dom.js";
import { showToast, escapeHtml } from "./util.js";
import { recognize, enrollFace as apiEnrollFace } from "./api.js";
import { drawWhenReady } from "./overlay.js";

function openPicker() { el.fileInput.click(); }

// Object URL of the current stage preview. Revoked when replaced or cleared —
// otherwise every inspected photo would stay pinned in memory for the session.
let stageURL = null;
// The File currently under inspection — kept so an unknown face row can be
// enrolled via /api/people/{name}/enroll-face with the exact same bytes
// (detection order is deterministic, so face_index matches the list row).
let currentFile = null;

el.dropzone.addEventListener("click", (e) => {
	if (el.dzPreview.hidden) openPicker();
});
el.dropzone.addEventListener("keydown", (e) => {
	if ((e.key === "Enter" || e.key === " ") && el.dzPreview.hidden) {
		e.preventDefault();
		openPicker();
	}
});
["dragenter", "dragover"].forEach((ev) =>
	el.dropzone.addEventListener(ev, (e) => {
		e.preventDefault();
		el.dropzone.classList.add("drag");
	})
);
["dragleave", "drop"].forEach((ev) =>
	el.dropzone.addEventListener(ev, (e) => {
		e.preventDefault();
		el.dropzone.classList.remove("drag");
	})
);
el.dropzone.addEventListener("drop", (e) => {
	const f = e.dataTransfer.files && e.dataTransfer.files[0];
	if (f) handleFile(f);
});
el.fileInput.addEventListener("change", () => {
	if (el.fileInput.files[0]) handleFile(el.fileInput.files[0]);
	el.fileInput.value = "";
});

el.clearBtn.addEventListener("click", resetStage);
el.againBtn.addEventListener("click", (e) => { e.stopPropagation(); openPicker(); });

function resetStage() {
	el.dzPreview.hidden = true;
	el.dzEmpty.hidden = false;
	el.clearBtn.hidden = true;
	el.againBtn.hidden = true;
	el.results.hidden = true;
	el.stageMeta.textContent = "";
	if (stageURL) { URL.revokeObjectURL(stageURL); stageURL = null; }
	currentFile = null;
	el.previewImg.src = "";
	el.dropzone.style.cursor = "pointer";
}

export async function handleFile(file) {
	if (!file.type.startsWith("image/")) {
		showToast("That file isn't an image.", "err");
		return;
	}
	currentFile = file;
	// Show preview immediately.
	const url = URL.createObjectURL(file);
	if (stageURL) URL.revokeObjectURL(stageURL);
	stageURL = url;
	el.previewImg.src = url;
	el.dzEmpty.hidden = true;
	el.dzPreview.hidden = false;
	el.clearBtn.hidden = false;
	el.againBtn.hidden = false;
	el.results.hidden = true;
	el.faceList.innerHTML = "";
	el.stageMeta.textContent = "analyzing…";
	el.dropzone.style.cursor = "default";

	try {
		const j = await recognize(file);
		renderResults(j.faces || []);
		el.stageMeta.textContent = `${file.name} · ${(file.size / 1024).toFixed(0)} KB`;
	} catch (e) {
		el.stageMeta.textContent = "";
		showToast(e.message || "Recognition failed.", "err");
	}
}

// Called after an unknown face is enrolled from the results, so main.js can
// refresh the people list and health without this module importing them.
let onEnrolled = () => {};
export function onFaceEnrolled(fn) { onEnrolled = fn; }

function renderResults(faces) {
	el.results.hidden = false;
	el.resultCount.textContent = String(faces.length);
	el.faceList.innerHTML = "";
	faces.forEach((f, i) => {
		const li = document.createElement("li");
		li.className = "face-row" + (f.name === "unknown" ? " unknown" : "");
		const conf = Math.round((f.confidence || 0) * 100);
		// All identities above the threshold, ranked — two near-tied entries
		// hint at duplicate people in the database.
		const matches = f.matches || [];
		let matchesHtml = "";
		if (matches.length >= 2) {
			const near = matches[0].score - matches[1].score <= 0.05;
			matchesHtml = `
				<ul class="face-matches${near ? " ambiguous" : ""}">
					${matches.map((m, idx) => `
						<li class="${idx === 0 ? "top" : ""}">
							<span class="fm-name">${escapeHtml(m.name)}</span>
							<span class="fm-score">${Math.round((m.score || 0) * 100)}%</span>
						</li>`).join("")}
				</ul>
				${near ? `<p class="face-dup-hint">scores nearly tied — possible duplicate people?</p>` : ""}`;
		}
		// Unknown faces can be enrolled right here: name them and the same
		// uploaded file is re-sent with this row's 1-based index.
		const enrollForm = f.name === "unknown" && currentFile
			? `<form class="face-enroll">
					<input type="text" class="face-enroll-input" placeholder="Name this person…" maxlength="120"
								 autocomplete="off" spellcheck="false" aria-label="Enroll this face as">
					<button type="submit" class="btn btn-accent btn-sm">Enroll</button>
				</form>`
			: "";
		li.innerHTML = `
			<span class="face-index">${String(i + 1).padStart(2, "0")}</span>
			<div>
				<div class="face-name">${escapeHtml(f.name)}</div>
				${matchesHtml}
				${enrollForm}
			</div>
			<div class="face-right">
				<span class="face-conf">${conf}%</span>
				<div class="conf-bar" style="color:${f.name === "unknown" ? "var(--unknown)" : "var(--match)"}">
					<span style="width:${conf}%"></span>
				</div>
			</div>`;
		const form = li.querySelector(".face-enroll");
		if (form) wireEnrollForm(form, li, i + 1);
		el.faceList.appendChild(li);
	});
	drawOverlay(faces);
}

// Submit handler for an unknown row's "name this face" form: enrolls the
// given 1-based face of the current photo under the typed name.
function wireEnrollForm(form, li, faceIndex) {
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (!currentFile) return;
		const input = form.querySelector(".face-enroll-input");
		const btn = form.querySelector("button");
		const person = input.value.trim();
		if (!person) { input.focus(); return; }
		btn.disabled = true;
		input.disabled = true;
		try {
			const j = await apiEnrollFace(person, currentFile, faceIndex);
			showToast(`Enrolled ${person} (${j.saved}).`, "ok");
			li.classList.remove("unknown");
			li.querySelector(".face-name").textContent = person;
			form.remove();
			onEnrolled();
		} catch (err) {
			showToast(err.message || "Enrollment failed.", "err");
			btn.disabled = false;
			input.disabled = false;
			input.focus();
		}
	});
}

// Draw corner-bracket boxes + labels over the preview, scaled to the image.
function drawOverlay(faces) {
	drawWhenReady(el.overlay, el.previewImg, faces, { labels: true });
}
