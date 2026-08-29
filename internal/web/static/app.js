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
  const addPersonForm = $("addPerson");
  const personName = $("personName");
  const personPhotos = $("personPhotos");
  const addHint = $("addHint");
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

  async function loadPeople() {
    try {
      const r = await fetch("/api/people");
      const j = await r.json();
      const people = j.people || [];
      peopleCount.textContent = people.length
        ? `${people.length} ${people.length === 1 ? "person" : "people"} in the database`
        : "No one enrolled yet.";
      peopleList.innerHTML = "";
      people.forEach((p) => {
        const li = document.createElement("li");
        li.className = "person-row";
        li.innerHTML = `
          <span class="person-avatar">${escapeHtml(initials(p.name))}</span>
          <div>
            <div class="person-name">${escapeHtml(p.name)}</div>
            <div class="person-count">${p.photos} photo(s)</div>
          </div>
          <button class="person-del" title="Remove ${escapeHtml(p.name)}" aria-label="Remove ${escapeHtml(p.name)}">×</button>`;
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

  // The add-person flow is two-step: enter a name, then "Add photos" opens a
  // file picker; choosing files enrolls them immediately.
  personPhotos.addEventListener("change", async () => {
    const name = personName.value.trim();
    if (!name) {
      showToast("Enter the person's name first.", "err");
      personPhotos.value = "";
      personName.focus();
      return;
    }
    if (!personPhotos.files.length) return;
    const fd = new FormData();
    for (const f of personPhotos.files) fd.append("images", f, f.name);
    addHint.textContent = `Enrolling ${personPhotos.files.length} photo(s) for ${name}…`;
    try {
      const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll`, { method: "POST", body: fd });
      const j = await r.json();
      if (!r.ok && r.status !== 422) throw new Error(j.error || "enroll failed");
      if (j.added > 0) {
        showToast(`Enrolled ${j.added} photo(s) for ${name}.`, "ok");
        personName.value = "";
      }
      if (j.failures && j.failures.length) {
        showToast(`${j.failures.length} photo(s) had no detectable face.`, "err");
      }
      addHint.textContent = "Pick a name, then choose one or more photos to enroll them.";
      loadPeople();
      checkHealth();
    } catch (e) {
      addHint.textContent = "Pick a name, then choose one or more photos to enroll them.";
      showToast(e.message, "err");
    }
    personPhotos.value = "";
  });

  addPersonForm.addEventListener("submit", (e) => e.preventDefault());

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
