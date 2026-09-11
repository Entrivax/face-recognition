// WebAuthn browser plumbing. The server speaks JSON with base64url-encoded
// buffers; navigator.credentials needs ArrayBuffer options and hands back
// live credential objects. These helpers translate both directions and wrap
// the get() (login assertion) / create() (registration attestation) calls.
//
// Availability (secure context + WebAuthn support) is checked at the call
// sites via webauthnAvailable() before offering passkey UI.

import type {
	CredentialCreationOptionsJSON,
	CredentialRequestOptionsJSON,
	PublicKeyCredentialJSON,
	PublicKeyCredentialRequestOptionsJSON,
	PublicKeyCredentialCreationOptionsJSON,
	PublicKeyCredentialDescriptorJSON,
} from "./types";

// Passkeys need a secure context (HTTPS or localhost) and the WebAuthn API.
export function webauthnAvailable(): boolean {
	return typeof window !== "undefined"
		&& window.isSecureContext
		&& typeof PublicKeyCredential !== "undefined";
}

// ---- base64url <-> bytes ----

export function base64urlToBuffer(value: string): ArrayBuffer {
	const b64 = value.replace(/-/g, "+").replace(/_/g, "/");
	const pad = b64.length % 4 === 0 ? "" : "=".repeat(4 - (b64.length % 4));
	const bin = atob(b64 + pad);
	const bytes = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
	return bytes.buffer;
}

export function bufferToBase64url(buf: ArrayBuffer): string {
	const bytes = new Uint8Array(buf);
	let bin = "";
	for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
	return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// ---- login: options JSON -> get() -> assertion JSON ----

export async function passkeyLogin(opts: CredentialRequestOptionsJSON): Promise<PublicKeyCredentialJSON> {
	const credential = await navigator.credentials.get({ publicKey: decodeRequestOptions(opts.publicKey) });
	if (!(credential instanceof PublicKeyCredential) || !("authenticatorData" in credential.response)) {
		throw new Error("No passkey credential was returned.");
	}
	return serializeAssertion(credential);
}

function decodeRequestOptions(json: PublicKeyCredentialRequestOptionsJSON): PublicKeyCredentialRequestOptions {
	const out: PublicKeyCredentialRequestOptions = {
		challenge: base64urlToBuffer(json.challenge),
	};
	if (json.rpId !== undefined) out.rpId = json.rpId;
	if (json.timeout !== undefined) out.timeout = json.timeout;
	if (json.userVerification !== undefined) out.userVerification = json.userVerification as UserVerificationRequirement;
	if (json.allowCredentials) out.allowCredentials = json.allowCredentials.map(decodeDescriptor);
	return out;
}

// ---- registration: options JSON -> create() -> attestation JSON ----

export async function passkeyRegister(opts: CredentialCreationOptionsJSON): Promise<PublicKeyCredentialJSON> {
	const credential = await navigator.credentials.create({ publicKey: decodeCreationOptions(opts.publicKey) });
	if (!(credential instanceof PublicKeyCredential) || !("attestationObject" in credential.response)) {
		throw new Error("No passkey credential was returned.");
	}
	return serializeAttestation(credential);
}

export function decodeCreationOptions(json: PublicKeyCredentialCreationOptionsJSON): PublicKeyCredentialCreationOptions {
	const out: PublicKeyCredentialCreationOptions = {
		challenge: base64urlToBuffer(json.challenge),
		rp: json.rp,
		user: {
			id: base64urlToBuffer(json.user.id),
			name: json.user.name,
			displayName: json.user.displayName,
		},
		pubKeyCredParams: json.pubKeyCredParams.map((p) => ({
			type: p.type as "public-key",
			alg: p.alg,
		})),
	};
	if (json.timeout !== undefined) out.timeout = json.timeout;
	if (json.excludeCredentials) out.excludeCredentials = json.excludeCredentials.map(decodeDescriptor);
	if (json.authenticatorSelection !== undefined) {
		out.authenticatorSelection = json.authenticatorSelection as AuthenticatorSelectionCriteria;
	}
	if (json.attestation !== undefined) out.attestation = json.attestation as AttestationConveyancePreference;
	return out;
}

function decodeDescriptor(d: PublicKeyCredentialDescriptorJSON): PublicKeyCredentialDescriptor {
	const out: PublicKeyCredentialDescriptor = {
		type: d.type as "public-key",
		id: base64urlToBuffer(d.id),
	};
	if (d.transports) out.transports = d.transports as PublicKeyCredentialDescriptor["transports"];
	return out;
}

// ---- credential -> JSON the server can parse ----

function serializeAssertion(credential: PublicKeyCredential): PublicKeyCredentialJSON {
	const r = credential.response as AuthenticatorAssertionResponse;
	const json: PublicKeyCredentialJSON = {
		id: credential.id,
		rawId: bufferToBase64url(credential.rawId),
		type: credential.type,
		response: {
			clientDataJSON: bufferToBase64url(r.clientDataJSON),
			authenticatorData: bufferToBase64url(r.authenticatorData),
			signature: bufferToBase64url(r.signature),
		},
	};
	if (r.userHandle) json.response.userHandle = bufferToBase64url(r.userHandle);
	return json;
}

function serializeAttestation(credential: PublicKeyCredential): PublicKeyCredentialJSON {
	const r = credential.response as AuthenticatorAttestationResponse;
	const json: PublicKeyCredentialJSON = {
		id: credential.id,
		rawId: bufferToBase64url(credential.rawId),
		type: credential.type,
		response: {
			clientDataJSON: bufferToBase64url(r.clientDataJSON),
			attestationObject: bufferToBase64url(r.attestationObject),
			authData: bufferToBase64url(r.getAuthenticatorData()),
			transports: r.getTransports(),
			publicKeyAlgorithm: r.getPublicKeyAlgorithm(),
		},
	};
	const pk = r.getPublicKey();
	if (pk) json.response.publicKey = bufferToBase64url(pk);
	return json;
}
