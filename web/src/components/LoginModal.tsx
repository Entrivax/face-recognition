// Login modal. Shown from the topbar's "Log in" button when the server has
// an admin-login method configured and this browser holds no valid session.
// Password form, plus a passkey sign-in button when the server has passkeys
// enabled and the browser can actually do WebAuthn. Success flips App to
// authed (via onLoggedIn) and refreshes the admin-only data.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { Health } from "../types";
import { passkeyLogin, webauthnAvailable } from "../webauthnClient";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface LoginModalProps {
	open: boolean;
	health: Health | null;
	onCloseRequest: () => void;
	/** credentials accepted — App flips to authed and refreshes admin data */
	onLoggedIn: () => void;
}

export function LoginModal(props: LoginModalProps) {
	const toast = useToast();

	const [password, setPassword] = useState("");
	const [busy, setBusy] = useState(false);
	const passwordRef = useRef<HTMLInputElement | null>(null);
	const busyRef = useRef(false);

	const setBusyBoth = (b: boolean) => {
		busyRef.current = b;
		setBusy(b);
	};

	// Fresh form each time the modal opens.
	useEffect(() => {
		if (props.open) {
			setPassword("");
			setBusyBoth(false);
		}
	}, [props.open]);

	// Passkey sign-in needs the server-side method AND a WebAuthn-capable
	// context (HTTPS or localhost).
	const passkeyReady = Boolean(props.health?.auth?.passkey) && webauthnAvailable();

	const close = () => {
		if (busyRef.current) return; // locked while a login is in flight
		props.onCloseRequest();
	};

	// Shared tail of both sign-in paths: App flips to authed and refreshes.
	const succeed = (msg: string) => {
		setBusyBoth(false);
		setPassword("");
		toast.show(msg, "ok");
		props.onLoggedIn();
		props.onCloseRequest();
	};

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current) return;
		if (!password) {
			passwordRef.current?.focus();
			return;
		}
		setBusyBoth(true);
		try {
			await api.login(password);
			succeed("Logged in.");
		} catch (err) {
			setBusyBoth(false);
			toast.show((err as Error).message || "Login failed.", "err");
		}
	};

	const passkeySignIn = async () => {
		if (busyRef.current) return;
		setBusyBoth(true);
		try {
			const opts = await api.passkeyLoginBegin();
			const assertion = await passkeyLogin(opts);
			await api.passkeyLoginFinish(assertion);
			succeed("Logged in with passkey.");
		} catch (err) {
			setBusyBoth(false);
			toast.show((err as Error).message || "Passkey sign-in failed.", "err");
		}
	};

	return (
		<Modal
			open={props.open}
			id="loginModal"
			cardId="loginCard"
			locked={busy}
			eyebrow="authentication"
			titleId="loginTitle"
			title="Log in"
			initialFocus={() => passwordRef.current}
			backdropClickDisabled={true}
			onCloseRequest={close}
			body={
				<form id="loginForm" class="modal-body login-form" novalidate onSubmit={submit}>
					<label class="field-label" for="loginPassword">Admin password</label>
					<input
						ref={passwordRef}
						id="loginPassword"
						type="password"
						placeholder="Password"
						autocomplete="current-password"
						disabled={busy}
						value={password}
						onInput={(e) => setPassword(e.currentTarget.value)}
					/>
					<p class="field-hint" id="loginHint">
						Enrollment, photo management and settings require signing in.
					</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Cancel</button>
						<button type="submit" id="loginSubmit" class="btn btn-accent" disabled={busy || !password}>
							{busy ? "Checking…" : "Log in"}
						</button>
					</footer>

					{passkeyReady && (
						<div class="login-alt">
							<button
								type="button"
								id="passkeyLoginBtn"
								class="btn btn-ghost"
								disabled={busy}
								onClick={passkeySignIn}
							>
								Sign in with a passkey
							</button>
						</div>
					)}
				</form>
			}
		/>
	);
}
