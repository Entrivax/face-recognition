// Change-password modal (any signed-in user). Validates the new password
// client-side (min length + confirmation) and lets the server re-check;
// 403 = wrong current password, surfaced as an ordinary error.
//
// State as signals: the three fields and the busy flag; the disabled
// attributes ride the signals (auto-unwrap). The instance unmounts on
// close, so the fresh-form reset is a plain mount effect.

import { useEffect, useRef } from "preact/hooks";
import type { TargetedEvent } from "preact";
import { useComputed, useSignal } from "@preact/signals";
import * as api from "../api";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface ChangePasswordModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
}

const PASSWORD_MIN = 8;

export function ChangePasswordModal(props: ChangePasswordModalProps) {
	const toast = useToast();

	const current = useSignal("");
	const next = useSignal("");
	const confirm = useSignal("");
	const busy = useSignal(false);

	const changePasswordDisabled = useComputed(() => busy.value || !current.value || !next.value || !confirm.value);

	const currentRef = useRef<HTMLInputElement | null>(null);

	// Fresh form each time the modal opens (the instance mounts per open).
	useEffect(() => {
		current.value = "";
		next.value = "";
		confirm.value = "";
	}, []);

	const close = () => {
		if (busy.value) return;
		props.onCloseRequest();
	};

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy.value) return;
		if (!current.value) { currentRef.current?.focus(); return; }
		if (next.value.length < PASSWORD_MIN) {
			toast.show(`The new password must be at least ${PASSWORD_MIN} characters.`, "err");
			return;
		}
		if (next.value !== confirm.value) {
			toast.show("The new passwords do not match.", "err");
			return;
		}
		busy.value = true;
		try {
			await api.changePassword(current.value, next.value);
			toast.show("Password changed.", "ok");
			props.onCloseRequest();
		} catch (err) {
			toast.show((err as Error).message || "Could not change the password.", "err");
		} finally {
			busy.value = false;
		}
	};

	return (
		<Modal
			open={props.open}
			id="passwordModal"
			cardId="passwordCard"
			locked={busy}
			eyebrow="account"
			titleId="passwordTitle"
			title="Change password"
			initialFocus={() => currentRef.current}
			backdropClickDisabled={true}
			onCloseRequest={close}
			body={
				<form id="passwordForm" class="modal-body login-form" novalidate onSubmit={submit}>
					<label class="field-label" for="passwordCurrent">Current password</label>
					<input
						ref={currentRef}
						id="passwordCurrent"
						type="password"
						autocomplete="current-password"
						disabled={busy}
						value={current}
						onInput={(e) => { current.value = e.currentTarget.value; }}
					/>

					<label class="field-label" for="passwordNew">New password</label>
					<input
						id="passwordNew"
						type="password"
						placeholder={`At least ${PASSWORD_MIN} characters`}
						autocomplete="new-password"
						disabled={busy}
						value={next}
						onInput={(e) => { next.value = e.currentTarget.value; }}
					/>

					<label class="field-label" for="passwordConfirm">Confirm new password</label>
					<input
						id="passwordConfirm"
						type="password"
						placeholder="Repeat the new password"
						autocomplete="new-password"
						disabled={busy}
						value={confirm}
						onInput={(e) => { confirm.value = e.currentTarget.value; }}
					/>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Cancel</button>
						<button
							type="submit"
							id="passwordSubmit"
							class="btn btn-accent"
							disabled={changePasswordDisabled}
						>
							{busy.value ? "Saving…" : "Change password"}
						</button>
					</footer>
				</form>
			}
		/>
	);
}
