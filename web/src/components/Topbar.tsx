// Topbar: brand mark, live server status pill, and the auth controls
// (login when the server has a method configured and we're logged out;
// passkeys manager + logout while logged in; nothing new in open mode).

import type { AuthMethods, Health } from "../types";

interface TopbarProps {
	health: Health | null;
	err: boolean;
	authed: boolean;
	/** which admin-login methods the server has configured */
	authMethods: AuthMethods;
	onLogin: () => void;
	onLogout: () => void;
	onPasskeys: () => void;
}

export function Topbar({ health, err, authed, authMethods, onLogin, onLogout, onPasskeys }: TopbarProps) {
	const cls = err ? "status err" : health ? "status ok" : "status";
	const text = err
		? "server unreachable"
		: health
			? `${health.people} people · threshold ${health.threshold.toFixed(2)}`
			: "connecting…";
	const anyMethod = authMethods.password || authMethods.passkey;
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
				<div class={cls} id="status">
					<span class="status-dot" id="statusDot" />
					<span id="statusText">{text}</span>
				</div>
				{!authed && anyMethod && (
					<button id="loginBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={onLogin}>
						Log in
					</button>
				)}
				{authed && (
					<>
						<button id="passkeysBtn" class="btn btn-sm" aria-haspopup="dialog" onClick={onPasskeys}>
							Passkeys
						</button>
						<button id="logoutBtn" class="btn btn-ghost btn-sm" onClick={onLogout}>
							Log out
						</button>
					</>
				)}
			</div>
		</header>
	);
}
