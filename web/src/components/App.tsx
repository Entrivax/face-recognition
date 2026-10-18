// App — the boot router. On mount it probes the session and renders exactly
// one of: the setup view (first run), the login view, the share view (a
// /share/<repoID>/<token> URL takes precedence — parsed from the pathname,
// no history.pushState involved), or the main app. Credentials accepted in
// setup/login re-run the probe; a 401 from any authed wrapper flips back to
// the login view.
//
// All signed-in state lives in MainApp.

import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import { setShareContext } from "../api/core";
import type { SessionUser } from "../types";
import { useToast } from "../toast";
import { LoginView } from "./LoginView";
import { MainApp } from "./MainApp";
import { parseSharePath, ShareMalformed, ShareView } from "./ShareView";
import { SetupView } from "./SetupView";

type Boot = "loading" | "setup" | "login" | "app" | "error";

export function App() {
	const toast = useToast();
	// Used by the (mount-stable) unauthorized hook below.
	const toastRef = useRef(toast);
	toastRef.current = toast;

	// Share links take precedence and are decided from the URL alone —
	// share mode never consults (or sends) the session cookie.
	const [sharePath] = useState(() => parseSharePath(window.location.pathname));

	const [boot, setBoot] = useState<Boot>("loading");
	const [user, setUser] = useState<SessionUser | null>(null);

	// Probe the session once and route: setup → login → app.
	const probe = useCallback(async () => {
		try {
			const s = await api.getSession();
			if (s.setup_required) {
				setBoot("setup");
				return;
			}
			if (!s.authenticated || !s.user) {
				setUser(null);
				setBoot("login");
				return;
			}
			setUser(s.user);
			setBoot("app");
		} catch {
			setBoot("error");
		}
	}, []);

	const isShare = sharePath.kind === "share" || sharePath.kind === "malformed";

	useEffect(() => {
		if (isShare) return; // share mode: no session probe
		void probe();
	}, [probe, isShare]);

	// Any authed wrapper that catches a 401 flips the app to the login view
	// — the session expired and the next call would fail anyway. (In share
	// mode cookies are never sent, so this cannot fire.)
	const onUnauthorized = useCallback(() => {
		setUser(null);
		setBoot("login");
		toastRef.current.show("Session expired — log in again.", "err");
	}, []);

	useEffect(() => {
		if (isShare) return;
		api.setOnUnauthorized(onUnauthorized);
		return () => api.setOnUnauthorized(null);
	}, [onUnauthorized, isShare]);

	// Credentials accepted (setup or login): re-probe so the app renders
	// from the server's view of the session.
	const onAuthed = useCallback(() => {
		void probe();
	}, [probe]);

	if (sharePath.kind === "share") {
		// Attach before the first child render: Avatar reads the share
		// context during its own render to decide whether thumbnails need
		// the header (ShareView re-asserts it and clears it on unmount).
		setShareContext({ repoId: sharePath.repoId, token: sharePath.token });
		return <ShareView repoId={sharePath.repoId} token={sharePath.token} />;
	}
	if (sharePath.kind === "malformed") {
		return <ShareMalformed />;
	}
	if (boot === "loading") {
		return (
			<div class="auth-page">
				<div class="auth-card">
					<p class="auth-lede" aria-live="polite">Connecting…</p>
				</div>
			</div>
		);
	}
	if (boot === "error") {
		return (
			<div class="auth-page">
				<div class="auth-card" role="alert">
					<p class="auth-eyebrow">server unreachable</p>
					<h1>Could not reach the server</h1>
					<p class="auth-lede">The session check failed — the server may be down or restarting.</p>
					<button type="button" class="btn" onClick={() => { setBoot("loading"); void probe(); }}>
						Try again
					</button>
				</div>
			</div>
		);
	}
	if (boot === "setup") {
		return <SetupView onAuthed={onAuthed} />;
	}
	if (boot === "login") {
		return <LoginView onAuthed={onAuthed} />;
	}
	return <MainApp user={user!} onSessionEnd={() => { setUser(null); setBoot("login"); }} />;
}
