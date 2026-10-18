// People, person details, metadata, photos, and thumbnail API wrappers.
// Every data route is repo-scoped: callers pass the selected repository's
// id and the wrapper appends ?repo=<id>.

import type {
	DeletePersonResponse,
	DeletePhotoResponse,
	DetectResponse,
	MetaUpdate,
	PeopleResponse,
	PersonDetail,
	PersonPhotosResponse,
	RenameResponse,
	ThumbResponse,
} from "../types";
import { expectJSON, fireUnauthorized, request, UnauthorizedError, withRepoQuery } from "./core";

// Reader+ (readers included): the repo's people list.
export async function getPeople(repoId: string): Promise<PeopleResponse> {
	const r = await request(withRepoQuery("/api/people", repoId));
	return expectJSON(r, "could not load people");
}

// Reader+: person details (name, optional metadata — aliases, partial
// birthdate, URLs, markdown description — plus the photo count and the
// repo this person belongs to). Enrolled photo paths are not part of this
// document — the photos manager uses getPersonPhotos (writer-only).
export async function getPerson(name: string, repoId: string): Promise<PersonDetail> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}`, repoId));
	return expectJSON(r, "could not load person details");
}

// Writer-only photos-manager payload: enrolled photo paths + hashes + thumb_src.
export async function getPersonPhotos(name: string, repoId: string): Promise<PersonPhotosResponse> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}/photos`, repoId));
	return expectJSON(r, "could not load photos");
}

// Replace a person's metadata (writer/admin; full-replace: omitted/empty
// fields clear the stored value). Returns the refreshed details document.
export async function setPersonMeta(name: string, repoId: string, meta: MetaUpdate): Promise<PersonDetail> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}/meta`, repoId), {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(meta),
	});
	return expectJSON(r, "could not save the details");
}

export async function deletePerson(name: string, repoId: string): Promise<DeletePersonResponse> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}`, repoId), { method: "DELETE" });
	return expectJSON(r, "delete failed");
}

// Rename a person: server-side this moves their folder in the repo,
// re-derives their ID, renames the thumbnail sidecar and updates the DB.
export async function renamePerson(oldName: string, repoId: string, newName: string): Promise<RenameResponse> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(oldName)}/rename`, repoId), {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name: newName }),
	});
	return expectJSON(r, "rename failed");
}

export function photoURL(name: string, path: string): string {
	return `/api/people/${encodeURIComponent(name)}/photos/${encodeURIComponent(path)}`;
}

// Fetch an enrolled photo's bytes as a File (writer-only route; 401-aware)
// so server-side photos can feed tools that upload files, e.g. the compare
// tool. Photo paths are folder-relative basenames, so the File name is
// meaningful.
export async function fetchPhotoFile(name: string, repoId: string, path: string): Promise<File> {
	const r = await request(withRepoQuery(photoURL(name, path), repoId));
	if (r.status === 401) {
		fireUnauthorized();
		throw new UnauthorizedError("authentication required");
	}
	if (!r.ok) throw new Error("could not load the photo");
	const blob = await r.blob();
	return new File([blob], path, { type: blob.type || "image/jpeg" });
}

// Reader+ (readers included): run detection over an enrolled photo — the
// photos manager uses it for the per-photo quality verdict.
export async function detectPhoto(name: string, repoId: string, path: string): Promise<DetectResponse> {
	const r = await request(withRepoQuery(photoURL(name, path) + "/detect", repoId));
	return expectJSON(r, "detection failed");
}

export async function deletePhoto(name: string, repoId: string, path: string): Promise<DeletePhotoResponse> {
	const r = await request(withRepoQuery(photoURL(name, path), repoId), { method: "DELETE" });
	return expectJSON(r, "delete failed");
}

export async function setThumbnail(name: string, repoId: string, photoPath: string): Promise<ThumbResponse> {
	const r = await request(withRepoQuery(`/api/people/${encodeURIComponent(name)}/thumbnail`, repoId), {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ photo: photoPath }),
	});
	return expectJSON(r, "could not update the avatar");
}

// In share mode thumbnails cannot be plain <img> URLs: the share token
// rides a request header, which an <img> element cannot carry — the bytes
// are fetched through request() (header attached, no cookies) and handed to
// the DOM as an object URL instead.
export async function fetchThumbObjectURL(src: string): Promise<string> {
	const r = await request(src);
	if (!r.ok) throw new Error("could not load the thumbnail");
	const blob = await r.blob();
	return URL.createObjectURL(blob);
}
