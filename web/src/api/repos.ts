// Repositories (per-repo datasets of people) and user administration.
// Any signed-in user may list the repositories they were granted (the admin
// sees all of them); everything else here is admin-only server-side and the
// 401s surface through the usual session-expiry hook.

import type {
	CreateRepoResponse,
	DeleteRepoResponse,
	GrantBody,
	OkResponse,
	RenameRepoResponse,
	RepoResponse,
	ReposResponse,
	ShareRepoResponse,
	UsersResponse,
} from "../types";
import { expectJSON, request, withRepoQuery } from "./core";

// ---- repositories ----

export async function listRepos(): Promise<ReposResponse> {
	const r = await request("/api/repos");
	return expectJSON(r, "could not load repositories");
}

// Reader+ (share tokens included): one repository's info in one document —
// id, display name, the match threshold and its people list. The app store
// and the share view both boot off this call.
export async function getRepo(repoId: string): Promise<RepoResponse> {
	const r = await request(withRepoQuery("/api/repo", repoId));
	return expectJSON(r, "could not load the repository");
}

export async function createRepo(name: string): Promise<CreateRepoResponse> {
	const r = await request("/api/repos", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name }),
	});
	return expectJSON(r, "could not create the repository");
}

export async function renameRepo(id: string, name: string): Promise<RenameRepoResponse> {
	const r = await request(`/api/repos/${encodeURIComponent(id)}/rename`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name }),
	});
	return expectJSON(r, "could not rename the repository");
}

// DESTRUCTIVE: removes the dataset folder and everyone enrolled in it.
export async function deleteRepo(id: string): Promise<DeleteRepoResponse> {
	const r = await request(`/api/repos/${encodeURIComponent(id)}`, { method: "DELETE" });
	return expectJSON(r, "could not delete the repository");
}

// Enable/disable the public read-only share link. Every enable mints a
// FRESH token (share_url is the full absolute URL); disabling nulls it.
// The current link stays re-readable through getRepoShare — enabling is
// only needed to rotate it.
export async function setRepoShare(id: string, enabled: boolean): Promise<ShareRepoResponse> {
	const r = await request(`/api/repos/${encodeURIComponent(id)}/share`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ enabled }),
	});
	return expectJSON(r, "could not update the share link");
}

// Re-read the repository's current share link (same token as the last
// enable — reveal never rotates; share_url is null while unshared).
export async function getRepoShare(id: string): Promise<ShareRepoResponse> {
	const r = await request(`/api/repos/${encodeURIComponent(id)}/share`);
	return expectJSON(r, "could not read the share link");
}

// ---- users (admin) ----

export async function listUsers(): Promise<UsersResponse> {
	const r = await request("/api/users");
	return expectJSON(r, "could not load users");
}

export async function createUser(username: string, password: string, admin: boolean): Promise<OkResponse> {
	const r = await request("/api/users", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ username, password, admin }),
	});
	return expectJSON(r, "could not create the user");
}

// Deleting an admin is refused only for the caller's own account and the
// last remaining admin (server-side rule).
export async function deleteUser(username: string): Promise<OkResponse> {
	const r = await request(`/api/users/${encodeURIComponent(username)}`, { method: "DELETE" });
	return expectJSON(r, "could not delete the user");
}

// Admin password reset — no current-password check (that's the point).
// Sessions are revoked; the user's API keys are left intact and the reply's
// notice reminds the caller to have the user review them.
export async function resetUserPassword(username: string, password: string): Promise<OkResponse> {
	const r = await request(`/api/users/${encodeURIComponent(username)}/password`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ password }),
	});
	return expectJSON(r, "could not reset the password");
}

// FULL-REPLACE of one user's grants: the body lists every repository they
// should have access to, with its role — repos left out lose access.
export async function setUserGrants(username: string, grants: GrantBody[]): Promise<OkResponse> {
	const r = await request(`/api/users/${encodeURIComponent(username)}/repos`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ grants }),
	});
	return expectJSON(r, "could not update the grants");
}
