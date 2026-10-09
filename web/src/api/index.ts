// Typed wrappers around the REST API under /api. Each throws
// Error(j.error || fallback) on !r.ok unless noted otherwise, matching the
// error handling the call sites expect. Admin endpoints throw
// UnauthorizedError on 401 (and fire the onUnauthorized hook App registers
// via setOnUnauthorized) so the UI can flip to logged-out on session expiry.

export * from "./core";
export * from "./people";
export * from "./enroll";
export * from "./auth";
export * from "./misc";
