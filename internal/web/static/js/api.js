/* recogn — thin wrappers around the REST API under /api.
	 Each throws Error(j.error || fallback) on !r.ok unless noted otherwise,
	 matching the error handling the call sites already had. */

async function parse(r) {
	return r.json().catch(() => ({}));
}

export async function getHealth() {
	const r = await fetch("/api/health");
	if (!r.ok) throw new Error("bad status");
	return parse(r);
}

export async function getPeople() {
	const r = await fetch("/api/people");
	return parse(r);
}

export async function getConfig() {
	const r = await fetch("/api/config");
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "could not read config");
	return j;
}

// Persist the match threshold server-side (survives restarts).
export async function setThreshold(value) {
	const r = await fetch("/api/config", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ threshold: value }),
	});
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "could not set the threshold");
	return j;
}

export async function getPerson(name) {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}`);
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "could not load photos");
	return j;
}

export async function recognize(file) {
	const fd = new FormData();
	fd.append("image", file, file.name);
	const r = await fetch("/api/recognize", { method: "POST", body: fd });
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "recognition failed");
	return j;
}

export async function deletePerson(name) {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}`, { method: "DELETE" });
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "delete failed");
	return j;
}

// Rename a person: server-side this moves their people/<name> folder,
// re-derives their ID, renames the thumbnail sidecar and updates the DB.
export async function renamePerson(oldName, newName) {
	const r = await fetch(`/api/people/${encodeURIComponent(oldName)}/rename`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name: newName }),
	});
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "rename failed");
	return j;
}

export function photoURL(name, path) {
	return `/api/people/${encodeURIComponent(name)}/photos/${encodeURIComponent(path)}`;
}

export async function detectPhoto(name, path) {
	const r = await fetch(photoURL(name, path) + "/detect");
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "detection failed");
	return j;
}

export async function deletePhoto(name, path) {
	const r = await fetch(photoURL(name, path), { method: "DELETE" });
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "delete failed");
	return j;
}

export async function setThumbnail(name, photoPath) {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/thumbnail`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ photo: photoPath }),
	});
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "could not update the avatar");
	return j;
}

// Enroll uploads. HTTP 422 (some photos rejected) is NOT thrown — the body
// carries { added, failures } that callers present to the user.
export async function enrollPhotos(name, files) {
	const fd = new FormData();
	for (const f of files) fd.append("images", f, f.name);
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll`, {
		method: "POST", body: fd,
	});
	const j = await parse(r);
	if (!r.ok && r.status !== 422) throw new Error(j.error || "upload failed");
	return j;
}

export async function rescan() {
	const r = await fetch("/api/enroll", { method: "POST" });
	const j = await parse(r);
	if (!r.ok) throw new Error(j.error || "rescan failed");
	return j;
}
