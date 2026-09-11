// Typed wrappers around the REST API under /api. Each throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect. Admin endpoints throw
// UnauthorizedError on 401 (and fire the onUnauthorized hook App registers
// via setOnUnauthorized) so the UI can flip to logged-out on session expiry.

import type {
	Config,
	CredentialCreationOptionsJSON,
	CredentialRequestOptionsJSON,
	DetectResponse,
	DeletePersonResponse,
	DeletePhotoResponse,
	EnrollFaceResponse,
	EnrollResponse,
	Health,
	OkResponse,
	PasskeysResponse,
	PeopleResponse,
	PersonDetail,
	PublicKeyCredentialJSON,
	RenameResponse,
	RecognizeResponse,
	RescanResponse,
	SessionInfo,
	ThumbResponse,
} from "./types";

// 401s from admin endpoints surface as this type so call sites can tell a
// dead session apart from other failures. App registers the hook below and
// flips to logged-out whenever it fires.
export class UnauthorizedError extends Error {
	constructor(message = "authentication required") {
		super(message);
		this.name = "UnauthorizedError";
	}
}

let onUnauthorized: (() => void) | null = null;

/** App registers a callback that fires (once per 401) on session expiry. */
export function setOnUnauthorized(fn: (() => void) | null): void {
	onUnauthorized = fn;
}

async function parse<T>(r: Response): Promise<T> {
	return (await r.json().catch(() => ({}))) as T;
}

async function expectJSON<T>(r: Response, fallback: string): Promise<T> {
	const j = await parse<T & { error?: string }>(r);
	if (r.status === 401) {
		onUnauthorized?.();
		throw new UnauthorizedError(j.error || fallback);
	}
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
// carries { added, failures } that callers present to the user. 401 still
// throws UnauthorizedError (and fires the hook) like the other admin calls.
export async function enrollPhotos(name: string, files: File[]): Promise<EnrollResponse> {
	const fd = new FormData();
	for (const f of files) fd.append("images", f, f.name);
	const r = await fetch(`/api/people/${encodeURIComponent(name)}/enroll`, {
		method: "POST",
		body: fd,
	});
	const j = await parse<EnrollResponse & { error?: string }>(r);
	if (r.status === 401) {
		onUnauthorized?.();
		throw new UnauthorizedError(j.error || "upload failed");
	}
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

// ---- auth ----
// Public endpoints (session, login, logout, passkey login) never fire the
// onUnauthorized hook: a 401 from a login attempt is an ordinary failure,
// not an expired session.

export async function getSession(): Promise<SessionInfo> {
	const r = await fetch("/api/auth/session");
	if (!r.ok) throw new Error("could not read the session");
	return parse<SessionInfo>(r);
}

// Password login. 401 = wrong password, 429 = too many attempts; the server's
// error message is preferred, with graceful fallbacks when it omits one.
export async function login(password: string): Promise<OkResponse> {
	const r = await fetch("/api/login", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ password }),
	});
	const j = await parse<OkResponse & { error?: string }>(r);
	if (!r.ok) {
		const fallback = r.status === 429
			? "Too many attempts — wait a moment and try again."
			: "Login failed.";
		throw new Error(j.error || fallback);
	}
	return j;
}

export async function logout(): Promise<OkResponse> {
	const r = await fetch("/api/logout", { method: "POST" });
	return expectJSON(r, "logout failed");
}

// ---- passkeys (WebAuthn JSON, base64url buffers) ----

// Step 1 of passkey sign-in: the server's CredentialRequestOptions JSON.
export async function passkeyLoginBegin(): Promise<CredentialRequestOptionsJSON> {
	const r = await fetch("/api/auth/passkey/login/begin", { method: "POST" });
	return expectJSON(r, "could not start the passkey sign-in");
}

// The finished assertion is the browser's PublicKeyCredential as JSON; a 401
// here means the signature was rejected, not that the session expired.
export async function passkeyLoginFinish(assertion: PublicKeyCredentialJSON): Promise<OkResponse> {
	const r = await fetch("/api/auth/passkey/login/finish", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(assertion),
	});
	const j = await parse<OkResponse & { error?: string }>(r);
	if (!r.ok) throw new Error(j.error || "Passkey sign-in failed.");
	return j;
}

// Step 1 of registering a new passkey (admin).
export async function passkeyRegisterBegin(): Promise<CredentialCreationOptionsJSON> {
	const r = await fetch("/api/auth/passkey/register/begin", { method: "POST" });
	return expectJSON(r, "could not start the passkey registration");
}

// Step 2 (admin): hand the browser's attestation back for verification.
export async function passkeyRegisterFinish(attestation: PublicKeyCredentialJSON): Promise<OkResponse> {
	const r = await fetch("/api/auth/passkey/register/finish", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(attestation),
	});
	return expectJSON(r, "passkey registration failed");
}

export async function listPasskeys(): Promise<PasskeysResponse> {
	const r = await fetch("/api/auth/passkeys");
	return expectJSON(r, "could not load the passkeys");
}

export async function deletePasskey(id: string): Promise<OkResponse> {
	const r = await fetch(`/api/auth/passkeys/${encodeURIComponent(id)}`, { method: "DELETE" });
	return expectJSON(r, "could not remove the passkey");
}
