// Typed wrappers around the REST API under /api. Each throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect.

import type {
	Config,
	DetectResponse,
	DeletePersonResponse,
	DeletePhotoResponse,
	EnrollFaceResponse,
	EnrollResponse,
	Health,
	PeopleResponse,
	PersonDetail,
	RenameResponse,
	RecognizeResponse,
	RescanResponse,
	ThumbResponse,
} from "./types";

async function parse<T>(r: Response): Promise<T> {
	return (await r.json().catch(() => ({}))) as T;
}

async function expectJSON<T>(r: Response, fallback: string): Promise<T> {
	const j = await parse<T & { error?: string }>(r);
	if (!r.ok) throw new Error(j.error || fallback);
	return j;
}

export async function getHealth(): Promise<Health> {
	const r = await fetch("/api/health");
	if (!r.ok) throw new Error("bad status");
	return parse<Health>(r);
}

export async function getPeople(): Promise<PeopleResponse> {
	const r = await fetch("/api/people");
	return parse<PeopleResponse>(r);
}

export async function getConfig(): Promise<Config> {
	const r = await fetch("/api/config");
	return expectJSON(r, "could not read config");
}

// Persist the match threshold server-side (survives restarts).
export async function setThreshold(value: number): Promise<Config> {
	const r = await fetch("/api/config", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ threshold: value }),
	});
	return expectJSON(r, "could not set the threshold");
}

export async function getPerson(name: string): Promise<PersonDetail> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}`);
	return expectJSON(r, "could not load photos");
}

export async function recognize(file: File): Promise<RecognizeResponse> {
	const fd = new FormData();
	fd.append("image", file, file.name);
	const r = await fetch("/api/recognize", { method: "POST", body: fd });
	return expectJSON(r, "recognition failed");
}

export async function deletePerson(name: string): Promise<DeletePersonResponse> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}`, { method: "DELETE" });
	return expectJSON(r, "delete failed");
}

// Rename a person: server-side this moves their people/<name> folder,
// re-derives their ID, renames the thumbnail sidecar and updates the DB.
export async function renamePerson(oldName: string, newName: string): Promise<RenameResponse> {
	const r = await fetch(`/api/people/${encodeURIComponent(oldName)}/rename`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name: newName }),
	});
	return expectJSON(r, "rename failed");
}

export function photoURL(name: string, path: string): string {
	return `/api/people/${encodeURIComponent(name)}/photos/${encodeURIComponent(path)}`;
}

export async function detectPhoto(name: string, path: string): Promise<DetectResponse> {
	const r = await fetch(photoURL(name, path) + "/detect");
	return expectJSON(r, "detection failed");
}

export async function deletePhoto(name: string, path: string): Promise<DeletePhotoResponse> {
	const r = await fetch(photoURL(name, path), { method: "DELETE" });
	return expectJSON(r, "delete failed");
}

export async function setThumbnail(name: string, photoPath: string): Promise<ThumbResponse> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/thumbnail`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ photo: photoPath }),
	});
	return expectJSON(r, "could not update the avatar");
}

// Enroll uploads. HTTP 422 (some photos rejected) is NOT thrown — the body
// carries { added, failures } that callers present to the user.
export async function enrollPhotos(name: string, files: File[]): Promise<EnrollResponse> {
	const fd = new FormData();
	for (const f of files) fd.append("images", f, f.name);
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll`, {
		method: "POST",
		body: fd,
	});
	const j = await parse<EnrollResponse & { error?: string }>(r);
	if (!r.ok && r.status !== 422) throw new Error(j.error || "upload failed");
	return j;
}

// Enroll one specific face of an uploaded photo (1-based index into the
// detection order the results list showed) as a new or existing person.
export async function enrollFace(name: string, file: File, faceIndex: number): Promise<EnrollFaceResponse> {
	const fd = new FormData();
	fd.append("image", file, file.name || "photo.jpg");
	fd.append("face_index", String(faceIndex));
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll-face`, {
		method: "POST",
		body: fd,
	});
	return expectJSON(r, "enrollment failed");
}

export async function rescan(): Promise<RescanResponse> {
	const r = await fetch("/api/enroll", { method: "POST" });
	return expectJSON(r, "rescan failed");
}
