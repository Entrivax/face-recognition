// Login view (full page, replaces the old login modal). Shown when the
// session probe reports no session (and setup is complete). Success hands
// the session user to App, which re-probes and renders the main app.

import type { TargetedEvent } from "preact";
import * as api from "../api";
import { batch, computed, createModel, Signal, signal, useModel, type ReadonlySignal } from "@preact/signals";
import { Show, useLiveSignal } from "@preact/signals/utils";

interface LoginViewProps {
	/** credentials accepted — App re-probes the session */
	onAuthed: () => void;
}

interface ILoginViewModel {
	username: Signal<string>;
	password: Signal<string>;
	error: ReadonlySignal<string | null>;
	busy: ReadonlySignal<boolean>;
	submitDisabled: ReadonlySignal<boolean>;
	usernameInputRef: Signal<HTMLInputElement | null>;
	passwordInputRef: Signal<HTMLInputElement | null>;
	submit: (e: TargetedEvent<HTMLFormElement>) => Promise<void>;
}

const LoginViewModel = createModel<ILoginViewModel, [ReadonlySignal<() => void>]>((onAuthed: ReadonlySignal<() => void>) => {
	const username = signal("");
	const password = signal("");
	const error = signal<string | null>(null);
	const busy = signal(false);
	const usernameInputRef = signal<HTMLInputElement | null>(null);
	const passwordInputRef = signal<HTMLInputElement | null>(null);

	return {
		username,
		password,
		error,
		busy,
		submitDisabled: computed(() => busy.value || !username.value.trim() || !password.value),
		usernameInputRef,
		passwordInputRef,
		async submit(e: TargetedEvent<HTMLFormElement>) {
			e.preventDefault()
			if (busy.value) return
			if (!username.value.trim()) { error.value = "Enter your username."; usernameInputRef.value?.focus(); return }
			if (!password.value) { error.value = "Enter your password."; passwordInputRef.value?.focus(); return }
			batch(() => {
				busy.value = true
				error.value = null
			})
			try {
				await api.login(username.value.trim(), password.value)
				onAuthed.value()
			} catch (err) {
				batch(() => {
					busy.value = false
					error.value = (err as Error).message || "Login failed."
				});
				return
			}
			busy.value = false
		}
	}
})

export function LoginView({ onAuthed }: LoginViewProps) {
	const onAuthedSignal = useLiveSignal(onAuthed);
	const model = useModel(() => new LoginViewModel(onAuthedSignal));

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

				<form class="auth-form" novalidate onSubmit={model.submit}>
					<label class="field-label" for="loginUsername">Username</label>
					<input
						ref={(el) => { model.usernameInputRef.value = el }}
						id="loginUsername"
						type="text"
						placeholder="Username"
						autocomplete="username"
						spellcheck={false}
						disabled={model.busy}
						value={model.username}
						onInput={(e) => model.username.value = e.currentTarget.value}
					/>

					<label class="field-label" for="loginPassword">Password</label>
					<input
						ref={(el) => { model.passwordInputRef.value = el }}
						id="loginPassword"
						type="password"
						placeholder="Password"
						autocomplete="current-password"
						disabled={model.busy}
						value={model.password}
						onInput={(e) => model.password.value = e.currentTarget.value}
					/>

					<Show when={model.error}>
						<p class="auth-error" id="loginError" role="alert">{model.error}</p>
					</Show>

					<button type="submit" id="loginSubmit" class="btn btn-accent" disabled={model.submitDisabled}>
						{model.busy.value ? "Checking…" : "Log in"}
					</button>
				</form>
			</div>
		</div>
	);
}
