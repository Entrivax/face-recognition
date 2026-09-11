// Passkeys manager (admin-only). Lists the credentials registered on the
// server, adds a new one via the WebAuthn registration ceremony (begin →
// navigator.credentials.create → finish), and removes individual passkeys
// with a confirm. Opened from the topbar's "Passkeys" button.

import { useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { PasskeyInfo } from "../types";
import { passkeyRegister, webauthnAvailable } from "../webauthnClient";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface PasskeysModalProps {
	open: boolean;
	onCloseRequest: () => void;
}

export function PasskeysModal(props: PasskeysModalProps) {
	const toast = useToast();

	const [passkeys, setPasskeys] = useState<PasskeyInfo[] | null>(null);
	const [hint, setHint] = useState("Loading passkeys…");
	const [busy, setBusy] = useState(false);
	const busyRef = useRef(false);

	const setBusyBoth = (b: boolean) => {
		busyRef.current = b;
		setBusy(b);
	};

	// The list is reloaded every time the modal opens.
	useEffect(() => {
		if (!props.open) return;
		setPasskeys(null);
		loadList();
	}, [props.open]);

	const close = () => {
		if (busyRef.current) return; // locked while a ceremony is in flight
		props.onCloseRequest();
	};

	async function loadList() {
		setHint("Loading passkeys…");
		try {
			const j = await api.listPasskeys();
			const list = j.passkeys || [];
			setPasskeys(list);
			setHint(list.length
				? "Passkeys sign in without the admin password."
				: "No passkeys registered yet — add one below.");
		} catch (err) {
			setPasskeys([]);
			setHint((err as Error).message || "Could not load the passkeys.");
		}
	}

	// Registration ceremony: the authenticator prompts mid-way through
	// navigator.credentials.create (fingerprint / security key / …).
	const addPasskey = async () => {
		if (busyRef.current) return;
		setBusyBoth(true);
		try {
			const opts = await api.passkeyRegisterBegin();
			const attestation = await passkeyRegister(opts);
			await api.passkeyRegisterFinish(attestation);
			toast.show("Passkey added.", "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Passkey registration failed.", "err");
		} finally {
			setBusyBoth(false);
		}
	};

	const removePasskey = async (pk: PasskeyInfo) => {
		if (busyRef.current) return;
		if (!confirm("Remove this passkey? It will no longer be able to sign in.")) return;
		setBusyBoth(true);
		try {
			await api.deletePasskey(pk.id);
			toast.show("Passkey removed.", "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not remove the passkey.", "err");
		} finally {
			setBusyBoth(false);
		}
	};

	const canRegister = webauthnAvailable();
	const list = passkeys || [];

	return (
		<Modal
			open={props.open}
			id="passkeysModal"
			cardId="passkeysCard"
			locked={busy}
			eyebrow="authentication"
			titleId="passkeysTitle"
			title="Passkeys"
			onCloseRequest={close}
			body={
				<div class="modal-body">
					<p class="lede" id="passkeysLede">
						Passkeys let you sign in without the admin password. They are tied to the
						device that creates them.
					</p>

					<ul id="passkeyList" class="passkey-list">
						{list.map((pk) => (
							<li key={pk.id} class="passkey-row">
								<span class="passkey-id" title={pk.id}>{shortID(pk.id)}</span>
								<span class="passkey-added">{fmtAdded(pk.added_at)}</span>
								<button
									type="button"
									class="btn btn-sm btn-ghost passkey-remove"
									disabled={busy}
									onClick={() => removePasskey(pk)}
								>
									Remove
								</button>
							</li>
						))}
					</ul>
					<p class="enroll-meta" id="passkeysMeta" hidden={list.length > 0}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
						<button
							type="button"
							id="passkeyAddBtn"
							class="btn btn-accent"
							disabled={busy || !canRegister}
							onClick={addPasskey}
						>
							{busy ? "Waiting for authenticator…" : "Add passkey"}
						</button>
					</footer>
					{!canRegister && (
						<p class="field-hint">
							This browser can't create passkeys — they need a secure context (HTTPS or
							localhost) and WebAuthn support.
						</p>
					)}
				</div>
			}
		/>
	);
}

// Credential IDs are long base64url blobs — show a readable head, full ID in
// the title tooltip.
function shortID(id: string): string {
	return id.length > 14 ? `${id.slice(0, 10)}…${id.slice(-6)}` : id;
}

function fmtAdded(rfc3339: string): string {
	const d = new Date(rfc3339);
	return Number.isNaN(d.getTime()) ? rfc3339 : `added ${d.toLocaleString()}`;
}
