// Users manager (admin). Lists accounts (username, admin badge, grants
// summary), creates users, resets passwords, deletes users (the caller's
// own account and the last remaining admin are refused server-side; the
// self row's delete button is hidden), and edits per-repo grants: one row
// per existing repository with a no-access/reader/writer select, saved as a
// FULL-REPLACE of that user's grants.

import { useEffect, useRef, useState } from "preact/hooks";
import type { TargetedEvent } from "preact";
import * as api from "../api";
import type { GrantRole, RepoInfo, UserInfo } from "../types";
import { USERNAME_RE } from "../util";
import { useToast } from "../toast";
import { Modal } from "./Modal";

interface UsersModalProps {
	open: boolean;
	/** the signed-in admin's own username, to mark "(you)" */
	currentUser: string;
	onCloseRequest: () => void;
}

const PASSWORD_MIN = 8;

type GrantChoice = "none" | GrantRole;

export function UsersModal(props: UsersModalProps) {
	const toast = useToast();

	const [users, setUsers] = useState<UserInfo[] | null>(null);
	const [repos, setRepos] = useState<RepoInfo[] | null>(null);
	const [hint, setHint] = useState("Loading users…");
	const [busy, setBusyState] = useState(false);

	// create form
	const [name, setName] = useState("");
	const [password, setPassword] = useState("");
	const [admin, setAdmin] = useState(false);

	// grants editor: the expanded user + per-repo role choices
	const [grantsUser, setGrantsUser] = useState<string | null>(null);
	const [grantSel, setGrantSel] = useState<Record<string, GrantChoice>>({});

	// password reset: the expanded user + the new password field
	const [pwUser, setPwUser] = useState<string | null>(null);
	const [pwVal, setPwVal] = useState("");

	const busyRef = useRef(false);
	const nameRef = useRef<HTMLInputElement | null>(null);
	const pwRef = useRef<HTMLInputElement | null>(null);

	const setBusy = (b: boolean) => {
		busyRef.current = b;
		setBusyState(b);
	};

	// Reload both lists every time the modal opens.
	useEffect(() => {
		if (!props.open) return;
		setUsers(null);
		setRepos(null);
		setGrantsUser(null);
		setPwUser(null);
		loadList();
		api.listRepos().then((j) => setRepos(j.repos || [])).catch(() => setRepos([]));
	}, [props.open]);

	const close = () => {
		if (busyRef.current) return;
		props.onCloseRequest();
	};

	async function loadList() {
		setHint("Loading users…");
		try {
			const j = await api.listUsers();
			const list = j.users || [];
			setUsers(list);
			setHint(list.length ? "" : "No users yet — create the first one below.");
		} catch (err) {
			setUsers([]);
			setHint((err as Error).message || "Could not load users.");
		}
	}

	const create = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current) return;
		const u = name.trim();
		if (!USERNAME_RE.test(u)) {
			toast.show("Username must be 2–32 characters and start with a letter or digit.", "err");
			nameRef.current?.focus();
			return;
		}
		if (password.length < PASSWORD_MIN) {
			toast.show(`Password must be at least ${PASSWORD_MIN} characters.`, "err");
			return;
		}
		setBusy(true);
		try {
			await api.createUser(u, password, admin);
			toast.show(`Created user ${u}.`, "ok");
			setName("");
			setPassword("");
			setAdmin(false);
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the user.", "err");
		} finally {
			setBusy(false);
		}
	};

	// ---- grants editor ----
	function openGrants(u: UserInfo) {
		if (busyRef.current || repos === null) return;
		// repos === null means the list is still loading: an editor opened
		// now would show no repo rows and its Save would full-replace the
		// user's grants with an empty list — wiping real access.
		setPwUser(null);
		const sel: Record<string, GrantChoice> = {};
		for (const r of repos ?? []) sel[r.id] = "none";
		for (const g of u.grants) {
			// Pre-select roles that the repo list knows (and that the editor
			// can represent); anything else defaults to no-access.
			if (sel[g.repo_id] !== undefined && (g.role === "reader" || g.role === "writer")) {
				sel[g.repo_id] = g.role;
			}
		}
		setGrantSel(sel);
		setGrantsUser(u.username);
	}

	const changeGrant = (repoId: string, role: GrantChoice) => {
		setGrantSel((s) => ({ ...s, [repoId]: role }));
	};

	const saveGrants = async (u: UserInfo) => {
		if (busyRef.current) return;
		const grants = (repos ?? [])
			.filter((r) => grantSel[r.id] === "reader" || grantSel[r.id] === "writer")
			.map((r) => ({ repo: r.id, role: grantSel[r.id] as GrantRole }));
		setBusy(true);
		try {
			await api.setUserGrants(u.username, grants);
			toast.show(`Grants saved for ${u.username}.`, "ok");
			setGrantsUser(null);
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not update the grants.", "err");
		} finally {
			setBusy(false);
		}
	};

	// ---- password reset ----
	function openReset(u: UserInfo) {
		if (busyRef.current) return;
		setGrantsUser(null);
		setPwVal("");
		setPwUser(u.username);
	}

	useEffect(() => {
		if (pwUser) pwRef.current?.focus();
	}, [pwUser]);

	const saveReset = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busyRef.current || !pwUser) return;
		if (pwVal.length < PASSWORD_MIN) {
			toast.show(`Password must be at least ${PASSWORD_MIN} characters.`, "err");
			pwRef.current?.focus();
			return;
		}
		setBusy(true);
		try {
			const j = await api.resetUserPassword(pwUser, pwVal);
			// One toast (single-slot): the confirmation carries the server's
			// API-key notice — the user's keys were left intact and should be
			// reviewed if the account may be compromised.
			toast.show(`Password reset for ${pwUser}.${j.notice ? " " + j.notice : ""}`, "ok");
			setPwUser(null);
			setPwVal("");
		} catch (err) {
			toast.show((err as Error).message || "Could not reset the password.", "err");
		} finally {
			setBusy(false);
		}
	};

	// ---- delete ----
	const remove = async (u: UserInfo) => {
		if (busyRef.current) return;
		if (!confirm(`Delete user ${u.username}? They lose access to every repository.`)) return;
		setBusy(true);
		try {
			await api.deleteUser(u.username);
			toast.show(`Deleted ${u.username}.`, "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not delete the user.", "err");
		} finally {
			setBusy(false);
		}
	};

	const list = users || [];
	const repoList = repos || [];
	// Grants summary text: "repo: role" pairs; empty = no access yet.
	const grantsText = (u: UserInfo) => {
		const parts = u.grants.map((g) => {
			const repo = repoList.find((r) => r.id === g.repo_id);
			return `${repo ? repo.name : g.repo_id.slice(0, 8) + "…"}: ${g.role}`;
		});
		return parts.length ? parts.join(", ") : "no access";
	};

	return (
		<Modal
			open={props.open}
			id="usersModal"
			cardId="usersCard"
			cardClass="wide"
			locked={busy}
			eyebrow="admin"
			titleId="usersTitle"
			title="Users"
			onCloseRequest={close}
			body={
				<div class="modal-body">
					<form class="admin-create-row" id="userCreateForm" onSubmit={create}>
						<input
							ref={nameRef}
							type="text"
							placeholder="Username"
							maxLength={32}
							autocomplete="off"
							spellcheck={false}
							aria-label="New username"
							disabled={busy}
							value={name}
							onInput={(e) => setName(e.currentTarget.value)}
						/>
						<input
							type="password"
							placeholder={`Password (${PASSWORD_MIN}+ chars)`}
							autocomplete="new-password"
							aria-label="New user's password"
							disabled={busy}
							value={password}
							onInput={(e) => setPassword(e.currentTarget.value)}
						/>
						<label class="check-row" title="Administrators manage repositories and users">
							<input type="checkbox" disabled={busy} checked={admin} onChange={(e) => setAdmin(e.currentTarget.checked)} />
							<span>admin</span>
						</label>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Create user</button>
					</form>

					<ul id="userList" class="admin-list">
						{list.map((u) => (
							<li key={u.username} class="admin-row user-row">
								<div class="admin-row-main">
									<span class="admin-row-name">{u.username}</span>
									{u.username === props.currentUser && <span class="badge you">you</span>}
									{u.admin && <span class="badge admin">admin</span>}
									<span class="admin-row-sub" title="Repository access">{grantsText(u)}</span>
								</div>
								<div class="admin-row-actions">
									<button
										type="button"
										class="btn btn-ghost btn-sm"
										disabled={busy || repos === null}
										title={repos === null ? "Loading repositories…" : undefined}
										onClick={() => (grantsUser === u.username ? setGrantsUser(null) : openGrants(u))}
									>
										{grantsUser === u.username ? "Close grants" : "Grants"}
									</button>
									<button type="button" class="btn btn-ghost btn-sm" disabled={busy} onClick={() => (pwUser === u.username ? setPwUser(null) : openReset(u))}>
										{pwUser === u.username ? "Close" : "Reset password"}
									</button>
									{u.username !== props.currentUser && (
										<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={busy} onClick={() => remove(u)}>
											Delete
										</button>
									)}
								</div>

								{grantsUser === u.username && (
									<div class="grants-editor">
										{repoList.length === 0 && <p class="field-hint">No repositories exist yet — create one in the Repositories manager first.</p>}
										{repoList.map((r) => (
											<div class="grant-row" key={r.id}>
												<span class="grant-repo" title={r.id}>{r.name}</span>
												<select
													class="grant-select"
													aria-label={`Role for ${r.name}`}
													disabled={busy}
													value={grantSel[r.id] ?? "none"}
													onChange={(e) => changeGrant(r.id, e.currentTarget.value as GrantChoice)}
												>
													<option value="none">no access</option>
													<option value="reader">reader</option>
													<option value="writer">writer</option>
												</select>
											</div>
										))}
										<div class="grants-foot">
											<button type="button" class="btn btn-accent btn-sm" disabled={busy || repoList.length === 0} onClick={() => saveGrants(u)}>
												Save grants
											</button>
											<p class="field-hint">Saving replaces all of this user's grants.</p>
										</div>
									</div>
								)}

								{pwUser === u.username && (
									<form class="pw-reset-row" onSubmit={saveReset}>
										<input
											ref={pwRef}
											type="password"
											placeholder={`New password (${PASSWORD_MIN}+ chars)`}
											autocomplete="new-password"
											aria-label={`New password for ${u.username}`}
											disabled={busy}
											value={pwVal}
											onInput={(e) => setPwVal(e.currentTarget.value)}
										/>
										<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Set password</button>
									</form>
								)}
							</li>
						))}
					</ul>
					<p class="enroll-meta" id="usersMeta" hidden={list.length > 0}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}
