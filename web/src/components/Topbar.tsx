// Topbar: brand mark, live server status pill, the repository selector, and
// the account controls (username chip, API keys, change password, log out;
// admin additionally gets the Users and Repositories managers).
// Data comes from the app store as signals; the dialogs open through the
// modal service. Each block reads its own signals in the smallest component,
// so a health poll or a repo-list refresh re-renders that block only — the
// header itself reads nothing reactive.

import type { TargetedEvent } from "preact";
import { useComputed } from "@preact/signals";
import { For, Show } from "@preact/signals/utils";
import { useApp } from "../appStore";
import { useModals } from "../modalRegistry";

export function Topbar() {
	const app = useApp();
	const modals = useModals();
	const user = app.user.value;
	const hasRepos = useComputed(() => app.repos.value.length > 0);

	return (
		<header class="topbar">
			<div class="brand">
				<span class="brand-mark" aria-hidden="true">
					<img src="/logo.svg" />
				</span>
				<span class="brand-word">recogn</span>
				<span class="brand-sub">face review console</span>
			</div>
			<div class="topbar-auth">
				<Show when={hasRepos}>
					{() => <RepoSelect />}
				</Show>
				<StatusPill />
				<span class="user-chip" id="userChip">
					<span class="user-name">{user?.name}</span>
					{user?.admin && <span class="badge admin">admin</span>}
				</span>
				{user?.admin && (
					<>
						<button id="reposBtn" class="btn btn-sm" onClick={() => modals.open("repos")}>
							Repositories
						</button>
						<button id="usersBtn" class="btn btn-sm" onClick={() => modals.open("users")}>
							Users
						</button>
					</>
				)}
				<button id="keysBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={() => modals.open("keys")}>
					API keys
				</button>
				<button id="passwordBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={() => modals.open("password")}>
					Change password
				</button>
				<button id="logoutBtn" class="btn btn-ghost btn-sm" onClick={() => void app.logout()}>
					Log out
				</button>
			</div>
		</header>
	);
}

// The repository selector: exists only while repositories are known (the
// Topbar gates it); the option rows and the selection ride the store's
// signals, so a refreshAll re-renders just this select.
function RepoSelect() {
	const app = useApp();
	const repos = app.repos.value;
	const repoIdAttr = useComputed(() => app.repoId.value ?? "");
	return (
		<select
			id="repoSelect"
			class="repo-select"
			aria-label="Repository"
			title={repos.length < 2 ? undefined : "Switch repository"}
			disabled={repos.length < 2}
			value={repoIdAttr}
			onChange={(e: TargetedEvent<HTMLSelectElement>) => app.setRepoId(e.currentTarget.value)}
		>
			<For each={app.repos} getKey={(r) => r.id}>
				{(r) => <option value={r.id}>{r.name} ({r.people})</option>}
			</For>
		</select>
	);
}

// The live server status pill: the only topbar block a health poll touches.
// Exported because ShareView's header reuses it (same ids/classes as the old
// static UI); it reads the app store from context, so it serves both trees.
export function StatusPill() {
	const app = useApp();
	const cls = useComputed(() =>
		app.healthErr.value ? "status err" : app.health.value ? "status ok" : "status",
	);
	const text = useComputed(() =>
		app.healthErr.value
			? "server unreachable"
			: app.health.value
				? app.health.value.status
				: "connecting…",
	);
	return (
		<div class={cls} id="status">
			<span class="status-dot" id="statusDot" />
			<span id="statusText">{text}</span>
		</div>
	);
}
