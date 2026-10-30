// First-run setup view (full page). Shown when the session probe reports
// setup_required: the server printed a one-time setup key to its console and
// this form exchanges it for the administrator account. Submit →
// POST /api/setup → POST /api/login → App re-probes and renders the app.
// Errors surface in-form (403 = wrong key or setup already completed).
// Built as a view-model (createModel) like LoginView: the fields, error and
// busy flags are signals, validation runs inside submit (with refocus).

import type { TargetedEvent } from "preact";
import * as api from "../api";
import { batch, createModel, Signal, signal, useModel, useSignalEffect, type ReadonlySignal } from "@preact/signals";
import { Show, useLiveSignal } from "@preact/signals/utils";
import { USERNAME_RE } from "../util";

interface SetupViewProps {
	/** setup + login accepted — App re-probes the session */
	onAuthed: () => void;
}

const PASSWORD_MIN = 8;

interface ISetupViewModel {
	key: Signal<string>;
	username: Signal<string>;
	password: Signal<string>;
	confirm: Signal<string>;
	error: ReadonlySignal<string | null>;
	busy: ReadonlySignal<boolean>;
	keyInputRef: Signal<HTMLInputElement | null>;
	submit: (e: TargetedEvent<HTMLFormElement>) => Promise<void>;
}

const SetupViewModel = createModel<ISetupViewModel, [ReadonlySignal<() => void>]>((onAuthed: ReadonlySignal<() => void>) => {
	const key = signal("");
	const username = signal("");
	const password = signal("");
	const confirm = signal("");
	const error = signal<string | null>(null);
	const busy = signal(false);
	const keyInputRef = signal<HTMLInputElement | null>(null);

	return {
		key,
		username,
		password,
		confirm,
		error,
		busy,
		keyInputRef,
		async submit(e: TargetedEvent<HTMLFormElement>) {
			e.preventDefault();
			if (busy.value) return;
			const u = username.value.trim();
			if (!key.value.trim()) { error.value = "Enter the setup key printed to the server console."; keyInputRef.value?.focus(); return; }
			if (!USERNAME_RE.test(u)) {
				error.value = "Username must be 2–32 characters and start with a letter or digit (letters, digits, dots, underscores and hyphens after).";
				return;
			}
			if (password.value.length < PASSWORD_MIN) {
				error.value = `Password must be at least ${PASSWORD_MIN} characters.`;
				return;
			}
			if (password.value !== confirm.value) {
				error.value = "The passwords do not match.";
				return;
			}
			batch(() => {
				busy.value = true;
				error.value = null;
			});
			try {
				await api.setup({ key: key.value.trim(), username: u, password: password.value });
			} catch (err) {
				batch(() => {
					busy.value = false;
					error.value = (err as Error).message || "Setup failed.";
				});
				return;
			}
			// Setup created the admin — sign the new account in right away. A
			// failed login is NOT a failed setup (resubmitting would 403): hand
			// control back so the re-probe routes to the login view.
			try {
				await api.login(u, password.value);
			} catch {
				// fall through to onAuthed
			}
			onAuthed.value();
		},
	};
});

export function SetupView({ onAuthed }: SetupViewProps) {
	const onAuthedSignal = useLiveSignal(onAuthed);
	const model = useModel(() => new SetupViewModel(onAuthedSignal));

	// Focus the setup key on mount (the input ref lives on the model as a
	// signal, so the tracked read drives the effect).
	useSignalEffect(() => {
		model.keyInputRef.value?.focus();
	});

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

				<form class="auth-form" novalidate onSubmit={model.submit}>
					<label class="field-label" for="setupKey">Setup key</label>
					<input
						ref={(el) => { model.keyInputRef.value = el }}
						id="setupKey"
						type="text"
						placeholder="XXXX-XXXX-XXXX-XXXX"
						autocomplete="off"
						spellcheck={false}
						disabled={model.busy}
						value={model.key}
						onInput={(e) => model.key.value = e.currentTarget.value}
					/>

					<label class="field-label" for="setupUsername">Admin username</label>
					<input
						id="setupUsername"
						type="text"
						placeholder="username"
						autocomplete="username"
						spellcheck={false}
						disabled={model.busy}
						value={model.username}
						onInput={(e) => model.username.value = e.currentTarget.value}
					/>

					<label class="field-label" for="setupPassword">Password</label>
					<input
						id="setupPassword"
						type="password"
						placeholder={`At least ${PASSWORD_MIN} characters`}
						autocomplete="new-password"
						disabled={model.busy}
						value={model.password}
						onInput={(e) => model.password.value = e.currentTarget.value}
					/>

					<label class="field-label" for="setupConfirm">Confirm password</label>
					<input
						id="setupConfirm"
						type="password"
						placeholder="Repeat the password"
						autocomplete="new-password"
						disabled={model.busy}
						value={model.confirm}
						onInput={(e) => model.confirm.value = e.currentTarget.value}
					/>

					<Show when={model.error}>
						{(msg) => <p class="auth-error" id="setupError" role="alert">{msg}</p>}
					</Show>

					<button type="submit" id="setupSubmit" class="btn btn-accent" disabled={model.busy}>
						{model.busy.value ? "Setting up…" : "Create administrator"}
					</button>
				</form>
			</div>
		</div>
	);
}
