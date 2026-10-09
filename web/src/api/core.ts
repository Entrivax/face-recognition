// Shared fetch plumbing for the REST API wrappers. Each wrapper throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect. Admin endpoints throw
// UnauthorizedError on 401 (and fire the onUnauthorized hook App registers
// via setOnUnauthorized) so the UI can flip to logged-out on session expiry.

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

/** Fire the 401 hook (session-expiry) without going through expectJSON. */
export function fireUnauthorized(): void {
	onUnauthorized?.();
}

export async function parse<T>(r: Response): Promise<T> {
	return (await r.json().catch(() => ({}))) as T;
}

export async function expectJSON<T>(r: Response, fallback: string): Promise<T> {
	const j = await parse<T & { error?: string }>(r);
	if (r.status === 401) {
		onUnauthorized?.();
		throw new UnauthorizedError(j.error || fallback);
	}
	if (!r.ok) throw new Error(j.error || fallback);
	return j;
}
