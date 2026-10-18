// Typed wrappers around the REST API under /api. Each throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect. Authed endpoints throw
// UnauthorizedError on 401 (and fire the onUnauthorized hook App registers)
// so the UI can flip to logged-out on session expiry; in share mode
// 401/403 fire the onShareInvalid hook instead (see core.ts).

export * from "./core";
export * from "./people";
export * from "./enroll";
export * from "./auth";
export * from "./repos";
export * from "./misc";
