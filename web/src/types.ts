// API data-transfer types, mirroring the JSON shapes internal/api/api.go emits.

export interface Health {
	status: string;
	people: number;
	threshold: number;
	/** which admin-login methods the server has configured (absent = open mode) */
	auth?: AuthMethods;
}

/** Which admin-login methods the server has configured. */
export interface AuthMethods {
	password: boolean;
	passkey: boolean;
}

/** GET /api/auth/session — session state of this browser. */
export interface SessionInfo {
	authenticated: boolean;
	methods: AuthMethods;
}

/** One passkey credential registered on the server (GET /api/auth/passkeys). */
export interface PasskeyInfo {
	id: string; // base64url credential ID
	added_at: string; // RFC3339
}

export interface PasskeysResponse {
	passkeys: PasskeyInfo[];
}

/** {"ok":true} replies from the auth endpoints. */
export interface OkResponse {
	ok: boolean;
}

export interface Config {
	threshold: number;
}

export interface PersonSummary {
	id: string;
	name: string;
	photos: number;
	embeddings: number;
	thumb: string;
}

export interface PeopleResponse {
	people: PersonSummary[];
	count: number;
}

export interface PersonPhoto {
	path: string;
	hash: string;
}

export interface PersonDetail {
	id: string;
	name: string;
	thumb_src: string;
	photos: PersonPhoto[];
}

export interface Match {
	person_id: string;
	name: string;
	score: number;
}

export interface Face {
	bbox: [number, number, number, number]; // x, y, width, height
	score: number; // detector confidence 0..1
	landmarks: [number, number][]; // 5-point landmarks
	name: string; // matched identity, or "unknown"
	person_id: string; // matched person's ID, or ""
	confidence: number; // best cosine similarity 0..1
	matches?: Match[]; // every identity above the threshold, ranked best-first
}

export interface RecognizeResponse {
	count: number;
	faces: Face[];
}

export interface DetectResponse {
	photo: string;
	count: number;
	faces: Face[];
}

/** One detected face in a compared photo (POST /api/compare). */
export interface CompareFaceInfo {
	index: number; // 1-based detection order
	bbox: [number, number, number, number]; // x, y, width, height
	score: number; // detector confidence 0..1
	used: boolean; // the largest face — the one the comparison embedded
}

/** The faces found in one of the two compared photos. */
export interface CompareImageInfo {
	count: number;
	faces: CompareFaceInfo[];
}

/** POST /api/compare — cosine similarity of the two photos' largest faces. */
export interface CompareResponse {
	similarity: number; // cosine similarity, roughly -1..1 (practically 0..1)
	threshold: number; // recognizer's match threshold, as a reference for the verdict
	image1: CompareImageInfo;
	image2: CompareImageInfo;
}

export interface EnrollResponse {
	person: string;
	added: number;
	total: number;
	saved: string[];
	failures?: string[];
}

export interface EnrollFaceResponse {
	person: string;
	saved: string;
}

export interface DeletePersonResponse {
	removed: string;
	folder_removed: boolean;
}

export interface RenameResponse {
	renamed: boolean;
	old_name: string;
	name: string;
	id: string;
}

export interface DeletePhotoResponse {
	removed: string;
	photos: number;
}

export interface ThumbResponse {
	person: string;
	thumb: string;
}

export interface RescanResponse {
	people: number;
	added: number;
	kept: number;
	failed: number;
	pruned: number;
	skipped_count: number;
}

// A photo opened in the enlarged face-check viewer. Faces may arrive after
// the viewer opens (the enroll modal's check chain reports them later).
export interface CheckView {
	src: string;
	title: string;
	faces: Face[] | null;
}

// ---- WebAuthn wire types ----
// The passkey endpoints speak JSON with base64url-encoded buffers, so the
// server's PublicKeyCredential{Creation,Request}Options can't be fed to
// navigator.credentials directly — webauthnClient.ts decodes them into the
// DOM types (preferred wherever the DOM lib suffices). These mirror the
// serialized options and credentials exchanged with the server.

/** Wrapper the begin endpoints return: { "publicKey": {...} }. */
export interface CredentialRequestOptionsJSON {
	publicKey: PublicKeyCredentialRequestOptionsJSON;
}

export interface CredentialCreationOptionsJSON {
	publicKey: PublicKeyCredentialCreationOptionsJSON;
}

export interface PublicKeyCredentialRequestOptionsJSON {
	challenge: string; // base64url
	rpId?: string;
	timeout?: number;
	userVerification?: string;
	allowCredentials?: PublicKeyCredentialDescriptorJSON[];
}

export interface PublicKeyCredentialCreationOptionsJSON {
	challenge: string; // base64url
	rp: { id?: string; name: string };
	user: { id: string; name: string; displayName: string }; // id base64url
	pubKeyCredParams: { type: string; alg: number }[];
	timeout?: number;
	excludeCredentials?: PublicKeyCredentialDescriptorJSON[];
	authenticatorSelection?: {
		authenticatorAttachment?: string;
		requireResidentKey?: boolean;
		residentKey?: string;
		userVerification?: string;
	};
	attestation?: string;
}

export interface PublicKeyCredentialDescriptorJSON {
	type: string;
	id: string; // base64url
	transports?: string[];
}

/** A PublicKeyCredential assertion/attestation serialized to JSON. */
export interface PublicKeyCredentialJSON {
	id: string;
	rawId: string; // base64url
	type: string;
	response: {
		clientDataJSON: string; // base64url
		// assertion fields
		authenticatorData?: string; // base64url
		signature?: string; // base64url
		userHandle?: string; // base64url
		// attestation fields
		attestationObject?: string; // base64url
		authData?: string; // base64url
		transports?: string[];
		publicKeyAlgorithm?: number;
		publicKey?: string; // base64url SubjectPublicKeyInfo
	};
}
