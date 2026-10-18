// Repositories manager (admin). Lists every repository (name, id, people
// count, shared state) and creates, renames and deletes them, and toggles
// the public read-only share link. Enabling mints a FRESH token and answers
// with the full share URL; the current link can always be re-read again
// ("Show link" button → GET) without rotating it. Disabling nulls it.
// After any change the list reloads and the parent refreshes its own
// repo state.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { RepoInfo } from "../types";
import { copyText } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface ReposModalProps {
	open: boolean;
	onCloseRequest: () => void;
	/** any change → the parent refreshes its repos list + people counts */
	onChanged: () => void;
}

export function ReposModal(props: ReposModalProps) {
	const toast = useToast();

	const [repos, setRepos] = useState<RepoInfo[] | null>(null);
	const [hint, setHint] = useState("Loading repositories…");
	const [busy, setBusyState] = useState(false);
	const [name, setName] = useState("");
	const [renamingId, setRenamingId] = useState<string | null>(null);
	const [renameVal, setRenameVal] = useState("");
	// The share URL of the revealed/most-recently-minted link, per repo id
	// (the listing itself carries no URLs — one comes back from the enable
	// call or from the GET reveal).
	const [shareUrls, setShareUrls] = useState<Record<string, string>>({});

	const busyRef = useRef(false);
	const nameRef = useRef<HTMLInputElement | null>(null);
	const renameRef = useRef<HTMLInputElement | null>(null);

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	// Reload the list every time the modal opens.
	useEffect(() => {
		if (!props.open) return;
		setRepos(null);
		setRenamingId(null);
		loadList();
	}, [props.open]);

	const close = () => {
		if (busyRef.current) return;
		props.onCloseRequest();
	};

	async function loadList() {
		setHint("Loading repositories…");
		try {
			const j = await api.listRepos();
			const list = j.repos || [];
			setRepos(list);
			setHint(list.length ? "" : "No repositories yet — create the first one below.");
		} catch (err) {
			setRepos([]);
			setHint((err as Error).message || "Could not load repositories.");
		}
	}

	const create = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current) return;
		const n = name.trim();
		if (!n) { nameRef.current?.focus(); return; }
		setBusy(true);
		try {
			await api.createRepo(n);
			toast.show(`Created repository ${n}.`, "ok");
			setName("");
			await loadList();
			props.onChanged();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the repository.", "err");
		} finally {
			setBusy(false);
		}
	};

	function startRename(r: RepoInfo) {
		if (busyRef.current) return;
		setRenameVal(r.name);
		setRenamingId(r.id);
	}

	useEffect(() => {
		if (renamingId) renameRef.current?.focus();
	}, [renamingId]);

	const submitRename = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current || !renamingId) return;
		const newName = renameVal.trim();
		if (!newName) { renameRef.current?.focus(); return; }
		setBusy(true);
		try {
			await api.renameRepo(renamingId, newName);
			toast.show("Repository renamed.", "ok");
			setRenamingId(null);
			await loadList();
			props.onChanged();
		} catch (err) {
			toast.show((err as Error).message || "Could not rename the repository.", "err");
		} finally {
			setBusy(false);
		}
	};

	const toggleShare = async (r: RepoInfo) => {
		if (busyRef.current) return;
		setBusy(true);
		try {
			const j = await api.setRepoShare(r.id, !r.shared);
			if (j.shared && j.share_url) {
				setShareUrls((m) => ({ ...m, [r.id]: j.share_url as string }));
				toast.show("Share link enabled — a fresh link was minted.", "ok");
			} else {
				setShareUrls((m) => {
					const { [r.id]: _gone, ...rest } = m;
					return rest;
				});
				toast.show("Share link disabled.", "ok");
			}
			await loadList();
			props.onChanged();
		} catch (err) {
			toast.show((err as Error).message || "Could not update the share link.", "err");
		} finally {
			setBusy(false);
		}
	};

	const copyShareURL = async (r: RepoInfo) => {
		const url = shareUrls[r.id];
		if (!url) return;
		toast.show(await copyText(url) ? "Share link copied." : "Could not copy — select the link manually.", "ok");
	};

	// Re-read the repository's current share link from the server (the token
	// is stored with the repo record, so this works after any reload) and
	// reveal it in the same box an enable shows. No rotation: recipients of
	// the previously shared link are unaffected.
	const showShareURL = async (r: RepoInfo) => {
		if (busyRef.current) return;
		setBusy(true);
		try {
			const j = await api.getRepoShare(r.id);
			if (j.shared && j.share_url) {
				setShareUrls((m) => ({ ...m, [r.id]: j.share_url as string }));
			} else {
				toast.show("The current share link could not be shown — disable and re-enable to mint a fresh one.", "err");
			}
		} catch (err) {
			toast.show((err as Error).message || "Could not read the share link.", "err");
		} finally {
			setBusy(false);
		}
	};

	const deleteRepo = async (r: RepoInfo) => {
		if (busyRef.current) return;
		if (!confirm(
			`Delete repository "${r.name}" and its ${r.people} enrolled people? ` +
			"The dataset folder is removed too. This cannot be undone.",
		)) return;
		setBusy(true);
		try {
			const j = await api.deleteRepo(r.id);
			toast.show(`Removed ${r.name} (${j.people_removed} people, folder ${j.folder_removed ? "deleted" : "kept"}).`, "ok");
			await loadList();
			props.onChanged();
		} catch (err) {
			toast.show((err as Error).message || "Could not delete the repository.", "err");
		} finally {
			setBusy(false);
		}
	};

	const list = repos || [];

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
							onInput={(e) => setName(e.currentTarget.value)}
						/>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy || !name.trim()}>
							Create repository
						</button>
					</form>

					<ul id="repoList" class="admin-list">
						{list.map((r) => (
							<li key={r.id} class="admin-row repo-row">
								{renamingId === r.id ? (
									<form class="rename-row" onSubmit={submitRename}>
										<input
											ref={renameRef}
											type="text"
											maxLength={120}
											autocomplete="off"
											spellcheck={false}
											aria-label="New repository name"
											disabled={busy}
											value={renameVal}
											onInput={(e) => setRenameVal(e.currentTarget.value)}
										/>
										<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Save</button>
										<button type="button" class="btn btn-ghost btn-sm" onClick={() => setRenamingId(null)} disabled={busy}>Cancel</button>
									</form>
								) : (
									<>
										<div class="admin-row-main">
											<span class="admin-row-name">{r.name}</span>
											<span class="admin-row-sub" title={r.id}>{r.people} people</span>
											{r.shared && <span class="badge admin">shared</span>}
										</div>
										<div class="admin-row-actions">
											<button type="button" class="btn btn-ghost btn-sm" disabled={busy} onClick={() => startRename(r)}>
												Rename
											</button>
											{r.shared && !shareUrls[r.id] && (
												<button
													type="button"
													class="btn btn-ghost btn-sm"
													disabled={busy}
													title="Show the current share link again so it can be copied"
													onClick={() => showShareURL(r)}
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
												onClick={() => toggleShare(r)}
											>
												{r.shared ? "Disable share" : "Share…"}
											</button>
											<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={busy} onClick={() => deleteRepo(r)}>
												Delete
											</button>
										</div>
									</>
								)}
								{r.shared && shareUrls[r.id] && (
									<div class="share-url-box">
										<code title={shareUrls[r.id]}>{shareUrls[r.id]}</code>
										<button type="button" class="btn btn-sm" disabled={busy} onClick={() => copyShareURL(r)}>
											Copy link
										</button>
									</div>
								)}
								{r.shared && shareUrls[r.id] && (
									<p class="field-hint">Anyone with the link can browse this repository read-only.</p>
								)}
							</li>
						))}
					</ul>
					<p class="enroll-meta" id="reposMeta" hidden={list.length > 0}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}
