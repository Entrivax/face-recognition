/* recogn — face review console front-end.
	 Vanilla JS, no build step. Talks to the REST API under /api. */
(() => {
	"use strict";

	const $ = (id) => document.getElementById(id);
	const dropzone = $("dropzone");
	const fileInput = $("fileInput");
	const dzEmpty = $("dzEmpty");
	const dzPreview = $("dzPreview");
	const previewImg = $("previewImg");
	const overlay = $("overlay");
	const clearBtn = $("clearBtn");
	const againBtn = $("againBtn");
	const stageMeta = $("stageMeta");
	const results = $("results");
	const resultCount = $("resultCount");
	const faceList = $("faceList");
	const peopleList = $("peopleList");
	const peopleCount = $("peopleCount");
	const enrollBtn = $("enrollBtn");
	const enrollModal = $("enrollModal");
	const enrollCard = $("enrollCard");
	const enrollForm = $("enrollForm");
	const enrollName = $("enrollName");
	const enrollNameHint = $("enrollNameHint");
	const enrollDrop = $("enrollDrop");
	const enrollPhotos = $("enrollPhotos");
	const enrollThumbs = $("enrollThumbs");
	const enrollMeta = $("enrollMeta");
	const enrollSubmit = $("enrollSubmit");
	const thumbModal = $("photoModal");
	const photoCard = $("photoCard");
	const photoTitle = $("photoTitle");
	const photoHint = $("photoHint");
	const photoGridView = $("photoGridView");
	const photoDetailView = $("photoDetailView");
	const photoGrid = $("photoGrid");
	const photoAdd = $("photoAdd");
	const photoFiles = $("photoFiles");
	const photoDetailImg = $("photoDetailImg");
	const photoOverlay = $("photoOverlay");
	const photoVerdict = $("photoVerdict");
	const photoBackBtn = $("photoBackBtn");
	const photoThumbBtn = $("photoThumbBtn");
	const photoDelBtn = $("photoDelBtn");
	// face-check viewer (enlarged enroll preview)
	const checkModal = $("checkModal");
	const checkCard = $("checkCard");
	const checkTitle = $("checkTitle");
	const checkImg = $("checkImg");
	const checkOverlay = $("checkOverlay");
	const checkVerdict = $("checkVerdict");
	const rescanBtn = $("rescanBtn");
	const statusEl = $("status");
	const statusText = $("statusText");
	const toast = $("toast");

	let toastTimer = null;

	function showToast(msg, kind = "") {
		toast.textContent = msg;
		toast.className = "toast show" + (kind ? " " + kind : "");
		clearTimeout(toastTimer);
		toastTimer = setTimeout(() => (toast.className = "toast"), 3600);
	}

	// ---------- health ----------
	async function checkHealth() {
		try {
			const r = await fetch("/api/health");
			if (!r.ok) throw new Error("bad status");
			const j = await r.json();
			statusEl.className = "status ok";
			statusText.textContent = `${j.people} people · threshold ${j.threshold.toFixed(2)}`;
		} catch (e) {
			statusEl.className = "status err";
			statusText.textContent = "server unreachable";
		}
	}

	// ---------- recognize ----------
	function openPicker() { fileInput.click(); }

	dropzone.addEventListener("click", (e) => {
		if (dzPreview.hidden) openPicker();
	});
	dropzone.addEventListener("keydown", (e) => {
		if ((e.key === "Enter" || e.key === " ") && dzPreview.hidden) {
			e.preventDefault();
			openPicker();
		}
	});
	["dragenter", "dragover"].forEach((ev) =>
		dropzone.addEventListener(ev, (e) => {
			e.preventDefault();
			dropzone.classList.add("drag");
		})
	);
	["dragleave", "drop"].forEach((ev) =>
		dropzone.addEventListener(ev, (e) => {
			e.preventDefault();
			dropzone.classList.remove("drag");
		})
	);
	dropzone.addEventListener("drop", (e) => {
		const f = e.dataTransfer.files && e.dataTransfer.files[0];
		if (f) handleFile(f);
	});
	fileInput.addEventListener("change", () => {
		if (fileInput.files[0]) handleFile(fileInput.files[0]);
		fileInput.value = "";
	});

	clearBtn.addEventListener("click", resetStage);
	againBtn.addEventListener("click", (e) => { e.stopPropagation(); openPicker(); });

	function resetStage() {
		dzPreview.hidden = true;
		dzEmpty.hidden = false;
		clearBtn.hidden = true;
		againBtn.hidden = true;
		results.hidden = true;
		stageMeta.textContent = "";
		previewImg.src = "";
		dropzone.style.cursor = "pointer";
	}

	async function handleFile(file) {
		if (!file.type.startsWith("image/")) {
			showToast("That file isn't an image.", "err");
			return;
		}
		// Show preview immediately.
		const url = URL.createObjectURL(file);
		previewImg.src = url;
		dzEmpty.hidden = true;
		dzPreview.hidden = false;
		clearBtn.hidden = false;
		againBtn.hidden = false;
		results.hidden = true;
		faceList.innerHTML = "";
		stageMeta.textContent = "analyzing…";
		dropzone.style.cursor = "default";

		try {
			const fd = new FormData();
			fd.append("image", file, file.name);
			const r = await fetch("/api/recognize", { method: "POST", body: fd });
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "recognition failed");
			renderResults(j.faces || []);
			stageMeta.textContent = `${file.name} · ${(file.size / 1024).toFixed(0)} KB`;
		} catch (e) {
			stageMeta.textContent = "";
			showToast(e.message || "Recognition failed.", "err");
		}
	}

	function renderResults(faces) {
		results.hidden = false;
		resultCount.textContent = String(faces.length);
		faceList.innerHTML = "";
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
			faceList.appendChild(li);
		});
		drawOverlay(faces);
	}

	// Draw corner-bracket boxes + labels over the preview, scaled to the image.
	function drawOverlay(faces) {
		const draw = () => drawFaces(overlay, previewImg, faces, { labels: true });
		if (previewImg.complete && previewImg.naturalWidth) draw();
		else previewImg.onload = draw;
	}


	// ---------- shared face overlay ----------
	// One renderer for every surface that shows detected faces: the main stage,
	// the photos-manager detail view, and the enroll face-check viewer. Draws
	// scaled corner brackets per face; labels (identity + confidence) are
	// optional for small thumbnails.
	function drawFaces(canvas, img, faces, opts = {}) {
		const w = img.naturalWidth;
		const h = img.naturalHeight;
		if (!w || !h) return;
		canvas.width = w;
		canvas.height = h;
		const ctx = canvas.getContext("2d");
		ctx.clearRect(0, 0, w, h);
		const scale = Math.max(w, h) / 900; // line width scales with image size

		faces.forEach((f) => {
			const [x, y, bw, bh] = f.bbox;
			const known = f.name && f.name !== "unknown";
			const col = known ? "#38e0c8" : "#f5b53f";
			const L = Math.max(14 * scale, Math.min(bw, bh) * 0.22); // bracket arm
			const lw = Math.max(2, 2.5 * scale);
			ctx.strokeStyle = col;
			ctx.lineWidth = lw;
			ctx.shadowColor = col;
			ctx.shadowBlur = 6 * scale;

			// corner brackets
			const corners = [
				[x, y, 1, 1], [x + bw, y, -1, 1],
				[x, y + bh, 1, -1], [x + bw, y + bh, -1, -1],
			];
			corners.forEach(([cx, cy, sx, sy]) => {
				ctx.beginPath();
				ctx.moveTo(cx + L * sx, cy);
				ctx.lineTo(cx, cy);
				ctx.lineTo(cx, cy + L * sy);
				ctx.stroke();
			});

			if (!opts || !opts.labels) return;

			// label
			ctx.shadowBlur = 0;
			const conf = Math.round((f.confidence || f.score || 0) * 100);
			const label = known ? `${f.name} ${conf}%` : `unknown ${conf}%`;
			const fs = Math.max(12, 15 * scale);
			ctx.font = `600 ${fs}px "Space Grotesk", sans-serif`;
			const tw = ctx.measureText(label).width;
			const pad = 6 * scale;
			const bx = x;
			const by = y - fs - pad * 2 < 0 ? y + bh : y - fs - pad * 2; // above, else below
			ctx.fillStyle = "rgba(11,14,18,0.85)";
			ctx.fillRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
			ctx.strokeStyle = col;
			ctx.lineWidth = 1;
			ctx.strokeRect(bx - 1, by - 1, tw + pad * 2 + 2, fs + pad * 2 + 2);
			ctx.fillStyle = col;
			ctx.fillText(label, bx + pad, by + fs + pad - 2 * scale);
		});
	}

	// ---------- people ----------
	function initials(name) {
		return name.trim().split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
	}

	let peopleNames = []; // for the "already enrolled" hint in the modal

	async function loadPeople() {
		try {
			const r = await fetch("/api/people");
			const j = await r.json();
			const people = j.people || [];
			peopleNames = people.map((p) => p.name);
			peopleCount.textContent = people.length
				? `${people.length} ${people.length === 1 ? "person" : "people"} in the database`
				: "No one enrolled yet.";
			peopleList.innerHTML = "";
			people.forEach((p) => {
				const li = document.createElement("li");
				li.className = "person-row";
				const avatar = p.thumb
					? `<img class="person-avatar person-avatar-img" src="${escapeHtml(p.thumb)}" alt="" data-initials="${escapeHtml(initials(p.name))}">`
					: `<span class="person-avatar">${escapeHtml(initials(p.name))}</span>`;
				li.innerHTML = `
					<button type="button" class="person-avatar-btn" title="Manage photos"
									aria-label="Manage photos for ${escapeHtml(p.name)}">${avatar}</button>
					<button type="button" class="person-open" title="Manage photos"
									aria-label="Manage photos for ${escapeHtml(p.name)}">
						<span class="person-name">${escapeHtml(p.name)}</span>
						<span class="person-count">${p.photos} photo(s)</span>
					</button>
					<button class="person-del" title="Remove ${escapeHtml(p.name)}" aria-label="Remove ${escapeHtml(p.name)}">×</button>`;
				const img = li.querySelector("img.person-avatar-img");
				if (img) {
					// Missing/broken thumbnail (e.g. deleted sidecar) → initials.
					img.addEventListener("error", () => {
						const span = document.createElement("span");
						span.className = "person-avatar";
						span.textContent = img.dataset.initials || "";
						img.replaceWith(span);
					});
				}
				li.querySelector(".person-avatar-btn")
					.addEventListener("click", () => openPhotosModal(p.name));
				li.querySelector(".person-open")
					.addEventListener("click", () => openPhotosModal(p.name));
				li.querySelector(".person-del").addEventListener("click", () => removePerson(p.name));
				peopleList.appendChild(li);
			});
		} catch (e) {
			peopleCount.textContent = "Could not load people.";
		}
	}

	async function removePerson(name) {
		if (!confirm(`Remove ${name} and all their photos from the database?`)) return;
		try {
			const r = await fetch(`/api/people/${encodeURIComponent(name)}`, { method: "DELETE" });
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "delete failed");
			showToast(`Removed ${name}.`, "ok");
			loadPeople();
			checkHealth();
		} catch (e) {
			showToast(e.message, "err");
		}
	}

	// ---------- photos manager modal ----------
	// Opens from a person's row or avatar. Grid view lists their enrolled
	// photos (remove per tile, add via drop/paste/browse); clicking a tile opens
	// the detail view, which runs detection on the stored photo and draws the
	// faces over it so the user can judge the photo's quality.
	let photoPerson = null;   // name of the person being managed
	let photoDetail = null;   // { path } currently shown in the detail view
	let photoBusy = false;
	let photoLastFocus = null;

	async function openPhotosModal(name) {
		photoLastFocus = document.activeElement;
		photoPerson = name;
		photoTitle.textContent = name;
		photoHint.textContent = "Loading photos…";
		showPhotoGrid();
		photoGrid.innerHTML = "";
		thumbModal.hidden = false;
		document.body.classList.add("modal-open");
		photoCard.querySelector(".modal-close").focus();
		await loadPhotoGrid(name);
	}

	function closePhotosModal() {
		if (photoBusy) return; // locked while a request is in flight
		thumbModal.hidden = true;
		document.body.classList.remove("modal-open");
		photoGrid.innerHTML = "";
		photoPerson = null;
		photoDetail = null;
		if (photoLastFocus && photoLastFocus.focus) photoLastFocus.focus();
	}

	function showPhotoDetail(state) {
		photoDetailView.hidden = !state;
		photoGridView.hidden = Boolean(state);
	}

	function showPhotoGrid() {
		showPhotoDetail(false);
	}

	function photoURL(name, path) {
		return `/api/people/${encodeURIComponent(name)}/photos/${encodeURIComponent(path)}`;
	}

	async function loadPhotoGrid(name) {
		photoHint.textContent = "Loading photos…";
		try {
			const r = await fetch(`/api/people/${encodeURIComponent(name)}`);
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "could not load photos");
			renderPhotoTiles(j.photos || []);
		} catch (e) {
			photoHint.textContent = e.message || "Could not load photos.";
		}
	}

	function renderPhotoTiles(photos) {
		photoGrid.innerHTML = "";
		photoHint.textContent = photos.length
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
			img.src = photoURL(photoPerson, ph.path);
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
			photoGrid.appendChild(li);
		}
	}

	// Detail view: load the photo, detect its faces, draw the overlay and a
	// plain-language verdict.
	async function openPhotoDetail(photoPath) {
		if (!photoPerson) return;
		photoDetail = { path: photoPath };
		photoVerdict.classList.remove("warn");
		photoVerdict.textContent = "Detecting faces…";
		// Clear any boxes left over from a previously viewed photo.
		photoOverlay.getContext("2d").clearRect(0, 0, photoOverlay.width, photoOverlay.height);
		photoDetailImg.src = photoURL(photoPerson, photoPath);
		showPhotoDetail(true);
		photoBackBtn.focus();

		try {
			const r = await fetch(photoURL(photoPerson, photoPath) + "/detect");
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "detection failed");
			if (!photoDetail || photoDetail.path !== photoPath) return; // view moved on
			renderPhotoVerdict(j.faces || []);
		} catch (e) {
			if (photoDetail && photoDetail.path === photoPath) {
				photoVerdict.textContent = e.message || "Could not analyze this photo.";
				photoVerdict.classList.add("warn");
			}
		}
	}

	function renderPhotoVerdict(faces) {
		const draw = () => drawFaces(photoOverlay, photoDetailImg, faces, { labels: true });
		if (photoDetailImg.complete && photoDetailImg.naturalWidth) draw();
		else photoDetailImg.onload = draw;

		photoVerdict.classList.remove("warn");
		if (!faces.length) {
			photoVerdict.textContent = "No face detected — this photo contributes nothing to recognition.";
			photoVerdict.classList.add("warn");
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
		photoVerdict.textContent = `${lead} · ${parts.join(", ")}`;
	}

	async function deletePhoto(photoPath) {
		if (photoBusy || !photoPerson) return;
		if (!confirm(`Remove ${photoPath} from ${photoTitle.textContent}? The file is deleted from the people folder too.`)) return;
		photoBusy = true;
		thumbModal.classList.add("locked");
		try {
			const r = await fetch(
				`/api/people/${encodeURIComponent(photoPerson)}/photos/${encodeURIComponent(photoPath)}`,
				{ method: "DELETE" });
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "delete failed");
			showToast(`Removed ${photoPath}.`, "ok");
			if (photoDetail && photoDetail.path === photoPath) {
				photoDetail = null;
				showPhotoGrid(); // the photo under inspection is gone — back to the grid
			}
			loadPhotoGrid(photoPerson);
			loadPeople();
			checkHealth();
		} catch (e) {
			showToast(e.message || "Could not remove the photo.", "err");
		} finally {
			photoBusy = false;
			thumbModal.classList.remove("locked");
		}
	}

	photoBackBtn.addEventListener("click", () => {
		photoDetail = null;
		showPhotoGrid();
	});
	photoDelBtn.addEventListener("click", () => {
		if (photoDetail) deletePhoto(photoDetail.path);
	});
	photoThumbBtn.addEventListener("click", async () => {
		if (photoBusy || !photoPerson || !photoDetail) return;
		photoBusy = true;
		thumbModal.classList.add("locked");
		try {
			const r = await fetch(`/api/people/${encodeURIComponent(photoPerson)}/thumbnail`, {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ photo: photoDetail.path }),
			});
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "could not update the avatar");
			showToast("Avatar updated.", "ok");
			loadPeople();
		} catch (e) {
			showToast(e.message || "Could not update the avatar.", "err");
		} finally {
			photoBusy = false;
			thumbModal.classList.remove("locked");
		}
	});

	// Add photos: browse, drag & drop anywhere on the modal, or paste.
	photoAdd.addEventListener("click", () => { if (!photoBusy) photoFiles.click(); });
	photoAdd.addEventListener("keydown", (e) => {
		if ((e.key === "Enter" || e.key === " ") && !photoBusy) {
			e.preventDefault();
			photoFiles.click();
		}
	});
	photoFiles.addEventListener("change", () => {
		if (photoFiles.files.length) addPhotos(photoFiles.files);
		photoFiles.value = "";
	});
	["dragenter", "dragover"].forEach((ev) =>
		thumbModal.addEventListener(ev, (e) => {
			e.preventDefault();
			if (!photoBusy && photoGridView && !photoGridView.hidden) photoAdd.classList.add("drag");
		})
	);
	thumbModal.addEventListener("dragleave", (e) => {
		if (!e.relatedTarget) photoAdd.classList.remove("drag");
	});
	thumbModal.addEventListener("drop", (e) => {
		e.preventDefault();
		photoAdd.classList.remove("drag");
		if (!photoBusy && photoGridView && !photoGridView.hidden &&
				e.dataTransfer && e.dataTransfer.files.length) {
			addPhotos(e.dataTransfer.files);
		}
	});

	// Upload the chosen files into the person's profile via the enroll
	// endpoint; only successfully enrolled photos land in the grid.
	async function addPhotos(fileList) {
		const files = Array.from(fileList).filter((f) => f.type.startsWith("image/"));
		if (!files.length) { showToast("No image files to add.", "err"); return; }
		photoBusy = true;
		thumbModal.classList.add("locked");
		photoHint.textContent = `Uploading ${files.length} photo${files.length === 1 ? "" : "s"}…`;
		const fd = new FormData();
		for (const f of files) fd.append("images", f, f.name);
		try {
			const r = await fetch(`/api/people/${encodeURIComponent(photoPerson)}/enroll`, {
				method: "POST", body: fd,
			});
			const j = await r.json();
			if (!r.ok && r.status !== 422) throw new Error(j.error || "upload failed");
			if (j.added > 0) {
				showToast(`Added ${j.added} photo${j.added === 1 ? "" : "s"}.`, "ok");
			}
			if (j.failures && j.failures.length) {
				showToast(`${j.failures.length} photo(s) rejected: ${j.failures[0]}`, "err");
			}
			loadPeople();
			checkHealth();
		} catch (e) {
			showToast(e.message || "Upload failed.", "err");
		} finally {
			photoBusy = false;
			thumbModal.classList.remove("locked");
			if (!thumbModal.hidden) loadPhotoGrid(photoPerson);
		}
	}

	thumbModal.addEventListener("click", (e) => {
		if (e.target === thumbModal || e.target.closest("[data-close]")) closePhotosModal();
	});

	// ---------- face-check viewer ----------
	// Enlarged preview used by the enroll modal: shows a pending photo with its
	// detected faces drawn over it before anything is uploaded. Read-only.
	let checkFaces = [];   // faces for the photo being viewed
	let checkSrc = "";     // image URL (object or server) currently shown
	let checkLastFocus = null;

	// openCheckModal({ src, title, faces }) — faces may arrive after the image.
	function openCheckModal({ src, title, faces }) {
		checkLastFocus = document.activeElement;
		checkTitle.textContent = title || "Face check";
		checkSrc = src;
		checkVerdict.textContent = faces ? "Loading…" : "Detecting faces…";
		checkVerdict.classList.remove("warn");
		checkImg.src = src;
		checkFaces = faces || null;
		if (faces) drawCheckOverlay(faces);
		checkModal.hidden = false;
		document.body.classList.add("modal-open");
		checkCard.querySelector(".modal-close").focus();
		if (faces) updateCheckVerdict(faces);
	}

	function closeCheckModal() {
		checkModal.hidden = true;
		checkImg.src = "";
		checkSrc = "";
		checkFaces = [];
		if (thumbModal.hidden && enrollModal.hidden) document.body.classList.remove("modal-open");
		// Return focus to the trigger: the enroll thumb when coming from the
		// enroll modal, otherwise back into the stacked photos modal.
		if (checkLastFocus && checkLastFocus.focus) checkLastFocus.focus();
		else if (!thumbModal.hidden) photoCard.focus();
	}

	function drawCheckOverlay(faces) {
		const draw = () => drawFaces(checkOverlay, checkImg, faces, { labels: true });
		if (checkImg.complete && checkImg.naturalWidth) draw();
		else checkImg.onload = draw;
	}

	function updateCheckVerdict(faces) {
		checkVerdict.classList.remove("warn");
		if (!faces.length) {
			checkVerdict.textContent = "No face detected — this photo will be rejected at enrollment.";
			checkVerdict.classList.add("warn");
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
		checkVerdict.textContent = `${lead} · ${parts.join(", ")}`;
	}

	checkModal.addEventListener("click", (e) => {
		if (e.target === checkModal || e.target.closest("[data-close]")) closeCheckModal();
	});

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
	trapTab(photoCard);
	trapTab(checkCard);

	// Escape closes the topmost open dialog only.
	document.addEventListener("keydown", (e) => {
		if (e.key !== "Escape") return;
		if (!checkModal.hidden) { closeCheckModal(); return; }
		if (!thumbModal.hidden) closePhotosModal();
		else if (!enrollModal.hidden) closeEnroll();
	});


	// ---------- enroll modal ----------
	// Photos chosen for enrollment are held client-side for review; the upload
	// only happens when the user confirms with the "Enroll" button. More photos
	// can be added at any time by drag & drop or the file input.
	const NAME_HINT = "Shown as the identity when their face is recognized.";
	let pending = []; // { key, file, url, li, canvas, badge, faces, checking }
	let enrolling = false;
	let lastFocus = null;

	enrollBtn.addEventListener("click", openEnroll);

	function openEnroll() {
		clearPending();
		enrollName.value = "";
		setNameHint();
		updateEnrollMeta();
		lastFocus = document.activeElement;
		enrollModal.hidden = false;
		document.body.classList.add("modal-open");
		enrollName.focus();
	}

	function closeEnroll() {
		if (enrolling) return; // locked while the request is in flight
		enrollModal.hidden = true;
		document.body.classList.remove("modal-open");
		clearPending();
		if (lastFocus && lastFocus.focus) lastFocus.focus();
	}

	function clearPending() {
		pending.forEach((p) => URL.revokeObjectURL(p.url));
		pending = [];
		enrollThumbs.innerHTML = "";
	}

	function setEnrollBusy(busy) {
		enrolling = busy;
		enrollModal.classList.toggle("locked", busy);
		enrollName.disabled = busy;
		enrollDrop.classList.toggle("disabled", busy);
	}

	function fmtSize(bytes) {
		return bytes >= 1024 * 1024
			? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
			: `${Math.max(1, Math.round(bytes / 1024))} KB`;
	}

	// Add files to the review list (no upload). Dedupes by name+size+mtime.
	// Each photo immediately gets a client-side face-check preview so the user
	// can see the detected faces before committing to enrollment.
	function addPending(fileList) {
		let added = 0, notImage = 0, duplicate = 0;
		for (const file of Array.from(fileList)) {
			if (!file.type.startsWith("image/")) { notImage++; continue; }
			const key = `${file.name}\n${file.size}\n${file.lastModified}`;
			if (pending.some((p) => p.key === key)) { duplicate++; continue; }
			const url = URL.createObjectURL(file);
			const p = { key, file, url, faces: null, checking: true };
			buildThumb(p);
			pending.push(p);
			enrollThumbs.appendChild(p.li);
			queueFaceCheck(p);
			added++;
		}
		if (notImage) showToast(`${notImage} file(s) skipped — not images.`, "err");
		if (duplicate && !added) showToast(`${duplicate} file(s) already selected.`);
		if (added) updateEnrollMeta();
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
			const fd = new FormData();
			fd.append("image", p.file, p.file.name);
			const r = await fetch("/api/recognize", { method: "POST", body: fd });
			const j = await r.json();
			if (!pending.includes(p)) return;
			if (!r.ok) throw new Error(j.error || "detection failed");
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
			if (!checkModal.hidden && checkSrc === p.url) {
				drawCheckOverlay(p.faces);
				updateCheckVerdict(p.faces);
			}
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
		const draw = () => drawFaces(p.canvas, p.li.querySelector("img"), p.faces || [], { labels: false });
		const img = p.li.querySelector("img");
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
		const openViewer = () => {
			openCheckModal({ src: p.url, title: p.file.name, faces: p.faces });
			checkLastFocus = li;
		};
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
		enrollThumbs.hidden = pending.length === 0;
		const n = pending.length;
		if (!n) {
			enrollMeta.textContent = "No photos selected yet.";
		} else {
			const bytes = pending.reduce((s, p) => s + p.file.size, 0);
			enrollMeta.textContent = `${n} photo${n === 1 ? "" : "s"} ready · ${fmtSize(bytes)} · review, then enroll`;
		}
		if (!enrolling) {
			enrollSubmit.disabled = !(n && enrollName.value.trim());
			enrollSubmit.textContent = n ? `Enroll ${n} photo${n === 1 ? "" : "s"}` : "Enroll photos";
		}
	}

	function setNameHint() {
		const name = enrollName.value.trim();
		const known = name && peopleNames.some((n) => n.toLowerCase() === name.toLowerCase());
		enrollNameHint.textContent = known
			? `${name} is already enrolled — photos will be added to their profile.`
			: NAME_HINT;
		enrollNameHint.classList.toggle("warn", Boolean(known));
	}

	enrollName.addEventListener("input", () => { setNameHint(); updateEnrollMeta(); });

	// dropzone: click / keyboard opens the picker; drag & drop appends files.
	enrollDrop.addEventListener("click", () => { if (!enrolling) enrollPhotos.click(); });
	enrollDrop.addEventListener("keydown", (e) => {
		if ((e.key === "Enter" || e.key === " ") && !enrolling) {
			e.preventDefault();
			enrollPhotos.click();
		}
	});
	enrollPhotos.addEventListener("change", () => {
		if (enrollPhotos.files.length) addPending(enrollPhotos.files);
		enrollPhotos.value = ""; // allow re-picking the same file later
	});

	// Drop anywhere on the modal adds photos; brackets highlight the dropzone.
	["dragenter", "dragover"].forEach((ev) =>
		enrollModal.addEventListener(ev, (e) => {
			e.preventDefault();
			if (!enrolling) enrollDrop.classList.add("drag");
		})
	);
	enrollModal.addEventListener("dragleave", (e) => {
		if (!e.relatedTarget) enrollDrop.classList.remove("drag");
	});
	enrollModal.addEventListener("drop", (e) => {
		e.preventDefault();
		enrollDrop.classList.remove("drag");
		if (!enrolling && e.dataTransfer && e.dataTransfer.files.length) {
			addPending(e.dataTransfer.files);
		}
	});

	// close: backdrop, ×/Cancel (Escape handled by the unified keydown above)
	enrollModal.addEventListener("click", (e) => {
		if (e.target === enrollModal || e.target.closest("[data-close]")) closeEnroll();
	});

	// keep Tab focus inside the dialog while it is open
	enrollCard.addEventListener("keydown", (e) => {
		if (e.key !== "Tab") return;
		const focusables = [...enrollCard.querySelectorAll(
			'button:not([hidden]):not(:disabled), input:not([hidden]):not(:disabled), [tabindex="0"]'
		)].filter((el) => el.offsetParent !== null);
		if (!focusables.length) return;
		const first = focusables[0];
		const last = focusables[focusables.length - 1];
		if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
		else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
	});

	// Submit uploads the reviewed photos — the only point where anything is sent.
	enrollForm.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (enrolling) return;
		const name = enrollName.value.trim();
		if (!name) { showToast("Enter the person's name first.", "err"); enrollName.focus(); return; }
		if (!pending.length) { showToast("Add at least one photo.", "err"); return; }

		setEnrollBusy(true);
		enrollSubmit.textContent = "Enrolling…";
		enrollMeta.textContent = `Enrolling ${pending.length} photo${pending.length === 1 ? "" : "s"} for ${name}…`;

		const fd = new FormData();
		for (const p of pending) fd.append("images", p.file, p.file.name);
		try {
			const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll`, { method: "POST", body: fd });
			const j = await r.json();
			if (!r.ok && r.status !== 422) throw new Error(j.error || "enroll failed");

			if (j.added > 0) showToast(`Enrolled ${j.added} photo${j.added === 1 ? "" : "s"} for ${name}.`, "ok");
			loadPeople();
			checkHealth();

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

	// ---------- clipboard paste ----------
	// Ctrl+V / Cmd+V routes by context: with the enroll modal open, pasted
	// images join the review list; with the photos manager open they upload
	// straight into that person; otherwise they are inspected on the stage.
	// Plain-text pastes into inputs are never hijacked.
	let pulseTimer = null;

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

	function pulseEnrollDrop() {
		enrollDrop.classList.add("pulse");
		clearTimeout(pulseTimer);
		pulseTimer = setTimeout(() => enrollDrop.classList.remove("pulse"), 900);
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

		if (!enrollModal.hidden) {
			if (enrolling) return;
			addPending(files);
			pulseEnrollDrop();
			if (enrollThumbs.lastElementChild) {
				enrollThumbs.lastElementChild.scrollIntoView({ block: "nearest" });
			}
			showToast(`Added ${files.length} photo${files.length === 1 ? "" : "s"} from clipboard.`, "ok");
		} else if (!thumbModal.hidden) {
			addPhotos(files);
		} else {
			handleFile(files[0]);
			if (files.length > 1) showToast("Clipboard had several images — inspecting the first.");
		}
	});

	rescanBtn.addEventListener("click", async () => {
		rescanBtn.disabled = true;
		rescanBtn.textContent = "Rescanning…";
		try {
			const r = await fetch("/api/enroll", { method: "POST" });
			const j = await r.json();
			if (!r.ok) throw new Error(j.error || "rescan failed");
			showToast(`Rescan done: ${j.added} new, ${j.kept} kept, ${j.failed} failed.`, "ok");
			loadPeople();
			checkHealth();
		} catch (e) {
			showToast(e.message, "err");
		} finally {
			rescanBtn.disabled = false;
			rescanBtn.textContent = "Rescan people folder";
		}
	});

	function escapeHtml(s) {
		return String(s).replace(/[&<>"']/g, (c) => ({
			"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
		}[c]));
	}

	// boot
	checkHealth();
	loadPeople();
	setInterval(checkHealth, 30000);
})();
