// Users manager (admin). Lists accounts (username, admin badge, grants
// summary), creates users, resets passwords, deletes users (the caller's
// own account and the last remaining admin are refused server-side; the
// self row's delete button is hidden), and edits per-repo grants: one row
// per existing repository with a no-access/reader/writer select, saved as a
// FULL-REPLACE of that user's grants.
//
// State as signals; the user rows are a <For> of UserRow components (keyed
// by the username) that receive the item plus the signals they show
// (busy, repos, grantsUser, grantSel, pwUser, pwVal) and read .value
// internally — an expansion toggle or a busy flip re-renders the rows, not
// the modal body.

import { useEffect, useRef } from "preact/hooks";
import type { Ref, TargetedEvent } from "preact";
import { batch, useComputed, useSignal, useSignalEffect, type ReadonlySignal, type Signal } from "@preact/signals";
import { For } from "@preact/signals/utils";
import * as api from "../api";
import type { GrantRole, RepoInfo, UserInfo } from "../types";
import { USERNAME_RE } from "../util";
import { useToast } from "../toast";
import { useApp } from "../appStore";
import { Modal } from "./Modal";

interface UsersModalProps {
	/** injected by the modal service (instances render open) */
	open: boolean;
	/** injected by the modal service; closing unmounts the instance */
	onCloseRequest: () => void;
}

const PASSWORD_MIN = 8;

type GrantChoice = "none" | GrantRole;

