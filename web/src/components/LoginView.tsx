// Login view (full page, replaces the old login modal). Shown when the
// session probe reports no session (and setup is complete). Success hands
// the session user to App, which re-probes and renders the main app.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";

interface LoginViewProps {
	/** credentials accepted — App re-probes the session */
	onAuthed: () => void;
}

export function LoginView({ onAuthed }: LoginViewProps) {
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [error, setError] = useState<string | null>(null);
	const [busy, setBusy] = useState(false);

	const usernameRef = useRef<HTMLInputElement | null>(null);
	useEffect(() => usernameRef.current?.focus(), []);

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy) return;
		if (!username.trim()) { setError("Enter your username."); usernameRef.current?.focus(); return; }
		if (!password) { setError("Enter your password."); return; }
		setBusy(true);
		setError(null);
		try {
			await api.login(username.trim(), password);
			onAuthed();
		} catch (err) {
			setBusy(false);
			setError((err as Error).message || "Login failed.");
		}
	};

	return (
		<div class="auth-page">
			<div class="auth-card" id="loginCard">
				<div class="auth-brand">
					<span class="brand-mark" aria-hidden="true">
						<img src="/logo.svg" />
					</span>
					<span class="brand-word">recogn</span>
					<span class="brand-sub">face review console</span>
				</div>
				<h1 id="loginTitle">Log in</h1>
				<p class="auth-lede">Sign in to browse repositories and inspect photos.</p>

				<form class="auth-form" novalidate onSubmit={submit}>
					<label class="field-label" for="loginUsername">Username</label>
					<input
						ref={usernameRef}
						id="loginUsername"
						type="text"
						placeholder="Username"
						autocomplete="username"
						spellcheck={false}
						disabled={busy}
						value={username}
						onInput={(e) => setUsername(e.currentTarget.value)}
					/>

					<label class="field-label" for="loginPassword">Password</label>
					<input
						id="loginPassword"
						type="password"
						placeholder="Password"
						autocomplete="current-password"
						disabled={busy}
						value={password}
						onInput={(e) => setPassword(e.currentTarget.value)}
					/>

					{error && <p class="auth-error" id="loginError" role="alert">{error}</p>}

					<button type="submit" id="loginSubmit" class="btn btn-accent" disabled={busy || !username.trim() || !password}>
						{busy ? "Checking…" : "Log in"}
					</button>
				</form>
			</div>
		</div>
	);
}
