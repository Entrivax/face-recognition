// API-keys manager (any signed-in user — own keys only). Lists the caller's
// keys (name, prefix, created), creates one — the full rk_… token comes
// back exactly once and is shown with a copy button and a store-it-now
// warning — and revokes keys.
//
// State as signals; the key rows are a <For> of KeyRow components (keyed by
// the key id), so a busy flip re-renders the rows' buttons only, and a
// refresh re-creates the rows (fresh server objects), as before. The
// minted-token box is a <Show> on the minted signal.

import { useEffect, useRef } from "preact/hooks";
import { useComputed, useSignal, type ReadonlySignal } from "@preact/signals";
import { For, Show } from "@preact/signals/utils";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { ApiKeyInfo, ApiKeyResponse } from "../types";
import { copyText } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface ApiKeysModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
}

export function ApiKeysModal(props: ApiKeysModalProps) {
	const toast = useToast();

	const keys = useSignal<ApiKeyInfo[] | null>(null);
	const hint = useSignal("Loading API keys…");
	const busy = useSignal(false);
	const name = useSignal("");
	// The freshly minted token — shown once, until the modal closes or the
	// next key is created.
	const minted = useSignal<ApiKeyResponse | null>(null);

	const nameRef = useRef<HTMLInputElement | null>(null);
	const list = useComputed(() => keys.value ?? []);
	const hasRows = useComputed(() => list.value.length > 0);

	const createKeyDisabled = useComputed(() => busy.value || !name.value.trim());

	// Reload on open (the instance mounts fresh per open — no `open` guard).
	useEffect(() => {
		keys.value = null;
		minted.value = null;
		name.value = "";
		loadList();
	}, []);

	const close = () => {
		if (busy.value) return;
		props.onCloseRequest();
	};

	async function loadList() {
		hint.value = "Loading API keys…";
		try {
			const j = await api.listApiKeys();
			const l = j.keys || [];
			keys.value = l;
			hint.value = l.length
				? "Keys authenticate scripts as you, via the Authorization header."
				: "No API keys yet — create one below for scripts.";
		} catch (err) {
			keys.value = [];
			hint.value = (err as Error).message || "Could not load the API keys.";
		}
	}

	const create = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy.value) return;
		const n = name.value.trim();
		if (!n) { nameRef.current?.focus(); return; }
		busy.value = true;
		try {
			const j = await api.createApiKey(n);
			minted.value = j;
			name.value = "";
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the API key.", "err");
		} finally {
			busy.value = false;
		}
	};

	const copyToken = async (m: ApiKeyResponse) => {
		toast.show(await copyText(m.token) ? "Token copied." : "Could not copy — select it manually.", "ok");
	};

	const revoke = async (k: ApiKeyInfo) => {
		if (busy.value) return;
		if (!confirm(`Revoke the key "${k.name}" (${k.prefix}…)? Scripts using it will stop working.`)) return;
		busy.value = true;
		try {
			await api.deleteApiKey(k.id);
			toast.show("Key revoked.", "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not revoke the key.", "err");
		} finally {
			busy.value = false;
		}
	};

	return (
		<Modal
			open={props.open}
			id="keysModal"
			cardId="keysCard"
			locked={busy}
			eyebrow="account"
			titleId="keysTitle"
			title="API keys"
			onCloseRequest={close}
			body={
				<div class="modal-body">
					<p class="lede" id="keysLede">
						Keys act as you for scripts: <code>Authorization: Bearer rk_…</code> with the
						same reader/writer rights you have.
					</p>

					<form class="admin-create-row" id="keyCreateForm" onSubmit={create}>
						<input
							ref={nameRef}
							type="text"
							placeholder="Key name — e.g. backup script"
							maxLength={120}
							autocomplete="off"
							spellcheck={false}
							aria-label="New key name"
							disabled={busy}
							value={name}
							onInput={(e) => { name.value = e.currentTarget.value; }}
						/>
						<button type="submit" class="btn btn-accent btn-sm" disabled={createKeyDisabled}>
							Create key
						</button>
					</form>

					<Show when={minted}>
						{(m) => (
							<div class="key-token-box" id="keyTokenBox">
								<p class="field-hint warn">Store it now — it will not be shown again.</p>
								<div class="key-token-row">
									<code id="keyTokenValue" title={m.token}>{m.token}</code>
									<button type="button" class="btn btn-sm" disabled={busy} onClick={() => copyToken(m)}>
										Copy
									</button>
								</div>
							</div>
						)}
					</Show>

					<ul id="keyList" class="admin-list">
						<For each={list} getKey={(k) => k.id}>
							{(k) => <KeyRow k={k} busy={busy} onRevoke={revoke} />}
						</For>
					</ul>
					<p class="enroll-meta" id="keysMeta" hidden={hasRows}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

// One key row: name, prefix, created date and the revoke action. The busy
// flag arrives as a signal — the row's own buttons react to it without the
// modal body re-rendering.
function KeyRow(props: {
	k: ApiKeyInfo;
	busy: ReadonlySignal<boolean>;
	onRevoke: (k: ApiKeyInfo) => void;
}) {
	const k = props.k;
	return (
		<li class="admin-row key-row">
			<div class="admin-row-main">
				<span class="admin-row-name">{k.name}</span>
				<span class="admin-row-sub" title="Key prefix">{k.prefix}…</span>
				<span class="admin-row-sub">{fmtCreated(k.created_at)}</span>
			</div>
			<div class="admin-row-actions">
				<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={props.busy} onClick={() => props.onRevoke(k)}>
					Revoke
				</button>
			</div>
		</li>
	);
}

function fmtCreated(rfc3339: string): string {
	const d = new Date(rfc3339);
	return Number.isNaN(d.getTime()) ? rfc3339 : `created ${d.toLocaleDateString()}`;
}