export function UsersModal(props: UsersModalProps) {
	const toast = useToast();
	const app = useApp();
	// The signed-in admin's own username, to mark "(you)".
	const currentUser = app.user.value?.name ?? "";

	const users = useSignal<UserInfo[] | null>(null);
	const repos = useSignal<RepoInfo[] | null>(null);
	const hint = useSignal("Loading users…");
	const busy = useSignal(false);

	// create form
	const name = useSignal("");
	const password = useSignal("");
	const admin = useSignal(false);

	// grants editor: the expanded user + per-repo role choices
	const grantsUser = useSignal<string | null>(null);
	const grantSel = useSignal<Record<string, GrantChoice>>({});

	// password reset: the expanded user + the new password field
	const pwUser = useSignal<string | null>(null);
	const pwVal = useSignal("");

	const nameRef = useRef<HTMLInputElement | null>(null);
	// The password-reset editor's input (mounted by the expanded row).
	const pwInputRef = useRef<HTMLInputElement | null>(null);
	const list = useComputed(() => users.value ?? []);
	const hasRows = useComputed(() => list.value.length > 0);

	// Reload both lists on open (the instance mounts fresh per open — no
	// `open` guard); the expansion states reset with them.
	useEffect(() => {
		batch(() => {
			users.value = null;
			repos.value = null;
			grantsUser.value = null;
			pwUser.value = null;
		})
		loadList();
		api.listRepos().then((j) => { repos.value = j.repos || []; }).catch(() => { repos.value = []; });
	}, []);

	const close = () => {
		if (busy.value) return;
		props.onCloseRequest();
	};

	async function loadList() {
		hint.value = "Loading users…";
		try {
			const j = await api.listUsers();
			const l = j.users || [];
			batch(() => {
				users.value = l;
				hint.value = l.length ? "" : "No users yet — create the first one below.";
			})
		} catch (err) {
			batch(() => {
				users.value = [];
				hint.value = (err as Error).message || "Could not load users.";
			})
		}
	}

	const create = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		if (busy.value) return;
		const u = name.value.trim();
		if (!USERNAME_RE.test(u)) {
			toast.show("Username must be 2–32 characters and start with a letter or digit.", "err");
			nameRef.current?.focus();
			return;
		}
		if (password.value.length < PASSWORD_MIN) {
			toast.show(`Password must be at least ${PASSWORD_MIN} characters.`, "err");
			return;
		}
		busy.value = true;
		try {
			await api.createUser(u, password.value, admin.value);
			toast.show(`Created user ${u}.`, "ok");
			batch(() => {
				name.value = "";
				password.value = "";
				admin.value = false;
			})
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not create the user.", "err");
		} finally {
			busy.value = false;
		}
	};

	// ---- grants editor ----
	const toggleGrants = (u: UserInfo) => {
		if (grantsUser.value === u.username) { grantsUser.value = null; return; }
		if (busy.value || repos.value === null) return;
		// repos === null means the list is still loading: an editor opened
		// now would show no repo rows and its Save would full-replace the
		// user's grants with an empty list — wiping real access.
		pwUser.value = null;
		const sel: Record<string, GrantChoice> = {};
		for (const r of repos.value) sel[r.id] = "none";
		for (const g of u.grants) {
			// Pre-select roles that the repo list knows (and that the editor
			// can represent); anything else defaults to no-access.
			if (sel[g.repo_id] !== undefined && (g.role === "reader" || g.role === "writer")) {
				sel[g.repo_id] = g.role;
			}
		}
		batch(() => {
			grantSel.value = sel;
			grantsUser.value = u.username;
		})
	};

	const changeGrant = (repoId: string, role: GrantChoice) => {
		grantSel.value = { ...grantSel.value, [repoId]: role };
	};

	const saveGrants = async (u: UserInfo) => {
		if (busy.value) return;
		const grants = (repos.value ?? [])
			.filter((r) => grantSel.value[r.id] === "reader" || grantSel.value[r.id] === "writer")
			.map((r) => ({ repo: r.id, role: grantSel.value[r.id] as GrantRole }));
		busy.value = true;
		try {
			await api.setUserGrants(u.username, grants);
			toast.show(`Grants saved for ${u.username}.`, "ok");
			grantsUser.value = null;
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not update the grants.", "err");
		} finally {
			busy.value = false;
		}
	};

	// ---- password reset ----
	const toggleReset = (u: UserInfo) => {
		if (pwUser.value === u.username) { pwUser.value = null; return; }
		if (busy.value) return;
		batch(() => {
			grantsUser.value = null;
			pwVal.value = "";
			pwUser.value = u.username;
		})
	};

	// The expanded row's reset input mounts when pwUser flips: focus it.
	useSignalEffect(() => {
		if (pwUser.value) pwInputRef.current?.focus();
	});

	const saveReset = async (e: TargetedEvent<HTMLFormElement>) => {
		e.preventDefault();
		const u = pwUser.value;
		if (busy.value || !u) return;
		if (pwVal.value.length < PASSWORD_MIN) {
			toast.show(`Password must be at least ${PASSWORD_MIN} characters.`, "err");
			pwInputRef.current?.focus();
			return;
		}
		busy.value = true;
		try {
			const j = await api.resetUserPassword(u, pwVal.value);
			// One toast (single-slot): the confirmation carries the server's
			// API-key notice — the user's keys were left intact and should be
			// reviewed if the account may be compromised.
			toast.show(`Password reset for ${u}.${j.notice ? " " + j.notice : ""}`, "ok");
			batch(() => {
				pwUser.value = null;
				pwVal.value = "";
			})
		} catch (err) {
			toast.show((err as Error).message || "Could not reset the password.", "err");
		} finally {
			busy.value = false;
		}
	};

	// ---- delete ----
	const remove = async (u: UserInfo) => {
		if (busy.value) return;
		if (!confirm(`Delete user ${u.username}? They lose access to every repository.`)) return;
		busy.value = true;
		try {
			await api.deleteUser(u.username);
			toast.show(`Deleted ${u.username}.`, "ok");
			await loadList();
		} catch (err) {
			toast.show((err as Error).message || "Could not delete the user.", "err");
		} finally {
			busy.value = false;
		}
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
							onInput={(e) => { name.value = e.currentTarget.value; }}
						/>
						<input
							type="password"
							placeholder={`Password (${PASSWORD_MIN}+ chars)`}
							autocomplete="new-password"
							aria-label="New user's password"
							disabled={busy}
							value={password}
							onInput={(e) => { password.value = e.currentTarget.value; }}
						/>
						<label class="check-row" title="Administrators manage repositories and users">
							<input type="checkbox" disabled={busy} checked={admin} onChange={(e) => { admin.value = e.currentTarget.checked; }} />
							<span>admin</span>
						</label>
						<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Create user</button>
					</form>

					<ul id="userList" class="admin-list">
						<For each={list} getKey={(u) => u.username}>
							{(u) => (
								<UserRow
									u={u}
									currentUser={currentUser}
									busy={busy}
									repos={repos}
									grantsUser={grantsUser}
									grantSel={grantSel}
									pwUser={pwUser}
									pwVal={pwVal}
									pwInputRef={pwInputRef}
									onToggleGrants={toggleGrants}
									onChangeGrant={changeGrant}
									onSaveGrants={saveGrants}
									onToggleReset={toggleReset}
									onSaveReset={saveReset}
									onRemove={remove}
								/>
							)}
						</For>
					</ul>
					<p class="enroll-meta" id="usersMeta" hidden={hasRows}>{hint}</p>

					<footer class="modal-foot">
						<button type="button" class="btn btn-ghost" data-close disabled={busy}>Close</button>
					</footer>
				</div>
			}
		/>
	);
}

