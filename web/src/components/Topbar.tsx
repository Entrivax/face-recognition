// Topbar: brand mark, live server status pill, the repository selector, and
// the account controls (username chip, API keys, change password, log out;
// admin additionally gets the Users and Repositories managers).

import type { TargetedEvent } from "preact";
import type { Health, RepoInfo, SessionUser } from "../types";

interface TopbarProps {
	health: Health | null;
	err: boolean;
	user: SessionUser;
	repos: RepoInfo[];
	repoId: string | null;
	onRepoChange: (id: string) => void;
	onLogout: () => void;
	onRepos: () => void;
	onUsers: () => void;
	onKeys: () => void;
	onPassword: () => void;
}

export function Topbar({
	health, err, user, repos, repoId,
	onRepoChange, onLogout, onRepos, onUsers, onKeys, onPassword,
}: TopbarProps) {
	const cls = err ? "status err" : health ? "status ok" : "status";
	const text = err ? "server unreachable" : health ? health.status : "connecting…";
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
				{repos.length > 0 && (
					<select
						id="repoSelect"
						class="repo-select"
						aria-label="Repository"
						title={repos.length < 2 ? undefined : "Switch repository"}
						disabled={repos.length < 2}
						value={repoId ?? ""}
						onChange={(e: TargetedEvent<HTMLSelectElement>) => onRepoChange(e.currentTarget.value)}
					>
						{repos.map((r) => (
							<option key={r.id} value={r.id}>{r.name} ({r.people})</option>
						))}
					</select>
				)}
				<div class={cls} id="status">
					<span class="status-dot" id="statusDot" />
					<span id="statusText">{text}</span>
				</div>
				<span class="user-chip" id="userChip">
					<span class="user-name">{user.name}</span>
					{user.admin && <span class="badge admin">admin</span>}
				</span>
				{user.admin && (
					<>
						<button id="reposBtn" class="btn btn-sm" onClick={onRepos}>
							Repositories
						</button>
						<button id="usersBtn" class="btn btn-sm" onClick={onUsers}>
							Users
						</button>
					</>
				)}
				<button id="keysBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={onKeys}>
					API keys
				</button>
				<button id="passwordBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={onPassword}>
					Change password
				</button>
				<button id="logoutBtn" class="btn btn-ghost btn-sm" onClick={onLogout}>
					Log out
				</button>
			</div>
		</header>
	);
}
