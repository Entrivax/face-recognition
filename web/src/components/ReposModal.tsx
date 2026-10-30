// Repositories manager (admin). Lists every repository (name, id, people
// count, shared state) and creates, renames and deletes them, and toggles
// the public read-only share link. Enabling mints a FRESH token and answers
// with the full share URL; the current link can always be re-read again
// ("Show link" button → GET) without rotating it. Disabling nulls it.
// After any change the list reloads and the app store refreshes its own
// repo state.
//
// State as signals; the repository rows are a <For> of RepoRow components
// (keyed by the repo id) that receive the item plus the signals they show
// (busy, renamingId, renameVal, shareUrls) and read .value internally — a
// busy flip or an expansion toggle re-renders the rows, not the modal body.

import { useEffect, useRef } from "preact/hooks";
import type { Ref, TargetedEvent } from "preact";
import { batch, useComputed, useSignal, useSignalEffect, type ReadonlySignal, type Signal } from "@preact/signals";
import { For } from "@preact/signals/utils";
import * as api from "../api";
import type { RepoInfo } from "../types";
import { copyText } from "../util";
import { useToast } from "../toast";
import { useApp } from "../appStore";
import { Modal } from "./Modal";

interface ReposModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
}

export function ReposModal(props: ReposModalProps) {
	const toast = useToast();
	const app = useApp();

	const repos = useSignal<RepoInfo[] | null>(null);
	const hint = useSignal("Loading repositories…");
	const busy = useSignal(false);
	const name = useSignal("");
	const renamingId = useSignal<string | null>(null);
	const renameVal = useSignal("");
	// The share URL of the revealed/most-recently-minted link, per repo id
	// (the listing itself carries no URLs — one comes back from the enable
	// call or from the GET reveal).
	const shareUrls = useSignal<Record<string, string>>({});

	const nameRef = useRef<HTMLInputElement | null>(null);
	// The inline rename editor's input (mounted by the renaming row).
	const renameInputRef = useRef<HTMLInputElement | null>(null);
	const list = useComputed(() => repos.value ?? []);
	const hasRows = useComputed(() => list.value.length > 0);

	// Reload on open (the instance mounts fresh per open — no `open` guard).
	useEffect(() => {
		batch(() => {
			repos.value = null;
			renamingId.value = null;
		})
		loadList();
	}, []);

	const close = () => {
		if (busy.value) return;
		props.onCloseRequest();
	};

	async function loadList() {
		hint.value = "Loading repositories…";
		try {
			const j = await api.listRepos();
			const l = j.repos || [];
			batch(() => {
				repos.value = l;
				hint.value = l.length ? "" : "No repositories yet — create the first one below.";
			})
		} catch (err) {
			batch(() => {
				repos.value = [];
				hint.value = (err as Error).message || "Could not load repositories.";
			})
		}
	}

	const create = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy.value) return;
		const n = name.value.trim();
		if (!n) { nameRef.current?.focus(); return; }
		busy.value = true;
		try {
			await api.createRepo(n);
			toast.show(`Created repository ${n}.`, "ok");
			name.value = "";
			await loadList();
			app.refreshAll();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the repository.", "err");
		} finally {
			busy.value = false;
		}
	};

	const startRename = (r: RepoInfo) => {
		if (busy.value) return;
		batch(() => {
			renameVal.value = r.name;
			renamingId.value = r.id;
		})
	};

	// The renaming row's editor mounts when the row flips: focus it.
	useSignalEffect(() => {
		if (renamingId.value) renameInputRef.current?.focus();
	});

	const submitRename = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		const id = renamingId.value;
		if (busy.value || !id) return;
		const newName = renameVal.value.trim();
		if (!newName) { renameInputRef.current?.focus(); return; }
		busy.value = true;
		try {
			await api.renameRepo(id, newName);
			toast.show("Repository renamed.", "ok");
			renamingId.value = null;
			await loadList();
			app.refreshAll();
		} catch (err) {
			toast.show((err as Error).message || "Could not rename the repository.", "err");
		} finally {
			busy.value = false;
		}
	};

	const toggleShare = async (r: RepoInfo) => {
		if (busy.value) return;
		busy.value = true;
		try {
			const j = await api.setRepoShare(r.id, !r.shared);
			if (j.shared && j.share_url) {
				shareUrls.value = { ...shareUrls.value, [r.id]: j.share_url as string };
				toast.show("Share link enabled — a fresh link was minted.", "ok");
			} else {
				const { [r.id]: _gone, ...rest } = shareUrls.value;
				shareUrls.value = rest;
				toast.show("Share link disabled.", "ok");
			}
			await loadList();
			app.refreshAll();
		} catch (err) {
			toast.show((err as Error).message || "Could not update the share link.", "err");
		} finally {
			busy.value = false;
		}
	};

	const copyShareURL = async (r: RepoInfo) => {
		const url = shareUrls.value[r.id];
		if (!url) return;
		toast.show(await copyText(url) ? "Share link copied." : "Could not copy — select the link manually.", "ok");
	};

	// Re-read the repository's current share link from the server (the token
	// is stored with the repo record, so this works after any reload) and
	// reveal it in the same box an enable shows. No rotation: recipients of
	// the previously shared link are unaffected.
	const showShareURL = async (r: RepoInfo) => {
		if (busy.value) return;
		busy.value = true;
		try {
			const j = await api.getRepoShare(r.id);
			if (j.shared && j.share_url) {
				shareUrls.value = { ...shareUrls.value, [r.id]: j.share_url as string };
			} else {
				toast.show("The current share link could not be shown — disable and re-enable to mint a fresh one.", "err");
			}
		} catch (err) {
			toast.show((err as Error).message || "Could not read the share link.", "err");
		} finally {
			busy.value = false;
		}
	};

	const deleteRepo = async (r: RepoInfo) => {
		if (busy.value) return;
		if (!confirm(
			`Delete repository "${r.name}" and its ${r.people} enrolled people? ` +
			"The dataset folder is removed too. This cannot be undone.",
		)) return;
		busy.value = true;
		try {
			const j = await api.deleteRepo(r.id);
			toast.show(`Removed ${r.name} (${j.people_removed} people, folder ${j.folder_removed ? "deleted" : "kept"}).`, "ok");
			await loadList();
			app.refreshAll();
		} catch (err) {
			toast.show((err as Error).message || "Could not delete the repository.", "err");
		} finally {
			busy.value = false;
		}
	};

	return (
		<Modal
			open={props.open}
			id="reposModal"
			cardId="reposCard"
			cardClass="wide"
			locked={busy}
			eyebrow="admin"
			titleId="reposTitle"
			title="Repositories"
			onCloseRequest={close}
			body={
				<div class="modal-body">
					<form class="admin-create-row" id="repoCreateForm" onSubmit={create}>
						<input
							ref={nameRef}
							type="text"
							placeholder="New repository name"
							maxLength={120}
							autocomplete="off"
							spellcheck={false}
							aria-label="New repository name"
							disabled={busy}
							value={name}
							onInput={(e) => { name.value = e.currentTarget.value; }}
						/>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy.value || !name.value.trim()}>
							Create repository
						</button>
					</form>

					<ul id="repoList" class="admin-list">
						<For each={list} getKey={(r) => r.id}>
							{(r) => (
								<RepoRow
									r={r}
									busy={busy}
									renamingId={renamingId}
									renameVal={renameVal}
									shareUrls={shareUrls}
									renameInputRef={renameInputRef}
									onStartRename={startRename}
									onSubmitRename={submitRename}
									onCancelRename={() => { renamingId.value = null; }}
									onToggleShare={toggleShare}
									onShowShareURL={showShareURL}
									onCopyShareURL={copyShareURL}
									onDelete={deleteRepo}
								/>
							)}
						</For>
					</ul>
					<p class="enroll-meta" id="reposMeta" hidden={hasRows}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

// One repository row: name/people/shared badge and the actions, swapping to
// an inline rename editor while this row is the renaming one, and showing
// the share-URL box once a link was minted/revealed. The reactive inputs
// (busy, renaming state, rename value, share URLs) arrive as signals and are
// read here, so the modal body does not re-render for them.
function RepoRow(props: {
	r: RepoInfo;
	busy: ReadonlySignal<boolean>;
	renamingId: ReadonlySignal<string | null>;
	renameVal: Signal<string>;
	shareUrls: ReadonlySignal<Record<string, string>>;
	renameInputRef: Ref<HTMLInputElement>;
	onStartRename: (r: RepoInfo) => void;
	onSubmitRename: (e: TargetedEvent<HTMLFormElement>) => void;
	onCancelRename: () => void;
	onToggleShare: (r: RepoInfo) => void;
	onShowShareURL: (r: RepoInfo) => void;
	onCopyShareURL: (r: RepoInfo) => void;
	onDelete: (r: RepoInfo) => void;
}) {
	const r = props.r;
	const busy = props.busy.value;
	const renaming = props.renamingId.value === r.id;
	const shareUrl = props.shareUrls.value[r.id];

	return (
		<li class="admin-row repo-row">
			{renaming ? (
				<form class="rename-row" onSubmit={props.onSubmitRename}>
					<input
						ref={props.renameInputRef}
						type="text"
						maxLength={120}
						autocomplete="off"
						spellcheck={false}
						aria-label="New repository name"
						disabled={busy}
						value={props.renameVal}
						onInput={(e) => { props.renameVal.value = e.currentTarget.value; }}
					/>
					<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Save</button>
					<button type="button" class="btn btn-ghost btn-sm" onClick={props.onCancelRename} disabled={busy}>Cancel</button>
				</form>
			) : (
				<>
					<div class="admin-row-main">
						<span class="admin-row-name">{r.name}</span>
						<span class="admin-row-sub" title={r.id}>{r.people} people</span>
						{r.shared && <span class="badge admin">shared</span>}
					</div>
					<div class="admin-row-actions">
						<button type="button" class="btn btn-ghost btn-sm" disabled={busy} onClick={() => props.onStartRename(r)}>
							Rename
						</button>
						{r.shared && !shareUrl && (
							<button
								type="button"
								class="btn btn-ghost btn-sm"
								disabled={busy}
								title="Show the current share link again so it can be copied"
								onClick={() => props.onShowShareURL(r)}
							>
								Show link
							</button>
						)}
						<button
							type="button"
							class="btn btn-ghost btn-sm"
							disabled={busy}
							title={r.shared
								? "Disable the public read-only share link (re-enabling mints a fresh link)"
								: "Enable a public read-only share link"}
							onClick={() => props.onToggleShare(r)}
						>
							{r.shared ? "Disable share" : "Share…"}
						</button>
						<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={busy} onClick={() => props.onDelete(r)}>
							Delete
						</button>
					</div>
				</>
			)}
			{r.shared && shareUrl && (
				<div class="share-url-box">
					<code title={shareUrl}>{shareUrl}</code>
					<button type="button" class="btn btn-sm" disabled={busy} onClick={() => props.onCopyShareURL(r)}>
						Copy link
					</button>
				</div>
			)}
			{r.shared && shareUrl && (
				<p class="field-hint">Anyone with the link can browse this repository read-only.</p>
			)}
		</li>
	);
}
