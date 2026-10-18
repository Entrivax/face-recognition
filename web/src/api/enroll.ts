// Enrollment API wrappers: photo uploads, face enrollment, and folder rescan
// — all scoped to the selected repository (writer-only routes server-side).

import type {
	EnrollFaceResponse,
	EnrollResponse,
	RescanResponse,
} from "../types";
import { expectJSON, fireUnauthorized, parse, request, UnauthorizedError, withRepoQuery } from "./core";

// Enroll uploads into a person of this repo. HTTP 422 (some photos
// rejected) is NOT thrown — the body carries { added, failures } that
// callers present to the user. 401 still throws UnauthorizedError (and
// fires the hook) like the other writer calls.
export async function enrollPhotos(name: string, repoId: string, files: File[]): Promise<EnrollResponse> {
	const fd = new FormData();
	for (const f of files) fd.append("images", f, f.name);
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}/enroll`, repoId), {
		method: "POST",
		body: fd,
	});
	const j = await parse<EnrollResponse & { error?: string }>(r);
	if (r.status === 401) {
		fireUnauthorized();
		throw new UnauthorizedError(j.error || "upload failed");
	}
	if (!r.ok && r.status !== 422) throw new Error(j.error || "upload failed");
	return j;
}

// Enroll one specific face of an uploaded photo (1-based index into the
// detection order the results list showed) as a new or existing person.
export async function enrollFace(name: string, repoId: string, file: File, faceIndex: number): Promise<EnrollFaceResponse> {
	const fd = new FormData();
	fd.append("image", file, file.name || "photo.jpg");
	fd.append("face_index", String(faceIndex));
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}/enroll-face`, repoId), {
		method: "POST",
		body: fd,
	});
	return expectJSON(r, "enrollment failed");
}

// Re-scan the repository's dataset folder (writer-only).
export async function rescan(repoId: string): Promise<RescanResponse> {
	const r = await request(withRepoQuery("/api/enroll", repoId), { method: "POST" });
	return expectJSON(r, "rescan failed");
}
