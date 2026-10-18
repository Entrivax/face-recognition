// First-run setup view (full page). Shown when the session probe reports
// setup_required: the server printed a one-time setup key to its console and
// this form exchanges it for the administrator account. Submit →
// POST /api/setup → POST /api/login → App re-probes and renders the app.
// Errors surface in-form (403 = wrong key or setup already completed).

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import { USERNAME_RE } from "../util";

interface SetupViewProps {
	/** setup + login accepted — App re-probes the session */
	onAuthed: () => void;
}

const PASSWORD_MIN = 8;

export function SetupView({ onAuthed }: SetupViewProps) {
	const [key, setKey] = useState("");
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [confirm, setConfirm] = useState("");
	const [error, setError] = useState<string | null>(null);
	const [busy, setBusy] = useState(false);

	const keyRef = useRef<HTMLInputElement | null>(null);
	useEffect(() => keyRef.current?.focus(), []);

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy) return;
		const u = username.trim();
		if (!key.trim()) { setError("Enter the setup key printed to the server console."); keyRef.current?.focus(); return; }
		if (!USERNAME_RE.test(u)) {
			setError("Username must be 2–32 characters and start with a letter or digit (letters, digits, dots, underscores and hyphens after).");
			return;
		}
		if (password.length < PASSWORD_MIN) {
			setError(`Password must be at least ${PASSWORD_MIN} characters.`);
			return;
		}
		if (password !== confirm) {
			setError("The passwords do not match.");
			return;
		}
		setBusy(true);
		setError(null);
		try {
			await api.setup({ key: key.trim(), username: u, password });
		} catch (err) {
			setBusy(false);
			setError((err as Error).message || "Setup failed.");
			return;
		}
		// Setup created the admin — sign the new account in right away. A
		// failed login is NOT a failed setup (resubmitting would 403): hand
		// control back so the re-probe routes to the login view.
		try {
			await api.login(u, password);
		} catch {
			// fall through to onAuthed
		}
		onAuthed();
	};

	return (
		<div class="auth-page">
			<div class="auth-card" id="setupCard">
				<div class="auth-brand">
					<span class="brand-mark" aria-hidden="true">
						<img src="/logo.svg" />
					</span>
					<span class="brand-word">recogn</span>
				</div>
				<p class="auth-eyebrow">first-run setup</p>
				<h1 id="setupTitle">Create the administrator</h1>
				<p class="auth-lede">
					This is a fresh installation. The setup key was printed to the server
					console when it started — use it here once to create the administrator
					account.
				</p>

				<form class="auth-form" novalidate onSubmit={submit}>
					<label class="field-label" for="setupKey">Setup key</label>
					<input
						ref={keyRef}
						id="setupKey"
						type="text"
						placeholder="XXXX-XXXX-XXXX-XXXX"
						autocomplete="off"
						spellcheck={false}
						disabled={busy}
						value={key}
						onInput={(e) => setKey(e.currentTarget.value)}
					/>

					<label class="field-label" for="setupUsername">Admin username</label>
					<input
						id="setupUsername"
						type="text"
						placeholder="username"
						autocomplete="username"
						spellcheck={false}
						disabled={busy}
						value={username}
						onInput={(e) => setUsername(e.currentTarget.value)}
					/>

					<label class="field-label" for="setupPassword">Password</label>
					<input
						id="setupPassword"
						type="password"
						placeholder={`At least ${PASSWORD_MIN} characters`}
						autocomplete="new-password"
						disabled={busy}
						value={password}
						onInput={(e) => setPassword(e.currentTarget.value)}
					/>

					<label class="field-label" for="setupConfirm">Confirm password</label>
					<input
						id="setupConfirm"
						type="password"
						placeholder="Repeat the password"
						autocomplete="new-password"
						disabled={busy}
						value={confirm}
						onInput={(e) => setConfirm(e.currentTarget.value)}
					/>

					{error && <p class="auth-error" id="setupError" role="alert">{error}</p>}

					<button type="submit" id="setupSubmit" class="btn btn-accent" disabled={busy}>
						{busy ? "Setting up…" : "Create administrator"}
					</button>
				</form>
			</div>
		</div>
	);
}
