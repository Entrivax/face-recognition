// App — the boot router. On mount it probes the session and renders exactly
// one of: the setup view (first run), the login view, the share view (a
// /share/<repoID>/<token> URL takes precedence — parsed from the pathname,
// no history.pushState involved), or the main app. Credentials accepted in
// setup/login re-run the probe; a 401 from any authed wrapper flips back to
// the login view.
//
// All signed-in state lives in MainApp.

import { useCallback, useRef } from "preact/hooks";
import * as api from "../api";
import { setShareContext } from "../api/core";
import type { SessionUser } from "../types";
import { useToast } from "../toast";
import { LoginView } from "./LoginView";
import { MainApp } from "./MainApp";
import { parseSharePath, ShareMalformed, ShareView } from "./ShareView";
import { SetupView } from "./SetupView";
import { useLocationUpdater, usePathname } from "../hooks"
import { batch, useComputed, useSignal, useSignalEffect } from "@preact/signals"

type Boot = "loading" | "setup" | "login" | "app" | "error";

export function App() {
	useLocationUpdater();
	const toast = useToast();
	// Used by the (mount-stable) unauthorized hook below.
	const toastRef = useRef(toast);
	toastRef.current = toast;

	// Share links take precedence and are decided from the URL alone —
	// share mode never consults (or sends) the session cookie.
	const pathname = usePathname()
	const sharePath = useComputed(() => parseSharePath(pathname.value));

	const boot = useSignal<Boot>("loading")
	const user = useSignal<SessionUser | null>(null)

	// Probe the session once and route: setup → login → app.
	const probe = useCallback(async () => {
		try {
			const s = await api.getSession();
			if (s.setup_required) {
				boot.value = "setup"
				return;
			}
			if (!s.authenticated || !s.user) {
				batch(() => {
					user.value = null
					boot.value = "login"
				})
				return;
			}
			batch(() => {
				user.value = s.user
				boot.value = "app"
			})
		} catch {
			boot.value = "error"
		}
	}, []);

	const isShare = useComputed(() => sharePath.value.kind === "share" || sharePath.value.kind === "malformed");

	useSignalEffect(() => {
		if (isShare.value) return; // share mode: no session probe
		void probe();
	});

	// Any authed wrapper that catches a 401 flips the app to the login view
	// — the session expired and the next call would fail anyway. (In share
	// mode cookies are never sent, so this cannot fire.)
	const onUnauthorized = useCallback(() => {
		batch(() => {
			user.value = null
			boot.value = "login"
		})
		toastRef.current.show("Session expired — log in again.", "err");
	}, []);

	useSignalEffect(() => {
		if (isShare.value) return;
		api.setOnUnauthorized(onUnauthorized);
		return () => api.setOnUnauthorized(null);
	});

	// Credentials accepted (setup or login): re-probe so the app renders
	// from the server's view of the session.
	const onAuthed = useCallback(() => {
		void probe();
	}, [probe]);

	if (sharePath.value.kind === "share") {
		// Attach before the first child render: Avatar reads the share
		// context during its own render to decide whether thumbnails need
		// the header (ShareView re-asserts it and clears it on unmount).
		setShareContext({ repoId: sharePath.value.repoId, token: sharePath.value.token });
		return <ShareView repoId={sharePath.value.repoId} token={sharePath.value.token} />;
	}
	if (sharePath.value.kind === "malformed") {
		return <ShareMalformed />;
	}
	switch (boot.value) {
		case 'loading':
			return (
				<div class="auth-page">
					<div class="auth-card">
						<p class="auth-lede" aria-live="polite">Connecting…</p>
					</div>
				</div>
			);
		case 'error':
			return (
				<div class="auth-page">
					<div class="auth-card" role="alert">
						<p class="auth-eyebrow">server unreachable</p>
						<h1>Could not reach the server</h1>
						<p class="auth-lede">The session check failed — the server may be down or restarting.</p>
						<button type="button" class="btn" onClick={() => { boot.value = "loading"; void probe(); }}>
							Try again
						</button>
					</div>
				</div>
			);
		case 'setup':
			return <SetupView onAuthed={onAuthed} />;
		case 'login':
			return <LoginView onAuthed={onAuthed} />;
	}
	return <MainApp user={user.value} onSessionEnd={() => batch(() => { user.value = null; boot.value = 'login' })} />;
}
