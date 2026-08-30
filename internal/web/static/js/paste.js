/* recogn — clipboard paste routing.
	 Ctrl+V / Cmd+V routes by context: with the enroll modal open, pasted
	 images join the review list; with the photos manager open they upload
	 straight into that person; otherwise they are inspected on the stage.
	 Plain-text pastes into inputs are never hijacked. */

import { showToast } from "./util.js";
import { handleFile } from "./recognize.js";
import * as enroll from "./enroll.js";
import * as photos from "./photos.js";

function imageFilesFromClipboard(dt) {
	if (!dt || !dt.items) return [];
	const out = [];
	for (const item of dt.items) {
		if (item.kind === "file" && item.type.startsWith("image/")) {
			const f = item.getAsFile();
			if (f) out.push(f);
		}
	}
	return out;
}

document.addEventListener("paste", (e) => {
	const files = imageFilesFromClipboard(e.clipboardData);
	if (!files.length) return; // normal text paste — leave to the browser

	const t = e.target;
	if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA") &&
			typeof e.clipboardData.getData === "function" &&
			e.clipboardData.getData("text/plain")) {
		return; // text field + text on the clipboard: default behaviour wins
	}
	e.preventDefault();

	if (enroll.isOpen()) {
		if (enroll.isBusy()) return;
		enroll.addPending(files);
		enroll.pulseDrop();
		enroll.scrollLastThumb();
		showToast(`Added ${files.length} photo${files.length === 1 ? "" : "s"} from clipboard.`, "ok");
	} else if (photos.isOpen()) {
		photos.addPhotos(files);
	} else {
		handleFile(files[0]);
		if (files.length > 1) showToast("Clipboard had several images — inspecting the first.");
	}
});
