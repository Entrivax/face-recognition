// Auth API wrappers: session probe, first-run setup, login/logout, password
// change, and personal API keys.
//
// setup/login rejections (401/403/429) are ordinary form failures — they
// never fire the onUnauthorized hook. Everything else (logout, password
// change, keys) goes through expectJSON: a 401 there means the session
// expired, and the hook flips the app back to the login screen.

import type {
	ApiKeyResponse,
	KeysResponse,
	LoginResponse,
	OkResponse,
	SessionInfo,
	SetupBody,
} from "../types";
import { expectJSON, parse, request } from "./core";

export async function getSession(): Promise<SessionInfo> {
	const r = await request("/api/auth/session");
	if (!r.ok) throw new Error("could not read the session");
	return parse<SessionInfo>(r);
}

// First-run setup: creates the administrator account with the one-time
// setup key the server printed to its console. A 403 (wrong key, or setup
// already completed) is an ordinary error the form displays.
export async function setup(body: SetupBody): Promise<OkResponse> {
	const r = await request("/api/setup", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(body),
	});
	const j = await parse<OkResponse & { error?: string }>(r);
	if (!r.ok) {
		const fallback = r.status === 403
			? "Setup refused — check the setup key."
			: "Setup failed.";
		throw new Error(j.error || fallback);
	}
	return j;
}

// Password login. 401 = unknown user or wrong password, 429 = rate limited
// (the server sends Retry-After); the server's message wins when it has one.
export async function login(username: string, password: string): Promise<LoginResponse> {
	const r = await request("/api/login", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ username, password }),
	});
	const j = await parse<LoginResponse & { error?: string }>(r);
	if (!r.ok) {
		const fallback = r.status === 429
			? "Too many attempts — wait a moment and try again."
			: "Login failed.";
		throw new Error(j.error || fallback);
	}
	return j;
}

export async function logout(): Promise<OkResponse> {
	const r = await request("/api/logout", { method: "POST" });
	return expectJSON(r, "logout failed");
}

// Change the signed-in user's own password. 403 = wrong current password
// (an ordinary form error), 401 = the session expired.
export async function changePassword(current: string, newPassword: string): Promise<OkResponse> {
	const r = await request("/api/auth/password", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ current, new: newPassword }),
	});
	return expectJSON(r, "could not change the password");
}

// ---- personal API keys (any signed-in user, own keys only) ----

export async function listApiKeys(): Promise<KeysResponse> {
	const r = await request("/api/auth/keys");
	return expectJSON(r, "could not load the API keys");
}

// The full token (rk_…) comes back exactly once, with this reply — the
// caller shows it immediately with a store-it-now note.
export async function createApiKey(name: string): Promise<ApiKeyResponse> {
	const r = await request("/api/auth/keys", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ name }),
	});
	return expectJSON(r, "could not create the API key");
}

export async function deleteApiKey(id: string): Promise<OkResponse> {
	const r = await request(`/api/auth/keys/${encodeURIComponent(id)}`, { method: "DELETE" });
	return expectJSON(r, "could not revoke the API key");
}
