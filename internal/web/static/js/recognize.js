/* recogn — the main stage: drop a photo, detect every face, show matches. */

import { el } from "./dom.js";
import { showToast, escapeHtml } from "./util.js";
import { recognize } from "./api.js";
import { drawWhenReady } from "./overlay.js";

function openPicker() { el.fileInput.click(); }

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
	el.previewImg.src = "";
	el.dropzone.style.cursor = "pointer";
}

export async function handleFile(file) {
	if (!file.type.startsWith("image/")) {
		showToast("That file isn't an image.", "err");
		return;
	}
	// Show preview immediately.
	const url = URL.createObjectURL(file);
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
		li.innerHTML = `
			<span class="face-index">${String(i + 1).padStart(2, "0")}</span>
			<div>
				<div class="face-name">${escapeHtml(f.name)}</div>
				${matchesHtml}
			</div>
			<div class="face-right">
				<span class="face-conf">${conf}%</span>
				<div class="conf-bar" style="color:${f.name === "unknown" ? "var(--unknown)" : "var(--match)"}">
					<span style="width:${conf}%"></span>
				</div>
			</div>`;
		el.faceList.appendChild(li);
	});
	drawOverlay(faces);
}

// Draw corner-bracket boxes + labels over the preview, scaled to the image.
function drawOverlay(faces) {
	drawWhenReady(el.overlay, el.previewImg, faces, { labels: true });
}
