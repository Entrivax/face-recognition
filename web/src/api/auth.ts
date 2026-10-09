// Auth and passkey (WebAuthn) API wrappers.
// Public endpoints (session, login, logout, passkey login) never fire the
// onUnauthorized hook: a 401 from a login attempt is an ordinary failure,
// not an expired session.

import type {
	CredentialCreationOptionsJSON,
	CredentialRequestOptionsJSON,
	OkResponse,
	PasskeysResponse,
	PublicKeyCredentialJSON,
	SessionInfo,
} from "../types";
import { expectJSON, parse } from "./core";

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
