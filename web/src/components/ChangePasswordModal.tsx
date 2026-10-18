// Change-password modal (any signed-in user). Validates the new password
// client-side (min length + confirmation) and lets the server re-check;
// 403 = wrong current password, surfaced as an ordinary error.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface ChangePasswordModalProps {
	open: boolean;
	onCloseRequest: () => void;
}

const PASSWORD_MIN = 8;

export function ChangePasswordModal(props: ChangePasswordModalProps) {
	const toast = useToast();

	const [current, setCurrent] = useState("");
	const [next, setNext] = useState("");
	const [confirm, setConfirm] = useState("");
	const [busy, setBusyState] = useState(false);

	const busyRef = useRef(false);
	const currentRef = useRef<HTMLInputElement | null>(null);

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	// Fresh form each time the modal opens.
	useEffect(() => {
		if (!props.open) return;
		setCurrent("");
		setNext("");
		setConfirm("");
	}, [props.open]);

	const close = () => {
		if (busyRef.current) return;
		props.onCloseRequest();
	};

	const submit = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current) return;
		if (!current) { currentRef.current?.focus(); return; }
		if (next.length < PASSWORD_MIN) {
			toast.show(`The new password must be at least ${PASSWORD_MIN} characters.`, "err");
			return;
		}
		if (next !== confirm) {
			toast.show("The new passwords do not match.", "err");
			return;
		}
		setBusy(true);
		try {
			await api.changePassword(current, next);
			toast.show("Password changed.", "ok");
			props.onCloseRequest();
		} catch (err) {
			toast.show((err as Error).message || "Could not change the password.", "err");
		} finally {
			setBusy(false);
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
						onInput={(e) => setCurrent(e.currentTarget.value)}
					/>

					<label class="field-label" for="passwordNew">New password</label>
					<input
						id="passwordNew"
						type="password"
						placeholder={`At least ${PASSWORD_MIN} characters`}
						autocomplete="new-password"
						disabled={busy}
						value={next}
						onInput={(e) => setNext(e.currentTarget.value)}
					/>

					<label class="field-label" for="passwordConfirm">Confirm new password</label>
					<input
						id="passwordConfirm"
						type="password"
						placeholder="Repeat the new password"
						autocomplete="new-password"
						disabled={busy}
						value={confirm}
						onInput={(e) => setConfirm(e.currentTarget.value)}
					/>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Cancel</button>
						<button type="submit" id="passwordSubmit" class="btn btn-accent" disabled={busy || !current || !next || !confirm}>
							{busy ? "Saving…" : "Change password"}
						</button>
					</footer>
				</form>
			}
		/>
	);
}
