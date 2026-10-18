// Shared fetch plumbing for the REST API wrappers. Each wrapper throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect.
//
// Two credential modes:
//  - App mode (default): the HttpOnly recogn_session cookie authenticates
//    every request (same-origin), and a 401 from an authed endpoint fires
//    the onUnauthorized hook App registers so the UI flips to the login
//    screen on session expiry.
//  - Share mode: App called setShareContext({ repoId, token }) for a
//    /share/<repoID>/<token> URL; every request then attaches
//    "X-Share-Token: <repoID>/<token>" and drops the browser's cookies
//    (credentials: "omit") — a share visitor never uses the signed-in
//    session. There, 401 AND 403 fire the onShareInvalid hook instead
//    (the link is malformed, revoked, or the repository is gone).

export class UnauthorizedError extends Error {
	constructor(message = "authentication required") {
		super(message);
		this.name = "UnauthorizedError";
	}
}

/** Thrown in share mode when the token is rejected (see expectJSON). */
export class ShareInvalidError extends Error {
	constructor(message = "this share link is no longer valid") {
		super(message);
		this.name = "ShareInvalidError";
	}
}

export interface ShareContext {
	repoId: string;
	token: string;
}

let shareContext: ShareContext | null = null;

/** Activate (or clear) share mode for every subsequent API request. */
export function setShareContext(ctx: ShareContext | null): void {
	shareContext = ctx;
}

/** Current share context, or null in app mode (Avatar reads this once). */
export function getShareContext(): ShareContext | null {
	return shareContext;
}

let onUnauthorized: (() => void) | null = null;

/** App registers a callback that fires (once per 401) on session expiry. */
export function setOnUnauthorized(fn: (() => void) | null): void {
	onUnauthorized = fn;
}

/** Fire the 401 hook (session-expiry) without going through expectJSON. */
export function fireUnauthorized(): void {
	onUnauthorized?.();
}

let onShareInvalid: (() => void) | null = null;

/** App registers a callback that fires when a share token is rejected. */
export function setOnShareInvalid(fn: (() => void) | null): void {
	onShareInvalid = fn;
}

// Low-level fetch every wrapper goes through: merges the share header in
// and drops the browser's cookies while a share link is open. App mode is
// unchanged — same-origin requests carry the session cookie implicitly.
export async function request(path: string, init: RequestInit = {}): Promise<Response> {
	if (shareContext) {
		const headers = new Headers(init.headers || undefined);
		headers.set("X-Share-Token", `${shareContext.repoId}/${shareContext.token}`);
		return fetch(path, { ...init, headers, credentials: "omit" });
	}
	return fetch(path, init);
}

/** Appends ?repo=<id> (or &repo=… when the path already carries a query). */
export function withRepoQuery(path: string, repoId: string): string {
	const sep = path.includes("?") ? "&" : "?";
	return `${path}${sep}repo=${encodeURIComponent(repoId)}`;
}

export async function parse<T>(r: Response): Promise<T> {
	return (await r.json().catch(() => ({}))) as T;
}

export async function expectJSON<T>(r: Response, fallback: string): Promise<T> {
	const j = await parse<T & { error?: string }>(r);
	if (shareContext && (r.status === 401 || r.status === 403)) {
		onShareInvalid?.();
		throw new ShareInvalidError(j.error || fallback);
	}
	if (r.status === 401) {
		onUnauthorized?.();
		throw new UnauthorizedError(j.error || fallback);
	}
	if (!r.ok) throw new Error(j.error || fallback);
	return j;
}
