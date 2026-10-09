// People, person details, metadata, photos, and thumbnail API wrappers.

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
import { expectJSON, fireUnauthorized, parse, UnauthorizedError } from "./core";

export async function getPeople(): Promise<PeopleResponse> {
	const r = await fetch("/api/people");
	return parse<PeopleResponse>(r);
}

// Public person details: name, optional metadata (aliases, partial
// birthdate, URLs, markdown description) and the photo count. Enrolled photo
// paths are not part of this document — the photos manager uses
// getPersonPhotos (admin).
export async function getPerson(name: string): Promise<PersonDetail> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}`);
	return expectJSON(r, "could not load person details");
}

// Admin photos-manager payload: enrolled photo paths + hashes + thumb_src.
export async function getPersonPhotos(name: string): Promise<PersonPhotosResponse> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/photos`);
	return expectJSON(r, "could not load photos");
}

// Replace a person's metadata (admin; full-replace: omitted/empty fields
// clear the stored value). Returns the refreshed public details document.
export async function setPersonMeta(name: string, meta: MetaUpdate): Promise<PersonDetail> {
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/meta`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(meta),
	});
	return expectJSON(r, "could not save the details");
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

// Fetch an enrolled photo's bytes as a File (admin route; 401-aware) so
// server-side photos can feed tools that upload files, e.g. the compare tool.
// Photo paths are folder-relative basenames, so the File name is meaningful.
export async function fetchPhotoFile(name: string, path: string): Promise<File> {
	const r = await fetch(photoURL(name, path));
	if (r.status === 401) {
		fireUnauthorized();
		throw new UnauthorizedError("authentication required");
	}
	if (!r.ok) throw new Error("could not load the photo");
	const blob = await r.blob();
	return new File([blob], path, { type: blob.type || "image/jpeg" });
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
