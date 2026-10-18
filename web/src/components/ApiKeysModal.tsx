// API-keys manager (any signed-in user — own keys only). Lists the caller's
// keys (name, prefix, created), creates one — the full rk_… token comes
// back exactly once and is shown with a copy button and a store-it-now
// warning — and revokes keys.

import { useEffect, useRef, useState } from "preact/hooks";
import * as api from "../api";
import type { ApiKeyInfo, ApiKeyResponse } from "../types";
import { copyText } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface ApiKeysModalProps {
	open: boolean;
	onCloseRequest: () => void;
}

export function ApiKeysModal(props: ApiKeysModalProps) {
	const toast = useToast();

	const [keys, setKeys] = useState<ApiKeyInfo[] | null>(null);
	const [hint, setHint] = useState("Loading API keys…");
	const [busy, setBusyState] = useState(false);
	const [name, setName] = useState("");
	// The freshly minted token — shown once, until the modal closes or the
	// next key is created.
	const [minted, setMinted] = useState<ApiKeyResponse | null>(null);

	const busyRef = useRef(false);
	const nameRef = useRef<HTMLInputElement | null>(null);

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	useEffect(() => {
		if (!props.open) return;
		setKeys(null);
		setMinted(null);
		setName("");
		loadList();
	}, [props.open]);

	const close = () => {
		if (busyRef.current) return;
		props.onCloseRequest();
	};

	async function loadList() {
		setHint("Loading API keys…");
		try {
			const j = await api.listApiKeys();
			const list = j.keys || [];
			setKeys(list);
			setHint(list.length
				? "Keys authenticate scripts as you, via the Authorization header."
				: "No API keys yet — create one below for scripts.");
		} catch (err) {
			setKeys([]);
			setHint((err as Error).message || "Could not load the API keys.");
		}
	}

	const create = async () => {
		if (busyRef.current) return;
		const n = name.trim();
		if (!n) { nameRef.current?.focus(); return; }
		setBusy(true);
		try {
			const j = await api.createApiKey(n);
			setMinted(j);
			setName("");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the API key.", "err");
		} finally {
			setBusy(false);
		}
	};

	const copyToken = async () => {
		if (!minted) return;
		toast.show(await copyText(minted.token) ? "Token copied." : "Could not copy — select it manually.", "ok");
	};

	const revoke = async (k: ApiKeyInfo) => {
		if (busyRef.current) return;
		if (!confirm(`Revoke the key "${k.name}" (${k.prefix}…)? Scripts using it will stop working.`)) return;
		setBusy(true);
		try {
			await api.deleteApiKey(k.id);
			toast.show("Key revoked.", "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not revoke the key.", "err");
		} finally {
			setBusy(false);
		}
	};

	const list = keys || [];

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

					<form class="admin-create-row" id="keyCreateForm" onSubmit={(e) => { e.preventDefault(); create(); }}>
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
							onInput={(e) => setName(e.currentTarget.value)}
						/>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy || !name.trim()}>
							Create key
						</button>
					</form>

					{minted && (
						<div class="key-token-box" id="keyTokenBox">
							<p class="field-hint warn">Store it now — it will not be shown again.</p>
							<div class="key-token-row">
								<code id="keyTokenValue" title={minted.token}>{minted.token}</code>
								<button type="button" class="btn btn-sm" disabled={busy} onClick={copyToken}>
									Copy
								</button>
							</div>
						</div>
					)}

					<ul id="keyList" class="admin-list">
						{list.map((k) => (
							<li key={k.id} class="admin-row key-row">
								<div class="admin-row-main">
									<span class="admin-row-name">{k.name}</span>
									<span class="admin-row-sub" title="Key prefix">{k.prefix}…</span>
									<span class="admin-row-sub">{fmtCreated(k.created_at)}</span>
								</div>
								<div class="admin-row-actions">
									<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={busy} onClick={() => revoke(k)}>
										Revoke
									</button>
								</div>
							</li>
						))}
					</ul>
					<p class="enroll-meta" id="keysMeta" hidden={list.length > 0}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

function fmtCreated(rfc3339: string): string {
	const d = new Date(rfc3339);
	return Number.isNaN(d.getTime()) ? rfc3339 : `created ${d.toLocaleDateString()}`;
}
