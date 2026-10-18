// API data-transfer types, mirroring the JSON shapes the server emits.
//
// The app is multi-user: a session names a user (with an admin flag), the
// face data lives in repositories (per-repo datasets of people), and the
// admin surface manages users, per-repo grants and personal API keys. Auth
// is a password login via the HttpOnly recogn_session cookie; a share link
// (repoID + token) grants read-only access without any session.

// ---- auth / boot ----

/** GET /api/health — liveness only ({"status":"ok"}). */
export interface Health {
	status: string;
}

/** The signed-in user (GET /api/auth/session, POST /api/login). */
export interface SessionUser {
	name: string;
	admin: boolean;
}

/** GET /api/auth/session — this browser's session state. */
export interface SessionInfo {
	authenticated: boolean;
	user: SessionUser | null;
	/** true on a fresh deployment: the administrator account still has to be created */
	setup_required: boolean;
}

/** POST /api/setup body — first-run administrator creation. */
export interface SetupBody {
	key: string;
	username: string;
	password: string;
}

/** POST /api/login reply. */
export interface LoginResponse {
	ok: boolean;
	user: SessionUser;
}

/** {"ok":true} replies from the auth endpoints. */
export interface OkResponse {
	ok: boolean;
}

// ---- repositories ----

/** A viewer's role within one repository. */
export type RepoRole = "reader" | "writer" | "admin";

/** One repository in GET /api/repos (admin sees every repo; users see their grants). */
export interface RepoInfo {
	id: string;
	name: string;
	people: number;
	/** the admin's share link is enabled (visible on admin listings only) */
	shared: boolean;
	/** this viewer's role in the repository */
	role: RepoRole;
}

export interface ReposResponse {
	repos: RepoInfo[];
}

/** POST /api/repos reply. */
export interface CreateRepoResponse {
	id: string;
	name: string;
	people: number;
}

/** POST /api/repos/{id}/rename reply. */
export interface RenameRepoResponse {
	id: string;
	name: string;
}

/** DELETE /api/repos/{id} reply — destructive (removes the dataset folder + people). */
export interface DeleteRepoResponse {
	ok: boolean;
	removed: string;
	people_removed: number;
	folder_removed: boolean;
}

/** GET/POST /api/repos/{id}/share reply — share_url of the current link (null while unshared; POST-enable always mints a fresh one). */
export interface ShareRepoResponse {
	id: string;
	shared: boolean;
	share_url: string | null;
}

// ---- users (admin) ----

/** A grant role for repo access ("admin" is never grantable). */
export type GrantRole = "reader" | "writer";

/** One repo grant of a user. */
export interface UserGrant {
	repo_id: string;
	role: RepoRole;
}

/** One user in GET /api/users. */
export interface UserInfo {
	username: string;
	admin: boolean;
	grants: UserGrant[];
	created_at: string;
}

export interface UsersResponse {
	users: UserInfo[];
}

/** POST /api/users/{u}/repos body — FULL-REPLACE of that user's grants. */
export interface GrantBody {
	repo: string; // repo id
	role: GrantRole;
}

// ---- API keys (any signed-in user, own keys only) ----

/** One API key in GET /api/auth/keys — the full token is never listed again. */
export interface ApiKeyInfo {
	id: string;
	name: string;
	prefix: string;
	created_at: string;
}

export interface KeysResponse {
	keys: ApiKeyInfo[];
}

/** POST /api/auth/keys reply — carries the full rk_… token exactly once. */
export interface ApiKeyResponse extends ApiKeyInfo {
	token: string;
}

// ---- config ----

export interface Config {
	threshold: number;
}

// ---- people (repo-scoped) ----

export interface PersonSummary {
	id: string;
	name: string;
	photos: number;
	embeddings: number;
	thumb: string;
	/** aliases (public metadata) — the list filter matches them and shows an aka hint */
	aliases?: string[];
}

export interface PeopleResponse {
	people: PersonSummary[];
	count: number;
	repo: string;
}

export interface PersonPhoto {
	path: string;
	hash: string;
}

/** Partial birthdate: any component may be null when unknown. */
export interface BirthDate {
	year?: number | null;
	month?: number | null;
	day?: number | null;
}

/** Optional descriptive metadata about a person (all fields optional). */
export interface PersonMeta {
	aliases?: string[];
	birthdate?: BirthDate;
	urls?: string[];
	description?: string;
}

/** GET /api/people/{name}?repo= — details view (no enrolled photo paths). */
export interface PersonDetail {
	id: string;
	name: string;
	photos: number;
	/** the repository the person lives in */
	repo: string;
	repo_name: string;
	aliases?: string[];
	birthdate?: BirthDate;
	urls?: string[];
	description?: string;
}

/** GET /api/people/{name}/photos?repo= — writer-only photos-manager payload. */
export interface PersonPhotosResponse {
	id: string;
	name: string;
	thumb_src: string;
	photos: PersonPhoto[];
}

/** POST /api/people/{name}/meta body — full-replace semantics. */
export interface MetaUpdate {
	aliases?: string[];
	birthdate?: BirthDate | null;
	urls?: string[];
	description?: string;
}

// ---- recognition ----

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
