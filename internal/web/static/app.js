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
      li.innerHTML = `
        <span class="face-index">${String(i + 1).padStart(2, "0")}</span>
        <div>
          <div class="face-name">${escapeHtml(f.name)}</div>
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
    const draw = () => {
      const w = previewImg.naturalWidth;
      const h = previewImg.naturalHeight;
      if (!w || !h) return;
      overlay.width = w;
      overlay.height = h;
      const ctx = overlay.getContext("2d");
      ctx.clearRect(0, 0, w, h);
      const scale = Math.max(w, h) / 900; // line width scales with image size

      faces.forEach((f) => {
        const [x, y, bw, bh] = f.bbox;
        const known = f.name !== "unknown";
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

        // label
        ctx.shadowBlur = 0;
        const label = known
          ? `${f.name} ${(f.confidence * 100).toFixed(0)}%`
          : `unknown ${(f.confidence * 100).toFixed(0)}%`;
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
    };
    if (previewImg.complete && previewImg.naturalWidth) draw();
    else previewImg.onload = draw;
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
          ${avatar}
          <div>
            <div class="person-name">${escapeHtml(p.name)}</div>
            <div class="person-count">${p.photos} photo(s)</div>
          </div>
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

  // ---------- enroll modal ----------
  // Photos chosen for enrollment are held client-side for review; the upload
  // only happens when the user confirms with the "Enroll" button. More photos
  // can be added at any time by drag & drop or the file input.
  const NAME_HINT = "Shown as the identity when their face is recognized.";
  let pending = []; // { key, file, url, li }
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
  function addPending(fileList) {
    let added = 0, notImage = 0, duplicate = 0;
    for (const file of Array.from(fileList)) {
      if (!file.type.startsWith("image/")) { notImage++; continue; }
      const key = `${file.name}\n${file.size}\n${file.lastModified}`;
      if (pending.some((p) => p.key === key)) { duplicate++; continue; }
      const url = URL.createObjectURL(file);
      const li = buildThumb(file, url);
      pending.push({ key, file, url, li });
      enrollThumbs.appendChild(li);
      added++;
    }
    if (notImage) showToast(`${notImage} file(s) skipped — not images.`, "err");
    if (duplicate && !added) showToast(`${duplicate} file(s) already selected.`);
    if (added) updateEnrollMeta();
  }

  function buildThumb(file, url) {
    const li = document.createElement("li");
    li.className = "enroll-thumb";

    const img = document.createElement("img");
    img.src = url;
    img.alt = "";

    const del = document.createElement("button");
    del.type = "button";
    del.className = "enroll-thumb-del";
    del.textContent = "×";
    del.setAttribute("aria-label", `Remove ${file.name}`);
    del.addEventListener("click", () => removePending(file));

    const meta = document.createElement("div");
    meta.className = "enroll-thumb-meta";
    const name = document.createElement("div");
    name.className = "enroll-thumb-name";
    name.textContent = file.name;
    name.title = file.name;
    const size = document.createElement("div");
    size.className = "enroll-thumb-size";
    size.textContent = fmtSize(file.size);
    meta.append(name, size);

    const note = document.createElement("p");
    note.className = "enroll-thumb-note";

    li.append(img, del, meta, note);
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

  // close: backdrop, ×/Cancel, Escape
  enrollModal.addEventListener("click", (e) => {
    if (e.target === enrollModal || e.target.closest("[data-close]")) closeEnroll();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !enrollModal.hidden) closeEnroll();
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
  // images join the review list; otherwise they are inspected on the stage.
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