// One user row: identity + grants summary, the Grants/Reset/Delete actions,
// and — while this row is expanded — the grants editor or the password-reset
// form. The reactive inputs (busy, the repo list, the expansion states, the
// per-repo role choices) arrive as signals and are read here, so the modal
// body does not re-render for them.
function UserRow(props: {
	u: UserInfo;
	/** the signed-in admin's username (marks "(you)", hides self-delete) */
	currentUser: string;
	busy: ReadonlySignal<boolean>;
	repos: ReadonlySignal<RepoInfo[] | null>;
	grantsUser: ReadonlySignal<string | null>;
	grantSel: ReadonlySignal<Record<string, GrantChoice>>;
	pwUser: ReadonlySignal<string | null>;
	pwVal: Signal<string>;
	pwInputRef: Ref<HTMLInputElement>;
	onToggleGrants: (u: UserInfo) => void;
	onChangeGrant: (repoId: string, role: GrantChoice) => void;
	onSaveGrants: (u: UserInfo) => void;
	onToggleReset: (u: UserInfo) => void;
	onSaveReset: (e: TargetedEvent<HTMLFormElement>) => void;
	onRemove: (u: UserInfo) => void;
}) {
	const u = props.u;
	const busy = props.busy.value;
	const repoList = props.repos.value ?? [];
	const grantsOpen = props.grantsUser.value === u.username;
	const resetOpen = props.pwUser.value === u.username;
	const isSelf = u.username === props.currentUser;

	// Grants summary text: "repo: role" pairs; empty = no access yet.
	const grantsText = (() => {
		const parts = u.grants.map((g) => {
			const repo = repoList.find((r) => r.id === g.repo_id);
			return `${repo ? repo.name : g.repo_id.slice(0, 8) + "…"}: ${g.role}`;
		});
		return parts.length ? parts.join(", ") : "no access";
	})();

	return (
		<li class="admin-row user-row">
			<div class="admin-row-main">
				<span class="admin-row-name">{u.username}</span>
				{isSelf && <span class="badge you">you</span>}
				{u.admin && <span class="badge admin">admin</span>}
				<span class="admin-row-sub" title="Repository access">{grantsText}</span>
			</div>
			<div class="admin-row-actions">
				<button
					type="button"
					class="btn btn-ghost btn-sm"
					disabled={busy || props.repos.value === null}
					title={props.repos.value === null ? "Loading repositories…" : undefined}
					onClick={() => props.onToggleGrants(u)}
				>
					{grantsOpen ? "Close grants" : "Grants"}
				</button>
				<button type="button" class="btn btn-ghost btn-sm" disabled={busy} onClick={() => props.onToggleReset(u)}>
					{resetOpen ? "Close" : "Reset password"}
				</button>
				{!isSelf && (
					<button type="button" class="btn btn-ghost btn-sm admin-danger" disabled={busy} onClick={() => props.onRemove(u)}>
						Delete
					</button>
				)}
			</div>

			{grantsOpen && (
				<div class="grants-editor">
					{repoList.length === 0 && <p class="field-hint">No repositories exist yet — create one in the Repositories manager first.</p>}
					{repoList.map((r) => (
						<div class="grant-row" key={r.id}>
							<span class="grant-repo" title={r.id}>{r.name}</span>
							<select
								class="grant-select"
								aria-label={`Role for ${r.name}`}
								disabled={busy}
								value={props.grantSel.value[r.id] ?? "none"}
								onChange={(e) => props.onChangeGrant(r.id, e.currentTarget.value as GrantChoice)}
							>
								<option value="none">no access</option>
								<option value="reader">reader</option>
								<option value="writer">writer</option>
							</select>
						</div>
					))}
					<div class="grants-foot">
						<button type="button" class="btn btn-accent btn-sm" disabled={busy || repoList.length === 0} onClick={() => props.onSaveGrants(u)}>
							Save grants
						</button>
						<p class="field-hint">Saving replaces all of this user's grants.</p>
					</div>
				</div>
			)}

			{resetOpen && (
				<form class="pw-reset-row" onSubmit={props.onSaveReset}>
					<input
						ref={props.pwInputRef}
						type="password"
						placeholder={`New password (${PASSWORD_MIN}+ chars)`}
						autocomplete="new-password"
						aria-label={`New password for ${u.username}`}
						disabled={busy}
						value={props.pwVal}
						onInput={(e) => { props.pwVal.value = e.currentTarget.value; }}
					/>
					<button type="submit" class="btn btn-accent btn-sm" disabled={busy}>Set password</button>
				</form>
			)}
		</li>
	);
}
